// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package retire

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Test doubles shared by the retire tests: telemetry fakes, resources,
// State Store handles, a deadline-recording ResponseWriter and a harness
// around a Holder on a fake clock.

func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// waitFor polls cond with a real-time bound; it waits only for goroutines
// the test or the Holder started, never for fake time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

// callLog records Binding calls in order across snapshots.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(s string) {
	l.mu.Lock()
	l.calls = append(l.calls, s)
	l.mu.Unlock()
}

func (l *callLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// fakeBinding logs "<name>.retire" and "<name>.release".
type fakeBinding struct {
	name     string
	log      *callLog
	retires  atomic.Int32
	releases atomic.Int32
	onRel    func()
}

func (b *fakeBinding) Retire() {
	b.retires.Add(1)
	b.log.add(b.name + ".retire")
}

func (b *fakeBinding) Release() {
	b.releases.Add(1)
	if b.onRel != nil {
		b.onRel()
	}
	b.log.add(b.name + ".release")
}

// fakeGauge keeps the last Set value.
type fakeGauge struct {
	v    atomic.Int64
	sets atomic.Int64
}

func (g *fakeGauge) Add(_ emit.Stripe, d int64) { g.v.Add(d) }
func (g *fakeGauge) Set(v int64)                { g.v.Store(v); g.sets.Add(1) }

// fakeCounter sums every Add.
type fakeCounter struct{ n atomic.Uint64 }

func (c *fakeCounter) Add(_ emit.Stripe, n uint64) { c.n.Add(n) }

// statusCall is one SetDegraded call.
type statusCall struct {
	reason catalog.Reason
	source string
	on     bool
}

// fakeStatus records degraded transitions.
type fakeStatus struct {
	mu    sync.Mutex
	calls []statusCall
}

func (s *fakeStatus) SetDegraded(r catalog.Reason, source string, on bool) {
	s.mu.Lock()
	s.calls = append(s.calls, statusCall{r, source, on})
	s.mu.Unlock()
}

func (s *fakeStatus) SetCleartextHops(catalog.Hop, int) {}

func (s *fakeStatus) get() []statusCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// fakeCloser is a snapshot resource counting its closes.
type fakeCloser struct {
	name   string
	closed atomic.Int32
	err    error
	onErr  func()
}

func (c *fakeCloser) Close() error {
	c.closed.Add(1)
	if c.onErr != nil {
		c.onErr()
	}
	return c.err
}

func (c *fakeCloser) isClosed() bool { return c.closed.Load() > 0 }

// fakeHandle is a snapshot.StoreHandle counting its releases.
type fakeHandle struct{ released atomic.Int32 }

func (*fakeHandle) Store() statestore.Store       { return nil }
func (*fakeHandle) Enqueuer() statestore.Enqueuer { return nil }
func (h *fakeHandle) Release()                    { h.released.Add(1) }

// newSnap returns an unpublished snapshot named name with its digest and a
// Binding logging into log (nil for none).
func newSnap(name string, log *callLog, resources ...io.Closer) *snapshot.Snapshot {
	content := []byte(`{"name":"` + name + `"}`)
	s := &snapshot.Snapshot{
		Revision:  revision.Revision{Digest: revision.Sum(content), Content: content},
		Resources: resources,
		Pins:      snapshot.NewPins(4),
	}
	if log != nil {
		s.Binding = &fakeBinding{name: name, log: log}
	}
	return s
}

func bindingOf(s *snapshot.Snapshot) *fakeBinding { return s.Binding.(*fakeBinding) }

// harness is a Holder on a fake clock with recording telemetry.
type harness struct {
	t      *testing.T
	clock  *clocktest.Fake
	h      *Holder
	gauge  *fakeGauge
	ended  *fakeCounter
	status *fakeStatus
	log    *callLog
	cancel context.CancelFunc
	done   chan error
}

// newHarness returns a harness; run starts Run and stops it at cleanup.
func newHarness(t *testing.T, cfg Config, run bool) *harness {
	t.Helper()
	x := &harness{
		t:      t,
		clock:  clocktest.New(epoch()),
		gauge:  &fakeGauge{},
		ended:  &fakeCounter{},
		status: &fakeStatus{},
		log:    &callLog{},
	}
	if cfg.Clock == nil {
		cfg.Clock = x.clock
	}
	cfg.RetiredSnapshots = x.gauge
	cfg.RetirementEnded = x.ended
	cfg.Status = x.status
	if cfg.Stripes == 0 {
		cfg.Stripes = 4
	}
	x.h = New(cfg)
	if run {
		ctx, cancel := context.WithCancel(context.Background())
		x.cancel = cancel
		x.done = make(chan error, 1)
		go func() { x.done <- x.h.Run(ctx) }()
		t.Cleanup(x.stop)
	}
	return x
}

// stop ends Run and waits for it.
func (x *harness) stop() {
	if x.cancel == nil {
		return
	}
	x.cancel()
	select {
	case err := <-x.done:
		if err != nil {
			x.t.Errorf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		x.t.Error("Run did not return")
	}
	x.cancel = nil
}

// publish publishes s and fails the test on error.
func (x *harness) publish(s *snapshot.Snapshot) {
	x.t.Helper()
	if _, err := x.h.Publish(s); err != nil {
		x.t.Fatalf("Publish(%s): %v", s.Revision.Digest.Short(), err)
	}
}

// pinned is one pinned request of a test.
type pinned struct {
	s       *snapshot.Snapshot
	stripe  emit.Stripe
	rec     *snapshot.PinnedRequest
	cancels *atomic.Int32
}

// pin pins the active snapshot with a fresh record whose Cancel counts.
func (x *harness) pin(stripe emit.Stripe) pinned {
	x.t.Helper()
	var n atomic.Int32
	rec := &snapshot.PinnedRequest{Cancel: func() { n.Add(1) }}
	s := x.h.Pin(stripe, rec)
	if s == nil {
		x.t.Fatal("Pin returned nil")
	}
	return pinned{s: s, stripe: stripe, rec: rec, cancels: &n}
}

func (x *harness) unpin(p pinned) { x.h.Unpin(p.s, p.stripe, p.rec) }

// ended reports the record's end reason under its mutex.
func ended(rec *snapshot.PinnedRequest) snapshot.EndReason {
	rec.Mu.Lock()
	defer rec.Mu.Unlock()
	return rec.Ended
}

// state returns the retirer state of s, or -1 when s is freed.
func (x *harness) state(s *snapshot.Snapshot) int {
	x.h.mu.Lock()
	defer x.h.mu.Unlock()
	if t := x.h.findLocked(s); t != nil {
		return int(t.state)
	}
	return -1
}

// waitState waits until s reaches st (-1: freed).
func (x *harness) waitState(s *snapshot.Snapshot, st int) {
	x.t.Helper()
	waitFor(x.t, "snapshot state", func() bool { return x.state(s) == st })
}

// idle waits until no Holder task runs.
func (x *harness) idle() {
	x.t.Helper()
	waitFor(x.t, "holder tasks", func() bool {
		x.h.mu.Lock()
		defer x.h.mu.Unlock()
		return x.h.tasks == 0
	})
}

const freed = -1

// deadlineWriter is a ResponseWriter recording deadlines through
// http.ResponseController.
type deadlineWriter struct {
	mu            sync.Mutex
	read, write   []time.Time
	header        http.Header
	panicOnRead   bool
	notSupported  bool
	writtenStatus int
}

func (w *deadlineWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (*deadlineWriter) Write(b []byte) (int, error) { return len(b), nil }

func (w *deadlineWriter) WriteHeader(code int) { w.writtenStatus = code }

func (w *deadlineWriter) SetReadDeadline(t time.Time) error {
	if w.panicOnRead {
		panic("handler finished")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.read = append(w.read, t)
	if w.notSupported {
		return http.ErrNotSupported
	}
	return nil
}

func (w *deadlineWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.write = append(w.write, t)
	return nil
}

func (w *deadlineWriter) deadlines() (read, write []time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.read), slices.Clone(w.write)
}

// logRecords captures log records for assertions: each message, and each
// message followed by " (<operation>)" when the record has an operation
// attribute.
type logRecords struct {
	mu   sync.Mutex
	msgs []string
}

func (l *logRecords) Enabled(context.Context, slog.Level) bool { return true }

func (l *logRecords) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, r.Message)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "operation" {
			l.msgs = append(l.msgs, r.Message+" ("+a.Value.String()+")")
		}
		return true
	})
	return nil
}

func (l *logRecords) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecords) WithGroup(string) slog.Handler      { return l }

func (l *logRecords) has(msg string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.msgs, msg)
}

var errCloseFailed = errors.New("close failed")
