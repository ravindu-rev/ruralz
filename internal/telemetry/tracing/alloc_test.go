// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

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

// TestUnsampledAllocatesNothing checks that the unsampled path (Decide,
// every span start, attribute and end) allocates nothing; Inject allocates
// exactly the new traceparent value and its slice (2 allocations per
// unsampled request).
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
// OTLP endpoint propagates but creates no span.
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
