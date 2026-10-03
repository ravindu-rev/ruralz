// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// unsampledRequest is the tracing work of one unsampled request with five
// Filters and one Upstream leg, minus Inject.
func unsampledRequest(ctx context.Context, tr *Tracer, h http.Header, d *emit.Decision, n *ordersNames) (context.Context, emit.Span) {
	tr.Decide(h, 0, d)
	sctx, server := tr.StartServer(ctx, d, http.MethodGet, emit.ServerAttrs{Listener: "public", Path: "/orders"})
	_, match := tr.StartRouteMatch(sctx)
	match.SetAttr(catalog.AttrRoute, slog.StringValue("orders-summary"))
	match.End(0, "", "")
	server.SetAttr(catalog.AttrRoute, slog.StringValue("orders-summary"))
	for _, name := range []string{n.cors, n.jwt, n.authz, n.rlg, n.rlo} {
		_, fs := tr.StartFilter(sctx, name, emit.FilterAttrs{PolicyType: "cors", Phase: phase.OnRequestHeaders})
		fs.SetAttr(catalog.AttrOutcome, slog.StringValue(emit.OutcomeContinue))
		fs.End(0, "", "")
	}
	lctx, up := tr.StartUpstream(sctx, n.orders, emit.UpstreamAttrs{Attempt: 1, Endpoint: "10.0.0.1:8080"})
	up.End(http.StatusOK, "", "")
	_ = server.SpanID()
	server.End(http.StatusOK, "", "")
	return lctx, server
}

// TestUnsampledAllocatesNothing checks the WP-10 "Done when": the
// unsampled path (Decide, every span start, attribute and end) allocates
// nothing; Inject allocates exactly the new traceparent value and its
// slice (spec 09 reqs 38 and 70: 2 allocations per unsampled request).
func TestUnsampledAllocatesNothing(t *testing.T) {
	skipUnderRace(t)
	tr := newTracer(t, Options{})
	tr.SetExporter(discardExporter{})
	n := newOrdersNames()
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		h    http.Header
	}{
		{"no parent", http.Header{}},
		{"unsampled parent with tracestate", header(HeaderTraceparent, tpUnsampled, HeaderTracestate, "congo=t61rcWkgMzE")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d emit.Decision
			if a := testing.AllocsPerRun(200, func() { unsampledRequest(ctx, tr, tc.h, &d, &n) }); a != 0 {
				t.Errorf("unsampled request allocs = %v, want 0", a)
			}
			if d.Sampled {
				t.Fatal("decision sampled")
			}
			out := http.Header{}
			if a := testing.AllocsPerRun(200, func() { tr.Inject(ctx, &d, out) }); a > 2 {
				t.Errorf("Inject allocs = %v, want <= 2", a)
			}
		})
	}
}

// TestSampledWithoutExporterAllocatesNothing: a sampled decision with no
// OTLP endpoint propagates but creates no span (spec 09 req 15).
func TestSampledWithoutExporterAllocatesNothing(t *testing.T) {
	skipUnderRace(t)
	tr := newTracer(t, Options{})
	n := newOrdersNames()
	h := header(HeaderTraceparent, tpSampled)
	var d emit.Decision
	a := testing.AllocsPerRun(200, func() {
		tr.Decide(h, 1, &d)
		sctx, s := tr.StartServer(context.Background(), &d, http.MethodGet, emit.ServerAttrs{})
		_, f := tr.StartFilter(sctx, n.jwt, emit.FilterAttrs{Phase: phase.OnRequestHeaders})
		f.End(0, "", "")
		s.End(http.StatusOK, "", "")
	})
	if a != 0 {
		t.Errorf("allocs = %v, want 0", a)
	}
}

func BenchmarkDecide(b *testing.B) {
	tr := newTracer(b, Options{})
	for _, bc := range []struct {
		name string
		h    http.Header
	}{
		{"root", http.Header{}},
		{"parent", header(HeaderTraceparent, tpUnsampled, HeaderTracestate, "rojo=00f067aa0ba902b7,congo=t61rcWkgMzE")},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			var d emit.Decision
			for b.Loop() {
				tr.Decide(bc.h, DefaultTraceSampling, &d)
			}
		})
	}
}

// BenchmarkUnsampledTrace is spec 09 test 44: at most 2 allocations and
// 1 µs per unsampled request (Decide, 8 no-op spans, Inject).
func BenchmarkUnsampledTrace(b *testing.B) {
	tr := newTracer(b, Options{})
	tr.SetExporter(discardExporter{})
	n := newOrdersNames()
	ctx := context.Background()
	h := http.Header{}
	out := http.Header{}
	var d emit.Decision
	b.ReportAllocs()
	for b.Loop() {
		lctx, _ := unsampledRequest(ctx, tr, h, &d, &n)
		tr.Inject(lctx, &d, out)
	}
}

// BenchmarkSampledTrace15Spans is spec 09 test 44: the 15 spans of the
// orders-summary trace (target 60 allocations, 30 µs; export excluded).
// The SDK required by spec 09 req 1 allocates at least 4 times per span
// (the recording span, its context, its attribute slice and its
// snapshot), so 15 spans cost 60 or more before this package adds its
// span wrappers, the server span's parent and name, and the injected
// headers; the target cannot be met on this SDK, which the observability
// document does not record yet (reported to its owner).
func BenchmarkSampledTrace15Spans(b *testing.B) {
	tr := newTracer(b, Options{QueueSize: 1 << 16, BatchSize: 4096, BatchInterval: time.Millisecond})
	tr.SetExporter(discardExporter{})
	n := newOrdersNames()
	ctx := context.Background()
	h := header(HeaderTraceparent, tpSampled)
	var d emit.Decision
	b.ReportAllocs()
	for b.Loop() {
		tr.Decide(h, 1, &d)
		d.Sampled = true // bypass the parent cap: the benchmark measures spans
		ordersSummary(ctx, tr, &d, &n)
	}
}

func BenchmarkParseTraceparent(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, _, _, ok := ParseTraceparent(tpSampled); !ok {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkValidTracestate(b *testing.B) {
	v := "rojo=00f067aa0ba902b7,congo=t61rcWkgMzE,vendor@tenant=abc def"
	b.ReportAllocs()
	for b.Loop() {
		if !ValidTracestate(v) {
			b.Fatal("invalid")
		}
	}
}

// BenchmarkLimiter measures the cap under contention (spec 09 test 45
// runs it at -cpu 4,32).
func BenchmarkLimiter(b *testing.B) {
	l := NewLimiter(DefaultRootPerSecond, DefaultRootPerSecond)
	start := time.Now()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l.Allow(time.Since(start))
		}
	})
}
