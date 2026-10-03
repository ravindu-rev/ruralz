// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package otlpsink

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Tests for WP-84 otlpsink (11 section 3, F 32, req 39; 09 tests 33, 34,
// 36 to 38; R-53). "Done when": the sink receives each signal from an
// OTLP gRPC client built on the proto module (TestReceivesEachSignal).

func TestReceivesEachSignal(t *testing.T) { // WP-84 Done when; 09 test 36 (TLS with a test CA)
	p := newTestPKI(t)
	for _, tc := range []struct {
		name      string
		serverTLS bool
		mutual    bool
		scheme    string
	}{
		{name: "h2c", scheme: "http://"},
		{name: "tls", serverTLS: true, scheme: "https://"},
		{name: "mtls", serverTLS: true, mutual: true, scheme: "https://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{}
			clientTLS := p.client(tc.mutual)
			if tc.serverTLS {
				c.TLS = p.server(tc.mutual)
			} else {
				clientTLS = nil
			}
			s := start(t, c)
			if got, want := s.URL(), tc.scheme+s.Addr(); got != want {
				t.Fatalf("URL = %q, want %q", got, want)
			}
			cc := dial(t, s, clientTLS)
			ctx := metadata.AppendToOutgoingContext(ctxTimeout(t, 10*time.Second), "x-test-header", "v1")
			if err := exportAll(ctx, cc, "rzcanary-marker"); err != nil {
				t.Fatal(err)
			}
			st := s.Stats()
			if st.TraceRequests != 1 || st.MetricRequests != 1 || st.LogRequests != 1 || st.Rejected != 0 || st.Dropped != 0 {
				t.Fatalf("Stats = %+v", st)
			}
			reqs := s.Requests()
			if len(reqs) != 3 {
				t.Fatalf("got %d requests, want 3", len(reqs))
			}
			for i, want := range []Signal{Traces, Metrics, Logs} {
				r := reqs[i]
				if r.Signal != want || r.Seq != uint64(i+1) {
					t.Errorf("request %d: signal %v seq %d, want %v seq %d", i, r.Signal, r.Seq, want, i+1)
				}
				if (r.Traces != nil) != (want == Traces) || (r.Metrics != nil) != (want == Metrics) || (r.Logs != nil) != (want == Logs) {
					t.Errorf("request %d: decoded fields do not match signal %v", i, want)
				}
				if r.Peer == "" || r.Received.IsZero() || r.Conn != 1 {
					t.Errorf("request %d: peer %q received %v conn %d", i, r.Peer, r.Received, r.Conn)
				}
				if (r.TLS != nil) != tc.serverTLS {
					t.Errorf("request %d: TLS state %v, want TLS %v", i, r.TLS != nil, tc.serverTLS)
				}
				if tc.mutual && (r.TLS == nil || len(r.TLS.PeerCertificates) != 1) {
					t.Errorf("request %d: no client certificate recorded", i)
				}
				extra := r.ExtraMetadata()
				if !slices.Equal(extra["x-test-header"], []string{"v1"}) || len(extra) != 1 {
					t.Errorf("request %d: ExtraMetadata = %v, want only x-test-header", i, extra)
				}
				if len(r.Metadata["content-type"]) == 0 {
					t.Errorf("request %d: Metadata lacks content-type: %v", i, r.Metadata)
				}
			}
			if !bytes.Contains(reqs[0].Raw, []byte("rzcanary-marker")) || !bytes.Contains(reqs[2].Raw, []byte("rzcanary-marker")) {
				t.Error("raw bytes lack the marker (11 req 39 canary scans)")
			}
			if got := s.SpanNames(s.TraceIDs()[0]); !slices.Equal(got, []string{"GET orders"}) {
				t.Errorf("SpanNames = %v", got)
			}
			if got := s.MetricNames(); !slices.Equal(got, []string{"ruralz_http_requests_total"}) {
				t.Errorf("MetricNames = %v", got)
			}
			if logs := s.Logs(); len(logs) != 1 || logs[0].EventName() != "ruralz.access" {
				t.Errorf("Logs = %v", logs)
			}
			if got := s.RequestsOf(Metrics); len(got) != 1 || got[0].Signal != Metrics {
				t.Errorf("RequestsOf(Metrics) = %v", got)
			}
		})
	}
}

func TestRawIsWireEncoding(t *testing.T) { // 11 req 39: raw bytes for canary scans are the client's encoding
	s := start(t, Config{})
	cc := dial(t, s, nil)
	req := traceReq("svc",
		span(2, 1, 0, "POST pay", tracepb.Span_SPAN_KIND_SERVER, 0, str("url.path", "/pay")),
		span(2, 2, 1, "ruralz.filter.auth-jwt", tracepb.Span_SPAN_KIND_INTERNAL, 1))
	if _, err := coltrace.NewTraceServiceClient(cc).Export(ctxTimeout(t, 10*time.Second), req); err != nil {
		t.Fatal(err)
	}
	enc, err := encoding.GetCodecV2("proto").Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want := enc.Materialize()
	enc.Free()
	got := s.Requests()[0].Raw
	if !bytes.Equal(got, want) {
		t.Fatalf("Raw differs from the proto encoding:\n got %x\nwant %x", got, want)
	}
}

func TestCodecCapture(t *testing.T) { // 11 req 39: the wire bytes travel with the decoded message
	c, err := newRawCodec()
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != "proto" {
		t.Fatalf("Name = %q", c.Name())
	}
	req := traceReq("svc", span(1, 1, 0, "x", tracepb.Span_SPAN_KIND_SERVER, 0))
	enc, err := c.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Free()
	want := enc.Materialize()

	cp := &capture{msg: new(coltrace.ExportTraceServiceRequest)}
	if err := c.Unmarshal(enc, cp); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cp.raw, want) {
		t.Fatalf("capture raw = %x, want %x", cp.raw, want)
	}
	if got := cp.msg.(*coltrace.ExportTraceServiceRequest); len(got.GetResourceSpans()) != 1 {
		t.Fatalf("capture message = %v", got)
	}
	// A bare message decodes without a copy of the bytes.
	var bare coltrace.ExportTraceServiceRequest
	if err := c.Unmarshal(enc, &bare); err != nil || len(bare.GetResourceSpans()) != 1 {
		t.Fatalf("bare Unmarshal = %v, %v", &bare, err)
	}
	// A decoding failure leaves the capture without bytes.
	bad := &capture{msg: "not a message"}
	if err := c.Unmarshal(enc, bad); err == nil || bad.raw != nil {
		t.Fatalf("Unmarshal into a non-message: err %v raw %x", err, bad.raw)
	}
}

// TestCollectorServicesMatchGenerated guards the hand-written Export
// handlers against the generated service descriptors: one unary Export
// method per service, decoding the same request type.
func TestCollectorServicesMatchGenerated(t *testing.T) {
	errStop := errors.New("stop")
	for _, svc := range collectorServices() {
		t.Run(svc.signal.String(), func(t *testing.T) {
			d := svc.desc
			if len(d.Methods) != 1 || d.Methods[0].MethodName != exportMethod || len(d.Streams) != 0 {
				t.Fatalf("%s: methods %v streams %v", d.ServiceName, d.Methods, d.Streams)
			}
			var decoded string
			_, err := d.Methods[0].Handler(nil, context.Background(), func(v any) error {
				decoded = fmt.Sprintf("%T", v)
				return errStop
			}, nil)
			if !errors.Is(err, errStop) {
				t.Fatalf("generated handler: %v", err)
			}
			if want := fmt.Sprintf("%T", svc.newReq()); decoded != want {
				t.Fatalf("generated handler decodes %s, ours %s", decoded, want)
			}
		})
	}
}

// TestExportHandlerDecodeFailure is the regression test for exports gRPC
// decodes but whose stream then fails (the client cancels, RST_STREAM,
// Stop): the handler returns the error, records nothing and keeps no
// state, since the bytes live in the capture only.
func TestExportHandlerDecodeFailure(t *testing.T) {
	s := start(t, Config{})
	enc, err := s.codec.Marshal(traceReq("svc", span(1, 1, 0, "lost", tracepb.Span_SPAN_KIND_SERVER, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Free()
	streamErr := status.Error(codes.Canceled, "context canceled")
	for _, svc := range collectorServices() {
		var cp *capture
		_, err := s.exportHandler(svc)(s, context.Background(), func(v any) error {
			cp = v.(*capture)
			if err := s.codec.Unmarshal(enc, v); err != nil && svc.signal == Traces {
				t.Fatal(err)
			}
			return streamErr
		}, nil)
		if !errors.Is(err, streamErr) {
			t.Fatalf("%v: handler err = %v, want the stream error", svc.signal, err)
		}
		if svc.signal == Traces && len(cp.raw) == 0 {
			t.Fatal("the capture did not receive the bytes")
		}
	}
	if st := s.Stats(); st != (Stats{}) || len(s.Requests()) != 0 {
		t.Fatalf("a failed decode was counted or recorded: %+v", st)
	}
}

// TestCanceledExportsKeepRawExact churns the modes while clients with
// short deadlines export, so gRPC decodes many requests whose handlers
// never run or end early (the case that leaked raw payloads, pinned
// decoded messages and finally made Raw fall back to re-encoding); every
// recorded request still carries its own wire bytes, and Close ends every
// connection.
func TestCanceledExportsKeepRawExact(t *testing.T) { // 11 req 39
	s := start(t, Config{})
	cc := dial(t, s, nil)
	client := coltrace.NewTraceServiceClient(cc)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range byte(8) {
		wg.Go(func() {
			for i := uint64(0); ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				_, _ = client.Export(ctx, traceReq(fmt.Sprintf("w%d-%d", w, i), span(w+1, 1, 0, "churn", tracepb.Span_SPAN_KIND_SERVER, i)))
				cancel()
			}
		})
	}
	modes := []Mode{Stall{}, Accept{}, Reject{Code: codes.Unavailable}, Down{}, Accept{}}
	for i := range 20 {
		if err := s.SetMode(modes[i%len(modes)]); err != nil {
			close(stop)
			wg.Wait()
			t.Fatal(err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	if err := s.SetMode(Accept{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Export(ctxTimeout(t, 20*time.Second), traceReq("final"), grpcWaitForReady()); err != nil {
		t.Fatal(err)
	}
	reqs := s.Requests()
	if len(reqs) == 0 {
		t.Fatal("nothing recorded")
	}
	codec := encoding.GetCodecV2("proto")
	for _, r := range reqs {
		enc, err := codec.Marshal(r.Traces)
		if err != nil {
			t.Fatal(err)
		}
		if want := enc.Materialize(); !bytes.Equal(r.Raw, want) {
			t.Fatalf("request %d: Raw differs from its encoding", r.Seq)
		}
		enc.Free()
	}
	_ = s.Close()
	if st := s.Stats(); st.OpenConnections != 0 || st.Stalled != 0 {
		t.Fatalf("after Close: %+v", st)
	}
}

func TestConnections(t *testing.T) { // 09 test 40: the old connection closes
	s := start(t, Config{})
	export := func(cc *grpc.ClientConn, marker string) Request {
		t.Helper()
		if _, err := coltrace.NewTraceServiceClient(cc).Export(ctxTimeout(t, 20*time.Second), traceReq(marker), grpcWaitForReady()); err != nil {
			t.Fatal(err)
		}
		reqs := s.Requests()
		return reqs[len(reqs)-1]
	}
	cc1 := dial(t, s, nil)
	cc2 := dial(t, s, nil)
	if r := export(cc1, "a"); r.Conn != 1 {
		t.Fatalf("first connection Conn = %d", r.Conn)
	}
	if r := export(cc1, "b"); r.Conn != 1 {
		t.Fatalf("same connection Conn = %d", r.Conn)
	}
	if r := export(cc2, "c"); r.Conn != 2 {
		t.Fatalf("second connection Conn = %d", r.Conn)
	}
	if st := s.Stats(); st.Connections != 2 || st.OpenConnections != 2 {
		t.Fatalf("Stats = %+v, want 2 opened and open", st)
	}
	_ = cc1.Close()
	waitFor(t, func() bool { return s.Stats().OpenConnections == 1 })

	// Down ends every connection before SetMode returns; numbering
	// continues on the next server.
	if err := s.SetMode(Down{}); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.OpenConnections != 0 || st.Connections != 2 {
		t.Fatalf("after Down: %+v", st)
	}
	// cc2 may not have read the end of the connection Down closed yet. An
	// export on that dead transport fails with Unavailable, which
	// wait-for-ready does not cover, so wait until the client sees it.
	ctx := ctxTimeout(t, 20*time.Second)
	for st := cc2.GetState(); st == connectivity.Ready; st = cc2.GetState() {
		if !cc2.WaitForStateChange(ctx, st) {
			t.Fatal("the client never saw Down close its connection")
		}
	}
	if err := s.SetMode(Accept{}); err != nil {
		t.Fatal(err)
	}
	if r := export(cc2, "d"); r.Conn != 3 {
		t.Fatalf("connection after Down Conn = %d", r.Conn)
	}
	_ = cc2.Close()
	waitFor(t, func() bool { return s.Stats().OpenConnections == 0 })
	if st := s.Stats(); st.Connections != 3 {
		t.Fatalf("Stats = %+v, want 3 opened", st)
	}
}

func TestReject(t *testing.T) { // 09 test 37: collector failing exports
	s := start(t, Config{Mode: Reject{Code: codes.Unavailable, Message: "try later"}})
	cc := dial(t, s, nil)
	ctx := ctxTimeout(t, 10*time.Second)
	_, err := coltrace.NewTraceServiceClient(cc).Export(ctx, traceReq("svc"))
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "try later") {
		t.Fatalf("err = %v, want Unavailable try later", err)
	}
	if err := s.SetMode(Reject{Code: codes.InvalidArgument}); err != nil {
		t.Fatal(err)
	}
	_, err = collogs.NewLogsServiceClient(cc).Export(ctx, logReq("svc"))
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "otlpsink: rejected") {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
	if st := s.Stats(); st.Rejected != 2 || st.TraceRequests != 0 || st.LogRequests != 0 {
		t.Fatalf("Stats = %+v", st)
	}
	if len(s.Requests()) != 0 {
		t.Fatal("rejected exports were recorded")
	}
	// Recovery.
	if err := s.SetMode(Accept{}); err != nil {
		t.Fatal(err)
	}
	if _, err := colmetrics.NewMetricsServiceClient(cc).Export(ctx, metricReq("svc", counter("a", 1))); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.MetricRequests != 1 {
		t.Fatalf("Stats = %+v", st)
	}
}

func TestStall(t *testing.T) { // 09 test 38: collector stalls
	s := start(t, Config{Mode: Stall{}})
	cc := dial(t, s, nil)
	client := coltrace.NewTraceServiceClient(cc)

	// An export with a deadline is held until the deadline passes.
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err := client.Export(short, traceReq("svc"))
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(began); d < 90*time.Millisecond {
		t.Fatalf("stalled export returned after %v", d)
	}
	// The server sees the cancellation shortly after the client.
	waitFor(t, func() bool { st := s.Stats(); return st.Stalled == 0 && st.Rejected == 1 })

	// Held exports get the next mode's answer.
	done := make(chan error, 1)
	go func() {
		_, err := client.Export(ctxTimeout(t, 10*time.Second), traceReq("svc", span(3, 1, 0, "held", tracepb.Span_SPAN_KIND_SERVER, 0)))
		done <- err
	}()
	waitFor(t, func() bool { return s.Stats().Stalled == 1 })
	if _, ok := s.Mode().(Stall); !ok {
		t.Fatalf("Mode = %v", s.Mode())
	}
	if err := s.SetMode(Stall{}); err != nil { // no-op
		t.Fatal(err)
	}
	if err := s.SetMode(Accept{}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if names := s.SpanNames(s.TraceIDs()[0]); !slices.Equal(names, []string{"held"}) {
		t.Fatalf("SpanNames = %v", names)
	}
	st := s.Stats()
	if st.Stalled != 0 || st.Rejected != 1 || st.TraceRequests != 1 {
		t.Fatalf("Stats = %+v", st)
	}

	// Stall again, then Reject: the held export is rejected.
	if err := s.SetMode(Stall{}); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := client.Export(ctxTimeout(t, 10*time.Second), traceReq("svc"))
		done <- err
	}()
	waitFor(t, func() bool { return s.Stats().Stalled == 1 })
	if err := s.SetMode(Reject{Code: codes.ResourceExhausted}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("err = %v, want ResourceExhausted", err)
	}
}

func TestDown(t *testing.T) { // 09 test 37: collector down, then recovery on the same address
	s := start(t, Config{})
	addr := s.Addr()
	cc := dial(t, s, nil)
	client := coltrace.NewTraceServiceClient(cc)
	ctx := ctxTimeout(t, 20*time.Second)
	if _, err := client.Export(ctx, traceReq("svc")); err != nil {
		t.Fatal(err)
	}
	// A held export ends when the server goes down.
	if err := s.SetMode(Stall{}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := client.Export(ctx, traceReq("svc"))
		done <- err
	}()
	waitFor(t, func() bool { return s.Stats().Stalled == 1 })
	if err := s.SetMode(Down{}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("held export succeeded across Down")
	}
	if err := s.SetMode(Down{}); err != nil { // no-op
		t.Fatal(err)
	}
	// Dials are refused while down.
	_, err := client.Export(ctxTimeout(t, time.Second), traceReq("svc"), grpcFailFast())
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("export while down: %v, want Unavailable", err)
	}
	if err := s.SetMode(Accept{}); err != nil {
		t.Fatal(err)
	}
	if s.Addr() != addr {
		t.Fatalf("Addr changed from %s to %s", addr, s.Addr())
	}
	// The client reconnects to the same address.
	if _, err := client.Export(ctx, traceReq("svc"), grpcWaitForReady()); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.TraceRequests != 2 {
		t.Fatalf("Stats = %+v", st)
	}
}

func TestStartDown(t *testing.T) {
	s := start(t, Config{Mode: Down{}})
	cc := dial(t, s, nil)
	client := coltrace.NewTraceServiceClient(cc)
	if _, err := client.Export(ctxTimeout(t, time.Second), traceReq("svc"), grpcFailFast()); status.Code(err) != codes.Unavailable {
		t.Fatalf("export to a sink started Down: %v", err)
	}
	if err := s.SetMode(Accept{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Export(ctxTimeout(t, 20*time.Second), traceReq("svc"), grpcWaitForReady()); err != nil {
		t.Fatal(err)
	}
}

func TestDownRelistenFails(t *testing.T) {
	s := start(t, Config{})
	if err := s.SetMode(Down{}); err != nil {
		t.Fatal(err)
	}
	// Take the port meanwhile.
	ln, err := listen(context.Background(), s.Addr())
	if err != nil {
		t.Skipf("port not reusable at once: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if err := s.SetMode(Accept{}); err == nil {
		t.Fatal("SetMode(Accept) succeeded with the port taken")
	}
	if _, ok := s.Mode().(Down); !ok {
		t.Fatalf("Mode = %v, want Down", s.Mode())
	}
}

func TestLimits(t *testing.T) { // bounded recording
	for _, tc := range []struct {
		name string
		c    Config
	}{
		{name: "MaxRequests", c: Config{MaxRequests: 2}},
		{name: "MaxBytes", c: Config{MaxBytes: 2*limitReqSize(t) + 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := start(t, tc.c)
			cc := dial(t, s, nil)
			client := coltrace.NewTraceServiceClient(cc)
			ctx := ctxTimeout(t, 10*time.Second)
			for i := range byte(3) {
				if _, err := client.Export(ctx, limitReq(i)); err != nil {
					t.Fatal(err)
				}
			}
			st := s.Stats()
			if st.TraceRequests != 3 || st.Dropped != 1 {
				t.Fatalf("Stats = %+v, want 3 answered and 1 dropped", st)
			}
			if n := len(s.Requests()); n != 2 {
				t.Fatalf("recorded %d requests, want 2", n)
			}
			s.Reset()
			if len(s.Requests()) != 0 || len(s.Spans()) != 0 {
				t.Fatal("Reset kept recordings")
			}
			if _, err := client.Export(ctx, traceReq("svc")); err != nil {
				t.Fatal(err)
			}
			if n := len(s.Requests()); n != 1 {
				t.Fatalf("after Reset recorded %d requests, want 1", n)
			}
		})
	}
}

// limitReq is the i-th request of TestLimits; all have the same size.
func limitReq(i byte) *coltrace.ExportTraceServiceRequest {
	return traceReq("svc", span(i+1, 1, 0, fmt.Sprintf("s%d", i), tracepb.Span_SPAN_KIND_SERVER, 0))
}

func limitReqSize(t *testing.T) int64 {
	t.Helper()
	enc, err := encoding.GetCodecV2("proto").Marshal(limitReq(0))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Free()
	return int64(enc.Len())
}

func TestWaitHelpers(t *testing.T) {
	s := start(t, Config{})
	cc := dial(t, s, nil)
	ctx := ctxTimeout(t, 10*time.Second)

	// Deadline.
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.WaitSpans(short, 1, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitSpans err = %v, want DeadlineExceeded", err)
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		time.Sleep(20 * time.Millisecond)
		_ = exportAll(ctx, cc, "m")
	})
	spans, err := s.WaitSpans(ctx, 1, func(sp Span) bool { return sp.Name() == "GET orders" })
	if err != nil || len(spans) != 1 {
		t.Fatalf("WaitSpans = %v, %v", spans, err)
	}
	logs, err := s.WaitLogs(ctx, 1, nil)
	if err != nil || len(logs) != 1 {
		t.Fatalf("WaitLogs = %v, %v", logs, err)
	}
	m, err := s.WaitMetric(ctx, "ruralz_http_requests_total")
	if err != nil || m.Name() != "ruralz_http_requests_total" {
		t.Fatalf("WaitMetric = %v, %v", m, err)
	}
	logs, err = s.WaitLogs(ctx, 1, func(l LogRecord) bool { return l.Body() == "access" })
	if err != nil || len(logs) != 1 {
		t.Fatalf("WaitLogs(pred) = %v, %v", logs, err)
	}
	wg.Wait()
}

func TestCloseAndContext(t *testing.T) {
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, Config{Mode: Stall{}})
	if err != nil {
		t.Fatal(err)
	}
	cc := dial(t, s, nil)
	done := make(chan error, 1)
	go func() {
		_, err := coltrace.NewTraceServiceClient(cc).Export(ctxTimeout(t, 10*time.Second), traceReq("svc"))
		done <- err
	}()
	waitFor(t, func() bool { return s.Stats().Stalled == 1 })
	cancel() // the Start context ends: the sink closes and ends held exports
	if err := <-done; err == nil {
		t.Fatal("held export succeeded across Close")
	}
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.isClosed
	})
	if err := s.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := s.SetMode(Accept{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("SetMode after Close: %v, want ErrClosed", err)
	}
	if st := s.Stats(); st.Rejected != 1 || st.Stalled != 0 {
		t.Fatalf("Stats = %+v", st)
	}
	_ = cc.Close()
	waitFor(t, func() bool { return runtime.NumGoroutine() <= base })
}

func TestAdmitAfterClose(t *testing.T) {
	s := start(t, Config{})
	_ = s.Close()
	if err := s.admit(context.Background()); status.Code(err) != codes.Unavailable {
		t.Fatalf("admit after Close = %v", err)
	}
}

func TestConfigErrors(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		c    Config
		want string
	}{
		{name: "reject OK", c: Config{Mode: Reject{Code: codes.OK}}, want: "other than OK"},
		{name: "unknown mode", c: Config{Mode: badMode{}}, want: "unknown mode"},
		{name: "listen", c: Config{Listen: "256.0.0.1:0"}, want: "listen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Start(ctx, tc.c)
			if err == nil {
				_ = s.Close()
				t.Fatal("Start succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	s := start(t, Config{})
	if err := s.SetMode(nil); err == nil || !strings.Contains(err.Error(), "nil mode") {
		t.Fatalf("SetMode(nil) = %v", err)
	}
}

type badMode struct{}

func (badMode) mode() {}

func TestStrings(t *testing.T) {
	for _, tc := range []struct {
		got, want string
	}{
		{Traces.String(), "traces"},
		{Metrics.String(), "metrics"},
		{Logs.String(), "logs"},
		{Signal(9).String(), "Signal(9)"},
		{Accept{}.String(), "accept"},
		{Reject{Code: codes.Unavailable}.String(), "reject(Unavailable)"},
		{Stall{}.String(), "stall"},
		{Down{}.String(), "down"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func BenchmarkExportTraces(b *testing.B) {
	s := start(b, Config{MaxRequests: 1 << 30, MaxBytes: 1 << 40})
	cc := dial(b, s, nil)
	client := coltrace.NewTraceServiceClient(cc)
	spans := make([]*tracepb.Span, 0, 16)
	for i := byte(2); i < 18; i++ {
		spans = append(spans, span(1, i, 1, "ruralz.filter.x", tracepb.Span_SPAN_KIND_INTERNAL, uint64(i)))
	}
	req := traceReq("svc", spans...)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := client.Export(ctx, req); err != nil {
			b.Fatal(err)
		}
		if len(s.rec.requests) > 10000 {
			s.Reset()
		}
	}
}

func grpcFailFast() grpc.CallOption { return grpc.WaitForReady(false) }

func grpcWaitForReady() grpc.CallOption { return grpc.WaitForReady(true) }

func waitFor(t testing.TB, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
