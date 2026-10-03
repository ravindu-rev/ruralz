// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockup

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Tests for WP-84 mockup (11 section 3, F 32, req 39): "Done when" mockup
// behaviors table-tested (TestBehaviors) over HTTP/1.1, h2c, and HTTP/1.1
// and HTTP/2 over TLS.

func TestBehaviors(t *testing.T) { // WP-84 Done when: behaviors table-tested
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
				if r.resp.Header.Get(HeaderServer) != "m1" {
					t.Fatalf("%s = %q", HeaderServer, r.resp.Header.Get(HeaderServer))
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
			name: "generated body size", b: Behavior{BodySize: 100_000},
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if !bytes.Equal(r.body, GeneratedBody(100_000)) || r.resp.ContentLength != 100_000 {
					t.Fatalf("body of %d bytes (length %d) differs from GeneratedBody", len(r.body), r.resp.ContentLength)
				}
				if r.resp.Header.Get("Content-Type") != "application/octet-stream" {
					t.Fatalf("type %q", r.resp.Header.Get("Content-Type"))
				}
			},
		},
		{
			name: "S1 1 KiB", b: Behavior{BodySize: 1024},
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if len(r.body) != 1024 {
					t.Fatalf("body %d bytes", len(r.body))
				}
			},
		},
		{
			name: "chunked", b: Behavior{BodySize: 5000, Chunked: true},
			check: func(t *testing.T, p proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if len(r.body) != 5000 || r.resp.ContentLength != -1 {
					t.Fatalf("body %d bytes length %d", len(r.body), r.resp.ContentLength)
				}
				if p.wantProto() == "HTTP/1.1" && (len(r.resp.TransferEncoding) != 1 || r.resp.TransferEncoding[0] != "chunked") {
					t.Fatalf("transfer encoding %v", r.resp.TransferEncoding)
				}
			},
		},
		{
			name: "HEAD", b: Behavior{BodySize: 300}, method: http.MethodHead,
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if len(r.body) != 0 || r.resp.Header.Get("Content-Length") != "300" {
					t.Fatalf("body %d bytes, Content-Length %q", len(r.body), r.resp.Header.Get("Content-Length"))
				}
			},
		},
		{
			name: "204 has no body", b: Behavior{Status: 204, BodySize: 300},
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
			name: "fixed delay", b: Behavior{Delay: Fixed(60 * time.Millisecond)},
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if r.elapsed < 60*time.Millisecond {
					t.Fatalf("answered after %v", r.elapsed)
				}
			},
		},
		{
			name: "slow body", b: Behavior{BodySize: 40, ChunkSize: 10, ChunkInterval: 30 * time.Millisecond},
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if !bytes.Equal(r.body, GeneratedBody(40)) || r.elapsed < 90*time.Millisecond {
					t.Fatalf("body %q after %v", r.body, r.elapsed)
				}
			},
		},
		{
			name: "slow body default chunk", b: Behavior{Body: bytes.Repeat([]byte("z"), 2*DefaultChunkSize+1), ChunkInterval: 20 * time.Millisecond},
			check: func(t *testing.T, _ proto, r result, err error) {
				ok(t, r, err, http.StatusOK)
				if len(r.body) != 2*DefaultChunkSize+1 || r.elapsed < 40*time.Millisecond {
					t.Fatalf("body %d bytes after %v", len(r.body), r.elapsed)
				}
			},
		},
		{
			name: "reset conn before headers", b: Behavior{Reset: ResetConn},
			check: func(t *testing.T, _ proto, _ result, err error) {
				if err == nil {
					t.Fatal("request succeeded")
				}
			},
		},
		{
			name: "reset stream before headers", b: Behavior{Reset: ResetStream},
			check: func(t *testing.T, _ proto, _ result, err error) {
				if err == nil {
					t.Fatal("request succeeded")
				}
			},
		},
		{
			name: "reset stream after 100 bytes", b: Behavior{BodySize: 1000, Reset: ResetStream, ResetAfterHeaders: true, ResetAfterBytes: 100},
			check: func(t *testing.T, _ proto, r result, err error) {
				if err != nil {
					t.Fatalf("headers not received: %v", err)
				}
				if r.resp.StatusCode != http.StatusOK || r.bodyErr == nil || len(r.body) != 100 {
					t.Fatalf("status %d, %d body bytes, body error %v", r.resp.StatusCode, len(r.body), r.bodyErr)
				}
			},
		},
		{
			name: "reset conn after headers", b: Behavior{BodySize: 1000, Reset: ResetConn, ResetAfterHeaders: true, ResetAfterBytes: 10},
			check: func(t *testing.T, _ proto, r result, err error) {
				if err == nil && r.bodyErr == nil {
					t.Fatalf("complete response of %d bytes", len(r.body))
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

func TestResetConnIsTCPReset(t *testing.T) { // faultproxy-style RST at the peer (SO_LINGER 0)
	s := start(t, Config{Behavior: Behavior{Reset: ResetConn}})
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err = io.ReadAll(conn)
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("read err = %v, want ECONNRESET", err)
	}
	if st := s.Stats(); st.Resets != 1 {
		t.Fatalf("Stats = %+v", st)
	}
}

func TestResetStreamKeepsH2Connection(t *testing.T) {
	s := start(t, Config{})
	if err := s.SetRoute("/reset", Behavior{Reset: ResetStream}); err != nil {
		t.Fatal(err)
	}
	c := client(t, h2c, testPKI{})
	for range 3 {
		if _, err := do(t, c, http.MethodGet, s.URL()+"/reset", "", nil); err == nil {
			t.Fatal("reset stream succeeded")
		}
	}
	r, err := do(t, c, http.MethodGet, s.URL()+"/ok", "", nil)
	ok(t, r, err, http.StatusOK)
	if st := s.Stats(); st.Connections != 1 || st.Resets < 3 {
		t.Fatalf("Stats = %+v, want one connection surviving the stream resets", st)
	}
}

func TestResetConnEndsEveryH2Stream(t *testing.T) {
	s := start(t, Config{})
	if err := s.SetRoute("/slow", Behavior{Delay: Fixed(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRoute("/reset", Behavior{Reset: ResetConn}); err != nil {
		t.Fatal(err)
	}
	c := client(t, h2c, testPKI{})
	done := make(chan error, 1)
	go func() {
		_, err := do(t, c, http.MethodGet, s.URL()+"/slow", "", nil)
		done <- err
	}()
	waitFor(t, func() bool { return s.Stats().Active == 1 })
	_, _ = do(t, c, http.MethodGet, s.URL()+"/reset", "", nil)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("slow stream completed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("slow stream survived the connection reset")
	}
}

func TestOverrides(t *testing.T) {
	s := start(t, Config{Overrides: true, Behavior: Behavior{Body: []byte("default")}})
	c := client(t, h1, testPKI{})
	for _, tc := range []struct {
		name   string
		header http.Header
		check  func(t *testing.T, r result, err error)
	}{
		{"none", nil, func(t *testing.T, r result, err error) {
			ok(t, r, err, 200)
			if string(r.body) != "default" {
				t.Fatalf("body %q", r.body)
			}
		}},
		{"status", http.Header{HeaderStatus: {"418"}}, func(t *testing.T, r result, err error) { ok(t, r, err, 418) }},
		{"body size", http.Header{HeaderBodySize: {"77"}}, func(t *testing.T, r result, err error) {
			ok(t, r, err, 200)
			if !bytes.Equal(r.body, GeneratedBody(77)) {
				t.Fatalf("body %q", r.body)
			}
		}},
		{"delay", http.Header{HeaderDelay: {"50ms"}}, func(t *testing.T, r result, err error) {
			ok(t, r, err, 200)
			if r.elapsed < 50*time.Millisecond {
				t.Fatalf("elapsed %v", r.elapsed)
			}
		}},
		{"echo", http.Header{HeaderEcho: {"true"}}, func(t *testing.T, r result, err error) {
			ok(t, r, err, 200)
			var e Echo
			if err := json.Unmarshal(r.body, &e); err != nil || e.Header[HeaderEcho][0] != "true" {
				t.Fatalf("echo %s: %v", r.body, err)
			}
		}},
		{"slow body", http.Header{HeaderBodySize: {"30"}, HeaderChunkSize: {"10"}, HeaderChunkInterval: {"25ms"}}, func(t *testing.T, r result, err error) {
			ok(t, r, err, 200)
			if len(r.body) != 30 || r.elapsed < 50*time.Millisecond {
				t.Fatalf("%d bytes after %v", len(r.body), r.elapsed)
			}
		}},
		{"reset stream after", http.Header{HeaderBodySize: {"100"}, HeaderReset: {"stream"}, HeaderResetAfter: {"20"}}, func(t *testing.T, r result, err error) {
			if err != nil || r.bodyErr == nil || len(r.body) != 20 {
				t.Fatalf("err %v body %d bytes, body error %v", err, len(r.body), r.bodyErr)
			}
		}},
		{"reset conn", http.Header{HeaderReset: {"CONN"}}, func(t *testing.T, _ result, err error) {
			if err == nil {
				t.Fatal("request succeeded")
			}
		}},
		{"reset none", http.Header{HeaderReset: {"none"}}, func(t *testing.T, r result, err error) { ok(t, r, err, 200) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := do(t, c, http.MethodGet, s.URL()+"/", "", tc.header)
			tc.check(t, r, err)
		})
	}
	for _, h := range []http.Header{
		{HeaderStatus: {"abc"}},
		{HeaderStatus: {"99"}},
		{HeaderDelay: {"soon"}},
		{HeaderDelay: {"-1s"}},
		{HeaderBodySize: {"x"}},
		{HeaderBodySize: {"-1"}},
		{HeaderEcho: {"maybe"}},
		{HeaderReset: {"hard"}},
		{HeaderResetAfter: {"x"}},
		{HeaderChunkSize: {"x"}},
		{HeaderChunkInterval: {"x"}},
	} {
		r, err := do(t, c, http.MethodGet, s.URL()+"/", "", h)
		ok(t, r, err, http.StatusBadRequest)
		if !strings.Contains(string(r.body), "mockup: invalid X-Mockup-") {
			t.Errorf("%v: body %q", h, r.body)
		}
	}
	// Without Config.Overrides the headers are ignored.
	s2 := start(t, Config{})
	r, err := do(t, c, http.MethodGet, s2.URL()+"/", "", http.Header{HeaderStatus: {"500"}})
	ok(t, r, err, http.StatusOK)
}

func TestRoutes(t *testing.T) {
	s := start(t, Config{Behavior: Behavior{Body: []byte("default")}})
	c := client(t, h1, testPKI{})
	get := func(path string) string {
		t.Helper()
		r, err := do(t, c, http.MethodGet, s.URL()+path, "", nil)
		ok(t, r, err, 200)
		return string(r.body)
	}
	hdr := http.Header{"X-A": {"1"}}
	if err := s.SetRoute("/a", Behavior{Body: []byte("route a"), Header: hdr}); err != nil {
		t.Fatal(err)
	}
	hdr.Set("X-A", "mutated") // the Server keeps its own copy
	if got := get("/a"); got != "route a" {
		t.Fatalf("/a = %q", got)
	}
	if got := get("/b"); got != "default" {
		t.Fatalf("/b = %q", got)
	}
	if err := s.SetBehavior(Behavior{Body: []byte("new default")}); err != nil {
		t.Fatal(err)
	}
	if got := get("/b"); got != "new default" {
		t.Fatalf("/b after SetBehavior = %q", got)
	}
	if got := get("/a"); got != "route a" {
		t.Fatalf("/a after SetBehavior = %q", got)
	}
	s.DeleteRoute("/a")
	if got := get("/a"); got != "new default" {
		t.Fatalf("/a after DeleteRoute = %q", got)
	}
	b := s.Behavior()
	b.Body[0] = 'X'
	if got := get("/"); got != "new default" {
		t.Fatalf("Behavior() is not a copy: %q", got)
	}
	if err := s.SetRoute("/bad", Behavior{Status: 700}); err == nil {
		t.Fatal("SetRoute accepted status 700")
	}
	if err := s.SetBehavior(Behavior{BodySize: -1}); err == nil {
		t.Fatal("SetBehavior accepted a negative size")
	}
}

func TestRequestLog(t *testing.T) { // 11 req 39: the mock's request log is scanned for stripped credentials
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
		if r.Seq != uint64(n+1) || r.Path != "/r"+strconv.Itoa(n) || r.RawQuery != "q=1" || r.URI != "/r"+strconv.Itoa(n)+"?q=1" {
			t.Errorf("entry %d: %+v", i, r)
		}
		if string(r.Body) != "body" || r.BodyBytes != 6 || !r.BodyTruncated || r.ContentLength != 6 {
			t.Errorf("entry %d: body %q of %d (truncated %v, Content-Length %d)", i, r.Body, r.BodyBytes, r.BodyTruncated, r.ContentLength)
		}
		if r.Header.Get("Authorization") != "Bearer t"+strconv.Itoa(n) || r.Method != http.MethodPost || r.Proto != "HTTP/2.0" {
			t.Errorf("entry %d: %s %s %v", i, r.Method, r.Proto, r.Header)
		}
		if !r.TLS || r.ALPN != "h2" || r.ServerName != "upstream.test" || r.Time.IsZero() || r.RemoteAddr == "" || r.Host == "" {
			t.Errorf("entry %d: TLS %v ALPN %q SNI %q", i, r.TLS, r.ALPN, r.ServerName)
		}
	}
	if st := s.Stats(); st.LogEvicted != 2 {
		t.Fatalf("LogEvicted = %d, want 2 (5 requests in a ring of 3)", st.LogEvicted)
	}
	last, found := s.LastRequest()
	if !found || last.Seq != 5 {
		t.Fatalf("LastRequest = %+v %v", last, found)
	}
	if got := s.FindRequests(func(r Request) bool { return r.Path == "/r3" }); len(got) != 1 || got[0].Seq != 4 {
		t.Fatalf("FindRequests = %+v", got)
	}
	s.ClearRequests()
	if len(s.Requests()) != 0 {
		t.Fatal("ClearRequests kept entries")
	}
	if _, found := s.LastRequest(); found {
		t.Fatal("LastRequest after ClearRequests found one")
	}
	r, err := do(t, c, http.MethodGet, s.URL()+"/after", "", nil)
	ok(t, r, err, 200)
	if got := s.Requests(); len(got) != 1 || got[0].Seq != 6 {
		t.Fatalf("after clear: %+v", got)
	}
	if st := s.Stats(); st.Requests != 6 || st.Active != 0 || st.LogEvicted != 2 {
		t.Fatalf("Stats = %+v", st)
	}
	if got := s.Requests(); got[0].BodyTruncated || got[0].BodyBytes != 0 {
		t.Fatalf("empty body marked truncated: %+v", got[0])
	}
}

// TestRequestFraming covers the framing fields of the log and Echo:
// net/http moves Transfer-Encoding out of the header, so the log keeps
// it apart (04 req 20; 11 C 21 forwarding framing at the mock).
func TestRequestFraming(t *testing.T) {
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
			r, found := s.LastRequest()
			if !found {
				t.Fatal("request not logged")
			}
			for _, got := range []struct {
				src   string
				n     int64
				te    []string
				close bool
			}{
				{"log", r.ContentLength, r.TransferEncoding, r.Close},
				{"echo", e.ContentLength, e.TransferEncoding, e.Close},
			} {
				if got.n != tc.wantLen || !slices.Equal(got.te, tc.wantTE) || got.close != tc.close {
					t.Errorf("%s: ContentLength %d TransferEncoding %q Close %v, want %d %q %v",
						got.src, got.n, got.te, got.close, tc.wantLen, tc.wantTE, tc.close)
				}
			}
			if r.BodyBytes != tc.wantBody || r.BodyTruncated || r.Header.Get("Transfer-Encoding") != "" {
				t.Errorf("log body %d bytes truncated %v header %v", r.BodyBytes, r.BodyTruncated, r.Header)
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
	if st := s.Stats(); st.Requests != 3 || st.Connections != 1 || st.OpenConnections != 1 {
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
	if err := s.SetBehavior(Behavior{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("SetBehavior after Close = %v, want ErrClosed", err)
	}
	if err := s.SetRoute("/x", Behavior{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("SetRoute after Close = %v, want ErrClosed", err)
	}
	if len(s.Requests()) != 1 {
		t.Fatal("the request log is not readable after Close")
	}
}

func TestProtocols(t *testing.T) {
	pki := newTestPKI(t)
	// DisableHTTP2 over TLS negotiates http/1.1; over cleartext, h2c
	// prior knowledge fails.
	s := start(t, Config{TLS: pki.serverTLS(), DisableHTTP2: true, Behavior: Behavior{Echo: true}})
	pr := new(http.Protocols)
	pr.SetHTTP1(true)
	pr.SetHTTP2(true)
	tr := &http.Transport{Protocols: pr, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pki.pool, ServerName: "upstream.test"}}
	defer tr.CloseIdleConnections()
	r, err := do(t, &http.Client{Transport: tr}, http.MethodGet, s.URL()+"/", "", nil)
	ok(t, r, err, 200)
	if r.resp.Proto != "HTTP/1.1" {
		t.Fatalf("proto %q, want HTTP/1.1", r.resp.Proto)
	}
	s2 := start(t, Config{DisableHTTP2: true})
	if _, err := do(t, client(t, h2c, pki), http.MethodGet, s2.URL()+"/", "", nil); err == nil {
		t.Fatal("h2c succeeded with DisableHTTP2")
	}
	// Both HTTP/1.1 and h2c serve on one cleartext port.
	s3 := start(t, Config{MaxConcurrentStreams: 10})
	for _, p := range []proto{h1, h2c} {
		r, err := do(t, client(t, p, pki), http.MethodGet, s3.URL()+"/", "", nil)
		ok(t, r, err, 200)
		if r.resp.Proto != p.wantProto() {
			t.Fatalf("%s: proto %q", p, r.resp.Proto)
		}
	}
	if s3.Name() != "" || !strings.HasPrefix(s3.URL(), "http://127.0.0.1:") || strings.TrimPrefix(s3.URL(), "http://") != s3.Addr() {
		t.Fatalf("Name %q URL %q Addr %q", s3.Name(), s3.URL(), s3.Addr())
	}
}

func TestIdleTimeout(t *testing.T) {
	s := start(t, Config{IdleTimeout: 50 * time.Millisecond})
	c := client(t, h1, testPKI{})
	r, err := do(t, c, http.MethodGet, s.URL()+"/", "", nil)
	ok(t, r, err, 200)
	waitFor(t, func() bool { return s.Stats().OpenConnections == 0 })
	if st := s.Stats(); st.Connections != 1 {
		t.Fatalf("Stats = %+v", st)
	}
}

func TestCloseEndsDelayedRequests(t *testing.T) {
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, Config{Behavior: Behavior{Delay: Fixed(time.Hour)}})
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
	waitFor(t, func() bool { return s.Stats().Active == 0 })
	if err := s.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	if d := time.Since(began); d > 5*time.Second {
		t.Fatalf("close took %v", d)
	}
	c.CloseIdleConnections()
	waitFor(t, func() bool { return runtime.NumGoroutine() <= base })
}

func TestCloseEndsSlowBody(t *testing.T) {
	s := start(t, Config{Behavior: Behavior{BodySize: 1 << 20, ChunkSize: 1, ChunkInterval: time.Hour}})
	c := client(t, h1, testPKI{})
	done := make(chan result, 1)
	go func() {
		r, _ := do(t, c, http.MethodGet, s.URL()+"/", "", nil)
		done <- r
	}()
	waitFor(t, func() bool { return s.Stats().Active == 1 })
	_ = s.Close()
	r := <-done
	if r.resp != nil && r.bodyErr == nil {
		t.Fatalf("slow body completed with %d bytes", len(r.body))
	}
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
		{"size", Config{Behavior: Behavior{BodySize: -1}}, "negative"},
		{"chunk", Config{Behavior: Behavior{ChunkInterval: -time.Second}}, "negative"},
		{"reset", Config{Behavior: Behavior{Reset: ResetKind(9)}}, "unknown reset kind ResetKind(9)"},
		{"fixed", Config{Behavior: Behavior{Delay: Fixed(-1)}}, "negative delay"},
		{"uniform", Config{Behavior: Behavior{Delay: Uniform{Min: 2, Max: 1}}}, "invalid delay"},
		{"normal", Config{Behavior: Behavior{Delay: Normal{StdDev: -1}}}, "negative delay"},
		{"exponential", Config{Behavior: Behavior{Delay: Exponential{Mean: -1}}}, "negative delay"},
		{"streams", Config{MaxConcurrentStreams: -1}, "MaxConcurrentStreams"},
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

func TestGeneratedBody(t *testing.T) {
	g := GeneratedBody(patternSize + 100)
	if !bytes.Equal(g[:36], []byte(patternAlphabet)) || !bytes.Equal(g[patternSize:patternSize+36], []byte(patternAlphabet)) {
		t.Fatal("pattern does not repeat")
	}
	s := &Server{pattern: GeneratedBody(patternSize)}
	var got []byte
	for off := int64(0); off < int64(len(g)); {
		p := s.piece(nil, off, int64(len(g))-off)
		got = append(got, p...)
		off += int64(len(p))
	}
	if !bytes.Equal(got, g) {
		t.Fatal("piece does not reproduce GeneratedBody across the wrap")
	}
}

// BenchmarkS1 measures the mock's cost for PBB S1 (GET, 1 KiB response,
// keep-alive) with the request log disabled; the PBB validity rule wants
// the mock's p99 beyond its delay at 200 µs or less.
func BenchmarkS1(b *testing.B) {
	s := start(b, Config{LogSize: -1, Behavior: Behavior{BodySize: 1024}})
	c := client(b, h1, testPKI{})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, s.URL()+"/", nil)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		resp, err := c.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// BenchmarkServeHTTP measures the handler alone (no network).
func BenchmarkServeHTTP(b *testing.B) {
	s := start(b, Config{LogSize: -1, Behavior: Behavior{BodySize: 1024}})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	if err != nil {
		b.Fatal(err)
	}
	w := &discardWriter{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		s.ServeHTTP(w, req)
	}
}

type discardWriter struct{ h http.Header }

func (w *discardWriter) Header() http.Header         { return w.h }
func (w *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardWriter) WriteHeader(int)             {}
