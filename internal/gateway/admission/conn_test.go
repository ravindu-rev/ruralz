// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"bytes"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Tests for spec 04 req 11 (Node-wide connection ceiling: a counting
// listener takes a slot before the inner Accept, releases it on Close and
// stops calling Accept at the ceiling; the admin listener has its own 256)
// and spec 04 req 67 (ceiling lowered during a handover); test plan item 9.

// fakeListener hands out queued connections and counts inner Accept calls.
type fakeListener struct {
	conns   chan net.Conn
	errs    chan error
	accepts atomic.Int64
	closed  chan struct{}
	once    sync.Once
}

func newFakeListener() *fakeListener {
	return &fakeListener{conns: make(chan net.Conn, 16), errs: make(chan error, 4), closed: make(chan struct{})}
}

func (f *fakeListener) Accept() (net.Conn, error) {
	f.accepts.Add(1)
	select {
	case c := <-f.conns:
		return c, nil
	case err := <-f.errs:
		return nil, err
	case <-f.closed:
		return nil, net.ErrClosed
	}
}

func (f *fakeListener) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080} }

// queue adds one connection to the inner accept queue.
func (f *fakeListener) queue(t *testing.T) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	f.conns <- a
}

// waitFor yields to the scheduler until cond holds and fails after a
// bounded number of yields; it reads no wall clock (architecture section
// 0 rule 4). The goroutines it waits for only need CPU time to get there.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 10_000_000 {
		if cond() {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("gave up waiting for %s", what)
}

func waiters(c *ConnLimiter) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waiters
}

type acceptResult struct {
	conn net.Conn
	err  error
}

func acceptAsync(ln net.Listener) <-chan acceptResult {
	ch := make(chan acceptResult, 1)
	go func() {
		c, err := ln.Accept()
		ch <- acceptResult{c, err}
	}()
	return ch
}

// TestReq11ConnCeilingBlocksAccept: at the ceiling the wrapped listener
// does not call the inner Accept even with connections queued; a Close
// frees the slot and accepting resumes.
func TestReq11ConnCeilingBlocksAccept(t *testing.T) {
	inner := newFakeListener()
	c := NewConnLimiter(2)
	ln := c.Wrap(inner)
	var open []net.Conn
	for range 2 {
		inner.queue(t)
		conn, err := ln.Accept()
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		open = append(open, conn)
	}
	inner.queue(t)
	res := acceptAsync(ln)
	waitFor(t, "the third Accept to wait for a slot", func() bool { return waiters(c) == 1 })
	if n := inner.accepts.Load(); n != 2 {
		t.Fatalf("inner Accept called %d times at the ceiling, want 2", n)
	}
	if c.Open() != 2 {
		t.Fatalf("Open = %d, want 2", c.Open())
	}
	if err := open[0].Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r := <-res
	if r.err != nil {
		t.Fatalf("Accept after a Close: %v", r.err)
	}
	if inner.accepts.Load() != 3 || c.Open() != 2 {
		t.Fatalf("inner accepts %d, open %d", inner.accepts.Load(), c.Open())
	}
	// A second Close of the same connection frees nothing more.
	_ = open[0].Close()
	if c.Open() != 2 {
		t.Fatalf("double Close released twice: open %d", c.Open())
	}
	_ = open[1].Close()
	_ = r.conn.Close()
	if c.Open() != 0 {
		t.Fatalf("Open = %d after closing everything", c.Open())
	}
}

// TestReq11ConnCeilingSharedAcrossListeners: one limiter bounds all client
// listeners together.
func TestReq11ConnCeilingSharedAcrossListeners(t *testing.T) {
	a, b := newFakeListener(), newFakeListener()
	c := NewConnLimiter(1)
	la, lb := c.Wrap(a), c.Wrap(b)
	a.queue(t)
	conn, err := la.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	b.queue(t)
	res := acceptAsync(lb)
	waitFor(t, "the second listener to wait", func() bool { return waiters(c) == 1 })
	if b.accepts.Load() != 0 {
		t.Fatal("the second listener accepted past the shared ceiling")
	}
	_ = conn.Close()
	if r := <-res; r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	} else {
		_ = r.conn.Close()
	}
}

// TestReq11ConnListenerCloseUnblocks: closing the listener ends an Accept
// waiting for a slot with an error matching net.ErrClosed, the stop path
// http.Server.Serve expects.
func TestReq11ConnListenerCloseUnblocks(t *testing.T) {
	inner := newFakeListener()
	c := NewConnLimiter(0)
	ln := c.Wrap(inner)
	res := acceptAsync(ln)
	waitFor(t, "Accept to wait", func() bool { return waiters(c) == 1 })
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r := <-res
	if !errors.Is(r.err, net.ErrClosed) {
		t.Fatalf("Accept after Close = %v, want net.ErrClosed", r.err)
	}
	var op *net.OpError
	if !errors.As(r.err, &op) || op.Op != "accept" {
		t.Fatalf("error %v is not an accept OpError", r.err)
	}
	if waiters(c) != 0 || c.Open() != 0 || inner.accepts.Load() != 0 {
		t.Fatal("a closed listener left state behind")
	}
	_ = ln.Close() // idempotent
}

// TestReq67ConnOpenCountsAcceptedOnly: an accept loop waiting in the inner
// Accept holds a slot against the ceiling but is no connection, so Open
// (the handover usage report's connections) leaves it out.
func TestReq67ConnOpenCountsAcceptedOnly(t *testing.T) {
	a, b := newFakeListener(), newFakeListener()
	c := NewConnLimiter(1)
	la, lb := c.Wrap(a), c.Wrap(b)
	resA := acceptAsync(la)
	waitFor(t, "the first accept loop to enter the inner Accept", func() bool { return a.accepts.Load() == 1 })
	if c.Open() != 0 {
		t.Fatalf("Open = %d with no connection accepted, want 0", c.Open())
	}
	// The pending accept's slot still counts against the ceiling.
	resB := acceptAsync(lb)
	waitFor(t, "the second accept loop to wait for a slot", func() bool { return waiters(c) == 1 })
	if b.accepts.Load() != 0 {
		t.Fatal("the second listener called Accept past the ceiling")
	}
	a.queue(t)
	r := <-resA
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	if c.Open() != 1 {
		t.Fatalf("Open = %d, want 1", c.Open())
	}
	_ = r.conn.Close()
	waitFor(t, "the second accept loop to enter the inner Accept", func() bool { return b.accepts.Load() == 1 })
	if c.Open() != 0 {
		t.Fatalf("Open = %d after Close, want 0", c.Open())
	}
	_ = lb.Close()
	if r := <-resB; !errors.Is(r.err, net.ErrClosed) {
		t.Fatalf("Accept after Close = %v", r.err)
	}
	_ = la.Close()
	c.mu.Lock()
	slots := c.slots
	c.mu.Unlock()
	if slots != 0 {
		t.Fatalf("slots = %d after closing every listener", slots)
	}
}

// TestReq11ConnInnerErrorReleases returns the slot when the inner Accept
// fails (for example a temporary error http.Server retries).
func TestReq11ConnInnerErrorReleases(t *testing.T) {
	inner := newFakeListener()
	c := NewConnLimiter(1)
	ln := c.Wrap(inner)
	boom := errors.New("accept: too many open files")
	inner.errs <- boom
	if _, err := ln.Accept(); !errors.Is(err, boom) {
		t.Fatalf("Accept = %v, want the inner error", err)
	}
	if c.Open() != 0 {
		t.Fatalf("a failed Accept kept its slot: open %d", c.Open())
	}
	inner.queue(t)
	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	_ = conn.Close()
}

// TestReq67ConnSetExternal lowers the ceiling during a handover and wakes a
// waiting Accept when the other process's usage drops.
func TestReq67ConnSetExternal(t *testing.T) {
	inner := newFakeListener()
	c := NewConnLimiter(3)
	ln := c.Wrap(inner)
	inner.queue(t)
	first, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	c.SetExternal(2)
	if c.Ceiling() != 1 {
		t.Fatalf("Ceiling = %d, want 1", c.Ceiling())
	}
	inner.queue(t)
	res := acceptAsync(ln)
	waitFor(t, "Accept to wait under the lowered ceiling", func() bool { return waiters(c) == 1 })
	c.SetExternal(1)
	r := <-res
	if r.err != nil {
		t.Fatalf("Accept after the usage dropped: %v", r.err)
	}
	if c.Open() != 2 {
		t.Fatalf("Open = %d, want 2", c.Open())
	}
	c.SetExternal(-4)
	if c.Ceiling() != 3 {
		t.Fatalf("Ceiling = %d, want 3", c.Ceiling())
	}
	_ = first.Close()
	_ = r.conn.Close()
}

// TestReq11AdminCeiling is the admin listener's own ceiling.
func TestReq11AdminCeiling(t *testing.T) {
	if AdminConnections != 256 || DefaultConnections != 20_000 {
		t.Fatalf("ceilings %d and %d", AdminConnections, DefaultConnections)
	}
	if c := NewConnLimiter(AdminConnections); c.Ceiling() != 256 {
		t.Fatalf("Ceiling = %d", c.Ceiling())
	}
	if c := NewConnLimiter(-1); c.Ceiling() != 0 {
		t.Fatalf("negative limit Ceiling = %d", c.Ceiling())
	}
}

func listenLoopback(t *testing.T) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
}

func dialContext(t *testing.T, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(t.Context(), "tcp", addr)
}

// TestReq11ConnTCP runs the limiter over a real loopback listener: the
// kernel completes a third handshake, but the server accepts it only after
// a connection closes.
func TestReq11ConnTCP(t *testing.T) {
	inner, err := listenLoopback(t)
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	c := NewConnLimiter(2)
	ln := c.Wrap(inner)
	defer func() { _ = ln.Close() }()

	accepted := make(chan net.Conn, 3)
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()
	var clients []net.Conn
	for range 3 {
		cc, err := dialContext(t, inner.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		clients = append(clients, cc)
	}
	defer func() {
		for _, cc := range clients {
			_ = cc.Close()
		}
	}()
	first := <-accepted
	second := <-accepted
	waitFor(t, "the accept loop to wait at the ceiling", func() bool { return waiters(c) == 1 })
	select {
	case <-accepted:
		t.Fatal("a third connection was accepted at the ceiling")
	default:
	}
	_ = first.Close()
	third := <-accepted
	if c.Open() != 2 {
		t.Fatalf("Open = %d, want 2", c.Open())
	}
	// The accepted connection keeps the TCP connection's capabilities.
	if nc, ok := third.(interface{ NetConn() net.Conn }); !ok {
		t.Fatal("no NetConn")
	} else if _, ok := nc.NetConn().(*net.TCPConn); !ok {
		t.Fatalf("NetConn is %T, want *net.TCPConn", nc.NetConn())
	}
	_ = second.Close()
	_ = third.Close()
	_ = ln.Close()
	<-serveDone
}

// TestConnPassThrough checks the wrapper keeps CloseWrite and ReadFrom.
func TestConnPassThrough(t *testing.T) {
	c := NewConnLimiter(4)

	t.Run("pipe without CloseWrite or ReadFrom", func(t *testing.T) {
		a, b := net.Pipe()
		defer func() { _ = b.Close() }()
		lc := &limitedConn{Conn: a, c: c}
		c.slots++
		c.conns++
		if err := lc.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite on a pipe = %v, want nil", err)
		}
		got := make(chan []byte, 1)
		go func() {
			data, _ := io.ReadAll(b)
			got <- data
		}()
		n, err := lc.ReadFrom(strings.NewReader("hello"))
		if err != nil || n != 5 {
			t.Fatalf("ReadFrom = %d, %v", n, err)
		}
		_ = lc.Close()
		if data := <-got; !bytes.Equal(data, []byte("hello")) {
			t.Fatalf("peer read %q", data)
		}
		if c.Open() != 0 {
			t.Fatalf("Open = %d", c.Open())
		}
	})

	t.Run("tcp with CloseWrite and ReadFrom", func(t *testing.T) {
		inner, err := listenLoopback(t)
		if err != nil {
			t.Skipf("no loopback listener: %v", err)
		}
		ln := c.Wrap(inner)
		defer func() { _ = ln.Close() }()
		client, err := dialContext(t, inner.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = client.Close() }()
		conn, err := ln.Accept()
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		rf, ok := conn.(io.ReaderFrom)
		if !ok {
			t.Fatal("no ReaderFrom")
		}
		if n, err := rf.ReadFrom(strings.NewReader("ping")); err != nil || n != 4 {
			t.Fatalf("ReadFrom = %d, %v", n, err)
		}
		cw, ok := conn.(interface{ CloseWrite() error })
		if !ok {
			t.Fatal("no CloseWrite")
		}
		if err := cw.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
		data, err := io.ReadAll(client)
		if err != nil || string(data) != "ping" {
			t.Fatalf("client read %q, %v", data, err)
		}
		_ = conn.Close()
		if c.Open() != 0 {
			t.Fatalf("Open = %d", c.Open())
		}
	})
}

// TestReq11ConnChurn accepts and closes from several listeners at once
// while checking the open count never exceeds the ceiling.
func TestReq11ConnChurn(t *testing.T) {
	const ceiling, listeners, perListener = 3, 4, 50
	c := NewConnLimiter(ceiling)
	var peak atomic.Int64
	var wg sync.WaitGroup
	for range listeners {
		inner := newFakeListener()
		ln := c.Wrap(inner)
		wg.Go(func() {
			for range perListener {
				a, b := net.Pipe()
				inner.conns <- a
				conn, err := ln.Accept()
				if err != nil {
					t.Errorf("Accept: %v", err)
					return
				}
				raise(&peak, c.Open())
				_ = conn.Close()
				_ = b.Close()
			}
		})
	}
	wg.Wait()
	if p := peak.Load(); p > ceiling {
		t.Fatalf("peak open %d over the ceiling %d", p, ceiling)
	}
	if c.Open() != 0 || waiters(c) != 0 {
		t.Fatalf("open %d, waiters %d after churn", c.Open(), waiters(c))
	}
}
