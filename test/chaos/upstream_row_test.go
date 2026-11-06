// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// TestPhaseResetFailures checks the split of a resetting phase's failed
// requests by the retry budget rule, over the requests due in the phase
// from its recorded start: one due between the start and the fault's
// injection counts, one due before the phase or at its end does not, a
// 502 RZ-UP-004 is a reset and every other failure, a 502 of another code,
// a transport failure or a missed request, is another failure, and Last is
// the latest reset answer, which may complete after the phase.
func TestPhaseResetFailures(t *testing.T) {
	phase := Phase{Name: "one-resetting", Start: at(100), End: at(1500)}
	type req struct {
		intended, done, status int
		code                   string
		outcome                loadgen.Outcome
	}
	reqs := []req{
		{99, 160, 502, CodeUpstreamReset, loadgen.ServerError},  // due before the phase
		{120, 158, 502, CodeUpstreamReset, loadgen.ServerError}, // due before the injection at 157
		{150, 151, 200, "", loadgen.Success},
		{170, 171, 502, "RZ-UP-007", loadgen.ServerError},
		{172, 173, 0, "", loadgen.Reset},
		{173, 0, 0, "", loadgen.Missed},
		{1490, 1600, 502, CodeUpstreamReset, loadgen.ServerError}, // answered after the phase
		{1500, 1501, 502, CodeUpstreamReset, loadgen.ServerError}, // due at the phase's end
	}
	stream := func(counted ...string) *Stream {
		st := &Stream{Spec: StreamSpec{Name: "plain", Counted: counted}}
		for _, q := range reqs {
			st.requests = append(st.requests, Request{Intended: at(q.intended), Sent: at(q.intended), Done: at(q.done), Status: q.status, Code: q.code, Outcome: q.outcome})
		}
		return st
	}
	for _, tc := range []struct {
		name string
		st   *Stream
		want ResetFailures
	}{
		{"every failure", stream(), ResetFailures{Resets: 2, Other: 3, Last: at(1600)}},
		{"resets counted as answers", stream(CodeUpstreamReset), ResetFailures{Other: 3}},
	} {
		if got := PhaseResetFailures(tc.st, phase); got != tc.want {
			t.Errorf("%s: PhaseResetFailures = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestResetFailuresCriteria checks the retry budget criteria: they pass
// while every failure is a 502 RZ-UP-004 and the retry budget refused at
// least as many retries, and each fails on its own breach.
func TestResetFailuresCriteria(t *testing.T) {
	for _, tc := range []struct {
		name     string
		f        ResetFailures
		refusals float64
		want     [2]string
	}{
		{"no failure", ResetFailures{}, 0, [2]string{VerdictPass, VerdictPass}},
		{"every reset a refusal", ResetFailures{Resets: 2}, 3, [2]string{VerdictPass, VerdictPass}},
		{"resets beyond the refusals", ResetFailures{Resets: 3}, 2, [2]string{VerdictPass, VerdictFail}},
		{"another failure", ResetFailures{Resets: 1, Other: 1}, 1, [2]string{VerdictFail, VerdictPass}},
	} {
		cs := tc.f.Criteria("one resetting", tc.refusals)
		if len(cs) != 2 {
			t.Fatalf("%s: Criteria = %d criteria, want 2", tc.name, len(cs))
		}
		for i, c := range cs {
			if c.Verdict != tc.want[i] || c.Kind() != Functional {
				t.Errorf("%s: %s = %s (%s), want %s and %s", tc.name, c.Name, c.Verdict, c.Kind(), tc.want[i], Functional)
			}
		}
	}
}

// TestIncreaseCovering checks the counter increase across a window: from
// the last sample answered by its start, so an increment that a sample
// begun before the start and answered after it holds still counts, which
// Increase misses, to the first sample begun at or after its end, which
// holds an increment made before the end that no earlier sample shows; a
// reset counts from 0, as does a trace without a sample answered by the
// start, and a trace without a sample begun by the end stops at its last.
func TestIncreaseCovering(t *testing.T) {
	// Increments at 150, 900 and 1400: the sample begun at 95 rendered after
	// the first one.
	tr := []Point{
		{Real: at(0), Done: at(5), Value: 0},
		{Real: at(95), Done: at(160), Value: 1},
		{Real: at(1000), Done: at(1005), Value: 2},
		{Real: at(1500), Done: at(1510), Value: 3},
		{Real: at(2500), Done: at(2505), Value: 7},
	}
	reset := []Point{
		{Real: at(0), Done: at(5), Value: 4},
		{Real: at(1000), Done: at(1005), Value: 1},
		{Real: at(2000), Done: at(2005), Value: 3},
	}
	for _, tc := range []struct {
		name     string
		trace    []Point
		from, to int
		want     float64
	}{
		{"sample answered after the start", tr, 100, 1500, 3},
		{"end between samples", tr, 100, 1200, 3},
		{"end before the next sample", tr, 100, 1000, 2},
		{"no sample begun by the end", tr, 100, 3000, 7},
		{"no sample answered by the start", tr, 3, 1500, 3},
		{"reset", reset, 10, 2000, 3},
		{"no sample answered by the start, reset", reset, 3, 2000, 4 + 1 + 2},
		{"empty", nil, 0, 1000, 0},
	} {
		if got := IncreaseCovering(tc.trace, at(tc.from), at(tc.to)); got != tc.want {
			t.Errorf("%s: IncreaseCovering(%d, %d) = %v, want %v", tc.name, tc.from, tc.to, got, tc.want)
		}
	}
	if got := Increase(tr, at(100), at(1500)); got != 2 {
		t.Errorf("Increase(100, 1500) = %v, want 2: from the sample begun at 95, which holds the increment at 150", got)
	}
}

// TestRetryBudgetRefusals checks the refusals a resetting phase's 502
// RZ-UP-004 answers are held against: node-1 refuses retries at 158, after
// the injection at 157, which its sample begun before the phase's start at
// 100 and answered at 170 already holds, and at 1490, answered at 1600
// after the phase ended at 1500 and held only by its sample begun at 2000;
// node-2 refuses one at 300. Every Node's samples of another Upstream
// stay out. The three answers meet three refusals, where the increase
// from the injection to the phase's end finds one.
func TestRetryBudgetRefusals(t *testing.T) {
	s := &Sampler{keep: DefaultKeep(), ids: map[string]int{}, samples: map[string][]Sample{}}
	expo := func(refused int) promtext.Set {
		text := "# TYPE ruralz_upstream_retry_budget_exhausted_total counter\n" +
			`ruralz_upstream_retry_budget_exhausted_total{upstream="chaos-backend"} ` + strconv.Itoa(refused) + "\n" +
			`ruralz_upstream_retry_budget_exhausted_total{upstream="other"} ` + strconv.Itoa(100+refused) + "\n"
		set, err := promtext.ParseBytes([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return set
	}
	for _, x := range []struct {
		node            string
		real, done, val int
	}{
		{"node-1", 0, 5, 0},
		{"node-1", 95, 170, 1},
		{"node-1", 1000, 1004, 1},
		{"node-1", 2000, 2004, 2},
		{"node-2", 0, 3, 0},
		{"node-2", 1000, 1003, 1},
		{"node-2", 2000, 2003, 1},
	} {
		s.record(x.node, at(x.real), at(x.real), at(x.done), expo(x.val), nil)
	}
	r := &Run{Sampler: s}
	match := promtext.L("upstream", "chaos-backend")
	phase := Phase{Name: "one-resetting", Start: at(100), End: at(1500)}
	f := ResetFailures{Resets: 3, Last: at(1600)}
	refusals := r.RetryBudgetRefusals(match, phase, f)
	if refusals != 3 {
		t.Errorf("RetryBudgetRefusals = %v, want 3", refusals)
	}
	for _, c := range f.Criteria("one resetting", refusals) {
		if c.Verdict != VerdictPass {
			t.Errorf("%s = %s with %v refusals", c.Name, c.Verdict, refusals)
		}
	}
	if got := r.SumIncrease(catalog.UpstreamRetryBudgetExhaustedTotal, match, at(157), phase.End); got != 1 {
		t.Errorf("SumIncrease from the injection to the phase's end = %v, want 1", got)
	}
}
