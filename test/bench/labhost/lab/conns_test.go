// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
)

// Tests for the conns service: each connection kind against a loopback
// server that answers, stalls or closes the way a Node does.

// waitConns polls the service's counters until cond holds.
func waitConns(t *testing.T, c *Conns, what string, cond func(*ConnsStats) bool) *ConnsStats {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := c.stats()
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %+v", what, st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startConns starts c, closed when the test ends.
func startConns(t *testing.T, c ConnsConfig) *Conns {
	t.Helper()
	s, err := StartConns(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// serve runs srv on a loopback listener, through TLS when tlsOn, and
// returns its address; the test's end closes it.
func serve(t *testing.T, srv *http.Server, tlsOn bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		if tlsOn {
			_ = srv.ServeTLS(ln, "", "")
		} else {
			_ = srv.Serve(ln)
		}
	})
	t.Cleanup(func() {
		_ = srv.Close()
		wg.Wait()
	})
	return ln.Addr().String()
}

// histogramCount decodes an encoded histogram's count.
func histogramCount(t *testing.T, enc string) uint64 {
	t.Helper()
	h, err := DecodeHistogram(enc)
	if err != nil {
		t.Fatal(err)
	}
	return h.Count()
}

// TestConnsIdleHTTP1 checks C1's cleartext kind: each connection sends one
// GET, reads an answer that keeps the connection open, a refusal such as
// a Policy's 401 included, and idles until the server closes it, which
// counts as closed by the peer with its close time; an answer that closes
// the connection fails the setup.
func TestConnsIdleHTTP1(t *testing.T) {
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/close":
			w.Header().Set("Connection", "close")
		case "/denied":
			w.WriteHeader(http.StatusUnauthorized)
		}
		_, _ = w.Write([]byte("ok"))
	}), ReadHeaderTimeout: 10 * time.Second}
	addr := serve(t, srv, false)

	c := startConns(t, ConnsConfig{Kind: ConnsIdleHTTP1, Target: addr, Count: 4, Path: "/denied"})
	st := waitConns(t, c, "every setup", func(s *ConnsStats) bool { return s.Done })
	if st.Attempted != 4 || st.Opened != 4 || st.Open != 4 || st.Failed != 0 {
		t.Fatalf("after setup = %+v, want 4 attempted, opened and open", st)
	}
	if n := histogramCount(t, st.Setup); n != 4 {
		t.Errorf("setup histogram holds %d, want 4", n)
	}
	_ = srv.Close()
	st = waitConns(t, c, "the server's closes", func(s *ConnsStats) bool { return s.ClosedByPeer == 4 && s.Open == 0 })
	if n := histogramCount(t, st.ClosedAfter); n != 4 {
		t.Errorf("close histogram holds %d, want 4", n)
	}

	closing := &http.Server{Handler: srv.Handler, ReadHeaderTimeout: 10 * time.Second}
	addr = serve(t, closing, false)
	c = startConns(t, ConnsConfig{Kind: ConnsIdleHTTP1, Target: addr, Count: 2, Path: "/close"})
	st = waitConns(t, c, "every setup", func(s *ConnsStats) bool { return s.Done })
	if st.Failed != 2 || st.Opened != 0 || len(st.Errors) != 2 || !strings.Contains(st.Errors[0], "closes the connection") {
		t.Errorf("closing answers = %+v, want 2 failed setups", st)
	}
}

// TestConnsIdleH2 checks C1's TLS kind and C2's burst: the TLS handshake
// with ALPN h2, the preface and SETTINGS exchange and an idle hold until
// the server closes; a handshake against the wrong name fails the setup.
func TestConnsIdleH2(t *testing.T) {
	ca, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Server("localhost")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := leaf.ServerTLS(nil)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, ca.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler(), TLSConfig: cfg, ReadHeaderTimeout: 10 * time.Second}
	addr := serve(t, srv, true)

	c := startConns(t, ConnsConfig{Kind: ConnsIdleH2, Target: addr, Count: 3, Rate: 1000, CAFile: caFile, ServerName: "localhost"})
	st := waitConns(t, c, "every setup", func(s *ConnsStats) bool { return s.Done })
	if st.Opened != 3 || st.Open != 3 || st.Failed != 0 {
		t.Fatalf("after setup = %+v, want 3 opened and open", st)
	}
	_ = srv.Close()
	waitConns(t, c, "the server's closes", func(s *ConnsStats) bool { return s.ClosedByPeer == 3 && s.Open == 0 })

	// The refused handshake is the point; the server need not log it.
	other := &http.Server{Handler: http.NotFoundHandler(), TLSConfig: cfg.Clone(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)}
	addr = serve(t, other, true)
	c = startConns(t, ConnsConfig{Kind: ConnsIdleH2, Target: addr, Count: 1, CAFile: caFile, ServerName: "bench.example"})
	st = waitConns(t, c, "the setup", func(s *ConnsStats) bool { return s.Done })
	if st.Failed != 1 || len(st.Errors) != 1 || !strings.Contains(st.Errors[0], "handshake") {
		t.Errorf("wrong server name = %+v, want a failed handshake", st)
	}
}

// TestConnsStallH2C checks the pre-routing writes' clients: h2c by prior
// knowledge with a zero initial window and one request, whose response
// HEADERS count as responded while its body never can be sent; a reset
// after the HEADERS counts as expired with its time, one before them as
// aborted.
func TestConnsStallH2C(t *testing.T) {
	release := make(chan struct{})
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Protocols: &p, ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/abort" {
			panic(http.ErrAbortHandler)
		}
		w.WriteHeader(http.StatusNotFound)
		http.NewResponseController(w).Flush() //nolint:errcheck // the HEADERS go out or the stream is gone
		select {
		case <-release:
		case <-r.Context().Done():
		}
		panic(http.ErrAbortHandler)
	})}
	addr := serve(t, srv, false)

	c := startConns(t, ConnsConfig{Kind: ConnsStallH2C, Target: addr, Count: 2, Path: "/bench/1k"})
	waitConns(t, c, "the response HEADERS", func(s *ConnsStats) bool { return s.Done && s.Responded == 2 })
	close(release)
	st := waitConns(t, c, "the resets", func(s *ConnsStats) bool { return s.Expired == 2 })
	if st.Aborted != 0 || st.Opened != 2 {
		t.Errorf("after the resets = %+v, want 2 opened, none aborted", st)
	}
	if n := histogramCount(t, st.ExpiredAfter); n != 2 {
		t.Errorf("expiry histogram holds %d, want 2", n)
	}

	c = startConns(t, ConnsConfig{Kind: ConnsStallH2C, Target: addr, Count: 1, Path: "/abort"})
	st = waitConns(t, c, "the reset", func(s *ConnsStats) bool { return s.Aborted == 1 })
	if st.Responded != 0 || st.Expired != 0 {
		t.Errorf("a reset before HEADERS = %+v, want only aborted", st)
	}
}

// rawServer accepts connections and reads each until done reports the
// bytes received so far complete, then closes it, as a Node closes a
// connection whose header or body deadline passed.
func rawServer(t *testing.T, done func([]byte) bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer func() { _ = c.Close() }()
				var got []byte
				buf := make([]byte, 4096)
				for !done(got) {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					got = append(got, buf[:n]...)
				}
			})
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return ln.Addr().String()
}

// TestConnsSlowHeader checks the slow-header flood's connections: a
// request line and one header field of HeaderBytes with no end of the
// header block; the server's close counts as closed by the peer, with the
// time from the connection's start.
func TestConnsSlowHeader(t *testing.T) {
	const size = 8 << 10
	header := rawServer(t, func(b []byte) bool {
		head, v, ok := bytes.Cut(b, []byte("\r\nX-Flood: "))
		return ok && bytes.HasPrefix(head, []byte("GET /bench/1k HTTP/1.1\r\n")) && len(v) >= size && !bytes.Contains(b, []byte("\r\n\r\n"))
	})
	c := startConns(t, ConnsConfig{Kind: ConnsSlowHeader, Target: header, Count: 3, Path: "/bench/1k", HeaderBytes: size})
	st := waitConns(t, c, "the closes", func(s *ConnsStats) bool { return s.Done && s.ClosedByPeer == 3 })
	if st.Opened != 3 || st.Failed != 0 || histogramCount(t, st.ClosedAfter) != 3 {
		t.Errorf("slow-header = %+v, want 3 opened and closed by the peer", st)
	}
}

// TestStartConnsRefuses covers the configurations StartConns rejects.
func TestStartConnsRefuses(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  ConnsConfig
	}{
		{"unknown kind", ConnsConfig{Kind: "idle-h3", Target: "127.0.0.1:1", Count: 1}},
		{"no target", ConnsConfig{Kind: ConnsIdleHTTP1, Count: 1}},
		{"no count", ConnsConfig{Kind: ConnsIdleHTTP1, Target: "127.0.0.1:1"}},
		{"negative rate", ConnsConfig{Kind: ConnsIdleHTTP1, Target: "127.0.0.1:1", Count: 1, Rate: -1}},
		{"slow-header without a size", ConnsConfig{Kind: ConnsSlowHeader, Target: "127.0.0.1:1", Count: 1}},
		{"idle-h2 without a CA", ConnsConfig{Kind: ConnsIdleH2, Target: "127.0.0.1:1", Count: 1, CAFile: filepath.Join(t.TempDir(), "none.pem")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if s, err := StartConns(t.Context(), c.cfg); err == nil {
				_ = s.Close()
				t.Errorf("StartConns(%+v) succeeded", c.cfg)
			}
		})
	}
}

// TestConnsArgsRoundTrip checks that the labhost conns command parses the
// flags ConnsArgs writes: the service starts on them, opens its
// connection and names its target in the ready line.
func TestConnsArgsRoundTrip(t *testing.T) {
	addr := rawServer(t, func(b []byte) bool { return bytes.Contains(b, []byte("X-Flood: aaaa")) })
	args := ConnsArgs(ConnsConfig{
		Kind: ConnsSlowHeader, Target: addr, Count: 1, Rate: 2.5, Host: "bench.example", Path: "/bench/1k",
		HeaderBytes: 4, Timeout: 5 * time.Second,
	})
	for _, want := range []string{"-kind", "-target", "-count", "-rate", "-host", "-path", "-header-bytes", "-timeout"} {
		if !strings.Contains(strings.Join(args, " "), want) {
			t.Errorf("ConnsArgs = %v, missing %s", args, want)
		}
	}
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if code := Main(ctx, append([]string{"conns"}, args...), strings.NewReader(""), &out, &errb); code != ExitOK {
		t.Fatalf("conns command = %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), `"service":"conns"`) || !strings.Contains(out.String(), addr) {
		t.Errorf("conns session output = %q, want the ready line naming %s", out.String(), addr)
	}
}
