// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
)

// job prepares one Latency or soak job: the settings from make
// bench-latency, the profile inventory (the loopback profile without one),
// the head's release build and the session.
func job(t *testing.T) (*Session, Build) {
	t.Helper()
	st, err := SettingsFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	p := Loopback(work)
	if st.Profile != "" {
		if p, err = ReadProfile(st.Profile); err != nil {
			t.Fatal(err)
		}
	}
	head, err := BuildTree(t.Context(), root, filepath.Join(work, "head"), false)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(t.Context(), p, st, root, filepath.Join(work, "session"))
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
// slot with RURALZ_BENCH_ROTATION_ALL; it writes the records and the next
// rotation state into RURALZ_BENCH_RECORDS for the records job.
func TestLatency(t *testing.T) {
	s, head := job(t)
	state, err := ReadState(s.Settings.State)
	if err != nil {
		t.Fatal(err)
	}
	if state, err = Accept(state, s.Settings.Accept); err != nil {
		t.Fatal(err)
	}
	for _, sc := range Catalog() {
		if _, err := os.Stat(filepath.Join(s.RepoRoot, filepath.FromSlash(sc.Bundle))); err != nil {
			t.Fatalf("scenario %s has no Bundle under %s: %v", sc.ID, ScenarioRoot, err)
		}
	}
	plan := NewPlan(state, s.Settings.RotationAll)
	t.Logf("night: slot %d, %d planned minutes, items %v, deferred %v", plan.Slot, plan.Minutes(), ids(plan.Items), plan.Deferred)
	n := &Night{S: s, State: state, Plan: plan, Head: head}
	res, err := n.Run(context.WithoutCancel(t.Context()))
	if errors.Is(err, ErrHostCheck) {
		annotate(t, "RH-1 host check failed", err.Error())
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteState(filepath.Join(s.Settings.Records, "state.json"), res.Next); err != nil {
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
		if o == Failed && !strings.HasPrefix(strings.Join(res.Failures, "\n"), id) {
			t.Errorf("%s failed", id)
		}
	}
}

// TestSoak is the release soak (PBB "Suite layers"): S2 at half
// saturation for 2 hours under the soak rule.
func TestSoak(t *testing.T) {
	s, head := job(t)
	sc, _ := Find(Catalog(), "S2")
	if err := NewHostChecker(s).Check(t.Context(), sc); err != nil {
		annotate(t, "RH-1 host check failed", err.Error())
		t.Fatal(err)
	}
	rev, err := Render(t.Context(), head.Ruralz, s.RepoRoot, sc, s.Profile)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Soak(context.WithoutCancel(t.Context()), SoakSpec{Scenario: sc, Build: head, Revision: rev})
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

func outcomeName(o Outcome) string {
	switch o {
	case Passed:
		return "passed"
	case Inconclusive:
		return "inconclusive"
	default:
		return "failed"
	}
}
