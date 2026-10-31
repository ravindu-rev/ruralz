// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
)

// benchForward measures the forwarder's own work per request, the onRoute
// leg selection of docs/architecture/12-performance-budgets-and-benchmarking.md
// "Per-stage latency budget" (1 µs p50, 4 µs p99, 0 allocations), over
// legs that return at once, on the runtime generator as in production.
func benchForward(b *testing.B, legs []*fakeLeg, weights []uint32) {
	f := forwarder(legs, weights, nil)
	o := outbound(nil)
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := f.Forward(ctx, o); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkForwardOneEntry is a Route with a single Upstream.
func BenchmarkForwardOneEntry(b *testing.B) {
	benchForward(b, []*fakeLeg{serving("orders")}, []uint32{1})
}

// BenchmarkForwardSplit is a 95/5 weighted split.
func BenchmarkForwardSplit(b *testing.B) {
	benchForward(b, []*fakeLeg{serving("orders-v1"), serving("orders-v2")}, []uint32{95, 5})
}

// BenchmarkForwardRefused picks again past an entry whose breaker is open.
func BenchmarkForwardRefused(b *testing.B) {
	benchForward(b, []*fakeLeg{refusing("orders-v1", resilience.GateBreaker), serving("orders-v2")}, []uint32{1, 0})
}
