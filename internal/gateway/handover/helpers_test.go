// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package handover

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
)

// bound is the real-time bound of every wait on goroutines; it only turns
// a hang into a failure.
const bound = 10 * time.Second

// waitFor polls cond with a real-time bound; it waits only for goroutines,
// never for fake time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(bound)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

// recv waits for one value from ch with a real-time bound.
func recv[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(bound):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// newClock returns a fake clock that starts at the real time.
func newClock() *clocktest.Fake { return clocktest.New(clock.Real().Now()) }

// waitArmed waits until clk holds n armed timers.
func waitArmed(t *testing.T, clk *clocktest.Fake, n int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d armed timers", n), func() bool { return clk.Pending() == n })
}

// dataDir opens a data directory under a short temporary path, since a
// Unix socket path holds at most 107 bytes.
func dataDir(t *testing.T) *nodedir.Dir {
	t.Helper()
	root, err := os.MkdirTemp("", "rzho")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	d, err := nodedir.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// takeLock takes the data-directory lock, released at the end of the test.
func takeLock(t *testing.T, d *nodedir.Dir) *nodedir.Lock {
	t.Helper()
	nl, err := d.TryLock()
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	t.Cleanup(func() { _ = nl.Release() })
	return nl
}

// logBuffer collects log records as text.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// has reports whether a record contains every part.
func (b *logBuffer) has(parts ...string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for l := range strings.SplitSeq(b.buf.String(), "\n") {
		if !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(l, p) }) {
			return true
		}
	}
	return false
}

func newLogger(b *logBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// events records what the fakes were asked to do, in order.
type events struct {
	mu   sync.Mutex
	list []string
}

func (e *events) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, s)
}

func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.list)
}

// count returns how many events equal s.
func (e *events) count(s string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, x := range e.list {
		if x == s {
			n++
		}
	}
	return n
}

// fakeDrainer is the holder's or new process's Drain.
type fakeDrainer struct {
	ev      *events
	started chan struct{}
	once    sync.Once
	mu      sync.Mutex
	held    bool
	// refuseHold makes Hold fail as if a Drain had just started.
	refuseHold bool
}

func newFakeDrainer(ev *events) *fakeDrainer {
	return &fakeDrainer{ev: ev, started: make(chan struct{})}
}

func (d *fakeDrainer) Started() <-chan struct{} { return d.started }

// start starts the Drain, as a stop signal does.
func (d *fakeDrainer) start() { d.once.Do(func() { d.ev.add("drain:start"); close(d.started) }) }

func (d *fakeDrainer) Hold() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refuseHold {
		d.ev.add("drain:hold refused")
		return false
	}
	select {
	case <-d.started:
		d.ev.add("drain:hold refused")
		return false
	default:
	}
	d.held = true
	d.ev.add("drain:hold")
	return true
}

func (d *fakeDrainer) Handover() {
	d.mu.Lock()
	d.held = false
	d.mu.Unlock()
	d.ev.add("drain:handover")
	d.once.Do(func() { close(d.started) })
}

func (d *fakeDrainer) Resume() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.held = false
	d.ev.add("drain:resume")
}

// fakeActivations is the holder's activation loader.
type fakeActivations struct {
	ev *events
	mu sync.Mutex
	// revs is what Hold returns.
	revs Revisions
	// busy, when set, is an activation in progress: Hold waits until it
	// is closed.
	busy chan struct{}
	held bool
}

func (a *fakeActivations) set(r Revisions) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.revs = r
}

func (a *fakeActivations) Hold(ctx context.Context) (Revisions, error) {
	a.mu.Lock()
	busy := a.busy
	a.mu.Unlock()
	if busy != nil {
		select {
		case <-busy:
		case <-ctx.Done():
			a.ev.add("activations:hold ended")
			return Revisions{}, ctx.Err()
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.held {
		return Revisions{}, errors.New("fake activations: held twice")
	}
	a.held = true
	a.ev.add("activations:hold")
	return a.revs, nil
}

func (a *fakeActivations) Release() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.held = false
	a.ev.add("activations:release")
}

func (a *fakeActivations) isHeld() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.held
}

// fakeSockets is one of the holder's socket groups.
type fakeSockets struct {
	ev   *events
	name string
	// lingering, when set, makes WaitLingers wait until it is closed.
	lingering chan struct{}
}

func (s *fakeSockets) WaitLingers(ctx context.Context) error {
	if s.lingering == nil {
		return nil
	}
	s.ev.add(s.name + ":wait")
	select {
	case <-s.lingering:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *fakeSockets) SteerToSuccessor() error { s.ev.add(s.name + ":steer"); return nil }
func (s *fakeSockets) Pin() error              { s.ev.add(s.name + ":pin"); return nil }
func (s *fakeSockets) StopAccepting()          { s.ev.add(s.name + ":stop") }

// fakeAdmin is the holder's admin server.
type fakeAdmin struct{ ev *events }

func (a *fakeAdmin) DisableKeepAlives() { a.ev.add("admin:keepalives off") }
func (a *fakeAdmin) EnableKeepAlives()  { a.ev.add("admin:keepalives on") }

// fakeBound is the new process's bound sockets.
type fakeBound struct {
	ev *events
	// gate, when set, makes Listen wait until it is closed.
	gate chan struct{}
	err  error
}

func (b *fakeBound) Listen(context.Context) error {
	if b.gate != nil {
		<-b.gate
	}
	b.ev.add("bound:listen")
	return b.err
}

func (b *fakeBound) Close() error { b.ev.add("bound:close"); return nil }

// fakePinner is one of the new process's socket groups.
type fakePinner struct {
	ev   *events
	name string
}

func (p *fakePinner) Pin() error { p.ev.add(p.name + ":pin"); return nil }

// fakeNotifier records MAINPID=, failing the first fails sends.
type fakeNotifier struct {
	ev    *events
	mu    sync.Mutex
	fails int
}

func (n *fakeNotifier) MainPID(_ context.Context, pid int) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.fails > 0 {
		n.fails--
		n.ev.add("notify:failed")
		return errors.New("fake notifier: send failed")
	}
	n.ev.add(fmt.Sprintf("notify:mainpid %d", pid))
	return nil
}

// wire is one end of a handover connection the test drives by hand.
type wire struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader
}

func newWire(t *testing.T, c net.Conn) *wire {
	t.Helper()
	t.Cleanup(func() { _ = c.Close() })
	return &wire{t: t, c: c, br: bufio.NewReaderSize(c, MaxMessage+1)}
}

// dialSocket connects to the holder's socket.
func dialSocket(t *testing.T, path string) *wire {
	t.Helper()
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "unix", path)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	return newWire(t, c)
}

// send writes v as one line.
func (w *wire) send(v any) {
	w.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		w.t.Fatal(err)
	}
	w.sendRaw(string(b))
}

// sendRaw writes s and a newline.
func (w *wire) sendRaw(s string) {
	w.t.Helper()
	if _, err := w.c.Write([]byte(s + "\n")); err != nil {
		w.t.Fatalf("write: %v", err)
	}
}

// read reads one message.
func (w *wire) read() inbound {
	w.t.Helper()
	m, err := w.tryRead()
	if err != nil {
		w.t.Fatalf("read: %v", err)
	}
	return m
}

// tryRead reads one message or returns the read error.
func (w *wire) tryRead() (inbound, error) {
	if err := w.c.SetReadDeadline(time.Now().Add(bound)); err != nil {
		return inbound{}, err
	}
	b, err := w.br.ReadBytes('\n')
	if err != nil {
		return inbound{}, err
	}
	return decode(bytes.TrimSuffix(b, []byte("\n")))
}

// expect reads one message and checks its type.
func (w *wire) expect(typ string) inbound {
	w.t.Helper()
	m := w.read()
	if m.Type != typ {
		w.t.Fatalf("message %+v, want type %q", m, typ)
	}
	return m
}

// closed reports whether the other end closed the connection: a read
// returns EOF or a reset, never a message.
func (w *wire) closed() bool {
	w.t.Helper()
	_, err := w.tryRead()
	return err != nil && (errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || isReset(err))
}

// isReset reports a connection reset.
func isReset(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && !op.Timeout()
}

// readyFor returns the ready message of a new process with active
// Revision d, zero for none.
func readyFor(d revision.Digest) readyWire {
	return readyMessage(ready{Version: "0.1.0-test", PID: 4242, Markers: Markers(), ActiveDigest: d})
}
