// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"testing"
	"time"
)

// BenchmarkAttemptAccounting measures the health bookkeeping of one
// attempt on the request path: a View, the step (b) and (c) grades and
// the least-request failure count of the chosen Endpoint, Begin (stall
// timer), Headers and End (passive ejection). It must stay at 0
// allocations (05 test 22, onUpstreamResponseHeaders accounting).
func BenchmarkAttemptAccounting(b *testing.B) {
	tr := NewTracker(Config{Upstream: "orders"})
	if err := tr.SetEndpoints(ids(16)); err != nil {
		b.Fatal(err)
	}
	defer tr.Close()
	var a Attempt
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		v := tr.View(now)
		ep := i & 15
		if !v.Down(ep) && !v.Avoid(ep) {
			_ = v.Failures(ep)
		}
		v.Begin(&a, ep, now)
		a.Headers(now)
		a.End(now, Outcome{Failed: i%7 == 0, Cause: CauseOther})
		i++
	}
}

// BenchmarkViewGrades measures grading every Endpoint of a 256-Endpoint
// Upstream for one selection pass (05 req 11 steps b and c), in parallel
// as concurrent attempts do.
func BenchmarkViewGrades(b *testing.B) {
	const n = 256
	tr := NewTracker(Config{Upstream: "orders"})
	if err := tr.SetEndpoints(ids(n)); err != nil {
		b.Fatal(err)
	}
	defer tr.Close()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			v := tr.View(now)
			eligible := 0
			for i := range n {
				if !v.Down(i) && !v.Avoid(i) {
					eligible++
				}
			}
			if eligible != n {
				b.Errorf("%d eligible, want %d", eligible, n)
				return
			}
		}
	})
}
