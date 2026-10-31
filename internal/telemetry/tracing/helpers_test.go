// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// counter is an emit.Counter for assertions.
type counter struct{ n atomic.Uint64 }

func (c *counter) Add(_ emit.Stripe, n uint64) { c.n.Add(n) }
func (c *counter) Load() uint64                { return c.n.Load() }

// testCounters returns fresh counters and the Counters wiring them.
type testCounters struct {
	spans, full, export, root, parent counter
}

func (tc *testCounters) wire() Counters {
	return Counters{
		Spans:              &tc.spans,
		DroppedQueueFull:   &tc.full,
		DroppedExportError: &tc.export,
		UnsampledRoot:      &tc.root,
		UnsampledParent:    &tc.parent,
	}
}

// newTracer builds a Tracer shut down at the end of the test.
func newTracer(tb testing.TB, o Options) *Tracer {
	tb.Helper()
	tr, err := New(o)
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tr.Shutdown(ctx); err != nil {
			tb.Errorf("Shutdown: %v", err)
		}
	})
	return tr
}

// newMemTracer builds a Tracer exporting to an in-memory exporter.
func newMemTracer(tb testing.TB, o Options) (*Tracer, *tracetest.InMemoryExporter) {
	tb.Helper()
	tr := newTracer(tb, o)
	mem := tracetest.NewInMemoryExporter()
	tr.SetExporter(mem)
	return tr, mem
}

// flushed flushes tr and returns the exported spans.
func flushed(ctx context.Context, tb testing.TB, tr *Tracer, mem *tracetest.InMemoryExporter) tracetest.SpanStubs {
	tb.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := tr.ForceFlush(ctx); err != nil {
		tb.Fatalf("ForceFlush: %v", err)
	}
	return mem.GetSpans()
}

// discardExporter accepts every batch.
type discardExporter struct{}

func (discardExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (discardExporter) Shutdown(context.Context) error                             { return nil }

// recordExporter records batch sizes and fails while fail is set; block,
// when set, holds ExportSpans until it is closed or ctx ends.
type recordExporter struct {
	mu       sync.Mutex
	batches  []int
	names    []string
	fail     atomic.Bool
	block    chan struct{}
	exported chan int
	shut     atomic.Int32
}

var errExport = errors.New("export failed")

func newRecordExporter() *recordExporter {
	return &recordExporter{exported: make(chan int, 1024)}
}

func (e *recordExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if e.block != nil {
		select {
		case <-e.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.mu.Lock()
	e.batches = append(e.batches, len(spans))
	for _, s := range spans {
		e.names = append(e.names, s.Name())
	}
	e.mu.Unlock()
	e.exported <- len(spans)
	if e.fail.Load() {
		return errExport
	}
	return nil
}

func (e *recordExporter) Shutdown(context.Context) error {
	e.shut.Add(1)
	return nil
}

func (e *recordExporter) got() ([]int, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]int(nil), e.batches...), append([]string(nil), e.names...)
}

// waitExported waits for one export and returns its batch length.
func (e *recordExporter) waitExported(tb testing.TB) int {
	tb.Helper()
	select {
	case n := <-e.exported:
		return n
	case <-time.After(10 * time.Second):
		tb.Fatal("no export within 10 s")
		return 0
	}
}

// Valid W3C values used across the tests.
const (
	testTraceIDHex  = "4bf92f3577b34da6a3ce929d0e0e4736"
	testParentIDHex = "00f067aa0ba902b7"
	tpSampled       = "00-" + testTraceIDHex + "-" + testParentIDHex + "-01"
	tpUnsampled     = "00-" + testTraceIDHex + "-" + testParentIDHex + "-00"
)

// header builds an http.Header from alternating canonical keys and values.
func header(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h[kv[i]] = append(h[kv[i]], kv[i+1])
	}
	return h
}

// Span names of the configuration model's orders-summary Route (OBS
// Figure 2), precomputed as the snapshot compiler does.
type ordersNames struct {
	cors, jwt, geo, authz, rlg, rlo, oauth, hdr, sec string
	orders, inventory                                string
}

func newOrdersNames() ordersNames {
	return ordersNames{
		cors:      catalog.FilterSpanName("cors-partner"),
		jwt:       catalog.FilterSpanName("jwt-default"),
		geo:       catalog.FilterSpanName("geo-block-default"),
		authz:     catalog.FilterSpanName("authz-orders"),
		rlg:       catalog.FilterSpanName("ratelimit-global"),
		rlo:       catalog.FilterSpanName("ratelimit-orders"),
		oauth:     catalog.FilterSpanName("upstream-oauth"),
		hdr:       catalog.FilterSpanName("headers-internal"),
		sec:       catalog.FilterSpanName("headers-security"),
		orders:    catalog.UpstreamSpanName("orders"),
		inventory: catalog.UpstreamSpanName("inventory"),
	}
}

// filterCall runs one Filter span as the executor does.
func filterCall(ctx context.Context, tr emit.Tracer, name, typ string, ph phase.Phase) {
	_, s := tr.StartFilter(ctx, name, emit.FilterAttrs{PolicyType: typ, Phase: ph})
	s.SetAttr(catalog.AttrOutcome, slog.StringValue(emit.OutcomeContinue))
	s.End(0, "", "")
}

// leg runs one Upstream leg attempt with its onUpstreamRequest Filters.
func leg(ctx context.Context, tr emit.Tracer, d *emit.Decision, name, step, endpoint string, filters func(context.Context)) http.Header {
	lctx, s := tr.StartUpstream(ctx, name, emit.UpstreamAttrs{Attempt: 1, Endpoint: endpoint, Step: step})
	filters(lctx)
	out := http.Header{}
	tr.Inject(lctx, d, out)
	s.End(http.StatusOK, "", "")
	return out
}

// ordersSummary drives the 15 Ruralz spans of OBS Figure 2 through tr for
// one request decided into d, and returns the headers of both legs.
func ordersSummary(ctx context.Context, tr emit.Tracer, d *emit.Decision, n *ordersNames) (orders, inventory http.Header) {
	sctx, server := tr.StartServer(ctx, d, http.MethodGet, emit.ServerAttrs{
		Listener: "public", Scheme: "https", Host: "api.example.com", Port: 443,
		Path: "/orders/summary", ClientAddress: "203.0.113.7", UserAgent: "curl/8.9", Protocol: "http2",
	})
	_, match := tr.StartRouteMatch(sctx)
	match.SetAttr(catalog.AttrRoute, slog.StringValue("orders-summary"))
	match.End(0, "", "")
	server.SetAttr(catalog.AttrRoute, slog.StringValue("orders-summary"))
	server.SetAttr(catalog.AttrRevision, slog.StringValue("rev-0123456789ab"))
	filterCall(sctx, tr, n.cors, "cors", phase.OnRequestHeaders)
	filterCall(sctx, tr, n.jwt, "auth.jwt", phase.OnRequestHeaders)
	server.SetAttr(catalog.AttrConsumer, slog.StringValue("partner-a"))
	server.SetAttr(catalog.AttrTier, slog.StringValue("gold"))
	filterCall(sctx, tr, n.geo, "plugin", phase.OnRequestHeaders)
	filterCall(sctx, tr, n.authz, "authz.cel", phase.OnRequestHeaders)
	filterCall(sctx, tr, n.rlg, "ratelimit", phase.OnRequestHeaders)
	filterCall(sctx, tr, n.rlo, "ratelimit", phase.OnRequestHeaders)
	orders = leg(sctx, tr, d, n.orders, "orders", "10.0.0.1:8080", func(lctx context.Context) {
		filterCall(lctx, tr, n.oauth, "auth.upstream-oauth2", phase.OnUpstreamRequest)
		filterCall(lctx, tr, n.hdr, "headers", phase.OnUpstreamRequest)
	})
	inventory = leg(sctx, tr, d, n.inventory, "inventory", "[2001:db8::2]:8443", func(lctx context.Context) {
		filterCall(lctx, tr, n.hdr, "headers", phase.OnUpstreamRequest)
	})
	filterCall(sctx, tr, n.sec, "headers", phase.OnResponse)
	filterCall(sctx, tr, n.cors, "cors", phase.OnResponse)
	server.End(http.StatusOK, "", "")
	return orders, inventory
}
