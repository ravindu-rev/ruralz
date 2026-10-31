// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package postcommit

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Fixed sizes (targets of "Post-commit write queue"; fixed by
// OQ-scalability-and-distributed-state-4 (a)).
const (
	// NumClasses is the number of classes, statestore.ClassSettle to
	// statestore.ClassPlugin.
	NumClasses = 4
	// DefaultWriters is the number of writer goroutines per Node.
	DefaultWriters = 8
	// DefaultBatch is the most writes in one batch, and the largest
	// Limits.Batch.
	DefaultBatch = 64
	// DefaultFlush bounds the flush at Drain.
	DefaultFlush = 5 * time.Second
	// Turns is the length of the turn schedule.
	Turns = 15
)

// ClassLimit caps one class.
type ClassLimit struct {
	// Items is the most queued writes.
	Items int
	// Bytes is the most retained memory of the queued writes (Size).
	Bytes int64
}

// Limits are the queue's fixed sizes; only tests change them. Zero fields
// take the DefaultLimits value.
type Limits struct {
	// Classes holds the caps indexed by class - 1.
	Classes [NumClasses]ClassLimit
	// Writers is the number of writer goroutines.
	Writers int
	// Batch is the most writes per batch, at most DefaultBatch.
	Batch int
	// Flush bounds Close's flush.
	Flush time.Duration
}

// DefaultLimits returns the targets: 64,000 items and 85 MiB in all.
func DefaultLimits() Limits {
	return Limits{
		Classes: [NumClasses]ClassLimit{
			{Items: 32_000, Bytes: 16 << 20}, // settle
			{Items: 6_400, Bytes: 1 << 20},   // invalidate, the reserved 10%
			{Items: 16_000, Bytes: 64 << 20}, // store
			{Items: 9_600, Bytes: 4 << 20},   // plugin
		},
		Writers: DefaultWriters, Batch: DefaultBatch, Flush: DefaultFlush,
	}
}

// withDefaults fills zero fields from DefaultLimits and clamps Batch to
// DefaultBatch, which a batch's bit set relies on.
func (l Limits) withDefaults() Limits {
	def := DefaultLimits()
	for i := range l.Classes {
		if l.Classes[i].Items <= 0 {
			l.Classes[i].Items = def.Classes[i].Items
		}
		if l.Classes[i].Bytes <= 0 {
			l.Classes[i].Bytes = def.Classes[i].Bytes
		}
	}
	if l.Writers <= 0 {
		l.Writers = def.Writers
	}
	if l.Batch <= 0 || l.Batch > DefaultBatch {
		l.Batch = def.Batch
	}
	if l.Flush <= 0 {
		l.Flush = def.Flush
	}
	return l
}

// ItemOverhead returns the fixed retained memory of one queued write: the
// statestore.Write the queue holds and its slot in the class ring, read
// as Go type sizes with reflect.
func ItemOverhead() int64 {
	n := reflect.TypeOf((*statestore.Write)(nil)).Elem().Size() + reflect.TypeOf((*item)(nil)).Elem().Size()
	return int64(n) //nolint:gosec // G115: two Go type sizes, far below 2^63
}

// Size returns the retained memory w counts against its class's byte cap
// ("Post-commit write queue"): ItemOverhead plus the bytes its key and
// payload hold, a refund's Quota name, a store's Vary names and entry, and
// a lease's entry. Byte slices count by capacity, the memory they keep.
func Size(w *statestore.Write) int64 {
	return ItemOverhead() + int64(len(w.Refund.Name)+len(w.Cache.Names)+cap(w.Cache.Entry)+cap(w.Lease.Entry))
}

// Target is the State Store a Binding's writes go to.
type Target interface {
	// Write sends one post-commit batch (statestore.Store.Write).
	Write(ctx context.Context, batch []*statestore.Write)
	// Hold takes a reference that keeps the target open while one write is
	// queued or being written; false means the target is closed.
	Hold() bool
	// Release returns a reference Hold took.
	Release()
}

// Binding enqueues writes for one Target; it implements
// statestore.Enqueuer.
type Binding struct {
	q *Queue
	t Target
}

var _ statestore.Enqueuer = (*Binding)(nil)

// Enqueue queues w in class for the Binding's Target, never blocking. The
// queue owns w until it is written or dropped, so the caller must not touch
// it after true. False means dropped and counted: a full or invalid class,
// a closed queue or a closed Target. A nil w or a kind that is not a
// post-commit write is refused uncounted.
func (b *Binding) Enqueue(class statestore.WriteClass, w *statestore.Write) bool {
	return b.q.enqueue(b, class, w)
}

// item is one queued write.
type item struct {
	b     *Binding
	w     *statestore.Write
	size  int64
	retry bool // an invalidation already written once
}

// class is one bounded ring of writes.
type class struct {
	mu    sync.Mutex
	buf   []item
	head  int
	n     int
	bytes int64
	limit int64
	count atomic.Int64 // n, read without the lock
}

// push appends it unless the queue is closed or the class is full.
func (c *class) push(it item, closed *atomic.Bool, depth emit.Gauge) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if closed.Load() || c.n == len(c.buf) || it.size > c.limit-c.bytes {
		return false
	}
	c.buf[(c.head+c.n)%len(c.buf)] = it
	c.n++
	c.bytes += it.size
	c.count.Store(int64(c.n))
	gaugeAdd(depth, 1)
	return true
}

// pop moves up to len(dst) writes, oldest first, into dst.
func (c *class) pop(dst []item, depth emit.Gauge) int {
	if c.count.Load() == 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := min(c.n, len(dst))
	for i := range n {
		j := (c.head + i) % len(c.buf)
		dst[i] = c.buf[j]
		c.buf[j] = item{}
		c.bytes -= dst[i].size
	}
	if n > 0 {
		c.head = (c.head + n) % len(c.buf)
		c.n -= n
		c.count.Store(int64(c.n))
		gaugeAdd(depth, -int64(n))
	}
	return n
}

// Queue is a Node's post-commit write queue. It is safe for concurrent
// use.
type Queue struct {
	clk     clock.Clock
	metrics *emit.StateMetrics
	log     *slog.Logger
	lim     Limits

	classes [NumClasses]class
	turn    atomic.Uint64
	// pending counts queued writes plus those a writer holds.
	pending  atomic.Int64
	flushing atomic.Bool
	// closed is set before Close drains the classes; push reads it under
	// the class lock, so nothing is queued after the drain.
	closed atomic.Bool
	// closing makes Close run once; finished closes when it returns.
	closing  atomic.Bool
	finished chan struct{}

	wake chan struct{} // work queued
	idle chan struct{} // pending reached 0 while flushing
	stop context.CancelFunc
	wg   sync.WaitGroup
}

// New returns a queue and starts its writers, which Close stops. ctx
// supplies values only: the writers outlive its cancellation. deps gives
// the clock, the State Store metrics and the logger; zero Limits fields
// take the defaults.
func New(ctx context.Context, deps statestore.Deps, lim Limits) *Queue {
	q := newQueue(deps, lim)
	q.start(ctx)
	return q
}

// newQueue returns a queue without writers.
func newQueue(deps statestore.Deps, lim Limits) *Queue {
	lim = lim.withDefaults()
	clk := deps.Clock
	if clk == nil {
		clk = clock.Real()
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	q := &Queue{
		clk: clk, metrics: deps.Metrics, log: logger, lim: lim,
		finished: make(chan struct{}), wake: make(chan struct{}, 1), idle: make(chan struct{}, 1),
	}
	for i := range q.classes {
		q.classes[i].buf = make([]item, lim.Classes[i].Items)
		q.classes[i].limit = lim.Classes[i].Bytes
	}
	return q
}

// start launches the writers.
func (q *Queue) start(ctx context.Context) {
	wctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	q.stop = stop
	for range q.lim.Writers {
		q.wg.Go(func() { q.run(wctx) })
	}
}

// Bind returns the Enqueuer of writes for t.
func (q *Queue) Bind(t Target) *Binding { return &Binding{q: q, t: t} }

// Len returns the number of queued writes, those being written excluded.
func (q *Queue) Len() int {
	var n int64
	for i := range q.classes {
		n += q.classes[i].count.Load()
	}
	return int(n)
}

// depth returns the ruralz_state_write_queue_items handle, or nil.
func (q *Queue) depth() emit.Gauge {
	if q.metrics == nil {
		return nil
	}
	return q.metrics.QueueItems
}

// gaugeAdd adds d to g when it is set.
func gaugeAdd(g emit.Gauge, d int64) {
	if g != nil {
		g.Add(0, d)
	}
}

// dropped counts one dropped write of kind label (a statestore.OpKind
// WriteLabel).
func (q *Queue) dropped(label int) {
	if q.metrics == nil || label < 0 || label >= emit.NumWriteKinds {
		return
	}
	if c := q.metrics.WritesDropped[label]; c != nil {
		c.Add(0, 1)
	}
}

// signal wakes one idle writer.
func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// done ends n pending writes and wakes a flushing Close at zero.
func (q *Queue) done(n int64) {
	if q.pending.Add(-n) == 0 && q.flushing.Load() {
		select {
		case q.idle <- struct{}{}:
		default:
		}
	}
}

// enqueue is Binding.Enqueue.
func (q *Queue) enqueue(b *Binding, cl statestore.WriteClass, w *statestore.Write) bool {
	if w == nil {
		return false
	}
	label := w.Kind.WriteLabel()
	if label < 0 {
		return false
	}
	if cl < statestore.ClassSettle || cl > statestore.ClassPlugin || q.closed.Load() || !b.t.Hold() {
		q.dropped(label)
		return false
	}
	q.pending.Add(1)
	if !q.classes[cl-1].push(item{b: b, w: w, size: Size(w)}, &q.closed, q.depth()) {
		q.done(1)
		b.t.Release()
		q.dropped(label)
		return false
	}
	q.signal()
	return true
}

// Close flushes queued writes until none is left, the flush bound (5 s)
// passes or ctx ends, then stops the writers and drops what is still
// queued. Later Enqueue calls drop. It returns ctx's error when ctx ended
// the flush; later calls wait for the first and return nil.
func (q *Queue) Close(ctx context.Context) error {
	if !q.closing.CompareAndSwap(false, true) {
		select {
		case <-q.finished:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("postcommit: close: %w", ctx.Err())
		}
	}
	defer close(q.finished)
	err := q.flush(ctx)
	q.closed.Store(true)
	if q.stop != nil {
		q.stop()
	}
	q.wg.Wait()
	if n := q.discard(); n > 0 {
		q.log.WarnContext(ctx, "post-commit queue closed with writes left; dropped", slog.Int("writes", n))
	}
	return err
}

// flush waits until no write is pending, the flush bound passes or ctx
// ends.
func (q *Queue) flush(ctx context.Context) error {
	q.flushing.Store(true)
	if q.pending.Load() == 0 {
		return nil
	}
	t := q.clk.NewTimer(q.lim.Flush)
	defer t.Stop()
	for q.pending.Load() > 0 {
		select {
		case <-q.idle:
		case <-t.C():
			return nil
		case <-ctx.Done():
			return fmt.Errorf("postcommit: flush: %w", ctx.Err())
		}
	}
	return nil
}

// discard drops every queued write after the writers stopped and returns
// how many it dropped. Targets are released outside the class locks.
func (q *Queue) discard() int {
	var buf [DefaultBatch]item
	total := 0
	for i := range q.classes {
		for {
			n := q.classes[i].pop(buf[:], q.depth())
			if n == 0 {
				break
			}
			for j := range n {
				q.dropped(buf[j].w.Kind.WriteLabel())
				buf[j].b.t.Release()
			}
			clear(buf[:n])
			q.done(int64(n))
			total += n
		}
	}
	return total
}
