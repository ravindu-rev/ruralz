// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package faultproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
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

// Tests for 11 test plan item 7 (faultproxy: each mode on live and new
// connections; delay mean and standard deviation within 5% over 10,000
// samples; black-hole stalls without closing; reset yields ECONNRESET at
// the peer; refuse yields ECONNREFUSED; upstream switch; MaxConns overflow
// resets; goroutines return to baseline after Close), verified against a
// TCP echo server (WP-26 "Done when").

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

func TestPassAndTaps(t *testing.T) { // 11 test plan item 7: pass
	e := startEcho(t, false)
	var mu sync.Mutex
	tapped := map[Direction]*bytes.Buffer{Up: {}, Down: {}}
	perConn := map[uint64]int{}
	p := start(t, Config{
		Upstream: e.addr(),
		Tap: func(d Direction, b []byte) {
			mu.Lock()
			_, _ = tapped[d].Write(b)
			mu.Unlock()
		},
		ConnTap: func(ci ConnInfo) TapFunc {
			if ci.Upstream != e.addr() || ci.ID == 0 || ci.Client == "" {
				t.Errorf("ConnTap info = %+v", ci)
			}
			return func(_ Direction, b []byte) {
				mu.Lock()
				perConn[ci.ID] += len(b)
				mu.Unlock()
			}
		},
	})
	if p.Mode() != (Pass{}) || p.Upstream() != e.addr() || !strings.HasPrefix(p.Addr(), "127.0.0.1:") {
		t.Fatalf("mode %v upstream %s addr %s", p.Mode(), p.Upstream(), p.Addr())
	}
	c := dial(t, p.Addr())
	mustEcho(t, c, "hello", "hello")
	mustEcho(t, c, "world", "world")
	// The byte counters move after each write completes, so the client can
	// read the echo before BytesDown counts it.
	waitFor(t, "byte counters", func() bool { s := p.Stats(); return s.BytesUp == 10 && s.BytesDown == 10 })
	st := p.Stats()
	if st.Accepted != 1 || st.Active != 1 || st.BytesUp != 10 || st.BytesDown != 10 || st.Reset != 0 {
		t.Fatalf("Stats = %+v", st)
	}
	mu.Lock()
	if tapped[Up].String() != "helloworld" || tapped[Down].String() != "helloworld" || perConn[1] != 20 {
		t.Fatalf("taps: up %q down %q per-conn %v", tapped[Up], tapped[Down], perConn)
	}
	mu.Unlock()
	conns := p.Conns()
	if len(conns) != 1 || conns[0].ID != 1 || conns[0].Upstream != e.addr() || conns[0].Client != c.LocalAddr().String() {
		t.Fatalf("Conns = %+v", conns)
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

func TestDelayLiveAndNew(t *testing.T) { // 11 test plan item 7: delay on live and new connections
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr()})
	live := dial(t, p.Addr())
	mustEcho(t, live, "a", "a")
	if err := p.SetMode(Delay{Mean: 100 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if d := mustEcho(t, live, "b", "b"); d < 190*time.Millisecond {
		t.Fatalf("live round trip took %v, want at least 2 x 100ms", d)
	}
	fresh := dial(t, p.Addr())
	if d := mustEcho(t, fresh, "c", "c"); d < 190*time.Millisecond {
		t.Fatalf("new connection round trip took %v, want at least 2 x 100ms", d)
	}
	if err := p.SetMode(Pass{}); err != nil {
		t.Fatal(err)
	}
	if d := mustEcho(t, live, "d", "d"); d > 150*time.Millisecond {
		t.Logf("pass round trip took %v (loaded host?)", d)
	}
}

func TestDelayFlushedOnPass(t *testing.T) {
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr(), Mode: Delay{Mean: 5 * time.Second}})
	c := dial(t, p.Addr())
	begin := time.Now()
	if _, err := c.Write([]byte("queued")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := p.SetMode(Pass{}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "queued" {
		t.Fatalf("read %q, %v", buf, err)
	}
	if d := time.Since(begin); d > 3*time.Second {
		t.Fatalf("queued bytes waited %v after Pass", d)
	}
}

// TestDelaySwitchDistribution moves from a long delay through Pass or
// Blackhole to a short delay: the short delay applies at once, because the
// long delay's queue is gone (11 test plan item 7, each mode on live
// connections; OQ-scalability-and-distributed-state-10 (a)).
func TestDelaySwitchDistribution(t *testing.T) {
	tests := []struct {
		name    string
		between Mode
		// readFirst reads the long-delayed bytes back before the switch
		// to the short delay (Pass forwards them; Blackhole drops them).
		readFirst bool
	}{
		{"through pass", Pass{}, true},
		{"through blackhole", Blackhole{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := startEcho(t, false)
			p := start(t, Config{Upstream: e.addr(), Mode: Delay{Mean: 2 * time.Second}})
			c := dial(t, p.Addr())
			if _, err := c.Write([]byte("long")); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "connection dialed", func() bool {
				cs := p.Conns()
				return len(cs) == 1 && cs[0].Upstream != ""
			})
			waitFor(t, "chunk queued under Delay", func() bool { return p.queuedUp.Load() == 4 })
			if s := p.Stats(); s.BytesUp != 0 {
				t.Fatalf("the chunk was forwarded before the switch: BytesUp %d", s.BytesUp)
			}
			if err := p.SetMode(tt.between); err != nil {
				t.Fatal(err)
			}
			if tt.readFirst {
				buf := make([]byte, 4)
				_ = c.SetReadDeadline(time.Now().Add(time.Second))
				if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "long" {
					t.Fatalf("flushed bytes = %q, %v", buf, err)
				}
			} else {
				waitFor(t, "queue dropped", func() bool { return p.queuedUp.Load() == 0 })
			}
			if err := p.SetMode(Delay{Mean: 10 * time.Millisecond}); err != nil {
				t.Fatal(err)
			}
			if d := mustEcho(t, c, "short", "short"); d >= 500*time.Millisecond {
				t.Fatalf("round trip under Delay{10ms} took %v, want under 500ms (the 2s delay leaked)", d)
			}
		})
	}
}

func TestDelayDistribution(t *testing.T) { // 11 test plan item 7: mean and std dev within 5% over 10,000 samples
	for _, d := range []Delay{{Mean: 10 * time.Millisecond, StdDev: 2 * time.Millisecond}, {Mean: 200 * time.Millisecond, StdDev: 50 * time.Millisecond}} {
		p := &Proxy{rng: rand.New(rand.NewPCG(7, 11))} //nolint:gosec // G404: test randomness
		const n = 10000
		var sum, sumSq float64
		for range n {
			v := float64(p.sampleDelay(d))
			sum += v
			sumSq += v * v
		}
		mean := sum / n
		sd := math.Sqrt(sumSq/n - mean*mean)
		if math.Abs(mean-float64(d.Mean)) > 0.05*float64(d.Mean) {
			t.Errorf("%v: mean %v", d, time.Duration(mean))
		}
		if math.Abs(sd-float64(d.StdDev)) > 0.05*float64(d.StdDev) {
			t.Errorf("%v: std dev %v", d, time.Duration(sd))
		}
	}
	// Clamped at 0.
	p := &Proxy{rng: rand.New(rand.NewPCG(1, 2))} //nolint:gosec // G404: test randomness
	zeros := 0
	for range 1000 {
		v := p.sampleDelay(Delay{Mean: time.Millisecond, StdDev: 5 * time.Millisecond})
		if v < 0 {
			t.Fatalf("negative delay %v", v)
		}
		if v == 0 {
			zeros++
		}
	}
	if zeros < 300 {
		t.Fatalf("clamped samples = %d, want about 42%%", zeros)
	}
	if got := p.sampleDelay(Delay{Mean: 3 * time.Millisecond}); got != 3*time.Millisecond {
		t.Fatalf("no jitter = %v", got)
	}
}

func TestDelayLiveMeasured(t *testing.T) { // 11 test plan item 7: the distribution on a live connection
	e := startEcho(t, false)
	mean, sd := 20*time.Millisecond, 4*time.Millisecond
	p := start(t, Config{Upstream: e.addr(), Mode: Delay{Mean: mean, StdDev: sd}, Seed: 42})
	c := dial(t, p.Addr())
	const n = 60
	var total time.Duration
	for i := range n {
		total += mustEcho(t, c, fmt.Sprintf("%03d", i), fmt.Sprintf("%03d", i))
	}
	// Each round trip holds two independent delays: mean 2 x 20ms.
	avg := total / n
	if avg < 2*mean-2*time.Millisecond || avg > 2*mean+15*time.Millisecond {
		t.Fatalf("average round trip %v, want about %v", avg, 2*mean)
	}
}

func TestDelayKeepsOrderAndPipelines(t *testing.T) {
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr(), Mode: Delay{Mean: 30 * time.Millisecond, StdDev: 20 * time.Millisecond}, Seed: 3})
	c := dial(t, p.Addr())
	const n = 100
	var want bytes.Buffer
	begin := time.Now()
	go func() {
		for i := range n {
			msg := fmt.Sprintf("%04d;", i)
			if _, err := c.Write([]byte(msg)); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	for i := range n {
		fmt.Fprintf(&want, "%04d;", i)
	}
	got := make([]byte, want.Len())
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("order changed:\n%s", got)
	}
	// Serialized delays would take n x 2 x 30ms = 6s.
	if d := time.Since(begin); d > 3*time.Second {
		t.Fatalf("delayed stream took %v: chunks were serialized", d)
	}
}

func TestDelayWithHalfClose(t *testing.T) {
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr(), Mode: Delay{Mean: 100 * time.Millisecond}})
	c := dial(t, p.Addr())
	begin := time.Now()
	if _, err := c.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "abc" {
		t.Fatalf("read %q, %v", got, err)
	}
	if d := time.Since(begin); d < 190*time.Millisecond {
		t.Fatalf("delayed bytes and FIN arrived after %v, want at least 2 x 100ms", d)
	}
}

func TestDelayQueueBound(t *testing.T) {
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr(), Mode: Delay{Mean: 20 * time.Millisecond}, MaxQueuedBytes: 16 << 10})
	c := dial(t, p.Addr())
	payload := bytes.Repeat([]byte("0123456789abcdef"), 16<<10) // 256 KiB
	go func() {
		_, _ = c.Write(payload)
		_ = c.CloseWrite()
	}()
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

func TestHeldConnectionReset(t *testing.T) {
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr(), Mode: Blackhole{}})
	c := dial(t, p.Addr())
	waitFor(t, "connection admitted", func() bool { return p.Stats().Active == 1 })
	id := p.Conns()[0].ID
	if err := p.SetConnMode(id, Reset{}); err != nil {
		t.Fatal(err)
	}
	if err := readErr(c, 5*time.Second); !isReset(err) {
		t.Fatalf("held connection reset: %v", err)
	}
	if e.accepted.Load() != 0 {
		t.Fatal("reset held connection was dialed")
	}
}

func TestBlackhole(t *testing.T) { // 11 test plan item 7: black-hole stalls without closing
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

func TestReset(t *testing.T) { // 11 test plan item 7: reset yields ECONNRESET
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
	if st := p.Stats(); st.Reset < 2 || st.Accepted != 2 {
		t.Fatalf("Stats = %+v", st)
	}
	if err := p.SetMode(Pass{}); err != nil {
		t.Fatal(err)
	}
	mustEcho(t, dial(t, p.Addr()), "back", "back")
}

func TestRefuse(t *testing.T) { // 11 test plan item 7: refuse yields ECONNREFUSED
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr()})
	live := dial(t, p.Addr())
	mustEcho(t, live, "x", "x")
	if err := p.SetMode(Refuse{}); err != nil {
		t.Fatal(err)
	}
	if err := readErr(live, 5*time.Second); !isReset(err) {
		t.Fatalf("live connection after Refuse: %v, want ECONNRESET", err)
	}
	_, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(context.Background(), "tcp", p.Addr())
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dial under Refuse: %v, want ECONNREFUSED", err)
	}
	if err := p.SetMode(Refuse{}); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := p.SetMode(Pass{}); err != nil {
		t.Fatal(err)
	}
	mustEcho(t, dial(t, p.Addr()), "reopened", "reopened")
}

func TestStartRefusing(t *testing.T) {
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr(), Mode: Refuse{}})
	if _, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", p.Addr()); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dial = %v, want ECONNREFUSED", err)
	}
	if err := p.SetMode(Delay{Mean: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	mustEcho(t, dial(t, p.Addr()), "ok", "ok")
}

func TestSetUpstream(t *testing.T) { // 11 test plan item 7: upstream switch (CE-5 failover)
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
	conns := p.Conns()
	if len(conns) != 2 || conns[0].Upstream != a.addr() || conns[1].Upstream != b.addr() {
		t.Fatalf("Conns = %+v", conns)
	}
}

func TestMaxConnsOverflowResets(t *testing.T) { // 11 test plan item 7: MaxConns overflow resets
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr(), MaxConns: 2})
	c1, c2 := dial(t, p.Addr()), dial(t, p.Addr())
	mustEcho(t, c1, "1", "1")
	mustEcho(t, c2, "2", "2")
	expectReset(t, p.Addr(), "third connection")
	if st := p.Stats(); st.Reset != 1 || st.Active != 2 || st.Accepted != 3 {
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

func TestSetConnMode(t *testing.T) {
	e := startEcho(t, false)
	p := start(t, Config{Upstream: e.addr()})
	c1, c2 := dial(t, p.Addr()), dial(t, p.Addr())
	mustEcho(t, c1, "1", "1")
	mustEcho(t, c2, "2", "2")
	conns := p.Conns()
	if len(conns) != 2 {
		t.Fatalf("Conns = %+v", conns)
	}
	id1, id2 := conns[0].ID, conns[1].ID
	if err := p.SetConnMode(id1, Blackhole{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c1.Write([]byte("lost")); err != nil {
		t.Fatal(err)
	}
	if err := readErr(c1, 300*time.Millisecond); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("overridden connection read = %v", err)
	}
	mustEcho(t, c2, "still", "still")
	if err := p.SetConnMode(id1, nil); err != nil {
		t.Fatal(err)
	}
	mustEcho(t, c1, "back", "back")
	if err := p.SetConnMode(id2, Reset{}); err != nil {
		t.Fatal(err)
	}
	if err := readErr(c2, 5*time.Second); !isReset(err) {
		t.Fatalf("connection reset by SetConnMode: %v", err)
	}
	if err := p.SetConnMode(9999, Pass{}); err == nil {
		t.Fatal("unknown connection: want error")
	}
	if err := p.SetConnMode(id1, Refuse{}); err == nil {
		t.Fatal("Refuse on a connection: want error")
	}
	if err := p.SetConnMode(id1, Delay{Mean: -1}); err == nil {
		t.Fatal("negative delay: want error")
	}
}

func TestCloseReturnsGoroutinesToBaseline(t *testing.T) { // 11 test plan item 7: goroutines return to baseline
	e := startEcho(t, false)
	base := runtime.NumGoroutine()
	p, err := Start(context.Background(), Config{Upstream: e.addr()})
	if err != nil {
		t.Fatal(err)
	}
	var clients []net.Conn
	for i := range 6 {
		c := dial(t, p.Addr())
		clients = append(clients, c)
		mustEcho(t, c, "x", "x")
		switch i % 3 {
		case 1: // queued delayed bytes
			if err := p.SetConnMode(p.Conns()[i].ID, Delay{Mean: time.Hour}); err != nil {
				t.Fatal(err)
			}
			_, _ = c.Write([]byte("held"))
		case 2: // black-holed
			if err := p.SetConnMode(p.Conns()[i].ID, Blackhole{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := p.SetMode(Blackhole{}); err != nil {
		t.Fatal(err)
	}
	clients = append(clients, dial(t, p.Addr())) // held, never dialed
	waitFor(t, "held connection admitted", func() bool { return p.Stats().Active == 7 })
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	for _, c := range clients {
		_ = c.Close()
	}
	waitFor(t, "goroutines back to baseline", func() bool { return runtime.NumGoroutine() <= base })
	if err := p.SetMode(Pass{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("SetMode after Close = %v", err)
	}
	if st := p.Stats(); st.Active != 0 {
		t.Fatalf("Active after Close = %d", st.Active)
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
	if _, err := Start(ctx, Config{Upstream: "x:1", Mode: Delay{StdDev: -1}}); err == nil {
		t.Fatal("negative std dev: want error")
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
	// Leaving Refuse fails when the port was taken meanwhile.
	if err := p.SetMode(Refuse{}); err != nil {
		t.Fatal(err)
	}
	thief, err := (&net.ListenConfig{}).Listen(ctx, "tcp", p.Addr())
	if err != nil {
		t.Skipf("port not reusable at once: %v", err)
	}
	defer func() { _ = thief.Close() }()
	if err := p.SetMode(Pass{}); err == nil {
		t.Fatal("reopen on a taken port: want error")
	}
	if _, refuse := p.Mode().(Refuse); !refuse {
		t.Fatalf("mode after a failed reopen = %v, want refuse", p.Mode())
	}
}

// badMode is a Mode from outside the package's set.
type badMode struct{ Pass }

func TestStrings(t *testing.T) {
	for m, want := range map[fmt.Stringer]string{
		Pass{}: "pass", Blackhole{}: "blackhole", Reset{}: "reset", Refuse{}: "refuse",
		Delay{Mean: time.Millisecond, StdDev: time.Microsecond}: "delay(1ms±1µs)",
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
