// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/problem"
)

// server serves HTTP/1.1 and h2c on loopback and counts connections.
type server struct {
	url   string
	conns atomic.Int64
}

func serve(t *testing.T, h http.Handler) *server {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{url: "http://" + ln.Addr().String()}
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Handler: h, Protocols: &p, ReadHeaderTimeout: 10 * time.Second,
		ConnState: func(_ net.Conn, st http.ConnState) {
			if st == http.StateNew {
				s.conns.Add(1)
			}
		},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return s
}

// tlsServer serves HTTP/1.1 and HTTP/2 over TLS and returns a client
// configuration trusting it.
func tlsServer(t *testing.T, h http.Handler) (*httptest.Server, *tls.Config) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func newTarget(t *testing.T, c HTTPConfig) *HTTPTarget {
	t.Helper()
	tg, err := NewHTTP(c)
	if err != nil {
		t.Fatalf("NewHTTP = %v", err)
	}
	t.Cleanup(func() { _ = tg.Close() })
	return tg
}

type sawReq struct {
	method, host, key, auth, body, query, remote, proto string
}

type saw struct {
	mu   sync.Mutex
	reqs []sawReq
}

func (s *saw) handler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, sawReq{
			r.Method, r.Host, r.Header.Get("X-Bench-Key"), r.Header.Get("Authorization"),
			string(b), r.URL.RawQuery, r.RemoteAddr, r.Proto,
		})
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}
}

func (s *saw) list() []sawReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sawReq(nil), s.reqs...)
}

// Requests carry the method, headers, body, Host, rotating key and bearer
// token, and the Prepare hook's change.
func TestHTTPRequestShape(t *testing.T) {
	var s saw
	srv := serve(t, s.handler("pong"))
	keys, _ := Cycle("k", 2)
	tg := newTarget(t, HTTPConfig{
		URL: srv.url + "/bench", Method: http.MethodPost, Body: []byte("hello"), Host: "api.example",
		Header: http.Header{"X-A": {"1"}}, KeyHeader: "x-bench-key", Keys: keys, Tokens: []string{"t0", "t1", "t2"},
		Prepare: func(r *http.Request, seq uint64) { r.URL.RawQuery = fmt.Sprintf("seq=%d", seq) },
	})
	for seq := range uint64(4) {
		resp := tg.Do(t.Context(), Request{Seq: seq})
		if resp.Outcome != Success || resp.Status != 200 || resp.Bytes != 4 || resp.Key != fmt.Sprintf("k%d", seq%2) || resp.Proto != "HTTP/1.1" {
			t.Errorf("seq %d: %+v", seq, resp)
		}
	}
	reqs := s.list()
	if len(reqs) != 4 {
		t.Fatalf("server saw %d requests, want 4", len(reqs))
	}
	for i, r := range reqs {
		want := sawReq{
			http.MethodPost, "api.example", fmt.Sprintf("k%d", i%2), fmt.Sprintf("Bearer t%d", i%3), "hello",
			fmt.Sprintf("seq=%d", i), r.remote, "HTTP/1.1",
		}
		if r != want {
			t.Errorf("request %d = %+v, want %+v", i, r, want)
		}
	}
}

// An error response's problem document yields its RZ code.
func TestHTTPProblemCode(t *testing.T) {
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/problem":
			problem.Write(w, problem.New("RZ-RT-005", strings.Repeat("a", 32)), nil)
		case "/plain":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/ok-problem":
			w.Header().Set("Content-Type", problem.ContentType)
			_, _ = io.WriteString(w, `{"code":"RZ-RT-005"}`)
		}
	}))
	for _, c := range []struct {
		path     string
		code     string
		outcome  Outcome
		minBytes int64
	}{
		{"/problem", "RZ-RT-005", ServerError, 50},
		{"/plain", "", ServerError, 0},
		{"/ok-problem", "", Success, 10},
	} {
		resp := newTarget(t, HTTPConfig{URL: srv.url + c.path}).Do(t.Context(), Request{})
		if resp.Code != c.code || resp.Outcome != c.outcome || resp.Bytes < c.minBytes {
			t.Errorf("%s: code %q outcome %v bytes %d, want %q %v >= %d", c.path, resp.Code, resp.Outcome, resp.Bytes, c.code, c.outcome, c.minBytes)
		}
	}
}

func TestHTTPProtocols(t *testing.T) {
	plain := serve(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	secure, tc := tlsServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for _, c := range []struct {
		name  string
		cfg   HTTPConfig
		proto string
	}{
		{"HTTP/1.1", HTTPConfig{URL: plain.url}, "HTTP/1.1"},
		{"h2c", HTTPConfig{URL: plain.url, Protocol: H2C}, "HTTP/2.0"},
		{"HTTP/1.1 over TLS", HTTPConfig{URL: secure.URL, TLS: tc}, "HTTP/1.1"},
		{"HTTP/2 over TLS", HTTPConfig{URL: secure.URL, Protocol: H2, TLS: tc}, "HTTP/2.0"},
	} {
		resp := newTarget(t, c.cfg).Do(t.Context(), Request{})
		if resp.Outcome != Success || resp.Proto != c.proto {
			t.Errorf("%s: outcome %v proto %q err %v, want success over %s", c.name, resp.Outcome, resp.Proto, resp.Err, c.proto)
		}
	}
	for _, p := range []Protocol{HTTP1, H2C, H2, Protocol(9)} {
		if p.String() == "" {
			t.Errorf("protocol %d has no name", int(p))
		}
	}
}

// Each response names the connection used, so an L4 balancer's record
// attributes it to a backend.
func TestHTTPReportsTheConnection(t *testing.T) {
	var s saw
	srv := serve(t, s.handler(""))
	tg := newTarget(t, HTTPConfig{URL: srv.url, Connections: 1})
	first := tg.Do(t.Context(), Request{Seq: 0})
	second := tg.Do(t.Context(), Request{Seq: 1})
	reqs := s.list()
	if first.Conn.Local != reqs[0].remote || first.Conn.Reused || first.Conn.Remote != strings.TrimPrefix(srv.url, "http://") {
		t.Errorf("first request connection %+v, server saw %s", first.Conn, reqs[0].remote)
	}
	if second.Conn.Local != first.Conn.Local || !second.Conn.Reused {
		t.Errorf("second request connection %+v, want the first reused", second.Conn)
	}
}

func TestHTTP1ConnectionBound(t *testing.T) {
	release := make(chan struct{})
	var inHandler atomic.Int64
	srv := serve(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		inHandler.Add(1)
		<-release
	}))
	tg := newTarget(t, HTTPConfig{URL: srv.url, Connections: 2})
	var wg sync.WaitGroup
	for seq := range uint64(6) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tg.Do(t.Context(), Request{Seq: seq})
		}()
	}
	waitFor(t, "two requests in handlers", func() bool { return inHandler.Load() == 2 })
	close(release)
	wg.Wait()
	if srv.conns.Load() != 2 || inHandler.Load() != 6 {
		t.Errorf("%d connections served %d requests, want 2 serving 6", srv.conns.Load(), inHandler.Load())
	}
}

// A light HTTP/1.1 load keeps few connections of a pool busy, since the
// pool takes its most recently used idle connection first; with
// RoundRobin, requests take Connections in turn, so the same light load
// uses every connection, each carrying one request at a time, and a
// request passes a busy connection over for the next free one.
func TestHTTP1RoundRobin(t *testing.T) {
	var s saw
	srv := serve(t, s.handler(""))
	pool := newTarget(t, HTTPConfig{URL: srv.url, Connections: 3})
	for seq := range uint64(6) {
		if resp := pool.Do(t.Context(), Request{Seq: seq}); resp.Outcome != Success {
			t.Fatalf("pool request %d: %+v", seq, resp)
		}
	}
	if srv.conns.Load() != 1 {
		t.Errorf("a sequential load opened %d pool connections, want 1", srv.conns.Load())
	}
	spread := serve(t, s.handler(""))
	tg := newTarget(t, HTTPConfig{URL: spread.url, Connections: 3, RoundRobin: true})
	locals := map[uint64]string{}
	for seq := range uint64(6) {
		resp := tg.Do(t.Context(), Request{Seq: seq})
		if resp.Outcome != Success || resp.Proto != "HTTP/1.1" {
			t.Fatalf("round-robin request %d: %+v", seq, resp)
		}
		locals[seq] = resp.Conn.Local
	}
	if spread.conns.Load() != 3 || locals[0] != locals[3] || locals[1] != locals[4] || locals[2] != locals[5] || locals[0] == locals[1] || locals[1] == locals[2] {
		t.Errorf("%d connections, by sequence %v; want 3, seq%%3 sharing one", spread.conns.Load(), locals)
	}

	// A round-robin connection carries one request at a time, and a
	// request takes the next connection in turn that is free: while
	// request 0 holds its connection, requests 3, 6 and 9, which the
	// sequence alone would have put there, take the other two; once every
	// connection holds a request, the next one waits, and its context
	// ends the wait without sending anything.
	h := newHolder(t)
	tg = newTarget(t, HTTPConfig{URL: h.url, Connections: 3, RoundRobin: true, Prepare: h.holdSeqs(0, 10, 11)})
	h.goDo(tg, 0)
	waitFor(t, "request 0 in its handler", func() bool { return h.held.Load() == 1 })
	for _, seq := range []uint64{3, 6, 9} {
		if resp := tg.Do(t.Context(), Request{Seq: seq}); resp.Outcome != Success || h.heldOn(resp.Conn.Local) {
			t.Errorf("request %d beside a busy round-robin connection: %+v, want Success on a free connection", seq, resp)
		}
	}
	h.goDo(tg, 10)
	h.goDo(tg, 11)
	waitFor(t, "a request on every connection", func() bool { return h.held.Load() == 3 })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if resp := tg.Do(ctx, Request{Seq: 12}); resp.Outcome != Canceled || resp.Status != 0 || h.answered.Load() != 3 {
		t.Errorf("request with every round-robin connection busy: %+v, %d answered; want Canceled before sending, 3 answered", resp, h.answered.Load())
	}
	h.finish()
}

// holder is a server answering every request at once but those carrying
// X-Hold, which it holds until finish, recording the client address of
// each held request's connection.
type holder struct {
	url      string
	release  chan struct{}
	held     atomic.Int64
	answered atomic.Int64
	mu       sync.Mutex
	addrs    map[string]bool
	wg       sync.WaitGroup
}

func newHolder(t *testing.T) *holder {
	t.Helper()
	h := &holder{release: make(chan struct{}), addrs: map[string]bool{}}
	h.url = serve(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Hold") == "" {
			h.answered.Add(1)
			return
		}
		h.mu.Lock()
		h.addrs[r.RemoteAddr] = true
		h.mu.Unlock()
		h.held.Add(1)
		select {
		case <-h.release:
		case <-r.Context().Done():
		}
	})).url
	t.Cleanup(h.finish)
	return h
}

// holdSeqs returns a Prepare hook marking the requests seqs to be held.
func (h *holder) holdSeqs(seqs ...uint64) func(*http.Request, uint64) {
	return func(r *http.Request, seq uint64) {
		if slices.Contains(seqs, seq) {
			r.Header.Set("X-Hold", "1")
		}
	}
}

// goDo sends request seq through tg on a goroutine finish waits for.
func (h *holder) goDo(tg *HTTPTarget, seq uint64) {
	h.wg.Go(func() { tg.Do(context.Background(), Request{Seq: seq}) })
}

// heldOn reports whether a held request arrived on the connection whose
// client address is local.
func (h *holder) heldOn(local string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.addrs[local]
}

// finish releases the held requests and waits for every goDo.
func (h *holder) finish() {
	h.mu.Lock()
	select {
	case <-h.release:
	default:
		close(h.release)
	}
	h.mu.Unlock()
	h.wg.Wait()
}

// HTTP/2 requests take Connections in turn, each connection carrying at
// most StreamsPerConn at once, and a request passes a connection whose
// streams are all taken over for the next one with a free stream.
func TestHTTP2LanesAndStreamBound(t *testing.T) {
	var s saw
	srv := serve(t, s.handler(""))
	tg := newTarget(t, HTTPConfig{URL: srv.url, Protocol: H2C, Connections: 3, StreamsPerConn: 1})
	locals := map[uint64]string{}
	for seq := range uint64(6) {
		resp := tg.Do(t.Context(), Request{Seq: seq})
		if resp.Outcome != Success || resp.Proto != "HTTP/2.0" {
			t.Fatalf("seq %d: %+v", seq, resp)
		}
		locals[seq] = resp.Conn.Local
	}
	if srv.conns.Load() != 3 || locals[0] != locals[3] || locals[1] != locals[4] || locals[0] == locals[1] || locals[1] == locals[2] {
		t.Errorf("%d connections, by sequence %v; want 3, seq%%3 sharing one", srv.conns.Load(), locals)
	}

	// With request 0 holding its connection's one stream, request 2,
	// which the sequence alone would have put beside it, takes the other
	// connection's; once request 4 holds that one too, request 6 waits,
	// and its context ends the wait without sending anything.
	h := newHolder(t)
	tg = newTarget(t, HTTPConfig{URL: h.url, Protocol: H2C, Connections: 2, StreamsPerConn: 1, Prepare: h.holdSeqs(0, 4)})
	h.goDo(tg, 0)
	waitFor(t, "request 0 in its handler", func() bool { return h.held.Load() == 1 })
	if resp := tg.Do(t.Context(), Request{Seq: 2}); resp.Outcome != Success || resp.Proto != "HTTP/2.0" || h.heldOn(resp.Conn.Local) {
		t.Errorf("request beside a connection with no free stream: %+v, want Success on the other connection", resp)
	}
	h.goDo(tg, 4)
	waitFor(t, "a held stream on each connection", func() bool { return h.held.Load() == 2 })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if resp := tg.Do(ctx, Request{Seq: 6}); resp.Outcome != Canceled || resp.Status != 0 || h.answered.Load() != 1 {
		t.Errorf("queued stream: %+v, %d answered; want Canceled before sending, 1 answered", resp, h.answered.Load())
	}
	h.finish()
}

// A round-robin connection that never answers, as one to a Node of the
// zone GD-2 loses, holds only its own request: under the generator, every
// later request takes another connection at its intended time, none
// waits behind the hung one, so the workers never run out, no backlog
// builds and nothing floods out once the hung request's timeout frees its
// connection, which the next request taking it then dials anew (Testing
// "Load generator and L4 balancer", HTTP). Requests queued behind the
// hung connection would hold a worker each until that timeout: with two
// workers, request 3 would never end before it.
func TestRoundRobinHungConnectionHoldsOnlyItsRequest(t *testing.T) {
	var mu sync.Mutex
	hung := "" // client address of the connection that never answers
	var onHung atomic.Int64
	srv := serve(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if hung == "" {
			hung = r.RemoteAddr
		}
		never := r.RemoteAddr == hung
		mu.Unlock()
		if never {
			onHung.Add(1)
			<-r.Context().Done()
		}
	}))
	tg := newTarget(t, HTTPConfig{URL: srv.url, Connections: 3, RoundRobin: true})
	const n = 12
	var smu sync.Mutex
	var samples []Sample
	fk := clocktest.New(epoch())
	g := start(t, tg, Config{
		Rate: 100, Requests: n, Timeout: time.Second, MaxInFlight: 2, Clock: fk,
		OnResult: func(s Sample) {
			smu.Lock()
			samples = append(samples, s)
			smu.Unlock()
		},
	})
	waitFor(t, "request 0 on the connection that never answers", func() bool { return onHung.Load() == 1 })
	for i := 1; i < n; i++ {
		fk.Jump(ms(10))
		waitFor(t, fmt.Sprintf("request %d to end beside the hung connection", i), func() bool {
			p := g.Progress()
			return p.Done == int64(i) && p.InFlight == 1
		})
		if p := g.Progress(); p.Backlog != 0 || p.Missed != 0 {
			t.Fatalf("after request %d: %d waiting, %d missed; want none", i, p.Backlog, p.Missed)
		}
	}
	fk.Jump(time.Second)
	res, err := g.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if m := res.Measured; m.Count(Success) != n-1 || m.Count(Timeout) != 1 || onHung.Load() != 1 {
		t.Errorf("outcomes %v, %d requests on the hung connection; want %d successes, 1 timeout, 1 on it", m.Outcomes, onHung.Load(), n-1)
	}
	mu.Lock()
	never := hung
	mu.Unlock()
	smu.Lock()
	for _, s := range samples {
		if s.Seq == 0 {
			continue
		}
		if s.Outcome != Success || !s.Sent.Equal(s.Intended) || !s.Done.Equal(s.Intended) || s.Conn.Local == never {
			t.Errorf("request %d: %v sent %v after its intended time, %v to its end, on %s; want Success at once off the hung connection %s",
				s.Seq, s.Outcome, s.Sent.Sub(s.Intended), s.Done.Sub(s.Intended), s.Conn.Local, never)
		}
	}
	smu.Unlock()
	// The hung connection closed at its request's timeout; the slot
	// freed then comes last in turn, so the third request from here takes
	// it and dials anew, while the other two reuse their connections.
	dialed := srv.conns.Load()
	for seq := uint64(n); seq < n+3; seq++ {
		if resp := tg.Do(t.Context(), Request{Seq: seq}); resp.Outcome != Success || resp.Conn.Local == never {
			t.Fatalf("request %d after the timeout: %+v", seq, resp)
		}
	}
	if got := srv.conns.Load() - dialed; got != 1 {
		t.Errorf("%d connections dialed after the hung request's timeout, want 1 in its place", got)
	}
}

func TestHTTPDisableKeepAlives(t *testing.T) {
	srv := serve(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	tg := newTarget(t, HTTPConfig{URL: srv.url, DisableKeepAlives: true})
	for seq := range uint64(3) {
		if resp := tg.Do(t.Context(), Request{Seq: seq}); resp.Conn.Reused {
			t.Errorf("seq %d reused a connection", seq)
		}
	}
	if srv.conns.Load() != 3 {
		t.Errorf("%d connections, want one per request", srv.conns.Load())
	}
}

func TestHTTPDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int64
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	resp := newTarget(t, HTTPConfig{URL: srv.url}).Do(t.Context(), Request{})
	if resp.Status != http.StatusFound || resp.Outcome != Success || hits.Load() != 1 {
		t.Errorf("status %d outcome %v after %d requests, want 302 unfollowed", resp.Status, resp.Outcome, hits.Load())
	}
}

// hijack answers with raw bytes, then closes the connection, with a reset
// when rst is set.
func hijack(raw string, rst bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		c, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			panic(err)
		}
		_, _ = buf.WriteString(raw)
		_ = buf.Flush()
		if tc, ok := c.(*net.TCPConn); ok && rst {
			_ = tc.SetLinger(0)
		}
		_ = c.Close()
	}
}

func TestHTTPFailuresAreClassified(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := ln.Addr().String()
	_ = ln.Close()
	reset := serve(t, hijack("", true))
	short := serve(t, hijack("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nabc", false))
	untrusted, _ := tlsServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for _, c := range []struct {
		name   string
		cfg    HTTPConfig
		want   Outcome
		status int
		bytes  int64
	}{
		{"refused", HTTPConfig{URL: "http://" + closedAddr}, Refused, 0, 0},
		{"reset", HTTPConfig{URL: reset.url}, Reset, 0, 0},
		{"closed mid-body", HTTPConfig{URL: short.url}, Closed, 200, 3},
		{"untrusted certificate", HTTPConfig{URL: untrusted.URL, Protocol: H2}, TLSError, 0, 0},
	} {
		resp := newTarget(t, c.cfg).Do(t.Context(), Request{})
		if resp.Outcome != c.want || resp.Status != c.status || resp.Bytes != c.bytes || resp.Err == nil {
			t.Errorf("%s: outcome %v status %d bytes %d err %v; want %v, %d, %d", c.name, resp.Outcome, resp.Status, resp.Bytes, resp.Err, c.want, c.status, c.bytes)
		}
	}
}

func TestNewHTTPRejects(t *testing.T) {
	keys, _ := Cycle("k", 1)
	for _, c := range []struct {
		name string
		cfg  HTTPConfig
	}{
		{"scheme", HTTPConfig{URL: "ftp://127.0.0.1/"}},
		{"no host", HTTPConfig{URL: "http://"}},
		{"unparsable", HTTPConfig{URL: "http://[::1"}},
		{"h2c over https", HTTPConfig{URL: "https://127.0.0.1/", Protocol: H2C}},
		{"h2 over http", HTTPConfig{URL: "http://127.0.0.1/", Protocol: H2}},
		{"unknown protocol", HTTPConfig{URL: "http://127.0.0.1/", Protocol: 7}},
		{"key header without keys", HTTPConfig{URL: "http://127.0.0.1/", KeyHeader: "k"}},
		{"keys without header", HTTPConfig{URL: "http://127.0.0.1/", Keys: keys}},
		{"streams on HTTP/1.1", HTTPConfig{URL: "http://127.0.0.1/", StreamsPerConn: 2}},
		{"keep-alives on HTTP/2", HTTPConfig{URL: "http://127.0.0.1/", Protocol: H2C, DisableKeepAlives: true}},
		{"round robin on HTTP/2", HTTPConfig{URL: "http://127.0.0.1/", Protocol: H2C, RoundRobin: true}},
		{"negative connections", HTTPConfig{URL: "http://127.0.0.1/", Connections: -1}},
		{"bad method", HTTPConfig{URL: "http://127.0.0.1/", Method: "BAD METHOD"}},
	} {
		if _, err := NewHTTP(c.cfg); err == nil {
			t.Errorf("%s: NewHTTP succeeded", c.name)
		}
	}
	tg, err := NewHTTP(HTTPConfig{URL: "http://127.0.0.1/", Protocol: H2C})
	if err != nil || len(tg.groups) != 1 || len(tg.groups[0].lanes) != DefaultHTTP2Connections || cap(tg.groups[0].free) != DefaultHTTP2Connections*DefaultHTTP2Streams {
		t.Errorf("h2c defaults: %d lanes, %d stream slots, err %v; want PBB S3's 64 of 64", len(tg.groups[0].lanes), cap(tg.groups[0].free), err)
	}
	tg, err = NewHTTP(HTTPConfig{URL: "http://127.0.0.1/"})
	if err != nil || len(tg.groups) != 1 || tg.groups[0].lanes[0].tr.MaxConnsPerHost != DefaultHTTP1Connections || tg.groups[0].free != nil {
		t.Errorf("HTTP/1.1 default connections %d, err %v; want PBB S1's 256 in one pool", tg.groups[0].lanes[0].tr.MaxConnsPerHost, err)
	}
	tg, err = NewHTTP(HTTPConfig{URL: "http://127.0.0.1/", RoundRobin: true})
	if err != nil || len(tg.groups) != 1 || len(tg.groups[0].lanes) != DefaultHTTP1Connections || tg.groups[0].lanes[0].tr.MaxConnsPerHost != 1 || cap(tg.groups[0].free) != DefaultHTTP1Connections {
		t.Errorf("round-robin HTTP/1.1 defaults: %d lanes, err %v; want PBB S1's 256 of one connection and one request each", len(tg.groups[0].lanes), err)
	}
}

// Through the generator, HTTP requests are counted by status, connection
// and bytes, and a request hanging past Timeout ends as Timeout on the
// injected clock.
func TestRunAgainstHTTP(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hang" {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = io.WriteString(w, "0123456789")
	}))
	fk := clocktest.New(epoch())
	g := start(t, newTarget(t, HTTPConfig{URL: srv.url, Connections: 4}), Config{Rate: 50, Duration: ms(200), Clock: fk})
	res := drive(t, g, fk, every(ms(20)))
	m := res.Measured
	if m.Count(Success) != 10 || m.Statuses[200] != 10 || m.Bytes != 100 || m.NewConns < 1 || m.NewConns > 4 {
		t.Errorf("success %d statuses %v bytes %d new connections %d", m.Count(Success), m.Statuses, m.Bytes, m.NewConns)
	}

	g = start(t, newTarget(t, HTTPConfig{URL: srv.url + "/hang"}), Config{Rate: 1, Requests: 1, Timeout: time.Second, Clock: fk})
	waitFor(t, "the request's timeout to be armed", func() bool { return fk.Pending() == 1 })
	fk.Advance(time.Second)
	res, err := g.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res.Measured.Count(Timeout) != 1 || !errors.Is(res.Failures[0].Err, ErrTimeout) {
		t.Errorf("hanging request: outcomes %v, err %v; want Timeout", res.Measured.Outcomes, res.Failures[0].Err)
	}
}
