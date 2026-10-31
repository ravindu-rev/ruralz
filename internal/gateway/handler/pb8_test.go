// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/composition"
	"github.com/ravindu-rev/ruralz/internal/gateway/exchange"
	"github.com/ravindu-rev/ruralz/internal/gateway/executor"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/gateway/tap"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/secret/resolver"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/manager"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/internal/telemetry"
)

// PB-8: Ruralz-owned allocations per S1 request, net/http excluded, 30 or
// fewer (target), measured by testing.AllocsPerRun over one whole S1
// request (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Budget catalog and SLO ties" and "Regression policy and gates"): a
// pre-parsed HTTP/1.1 request through the exchange, the Router, the Filter
// Chain executor and the Upstream leg on a canned transport, to the
// response write into a discard ResponseWriter and onLog with metrics, an
// unsampled trace and the access log. The Node is assembled from the parts
// the ruralzd wiring builds (internal/gateway buildParts), its snapshot
// compiled from the S1 Bundle by the snapshot compiler; only net/http is
// replaced: no server parses the request, the canned transport answers the
// leg, and the discard writer takes the response. test/bench/allocgate.json
// caps BenchmarkS1Request at 30 and keeps it under the 3% rule.

// pb8MaxAllocs is PB-8's cap.
const pb8MaxAllocs = 30

// s1Bundle is the S1 scenario of PBB "Scenarios": the S2 Bundle without the
// Gateway Policies, with the mock Upstream at an address the canned
// transport stands in for, no OTLP export (the access log still writes
// every record) and every trace unsampled.
const s1Bundle = `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: bench
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
  admin:
    port: 9901
  telemetry:
    traceSampling: 0
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: mock
spec:
  protocol: http
  endpoints:
    - address: "10.0.0.1:8080"
  loadBalancing: {algorithm: round-robin}
  circuitBreaker:
    maxConnections: 65536
    maxPendingRequests: 1024
  timeout: 2s
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: bench-1k
spec:
  match:
    path: {exact: /bench/1k}
    methods: [GET]
  upstreams:
    - name: mock
  timeout: 5s
`

// s1Body is the mock's 1 KiB response.
var s1Body = bytes.Repeat([]byte("r"), 1024)

// cannedS1 is the canned transport of the mock Upstream: every attempt gets
// the same 200 response with a 1 KiB body, rebuilt in place, so it
// allocates nothing and the leg's own allocations are counted. Requests
// run one at a time.
type cannedS1 struct {
	resp http.Response
	body cannedBody
	hdr  http.Header
	ct   []string
	cl   []string
}

// cannedBody reads s1Body once per attempt.
type cannedBody struct{ off int }

func (b *cannedBody) Read(p []byte) (int, error) {
	if b.off >= len(s1Body) {
		return 0, io.EOF
	}
	n := copy(p, s1Body[b.off:])
	b.off += n
	return n, nil
}

func (*cannedBody) Close() error { return nil }

func newCannedS1() *cannedS1 {
	return &cannedS1{hdr: http.Header{}, ct: []string{"application/octet-stream"}, cl: []string{"1024"}}
}

func (c *cannedS1) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_ = r.Body.Close()
	}
	clear(c.hdr)
	c.hdr["Content-Type"], c.hdr["Content-Length"] = c.ct, c.cl
	c.body.off = 0
	c.resp = http.Response{
		Status: "200 OK", StatusCode: http.StatusOK, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: c.hdr, Body: &c.body, ContentLength: int64(len(s1Body)), Request: r,
	}
	return &c.resp, nil
}

// discardWriter is the discard ResponseWriter: it keeps one header map,
// cleared before each request, takes the deadlines the handler sets
// through http.ResponseController and drops the body.
type discardWriter struct {
	h      http.Header
	status int
	n      int
}

func (w *discardWriter) Header() http.Header { return w.h }
func (w *discardWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *discardWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.n += len(p)
	return len(p), nil
}
func (*discardWriter) Flush()                           {}
func (*discardWriter) SetReadDeadline(time.Time) error  { return nil }
func (*discardWriter) SetWriteDeadline(time.Time) error { return nil }

// accessSignal is the telemetry runtime's stdout: it drops every line and
// signals each access line, so a measured request includes the encoding
// of its access record on the stdout worker.
type accessSignal struct{ ch chan struct{} }

func (s *accessSignal) Write(p []byte) (int, error) {
	for n := bytes.Count(p, []byte(`"msg":"access"`)); n > 0; n-- {
		s.ch <- struct{}{}
	}
	return len(p), nil
}

// s1Node is a Node serving the S1 Bundle, as the ruralzd wiring builds it.
type s1Node struct {
	h      *Handler
	rt     *telemetry.Runtime
	conn   *handlertest.Conn
	req    *http.Request
	w      *discardWriter
	access *accessSignal
}

// newS1Node assembles the Node and publishes the compiled S1 snapshot.
func newS1Node(tb testing.TB) *s1Node {
	tb.Helper()
	ctx := tb.Context()
	clk := clock.Real()
	access := &accessSignal{ch: make(chan struct{}, 1024)}
	rt, err := telemetry.New(telemetry.Options{
		Service: telemetry.ServiceGateway, NodeID: "01JBENCH000000000000000000", Version: "0.0.0-bench",
		Stdout: access, Level: slog.LevelError, Clock: clk,
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = rt.Shutdown(context.Background()) })
	meter, nm := rt.Meter(), rt.Meter().Node()
	celc, err := cel.New()
	if err != nil {
		tb.Fatal(err)
	}
	pipe, err := pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: celc})
	if err != nil {
		tb.Fatal(err)
	}
	filters, err := builtin.New(builtin.Options{Clock: clk, Logger: rt.Logger("filter"), Status: rt, Counter: meter.Counter})
	if err != nil {
		tb.Fatal(err)
	}
	res, err := resolver.NewResolver(resolver.Config{Root: tb.TempDir(), Clock: clk, Logger: rt.Logger("secret"), Status: rt})
	if err != nil {
		tb.Fatal(err)
	}
	stores := manager.New(ctx, manager.Config{
		Deps:    statestore.Deps{Clock: clk, Metrics: &nm.State, Status: rt, NodeID: "01JBENCH000000000000000000"},
		Openers: manager.Openers{Memory: memory.Open},
	})
	tb.Cleanup(func() { _ = stores.Close(context.Background()) })
	ups := upstream.NewManager(upstream.Options{Clock: clk, Status: rt, Tracer: rt.Tracer(), Send: newCannedS1()})
	units := admission.NewUnits(admission.DefaultUnits)
	budget := body.NewBudget(body.DefaultMaxBufferedBytes, nm.BufferedBytes)
	engine := composition.New(composition.Deps{Clock: clk, Budget: budget, Units: units, Decoder: celc, Logger: rt.Logger("composition")})
	exchanges := exchange.NewPool(exchange.Deps{Budget: budget, Decoder: celc})
	exec := executor.New(executor.Deps{Clock: clk, Tracer: rt.Tracer(), Logger: rt.Logger("filter")})
	holder := retire.New(retire.Config{
		Clock: clk, Wall: clk, Logger: rt.Logger("retire"), Stripes: meter.Stripes(), NewStripe: meter.NewStripe,
		RetiredSnapshots: nm.Config.RetiredSnapshots, RetirementEnded: nm.Config.RetirementEnded, Status: rt,
	})
	compiler, err := compile.New(compile.Deps{
		Pipeline: pipe, CEL: celc, Filters: filters, Secrets: res, StateStores: stores, Upstreams: ups,
		Composition: engine, Meter: meter, Status: rt, Clock: clk, Logger: rt.Logger("compile"),
	})
	if err != nil {
		tb.Fatal(err)
	}
	fsys := fstest.MapFS{"ruralz.yaml": {Data: []byte(s1Bundle)}}
	pr, err := pipe.Run(ctx, pipeline.Request{Source: loader.Source{FS: fsys, Path: "."}})
	if err != nil || pr.Validated == nil {
		tb.Fatalf("S1 Bundle: %v %v", err, pr.Diagnostics)
	}
	c, ds, err := compiler.Compile(ctx, compile.Input{Validated: pr.Validated, Files: pr.Files})
	if err != nil || len(ds) > 0 {
		tb.Fatalf("compile S1: %v %v", err, ds)
	}
	if _, err := holder.Publish(c.Snapshot); err != nil {
		tb.Fatal(err)
	}
	c.Activate()
	h, err := New(Deps{
		Snapshots: holder, Units: units, PreRouting: admission.NewPreRouting(admission.DefaultPreRouting, clk),
		Exchanges: exchanges, Executor: exec, Tracer: rt.Tracer(), AccessLog: rt.AccessLog(), Node: nm,
		Tap: tap.New(tap.Config{Dropped: nm.TapEventsDropped, Clock: clk}), Clock: clk, Logger: rt.Logger("handler"),
	})
	if err != nil {
		tb.Fatal(err)
	}
	conn := handlertest.NewConn("http", "10.1.0.7:40000")
	conn.StripeV = meter.NewStripe()
	h.connOf = func(context.Context) Conn { return conn }
	req := (&http.Request{
		Method: http.MethodGet, URL: &url.URL{Path: "/bench/1k"}, RequestURI: "/bench/1k",
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Host: "bench.example:8080", RemoteAddr: "10.1.0.7:40000",
		Header: http.Header{"User-Agent": {"oha/1.16.0"}, "Accept": {"*/*"}}, Body: http.NoBody,
	}).WithContext(ctx)
	return &s1Node{h: h, rt: rt, conn: conn, req: req, w: &discardWriter{h: http.Header{}}, access: access}
}

// serve runs one whole S1 request and waits until its access record was
// written.
func (n *s1Node) serve() {
	clear(n.w.h)
	n.w.status, n.w.n = 0, 0
	n.h.ServeHTTP(n.w, n.req)
	<-n.access.ch
}

// TestS1RequestAllocations is PB-8's gate: one whole S1 request allocates
// at most 30 times once the pools are warm, and answers the canned 1 KiB
// body.
func TestS1RequestAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items under -race; the allocation gate runs without it")
	}
	n := newS1Node(t)
	for range 200 {
		n.serve()
	}
	if n.w.status != http.StatusOK || n.w.n != len(s1Body) {
		t.Fatalf("S1 request answered %d with %d bytes, want 200 with %d", n.w.status, n.w.n, len(s1Body))
	}
	if got := n.req.Header; len(got) != 2 || strings.Join(got["Accept"], ",") != "*/*" {
		t.Fatalf("the handler changed the pre-parsed request's header: %v", got)
	}
	got := testing.AllocsPerRun(1000, n.serve)
	t.Logf("allocations per S1 request: %.1f (PB-8: %d)", got, pb8MaxAllocs)
	if got > pb8MaxAllocs {
		t.Errorf("allocations per S1 request = %.1f, want at most %d (PB-8)", got, pb8MaxAllocs)
	}
}

// BenchmarkS1Request is PB-8's benchmark, which the alloc/op gate runs
// (test/bench/allocgate.json).
func BenchmarkS1Request(b *testing.B) {
	n := newS1Node(b)
	for range 200 {
		n.serve()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		n.serve()
	}
}
