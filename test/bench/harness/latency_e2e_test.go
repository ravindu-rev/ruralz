// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// The Latency and soak jobs live in an external test package: the Latency
// job registers the drivers of test/bench/scenarios, which builds on the
// harness itself.
package harness_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
	"github.com/ravindu-rev/ruralz/test/bench/scenarios"
)

// job prepares one Latency or soak job: the settings from make
// bench-latency, the profile inventory (the loopback profile without one),
// the head's release build and the session.
func job(t *testing.T) (*harness.Session, harness.Build) {
	t.Helper()
	st, err := harness.SettingsFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	p := harness.Loopback(work)
	if st.Profile != "" {
		if p, err = harness.ReadProfile(st.Profile); err != nil {
			t.Fatal(err)
		}
	}
	head, err := harness.BuildTree(t.Context(), root, filepath.Join(work, "head"), false)
	if err != nil {
		t.Fatal(err)
	}
	s, err := harness.NewSession(t.Context(), p, st, root, filepath.Join(work, "session"))
	if err != nil {
		t.Fatal(err)
	}
	return s, head
}

// annotate prints a GitHub Actions error annotation under GitHub Actions.
func annotate(t *testing.T, title, msg string) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Logf("::error title=%s::%s", title, strings.ReplaceAll(msg, "\n", " "))
	}
}

// TestLatency is the nightly Latency job (PBB "Suite layers"): S1 and S2
// A/B, the component benchmarks and one slot of the rotation, or every
// slot with RURALZ_BENCH_ROTATION_ALL, with the drivers of O1, O2, C1, C2,
// R1, G1, the constants runs, PB-10's telemetry-off S2 and the GOMEMLIMIT
// run at the Latency job's scale; it writes the records and the next
// rotation state into RURALZ_BENCH_RECORDS for the records job.
func TestLatency(t *testing.T) {
	s, head := job(t)
	state, err := harness.ReadState(s.Settings.State)
	if err != nil {
		t.Fatal(err)
	}
	if state, err = harness.Accept(state, s.Settings.Accept); err != nil {
		t.Fatal(err)
	}
	for _, sc := range harness.Catalog() {
		if _, err := os.Stat(filepath.Join(s.RepoRoot, filepath.FromSlash(sc.Bundle))); err != nil {
			t.Fatalf("scenario %s has no Bundle under %s: %v", sc.ID, harness.ScenarioRoot, err)
		}
	}
	plan := harness.NewPlan(state, s.Settings.RotationAll)
	t.Logf("night: slot %d, %d planned minutes, items %v, deferred %v", plan.Slot, plan.Minutes(), itemIDs(plan.Items), plan.Deferred)
	n := &harness.Night{S: s, State: state, Plan: plan, Head: head, Drivers: scenarios.Drivers(scenarios.Full())}
	res, err := n.Run(context.WithoutCancel(t.Context()))
	if errors.Is(err, harness.ErrHostCheck) {
		annotate(t, "RH-1 host check failed", err.Error())
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.WriteState(filepath.Join(s.Settings.Records, "state.json"), res.Next); err != nil {
		t.Fatal(err)
	}
	for id, o := range res.Outcomes {
		t.Logf("%s: %v", id, outcomeName(o))
	}
	for _, f := range res.Failures {
		annotate(t, "Latency job failure", f)
		t.Error(f)
	}
	for id, o := range res.Outcomes {
		if o == harness.Failed && !strings.HasPrefix(strings.Join(res.Failures, "\n"), id) {
			t.Errorf("%s failed", id)
		}
	}
}

// TestSoak is the release soak (PBB "Suite layers"): S2 at half
// saturation for 2 hours under the soak rule.
func TestSoak(t *testing.T) {
	s, head := job(t)
	sc, _ := harness.Find(harness.Catalog(), "S2")
	if err := harness.NewHostChecker(s).Check(t.Context(), sc); err != nil {
		annotate(t, "RH-1 host check failed", err.Error())
		t.Fatal(err)
	}
	rev, err := harness.Render(t.Context(), head.Ruralz, s.RepoRoot, sc, s.Profile)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Soak(context.WithoutCancel(t.Context()), harness.SoakSpec{Scenario: sc, Build: head, Revision: rev})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Validity.Verdict != "valid" {
		t.Errorf("the soak is %s: %q", rec.Validity.Verdict, rec.Validity.Reasons)
	}
	for _, b := range rec.Budgets {
		if b.Verdict == "fail" {
			annotate(t, "Soak budget missed", b.ID+" "+b.Result)
			t.Errorf("%s: %s, want %s", b.ID, b.Result, b.Value)
		}
	}
}

// itemIDs returns the items' IDs.
func itemIDs(items []harness.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func outcomeName(o harness.Outcome) string {
	switch o {
	case harness.Passed:
		return "passed"
	case harness.Inconclusive:
		return "inconclusive"
	default:
		return "failed"
	}
}
