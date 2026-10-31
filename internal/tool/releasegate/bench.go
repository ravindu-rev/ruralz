// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// benchFormat is the benchmark result record format
// (docs/architecture/12-performance-budgets-and-benchmarking.md, "Result
// record").
const benchFormat = "ruralz.bench.v1"

// maxRecordBytes bounds one record, as internal/testkit/benchrecord's
// MaxRecordBytes does.
const maxRecordBytes = 16 << 20

// stateFile is the rotation state the Latency job writes beside its
// records for the bench-records branch; it is not a record.
const stateFile = "state.json"

// Record kinds and summary types of ruralz.bench.v1.
const (
	kindMacro     = "macro"
	kindComponent = "component"
	kindSummary   = "summary"

	ofComparison = "comparison"
	ofLadder     = "ladder"
	ofSoak       = "soak"
)

// rerunSuffix ends the name of the harness's single re-run of an invalid
// run, which supersedes that run (test/bench/harness, "-rerun").
const rerunSuffix = "-rerun"

// benchBudget is one budget result of a record.
type benchBudget struct {
	ID      string `json:"id"`
	Value   string `json:"value"`
	Result  string `json:"result"`
	Verdict string `json:"verdict"`
}

// benchRecord is the part of a ruralz.bench.v1 record the Benchmarks gate
// reads.
type benchRecord struct {
	Format   string `json:"format"`
	Identity struct {
		Scenario string `json:"scenario"`
		Kind     string `json:"kind"`
		Commit   string `json:"commit"`
		Baseline string `json:"baseline"`
	} `json:"identity"`
	Validity struct {
		Verdict string   `json:"verdict"`
		Reasons []string `json:"reasons"`
	} `json:"validity"`
	Budgets []benchBudget `json:"budgets"`
	Summary *struct {
		Of      string   `json:"of"`
		Verdict string   `json:"verdict"`
		Runs    []string `json:"runs"`
	} `json:"summary"`
	// file is the record's slash path below the records directory.
	file string
}

// readRecords reads every record (*.json) below dir, sorted by path. A
// missing directory holds none; a state.json that is no record (the
// rotation state) is skipped; any other JSON file that is not a
// ruralz.bench.v1 record is an error, so a result never goes missing
// silently.
func readRecords(dir string) ([]benchRecord, error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var out []benchRecord
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".json" {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f, err := os.Open(p) //nolint:gosec // A record below the records directory given on the command line.
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
		_ = f.Close()
		if err != nil {
			return err
		}
		if len(data) > maxRecordBytes {
			return fmt.Errorf("%s exceeds %d bytes", rel, maxRecordBytes)
		}
		var r benchRecord
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if r.Format != benchFormat {
			if path.Base(rel) == stateFile {
				return nil
			}
			return fmt.Errorf("%s: format %q, want %q", rel, r.Format, benchFormat)
		}
		switch r.Identity.Kind {
		case kindMacro, kindComponent:
		case kindSummary:
			if r.Summary == nil {
				return fmt.Errorf("%s: a summary record without its summary", rel)
			}
		default:
			return fmt.Errorf("%s: kind %q", rel, r.Identity.Kind)
		}
		r.file = rel
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("benchmark records: %w", err)
	}
	slices.SortFunc(out, func(a, b benchRecord) int { return strings.Compare(a.file, b.file) })
	return out, nil
}

// baseline is the comparison a release's macro scenarios must make: with
// the previous release's commit, or none for the first release, which is
// judged on absolute budgets only.
type baseline struct {
	Tag, Commit string
}

// requiredSummaries are the summaries every release run writes: the S1
// and S2 comparisons of the Latency job, whose budgets carry SM-4, SM-5
// and PB-4, and the S2 soak of the soak job, which the harness records as
// S2-soak (docs/architecture/12-performance-budgets-and-benchmarking.md,
// "Suite layers"), each as scenario and summary type.
func requiredSummaries() [][2]string {
	return [][2]string{{"S1", ofComparison}, {"S2", ofComparison}, {"S2-soak", ofSoak}}
}

// soakBudgets are the budgets the soak meets over its whole run besides
// its drift rule: PB-2 and PB-3 (Suite layers).
func soakBudgets() []string { return []string{"PB-2", "PB-3"} }

// benchmarkFindings is the records part of the Benchmarks gate: every
// verdict of the release's Latency and soak jobs passes. A summary record
// is of the release commit and valid; its A/B verdict passes, or is none
// for a ladder, a soak, a component benchmark (whose baseline is its own
// last passing run) and, for the first release, a comparison; with a
// previous release every macro comparison passes against that release's
// commit; and every budget it holds was measured and met. A run that no
// summary covers, a single run judged on absolute criteria, is valid and
// meets every budget, unless its re-run supersedes it. The S1 and S2
// comparisons and the S2 soak, with PB-2 and PB-3, must be present.
func benchmarkFindings(records []benchRecord, commit string, prev *baseline) []string {
	kinds := map[string]string{}
	covered := map[string]bool{}
	for _, r := range records {
		kinds[r.file] = r.Identity.Kind
		if r.Summary != nil {
			for _, run := range r.Summary.Runs {
				covered[path.Join(path.Dir(r.file), run)] = true
			}
		}
	}
	var out []string
	present := map[[2]string]bool{}
	for _, r := range records {
		where := fmt.Sprintf("%s (%s)", r.Identity.Scenario, r.file)
		if r.Identity.Kind != kindSummary {
			if covered[r.file] || superseded(r.file, kinds) {
				continue
			}
			out = append(out, runFindings(r, where, commit)...)
			continue
		}
		s := r.Summary
		switch s.Of {
		case ofComparison, ofSoak, ofLadder:
			present[[2]string{r.Identity.Scenario, s.Of}] = true
		default:
			out = append(out, fmt.Sprintf("%s: summary of %q", where, s.Of))
		}
		if r.Identity.Commit != commit {
			out = append(out, fmt.Sprintf("%s: measured commit %s, not the release commit %s", where, r.Identity.Commit, commit))
		}
		if r.Validity.Verdict != "valid" {
			out = append(out, fmt.Sprintf("%s: %s summary (%s)", where, r.Validity.Verdict, strings.Join(r.Validity.Reasons, "; ")))
		}
		// A comparison whose runs are macro runs compares the release
		// with the previous release.
		macro := false
		for _, run := range s.Runs {
			if kinds[path.Join(path.Dir(r.file), run)] == kindMacro {
				macro = true
			}
		}
		comparesRelease := prev != nil && macro && s.Of == ofComparison
		switch s.Verdict {
		case "pass":
			if comparesRelease && r.Identity.Baseline != prev.Commit {
				out = append(out, fmt.Sprintf("%s: compared with %q, not with the previous release %s (%s)", where, r.Identity.Baseline, prev.Tag, prev.Commit))
			}
		case "none":
			if comparesRelease {
				out = append(out, fmt.Sprintf("%s: no comparison with the previous release %s", where, prev.Tag))
			}
		default:
			out = append(out, fmt.Sprintf("%s: A/B verdict %s", where, s.Verdict))
		}
		out = append(out, budgetFindings(r.Budgets, where)...)
		if s.Of == ofSoak {
			for _, id := range soakBudgets() {
				if !slices.ContainsFunc(r.Budgets, func(b benchBudget) bool { return b.ID == id }) {
					out = append(out, fmt.Sprintf("%s: the soak holds no %s result", where, id))
				}
			}
		}
	}
	for _, want := range requiredSummaries() {
		if !present[want] {
			job := "11. Latency"
			if want[1] == ofSoak {
				job = soakJob
			}
			out = append(out, fmt.Sprintf("no %s %s summary record from the %s job", want[0], want[1], job))
		}
	}
	return out
}

// runFindings checks a run that no summary covers.
func runFindings(r benchRecord, where, commit string) []string {
	var out []string
	if r.Identity.Commit != commit {
		out = append(out, fmt.Sprintf("%s: measured commit %s, not the release commit %s", where, r.Identity.Commit, commit))
	}
	if r.Validity.Verdict != "valid" {
		out = append(out, fmt.Sprintf("%s: %s run (%s)", where, r.Validity.Verdict, strings.Join(r.Validity.Reasons, "; ")))
	}
	return append(out, budgetFindings(r.Budgets, where)...)
}

// budgetFindings reports every budget not measured or not met.
func budgetFindings(budgets []benchBudget, where string) []string {
	var out []string
	for _, b := range budgets {
		switch b.Verdict {
		case "pass":
		case "skipped":
			out = append(out, fmt.Sprintf("%s: budget %s (%s) was not measured", where, b.ID, b.Value))
		default:
			out = append(out, fmt.Sprintf("%s: budget %s %s: %s, want %s", where, b.ID, b.Verdict, b.Result, b.Value))
		}
	}
	return out
}

// superseded reports whether a run record has a re-run beside it: the
// harness re-runs an invalid run once as <name>-rerun.json.
func superseded(file string, kinds map[string]string) bool {
	stem, ok := strings.CutSuffix(file, ".json")
	if !ok {
		return false
	}
	_, rerun := kinds[stem+rerunSuffix+".json"]
	return rerun
}
