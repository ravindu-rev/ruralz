// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"

	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestLegAddsVia checks the Via rule of docs/architecture/07-multi-protocol.md
// "HTTP intermediary behavior": each Upstream request gains the
// Outbound's entry after any the client sent, and an Outbound without one
// (a Response Cache replay) adds none.
func TestLegAddsVia(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, ""))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	tests := []struct {
		name   string
		client []string
		via    string
		want   []string
	}{
		{"http/1.1 client", nil, "1.1 ruralz", []string{"1.1 ruralz"}},
		{"http/2 client", nil, "2 ruralz", []string{"2 ruralz"}},
		{"after the client's entries", []string{"1.0 edge, 1.1 cdn"}, "1.1 ruralz", []string{"1.0 edge, 1.1 cdn", "1.1 ruralz"}},
		{"no client hop", []string{"1.1 cdn"}, "", []string{"1.1 cdn"}},
		{"none at all", nil, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x := exchange(http.MethodGet, "/")
			if tt.client != nil {
				x.Hdr["Via"] = tt.client
			}
			o := outbound(x, nil)
			o.Via = tt.via
			r, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{})
			if err != nil {
				t.Fatalf("Leg: %v", err)
			}
			readAll(t, r)
			if got := srv.last().Header["Via"]; !slices.Equal(got, tt.want) {
				t.Errorf("Upstream Via = %q, want %q", got, tt.want)
			}
			if got := x.Hdr["Via"]; !slices.Equal(got, tt.client) {
				t.Errorf("client Via = %q, want it unchanged %q", got, tt.client)
			}
		})
	}
}

// TestLegReplayAddsNoForwardingFields checks a Response Cache replay's leg:
// the forwarding fields the saved request kept are removed and none are
// added (docs/architecture/03-data-plane.md "Upstream layer").
func TestLegReplayAddsNoForwardingFields(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, ""))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	x := exchange(http.MethodGet, "/")
	x.Hdr.Set("X-Forwarded-For", "198.51.100.1")
	x.Hdr.Set("X-Forwarded-Proto", "http")
	x.Hdr.Set("X-Forwarded-Host", "old.example")
	x.Hdr.Set("Forwarded", "for=198.51.100.1")
	x.Hdr["X-Forwarded-for"] = []string{"198.51.100.2"}
	o := outbound(x, nil)
	o.Replay = true
	r, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	got := srv.last().Header
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "Forwarded"} {
		if v, ok := got[name]; ok {
			t.Errorf("replay sent %s = %q, want none", name, v)
		}
	}
	if got.Get("Accept") != "application/json" {
		t.Errorf("replay Accept = %q, want the saved request's application/json", got.Get("Accept"))
	}
}

// TestLegForwardsRequestTrailers checks the request trailer rule: an
// attempt that sends the client body forwards the client's declared
// trailer fields after the body, without hop-by-hop and framing fields, for
// a gated body (values known at once, resent on a retry) and a streamed one
// (values known at the body's end); a leg that sends no client body
// forwards none.
func TestLegForwardsRequestTrailers(t *testing.T) {
	type seen struct {
		body    string
		trailer http.Header
		length  int64
	}
	var (
		mu  sync.Mutex
		got []seen
	)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{body: string(b), trailer: r.Trailer.Clone(), length: r.ContentLength})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	clientTrailer := func() http.Header {
		return http.Header{"X-Checksum": nil, "Content-Length": nil, "Te": nil, "Grpc-Status": nil}
	}
	t.Run("gated", func(t *testing.T) {
		mu.Lock()
		got = nil
		mu.Unlock()
		tr := clientTrailer()
		tr["X-Checksum"], tr["Content-Length"], tr["Te"], tr["Grpc-Status"] = []string{"abc"}, []string{"9"}, []string{"trailers"}, []string{"0"}
		o := outbound(exchange(http.MethodPost, "/orders"), nil)
		o.Body = gatedBody(t, "payload")
		o.Trailer = tr
		r, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{})
		if err != nil {
			t.Fatalf("Leg: %v", err)
		}
		readAll(t, r)
		mu.Lock()
		defer mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("Upstream requests = %d, want 1", len(got))
		}
		want := http.Header{"X-Checksum": {"abc"}, "Grpc-Status": {"0"}}
		if g := got[0]; g.body != "payload" || !equalHeader(g.trailer, want) || g.length != -1 {
			t.Errorf("Upstream got body %q trailer %v length %d, want payload %v chunked", g.body, g.trailer, g.length, want)
		}
	})
	t.Run("streamed", func(t *testing.T) {
		mu.Lock()
		got = nil
		mu.Unlock()
		tr := clientTrailer()
		o := outbound(exchange(http.MethodPost, "/orders"), nil)
		q := &body.Request{}
		// The values arrive with the body's end, as net/http fills them.
		q.SetStream(&fillAtEOF{r: strings.NewReader("streamed"), t: tr}, -1, 1<<20)
		o.Body = q
		o.Trailer = tr
		r, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{})
		if err != nil {
			t.Fatalf("Leg: %v", err)
		}
		readAll(t, r)
		mu.Lock()
		defer mu.Unlock()
		want := http.Header{"X-Checksum": {"late"}, "Grpc-Status": {"2"}}
		if len(got) != 1 || got[0].body != "streamed" || !equalHeader(got[0].trailer, want) {
			t.Errorf("Upstream got %+v, want body streamed with trailer %v", got, want)
		}
	})
	t.Run("no client body", func(t *testing.T) {
		mu.Lock()
		got = nil
		mu.Unlock()
		tr := clientTrailer()
		tr["X-Checksum"] = []string{"abc"}
		o := outbound(exchange(http.MethodPost, "/orders"), nil)
		o.Body = nil
		o.Trailer = tr
		r, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{Step: "lookup", Method: http.MethodGet})
		if err != nil {
			t.Fatalf("Leg: %v", err)
		}
		readAll(t, r)
		mu.Lock()
		defer mu.Unlock()
		if len(got) != 1 || len(got[0].trailer) != 0 {
			t.Errorf("Upstream got %+v, want no trailer without the client body", got)
		}
	})
}

// fillAtEOF is a client body whose trailer values appear when it ends.
type fillAtEOF struct {
	r io.Reader
	t http.Header
}

func (b *fillAtEOF) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		b.t["X-Checksum"], b.t["Content-Length"], b.t["Grpc-Status"] = []string{"late"}, []string{"8"}, []string{"2"}
	}
	return n, err
}

// equalHeader compares two headers, treating nil and empty alike.
func equalHeader(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

// hintsRecorder is a snapshot.EarlyHints that records relays.
type hintsRecorder struct {
	mu     sync.Mutex
	next   uint64
	relays []string
}

func (h *hintsRecorder) HintsToken() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	return h.next
}

func (h *hintsRecorder) RelayHints(token uint64, hdr http.Header) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.relays = append(h.relays, strings.Join(hdr["Link"], ",")+" #"+strconv.FormatUint(token, 10))
}

func (h *hintsRecorder) list() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.relays)
}

// TestLegRelaysEarlyHints checks that a plain upstreams leg relays each
// 103 Early Hints of each attempt with that attempt's token, that other
// 1xx responses stay on their hop, and that a composition step relays
// nothing (docs/architecture/03-data-plane.md "Upstream layer").
func TestLegRelaysEarlyHints(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</a.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Link", "</b.js>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.WriteHeader(http.StatusProcessing)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "done")
	})
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	h := &hintsRecorder{}
	o := outbound(exchange(http.MethodGet, "/"), nil)
	o.Hints = h
	r, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if b := readAll(t, r); b != "done" || r.Status != http.StatusOK {
		t.Errorf("response = %d %q, want 200 done", r.Status, b)
	}
	want := []string{"</a.css>; rel=preload #1", "</b.js>; rel=preload #1"}
	if got := h.list(); !slices.Equal(got, want) {
		t.Errorf("relayed = %q, want the two 103s with token 1, not the 102: %q", got, want)
	}
	h2 := &hintsRecorder{}
	o = outbound(exchange(http.MethodGet, "/"), nil)
	o.Hints = h2
	r, err = f.upstream(s, "orders").Leg(t.Context(), o, Target{Step: "summary"})
	if err != nil {
		t.Fatalf("step Leg: %v", err)
	}
	readAll(t, r)
	if got := h2.list(); len(got) != 0 {
		t.Errorf("a composition step relayed %q, want nothing", got)
	}
}

// TestLegRequestBodyOverLimit checks the streamed request body rule of
// docs/architecture/03-data-plane.md "Streaming": a body cut at
// limits.maxRequestBodyBytes while an attempt sends it ends that attempt
// without retry, as no Upstream failure (no attempt count, no breaker or
// ejection count), and the leg fails with 413 RZ-RT-003.
func TestLegRequestBodyOverLimit(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	f := newFixture(t)
	met := newMetrics()
	up := newUpstream("orders", srv.addr)
	// Any counted failure would open this breaker after the 20-leg guard,
	// and the default retryOn retries a PUT that got no response.
	up.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{ConsecutiveFailures: ptr[int32](1)}
	s := f.compile(nil, Config{Upstream: up, Metrics: met.m})
	u := f.upstream(s, "orders")
	for range resilience.BreakerMinimumLegs + 1 {
		o := outbound(exchange(http.MethodPut, "/orders"), nil)
		q := &body.Request{}
		q.SetStream(strings.NewReader(strings.Repeat("x", 4096)), -1, 1024)
		o.Body = q
		_, err := u.Leg(t.Context(), o, Target{})
		if codeOf(err) != "RZ-RT-003" {
			t.Fatalf("Leg error = %v, want RZ-RT-003", err)
		}
		if !q.Exceeded() {
			t.Fatal("the body was not cut at its limit")
		}
	}
	if a := met.attempts.list(); len(a) != 0 {
		t.Errorf("attempts counted = %v, want none", a)
	}
	if st := u.rt.res.Breaker().State(); st != resilience.StateClosed {
		t.Errorf("breaker state = %v, want closed", st)
	}
	if n := met.eject[0].get(); n != 0 {
		t.Errorf("passive ejections = %d, want 0", n)
	}
}

// TestLegRequestBodyBroken checks a streamed client body whose read fails
// while an attempt sends it, as an HTTP/2 body that ends short of its
// content-length does: the client's fault, so the attempt ends without
// retry and as no Upstream failure (no attempt count, no breaker or
// ejection count), and the leg fails with ErrClientBody and no RZ code
// instead of the RZ-UP code of a reset.
func TestLegRequestBodyBroken(t *testing.T) {
	var served atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	f := newFixture(t)
	met := newMetrics()
	up := newUpstream("orders", srv.addr)
	// Any counted failure would open this breaker after the 20-leg guard,
	// and the default retryOn retries a PUT that got no response.
	up.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{ConsecutiveFailures: ptr[int32](1)}
	s := f.compile(nil, Config{Upstream: up, Metrics: met.m})
	u := f.upstream(s, "orders")
	short := errors.New("request declared a Content-Length of 4096 but only wrote 2048 bytes")
	for range resilience.BreakerMinimumLegs + 1 {
		o := outbound(exchange(http.MethodPut, "/orders"), nil)
		q := &body.Request{}
		q.SetStream(io.MultiReader(strings.NewReader(strings.Repeat("x", 2048)), iotest.ErrReader(short)), 4096, 1<<20)
		o.Body = q
		r, err := u.Leg(t.Context(), o, Target{})
		if r != nil || !errors.Is(err, ErrClientBody) || codeOf(err) != "" {
			t.Fatalf("Leg = %v, %v, want ErrClientBody without a code", r, err)
		}
		if !q.Broken() {
			t.Fatal("the body does not report its failed read")
		}
	}
	if a := met.attempts.list(); len(a) != 0 {
		t.Errorf("attempts counted = %v, want none", a)
	}
	if st := u.rt.res.Breaker().State(); st != resilience.StateClosed {
		t.Errorf("breaker state = %v, want closed", st)
	}
	if n := met.eject[0].get(); n != 0 {
		t.Errorf("passive ejections = %d, want 0", n)
	}
	if n := served.Load(); n > int64(resilience.BreakerMinimumLegs+1) {
		t.Errorf("the Upstream saw %d requests for %d legs: an attempt was retried", n, resilience.BreakerMinimumLegs+1)
	}
}

// TestLegRecordsReached checks Outbound.Reached, which
// filter.Final.UpstreamReached reports: set once an attempt got a
// connection, left unset when every attempt failed to connect.
func TestLegRecordsReached(t *testing.T) {
	srv := newServer(t, status(http.StatusServiceUnavailable, ""))
	f := newFixture(t, "192.0.2.1:80")
	s := f.compile(nil,
		Config{Upstream: newUpstream("served", srv.addr)},
		Config{Upstream: newUpstream("refused", "192.0.2.1:80")})
	for _, tt := range []struct {
		upstream string
		want     bool
	}{{"served", true}, {"refused", false}} {
		var reached atomic.Bool
		o := outbound(exchange(http.MethodPost, "/"), nil)
		o.Reached = &reached
		r, _ := f.upstream(s, tt.upstream).Leg(t.Context(), o, Target{})
		if r != nil {
			readAll(t, r)
		}
		if reached.Load() != tt.want {
			t.Errorf("%s: Reached = %v, want %v", tt.upstream, reached.Load(), tt.want)
		}
	}
}

// TestLegResponseTrailerAccessor checks that the leg's response body gives
// the Upstream's trailer section once read, undeclared fields included.
func TestLegResponseTrailerAccessor(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "X-Declared")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "body")
		w.(http.Flusher).Flush()
		w.Header().Set("X-Declared", "d")
		w.Header().Set(http.TrailerPrefix+"X-Late", "l")
	})
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	tb, ok := r.Body.(interface{ Trailer() http.Header })
	if !ok {
		t.Fatalf("leg body %T has no Trailer accessor", r.Body)
	}
	if _, ok := r.Trailer["X-Declared"]; !ok {
		t.Errorf("declared trailer = %v, want X-Declared declared", r.Trailer)
	}
	readAll(t, r)
	got := tb.Trailer()
	if got.Get("X-Declared") != "d" || got.Get("X-Late") != "l" {
		t.Errorf("trailer after the body = %v, want X-Declared d and X-Late l", got)
	}
}

// TestLegResponseTrailerWithoutConnectionOptions checks that the trailer
// fields the response's Connection field names, hop-by-hop fields of the
// whole response (RFC 9110 section 7.6.1), leave the leg's response with
// its header fields: the declared ones at once, the undeclared ones net/http
// adds as the body ends, while the others pass.
func TestLegResponseTrailerWithoutConnectionOptions(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Connection", "X-Hop-Declared, X-Hop-Late")
		h.Set("Trailer", "X-Declared, X-Hop-Declared")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "body")
		w.(http.Flusher).Flush()
		h.Set("X-Declared", "d")
		h.Set("X-Hop-Declared", "hd")
		h.Set(http.TrailerPrefix+"X-Late", "l")
		h.Set(http.TrailerPrefix+"X-Hop-Late", "hl")
	})
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if _, ok := r.Trailer["X-Hop-Declared"]; ok {
		t.Errorf("declared trailer = %v, want no X-Hop-Declared, which Connection names", r.Trailer)
	}
	if _, ok := r.Trailer["X-Declared"]; !ok {
		t.Errorf("declared trailer = %v, want X-Declared declared", r.Trailer)
	}
	readAll(t, r)
	got := r.Body.(interface{ Trailer() http.Header }).Trailer()
	if got.Get("X-Declared") != "d" || got.Get("X-Late") != "l" || got.Get("X-Hop-Declared") != "" || got.Get("X-Hop-Late") != "" {
		t.Errorf("trailer after the body = %v, want X-Declared d and X-Late l without the fields Connection names", got)
	}
}
