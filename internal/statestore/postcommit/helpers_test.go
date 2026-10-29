// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package postcommit

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// epoch is the fake clock's start.
func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// counter is an allocation-free emit.Counter.
type counter struct{ n atomic.Uint64 }

func (c *counter) Add(_ emit.Stripe, n uint64) { c.n.Add(n) }

// gauge is an emit.Gauge summing every stripe.
type gauge struct{ v atomic.Int64 }

func (g *gauge) Add(_ emit.Stripe, d int64) { g.v.Add(d) }
func (g *gauge) Set(v int64)                { g.v.Store(v) }

// metrics holds the queue's handles.
type metrics struct {
	dropped [emit.NumWriteKinds]counter
	depth   gauge
}

func (m *metrics) handles() *emit.StateMetrics {
	h := &emit.StateMetrics{QueueItems: &m.depth}
	for k := range emit.NumWriteKinds {
		h.WritesDropped[k] = &m.dropped[k]
	}
	return h
}

// drops returns the dropped writes of op.
func (m *metrics) drops(op statestore.OpKind) uint64 { return m.dropped[op.WriteLabel()].n.Load() }

// logRecorder keeps every log record.
type logRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, r.Message)
	return nil
}
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecorder) WithGroup(string) slog.Handler      { return l }

func (l *logRecorder) has(msg string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.msgs {
		if m == msg {
			return true
		}
	}
	return false
}

// target is a fake Target. answer, when set, decides each write's
// outcome; nil answers every write as applied. block, when set, holds
// every Write until it is closed or ctx ends.
type target struct {
	answer func(w *statestore.Write)
	block  chan struct{}

	refs    atomic.Int64
	closed  atomic.Bool
	count   atomic.Uint64 // writes received
	entered chan struct{} // receives once per Write call, when set

	mu      sync.Mutex
	batches [][]string
}

func newTarget() *target { return &target{} }

func (t *target) Write(ctx context.Context, batch []*statestore.Write) {
	if t.entered != nil {
		t.entered <- struct{}{}
	}
	if t.block != nil {
		select {
		case <-t.block:
		case <-ctx.Done():
			for _, w := range batch {
				w.Err = statestore.ErrNotAttempted
			}
			t.record(batch)
			return
		}
	}
	for _, w := range batch {
		if t.answer != nil {
			t.answer(w)
		} else {
			w.Applied = true
		}
	}
	t.record(batch)
}

func (t *target) record(batch []*statestore.Write) {
	ids := make([]string, len(batch))
	for i, w := range batch {
		ids[i] = idOf(w)
		t.count.Add(1)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.batches = append(t.batches, ids)
}

func (t *target) Hold() bool {
	if t.closed.Load() {
		return false
	}
	t.refs.Add(1)
	return true
}

func (t *target) Release() {
	if t.refs.Add(-1) < 0 {
		panic("target released more often than held")
	}
}

// written returns every written id in order, a retried id twice.
func (t *target) written() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, b := range t.batches {
		out = append(out, b...)
	}
	return out
}

// batchList returns the ids of every Write call's batch.
func (t *target) batchList() [][]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.batches)
}

// idOf names a test write by its kind and the id test writes carry.
func idOf(w *statestore.Write) string {
	switch w.Kind {
	case statestore.OpRefund:
		return w.Refund.Name
	case statestore.OpCacheInvalidate:
		return "inv" + strconv.Itoa(int(w.Invalidate[0]))
	default:
		return w.Cache.Names
	}
}

// refund returns a refund write named id.
func refund(id string) *statestore.Write {
	return &statestore.Write{Kind: statestore.OpRefund, Timeout: time.Second, Refund: statestore.Refund{Name: id, Window: time.Minute}}
}

// store returns a cache store write named id with an entry of n bytes.
func store(id string, n int) *statestore.Write {
	return &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second, Cache: statestore.CacheStore{Names: id, Entry: make([]byte, n)}}
}

// invalidation returns an invalidation whose digest starts with i.
func invalidation(i byte) *statestore.Write {
	w := &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: time.Second}
	w.Invalidate[0] = i
	return w
}

// testQueue returns a queue without writers, its metrics, a fake clock
// and a log recorder.
func testQueue(t *testing.T, lim Limits) (*Queue, *metrics, *clocktest.Fake, *logRecorder) {
	t.Helper()
	m := &metrics{}
	clk := clocktest.New(epoch())
	logs := &logRecorder{}
	q := newQueue(statestore.Deps{Clock: clk, Metrics: m.handles(), Logger: slog.New(logs)}, lim)
	return q, m, clk, logs
}

// startedQueue is testQueue with the writers running; cleanup closes it.
func startedQueue(t *testing.T, lim Limits) (*Queue, *metrics, *logRecorder) {
	t.Helper()
	q, m, _, logs := testQueue(t, lim)
	q.start(t.Context())
	// t.Context is canceled before cleanups run: Close skips the flush.
	t.Cleanup(func() { _ = q.Close(t.Context()) })
	return q, m, logs
}

// waitFor polls cond with a real-time bound; it waits only for goroutines
// the test started, never for fake time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// small returns limits of n items and plenty of bytes per class.
func small(n, batch, writers int) Limits {
	l := Limits{Writers: writers, Batch: batch}
	for i := range l.Classes {
		l.Classes[i] = ClassLimit{Items: n, Bytes: 1 << 30}
	}
	return l
}
