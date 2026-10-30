// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// applyEndpoint compiles and applies a telemetry block with endpoint.
func applyEndpoint(t *testing.T, r *Runtime, tel *v1alpha1.Telemetry, secrets secret.Store) {
	t.Helper()
	s, err := CompileSettings(tel, secrets)
	if err != nil {
		t.Fatalf("CompileSettings: %v", err)
	}
	if err := r.Apply(t.Context(), s); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// cleartext returns the telemetry block of an h2c collector.
func cleartext(s *otlpsink.Sink) *v1alpha1.Telemetry {
	return &v1alpha1.Telemetry{OTLP: &v1alpha1.OTLP{Endpoint: "http://" + s.Addr()}}
}

// sampledSpan ends one sampled server span named after path.
func sampledSpan(t *testing.T, r *Runtime, path string) {
	t.Helper()
	var d emit.Decision
	r.Tracer().Decide(http.Header{}, 1, &d)
	if !d.Sampled {
		t.Fatal("ratio 1 did not sample")
	}
	_, sp := r.Tracer().StartServer(context.Background(), &d, "GET", emit.ServerAttrs{Listener: "http", Scheme: "http", Path: path})
	sp.End(200, "", "")
}

// TestExportOverTLS sends every signal to a collector over TLS with a
// private CA: spans, metrics, process and access logs arrive under the
// resource, and no request carries a header of its own.
func TestExportOverTLS(t *testing.T) {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_RESOURCE_ATTRIBUTES"} {
		t.Setenv(k, "x-leak=1")
	}
	ScrubOTELEnvironment()
	ca, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Server("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := leaf.ServerTLS(nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := startSink(t, otlpsink.Config{TLS: serverTLS})
	r, _ := newRuntime(t, nil)
	caRef := v1alpha1.SecretRef{Provider: "file", Name: "/etc/ruralz/otel/ca.crt"}
	store := fakeStore{secret.RefOf(caRef): secret.NewValue(ca.CertPEM)}
	applyEndpoint(t, r, &v1alpha1.Telemetry{OTLP: &v1alpha1.OTLP{
		Endpoint: "https://" + sink.Addr(),
		TLS:      &v1alpha1.OTLPTLS{CACertificate: &v1alpha1.SecretValue{SecretRef: caRef}},
	}}, store)

	ctx := waitCtx(t)
	sampledSpan(t, r, "/orders")
	if err := r.Tracer().ForceFlush(ctx); err != nil {
		t.Fatalf("span flush: %v", err)
	}
	r.Logger("loader").Info("activated")
	rec := r.AccessLog().Acquire()
	rec.Start, rec.Method, rec.Path, rec.Status, rec.Route = r.clk.Now(), "GET", "/orders", 200, "orders"
	r.AccessLog().Submit(rec)
	flushLogs(t, r)
	if err := r.pipe.lp.ForceFlush(ctx); err != nil {
		t.Fatalf("log flush: %v", err)
	}
	if err := r.pipe.push.pushNow(ctx); err != nil {
		t.Fatalf("metric flush: %v", err)
	}

	spans := sink.Spans()
	if len(spans) != 1 || spans[0].Name() != "GET" {
		t.Fatalf("spans = %v", spans)
	}
	if v, _ := otlpsink.ResourceAttr(spans[0].Resource, "service.instance.id"); otlpsink.ValueString(v) != testNodeID {
		t.Errorf("span resource = %v", spans[0].Resource)
	}
	var access, process bool
	for _, l := range sink.Logs() {
		switch {
		case l.EventName() == "ruralz.access" && l.AttrString("route") == "orders":
			access = true
		case l.Body() == "activated":
			process = true
		}
	}
	if !access || !process {
		t.Errorf("logs: access %v, process %v in %v", access, process, sink.Logs())
	}
	if _, ok := sink.LastMetric(catalog.NodeDegradedInfo); !ok {
		t.Errorf("metric %s not exported; got %v", catalog.NodeDegradedInfo, sink.MetricNames())
	}
	for _, req := range sink.Requests() {
		if req.TLS == nil {
			t.Errorf("%v export not over TLS", req.Signal)
		}
		if extra := req.ExtraMetadata(); len(extra) != 0 {
			t.Errorf("%v export carries headers %v", req.Signal, extra)
		}
		res := req.Traces.GetResourceSpans()
		for _, rs := range res {
			if n := len(rs.GetResource().GetAttributes()); n != 3 {
				t.Errorf("resource has %d attributes, want 3", n)
			}
		}
	}
	if value(t, scrapeAfter(t, r), catalog.SecurityCleartextHops, "hop", "telemetry") != 0 {
		t.Error("TLS export reported as a cleartext hop")
	}
}

// TestCollectorDownRaisesFailing checks a collector that refuses
// connections: each failed span batch counts its spans as export_error,
// a failure run past one interval raises telemetry_export_failing, and the
// first export after the collector returns clears it.
func TestCollectorDownRaisesFailing(t *testing.T) {
	sink := startSink(t, otlpsink.Config{Mode: otlpsink.Down{}})
	// The metric push follows the fake clock, which moves the failure run
	// past one interval; it pushes, and fails, on the way.
	r, _ := newRuntime(t, func(o *Options) { o.MetricInterval = time.Hour })
	r.export = exportBounds{timeout: time.Second}
	applyEndpoint(t, r, cleartext(sink), nil)
	clk := r.clk.(*clocktest.Fake)
	ctx := waitCtx(t)
	failing := func() float64 {
		return value(t, scrapeAfter(t, r), catalog.NodeDegradedInfo, "reason", "telemetry_export_failing")
	}

	sampledSpan(t, r, "/a")
	sampledSpan(t, r, "/b")
	_ = r.Tracer().ForceFlush(ctx)
	if v := value(t, scrapeAfter(t, r), catalog.TelemetrySpansDroppedTotal, "reason", "export_error"); v != 2 {
		t.Errorf("export_error drops = %v, want 2", v)
	}
	if failing() != 0 {
		t.Fatal("raised by the first failure")
	}
	clk.Advance(2 * r.metricInterval)
	sampledSpan(t, r, "/c")
	_ = r.Tracer().ForceFlush(ctx)
	if failing() != 1 {
		t.Fatal("not raised past one interval")
	}
	if err := sink.SetMode(otlpsink.Accept{}); err != nil {
		t.Fatal(err)
	}
	// The connection backs off after the refusals: reset the backoff and
	// export until one gets through. Every signal that failed clears on
	// its own next success, so the logs (whose failure was logged too) and
	// the metrics need an export of their own.
	for failing() != 0 {
		if ctx.Err() != nil {
			t.Fatal("never cleared after the collector returned")
		}
		r.pipe.conn.ResetConnectBackoff()
		sampledSpan(t, r, "/d")
		_ = r.Tracer().ForceFlush(ctx)
		r.Logger("probe").Info("collector back")
		_ = r.pipe.lp.ForceFlush(ctx)
		_ = r.pipe.push.pushNow(ctx)
		time.Sleep(50 * time.Millisecond)
	}
	if v := value(t, scrapeAfter(t, r), catalog.SecurityCleartextHops, "hop", "telemetry"); v != 1 {
		t.Errorf("cleartext telemetry hop = %v, want 1", v)
	}
}

// TestCollectorStallDropsQueueFull checks a stalled collector: spans keep
// ending without blocking, and those past the bounded queue are counted as
// queue_full.
func TestCollectorStallDropsQueueFull(t *testing.T) {
	sink := startSink(t, otlpsink.Config{Mode: otlpsink.Stall{}})
	r, _ := newRuntime(t, nil)
	// The export bound runs on the fake clock, which the loop below moves
	// by 10 s in all: an hour keeps the first export stalled throughout.
	r.export = exportBounds{timeout: time.Hour}
	applyEndpoint(t, r, cleartext(sink), nil)
	clk := r.clk.(*clocktest.Fake)
	start := time.Now()
	// The root cap admits 1,000 sampled decisions per second of the clock.
	for range 10 {
		for range 1000 {
			sampledSpan(t, r, "/x")
		}
		clk.Advance(time.Second)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("10,000 spans took %v while the collector stalled", took)
	}
	if v := value(t, scrapeAfter(t, r), catalog.TelemetrySpansDroppedTotal, "reason", "queue_full"); v == 0 {
		t.Error("no queue_full drop while the collector stalled")
	}
	// Let the final flush through.
	if err := sink.SetMode(otlpsink.Accept{}); err != nil {
		t.Fatal(err)
	}
}

// TestApplySwapsPipelines checks Hot Reloads of the telemetry settings:
// A, then B with A's connection closed, then none, which stops span
// creation; unchanged settings keep the running pipeline.
func TestApplySwapsPipelines(t *testing.T) {
	a := startSink(t, otlpsink.Config{})
	b := startSink(t, otlpsink.Config{})
	r, _ := newRuntime(t, nil)
	ctx := waitCtx(t)

	applyEndpoint(t, r, cleartext(a), nil)
	first := r.pipe
	applyEndpoint(t, r, cleartext(a), nil)
	if r.pipe != first {
		t.Fatal("unchanged settings replaced the pipeline")
	}
	sampledSpan(t, r, "/a")
	if err := r.Tracer().ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}

	applyEndpoint(t, r, cleartext(b), nil)
	sampledSpan(t, r, "/b")
	if err := r.Tracer().ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(a.Spans()) != 1 || len(b.Spans()) != 1 {
		t.Errorf("spans: A %d, B %d; want 1 and 1", len(a.Spans()), len(b.Spans()))
	}
	if !eventually(ctx, func() bool { return a.Stats().OpenConnections == 0 }) {
		t.Errorf("A's connection stayed open: %+v", a.Stats())
	}

	applyEndpoint(t, r, nil, nil)
	if r.Tracer().SpansEnabled() || r.pipe != nil {
		t.Error("spans still enabled without an endpoint")
	}
	if v := value(t, scrapeAfter(t, r), catalog.SecurityCleartextHops, "hop", "telemetry"); v != 0 {
		t.Errorf("cleartext telemetry hop without export = %v", v)
	}
}

// TestShutdownFlushesEverySignal checks that Shutdown hands queued logs
// and spans to the collector and makes a final metric export.
func TestShutdownFlushesEverySignal(t *testing.T) {
	sink := startSink(t, otlpsink.Config{})
	r, _ := newRuntime(t, nil)
	applyEndpoint(t, r, cleartext(sink), nil)
	r.Logger("drain").Info("draining")
	sampledSpan(t, r, "/last")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := r.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if len(sink.Spans()) != 1 || len(sink.Logs()) == 0 || len(sink.MetricNames()) == 0 {
		t.Errorf("after Shutdown: %d spans, %d logs, %d metrics", len(sink.Spans()), len(sink.Logs()), len(sink.MetricNames()))
	}
	if !eventually(waitCtx(t), func() bool { return sink.Stats().OpenConnections == 0 }) {
		t.Errorf("connection open after Shutdown: %+v", sink.Stats())
	}
}
