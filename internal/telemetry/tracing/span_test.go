// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// TestNoopSpans: the no-op spans ignore every call (spec 09 req 7).
func TestNoopSpans(t *testing.T) {
	d := emit.Decision{ServerSpanID: [8]byte{1, 2, 3}}
	for _, s := range []emit.Span{noopSpan{}, (*noopServer)(&d)} {
		s.SetAttr("k", slog.StringValue("v"))
		s.End(http.StatusInternalServerError, "RZ-RT-006", "cel")
	}
	if (noopSpan{}).SpanID() != ([8]byte{}) || (*noopServer)(&d).SpanID() != d.ServerSpanID {
		t.Fatal("no-op span IDs")
	}
}

func TestProtocolVersion(t *testing.T) {
	for in, want := range map[string]string{
		"http1": "1.1", "HTTP/1.1": "1.1", "1.1": "1.1", "HTTP/1.0": "1.0", "1.0": "1.0",
		"http2": "2", "HTTP/2.0": "2", "HTTP/2": "2", "2": "2", "": "", "h3": "h3",
	} {
		if got := protocolVersion(in); got != want {
			t.Errorf("protocolVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitEndpoint(t *testing.T) {
	for _, tc := range []struct {
		in   string
		host string
		port int
	}{
		{"10.0.0.1:8080", "10.0.0.1", 8080},
		{"[2001:db8::1]:443", "2001:db8::1", 443},
		{"svc.internal:0", "svc.internal", 0},
		{"svc.internal:70000", "svc.internal", 0},
		{"svc.internal:http", "svc.internal", 0},
		{"svc.internal", "svc.internal", 0},
	} {
		if h, p := splitEndpoint(tc.in); h != tc.host || p != tc.port {
			t.Errorf("splitEndpoint(%q) = %q %d, want %q %d", tc.in, h, p, tc.host, tc.port)
		}
	}
}

func TestPathOnly(t *testing.T) {
	for in, want := range map[string]string{"/a": "/a", "/a?b=c": "/a", "?x": "", "": ""} {
		if got := pathOnly(in); got != want {
			t.Errorf("pathOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestProcessorDefaults: zero options take the defaults, nil counters
// count nothing, OnStart does nothing.
func TestProcessorDefaults(t *testing.T) {
	p := NewProcessor(ProcessorOptions{})
	if p.batchSize != DefaultBatchSize || p.interval != DefaultBatchInterval || p.timeout != DefaultExportTimeout || cap(p.queue) != DefaultQueueSize {
		t.Fatalf("defaults %d %v %v %d", p.batchSize, p.interval, p.timeout, cap(p.queue))
	}
	if p.Exporter() != nil {
		t.Fatal("exporter set")
	}
	p.OnStart(context.Background(), nil)
	p.OnEnd(ended(1, "s")[0])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestTracerShutdownDeadline: a Tracer whose collector stalls returns
// from Shutdown at its deadline with the cause (spec 09 req 25).
func TestTracerShutdownDeadline(t *testing.T) {
	tr, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	exp := newRecordExporter()
	exp.block = make(chan struct{})
	tr.SetExporter(exp)
	var d emit.Decision
	tr.Decide(nil, 1, &d)
	_, s := tr.StartServer(context.Background(), &d, http.MethodGet, emit.ServerAttrs{})
	s.End(http.StatusOK, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := tr.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v", err)
	}
	if tr.SpansEnabled() {
		t.Fatal("spans still enabled after Shutdown")
	}
}

// TestTracerShutdownDoneContext covers spec 09 req 25 and the goroutine
// stop path of architecture section 0 item 2: Shutdown with a context that
// is already done still stops the worker, counts the queued span as an
// export_error drop and shuts the exporter down once; a retry is a no-op.
func TestTracerShutdownDoneContext(t *testing.T) {
	var c testCounters
	tr, err := New(Options{Counters: c.wire()})
	if err != nil {
		t.Fatal(err)
	}
	exp := newRecordExporter()
	tr.SetExporter(exp)
	var d emit.Decision
	tr.Decide(nil, 1, &d)
	_, s := tr.StartServer(context.Background(), &d, http.MethodGet, emit.ServerAttrs{})
	s.End(http.StatusOK, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tr.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown = %v, want context canceled", err)
	}
	select {
	case <-tr.proc.done:
	default:
		t.Fatal("worker still running after Shutdown returned")
	}
	if !tr.proc.closed.Load() || tr.SpansEnabled() {
		t.Fatal("processor still open or spans still enabled")
	}
	if got := exp.shut.Load(); got != 1 {
		t.Fatalf("exporter Shutdown calls = %d, want 1", got)
	}
	if spans, dropped := c.spans.Load(), c.export.Load(); spans != 1 || dropped != 1 {
		t.Fatalf("spans_total %d, export_error %d, want 1 and 1", spans, dropped)
	}
	live, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := tr.Shutdown(live); err != nil {
		t.Fatalf("second Shutdown = %v", err)
	}
	if got := exp.shut.Load(); got != 1 {
		t.Fatalf("exporter Shutdown calls after retry = %d, want 1", got)
	}
	// The provider is shut down too: it hands no later span to the
	// processor.
	_, late := tr.tracer.Start(context.Background(), "late")
	late.End()
	if got := c.spans.Load(); got != 1 {
		t.Fatalf("span reached the processor after Shutdown: spans_total %d", got)
	}
}

// TestSpanAttributeBuffer: a repeated key replaces the buffered value, and
// a span without attributes ends cleanly.
func TestSpanAttributeBuffer(t *testing.T) {
	tr, mem := newMemTracer(t, Options{})
	var d emit.Decision
	tr.Decide(nil, 1, &d)
	sctx, s := tr.StartServer(context.Background(), &d, http.MethodGet, emit.ServerAttrs{})
	_, m := tr.StartRouteMatch(sctx)
	m.End(0, "", "")
	s.SetAttr(catalog.AttrRoute, slog.StringValue("first"))
	s.SetAttr(catalog.AttrRoute, slog.StringValue("second"))
	s.End(http.StatusOK, "", "")
	spans := flushed(t.Context(), t, tr, mem)
	server, ok := byName(spans, "GET second")
	if !ok || attrs(server)[catalog.AttrRoute].AsString() != "second" {
		t.Fatalf("spans %v", names(spans))
	}
	match, _ := byName(spans, catalog.SpanRouteMatch)
	if len(match.Attributes) != 0 {
		t.Fatalf("match attributes %v", match.Attributes)
	}
}
