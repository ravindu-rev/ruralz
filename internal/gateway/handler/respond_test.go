// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/problem"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// answer is a Forwarder answering the response resp returns, from Upstream
// orders.
func answer(resp func() *snapshot.UpstreamResponse) snapshot.Forwarder {
	return handlertest.ForwardFunc(func(context.Context, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		u := resp()
		if u.Upstream == "" {
			u.Upstream, u.Endpoint, u.Attempts = "orders", "10.0.0.1:80", 1
		}
		return u, nil
	})
}

// TestNoContentResponses checks the responses that carry no content: a
// HEAD request gets the header section only, an Upstream's included, and
// so does a problem document; 204 and 304 never carry a body.
func TestNoContentResponses(t *testing.T) {
	tests := []struct {
		name   string
		method string
		status int
		target string
	}{
		{"HEAD", http.MethodHead, http.StatusOK, "/r"},
		{"204", http.MethodGet, http.StatusNoContent, "/r"},
		{"304", http.MethodGet, http.StatusNotModified, "/r"},
		{"HEAD problem", http.MethodHead, http.StatusNotFound, "/nothing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newKit(t)
			fwd := handlertest.Respond("orders", tt.status, http.Header{"Content-Length": {"5"}}, "hello")
			k.publish(handlertest.Spec{Routes: []handlertest.Route{{Name: "r", Match: &v1alpha1.RouteMatch{Path: &v1alpha1.PathMatch{Prefix: "/r"}}, Forward: fwd}}})
			res := k.do(httptest.NewRequest(tt.method, tt.target, nil))
			if res.Code != tt.status || res.Body.Len() != 0 {
				t.Fatalf("%s %s = %d with body %q, want %d without content", tt.method, tt.target, res.Code, res.Body.String(), tt.status)
			}
			if tt.status == http.StatusNotFound && res.Header().Get("Content-Type") != problem.ContentType {
				t.Errorf("Content-Type = %q, want the problem document's", res.Header().Get("Content-Type"))
			}
			if rec := k.record(); rec.ResponseBytes != 0 {
				t.Errorf("response bytes = %d, want 0", rec.ResponseBytes)
			}
		})
	}
}

// blockingBody is a response body whose Read waits for its context and
// fails with the context's error, signaling entered first.
type blockingBody struct {
	ctx     context.Context //nolint:containedctx // the leg's context, as a transport body holds it.
	entered chan struct{}
	once    sync.Once
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*blockingBody) Close() error { return nil }

// TestResponseGateFailures checks a gated response body that cannot be
// read whole: over limits.maxResponseBodyBytes 502 RZ-UP-010, a failed read
// 502 RZ-UP-004, a read the Route timeout ended 504 RZ-UP-003; the
// Upstream's trailers go with the body.
func TestResponseGateFailures(t *testing.T) {
	t.Run("over the limit", func(t *testing.T) {
		k := newKit(t)
		k.publish(handlertest.Spec{Limits: snapshot.Limits{MaxResponseBodyBytes: 100}, Routes: []handlertest.Route{{
			Name: "r", Body: snapshot.BodyNeeds{ResponseGate: true},
			Forward: handlertest.Respond("orders", http.StatusOK, http.Header{}, strings.Repeat("x", 200)),
		}}})
		wantProblem(t, k.get("/"), http.StatusBadGateway, "RZ-UP-010")
	})
	t.Run("read fails", func(t *testing.T) {
		k := newKit(t)
		k.publish(handlertest.Spec{Routes: []handlertest.Route{{
			Name: "r", Body: snapshot.BodyNeeds{ResponseGate: true},
			Forward: answer(func() *snapshot.UpstreamResponse {
				return &snapshot.UpstreamResponse{
					Status: http.StatusOK, Header: http.Header{}, Trailer: http.Header{"X-Sum": {"1"}},
					Body: &errReader{data: []byte("ab"), err: errors.New("connection reset")},
				}
			}),
		}}})
		res := k.get("/")
		wantProblem(t, res, http.StatusBadGateway, codeUpstreamReset)
		if tr := res.Result().Trailer; len(tr) != 0 || res.Header().Get("Trailer") != "" {
			t.Errorf("trailers = %v, want none on the generated response", tr)
		}
	})
	t.Run("route timeout", func(t *testing.T) {
		k := newKit(t)
		entered := make(chan struct{})
		k.publish(handlertest.Spec{Routes: []handlertest.Route{{
			Name: "r", Timeout: 3 * handlertest.DefaultTimeout / 15, Body: snapshot.BodyNeeds{ResponseGate: true},
			Forward: handlertest.ForwardFunc(func(ctx context.Context, _ *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
				return &snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{}, Body: &blockingBody{ctx: ctx, entered: entered}}, nil
			}),
		}}})
		done := make(chan result, 1)
		go func() { done <- k.get("/") }()
		<-entered
		k.clk.Advance(3 * handlertest.DefaultTimeout / 15)
		wantProblem(t, <-done, http.StatusGatewayTimeout, codeUpstreamTimeout)
	})
}

// trailerBody is an Upstream body that fills its trailer map at the end of
// the body, as net/http does: the declared fields and any the Upstream sent
// undeclared.
type trailerBody struct {
	r   io.Reader
	tr  http.Header
	add http.Header
}

func (b *trailerBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) {
		for k, v := range b.add {
			b.tr[k] = v
		}
	}
	return n, err
}

func (*trailerBody) Close() error { return nil }

// exposedTrailer is an Upstream body exposing its trailer section through
// Trailer, as the Upstream layer's leg body does.
type exposedTrailer struct {
	io.Reader
	tr http.Header
}

func (*exposedTrailer) Close() error           { return nil }
func (b *exposedTrailer) Trailer() http.Header { return b.tr }

// TestResponseTrailers checks the Upstream's trailer fields: the declared
// ones are announced in Trailer and framed for (no Content-Length on
// HTTP/1.1), every forwardable one goes after the body, undeclared ones
// included, hop-by-hop and framing fields never; an HTTP/1.0 client gets
// none.
func TestResponseTrailers(t *testing.T) {
	upstreamFilled := func() *snapshot.UpstreamResponse {
		tr := http.Header{"X-Checksum": nil, "Connection": nil, "Content-Length": nil}
		return &snapshot.UpstreamResponse{
			Status: http.StatusOK, Header: http.Header{"Content-Length": {"4"}}, Trailer: tr,
			Body: &trailerBody{r: strings.NewReader("data"), tr: tr, add: http.Header{"X-Checksum": {"abc"}, "X-Late": {"1"}, "Keep-Alive": {"x"}}},
		}
	}
	exposed := func() *snapshot.UpstreamResponse {
		return &snapshot.UpstreamResponse{
			Status: http.StatusOK, Header: http.Header{"Content-Length": {"4"}}, Trailer: http.Header{"X-Checksum": nil},
			Body: &exposedTrailer{Reader: strings.NewReader("data"), tr: http.Header{"X-Checksum": {"abc"}, "X-Late": {"1"}, "Connection": {"close"}}},
		}
	}
	want := http.Header{"X-Checksum": {"abc"}, "X-Late": {"1"}}
	tests := []struct {
		name     string
		resp     func() *snapshot.UpstreamResponse
		major    int
		minor    int
		gate     bool
		want     http.Header
		declared string
		length   string
	}{
		{"filled at the end", upstreamFilled, 1, 1, false, want, "X-Checksum", ""},
		{"exposed by the body", exposed, 1, 1, false, want, "X-Checksum", ""},
		// The gate read the body to its end, so the whole trailer section
		// is known and announced at commit.
		{"gated", upstreamFilled, 1, 1, true, want, "X-Checksum, X-Late", ""},
		{"HTTP/2 keeps the length", upstreamFilled, 2, 0, false, want, "X-Checksum", "4"},
		{"HTTP/1.0", upstreamFilled, 1, 0, false, nil, "", "4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newKit(t)
			k.publish(handlertest.Spec{Routes: []handlertest.Route{{Name: "r", Forward: answer(tt.resp), Body: snapshot.BodyNeeds{ResponseGate: tt.gate}}}})
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.ProtoMajor, r.ProtoMinor = tt.major, tt.minor
			res := k.do(r)
			if res.Code != http.StatusOK || res.Body.String() != "data" {
				t.Fatalf("response = %d %q, want 200 data", res.Code, res.Body.String())
			}
			got := res.Result().Trailer
			if len(got) != len(tt.want) {
				t.Errorf("trailers = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if !slices.Equal(got[k], v) {
					t.Errorf("trailer %s = %q, want %q", k, got[k], v)
				}
			}
			if declared := res.Header().Get("Trailer"); declared != tt.declared {
				t.Errorf("Trailer = %q, want %q", declared, tt.declared)
			}
			if l := res.Header().Get("Content-Length"); l != tt.length {
				t.Errorf("Content-Length = %q, want %q", l, tt.length)
			}
		})
	}
}

// TestTrailersOverHTTP checks the trailer section net/http frames: an
// HTTP/1.1 client reads the Upstream's trailers after a chunked body.
func TestTrailersOverHTTP(t *testing.T) {
	k := newKit(t, func(o *kitOptions) { o.start = beyondDeadlines() })
	k.publish(handlertest.Spec{Routes: []handlertest.Route{{Name: "r", Forward: answer(func() *snapshot.UpstreamResponse {
		tr := http.Header{"X-Checksum": nil}
		return &snapshot.UpstreamResponse{
			Status: http.StatusOK, Header: http.Header{"Content-Length": {"4"}}, Trailer: tr,
			Body: &trailerBody{r: strings.NewReader("data"), tr: tr, add: http.Header{"X-Checksum": {"abc"}, "X-Late": {"1"}}},
		}
	})}}})
	srv := httptest.NewServer(k.h)
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "data" || !slices.Equal(resp.TransferEncoding, []string{"chunked"}) {
		t.Errorf("body %q with transfer encoding %v, want data chunked", b, resp.TransferEncoding)
	}
	if resp.Trailer.Get("X-Checksum") != "abc" || resp.Trailer.Get("X-Late") != "1" {
		t.Errorf("trailers = %v, want X-Checksum abc and X-Late 1", resp.Trailer)
	}
}

// chunks is a body that returns one part per Read.
type chunks struct{ parts []string }

func (c *chunks) Read(p []byte) (int, error) {
	if len(c.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.parts[0])
	c.parts = c.parts[1:]
	return n, nil
}

func (*chunks) Close() error { return nil }

// TestFlushPolicy checks when the copy flushes after every Upstream read:
// a body without Content-Length or an event stream, never another.
func TestFlushPolicy(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   int
	}{
		{"no length", http.Header{"Content-Type": {"text/plain"}}, 2},
		{"length", http.Header{"Content-Length": {"8"}}, 0},
		{"event stream with a length", http.Header{"Content-Type": {"Text/Event-Stream; charset=utf-8"}, "Content-Length": {"8"}}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newKit(t)
			k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", answer(func() *snapshot.UpstreamResponse {
				return &snapshot.UpstreamResponse{Status: http.StatusOK, Header: tt.header.Clone(), Body: &chunks{parts: []string{"abcd", "efgh"}}}
			}))}})
			w := newRecordingWriter()
			k.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
			if w.Body.String() != "abcdefgh" || w.flushes != tt.want {
				t.Errorf("body %q with %d flushes, want abcdefgh with %d", w.Body.String(), w.flushes, tt.want)
			}
		})
	}
}

// teeFilter is a cache-class Filter that tees the response in onResponse
// and reads the copy in Finish.
type teeFilter struct {
	mu       sync.Mutex
	body     []byte
	complete bool
	finished bool
}

func (f *teeFilter) Handle(_ context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	if p == phase.OnResponse {
		*x.PolicyState() = x.TeeResponse(1 << 20)
	}
	return filter.Next()
}

func (f *teeFilter) Finish(_ context.Context, x filter.Exchange) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = true
	if t, ok := (*x.PolicyState()).(filter.Tee); ok && t != nil {
		b, complete := t.Result()
		f.body, f.complete = append([]byte(nil), b...), complete
	}
}

// TestTeesFollowTheCopy checks the Response Cache tees: the copy feeds
// every byte it writes and completes them at the body's end; a body that
// fails after commit leaves them incomplete, so nothing is stored.
func TestTeesFollowTheCopy(t *testing.T) {
	tests := []struct {
		name     string
		body     func() io.ReadCloser
		complete bool
		want     string
	}{
		{"whole", func() io.ReadCloser { return &chunks{parts: []string{"hello ", "world"}} }, true, "hello world"},
		{"cut", func() io.ReadCloser { return &errReader{data: []byte("hel"), err: errors.New("reset")} }, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newKit(t)
			tf := &teeFilter{}
			k.publish(handlertest.Spec{Routes: []handlertest.Route{{
				Name: "r", Body: snapshot.BodyNeeds{ResponseTee: true},
				Forward: answer(func() *snapshot.UpstreamResponse {
					return &snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{"Cache-Control": {"max-age=60"}}, Body: tt.body()}
				}),
				Policies: []*snapshot.Policy{handlertest.Policy("cache", phase.ClassCache, v1alpha1.PolicyTypeCache, tf, phase.OnRequestHeaders, phase.OnResponse)},
			}}})
			k.get("/")
			tf.mu.Lock()
			defer tf.mu.Unlock()
			if !tf.finished || tf.complete != tt.complete || string(tf.body) != tt.want {
				t.Errorf("tee = %q complete %t (finished %t), want %q complete %t", tf.body, tf.complete, tf.finished, tt.want, tt.complete)
			}
		})
	}
}

// TestUpstreamFailsAfterCommit checks an Upstream body that fails after
// commit: the stream ends with an error (the request aborts, so the client
// never takes the cut body as whole), recorded with the read's code or
// RZ-UP-009 and the committed status.
func TestUpstreamFailsAfterCommit(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{"uncoded", errors.New("connection reset"), codeAfterCommit},
		{"coded", errcode.Wrap(codeUpstreamTimeout, errors.New("deadline")), codeUpstreamTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newKit(t)
			k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", answer(func() *snapshot.UpstreamResponse {
				return &snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{}, Body: &errReader{data: []byte("par"), err: tt.err}}
			}))}})
			res := k.get("/")
			if !res.aborted || res.Code != http.StatusOK || res.Body.String() != "par" {
				t.Fatalf("aborted %t, response %d %q, want an abort after 200 par", res.aborted, res.Code, res.Body.String())
			}
			if rec := k.record(); rec.Status != http.StatusOK || rec.Code != tt.code || rec.ResponseBytes != 3 {
				t.Errorf("record = %d %s %d bytes, want 200 %s 3", rec.Status, rec.Code, rec.ResponseBytes, tt.code)
			}
			if c := k.node.Responses.Codes(); len(c) != 0 {
				t.Errorf("node responses = %v, want none for an Upstream response", c)
			}
		})
	}
}

// overrunBody is an Upstream response body that, as a full-duplex
// Upstream would, reads the whole streamed request body before its end.
type overrunBody struct {
	o    *snapshot.Outbound
	done bool
}

func (b *overrunBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	b.done = true
	if rd, err := b.o.Body.Open(); err == nil {
		_, _ = io.Copy(io.Discard, rd)
		_ = rd.Close()
	}
	return copy(p, "ok"), nil
}

func (*overrunBody) Close() error { return nil }

// TestRequestBodyOverrunAfterCommit checks a streamed request body that
// passes its limit after commit: the stream resets, recorded with
// RZ-RT-003.
func TestRequestBodyOverrunAfterCommit(t *testing.T) {
	k := newKit(t)
	fwd := handlertest.ForwardFunc(func(_ context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		return &snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{}, Body: &overrunBody{o: o}}, nil
	})
	k.publish(handlertest.Spec{Limits: snapshot.Limits{MaxRequestBodyBytes: 10}, Routes: []handlertest.Route{route("r", fwd)}})
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 100)))
	r.ContentLength = -1
	res := k.do(r)
	if !res.aborted || res.Code != http.StatusOK {
		t.Fatalf("aborted %t, status %d, want an abort after commit", res.aborted, res.Code)
	}
	if rec := k.record(); rec.Code != codeBodyTooLarge || rec.Status != http.StatusOK {
		t.Errorf("record = %d %s, want 200 RZ-RT-003", rec.Status, rec.Code)
	}
}

// endingBody is an Upstream body that ends every pinned request while the
// copy runs.
type endingBody struct {
	k    *kit
	done bool
}

func (b *endingBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	b.done = true
	if _, err := b.k.holder.EndAll(context.Background(), snapshot.EndDrain); err != nil {
		return 0, err
	}
	return copy(p, "ok"), nil
}

func (*endingBody) Close() error { return nil }

// TestEndedAfterCommit checks the ending protocol after commit: the
// response keeps its status and is recorded with the end code.
func TestEndedAfterCommit(t *testing.T) {
	k := newKit(t)
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", answer(func() *snapshot.UpstreamResponse {
		return &snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{}, Body: &endingBody{k: k}}
	}))}})
	res := k.get("/")
	if res.Code != http.StatusOK || res.Body.String() != "ok" {
		t.Fatalf("response = %d %q, want 200 ok", res.Code, res.Body.String())
	}
	if rec := k.record(); rec.Status != http.StatusOK || rec.Code != codeDrain {
		t.Errorf("record = %d %s, want 200 RZ-RT-016", rec.Status, rec.Code)
	}
}

// TestFaultRecovered checks a fault outside the Filter Chain: 500 RZ-RT-025
// with a problem document, logged at error with its stack and trace ID,
// counted, the server span ended, and the pin and in-flight unit released.
func TestFaultRecovered(t *testing.T) {
	k := newKit(t)
	snap := k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", handlertest.ForwardFunc(func(context.Context, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		panic("boom")
	}))}})
	res := k.get("/")
	d := wantProblem(t, res, http.StatusInternalServerError, codeFault)
	if !k.logs.has("ERROR request handling panic recovered") || !k.logs.has("panic_type=string") || !k.logs.has("trace_id="+d.RequestID) || !k.logs.has("stack=goroutine") {
		t.Errorf("logs = %v, want the panic at error with its type, trace ID and stack", k.logs.recs)
	}
	if pins(snap) != 0 || k.units.InUse() != 0 {
		t.Errorf("pins %d, units %d after the fault, want 0 and 0", pins(snap), k.units.InUse())
	}
	if c := k.node.Responses.Codes(); !slices.Equal(c, []string{codeFault}) {
		t.Errorf("node responses = %v, want [RZ-RT-025]", c)
	}
	if s := k.plan.RouteHandles("r").Requests.Statuses(); !slices.Equal(s, []int{http.StatusInternalServerError}) {
		t.Errorf("route requests = %v, want [500]", s)
	}
	if ev := k.plan.ListenerHandles(handlertest.ListenerName).Requests.Events(); len(ev) != 1 || ev[0].Status != 500 || ev[0].Origin != emit.OriginNode {
		t.Errorf("listener requests = %+v, want one 500 from the Node", ev)
	}
	if spans := k.tracer.Spans("server"); len(spans) != 1 {
		t.Fatalf("server spans = %d, want 1", len(spans))
	} else if ended, status, code := spans[0].Done(); !ended || status != 500 || code != codeFault {
		t.Errorf("server span = ended %t %d %s, want ended 500 RZ-RT-025", ended, status, code)
	}
	// The pooled state serves the next request normally.
	k2 := k.get("/")
	wantProblem(t, k2, http.StatusInternalServerError, codeFault)
}

// panicBody panics on its second read, after the handler committed.
type panicBody struct{ reads int }

func (b *panicBody) Read(p []byte) (int, error) {
	b.reads++
	if b.reads > 1 {
		panic("body fault")
	}
	return copy(p, "par"), nil
}

func (*panicBody) Close() error { return nil }

// TestFaultAfterCommit checks a fault after commit: the request aborts with
// http.ErrAbortHandler, keeping what was written, with the pin and unit
// released.
func TestFaultAfterCommit(t *testing.T) {
	k := newKit(t)
	snap := k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", answer(func() *snapshot.UpstreamResponse {
		return &snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{}, Body: &panicBody{}}
	}))}})
	res := k.get("/")
	if !res.aborted || res.Code != http.StatusOK || res.Body.String() != "par" {
		t.Fatalf("aborted %t, response %d %q, want an abort after 200 par", res.aborted, res.Code, res.Body.String())
	}
	if pins(snap) != 0 || k.units.InUse() != 0 {
		t.Errorf("pins %d, units %d, want 0 and 0", pins(snap), k.units.InUse())
	}
	if s := k.plan.RouteHandles("r").Requests.Statuses(); len(s) != 0 {
		t.Errorf("route requests = %v, want none for a cut stream", s)
	}
	if c := k.node.Responses.Codes(); !slices.Equal(c, []string{codeFault}) {
		t.Errorf("node responses = %v, want [RZ-RT-025]", c)
	}
}

// hintWriter is a ResponseWriter recording every WriteHeader, 1xx
// included, with a copy of the header map at that moment.
type hintWriter struct {
	*httptest.ResponseRecorder
	mu    sync.Mutex
	codes []int
	hdrs  []http.Header
}

func (w *hintWriter) WriteHeader(code int) {
	w.mu.Lock()
	w.codes = append(w.codes, code)
	w.hdrs = append(w.hdrs, w.ResponseRecorder.Header().Clone())
	w.mu.Unlock()
	if code >= http.StatusOK {
		w.ResponseRecorder.WriteHeader(code)
	}
}

// relayBody relays hints from its Read, after commit.
type relayBody struct {
	hints snapshot.EarlyHints
	token uint64
	done  bool
}

func (b *relayBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	b.done = true
	b.hints.RelayHints(b.token, http.Header{"Link": {"</late.css>"}})
	return copy(p, "ok"), nil
}

func (*relayBody) Close() error { return nil }

// TestEarlyHintsRelay checks the 103 relay: the attempt's hints go out
// before the final response without hop-by-hop fields, and leave the final
// header section; a stale token, a 103 with only hop-by-hop fields, a
// relay after commit and one after the request ended write nothing.
func TestEarlyHintsRelay(t *testing.T) {
	k := newKit(t)
	var hints snapshot.EarlyHints
	var token uint64
	fwd := handlertest.ForwardFunc(func(_ context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		hints, token = o.Hints, o.Hints.HintsToken()
		o.Hints.RelayHints(token, http.Header{"Link": {"</a.css>; rel=preload"}, "Connection": {"close"}, "Keep-Alive": {"timeout=5"}})
		o.Hints.RelayHints(token+1, http.Header{"Link": {"</stale.css>"}})
		o.Hints.RelayHints(token, http.Header{"Connection": {"close"}})
		return &snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{"X-Final": {"1"}}, Body: &relayBody{hints: o.Hints, token: token}}, nil
	})
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", fwd)}})
	w := &hintWriter{ResponseRecorder: httptest.NewRecorder()}
	k.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	hints.RelayHints(token, http.Header{"Link": {"</after.css>"}})

	w.mu.Lock()
	defer w.mu.Unlock()
	if !slices.Equal(w.codes, []int{http.StatusEarlyHints, http.StatusOK}) {
		t.Fatalf("status lines = %v, want [103 200]", w.codes)
	}
	if h := w.hdrs[0]; h.Get("Link") != "</a.css>; rel=preload" || h.Get("Connection") != "" || h.Get("Keep-Alive") != "" {
		t.Errorf("103 fields = %v, want Link only", h)
	}
	if h := w.hdrs[1]; h.Get("Link") != "" || h.Get("X-Final") != "1" {
		t.Errorf("final fields = %v, want X-Final without the hints", h)
	}
	if w.Body.String() != "ok" {
		t.Errorf("body = %q, want ok", w.Body.String())
	}
}

// TestEarlyHintsOverHTTP checks the relay over net/http: the client sees
// the 103 with its Link before the final response, which does not repeat
// it.
func TestEarlyHintsOverHTTP(t *testing.T) {
	k := newKit(t, func(o *kitOptions) { o.start = beyondDeadlines() })
	fwd := handlertest.ForwardFunc(func(_ context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		o.Hints.RelayHints(o.Hints.HintsToken(), http.Header{"Link": {"</a.css>; rel=preload"}})
		return &snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{"Content-Length": {"2"}}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", fwd)}})
	srv := httptest.NewServer(k.h)
	defer srv.Close()
	var mu sync.Mutex
	var got []string
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
		mu.Lock()
		defer mu.Unlock()
		if code == http.StatusEarlyHints {
			got = append(got, h.Get("Link"))
		}
		return nil
	}})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(got, []string{"</a.css>; rel=preload"}) || resp.Header.Get("Link") != "" || resp.StatusCode != http.StatusOK {
		t.Errorf("103 links %v, final %d with Link %q, want one 103 then 200 without it", got, resp.StatusCode, resp.Header.Get("Link"))
	}
}
