// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// TestRecoveryStartsOnceTheUnfillReturned: CE-6's unfill restores
// maxmemory and deletes the filler, and the deployment refuses writes
// until the deletes land, so a closed Policy answers RZ-STS-002 while the
// unfill runs. Remove dates the unfill at its start in the record, as
// Inject does, and returns the instant it returned, from which the
// recovery counts. With no breaker raised after it, as on main-noeviction,
// whose Response Cache lookups on the same shard keep the share of failed
// calls under the breaker's, ReasonClearedAt falls back to that instant as
// the breakers' closing, so the refusal is no recovery error; counted from
// the unfill's start it would be one.
func TestRecoveryStartsOnceTheUnfillReturned(t *testing.T) {
	const unfill = 25 * time.Millisecond
	clk := clocktest.New(at(0))
	closed := &Stream{Spec: StreamSpec{Name: "closed", Counted: append(Decisions(), STSCodes()...)}}
	add := func(due time.Time, status int, code string, outcome loadgen.Outcome) {
		closed.requests = append(closed.requests, Request{Intended: due, Sent: due, Done: due.Add(time.Millisecond), Status: status, Code: code, Outcome: outcome})
	}
	l := &Load{Streams: []*Stream{closed}}
	r := &Run{T: t, real: clk, rec: Record{Breakdowns: map[string]float64{}}}

	end := r.Remove("unfill", "redis-server", func() error {
		// A request due while the filler is still there meets the full
		// deployment.
		add(at(10), 503, CodeSTSError, loadgen.ServerError)
		clk.Advance(unfill)
		return nil
	})
	if want := at(0).Add(unfill); !end.Equal(want) {
		t.Fatalf("Remove = %v, want the instant the unfill returned, %v", end, want)
	}
	if inj := r.Record().Injections; len(inj) != 1 || inj[0].Kind != "unfill" || !inj[0].Time.Equal(at(0)) {
		t.Errorf("injections = %+v, want the unfill recorded at its start %v", inj, at(0))
	}
	add(end.Add(15*time.Millisecond), 200, "", loadgen.Success)

	// Every sample of both Nodes shows the breaker closed.
	s := &Sampler{keep: DefaultKeep(), ids: map[string]int{}, samples: map[string][]Sample{}}
	expo := func(v int) promtext.Set {
		text := "# TYPE " + DegradedMetric + " gauge\n" + DegradedMetric + `{reason="` + BreakerReason + `"} ` + strconv.Itoa(v) + "\n"
		set, err := promtext.ParseBytes([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return set
	}
	nodes := []string{"node-1", "node-2"}
	for _, n := range nodes {
		for _, ms := range []int{0, 250, 500} {
			s.record(n, at(ms), at(ms), at(ms+2), expo(0), nil)
		}
	}
	recEnd := at(60_000)
	closedAt, ok := s.ReasonClearedAt(nodes, BreakerReason, end)
	if !ok || !closedAt.Equal(end) {
		t.Fatalf("ReasonClearedAt = %v, %v; want the unfill's return %v with no breaker raised", closedAt, ok, end)
	}
	if got := ErrorsAfter(l, closedAt, recEnd, STSCodes()...); got != 0 {
		t.Errorf("recovery errors from the unfill's return = %d, want 0: the refusal came while the unfill ran", got)
	}
	if got := ErrorsAfter(l, at(0), recEnd, STSCodes()...); got != 1 {
		t.Errorf("recovery errors from the unfill's start = %d, want the refusal counted, 1", got)
	}

	// Inject still dates a fault by its start, which the criteria counting
	// from an injection want.
	injected := r.Inject("blackhole", "state-store", func() error {
		clk.Advance(5 * time.Millisecond)
		return nil
	})
	if !injected.Equal(end) {
		t.Errorf("Inject = %v, want its start %v", injected, end)
	}
}
