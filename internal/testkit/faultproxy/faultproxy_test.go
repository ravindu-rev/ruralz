// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package faultproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Tests for each mode on live and new connections (black-hole stalls
// without closing; reset yields ECONNRESET at the peer; upstream switch),
// the connection hooks, MaxConns overflow resets and goroutines returning
// to baseline after Close, verified against a TCP echo server.

// echo is a TCP echo server; with upper set it echoes upper case, so tests
// can tell two upstreams apart.
type echo struct {
	ln       net.Listener
	upper    bool
	accepted atomic.Int64
	wg       sync.WaitGroup
	mu       sync.Mutex
	conns    []net.Conn
}

func startEcho(t *testing.T, upper bool) *echo {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &echo{ln: ln, upper: upper}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			e.accepted.Add(1)
			e.mu.Lock()
			e.conns = append(e.conns, c)
			e.mu.Unlock()
			e.wg.Add(1)
			go func() {
				defer e.wg.Done()
				defer func() { _ = c.Close() }()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						out := buf[:n]
						if e.upper {
							out = bytes.ToUpper(out)
						}
						if _, werr := c.Write(out); werr != nil {
							return
						}
					}
					if err != nil {
						if errors.Is(err, io.EOF) {
							_ = c.(*net.TCPConn).CloseWrite()
						}
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(e.close)
	return e
}

func (e *echo) addr() string { return e.ln.Addr().String() }

func (e *echo) close() {
	_ = e.ln.Close()
	e.mu.Lock()
	for _, c := range e.conns {
		_ = c.Close()
	}
	e.mu.Unlock()
	e.wg.Wait()
}

func start(t *testing.T, c Config) *Proxy {
	t.Helper()
	p, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func dial(t *testing.T, addr string) *net.TCPConn {
	t.Helper()
	c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*net.TCPConn)
}

// roundTrip writes msg and reads len(msg) bytes back within timeout.
func roundTrip(c net.Conn, msg string, timeout time.Duration) (string, time.Duration, error) {
	begin := time.Now()
	_ = c.SetDeadline(begin.Add(timeout))
	defer func() { _ = c.SetDeadline(time.Time{}) }()
	if _, err := c.Write([]byte(msg)); err != nil {
		return "", 0, err
	}
	buf := make([]byte, len(msg))
	_, err := io.ReadFull(c, buf)
	return string(buf), time.Since(begin), err
}

func mustEcho(t *testing.T, c net.Conn, msg, want string) time.Duration {
	t.Helper()
	got, d, err := roundTrip(c, msg, 10*time.Second)
	if err != nil || got != want {
		t.Fatalf("round trip %q = %q, %v; want %q", msg, got, err, want)
	}
	return d
}

// readErr reads once with a timeout and returns the error.
func readErr(c net.Conn, timeout time.Duration) error {
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	_, err := c.Read(make([]byte, 16))
	return err
}

func isReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// expectReset dials addr and expects the proxy to reset the connection at
// once. The client sees the RST on its first read or, when the RST beats
// the dialer's check of the finished handshake, as the dial error.
func expectReset(t *testing.T, addr, what string) {
	t.Helper()
	c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		if !isReset(err) {
			t.Fatalf("%s: dial %v, want ECONNRESET", what, err)
		}
		return
	}
	defer func() { _ = c.Close() }()
	if err := readErr(c, 5*time.Second); !isReset(err) {
		t.Fatalf("%s: %v, want ECONNRESET", what, err)
	}
}

// TestPassAndTaps checks Pass, the counters and the connection hooks:
// ConnTap runs once per connection with its ID, numbered from 1 in accept
// order as Conns lists them; the connection's Tap sees every byte it
// forwards, in each direction, before the peer can read it; and Stats
// counts the bytes forwarded each way.
func TestPassAndTaps(t *testing.T) {
	e := startEcho(t, false)
	var mu sync.Mutex
	tapped := map[uint64]map[Direction]*bytes.Buffer{}
	p := start(t, Config{
		Upstream: e.addr(),
		ConnTap: func(ci ConnInfo) ConnHooks {
			mu.Lock()
			defer mu.Unlock()
			if tapped[ci.ID] != nil {
				t.Errorf("ConnTap ran twice for connection %d", ci.ID)
			}
			tapped[ci.ID] = map[Direction]*bytes.Buffer{Up: {}, Down: {}}
			return ConnHooks{Tap: func(d Direction, b []byte) {
				mu.Lock()
				_, _ = tapped[ci.ID][d].Write(b)
				mu.Unlock()
			}}
		},
	})
	if p.Upstream() != e.addr() || !strings.HasPrefix(p.Addr(), "127.0.0.1:") {
		t.Fatalf("upstream %s addr %s", p.Upstream(), p.Addr())
	}
	first, second := dial(t, p.Addr()), dial(t, p.Addr())
	mustEcho(t, first, "hello", "hello")
	mustEcho(t, first, "world", "world")
	mustEcho(t, second, "again", "again")
	// The byte counters move after each write completes, so a client can
	// read its echo before BytesDown counts it.
	waitFor(t, "byte counters", func() bool { s := p.Stats(); return s.BytesUp == 15 && s.BytesDown == 15 })
	if st := p.Stats(); st.Accepted != 2 || st.Active != 2 {
		t.Fatalf("Stats = %+v", st)
	}
	if conns := p.Conns(); len(conns) != 2 || conns[0].ID != 1 || conns[1].ID != 2 {
		t.Fatalf("Conns = %+v, want connections 1 and 2", conns)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tapped) != 2 || tapped[1] == nil || tapped[2] == nil {
		t.Fatalf("ConnTap ran for connections %v, want 1 and 2", tapped)
	}
	for id, want := range map[uint64]string{1: "helloworld", 2: "again"} {
		if up, down := tapped[id][Up].String(), tapped[id][Down].String(); up != want || down != want {
			t.Errorf("connection %d tapped up %q, down %q; want %q each way", id, up, down, want)
		}
	}
}

// holdUpper is a ConnTap whose Hold holds every one-byte chunk of dir that
// is an upper-case letter, keeping its release in releases, and records
// the chunks Hold saw and the chunks the Tap saw forwarded.
type holdUpper struct {
	dir Direction

	mu       sync.Mutex
	seen     []byte
	tapped   []byte
	releases map[byte]func()
}

func (h *holdUpper) hooks(ConnInfo) ConnHooks {
	return ConnHooks{
		Tap: func(d Direction, b []byte) {
			if d == h.dir {
				h.mu.Lock()
				h.tapped = append(h.tapped, b...)
				h.mu.Unlock()
			}
		},
		Hold: func(d Direction, b []byte, release func()) bool {
			if d != h.dir {
				return false
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			h.seen = append(h.seen, b...)
			if len(b) != 1 || b[0] < 'A' || b[0] > 'Z' {
				return false
			}
			if h.releases == nil {
				h.releases = map[byte]func(){}
			}
			h.releases[b[0]] = release
			return true
		},
	}
}

// saw reports whether Hold saw s last.
func (h *holdUpper) saw(s string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return bytes.HasSuffix(h.seen, []byte(s))
}

// forwarded returns the chunks the Tap saw.
func (h *holdUpper) forwarded() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return string(h.tapped)
}

// release calls the release of the held chunk k.
func (h *holdUpper) release(t *testing.T, k byte) {
	t.Helper()
	h.mu.Lock()
	r := h.releases[k]
	h.mu.Unlock()
	if r == nil {
		t.Fatalf("no held chunk %q", k)
	}
	r()
}

// TestHoldKeepsReadingAndOrder checks a connection's Hold: the pump goes on
// reading while a chunk is held, so Hold sees the chunks behind it at once
// and holds of its own run beside the first; every chunk behind a held one,
// held or not, waits for it, so the chunks reach the peer in the order
// read whatever the order of the releases; a second release, or one after
// the connection ended, does nothing.
func TestHoldKeepsReadingAndOrder(t *testing.T) {
	e := startEcho(t, false)
	h := &holdUpper{dir: Down}
	p := start(t, Config{Upstream: e.addr(), ConnTap: h.hooks})
	c := dial(t, p.Addr())
	send := func(s string) {
		t.Helper()
		if _, err := c.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "Hold to see "+s, func() bool { return h.saw(s) })
	}
	send("A") // held
	send("b") // read while A is held: it waits behind A
	send("C") // read only because the pump went on reading: held too
	// The pump handled b before it read C, so a b sent past the held A
	// would show by now.
	if got := h.forwarded(); got != "" {
		t.Fatalf("forwarded %q while A was held", got)
	}
	if st := p.Stats(); st.BytesDown != 0 {
		t.Fatalf("BytesDown = %d while A was held", st.BytesDown)
	}
	h.release(t, 'C') // released first, but behind A
	send("d")
	h.release(t, 'A')
	h.release(t, 'A')
	got := make([]byte, 4)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "AbCd" {
		t.Fatalf("peer read %q, %v; want AbCd, the order read", got, err)
	}
	if got := h.forwarded(); got != "AbCd" {
		t.Errorf("Tap saw %q, want AbCd", got)
	}
	send("E") // held when the proxy closes
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	h.release(t, 'E')
	h.release(t, 'C')
}

// TestHoldDroppedByBlackhole checks that Blackhole drops a held chunk, which
// its later release never forwards: after Pass, the upstream sees only what
// the client sent since.
func TestHoldDroppedByBlackhole(t *testing.T) {
	e := startEcho(t, false)
	h := &holdUpper{dir: Up}
	p := start(t, Config{Upstream: e.addr(), ConnTap: h.hooks})
	c := dial(t, p.Addr())
	if _, err := c.Write([]byte("H")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the held chunk queued", func() bool { return h.saw("H") && p.queuedUp.Load() == 1 })
	if err := p.SetMode(Blackhole{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Blackhole to drop the held chunk", func() bool { return p.queuedUp.Load() == 0 })
	h.release(t, 'H')
	if err := p.SetMode(Pass{}); err != nil {
		t.Fatal(err)
	}
	mustEcho(t, c, "x", "x")
	if got := h.forwarded(); got != "x" {
		t.Errorf("forwarded %q upstream, want only x", got)
	}
}

// TestHoldBeforeHalfClose checks that a chunk held when its source ends is
// forwarded once released, and the end follows it.
func TestHoldBeforeHalfClose(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		uc, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = uc.Close() }()
		if _, err := io.ReadAll(uc); err != nil {
			return
		}
		_, _ = uc.Write([]byte("H"))
		_ = uc.(*net.TCPConn).CloseWrite()
	}()
	h := &holdUpper{dir: Down}
	p := start(t, Config{Upstream: ln.Addr().String(), ConnTap: h.hooks})
	c := dial(t, p.Addr())
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	<-closed
	waitFor(t, "the held chunk", func() bool { return h.saw("H") })
	h.release(t, 'H')
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "H" {
		t.Fatalf("read %q, %v; want the held chunk, then the end", got, err)
	}
}

func TestHalfClose(t *testing.T) {
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr()})
	c := dial(t, p.Addr())
	if _, err := c.Write([]byte("bye")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "bye" {
		t.Fatalf("after half-close read %q, %v", got, err)
	}
	waitFor(t, "connection removed", func() bool { return p.Stats().Active == 0 })
}

// TestHoldQueueBound checks MaxQueuedBytes: behind a held chunk a
// direction queues what it reads until the queue holds MaxQueuedBytes,
// then stops reading, its last read adding at most one chunk; once the
// hold is released every byte reaches the peer in order.
func TestHoldQueueBound(t *testing.T) {
	e := startEcho(t, false)
	const bound = 16 << 10
	var (
		mu      sync.Mutex
		release func()
	)
	p := start(t, Config{
		Upstream:       e.addr(),
		MaxQueuedBytes: bound,
		ConnTap: func(ConnInfo) ConnHooks {
			return ConnHooks{Hold: func(d Direction, _ []byte, r func()) bool {
				mu.Lock()
				defer mu.Unlock()
				if d != Up || release != nil {
					return false
				}
				release = r // the first chunk up
				return true
			}}
		},
	})
	c := dial(t, p.Addr())
	payload := bytes.Repeat([]byte("0123456789abcdef"), 16<<10) // 256 KiB
	go func() {
		_, _ = c.Write(payload)
		_ = c.CloseWrite()
	}()
	waitFor(t, "the queue to fill", func() bool { return p.queuedUp.Load() >= bound })
	if q := p.queuedUp.Load(); q >= bound+chunkSize {
		t.Fatalf("queued %d bytes, want under MaxQueuedBytes plus one chunk, %d", q, bound+chunkSize)
	}
	mu.Lock()
	r := release
	mu.Unlock()
	r()
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("read %d bytes, %v; want the payload intact", len(got), err)
	}
}

func TestBlackholeWithHalfClose(t *testing.T) {
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr()})
	c := dial(t, p.Addr())
	mustEcho(t, c, "x", "x")
	if err := p.SetMode(Blackhole{}); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	// The FIN is held like the bytes.
	if err := readErr(c, 300*time.Millisecond); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read during the black hole = %v", err)
	}
	if err := p.SetMode(Pass{}); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if got, err := io.ReadAll(c); err != nil || len(got) != 0 {
		t.Fatalf("after Pass read %q, %v; want EOF", got, err)
	}
}

// TestHeldConnectionReset checks that Reset resets a connection held
// under Blackhole before its dial, which then never reaches the upstream.
func TestHeldConnectionReset(t *testing.T) {
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr(), Mode: Blackhole{}})
	c := dial(t, p.Addr())
	waitFor(t, "connection admitted", func() bool { return p.Stats().Active == 1 })
	if err := p.SetMode(Reset{}); err != nil {
		t.Fatal(err)
	}
	if err := readErr(c, 5*time.Second); !isReset(err) {
		t.Fatalf("held connection reset: %v", err)
	}
	if e.accepted.Load() != 0 {
		t.Fatal("reset held connection was dialed")
	}
}

func TestBlackhole(t *testing.T) { // Black-hole stalls without closing
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr()})
	live := dial(t, p.Addr())
	mustEcho(t, live, "x", "x")
	if err := p.SetMode(Blackhole{}); err != nil {
		t.Fatal(err)
	}
	if _, err := live.Write([]byte("lost")); err != nil {
		t.Fatal(err)
	}
	if err := readErr(live, 300*time.Millisecond); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("black-holed live read = %v, want a timeout (connection kept open)", err)
	}
	// New connections are accepted but not dialed upstream.
	before := e.accepted.Load()
	fresh := dial(t, p.Addr())
	if _, err := fresh.Write([]byte("also lost")); err != nil {
		t.Fatal(err)
	}
	if err := readErr(fresh, 300*time.Millisecond); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("black-holed new read = %v, want a timeout", err)
	}
	if got := e.accepted.Load(); got != before {
		t.Fatalf("upstream accepted %d connections during the black hole", got-before)
	}
	if err := p.SetMode(Pass{}); err != nil {
		t.Fatal(err)
	}
	mustEcho(t, live, "found", "found")
	mustEcho(t, fresh, "dialed", "dialed")
	if e.accepted.Load() != before+1 {
		t.Fatalf("held connection not dialed after Pass")
	}
}

func TestBlackholeHeldClientCloses(t *testing.T) {
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr(), Mode: Blackhole{}})
	c := dial(t, p.Addr())
	waitFor(t, "connection admitted", func() bool { return p.Stats().Active == 1 })
	_ = c.Close()
	waitFor(t, "held connection closed", func() bool { return p.Stats().Active == 0 })
	if e.accepted.Load() != 0 {
		t.Fatal("closed held connection was dialed")
	}
}

func TestReset(t *testing.T) { // Reset yields ECONNRESET
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr()})
	live := dial(t, p.Addr())
	mustEcho(t, live, "x", "x")
	if err := p.SetMode(Reset{}); err != nil {
		t.Fatal(err)
	}
	if err := readErr(live, 5*time.Second); !isReset(err) {
		t.Fatalf("live connection after Reset: %v, want ECONNRESET", err)
	}
	expectReset(t, p.Addr(), "new connection under Reset")
	if st := p.Stats(); st.Accepted != 2 {
		t.Fatalf("Stats = %+v", st)
	}
	if err := p.SetMode(Pass{}); err != nil {
		t.Fatal(err)
	}
	mustEcho(t, dial(t, p.Addr()), "back", "back")
}

func TestSetUpstream(t *testing.T) { // Upstream switch (scripted failover)
	a, b := startEcho(t, false), startEcho(t, true)
	p := start(t, Config{Upstream: a.addr()})
	first := dial(t, p.Addr())
	mustEcho(t, first, "abc", "abc")
	p.SetUpstream(b.addr())
	if p.Upstream() != b.addr() {
		t.Fatal("Upstream not switched")
	}
	second := dial(t, p.Addr())
	mustEcho(t, second, "abc", "ABC")
	mustEcho(t, first, "def", "def") // live connections keep their upstream
	if a.accepted.Load() != 1 || b.accepted.Load() != 1 {
		t.Fatalf("upstreams accepted %d and %d connections, want one each", a.accepted.Load(), b.accepted.Load())
	}
}

func TestMaxConnsOverflowResets(t *testing.T) { // MaxConns overflow resets
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr(), MaxConns: 2})
	c1, c2 := dial(t, p.Addr()), dial(t, p.Addr())
	mustEcho(t, c1, "1", "1")
	mustEcho(t, c2, "2", "2")
	expectReset(t, p.Addr(), "third connection")
	if st := p.Stats(); st.Active != 2 || st.Accepted != 3 {
		t.Fatalf("Stats = %+v", st)
	}
	_ = c1.Close()
	waitFor(t, "slot freed", func() bool { return p.Stats().Active == 1 })
	mustEcho(t, dial(t, p.Addr()), "4", "4")
}

func TestUpstreamFailures(t *testing.T) {
	// A refused upstream dial resets the client.
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()
	p := start(t, Config{Upstream: dead})
	expectReset(t, p.Addr(), "dead upstream")
	// An upstream reset propagates as a reset to the client.
	rst, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rst.Close() }()
	go func() {
		for {
			c, err := rst.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 1)
			_, _ = c.Read(buf)
			_ = c.(*net.TCPConn).SetLinger(0)
			_ = c.Close()
		}
	}()
	q := start(t, Config{Upstream: rst.Addr().String()})
	c2 := dial(t, q.Addr())
	if _, err := c2.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := readErr(c2, 5*time.Second); !isReset(err) {
		t.Fatalf("upstream reset: %v, want ECONNRESET", err)
	}
}

// TestCloseReturnsGoroutinesToBaseline checks that Close ends every
// goroutine of a proxy whatever its connections do: forwarding, queued
// behind a held chunk with the queue full, black-holed after their dial,
// or held under Blackhole before it.
func TestCloseReturnsGoroutinesToBaseline(t *testing.T) {
	e := startEcho(t, false)
	base := runtime.NumGoroutine()
	var holds atomic.Int64
	held, err := Start(context.Background(), Config{
		Upstream:       e.addr(),
		MaxQueuedBytes: 1,
		ConnTap: func(ConnInfo) ConnHooks {
			return ConnHooks{Hold: func(d Direction, b []byte, _ func()) bool {
				if d == Up && string(b) == "held" {
					holds.Add(1)
					return true
				}
				return false
			}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	holed, err := Start(context.Background(), Config{Upstream: e.addr()})
	if err != nil {
		t.Fatal(err)
	}
	var clients []net.Conn
	for i := range 3 {
		c := dial(t, held.Addr())
		clients = append(clients, c)
		mustEcho(t, c, "x", "x")
		if i > 0 {
			if _, err := c.Write([]byte("held")); err != nil {
				t.Fatal(err)
			}
		}
	}
	waitFor(t, "the held chunks queued", func() bool { return holds.Load() == 2 && held.queuedUp.Load() == 8 })
	for range 3 {
		c := dial(t, holed.Addr())
		clients = append(clients, c)
		mustEcho(t, c, "x", "x")
	}
	if err := holed.SetMode(Blackhole{}); err != nil {
		t.Fatal(err)
	}
	clients = append(clients, dial(t, holed.Addr())) // held, never dialed
	waitFor(t, "held connection admitted", func() bool { return holed.Stats().Active == 4 })
	for _, p := range []*Proxy{held, holed} {
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		if err := p.Close(); err != nil {
			t.Fatalf("second Close = %v", err)
		}
	}
	for _, c := range clients {
		_ = c.Close()
	}
	waitFor(t, "goroutines back to baseline", func() bool { return runtime.NumGoroutine() <= base })
	for _, p := range []*Proxy{held, holed} {
		if err := p.SetMode(Pass{}); !errors.Is(err, ErrClosed) {
			t.Fatalf("SetMode after Close = %v", err)
		}
		if st := p.Stats(); st.Active != 0 {
			t.Fatalf("Active after Close = %d", st.Active)
		}
	}
}

func TestContextEndCloses(t *testing.T) {
	e := startEcho(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	p, err := Start(ctx, Config{Upstream: e.addr()})
	if err != nil {
		t.Fatal(err)
	}
	c := dial(t, p.Addr())
	mustEcho(t, c, "x", "x")
	cancel()
	waitFor(t, "proxy closed", func() bool {
		_, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", p.Addr())
		return errors.Is(err, syscall.ECONNREFUSED)
	})
	if err := readErr(c, 5*time.Second); err == nil {
		t.Fatal("client connection survived the proxy")
	}
	// A context already done closes the proxy at once.
	done, stop := context.WithCancel(context.Background())
	stop()
	q, err := Start(done, Config{Upstream: e.addr()})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "proxy closed", func() bool { return errors.Is(q.SetMode(Pass{}), ErrClosed) })
}

func TestConfigErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := Start(ctx, Config{}); err == nil {
		t.Fatal("empty upstream: want error")
	}
	if _, err := Start(ctx, Config{Upstream: "x:1", Mode: badMode{}}); err == nil {
		t.Fatal("foreign initial mode: want error")
	}
	e := startEcho(t, false)
	if _, err := Start(ctx, Config{Upstream: "x:1", Listen: e.addr()}); err == nil {
		t.Fatal("address in use: want error")
	}
	if _, err := Start(ctx, Config{Upstream: "x:1", Listen: "bad address"}); err == nil {
		t.Fatal("bad listen address: want error")
	}
	p := start(t, Config{Upstream: e.addr()})
	if err := p.SetMode(nil); err == nil {
		t.Fatal("nil mode: want error")
	}
	if err := p.SetMode(badMode{}); err == nil {
		t.Fatal("foreign mode: want error")
	}
}

// badMode is a Mode from outside the package's set.
type badMode struct{ Pass }

func TestStrings(t *testing.T) {
	for m, want := range map[fmt.Stringer]string{
		Pass{}: "pass", Blackhole{}: "blackhole", Reset{}: "reset",
		Up: "up", Down: "down", Direction(7): "Direction(7)",
	} {
		if got := m.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func BenchmarkPassRoundTrip(b *testing.B) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	p, err := Start(context.Background(), Config{Upstream: ln.Addr().String()})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	c, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", p.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	msg := make([]byte, 512)
	buf := make([]byte, 512)
	b.SetBytes(int64(len(msg)))
	for b.Loop() {
		if _, err := c.Write(msg); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(c, buf); err != nil {
			b.Fatal(err)
		}
	}
}
