// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// Tests for the deadlines of docs/reference/01-cli-and-api-surface.md
// "Admin API conventions", on the fake clock: 10 s to connect and finish
// TLS, 30 s to the response headers, 5 min for a whole call that does not
// stream, which --timeout replaces and 0 removes, and no body deadline on
// a /tap stream. An expired deadline exits 2; Ctrl-C ends any wait.

// blockingDial is a dial hook that waits until its context ends.
type blockingDial struct {
	started, done chan struct{}
}

func newBlockingDial() *blockingDial {
	return &blockingDial{started: make(chan struct{}), done: make(chan struct{})}
}

func (b *blockingDial) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	close(b.started)
	<-ctx.Done()
	close(b.done)
	return nil, ctx.Err()
}

// within waits for ch under a real-time deadline.
func within(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// notYet fails when the call already returned.
func notYet(t *testing.T, ch <-chan result) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("the call returned before its deadline: %v", r.err)
	default:
	}
}

// silentListener accepts connections and never answers, as a peer that
// stalls the TLS handshake.
func silentListener(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				_, _ = io.Copy(io.Discard, conn)
				_ = conn.Close()
			})
		}
	})
	return ln
}

// wantDeadline checks that err is a DeadlineError of kind with message
// want, exiting 2.
func wantDeadline(t *testing.T, err error, kind DeadlineKind, want string) {
	t.Helper()
	de, ok := errors.AsType[*DeadlineError](err)
	if !ok || de.Kind != kind || command.Code(t.Context(), err) != command.ExitNoResult {
		t.Fatalf("error = %v, want a %s deadline, exit 2", err, kind)
	}
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// TestConnectDeadline checks that connecting gets 10 s.
func TestConnectDeadline(t *testing.T) {
	c, clk := newTestClient(t, Config{URL: "http://node-a.shop.example:9901", Timeout: DefaultTimeout})
	bd := newBlockingDial()
	c.dial = bd.dial
	ch := goDump(t.Context(), c)
	within(t, "the dial", bd.started)
	waitFor(t, "the call and connection deadlines", func() bool { return clk.Pending() == 2 })
	clk.Advance(ConnectTimeout - time.Nanosecond)
	notYet(t, ch)
	clk.Advance(time.Nanosecond)
	r := await(t, ch)
	wantDeadline(t, r.err, DeadlineConnect, "connect to node-a.shop.example:9901: no connection within 10s")
	within(t, "the dial to end", bd.done)
}

// TestTLSConnectDeadline checks that the 10 s also cover the TLS
// handshake.
func TestTLSConnectDeadline(t *testing.T) {
	ln := silentListener(t)
	c, clk := newTestClient(t, Config{URL: "https://" + ln.Addr().String(), Timeout: DefaultTimeout})
	ch := goDump(t.Context(), c)
	waitFor(t, "the call and connection deadlines", func() bool { return clk.Pending() == 2 })
	clk.Advance(ConnectTimeout - time.Nanosecond)
	notYet(t, ch)
	clk.Advance(time.Nanosecond)
	r := await(t, ch)
	wantDeadline(t, r.err, DeadlineConnect, "connect to "+ln.Addr().String()+": no TLS connection within 10s")
}

// stallingServer answers each request with pre, then waits until release
// is closed or the client leaves; entered reports each request once pre
// has run.
type stallingServer struct {
	url     string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newStallingServer(t *testing.T, pre func(w http.ResponseWriter), post func(w http.ResponseWriter)) *stallingServer {
	t.Helper()
	s := &stallingServer{entered: make(chan struct{}, 4), release: make(chan struct{})}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if pre != nil {
			pre(w)
		}
		s.entered <- struct{}{}
		select {
		case <-s.release:
			if post != nil {
				post(w)
			}
		case <-r.Context().Done():
		}
	})
	t.Cleanup(s.free)
	s.url = srv.URL
	return s
}

// free lets every stalled request finish.
func (s *stallingServer) free() { s.once.Do(func() { close(s.release) }) }

// partialBody writes the start of a body and flushes it, so the headers
// arrive and the body stalls.
func partialBody(w http.ResponseWriter) {
	_, _ = io.WriteString(w, `{"content":`)
	_ = http.NewResponseController(w).Flush()
}

// TestHeaderDeadline checks that the response headers get 30 s from the
// established connection.
func TestHeaderDeadline(t *testing.T) {
	s := newStallingServer(t, nil, nil)
	c, clk := newTestClient(t, Config{URL: s.url, Timeout: DefaultTimeout})
	ch := goDump(t.Context(), c)
	within(t, "the request", s.entered)
	waitFor(t, "the call and header deadlines", func() bool { return clk.Pending() == 2 })
	clk.Advance(HeaderTimeout - time.Nanosecond)
	notYet(t, ch)
	clk.Advance(time.Nanosecond)
	r := await(t, ch)
	wantDeadline(t, r.err, DeadlineHeaders, "GET "+s.url+"/config/dump: no response headers within 30s")
}

// TestHeaderDeadlineOfStream checks that a /tap subscription waits for
// its headers under the same 30 s.
func TestHeaderDeadlineOfStream(t *testing.T) {
	s := newStallingServer(t, nil, nil)
	c, clk := newTestClient(t, Config{URL: s.url, Timeout: DefaultTimeout})
	ch := make(chan result, 1)
	go func() {
		st, err := c.Tap(t.Context(), 1)
		if st != nil {
			_ = st.Close()
		}
		ch <- result{err: err}
	}()
	within(t, "the subscription", s.entered)
	waitFor(t, "the header deadline alone", func() bool { return clk.Pending() == 1 })
	clk.Advance(HeaderTimeout)
	r := await(t, ch)
	wantDeadline(t, r.err, DeadlineHeaders, "GET "+s.url+"/tap?sample=1: no response headers within 30s")
}

// TestCallDeadline checks the whole-call deadline: 5 min by default, the
// --timeout value when given, through a body that stalls.
func TestCallDeadline(t *testing.T) {
	for _, timeout := range []time.Duration{DefaultTimeout, 2 * time.Second} {
		t.Run(command.FormatDuration(timeout), func(t *testing.T) {
			s := newStallingServer(t, partialBody, nil)
			c, clk := newTestClient(t, Config{URL: s.url, Timeout: timeout})
			ch := goDump(t.Context(), c)
			within(t, "the partial body", s.entered)
			waitFor(t, "the call deadline alone", func() bool { return clk.Pending() == 1 })
			clk.Advance(timeout - time.Nanosecond)
			notYet(t, ch)
			clk.Advance(time.Nanosecond)
			r := await(t, ch)
			wantDeadline(t, r.err, DeadlineCall,
				"GET "+s.url+"/config/dump: not complete within "+command.FormatDuration(timeout)+" (--timeout)")
		})
	}
}

// TestNoCallDeadlineWithZeroTimeout checks that --timeout 0 removes the
// whole-call deadline: a slow body completes however long it takes.
func TestNoCallDeadlineWithZeroTimeout(t *testing.T) {
	s := newStallingServer(t, partialBody, func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{}}`) })
	c, clk := newTestClient(t, Config{URL: s.url, Timeout: 0})
	ch := goDump(t.Context(), c)
	within(t, "the partial body", s.entered)
	waitFor(t, "no deadline", func() bool { return clk.Pending() == 0 })
	clk.Advance(24 * time.Hour)
	notYet(t, ch)
	s.free()
	r := await(t, ch)
	if r.err != nil || string(r.body) != `{"content":{}}` {
		t.Errorf("ConfigDump = %q, %v", r.body, r.err)
	}
}

// TestStreamHasNoBodyDeadline checks that an accepted /tap stream runs
// without a deadline, whatever --timeout says.
func TestStreamHasNoBodyDeadline(t *testing.T) {
	lines := make(chan string, 1)
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = http.NewResponseController(w).Flush()
		for {
			select {
			case l := <-lines:
				_, _ = io.WriteString(w, l+"\n")
				_ = http.NewResponseController(w).Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	c, clk := newTestClient(t, Config{URL: srv.URL, Timeout: time.Second})
	s, err := c.Tap(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if n := clk.Pending(); n != 0 {
		t.Fatalf("%d deadlines run on the stream, want none", n)
	}
	clk.Advance(24 * time.Hour)
	lines <- `{"dropped":3}`
	line, err := s.Next(t.Context())
	if err != nil || string(line) != `{"dropped":3}` {
		t.Errorf("Next = %q, %v", line, err)
	}
}

// wantInterrupted checks that err ends the command as the interruption of
// ctx: exit 130, the interruption in its chain.
func wantInterrupted(t *testing.T, ctx context.Context, err error) {
	t.Helper()
	if command.Code(ctx, err) != command.ExitInterrupted {
		t.Errorf("error %v exits %d, want 130", err, command.Code(ctx, err))
	}
	if _, ok := errors.AsType[command.Interrupted](err); !ok {
		t.Errorf("error = %v, want the interruption as its cause", err)
	}
}

// TestInterruptEndsEveryWait checks that Ctrl-C ends the wait for the
// connection, the TLS handshake, the headers and the body, and that a
// dial the call stops waiting for is canceled.
func TestInterruptEndsEveryWait(t *testing.T) {
	t.Run("connection", func(t *testing.T) {
		c, _ := newTestClient(t, Config{URL: "http://node-a.shop.example:9901", Timeout: DefaultTimeout})
		bd := newBlockingDial()
		c.dial = bd.dial
		ctx, interrupt := interruptible(t)
		ch := goDump(ctx, c)
		within(t, "the dial", bd.started)
		interrupt()
		wantInterrupted(t, ctx, await(t, ch).err)
		within(t, "the abandoned dial to end", bd.done)
	})
	t.Run("TLS handshake", func(t *testing.T) {
		ln := silentListener(t)
		c, clk := newTestClient(t, Config{URL: "https://" + ln.Addr().String(), Timeout: DefaultTimeout})
		ctx, interrupt := interruptible(t)
		ch := goDump(ctx, c)
		waitFor(t, "the connection deadline", func() bool { return clk.Pending() == 2 })
		interrupt()
		wantInterrupted(t, ctx, await(t, ch).err)
		waitFor(t, "the abandoned handshake to end", func() bool { return clk.Pending() == 0 })
	})
	t.Run("headers", func(t *testing.T) {
		s := newStallingServer(t, nil, nil)
		c, _ := newTestClient(t, Config{URL: s.url, Timeout: DefaultTimeout})
		ctx, interrupt := interruptible(t)
		ch := goDump(ctx, c)
		within(t, "the request", s.entered)
		interrupt()
		wantInterrupted(t, ctx, await(t, ch).err)
	})
	t.Run("body", func(t *testing.T) {
		s := newStallingServer(t, partialBody, nil)
		c, clk := newTestClient(t, Config{URL: s.url, Timeout: 0})
		ctx, interrupt := interruptible(t)
		ch := goDump(ctx, c)
		within(t, "the partial body", s.entered)
		interrupt()
		wantInterrupted(t, ctx, await(t, ch).err)
		if n := clk.Pending(); n != 0 {
			t.Errorf("%d deadlines left armed", n)
		}
	})
}

// TestDeadlineStopsWithTheCall checks that a finished call leaves no
// deadline armed on the clock.
func TestDeadlineStopsWithTheCall(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{}") })
	c, clk := newTestClient(t, Config{URL: srv.URL, Timeout: DefaultTimeout})
	for range 3 {
		if _, err := c.ConfigDump(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n := clk.Pending(); n != 0 {
		t.Errorf("%d deadlines armed after the calls, want none", n)
	}
}
