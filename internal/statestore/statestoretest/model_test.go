// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package statestoretest_test

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest"
)

// Tests of the reference Model against the literal numbers of the
// conformance cases.

func TestModelGCRA(t *testing.T) {
	// requests 10, window 1 s, burst 10: T = 100 ms, τ = 1 s; 11 calls
	// at one instant, then RetryAfter = T.
	m := statestoretest.NewModel()
	d := statestore.DigestOf("k")
	call := func(now time.Time, ls ...statestore.GCRALimit) statestore.GCRA {
		g := statestore.GCRA{Policy: "p", Digest: d, Limits: ls, Out: make([]statestore.GCRAOutcome, len(ls))}
		if !m.GCRA(now, &g) {
			t.Fatal("model rejected a valid call")
		}
		return g
	}
	now := start()
	first := call(now, limit)
	if !first.Allowed || first.Out[0] != (statestore.GCRAOutcome{Remaining: 10, ResetAfter: 100 * time.Millisecond}) || !first.ServerNow.Equal(now) {
		t.Fatalf("first: %+v", first)
	}
	for range 10 {
		call(now, limit)
	}
	deny := call(now, limit)
	if deny.Allowed || deny.Denied != 0 || deny.RetryAfter != 100*time.Millisecond || deny.Out[0].Remaining != 0 {
		t.Fatalf("12th: %+v", deny)
	}
	tat, ok := m.TAT("p", limit, d)
	if !ok || tat != statestoretest.GCRAMicros(now)+1_100_000 {
		t.Fatalf("TAT %v, want now + 1.1 s", tat)
	}
	// Two non-conforming limits: the larger retry-after wins; ties go to
	// the lowest index.
	a := statestore.GCRALimit{Requests: 1, Window: time.Second}
	b := statestore.GCRALimit{Requests: 1, Window: 2 * time.Second}
	m2 := statestoretest.NewModel()
	m, d = m2, statestore.DigestOf("k2")
	call(now, a, b)
	g := call(now, a, b)
	if g.Allowed || g.Denied != 1 || g.RetryAfter != 2*time.Second {
		t.Fatalf("two limits: %+v", g)
	}
	c := statestore.GCRALimit{Requests: 2, Window: 2 * time.Second}
	d = statestore.DigestOf("k3")
	call(now, a, c)
	if g := call(now, a, c); g.Denied != 0 || g.RetryAfter != time.Second {
		t.Fatalf("tie: %+v", g)
	}
	// Invalid input is rejected like an error reply.
	for _, l := range []statestore.GCRALimit{{Requests: 0, Window: time.Second}, {Requests: 1, Window: time.Nanosecond}, {Requests: 1, Window: time.Second, Burst: -1}} {
		g := statestore.GCRA{Limits: []statestore.GCRALimit{l}, Out: make([]statestore.GCRAOutcome, 1)}
		if m.GCRA(now, &g) {
			t.Errorf("limit %+v accepted", l)
		}
		if _, _, ok := statestoretest.GCRAParams(l); ok {
			t.Errorf("GCRAParams(%+v) ok", l)
		}
	}
	if m.GCRA(now, &statestore.GCRA{Limits: []statestore.GCRALimit{limit}}) {
		t.Fatal("call without Out accepted")
	}
	if _, ok := m.TAT("none", limit, d); ok {
		t.Fatal("TAT of an unknown key")
	}
}

func TestModelQuota(t *testing.T) {
	m := statestoretest.NewModel()
	d := statestore.DigestOf("k")
	now := start().Add(30 * time.Minute)
	q := statestore.Quota{Name: "q", Window: time.Hour, Limit: 1, Digest: d}
	if ok, skew := m.Quota(now, now, &q); !ok || skew || !q.Allowed || q.Used != 1 || !q.WindowStart.Equal(start()) {
		t.Fatalf("reserve: %+v", q)
	}
	if m.Count("q", time.Hour, false, start(), d) != 1 {
		t.Fatal("count")
	}
	q = statestore.Quota{Name: "q", Window: time.Hour, Limit: 1, Digest: d}
	if m.Quota(now, now, &q); q.Allowed || q.RetryAfter != 30*time.Minute {
		t.Fatalf("deny: %+v", q)
	}
	ref := statestore.Refund{Name: "q", Window: time.Hour, WindowStart: start(), Digest: d}
	if !m.Refund(ref) || m.Refund(ref) {
		t.Fatal("refund not bounded at 0")
	}
	// Candidates: A = the Node's window, B the nearer neighbor.
	for _, c := range []struct {
		node string
		a, b string
	}{
		{"2026-09-26T12:10:00Z", "2026-09-26T12:00:00Z", "2026-09-26T11:00:00Z"},
		{"2026-09-26T12:30:00Z", "2026-09-26T12:00:00Z", "2026-09-26T13:00:00Z"},
		{"2026-09-26T12:50:00Z", "2026-09-26T12:00:00Z", "2026-09-26T13:00:00Z"},
		{"1969-12-31T23:50:00Z", "1969-12-31T23:00:00Z", "1970-01-01T00:00:00Z"},
	} {
		node, _ := time.Parse(time.RFC3339, c.node)
		wa, _ := time.Parse(time.RFC3339, c.a)
		wb, _ := time.Parse(time.RFC3339, c.b)
		a, b := statestoretest.QuotaWindows(node, time.Hour, false)
		if a != wa.UnixMilli() || b != wb.UnixMilli() {
			t.Errorf("node %s: candidates %v, %v; want %s, %s", c.node, time.UnixMilli(a).UTC(), time.UnixMilli(b).UTC(), c.a, c.b)
		}
	}
	// Skew beyond half a window; sub-millisecond windows are invalid.
	q = statestore.Quota{Name: "q", Window: time.Hour, Limit: 5, Digest: d}
	if ok, skew := m.Quota(start().Add(95*time.Minute), start().Add(59*time.Minute), &q); ok || !skew {
		t.Fatal("skew not reported")
	}
	q.Window = time.Microsecond
	if ok, _ := m.Quota(now, now, &q); ok {
		t.Fatal("sub-millisecond window accepted")
	}
}

// TestModelQuotaCost covers cost units: all or none, a cost above the
// units left denying with the count unchanged, 0 counting as 1, a
// negative cost or limit refused; and refunds of units, bounded at 0.
func TestModelQuotaCost(t *testing.T) {
	m := statestoretest.NewModel()
	d := statestore.DigestOf("k")
	now := start().Add(30 * time.Minute)
	reserve := func(cost int64) statestore.Quota {
		q := statestore.Quota{Name: "c", Window: time.Hour, Limit: 5, Cost: cost, Digest: d}
		if ok, skew := m.Quota(now, now, &q); !ok || skew {
			t.Fatalf("cost %d: the model refused the call", cost)
		}
		return q
	}
	if q := reserve(3); !q.Allowed || q.Used != 3 || !q.WindowEnd.Equal(start().Add(time.Hour)) {
		t.Fatalf("cost 3: %+v", q)
	}
	if q := reserve(3); q.Allowed || q.Used != 3 || q.RetryAfter != 30*time.Minute {
		t.Fatalf("cost 3 past the units left: %+v", q)
	}
	if q := reserve(0); !q.Allowed || q.Used != 4 {
		t.Fatalf("cost 0 counts as 1: %+v", q)
	}
	if q := reserve(1); !q.Allowed || q.Used != 5 {
		t.Fatalf("the last unit: %+v", q)
	}
	ref := statestore.Refund{Name: "c", Window: time.Hour, WindowStart: start(), Units: 2, Digest: d}
	if !m.Refund(ref) || m.Count("c", time.Hour, false, start(), d) != 3 {
		t.Fatalf("refund of 2 units: count %d, want 3", m.Count("c", time.Hour, false, start(), d))
	}
	ref.Units = 10
	if !m.Refund(ref) || m.Count("c", time.Hour, false, start(), d) != 0 {
		t.Fatal("a refund above the count does not stop at 0")
	}
	ref.Units = -1
	if m.Refund(ref) {
		t.Fatal("refund of negative units applied")
	}
	for _, q := range []statestore.Quota{
		{Name: "c", Window: time.Hour, Limit: 5, Cost: -1, Digest: d},
		{Name: "c", Window: time.Hour, Limit: -1, Digest: d},
	} {
		if ok, _ := m.Quota(now, now, &q); ok {
			t.Errorf("Quota(cost %d, limit %d) accepted", q.Cost, q.Limit)
		}
	}
	huge := statestore.Quota{Name: "h", Window: time.Hour, Limit: 5, Cost: math.MaxInt64, Digest: d}
	m.Quota(now, now, &huge)
	if ok, _ := m.Quota(now, now, &huge); !ok || huge.Allowed || huge.Used != 0 {
		t.Fatalf("the largest cost: %+v", huge)
	}
}

// TestModelQuotaMonth covers calendar months: the UTC month holding the
// server time, 28 to 31 days, its own counter apart from any window, and
// the nearer neighbor by the middle of the Node's month, across a year
// and a leap February.
func TestModelQuotaMonth(t *testing.T) {
	m := statestoretest.NewModel()
	d := statestore.DigestOf("k")
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	now := at("2026-02-14T08:00:00Z")
	q := statestore.Quota{Name: "m", Month: true, Window: time.Hour, Limit: 2, Cost: 2, Digest: d}
	if ok, skew := m.Quota(now, now, &q); !ok || skew || !q.Allowed || q.Used != 2 {
		t.Fatalf("month reserve: %+v", q)
	}
	if !q.WindowStart.Equal(at("2026-02-01T00:00:00Z")) || !q.WindowEnd.Equal(at("2026-03-01T00:00:00Z")) {
		t.Fatalf("month window [%v, %v)", q.WindowStart, q.WindowEnd)
	}
	if m.Count("m", 0, true, q.WindowStart, d) != 2 || m.Count("m", time.Hour, false, q.WindowStart, d) != 0 {
		t.Fatal("a month counter shares a window's")
	}
	q = statestore.Quota{Name: "m", Month: true, Limit: 2, Digest: d}
	if m.Quota(now, now, &q); q.Allowed || q.RetryAfter != at("2026-03-01T00:00:00Z").Sub(now) {
		t.Fatalf("month deny: %+v", q)
	}
	for _, c := range []struct{ node, a, b string }{
		{"2026-02-10T00:00:00Z", "2026-02-01T00:00:00Z", "2026-01-01T00:00:00Z"},
		{"2026-02-15T00:00:00Z", "2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z"},
		{"2026-03-16T11:59:59.999Z", "2026-03-01T00:00:00Z", "2026-02-01T00:00:00Z"},
		{"2026-03-16T12:00:00Z", "2026-03-01T00:00:00Z", "2026-04-01T00:00:00Z"},
		{"2026-12-20T00:00:00Z", "2026-12-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"2027-01-02T00:00:00Z", "2027-01-01T00:00:00Z", "2026-12-01T00:00:00Z"},
		{"2028-02-15T12:00:00Z", "2028-02-01T00:00:00Z", "2028-03-01T00:00:00Z"},
		{"2028-02-15T11:59:59.999Z", "2028-02-01T00:00:00Z", "2028-01-01T00:00:00Z"},
	} {
		a, b := statestoretest.QuotaWindows(at(c.node), 0, true)
		if a != at(c.a).UnixMilli() || b != at(c.b).UnixMilli() {
			t.Errorf("node %s: candidates %v, %v; want %s, %s", c.node, time.UnixMilli(a).UTC(), time.UnixMilli(b).UTC(), c.a, c.b)
		}
	}
	// A server half a month behind the Node's late-month clock: skew.
	late := statestore.Quota{Name: "m", Month: true, Limit: 9, Digest: d}
	if ok, skew := m.Quota(at("2026-03-20T00:00:00Z"), at("2026-02-10T00:00:00Z"), &late); ok || !skew {
		t.Fatal("skew beyond the nearer month not reported")
	}
	ref := statestore.Refund{Name: "m", Month: true, WindowStart: at("2026-02-01T00:00:00Z"), Units: 1, Digest: d}
	if !m.Refund(ref) || m.Count("m", 0, true, ref.WindowStart, d) != 1 {
		t.Fatal("month refund")
	}
}

// skewed is a Node clock offset from its base.
type skewed struct {
	clock.Clock
	off atomic.Int64
}

func (s *skewed) Now() time.Time { return s.Clock.Now().Add(time.Duration(s.off.Load())) }

func TestConformanceSplitClocks(t *testing.T) {
	// The suite's clock-skew cases run through the Fake on a memory driver
	// with separate Node and server clocks; and, without -short, the
	// real-time path.
	for _, exact := range []bool{true, false} {
		if !exact && testing.Short() {
			continue
		}
		type pair struct {
			server *clocktest.Fake
			node   *skewed
		}
		var mu sync.Mutex
		reg := map[statestore.Driver]pair{}
		get := func(d statestore.Driver) pair {
			mu.Lock()
			defer mu.Unlock()
			return reg[d]
		}
		t.Run(map[bool]string{true: "fake", false: "real"}[exact], func(t *testing.T) {
			statestoretest.RunConformance(t, statestoretest.Harness{
				Exact: exact, Parallel: true, DigestSlot: keys.DigestSlot,
				Open: func(t *testing.T, o statestoretest.OpenOptions) statestore.Driver {
					p := pair{node: &skewed{}}
					base := clock.Real()
					if exact {
						p.server = clocktest.New(start())
						base = p.server
					}
					p.node.Clock = base
					mem, err := memory.New(context.Background(), statestore.Config{}, statestore.Deps{Clock: p.node, MACKey: o.MACKey}, memory.Options{ServerClock: base})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := mem.Close(context.Background()); err != nil {
							t.Error(err)
						}
					})
					f := &statestoretest.Fake{Inner: mem, Clock: p.node}
					mu.Lock()
					reg[f] = p
					mu.Unlock()
					return f
				},
				Advance: func(t *testing.T, d statestore.Driver, by time.Duration) {
					if p := get(d); p.server != nil {
						p.server.Advance(by)
						return
					}
					if err := clock.Real().Sleep(context.Background(), by); err != nil {
						t.Fatal(err)
					}
				},
				Now: func(d statestore.Driver) time.Time { return get(d).server.Now() },
				SkewNode: func(_ *testing.T, d statestore.Driver, off time.Duration) {
					get(d).node.off.Store(int64(off))
				},
			})
		})
	}
}
