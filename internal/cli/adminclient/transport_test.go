// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// Tests for the transport rules of docs/reference/01-cli-and-api-surface.md
// "Admin API conventions" and "Admin API": a token never goes in cleartext
// beyond loopback, tested on the connected peer before a byte is sent;
// direct connections only, proxy variables ignored; no redirect followed;
// no call retried.

// TestTransportSettings checks the transport the rules rest on.
func TestTransportSettings(t *testing.T) {
	c, _ := newTestClient(t, Config{URL: "https://127.0.0.1:9901"})
	tr := c.transport
	if tr.Proxy != nil || !tr.DisableKeepAlives || !tr.DisableCompression || tr.DialContext == nil || tr.DialTLSContext == nil {
		t.Errorf("transport = %+v", tr)
	}
	if p := tr.Protocols; p == nil || !p.HTTP1() || p.HTTP2() || p.UnencryptedHTTP2() {
		t.Errorf("transport protocols = %v, want HTTP/1.1 only", p)
	}
}

// TestCleartextTokenRefusedBeyondLoopback checks that a token over http to
// a peer that is not a loopback address is refused, exit 2, before any
// byte is written, and the connection closed.
func TestCleartextTokenRefusedBeyondLoopback(t *testing.T) {
	for _, peer := range []string{"192.0.2.10:9901", "[2001:db8::7]:9901", "[fe80::1%eth0]:9901", "[::ffff:192.0.2.10]:9901"} {
		t.Run(peer, func(t *testing.T) {
			c, _ := newTestClient(t, Config{URL: "http://node-a.shop.example:9901", TokenFile: tokenFile(t, testToken), Timeout: DefaultTimeout})
			client, server := net.Pipe()
			conn := &peerConn{Conn: client, remote: tcpAddr(peer)}
			read := make(chan int64, 1)
			go func() {
				n, _ := io.Copy(io.Discard, server)
				read <- n
			}()
			c.dial = func(context.Context, string, string) (net.Conn, error) { return conn, nil }
			_, err := c.ConfigDump(t.Context())
			if !errors.Is(err, ErrCleartext) || command.Code(t.Context(), err) != command.ExitNoResult {
				t.Fatalf("ConfigDump = %v, want ErrCleartext, exit 2", err)
			}
			if want := "to " + conn.remote.String() + ", which is not a loopback address; use https, or drop --admin-token-file"; !strings.Contains(err.Error(), want) {
				t.Errorf("ConfigDump = %q, want it to contain %q", err, want)
			}
			if n := <-read; n != 0 || conn.wrote.Load() != 0 {
				t.Errorf("%d bytes reached the peer (%d written), want none", n, conn.wrote.Load())
			}
			if strings.Contains(err.Error(), testToken) {
				t.Error("the error repeats the token")
			}
		})
	}
}

// TestCleartextTokenNamesItsSource checks that the refusal names the
// variable the token came from.
func TestCleartextTokenNamesItsSource(t *testing.T) {
	c, _ := newTestClient(t, Config{
		URL: "http://node-a.shop.example:9901", TokenFile: tokenFile(t, testToken),
		Names: Names{TokenFile: EnvTokenFile},
	})
	c.dial = func(context.Context, string, string) (net.Conn, error) {
		client, _ := net.Pipe()
		return &peerConn{Conn: client, remote: tcpAddr("192.0.2.10:9901")}, nil
	}
	if _, err := c.Readyz(t.Context()); err == nil || !strings.HasSuffix(err.Error(), "or drop RURALZ_ADMIN_TOKEN_FILE") {
		t.Errorf("Readyz = %v", err)
	}
}

// TestCleartextTokenToLoopback checks the loopback peers the Node's rule
// admits, whatever name the URL gives: 127.0.0.0/8, ::1 and IPv4-mapped
// loopback addresses, so localhost works.
func TestCleartextTokenToLoopback(t *testing.T) {
	var rec recorder
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, _ = io.WriteString(w, `{"digest":"sha256:00"}`)
	})
	for _, peer := range []string{"127.0.0.1:9901", "127.3.2.1:9901", "[::1]:9901", "[::ffff:127.0.0.1]:9901"} {
		for _, host := range []string{"localhost", "node-a.shop.example"} {
			c, _ := newTestClient(t, Config{URL: "http://" + host + ":9901", TokenFile: tokenFile(t, testToken), Timeout: DefaultTimeout})
			d := &dialer{target: srv.Listener.Addr().String(), remote: peer}
			c.dial = d.dial
			if body, err := c.ConfigDump(t.Context()); err != nil || string(body) != `{"digest":"sha256:00"}` {
				t.Errorf("%s via %s: ConfigDump = %q, %v", host, peer, body, err)
			}
		}
	}
	for _, r := range rec.all() {
		if r.header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("Authorization = %q", r.header.Get("Authorization"))
		}
	}
	if len(rec.all()) != 8 {
		t.Errorf("%d requests, want 8", len(rec.all()))
	}
}

// TestNoTokenNoPeerTest checks that without a token an http call goes to
// any peer: there is nothing to protect.
func TestNoTokenNoPeerTest(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ready","revision":"rev-162af81f5de4"}`)
	})
	c, _ := newTestClient(t, Config{URL: "http://node-a.shop.example:9901", Timeout: DefaultTimeout})
	c.dial = (&dialer{target: srv.Listener.Addr().String(), remote: "192.0.2.10:9901"}).dial
	if r, err := c.Readyz(t.Context()); err != nil || !r.Ready {
		t.Errorf("Readyz = %+v, %v", r, err)
	}
}

// TestTokenOverTLSToAnyPeer checks that over https a token goes to a peer
// that is not a loopback address.
func TestTokenOverTLSToAnyPeer(t *testing.T) {
	p := newPKI(t)
	var rec recorder
	srv := serveTLS(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, _ = io.WriteString(w, "{}")
	}))
	c, _ := newTestClient(t, Config{URL: "https://localhost:9901", TokenFile: tokenFile(t, testToken), CAFile: p.caFile, Timeout: DefaultTimeout})
	c.dial = (&dialer{target: srv.Listener.Addr().String(), remote: "192.0.2.10:9901"}).dial
	if _, err := c.ConfigDump(t.Context()); err != nil {
		t.Fatalf("ConfigDump = %v", err)
	}
	if got := rec.all(); len(got) != 1 || got[0].header.Get("Authorization") != "Bearer "+testToken {
		t.Errorf("requests = %+v", got)
	}
}

// TestProxyVariablesIgnored checks that the client dials the Node itself
// while proxy variables name a proxy.
func TestProxyVariablesIgnored(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{}") })
	for _, v := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY"} {
		t.Setenv(v, "http://192.0.2.99:3128")
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	c, _ := newTestClient(t, Config{URL: "http://node-a.shop.example:9901", Timeout: DefaultTimeout})
	d := &dialer{target: srv.Listener.Addr().String(), remote: "127.0.0.1:9901"}
	c.dial = d.dial
	if _, err := c.ConfigDump(t.Context()); err != nil {
		t.Fatalf("ConfigDump = %v", err)
	}
	if got := d.asked(); len(got) != 1 || got[0] != "node-a.shop.example:9901" {
		t.Errorf("dialed %q, want the Node only", got)
	}
}

// TestRedirectNotFollowed checks that a 3xx exits 2 with its Location and
// is never followed.
func TestRedirectNotFollowed(t *testing.T) {
	var requests atomic.Int32
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/elsewhere" {
			_, _ = io.WriteString(w, "{}")
			return
		}
		if r.URL.Path == PathReadyz {
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	c, _ := newTestClient(t, Config{URL: srv.URL, Timeout: DefaultTimeout})
	_, err := c.ConfigDump(t.Context())
	re, ok := errors.AsType[*RedirectError](err)
	if !ok || command.Code(t.Context(), err) != command.ExitNoResult {
		t.Fatalf("ConfigDump = %v, want a RedirectError, exit 2", err)
	}
	want := "GET " + srv.URL + "/config/dump: 302 Found with Location /elsewhere; redirects are not followed"
	if re.Status != http.StatusFound || re.Location != "/elsewhere" || err.Error() != want {
		t.Errorf("ConfigDump = %q, want %q", err, want)
	}
	_, err = c.Readyz(t.Context())
	if want := "GET " + srv.URL + "/readyz: 301 Moved Permanently without a Location; redirects are not followed"; err == nil || err.Error() != want {
		t.Errorf("Readyz = %v, want %q", err, want)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("the server saw %d requests, want 2", n)
	}
}

// TestNoRetry checks that a connection the server closes without an answer
// fails the call after one request: nothing retries it.
func TestNoRetry(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			// Read the request, then hang up without an answer.
			_, _ = http.ReadRequest(bufio.NewReader(conn))
			_ = conn.Close()
		}
	}()
	c, _ := newTestClient(t, Config{URL: "http://" + ln.Addr().String(), Timeout: DefaultTimeout})
	_, err = c.ConfigDump(t.Context())
	if err == nil || command.Code(t.Context(), err) != command.ExitNoResult {
		t.Fatalf("ConfigDump = %v, want exit 2", err)
	}
	if n := accepted.Load(); n != 1 {
		t.Errorf("the server accepted %d connections, want 1", n)
	}
}

// TestNoCompressionRequested checks that the client asks for no encoding,
// so bodies arrive as the Node sent them.
func TestNoCompressionRequested(t *testing.T) {
	var rec recorder
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, _ = io.WriteString(w, "{}")
	})
	c, _ := newTestClient(t, Config{URL: srv.URL, Timeout: DefaultTimeout})
	if _, err := c.ConfigDump(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := rec.all(); len(got) != 1 || got[0].header.Get("Accept-Encoding") != "" {
		t.Errorf("requests = %+v, want no Accept-Encoding", got)
	}
}
