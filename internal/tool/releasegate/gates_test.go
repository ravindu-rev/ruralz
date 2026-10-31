// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// releaseJobs are the jobs of a passing release run: every re-run
// workflow's jobs under its caller's name, and the soak job.
func releaseJobs() []workflowJob {
	ok := func(name string) workflowJob {
		return workflowJob{Name: name, Status: "completed", Conclusion: "success"}
	}
	skipped := func(name string) workflowJob {
		return workflowJob{Name: name, Status: "completed", Conclusion: "skipped"}
	}
	return []workflowJob{
		ok("12. Tag check"),
		ok("pr-fast / Path filters"), ok("pr-fast / 1. Commit hygiene"), ok("pr-fast / 2. Format and lint"),
		ok("pr-fast / 3. Generated code drift"), ok("pr-fast / 4. Build (G1)"), ok("pr-fast / 4. Build and test on the Go 1.26 floor"),
		ok("pr-fast / 5. Unit tests"), ok("pr-fast / 5. CLI tests on macos-latest"), ok("pr-fast / 5. CLI tests on windows-latest"),
		ok("pr-fast / 6. Supply chain"), ok("pr-fast / pr-fast"),
		ok("pr-full / Path filters"), ok("pr-full / 8. Integration and conformance (linux/amd64 process)"),
		ok("pr-full / 8. Integration and conformance (linux/arm64 process)"), ok("pr-full / 9. Regression gates (linux/amd64 alloc/op, size and idle RSS)"),
		ok("pr-full / pr-full"),
		ok("main / 10. End-to-end"), ok("main / 10. Quickstart archives"), ok("main / 10. Quickstart linux/amd64"),
		ok("nightly / 11. Fuzz plan"), ok("nightly / 11. Fuzz shard 0"), ok("nightly / 11. Fuzz shard 1"), ok("nightly / 11. Chaos"),
		ok("nightly / 11. Latency"), ok("nightly / 11. Benchmark records"), ok("nightly / 6. Supply chain rescan"), skipped("nightly / Nightly issues"),
		ok("t5-systemd / T5 systemd (ubuntu-24.04)"), skipped("t5-systemd / T5 issues"),
		ok("chaos-scale / Chaos at target scale"), skipped("chaos-scale / Chaos-scale issues"),
		ok("12. Soak"),
	}
}

// setJob changes the conclusion of the job named name.
func setJob(jobs []workflowJob, name, status, conclusion string) []workflowJob {
	for i := range jobs {
		if jobs[i].Name == name {
			jobs[i].Status, jobs[i].Conclusion = status, conclusion
		}
	}
	return jobs
}

// TestGateSelections: from M6, with the rh-1 jobs, each gate reads its
// jobs; a job that did not finish, did not succeed or did not run is a
// finding.
func TestGateSelections(t *testing.T) {
	sels := gateJobs(true)
	for _, gate := range []string{gateGolden, gateConformance, gateFuzzing, gateChaos, gateBenchmarks, gateSecurity} {
		if got := selectionFindings(releaseJobs(), sels[gate]); len(got) != 0 {
			t.Errorf("%s on a passing run: %q", gate, got)
		}
	}
	cases := []struct {
		gate, job, status, conclusion, want string
	}{
		{gateGolden, "pr-fast / 5. CLI tests on windows-latest", "completed", "failure", `"pr-fast / 5. CLI tests on windows-latest" concluded failure`},
		{gateGolden, "pr-fast / 4. Build and test on the Go 1.26 floor", "completed", "cancelled", "concluded cancelled"}, //nolint:misspell // GitHub spells the conclusion "cancelled".
		{gateConformance, "pr-full / 8. Integration and conformance (linux/arm64 process)", "in_progress", "", "has not finished (in_progress)"},
		{gateFuzzing, "nightly / 11. Fuzz shard 1", "completed", "failure", "11. Fuzz shard 1"},
		{gateChaos, "chaos-scale / Chaos at target scale", "completed", "failure", "chaos-scale / Chaos at target scale"},
		{gateBenchmarks, "12. Soak", "completed", "failure", `"12. Soak" concluded failure`},
		{gateBenchmarks, "pr-full / 9. Regression gates (linux/amd64 alloc/op, size and idle RSS)", "completed", "failure", "9. Regression gates"},
		{gateSecurity, "main / 10. End-to-end", "completed", "failure", "10. End-to-end"},
		{gateSecurity, "pr-fast / 1. Commit hygiene", "completed", "skipped", "concluded skipped"},
	}
	for _, c := range cases {
		got := selectionFindings(setJob(releaseJobs(), c.job, c.status, c.conclusion), sels[c.gate])
		if !strings.Contains(strings.Join(got, "\n"), c.want) {
			t.Errorf("%s with %s %s/%s: findings %q, want %q", c.gate, c.job, c.status, c.conclusion, got, c.want)
		}
	}
	var noChaos []workflowJob
	for _, j := range releaseJobs() {
		if !strings.HasPrefix(j.Name, "chaos-scale / ") {
			noChaos = append(noChaos, j)
		}
	}
	if got := selectionFindings(noChaos, sels[gateChaos]); !strings.Contains(strings.Join(got, "\n"), `no job "chaos-scale / Chaos at target scale" ran`) {
		t.Errorf("Chaos without chaos-scale.yml: %q", got)
	}
}

// rh1Skipped returns the release run's jobs as before M6: GitHub skipped
// the rh-1 jobs, the Latency job, the soak and chaos-scale.yml's chaos job.
func rh1Skipped() []workflowJob {
	jobs := releaseJobs()
	for _, name := range []string{"nightly / 11. Latency", "nightly / 11. Benchmark records", "12. Soak", "chaos-scale / " + chaosScaleJob} {
		jobs = setJob(jobs, name, "completed", "skipped")
	}
	return jobs
}

// TestGateSelectionsBeforeM6: without the rh-1 jobs, the Chaos gate reads
// the 11. Chaos job alone and the Benchmarks gate stage 9's gates alone,
// so a release run in which GitHub skipped the rh-1 jobs passes both, and
// a failed stage 9 gate still fails Benchmarks.
func TestGateSelectionsBeforeM6(t *testing.T) {
	sels := gateJobs(false)
	for _, gate := range []string{gateChaos, gateBenchmarks} {
		if got := selectionFindings(rh1Skipped(), sels[gate]); len(got) != 0 {
			t.Errorf("%s with the rh-1 jobs skipped: %q", gate, got)
		}
	}
	if got := ciFindings(rh1Skipped()); len(got) != 0 {
		t.Errorf("All CI stages green with the rh-1 jobs skipped: %q", got)
	}
	failed := setJob(rh1Skipped(), "pr-full / 9. Regression gates (linux/amd64 alloc/op, size and idle RSS)", "completed", "failure")
	if got := selectionFindings(failed, sels[gateBenchmarks]); !strings.Contains(strings.Join(got, "\n"), "9. Regression gates") {
		t.Errorf("Benchmarks with a failed stage 9 gate: %q", got)
	}
	if got := selectionFindings(rh1Skipped(), gateJobs(true)[gateBenchmarks]); !strings.Contains(strings.Join(got, "\n"), `"nightly / 11. Latency" concluded skipped`) {
		t.Errorf("from M6, Benchmarks with the Latency job skipped: %q", got)
	}
}

// TestChaosScaleWorkflowJob: release.yml calls chaos-scale.yml as the
// job the Chaos gate and the re-run list name, gates on it, and
// chaos-scale.yml's job carries the name the Chaos gate selects.
func TestChaosScaleWorkflowJob(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", name)) //nolint:gosec // G304: a workflow of the repository
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	release := read("release.yml")
	if !strings.Contains(release, "\n  chaos-scale:\n    name: chaos-scale\n") || !strings.Contains(release, "uses: ./.github/workflows/chaos-scale.yml\n") {
		t.Error("release.yml does not call chaos-scale.yml as its chaos-scale job")
	}
	if !strings.Contains(release, "needs: [tag, pr-fast, pr-full, main, nightly, t5-systemd, chaos-scale, soak]") {
		t.Error("release.yml's gates job does not wait for chaos-scale")
	}
	if !slices.Contains(reruns(), rerun{"chaos-scale", "chaos-scale.yml"}) {
		t.Error("the re-run list lacks chaos-scale.yml")
	}
	if !strings.Contains(read("chaos-scale.yml"), "\n    name: "+chaosScaleJob+"\n") {
		t.Errorf("chaos-scale.yml has no job named %q", chaosScaleJob)
	}
}

// TestCIFindings: every re-run workflow ran, and each of its jobs
// succeeded or was skipped by its own condition.
func TestCIFindings(t *testing.T) {
	if got := ciFindings(releaseJobs()); len(got) != 0 {
		t.Errorf("passing run: %q", got)
	}
	got := ciFindings(setJob(releaseJobs(), "main / 10. Quickstart linux/amd64", "completed", "failure"))
	if !strings.Contains(strings.Join(got, "\n"), `"main / 10. Quickstart linux/amd64" concluded failure`) {
		t.Errorf("failed quickstart: %q", got)
	}
	var noT5 []workflowJob
	for _, j := range releaseJobs() {
		if !strings.HasPrefix(j.Name, "t5-systemd / ") {
			noT5 = append(noT5, j)
		}
	}
	if got := ciFindings(noT5); !strings.Contains(strings.Join(got, "\n"), "re-ran no t5-systemd.yml job") {
		t.Errorf("no t5-systemd jobs: %q", got)
	}
}

// TestReadSkips: a skip counts only when the issue carries the label, the
// latest application was by a maintainer, and that maintainer commented
// skip-gate:<gate>: <reason>.
func TestReadSkips(t *testing.T) {
	f := newFakeGitHub(t)
	f.addCollaborator("jane", "admin")
	f.addCollaborator("omar", "maintain")
	f.addCollaborator("wes", "write")
	iss := f.addIssue("v0.2.0", trackingAuthor, "open", "")
	iss.label(skipPrefix+gateChaos, "jane")
	iss.comment("jane", "skip-gate:Chaos: CE-5 flakes on the arm64 runner, issue #12")
	iss.label(skipPrefix+gateFuzzing, "wes")
	iss.comment("wes", "skip-gate:Fuzzing: not needed")
	iss.label(skipPrefix+gateConformance, "omar")
	iss.label(skipPrefix+gateGolden, "omar")
	iss.comment("omar", "skip-gate:Golden corpus:   ")
	iss.label(skipPrefix+gateSecurity, "omar")
	iss.comment("jane", "skip-gate:Security scanning: jane's reason")
	iss.label(skipPrefix+gateCandidate, "wes")
	iss.label(skipPrefix+gateCandidate, "omar") // the latest application counts
	iss.comment("omar", "skip-gate:Release candidate: an earlier reason")
	iss.comment("omar", "skip-gate:Release candidate: the newest reason")

	got, notes, err := readSkips(t.Context(), f.client(t), &iss.issue)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]skip{
		gateChaos:     {Gate: gateChaos, By: "jane", Role: "admin", Reason: "CE-5 flakes on the arm64 runner, issue #12"},
		gateCandidate: {Gate: gateCandidate, By: "omar", Role: "maintain", Reason: "the newest reason"},
	}
	if len(got) != len(want) {
		t.Errorf("skips %+v, want %+v", got, want)
	}
	for g, w := range want {
		if got[g] != w {
			t.Errorf("skip of %s = %+v, want %+v", g, got[g], w)
		}
	}
	text := strings.Join(notes, "\n")
	for _, w := range []string{
		"Fuzzing: skip-gate:Fuzzing was applied by @wes, whose role write is not admin or maintain",
		"Conformance: @omar applied skip-gate:Conformance but wrote no skip-gate:Conformance: <reason> comment",
		"Golden corpus: @omar's skip-gate:Golden corpus: comment gives no reason",
		"Security scanning: @omar applied skip-gate:Security scanning but wrote no",
	} {
		if !strings.Contains(text, w) {
			t.Errorf("notes %q lack %q", notes, w)
		}
	}
	if skips, notes, err := readSkips(t.Context(), f.client(t), nil); err != nil || len(skips) != 0 || notes != nil {
		t.Errorf("no tracking issue: %v %v %v", skips, notes, err)
	}
}

// TestDecide: the gates' rules over the evidence, then the skips: a
// failed gate a maintainer skipped is skipped, the Benchmarks gate
// included, an unused skip is noted, and a release needing no candidate
// leaves that gate not applicable; before M6 the rh-1 jobs and records
// count for nothing.
func TestDecide(t *testing.T) {
	recs, err := readRecords(writeRecords(t, releaseRecords("")))
	if err != nil {
		t.Fatal(err)
	}
	passing := func() *evidence {
		return &evidence{jobs: releaseJobs(), records: recs, candidateApplies: true, rh1: true}
	}
	outcomes := func(rs []gateResult) map[string]string {
		m := map[string]string{}
		for _, r := range rs {
			m[r.Gate] = r.Outcome
		}
		return m
	}

	rs, notes := decide(commitA, passing(), nil, nil)
	for _, r := range rs {
		if r.Outcome != outcomePassed {
			t.Errorf("passing evidence: %s %s %q", r.Gate, r.Outcome, r.Findings)
		}
	}
	if len(rs) != len(gates()) || len(notes) != 0 {
		t.Errorf("results %d, notes %q", len(rs), notes)
	}

	ev := passing()
	ev.nightly = []issue{{Number: 7, Title: "nightly failure: 11. Chaos"}}
	ev.crashersOpen = []issue{{Number: 8, Title: "fuzz crasher: p FuzzX"}}
	ev.missingSeeds = []string{"closed fuzz-crasher issue #3: its regression seed p/testdata/fuzz/FuzzY/1 is not at aaaaaaaaaaaa"}
	ev.jobs = setJob(ev.jobs, "nightly / 11. Chaos", "completed", "failure")
	ev.candidateApplies, ev.candidate = true, []string{"no v0.1.0-rc.N tag names commit"}
	skips := map[string]skip{
		gateChaos:      {Gate: gateChaos, By: "jane", Role: "admin", Reason: "known"},
		gateGolden:     {Gate: gateGolden, By: "jane", Role: "admin", Reason: "unused"},
		gateCandidate:  {Gate: gateCandidate, By: "omar", Role: "maintain", Reason: "first release"},
		gateBenchmarks: {Gate: gateBenchmarks, By: "omar", Role: "maintain", Reason: "rh-1 down"},
	}
	ev.records = nil // Benchmarks fails without records
	rs, notes = decide(commitA, ev, skips, []string{"earlier note"})
	got := outcomes(rs)
	want := map[string]string{
		gateCI: outcomeFailed, gateGolden: outcomePassed, gateConformance: outcomePassed, gateFuzzing: outcomeFailed,
		gateChaos: outcomeSkipped, gateBenchmarks: outcomeSkipped, gateSecurity: outcomePassed, gateCandidate: outcomeSkipped,
	}
	for g, w := range want {
		if got[g] != w {
			t.Errorf("%s = %s, want %s", g, got[g], w)
		}
	}
	text := strings.Join(notes, "\n")
	for _, w := range []string{"earlier note", "Golden corpus: @jane's skip is unused: the gate passed"} {
		if !strings.Contains(text, w) {
			t.Errorf("notes %q lack %q", notes, w)
		}
	}
	for _, r := range rs {
		if r.Gate == gateCI && !strings.Contains(strings.Join(r.Findings, "\n"), "nightly-failure issue #7 is open") {
			t.Errorf("CI findings %q lack the open nightly-failure issue", r.Findings)
		}
		if r.Gate == gateFuzzing && (!strings.Contains(strings.Join(r.Findings, "\n"), "fuzz-crasher issue #8 is open") || !strings.Contains(strings.Join(r.Findings, "\n"), "regression seed")) {
			t.Errorf("Fuzzing findings %q", r.Findings)
		}
	}

	// From M6 the Benchmarks gate fails without records; before M6, with
	// the rh-1 jobs skipped and no records, every gate passes, and a
	// release needing no candidate leaves that gate not applicable.
	rs, _ = decide(commitA, &evidence{jobs: releaseJobs(), rh1: true}, nil, nil)
	if got := outcomes(rs); got[gateBenchmarks] != outcomeFailed || got[gateCandidate] != outcomeNotApplicable {
		t.Errorf("from M6 without records: %v", got)
	}
	rs, notes = decide(commitA, &evidence{jobs: rh1Skipped()}, nil, nil)
	for _, r := range rs {
		if want := map[bool]string{true: outcomeNotApplicable, false: outcomePassed}[r.Gate == gateCandidate]; r.Outcome != want {
			t.Errorf("before M6: %s %s %q, want %s", r.Gate, r.Outcome, r.Findings, want)
		}
	}
	if len(notes) != 0 {
		t.Errorf("before M6: notes %q", notes)
	}
}

// fakeGit answers git ls-tree for the files of commitA.
func fakeGit(files ...string) gitRunner {
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) >= 6 && args[0] == "ls-tree" && args[3] == commitA && args[4] == "--" {
			if slices.Contains(files, args[5]) {
				return []byte(args[5] + "\n"), nil
			}
			return nil, nil
		}
		return nil, fmt.Errorf("fake git: unexpected %q", args)
	}
}

// TestFileAt: a seed file counts only when the commit holds it at that
// path; a path that is not clean never does.
func TestFileAt(t *testing.T) {
	git := fakeGit("p/testdata/fuzz/FuzzX/1")
	for p, want := range map[string]bool{"p/testdata/fuzz/FuzzX/1": true, "p/testdata/fuzz/FuzzX/2": false, "../x": false, "/etc/passwd": false, "p//x": false} {
		got, err := fileAt(t.Context(), git, ".", commitA, p)
		if err != nil || got != want {
			t.Errorf("fileAt(%q) = %t, %v; want %t", p, got, err, want)
		}
	}
}

// TestCrasherSeed: nightly.yml's fuzz-crasher issue body names the seed to
// commit; a path that escapes the repository is refused.
func TestCrasherSeed(t *testing.T) {
	body := "Fuzz target `FuzzParse` in `p` found a failing input.\n\nCommit the input as `p/testdata/fuzz/FuzzParse/771e938e4458e983` (a regression seed) before its fix merges."
	if got, ok := crasherSeed(body); !ok || got != "p/testdata/fuzz/FuzzParse/771e938e4458e983" {
		t.Errorf("crasherSeed = %q, %t", got, ok)
	}
	for _, b := range []string{"no seed here", "Commit the input as `../etc/x`", "Commit the input as `/abs`", "Commit the input as ``"} {
		if got, ok := crasherSeed(b); ok {
			t.Errorf("crasherSeed(%q) = %q", b, got)
		}
	}
}

// gatesFixture is a fake repository with a passing release run 42 of
// v0.2.0 at commitA: its jobs, a passed candidate published 20 days
// earlier, the previous release v0.1.0 at commitB, a closed fuzz-crasher
// issue whose seed the commit holds, and an issue titled v0.2.0 that
// someone else opened.
func gatesFixture(t *testing.T, now time.Time) *fakeGitHub {
	t.Helper()
	f := newFakeGitHub(t)
	f.addCollaborator("jane", "admin")
	blob, _ := sshKeyPair(1)
	sig := sshSignature(blob, "git")
	f.addTag("v0.1.0", commitB, sig, true)
	f.addTag("v0.2.0-rc.1", commitA, sig, true)
	f.addTag("v0.2.0", commitA, sig, true)
	f.addRelease("v0.1.0", false, now.Add(-90*24*time.Hour))
	f.addRelease("v0.2.0-rc.1", true, now.Add(-20*24*time.Hour))
	f.runs = []workflowRun{{HeadBranch: "v0.2.0-rc.1", HeadSHA: commitA, Status: "completed", Conclusion: "success"}}
	f.jobs[42] = releaseJobs()
	f.addIssue("fuzz crasher: p FuzzX", "github-actions[bot]", "closed", "Commit the input as `p/testdata/fuzz/FuzzX/1` (a regression seed)", labelCrasher)
	f.addIssue("v0.2.0", "mallory", "open", "not the tracking issue")
	return f
}

// TestRunGates: the gates of a passing release run pass; releasegate opens
// the tracking issue with the skip labels, never taking another author's
// issue of the same title, records each gate's outcome on it and in the
// summary, and writes the skipped-gates file; a failure then a skip on a
// re-run applies the skip.
func TestRunGates(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	f := gatesFixture(t, now)
	dir := t.TempDir()
	records := writeRecords(t, releaseRecords(commitB))
	o := gatesOptions{
		tag: "v0.2.0", commit: commitA, runID: 42, runURL: "https://github.example/run/42", records: records,
		skipped: filepath.Join(dir, "skipped.json"), summary: filepath.Join(dir, "summary.md"), dir: ".",
	}
	var out bytes.Buffer
	findings, err := runGates(t.Context(), f.client(t), o, now, fakeGit("p/testdata/fuzz/FuzzX/1"), &out)
	if err != nil || len(findings) != 0 {
		t.Fatalf("runGates: %q %v\n%s", findings, err, out.String())
	}
	var tracking *fakeIssue
	for _, i := range f.issues {
		if i.Title == "v0.2.0" && i.User.Login == trackingAuthor {
			tracking = i
		}
	}
	if tracking == nil {
		t.Fatal("no tracking issue opened")
	}
	if !strings.Contains(tracking.Body, "| Benchmarks | passed |") || !strings.Contains(tracking.Body, "| Release candidate | passed |") {
		t.Errorf("tracking issue body:\n%s", tracking.Body)
	}
	if f.issues[1].Body != "not the tracking issue" {
		t.Error("releasegate edited an issue someone else opened")
	}
	for _, g := range gates() {
		if !slices.Contains(f.labels, skipPrefix+g) {
			t.Errorf("label %s%s not created", skipPrefix, g)
		}
	}
	summary := readBack(t, o.summary)
	if !strings.Contains(string(summary), "| All CI stages green | passed |") {
		t.Errorf("summary:\n%s", summary)
	}
	var sf skippedFile
	data := readBack(t, o.skipped)
	if err := json.Unmarshal(data, &sf); err != nil || sf.Format != skippedFormat || len(sf.Skipped) != 0 {
		t.Errorf("skipped file %s: %v", data, err)
	}

	// The Chaos job fails: the Chaos gate fails, and so does "All CI
	// stages green", which reads every re-run job; no skipped file is
	// written.
	f.jobs[42] = setJob(releaseJobs(), "nightly / 11. Chaos", "completed", "failure")
	_ = os.Remove(o.skipped)
	findings, err = runGates(t.Context(), f.client(t), o, now, fakeGit("p/testdata/fuzz/FuzzX/1"), &out)
	if err != nil || len(findings) != 2 || !strings.Contains(findings[0], `gate "All CI stages green" failed`) || !strings.Contains(findings[1], `gate "Chaos" failed`) {
		t.Fatalf("failed Chaos: %q %v", findings, err)
	}
	if _, err := os.Stat(o.skipped); !os.IsNotExist(err) {
		t.Error("a skipped-gates file was written for a failed run")
	}
	if !strings.Contains(tracking.Body, "| Chaos | failed |") || !strings.Contains(tracking.Body, "### Chaos") {
		t.Errorf("tracking issue after the failure:\n%s", tracking.Body)
	}

	// A maintainer skips both; re-running the gates applies the skips.
	tracking.label(skipPrefix+gateChaos, "jane")
	tracking.comment("jane", "skip-gate:Chaos: the runner's kernel lacks tcp_migrate_req")
	tracking.label(skipPrefix+gateCI, "jane")
	tracking.comment("jane", "skip-gate:All CI stages green: only the Chaos job failed")
	findings, err = runGates(t.Context(), f.client(t), o, now, fakeGit("p/testdata/fuzz/FuzzX/1"), &out)
	if err != nil || len(findings) != 0 {
		t.Fatalf("after the skip: %q %v", findings, err)
	}
	data = readBack(t, o.skipped)
	if err := json.Unmarshal(data, &sf); err != nil || len(sf.Skipped) != 2 ||
		sf.Skipped[0] != (skip{Gate: gateCI, By: "jane", Role: "admin", Reason: "only the Chaos job failed"}) ||
		sf.Skipped[1] != (skip{Gate: gateChaos, By: "jane", Role: "admin", Reason: "the runner's kernel lacks tcp_migrate_req"}) {
		t.Errorf("skipped file %s", data)
	}
	if !strings.Contains(tracking.Body, "| Chaos | skipped by @jane (admin) |") {
		t.Errorf("tracking issue after the skip:\n%s", tracking.Body)
	}
	if n := strings.Count(strings.Join(f.requests, "\n"), "POST /repos/"+fakeRepo+"/issues\n"); n != 1 {
		t.Errorf("%d tracking issues opened, want 1", n)
	}

	// A closed crasher whose seed the commit lacks fails Fuzzing.
	findings, err = runGates(t.Context(), f.client(t), o, now, fakeGit(), &out)
	if err != nil || !strings.Contains(strings.Join(findings, "\n"), "regression seed p/testdata/fuzz/FuzzX/1 is not at") {
		t.Errorf("missing seed: %q %v", findings, err)
	}
}

// TestTrackingBody: findings are escaped and bounded, so commit text adds
// no markup and the body stays within GitHub's limit.
func TestTrackingBody(t *testing.T) {
	var many []string
	for i := range 100 {
		many = append(many, fmt.Sprintf("finding %d with `code` and [link](x) and <b>", i))
	}
	body := trackingBody(gatesOptions{tag: "v0.2.0", commit: commitA, runID: 1, runURL: "u"},
		[]gateResult{{Gate: gateChaos, Outcome: outcomeFailed, Findings: many}}, []string{"note *x*"}, "")
	if strings.Contains(body, "`code`") || !strings.Contains(body, "\\`code\\`") || !strings.Contains(body, "\\<b\\>") {
		t.Errorf("findings are not escaped:\n%s", body)
	}
	if !strings.Contains(body, fmt.Sprintf("- and %d more", 100-maxFindingsShown)) || strings.Contains(body, "finding 99 ") {
		t.Error("findings are not bounded")
	}
	if !strings.Contains(body, "note \\*x\\*") || !strings.Contains(body, "skip-gate:Benchmarks") {
		t.Errorf("notes or instructions missing:\n%s", body)
	}
}
