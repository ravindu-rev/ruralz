// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
)

// The release gates of docs/engineering/03-testing-and-quality-strategy.md,
// "Release gates", that apply from M0 and M1, by their table names. The
// Compatibility gate joins at M2 and the FIPS build gate at M5.
const (
	gateCI          = "All CI stages green"
	gateGolden      = "Golden corpus"
	gateConformance = "Conformance"
	gateFuzzing     = "Fuzzing"
	gateChaos       = "Chaos"
	gateBenchmarks  = "Benchmarks"
	gateSecurity    = "Security scanning"
	gateCandidate   = "Release candidate"
)

// gates lists the release gates in table order.
func gates() []string {
	return []string{gateCI, gateGolden, gateConformance, gateFuzzing, gateChaos, gateBenchmarks, gateSecurity, gateCandidate}
}

// rerun is a workflow the release run re-runs on the tagged commit, by the
// name of the release.yml job that calls it: GitHub names a called job
// "<caller> / <job>".
type rerun struct {
	Caller, Workflow string
}

// reruns lists the re-run workflows of the "All CI stages green" gate:
// pr-fast, pr-full, main, nightly, t5-systemd.yml and chaos-scale.yml.
func reruns() []rerun {
	return []rerun{
		{"pr-fast", "pr-fast.yml"},
		{"pr-full", "pr-full.yml"},
		{"main", "main.yml"},
		{"nightly", "nightly.yml"},
		{"t5-systemd", "t5-systemd.yml"},
		{"chaos-scale", "chaos-scale.yml"},
	}
}

// soakJob is release.yml's soak job, the release-only S2 soak on RH-1.
const soakJob = "12. Soak"

// chaosScaleJob is chaos-scale.yml's job: the target chaos profile on the
// RH-1 hosts.
const chaosScaleJob = "Chaos at target scale"

// jobSel selects jobs of the release run: the jobs a caller's workflow
// ran, by exact name or by name prefix (a matrix job), or all of them.
// An empty Caller selects jobs of release.yml itself.
type jobSel struct {
	Caller string
	Name   string
	// Prefix matches every job whose name starts with Name.
	Prefix bool
}

// label names the selection in findings.
func (s jobSel) label() string {
	name := s.Name
	switch {
	case name == "":
		name = "*"
	case s.Prefix:
		name += "*"
	}
	if s.Caller == "" {
		return name
	}
	return s.Caller + " / " + name
}

// matches reports whether the job named full belongs to the selection.
func (s jobSel) matches(full string) bool {
	caller, name := splitJob(full)
	if caller != s.Caller {
		return false
	}
	switch {
	case s.Name == "":
		return true
	case s.Prefix:
		return strings.HasPrefix(name, s.Name)
	default:
		return name == s.Name
	}
}

// splitJob splits "<caller> / <job>" into its caller and job names; a job
// of release.yml itself has no caller.
func splitJob(full string) (caller, name string) {
	if c, n, ok := strings.Cut(full, " / "); ok {
		return c, n
	}
	return "", full
}

// gateJobs lists the jobs each gate reads (the testing document's second
// Release gates table), besides the issues and records it also reads. The
// rh-1 jobs, chaos-scale.yml's chaos job, the Latency job and the soak,
// join the Chaos and Benchmarks gates only with rh1, from M6: before it
// GitHub skips them.
func gateJobs(rh1 bool) map[string][]jobSel {
	unit := jobSel{Caller: "pr-fast", Name: "5. Unit tests"}
	stage8 := jobSel{Caller: "pr-full", Name: "8. Integration and conformance (", Prefix: true}
	chaos := []jobSel{{Caller: "nightly", Name: "11. Chaos (", Prefix: true}}
	bench := []jobSel{{Caller: "pr-full", Name: "9. Regression gates (", Prefix: true}}
	if rh1 {
		chaos = append(chaos, jobSel{Caller: "chaos-scale", Name: chaosScaleJob})
		bench = append([]jobSel{{Caller: "nightly", Name: "11. Latency"}, {Name: soakJob}}, bench...)
	}
	return map[string][]jobSel{
		// Stage 5's untagged configuration conformance suite on linux,
		// darwin and windows and in the floor job, and stage 8's
		// configuration suite.
		gateGolden: {
			unit,
			{Caller: "pr-fast", Name: "5. CLI tests on ", Prefix: true},
			{Caller: "pr-fast", Name: "4. Build and test on the Go 1.26 floor"},
			stage8,
		},
		// Stage 8 on every leg: the configuration and protocol suites.
		gateConformance: {stage8},
		// Every 11. Fuzz shard job.
		gateFuzzing: {{Caller: "nightly", Name: "11. Fuzz shard ", Prefix: true}},
		// The three 11. Chaos jobs and, from M6, chaos-scale.yml's job,
		// whose issue job a release run skips.
		gateChaos: chaos,
		// From M6, the release's Latency and soak jobs, whose records the
		// gate also reads; stage 9's alloc/op, size and idle RSS gates.
		gateBenchmarks: bench,
		// Stage 6, the stage 1 no-license-check scan, stage 5's redaction
		// unit tests and the secret leak tests of main's 10. End-to-end.
		gateSecurity: {
			{Caller: "pr-fast", Name: "6. Supply chain"},
			{Caller: "pr-fast", Name: "1. Commit hygiene"},
			unit,
			{Caller: "main", Name: "10. End-to-end"},
		},
	}
}

// workflowJob is one job of a workflow run.
type workflowJob struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// jobsPage is one page of a run's jobs.
type jobsPage struct {
	Jobs []workflowJob `json:"jobs"`
}

// selectionFindings checks that every selection matched at least one job
// and that each matched job finished with success.
func selectionFindings(jobs []workflowJob, sels []jobSel) []string {
	var out []string
	for _, s := range sels {
		n := 0
		for _, j := range jobs {
			if !s.matches(j.Name) {
				continue
			}
			n++
			switch {
			case j.Status != "completed":
				out = append(out, fmt.Sprintf("job %q has not finished (%s)", j.Name, j.Status))
			case j.Conclusion != "success":
				out = append(out, fmt.Sprintf("job %q concluded %s", j.Name, j.Conclusion))
			}
		}
		if n == 0 {
			out = append(out, fmt.Sprintf("no job %q ran in the release run", s.label()))
		}
	}
	return out
}

// ciFindings is the "All CI stages green" gate over the jobs: every
// re-run workflow ran, and each of its jobs finished with success or was
// skipped by its own condition (an issue job with nothing to report).
func ciFindings(jobs []workflowJob) []string {
	var out []string
	for _, r := range reruns() {
		n := 0
		for _, j := range jobs {
			if c, _ := splitJob(j.Name); c != r.Caller {
				continue
			}
			n++
			switch {
			case j.Status != "completed":
				out = append(out, fmt.Sprintf("job %q has not finished (%s)", j.Name, j.Status))
			case j.Conclusion != "success" && j.Conclusion != "skipped":
				out = append(out, fmt.Sprintf("job %q concluded %s", j.Name, j.Conclusion))
			}
		}
		if n == 0 {
			out = append(out, fmt.Sprintf("the release run re-ran no %s job (release.yml job %q)", r.Workflow, r.Caller))
		}
	}
	return out
}
