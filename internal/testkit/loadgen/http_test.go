// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/problem"
)

// server serves HTTP/1.1 on loopback and counts connections.
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
	srv := &http.Server{
		Handler: h, ReadHeaderTimeout: 10 * time.Second,
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

func newTarget(t *testing.T, c HTTPConfig) *HTTPTarget {
	t.Helper()
	tg, err := NewHTTP(c)
	if err != nil {
		t.Fatalf("NewHTTP = %v", err)
	}
	t.Cleanup(func() { _ = tg.Close() })
	return tg
}

// Requests are plain GETs of the URL over HTTP/1.1, reusing a keep-alive
// connection.
func TestHTTPRequestShape(t *testing.T) {
	type sawReq struct{ method, uri, proto, encoding string }
	var mu sync.Mutex
	var reqs []sawReq
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, sawReq{r.Method, r.RequestURI, r.Proto, r.Header.Get("Accept-Encoding")})
		mu.Unlock()
		_, _ = io.WriteString(w, "pong")
	}))
	tg := newTarget(t, HTTPConfig{URL: srv.url + "/work?x=1"})
	for seq := range uint64(3) {
		if resp := tg.Do(t.Context()); resp.Outcome != Success || resp.Status != 200 || resp.Err != nil {
			t.Errorf("seq %d: %+v", seq, resp)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 3 || srv.conns.Load() != 1 {
		t.Fatalf("server saw %d requests on %d connections, want 3 on 1", len(reqs), srv.conns.Load())
	}
	for i, r := range reqs {
		if want := (sawReq{http.MethodGet, "/work?x=1", "HTTP/1.1", ""}); r != want {
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
		path    string
		code    string
		outcome Outcome
	}{
		{"/problem", "RZ-RT-005", ServerError},
		{"/plain", "", ServerError},
		{"/ok-problem", "", Success},
	} {
		resp := newTarget(t, HTTPConfig{URL: srv.url + c.path}).Do(t.Context())
		if resp.Code != c.code || resp.Outcome != c.outcome {
			t.Errorf("%s: code %q outcome %v, want %q %v", c.path, resp.Code, resp.Outcome, c.code, c.outcome)
		}
	}
}

func TestHTTPConnectionBound(t *testing.T) {
	release := make(chan struct{})
	var inHandler atomic.Int64
	srv := serve(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		inHandler.Add(1)
		<-release
	}))
	tg := newTarget(t, HTTPConfig{URL: srv.url, Connections: 2})
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() { tg.Do(t.Context()) })
	}
	waitFor(t, "two requests in handlers", func() bool { return inHandler.Load() == 2 })
	close(release)
	wg.Wait()
	if srv.conns.Load() != 2 || inHandler.Load() != 6 {
		t.Errorf("%d connections served %d requests, want 2 serving 6", srv.conns.Load(), inHandler.Load())
	}
}

func TestHTTPDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int64
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	resp := newTarget(t, HTTPConfig{URL: srv.url}).Do(t.Context())
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
	untrusted := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	untrusted.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	untrusted.StartTLS()
	t.Cleanup(untrusted.Close)
	for _, c := range []struct {
		name   string
		url    string
		want   Outcome
		status int
	}{
		{"refused", "http://" + closedAddr, Refused, 0},
		{"reset", reset.url, Reset, 0},
		{"closed mid-body", short.url, Closed, 200},
		{"untrusted certificate", untrusted.URL, TLSError, 0},
	} {
		resp := newTarget(t, HTTPConfig{URL: c.url}).Do(t.Context())
		if resp.Outcome != c.want || resp.Status != c.status || resp.Err == nil {
			t.Errorf("%s: outcome %v status %d err %v; want %v, %d", c.name, resp.Outcome, resp.Status, resp.Err, c.want, c.status)
		}
	}
}

func TestNewHTTPRejects(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  HTTPConfig
	}{
		{"scheme", HTTPConfig{URL: "ftp://127.0.0.1/"}},
		{"no host", HTTPConfig{URL: "http://"}},
		{"unparsable", HTTPConfig{URL: "http://[::1"}},
		{"negative connections", HTTPConfig{URL: "http://127.0.0.1/", Connections: -1}},
	} {
		if _, err := NewHTTP(c.cfg); err == nil {
			t.Errorf("%s: NewHTTP succeeded", c.name)
		}
	}
	tg, err := NewHTTP(HTTPConfig{URL: "http://127.0.0.1/"})
	if err != nil || tg.tr.MaxConnsPerHost != DefaultConnections || tg.tr.MaxIdleConnsPerHost != DefaultConnections {
		t.Errorf("default connections %d, err %v; want %d", tg.tr.MaxConnsPerHost, err, DefaultConnections)
	}
}

// Through the generator, HTTP requests are counted by status, and a
// request hanging past Timeout ends as Timeout on the injected clock.
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
	res := drive(t, g, fk, ms(20))
	if res.Offered != 10 || res.Outcomes[Success] != 10 || res.Statuses[200] != 10 || srv.conns.Load() < 1 || srv.conns.Load() > 4 {
		t.Errorf("offered %d success %d statuses %v over %d connections", res.Offered, res.Outcomes[Success], res.Statuses, srv.conns.Load())
	}

	g = start(t, newTarget(t, HTTPConfig{URL: srv.url + "/hang"}), Config{Rate: 1, Duration: time.Second, Timeout: time.Second, Clock: fk})
	waitFor(t, "the request's timeout to be armed", func() bool { return fk.Pending() == 1 })
	fk.Advance(time.Second)
	res, err := g.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcomes[Timeout] != 1 {
		t.Errorf("hanging request: outcomes %v; want one Timeout", res.Outcomes)
	}
}
