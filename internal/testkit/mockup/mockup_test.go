// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests for mockup: its behaviors table-tested (TestBehaviors) over
// HTTP/1.1, h2c, and HTTP/1.1 and HTTP/2 over TLS.

func TestBehaviors(t *testing.T) {
	pki := newTestPKI(t)
	type check func(t *testing.T, p proto, r result, err error)
	for _, tc := range []struct {
		name   string
		b      Behavior
		method string
		body   string
		query  string
		check  check
	}{
		{
			name: "zero value", b: Behavior{},
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if len(r.body) != 0 || r.resp.ContentLength != 0 {
					t.Fatalf("body %q length %d", r.body, r.resp.ContentLength)
				}
			},
		},
		{
			name: "status and header", b: Behavior{Status: 503, Header: http.Header{"Retry-After": {"7"}, "X-A": {"1", "2"}}},
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusServiceUnavailable)
				if r.resp.Header.Get("Retry-After") != "7" || len(r.resp.Header.Values("X-A")) != 2 {
					t.Fatalf("header %v", r.resp.Header)
				}
			},
		},
		{
			name: "literal body", b: Behavior{Body: []byte(`{"ok":true}`), ContentType: "application/json"},
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if string(r.body) != `{"ok":true}` || r.resp.Header.Get("Content-Type") != "application/json" || r.resp.ContentLength != 11 {
					t.Fatalf("body %q type %q length %d", r.body, r.resp.Header.Get("Content-Type"), r.resp.ContentLength)
				}
			},
		},
		{
			name: "header replaces content type", b: Behavior{Body: []byte("x"), Header: http.Header{"Content-Type": {"text/plain"}}},
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if r.resp.Header.Get("Content-Type") != "text/plain" {
					t.Fatalf("type %q", r.resp.Header.Get("Content-Type"))
				}
			},
		},
		{
			name: "large body", b: Behavior{Body: bytes.Repeat([]byte("0123456789"), 10_000)},
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if !bytes.Equal(r.body, bytes.Repeat([]byte("0123456789"), 10_000)) || r.resp.ContentLength != 100_000 {
					t.Fatalf("body of %d bytes (length %d) differs from the Behavior's", len(r.body), r.resp.ContentLength)
				}
				if r.resp.Header.Get("Content-Type") != "application/octet-stream" {
					t.Fatalf("type %q", r.resp.Header.Get("Content-Type"))
				}
			},
		},
		{
			name: "HEAD", b: Behavior{Body: bytes.Repeat([]byte("x"), 300)}, method: http.MethodHead,
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if len(r.body) != 0 || r.resp.Header.Get("Content-Length") != "300" {
					t.Fatalf("body %d bytes, Content-Length %q", len(r.body), r.resp.Header.Get("Content-Length"))
				}
			},
		},
		{
			name: "204 has no body", b: Behavior{Status: 204, Body: bytes.Repeat([]byte("x"), 300)},
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusNoContent)
				if len(r.body) != 0 {
					t.Fatalf("body %d bytes", len(r.body))
				}
			},
		},
		{
			name: "echo", b: Behavior{Echo: true}, method: http.MethodPost, body: "hello body", query: "?a=1&b=%20",
			check: func(t *testing.T, p proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if r.resp.Header.Get("Content-Type") != "application/json" {
					t.Fatalf("type %q", r.resp.Header.Get("Content-Type"))
				}
				var e Echo
				if err := json.Unmarshal(r.body, &e); err != nil {
					t.Fatal(err)
				}
				if e.Method != http.MethodPost || e.Path != "/p" || e.RawQuery != "a=1&b=%20" || e.URI != "/p?a=1&b=%20" ||
					string(e.Body) != "hello body" || e.BodyBytes != 10 || e.Server != "m1" || e.Proto != p.wantProto() {
					t.Fatalf("echo = %+v", e)
				}
				if e.TLS != p.tls() || (p.tls() && e.ServerName != "upstream.test") {
					t.Fatalf("echo TLS %v server name %q", e.TLS, e.ServerName)
				}
				if p == h2TLS && e.ALPN != "h2" {
					t.Fatalf("ALPN %q", e.ALPN)
				}
				if got := e.Header["X-Test"]; len(got) != 1 || got[0] != "v" {
					t.Fatalf("echo header %v", e.Header)
				}
				if e.Host == "" || e.RemoteAddr == "" {
					t.Fatalf("echo host %q remote %q", e.Host, e.RemoteAddr)
				}
			},
		},
		{
			name: "echo empty body", b: Behavior{Echo: true, ContentType: "application/x-echo"},
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if !bytes.Contains(r.body, []byte(`"body":""`)) || r.resp.Header.Get("Content-Type") != "application/x-echo" {
					t.Fatalf("echo %s type %q", r.body, r.resp.Header.Get("Content-Type"))
				}
			},
		},
		{
			name: "delay", b: Behavior{Delay: 60 * time.Millisecond},
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if r.elapsed < 60*time.Millisecond {
					t.Fatalf("answered after %v", r.elapsed)
				}
			},
		},
	} {
		for _, p := range []proto{h1, h2c, h1TLS, h2TLS} {
			t.Run(tc.name+"/"+string(p), func(t *testing.T) {
				c := Config{Name: "m1", Behavior: tc.b}
				if p.tls() {
					c.TLS = pki.serverTLS()
				}
				s := start(t, c)
				method := tc.method
				if method == "" {
					method = http.MethodGet
				}
				r, err := do(t, client(t, p, pki), method, s.URL()+"/p"+tc.query, tc.body, http.Header{"X-Test": {"v"}})
				tc.check(t, p, r, err)
			})
		}
	}
}

func ok(t *testing.T, r result, err error, status int) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if r.bodyErr != nil {
		t.Fatalf("body: %v", r.bodyErr)
	}
	if r.resp.StatusCode != status {
		t.Fatalf("status %d, want %d", r.resp.StatusCode, status)
	}
}

// TestRoutes checks SetRoute: a route answers its path with its own copy
// of the Behavior, every other path keeps the default, a later SetRoute
// replaces it, and an invalid Behavior is refused.
func TestRoutes(t *testing.T) {
	s := start(t, Config{Behavior: Behavior{Body: []byte("default")}})
	c := client(t, h1, testPKI{})
	get := func(path string) string {
		t.Helper()
		r, err := do(t, c, http.MethodGet, s.URL()+path, "", nil)
		ok(t, r, err, 200)
		return string(r.body)
	}
	body := []byte("route a")
	if err := s.SetRoute("/a", Behavior{Body: body}); err != nil {
		t.Fatal(err)
	}
	body[0] = 'X' // the Server keeps its own copy
	if got := get("/a"); got != "route a" {
		t.Fatalf("/a = %q", got)
	}
	if got := get("/b"); got != "default" {
		t.Fatalf("/b = %q", got)
	}
	if err := s.SetRoute("/a", Behavior{Body: []byte("route a again")}); err != nil {
		t.Fatal(err)
	}
	if got := get("/a"); got != "route a again" {
		t.Fatalf("/a after a second SetRoute = %q", got)
	}
	if err := s.SetRoute("/bad", Behavior{Status: 700}); err == nil {
		t.Fatal("SetRoute accepted status 700")
	}
	if err := s.SetRoute("/bad", Behavior{Delay: -time.Second}); err == nil {
		t.Fatal("SetRoute accepted a negative delay")
	}
}

func TestRequestLog(t *testing.T) { // The mock's request log is scanned for stripped credentials
	pki := newTestPKI(t)
	s := start(t, Config{TLS: pki.serverTLS(), LogSize: 3, MaxLoggedBody: 4})
	c := client(t, h2TLS, pki)
	for i := range 5 {
		r, err := do(t, c, http.MethodPost, s.URL()+"/r"+strconv.Itoa(i)+"?q=1", "body-"+strconv.Itoa(i), http.Header{"Authorization": {"Bearer t" + strconv.Itoa(i)}})
		ok(t, r, err, 200)
	}
	reqs := s.Requests()
	if len(reqs) != 3 {
		t.Fatalf("logged %d requests, want 3 (ring)", len(reqs))
	}
	for i, r := range reqs {
		n := i + 2
		if r.Path != "/r"+strconv.Itoa(n) || r.URI != "/r"+strconv.Itoa(n)+"?q=1" {
			t.Errorf("entry %d: %+v", i, r)
		}
		if string(r.Body) != "body" || !r.BodyTruncated {
			t.Errorf("entry %d: body %q (truncated %v)", i, r.Body, r.BodyTruncated)
		}
		if r.Header.Get("Authorization") != "Bearer t"+strconv.Itoa(n) || r.Method != http.MethodPost || r.Proto != "HTTP/2.0" {
			t.Errorf("entry %d: %s %s %v", i, r.Method, r.Proto, r.Header)
		}
		if r.RemoteAddr == "" || r.Host == "" {
			t.Errorf("entry %d: remote %q host %q", i, r.RemoteAddr, r.Host)
		}
	}
	if st := s.Stats(); st.LogEvicted != 2 || st.Requests != 5 {
		t.Fatalf("Stats = %+v, want 5 requests, 2 evicted from a ring of 3", st)
	}
	r, err := do(t, c, http.MethodGet, s.URL()+"/after", "", nil)
	ok(t, r, err, 200)
	if got := s.Requests(); len(got) != 3 || got[2].Path != "/after" || got[2].BodyTruncated {
		t.Fatalf("after one more request: %+v, want /after last with an empty body not cut", got)
	}
}

// TestEchoFraming covers the framing fields of Echo, which protocol
// conformance checks forwarded framing with: net/http moves
// Transfer-Encoding out of the header, so Echo reports it apart, and the
// log's header never holds it.
func TestEchoFraming(t *testing.T) {
	type body int
	const (
		none body = iota
		sized
		streamed
	)
	for _, tc := range []struct {
		name     string
		p        proto
		body     body
		close    bool
		wantLen  int64
		wantTE   []string
		wantBody int64
	}{
		{name: "h1 sized", p: h1, body: sized, wantLen: 5, wantBody: 5},
		{name: "h1 chunked", p: h1, body: streamed, wantLen: -1, wantTE: []string{"chunked"}, wantBody: 5},
		{name: "h1 close", p: h1, body: none, close: true, wantLen: 0},
		{name: "h2c sized", p: h2c, body: sized, wantLen: 5, wantBody: 5},
		{name: "h2c streamed", p: h2c, body: streamed, wantLen: -1, wantBody: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := start(t, Config{Behavior: Behavior{Echo: true}})
			var rd io.Reader
			switch tc.body {
			case sized:
				rd = strings.NewReader("hello")
			case streamed:
				rd = io.MultiReader(strings.NewReader("hel"), strings.NewReader("lo"))
			case none:
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL()+"/f", rd)
			if err != nil {
				t.Fatal(err)
			}
			req.Close = tc.close
			resp, err := client(t, tc.p, testPKI{}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			var e Echo
			if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
				t.Fatal(err)
			}
			if e.ContentLength != tc.wantLen || !slices.Equal(e.TransferEncoding, tc.wantTE) || e.Close != tc.close || e.BodyBytes != tc.wantBody {
				t.Errorf("echo: ContentLength %d TransferEncoding %q Close %v BodyBytes %d, want %d %q %v %d",
					e.ContentLength, e.TransferEncoding, e.Close, e.BodyBytes, tc.wantLen, tc.wantTE, tc.close, tc.wantBody)
			}
			reqs := s.Requests()
			if len(reqs) != 1 {
				t.Fatalf("logged %d requests, want 1", len(reqs))
			}
			if r := reqs[0]; r.BodyTruncated || r.Header.Get("Transfer-Encoding") != "" {
				t.Errorf("log: truncated %v header %v", r.BodyTruncated, r.Header)
			}
		})
	}
}

func TestRequestLogDisabled(t *testing.T) {
	s := start(t, Config{LogSize: -1, MaxLoggedBody: -1})
	c := client(t, h1, testPKI{})
	for range 3 {
		r, err := do(t, c, http.MethodPost, s.URL()+"/", "abc", nil)
		ok(t, r, err, 200)
	}
	if len(s.Requests()) != 0 {
		t.Fatal("disabled log kept entries")
	}
	if err := s.WaitRequests(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Requests != 3 || st.Connections != 1 {
		t.Fatalf("Stats = %+v, want 3 requests on one keep-alive connection", st)
	}
}

func TestWaitRequests(t *testing.T) {
	s := start(t, Config{})
	c := client(t, h1, testPKI{})
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.WaitRequests(short, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitRequests = %v, want DeadlineExceeded", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.WaitRequests(context.Background(), 2) }()
	for range 2 {
		r, err := do(t, c, http.MethodGet, s.URL()+"/", "", nil)
		ok(t, r, err, 200)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestClosedServer(t *testing.T) { // ErrClosed after Close
	s := start(t, Config{})
	c := client(t, h1, testPKI{})
	r, err := do(t, c, http.MethodGet, s.URL()+"/", "", nil)
	ok(t, r, err, 200)
	waiting := make(chan error, 1)
	go func() { waiting <- s.WaitRequests(context.Background(), 2) }()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A waiter blocked across Close ends with ErrClosed.
	if err := <-waiting; !errors.Is(err, ErrClosed) {
		t.Fatalf("WaitRequests across Close = %v, want ErrClosed", err)
	}
	if err := s.WaitRequests(context.Background(), 2); !errors.Is(err, ErrClosed) {
		t.Fatalf("WaitRequests after Close = %v, want ErrClosed", err)
	}
	// Requests received before Close still count.
	if err := s.WaitRequests(context.Background(), 1); err != nil {
		t.Fatalf("WaitRequests(1) after Close = %v", err)
	}
	if err := s.SetRoute("/x", Behavior{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("SetRoute after Close = %v, want ErrClosed", err)
	}
	if len(s.Requests()) != 1 {
		t.Fatal("the request log is not readable after Close")
	}
}

// TestProtocols checks the protocols of both listeners: over TLS, ALPN
// negotiates HTTP/2 or HTTP/1.1 as the client offers; in cleartext,
// HTTP/1.1 and h2c with prior knowledge share one port.
func TestProtocols(t *testing.T) {
	pki := newTestPKI(t)
	s := start(t, Config{TLS: pki.serverTLS(), Behavior: Behavior{Echo: true}})
	for _, p := range []proto{h1TLS, h2TLS} {
		r, err := do(t, client(t, p, pki), http.MethodGet, s.URL()+"/", "", nil)
		ok(t, r, err, 200)
		if r.resp.Proto != p.wantProto() {
			t.Fatalf("%s: proto %q", p, r.resp.Proto)
		}
	}
	if !strings.HasPrefix(s.URL(), "https://127.0.0.1:") {
		t.Fatalf("TLS URL %q", s.URL())
	}
	s2 := start(t, Config{})
	for _, p := range []proto{h1, h2c} {
		r, err := do(t, client(t, p, pki), http.MethodGet, s2.URL()+"/", "", nil)
		ok(t, r, err, 200)
		if r.resp.Proto != p.wantProto() {
			t.Fatalf("%s: proto %q", p, r.resp.Proto)
		}
	}
	if s2.Name() != "" || !strings.HasPrefix(s2.URL(), "http://127.0.0.1:") || strings.TrimPrefix(s2.URL(), "http://") != s2.Addr() {
		t.Fatalf("Name %q URL %q Addr %q", s2.Name(), s2.URL(), s2.Addr())
	}
}

// TestCloseEndsDelayedRequests checks that the end of the Start context
// closes the Server, ending a request it delays, and that every goroutine
// ends with it.
func TestCloseEndsDelayedRequests(t *testing.T) {
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, Config{Behavior: Behavior{Delay: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	c := client(t, h1, testPKI{})
	done := make(chan error, 1)
	go func() {
		_, err := do(t, c, http.MethodGet, s.URL()+"/", "", nil)
		done <- err
	}()
	if err := s.WaitRequests(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	cancel() // the Start context ends: the Server closes
	if err := <-done; err == nil {
		t.Fatal("delayed request completed")
	}
	if err := s.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	if d := time.Since(began); d > 5*time.Second {
		t.Fatalf("close took %v", d)
	}
	c.CloseIdleConnections()
	waitFor(t, func() bool { return runtime.NumGoroutine() <= base })
}

func TestAbortedWhenClosed(t *testing.T) {
	s := start(t, Config{})
	_ = s.Close()
	defer func() {
		if r := recover(); r != http.ErrAbortHandler { //nolint:errorlint // the panic value is compared, not an error chain
			t.Fatalf("recovered %v, want ErrAbortHandler", r)
		}
	}()
	s.ServeHTTP(nil, nil)
}

func TestConfigErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Config
		want string
	}{
		{"status", Config{Behavior: Behavior{Status: 99}}, "status 99"},
		{"delay", Config{Behavior: Behavior{Delay: -time.Second}}, "negative delay"},
		{"listen", Config{Listen: "256.0.0.1:0"}, "listen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Start(context.Background(), tc.c)
			if err == nil {
				_ = s.Close()
				t.Fatal("Start succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReadBody(t *testing.T) {
	if b, n := readBody(nil, 10); b != nil || n != 0 {
		t.Fatalf("nil body: %q %d", b, n)
	}
	b, n := readBody(strings.NewReader("0123456789"), 4)
	if string(b) != "0123" || n != 10 {
		t.Fatalf("readBody = %q %d", b, n)
	}
}

