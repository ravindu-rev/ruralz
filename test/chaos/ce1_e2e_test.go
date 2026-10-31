// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"slices"
	"testing"
)

// TestCE1NodeKilled is CE-1, and the System overview row "One Node
// killed at 50% load": the topology at half its saturation, plain and
// quota-charged requests, one Node SIGKILLed after the baseline. Errors
// come only from the killed Node's in-flight requests, as the L4 balancer
// attributes them, every Consumer's quota counter holds at least its
// admitted requests, and the recovery sees no error and no new degraded
// reason (docs/architecture/11-scalability-and-distributed-state.md
// "Chaos experiments").
func TestCE1NodeKilled(t *testing.T) {
	s := saturation(t)
	r := suite.Start(t, Spec{Name: CE1})
	rate := suite.Profile.LoadShare * s
	l := r.StartLoad(LoadSpec{Duration: Baseline + FaultPhase + RecoveryTime, Streams: []StreamSpec{
		{Name: "plain", Path: PathPlain, Rate: rate / 2},
		{Name: "limited", Path: PathLimited, Rate: rate / 2, APIKeys: true, Counted: Decisions()},
	}})
	r.Begin(l)
	base := r.Hold(l, PhaseBaseline, Baseline)
	victim := r.Top.Nodes()[len(r.Top.Nodes())/2]
	killed := r.Inject("sigkill", victim.Name(), func() error { return victim.Kill(r.Context()) })
	r.Hold(l, PhaseFault, FaultPhase)
	rec := r.Hold(l, PhaseRecovery, RecoveryTime)
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, st := range l.Streams {
		r.IntervalLog(st.Spec.Name, st)
	}
	r.Baseline(l, base)
	r.Attribute(Attribution(l, killed, rec.End))
	elsewhere, late := InFlightFailures(l, victim.Name(), killed)
	r.Add(
		Check("failed requests not on the killed Node", Functional, float64(elsewhere), EQ, 0),
		Check("failed requests sent after their connection failed", Functional, float64(late), EQ, 0),
		Check("recovery errors", Functional, float64(LoadStats(l, rec.Start, rec.End).Failed), EQ, 0),
	)
	r.Add(quotaUndercharge(t, r, l.Streams[1]))
	var survivors []string
	for _, n := range r.Top.Nodes() {
		if n != victim {
			survivors = append(survivors, n.Name())
		}
	}
	r.StaleReasons(survivors, base.End, rec.End)
	r.Finish()
}

// quotaUndercharge is CE-1's quota criterion: each Consumer's counter in
// the State Store holds at least the requests the stream got admitted for
// it, every success of the whole run having been charged once.
func quotaUndercharge(t *testing.T, r *Run, st *Stream) Criterion {
	t.Helper()
	admitted := map[string]int64{}
	for _, q := range st.Requests() {
		if q.Status >= 200 && q.Status < 300 {
			admitted[q.Key]++
		}
	}
	counters, err := QuotaCounters(r.Context(), r.Primaries())
	if err != nil {
		t.Fatal(err)
	}
	var under int64
	consumers := Consumers()
	slices.Sort(consumers)
	for _, c := range consumers {
		t.Logf("chaos: %s: %d admitted, quota counter %d", c, admitted[c], counters[c])
		under += max(0, admitted[c]-counters[c])
	}
	return Check("quota under-charge (admitted requests above the counter)", Functional, float64(under), EQ, 0)
}
