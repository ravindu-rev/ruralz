// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package postcommit

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Class shorthands.
const (
	settleC = statestore.ClassSettle
	invC    = statestore.ClassInvalidate
	storeC  = statestore.ClassStore
	pluginC = statestore.ClassPlugin
)

// TestDefaultLimits checks the fixed sizes of "Post-commit write queue":
// 32,000 items and 16 MiB for settlement, 6,400 and 1 MiB for
// invalidations (the reserved 10%), 16,000 and 64 MiB for stores and
// 9,600 and 4 MiB for Plugin writes, 64,000 items and 85 MiB in all,
// eight writers, 64 writes per batch and a 5 s flush at Drain.
func TestDefaultLimits(t *testing.T) {
	l := DefaultLimits()
	want := [NumClasses]ClassLimit{{32_000, 16 << 20}, {6_400, 1 << 20}, {16_000, 64 << 20}, {9_600, 4 << 20}}
	if l.Classes != want {
		t.Errorf("DefaultLimits().Classes = %v, want %v", l.Classes, want)
	}
	items, bytes := 0, int64(0)
	for _, c := range l.Classes {
		items += c.Items
		bytes += c.Bytes
	}
	if items != 64_000 || bytes != 85<<20 {
		t.Errorf("totals = %d items, %d bytes; want 64,000 and 85 MiB", items, bytes)
	}
	if l.Classes[invC-1].Items*10 != items {
		t.Errorf("invalidations hold %d of %d items, not the reserved 10%%", l.Classes[invC-1].Items, items)
	}
	if l.Writers != 8 || l.Batch != 64 || l.Flush != 5*time.Second {
		t.Errorf("writers %d, batch %d, flush %v; want 8, 64, 5s", l.Writers, l.Batch, l.Flush)
	}
	if DefaultLimits() != l {
		t.Error("DefaultLimits is not a pure function")
	}
}

// TestLimitsWithDefaults checks that zero fields take the defaults and a
// batch above 64 is clamped, since a batch is one 64-bit set.
func TestLimitsWithDefaults(t *testing.T) {
	if got := (Limits{}).withDefaults(); got != DefaultLimits() {
		t.Errorf("zero Limits = %+v, want the defaults", got)
	}
	l := Limits{Batch: 65, Writers: 2}
	l.Classes[storeC-1] = ClassLimit{Items: 3}
	got := l.withDefaults()
	if got.Batch != DefaultBatch || got.Writers != 2 || got.Classes[storeC-1] != (ClassLimit{3, 64 << 20}) {
		t.Errorf("withDefaults = %+v", got)
	}
}

// TestTurnSchedule checks the drain turns: settlement first and
// invalidations second in every round of three, stores third in four
// rounds of five and Plugin writes third in the fifth, so 15 turns give
// Plugin writes one batch.
func TestTurnSchedule(t *testing.T) {
	want := []statestore.WriteClass{
		settleC, invC, storeC, settleC, invC, storeC, settleC, invC, storeC,
		settleC, invC, storeC, settleC, invC, pluginC,
	}
	for turn := range uint64(3 * Turns) {
		if got := classAt(turn); got != want[turn%Turns] {
			t.Errorf("classAt(%d) = %d, want %d", turn, got, want[turn%Turns])
		}
	}
}

// fill enqueues n writes made by mk into class cl through b, failing the
// test on a refusal.
func fill(t *testing.T, b *Binding, cl statestore.WriteClass, n int, mk func(i int) *statestore.Write) {
	t.Helper()
	for i := range n {
		if !b.Enqueue(cl, mk(i)) {
			t.Fatalf("Enqueue(%d, write %d) refused", cl, i)
		}
	}
}

// TestSaturatedTurnOrder checks that, with every class full, batches
// follow the schedule exactly, so an invalidation waits at most one
// settlement batch plus the batch in progress: between two invalidation
// turns there is one settlement turn and one other.
func TestSaturatedTurnOrder(t *testing.T) {
	q, _, _, _ := testQueue(t, small(64, 1, 1))
	b := q.Bind(newTarget())
	for _, cl := range []statestore.WriteClass{settleC, invC, storeC, pluginC} {
		fill(t, b, cl, 60, func(i int) *statestore.Write { return refund(strconv.Itoa(int(cl)) + "-" + strconv.Itoa(i)) })
	}
	var got []statestore.WriteClass
	dst := make([]item, 1)
	for range 3 * Turns {
		n, cl := q.next(dst)
		if n != 1 {
			t.Fatalf("next = %d writes, want 1", n)
		}
		got = append(got, cl)
	}
	var turn uint64
	for _, cl := range got {
		if cl != classAt(turn) {
			t.Fatalf("turn %d took class %d, want %d (order %v)", turn, cl, classAt(turn), got)
		}
		turn++
	}
	last := -1
	for i, cl := range got {
		if cl != invC {
			continue
		}
		if last >= 0 {
			between := got[last+1 : i]
			if len(between) != 2 || slices.Index(between, settleC) < 0 {
				t.Errorf("between invalidation turns %d and %d: %v, want one settlement and one other", last, i, between)
			}
		}
		last = i
	}
}

// TestNextSkipsEmptyClasses checks that a writer skips empty classes in
// turn order, wrapping around the schedule, and finds nothing when every
// class is empty.
func TestNextSkipsEmptyClasses(t *testing.T) {
	tests := []struct {
		name  string
		class statestore.WriteClass
		turn  uint64
	}{
		{"settlement turn serves invalidations", invC, 0},
		{"Plugin writes found at the last turn", pluginC, 0},
		{"Plugin turn wraps to stores", storeC, Turns - 1},
		{"store turn wraps to settlement", settleC, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, _, _, _ := testQueue(t, small(8, 8, 1))
			fill(t, q.Bind(newTarget()), tt.class, 2, func(i int) *statestore.Write { return refund(strconv.Itoa(i)) })
			q.turn.Store(tt.turn)
			dst := make([]item, 8)
			if n, cl := q.next(dst); n != 2 || cl != tt.class {
				t.Errorf("next = %d writes of class %d, want 2 of class %d", n, cl, tt.class)
			}
			if n, _ := q.next(dst); n != 0 {
				t.Errorf("next on an empty queue = %d writes", n)
			}
		})
	}
}

// TestBatchesOldestFirst checks that a batch holds at most Batch writes
// of one class, oldest first.
func TestBatchesOldestFirst(t *testing.T) {
	q, _, _, _ := testQueue(t, small(200, 64, 1))
	fill(t, q.Bind(newTarget()), settleC, 100, func(i int) *statestore.Write { return refund(strconv.Itoa(i)) })
	dst := make([]item, 64)
	next := 0
	for _, want := range []int{64, 36, 0} {
		n, _ := q.next(dst)
		if n != want {
			t.Fatalf("next = %d writes, want %d", n, want)
		}
		for i := range n {
			if id := idOf(dst[i].w); id != strconv.Itoa(next) {
				t.Fatalf("write %d of the batch is %s, want %d", i, id, next)
			}
			next++
		}
	}
}

// TestFullClassDropsOnlyItsOwnWrites checks that a class at its item cap
// drops and counts its next write, releasing the Target reference, while
// the other classes keep accepting: stores saturating the queue never
// drop an invalidation (CE-16).
func TestFullClassDropsOnlyItsOwnWrites(t *testing.T) {
	q, m, _, _ := testQueue(t, small(4, 64, 1))
	tg := newTarget()
	b := q.Bind(tg)
	fill(t, b, storeC, 4, func(i int) *statestore.Write { return store(strconv.Itoa(i), 10) })
	if b.Enqueue(storeC, store("x", 10)) {
		t.Fatal("a full store class accepted a write")
	}
	if got := m.drops(statestore.OpCacheSet); got != 1 {
		t.Errorf("cache_set drops = %d, want 1", got)
	}
	if !b.Enqueue(invC, invalidation(1)) || !b.Enqueue(settleC, refund("r")) {
		t.Fatal("a full store class refused another class's write")
	}
	if got := m.drops(statestore.OpCacheInvalidate) + m.drops(statestore.OpRefund); got != 0 {
		t.Errorf("other kinds dropped %d writes", got)
	}
	if got := tg.refs.Load(); got != 6 {
		t.Errorf("target holds %d references, want 6 for the queued writes", got)
	}
}

// TestByteCapDropsWrites checks the byte cap of a class: a write whose
// retained memory would pass it is dropped and counted, a write that
// fills it exactly is queued, and popping frees its bytes.
func TestByteCapDropsWrites(t *testing.T) {
	a, c := store("a", 59), store("c", 39)
	l := small(100, 64, 1)
	l.Classes[storeC-1].Bytes = Size(a) + Size(c)
	q, m, _, _ := testQueue(t, l)
	b := q.Bind(newTarget())
	if !b.Enqueue(storeC, a) {
		t.Fatal("first store refused")
	}
	if b.Enqueue(storeC, store("b", 40)) {
		t.Fatal("a store over the byte cap was queued")
	}
	if !b.Enqueue(storeC, c) {
		t.Fatal("a store filling the cap exactly was refused")
	}
	if got := m.drops(statestore.OpCacheSet); got != 1 {
		t.Errorf("cache_set drops = %d, want 1", got)
	}
	if n, _ := q.next(make([]item, 1)); n != 1 {
		t.Fatal("next took nothing")
	}
	if !b.Enqueue(storeC, store("d", 59)) {
		t.Fatal("popping did not free the bytes")
	}
}

// TestItemOverhead checks the fixed retained memory of a queued write:
// the Go type size of the statestore.Write the queue holds plus that of
// its ring slot.
func TestItemOverhead(t *testing.T) {
	want := reflect.TypeOf(statestore.Write{}).Size() + reflect.TypeOf(item{}).Size()
	got := ItemOverhead()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ItemOverhead() = %d, want %d", got, want)
	}
	t.Logf("ItemOverhead() = %d bytes", got)
}

// TestSize checks the retained memory a write counts against its class:
// ItemOverhead plus its key and payload bytes, a refund's Quota name, a
// store's Vary names and entry and a lease's entry, byte slices by their
// capacity.
func TestSize(t *testing.T) {
	o := ItemOverhead()
	lease := &statestore.Write{Kind: statestore.OpCacheSet, Lease: statestore.CacheLease{Mode: statestore.LeaseRefresh, Entry: make([]byte, 7)}}
	spare := store("accept", 0)
	spare.Cache.Entry = make([]byte, 10, 4096)
	tests := []struct {
		name string
		w    *statestore.Write
		want int64
	}{
		{"refund", refund("checkout"), o + 8},
		{"store", store("accept", 100), o + 106},
		{"store entry by capacity", spare, o + 6 + 4096},
		{"lease", lease, o + 7},
		{"invalidation", invalidation(1), o},
	}
	for _, tt := range tests {
		if got := Size(tt.w); got != tt.want {
			t.Errorf("Size(%s) = %d, want %d", tt.name, got, tt.want)
		}
	}
}

// TestInvalidationBytesBindBeforeItems checks the default invalidation
// class: its 1 MiB of retained memory fills before its 6,400 items, so
// the byte cap is what bounds the reserved slice.
func TestInvalidationBytesBindBeforeItems(t *testing.T) {
	q, m, _, _ := testQueue(t, DefaultLimits())
	b := q.Bind(newTarget())
	lim := DefaultLimits().Classes[invC-1]
	n := 0
	for b.Enqueue(invC, invalidation(1)) {
		n++
		if n > lim.Items {
			t.Fatalf("queued more than the %d-item cap", lim.Items)
		}
	}
	if want := lim.Bytes / Size(invalidation(1)); int64(n) != want || n >= lim.Items {
		t.Errorf("queued %d invalidations, want %d: the 1 MiB cap before the %d-item cap", n, want, lim.Items)
	}
	if got := m.drops(statestore.OpCacheInvalidate); got != 1 {
		t.Errorf("cache_invalidate drops = %d, want 1", got)
	}
}

// TestEnqueueRefusals checks the refusals of Enqueue: a nil write or a
// kind that is not a post-commit write is refused uncounted; an invalid
// class, a closed Target and a closed queue drop the write and count it.
func TestEnqueueRefusals(t *testing.T) {
	q, m, _, _ := testQueue(t, small(8, 8, 1))
	tg := newTarget()
	b := q.Bind(tg)
	if b.Enqueue(settleC, nil) {
		t.Error("a nil write was queued")
	}
	if b.Enqueue(settleC, &statestore.Write{Kind: statestore.OpGCRA}) {
		t.Error("a GCRA call was queued as a write")
	}
	for _, cl := range []statestore.WriteClass{0, pluginC + 1} {
		if b.Enqueue(cl, refund("r")) {
			t.Errorf("class %d accepted a write", cl)
		}
	}
	if got := m.drops(statestore.OpRefund); got != 2 {
		t.Errorf("refund drops after invalid classes = %d, want 2", got)
	}
	tg.closed.Store(true)
	if b.Enqueue(settleC, refund("r")) {
		t.Error("a closed target accepted a write")
	}
	tg.closed.Store(false)
	if err := q.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if b.Enqueue(settleC, refund("r")) {
		t.Error("a closed queue accepted a write")
	}
	if got := m.drops(statestore.OpRefund); got != 4 {
		t.Errorf("refund drops = %d, want 4", got)
	}
	if got := tg.refs.Load(); got != 0 {
		t.Errorf("refused writes left %d target references", got)
	}
	if q.Len() != 0 {
		t.Errorf("Len = %d after refusals", q.Len())
	}
}

// TestDropPathAllocatesNothing checks that Enqueue allocates nothing when
// it drops a write.
func TestDropPathAllocatesNothing(t *testing.T) {
	q, _, _, _ := testQueue(t, small(1, 1, 1))
	b := q.Bind(newTarget())
	fill(t, b, settleC, 1, func(int) *statestore.Write { return refund("full") })
	w := refund("dropped")
	if allocs := testing.AllocsPerRun(100, func() { b.Enqueue(settleC, w) }); allocs != 0 {
		t.Errorf("a dropped Enqueue allocated %v times", allocs)
	}
}

// TestQueueDepth checks ruralz_state_write_queue_items: it counts queued
// writes, falls as writers take them and ends at 0.
func TestQueueDepth(t *testing.T) {
	q, m, _, _ := testQueue(t, small(8, 2, 1))
	b := q.Bind(newTarget())
	fill(t, b, settleC, 3, func(i int) *statestore.Write { return refund(strconv.Itoa(i)) })
	if got := m.depth.v.Load(); got != 3 || q.Len() != 3 {
		t.Fatalf("depth %d, Len %d; want 3", got, q.Len())
	}
	if n, _ := q.next(make([]item, 2)); n != 2 {
		t.Fatalf("next = %d", n)
	}
	if got := m.depth.v.Load(); got != 1 || q.Len() != 1 {
		t.Fatalf("depth %d, Len %d after a batch of 2; want 1", got, q.Len())
	}
}

// TestBatchPerTarget checks that one popped batch is written with one
// Store.Write per Target, in the order the Targets first appear, so a
// write always reaches the driver it was queued for.
func TestBatchPerTarget(t *testing.T) {
	q, _, _ := startedQueue(t, small(64, 64, 1))
	a, c := newTarget(), newTarget()
	a.block, a.entered = make(chan struct{}), make(chan struct{}, 8)
	ba, bc := q.Bind(a), q.Bind(c)
	if !ba.Enqueue(settleC, refund("first")) {
		t.Fatal("Enqueue refused")
	}
	<-a.entered // the only writer now holds "first"
	for i := range 4 {
		if !ba.Enqueue(settleC, refund("a"+strconv.Itoa(i))) || !bc.Enqueue(settleC, refund("c"+strconv.Itoa(i))) {
			t.Fatal("Enqueue refused")
		}
	}
	close(a.block)
	waitFor(t, "every write", func() bool { return q.pending.Load() == 0 })
	if got, want := a.batchList(), [][]string{{"first"}, {"a0", "a1", "a2", "a3"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("target a batches = %v, want %v", got, want)
	}
	if got, want := c.batchList(), [][]string{{"c0", "c1", "c2", "c3"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("target c batches = %v, want %v", got, want)
	}
	if a.refs.Load() != 0 || c.refs.Load() != 0 {
		t.Errorf("references left: a %d, c %d", a.refs.Load(), c.refs.Load())
	}
}

// TestSkippedWritesDropAtOnce checks that a write its store skips (the
// shard's breaker open, RZ-STS-003; not attempted, RZ-STS-004; or a
// missing command set, RZ-STS-005) is dropped at once and counted by
// kind, an invalidation included, without a retry.
func TestSkippedWritesDropAtOnce(t *testing.T) {
	for _, skip := range []*statestore.Error{statestore.ErrBreakerOpen, statestore.ErrNotAttempted, statestore.ErrUnsupported} {
		t.Run(skip.Code(), func(t *testing.T) {
			q, m, _ := startedQueue(t, small(8, 8, 2))
			tg := newTarget()
			tg.answer = func(w *statestore.Write) { w.Err = skip }
			b := q.Bind(tg)
			if !b.Enqueue(settleC, refund("r")) || !b.Enqueue(storeC, store("s", 1)) || !b.Enqueue(invC, invalidation(1)) {
				t.Fatal("Enqueue refused")
			}
			waitFor(t, "every write", func() bool { return q.pending.Load() == 0 })
			if got := len(tg.written()); got != 3 {
				t.Errorf("writes sent = %d, want 3 (no retry)", got)
			}
			for _, op := range []statestore.OpKind{statestore.OpRefund, statestore.OpCacheSet, statestore.OpCacheInvalidate} {
				if got := m.drops(op); got != 1 {
					t.Errorf("%s drops = %d, want 1", op, got)
				}
			}
			if tg.refs.Load() != 0 {
				t.Errorf("references left: %d", tg.refs.Load())
			}
		})
	}
}

// TestInvalidationRetriedOnce checks that a failed invalidation
// (RZ-STS-001 or RZ-STS-002) is written once more, and is dropped and
// counted only when that retry fails too.
func TestInvalidationRetriedOnce(t *testing.T) {
	tests := []struct {
		name  string
		fails int
		drops uint64
	}{
		{"retry succeeds", 1, 0},
		{"retry fails", 2, 1},
	}
	for _, tt := range tests {
		for _, fail := range []*statestore.Error{statestore.ErrTimeout, statestore.ErrFailed} {
			t.Run(tt.name+" "+fail.Code(), func(t *testing.T) {
				q, m, _ := startedQueue(t, small(8, 8, 1))
				tg := newTarget()
				seen := 0
				tg.answer = func(w *statestore.Write) {
					seen++
					if seen <= tt.fails {
						w.Err = fail
						return
					}
					w.Applied = true
				}
				if !q.Bind(tg).Enqueue(invC, invalidation(7)) {
					t.Fatal("Enqueue refused")
				}
				waitFor(t, "the invalidation", func() bool { return q.pending.Load() == 0 })
				if got := tg.written(); !slices.Equal(got, []string{"inv7", "inv7"}) {
					t.Errorf("written %v, want the invalidation twice", got)
				}
				if got := m.drops(statestore.OpCacheInvalidate); got != tt.drops {
					t.Errorf("cache_invalidate drops = %d, want %d", got, tt.drops)
				}
				if tg.refs.Load() != 0 {
					t.Errorf("references left: %d", tg.refs.Load())
				}
			})
		}
	}
}

// TestFailedWritesNotRetried checks that a refund or store the store
// attempted and failed is written once and not counted as dropped: the
// driver counts the failure, and a lost refund only over-charges.
func TestFailedWritesNotRetried(t *testing.T) {
	q, m, _ := startedQueue(t, small(8, 8, 1))
	tg := newTarget()
	tg.answer = func(w *statestore.Write) { w.Err = statestore.ErrTimeout }
	b := q.Bind(tg)
	if !b.Enqueue(settleC, refund("r")) || !b.Enqueue(storeC, store("s", 1)) {
		t.Fatal("Enqueue refused")
	}
	waitFor(t, "both writes", func() bool { return q.pending.Load() == 0 })
	if got := tg.written(); len(got) != 2 {
		t.Errorf("written %v, want each write once", got)
	}
	if got := m.drops(statestore.OpRefund) + m.drops(statestore.OpCacheSet); got != 0 {
		t.Errorf("failed writes counted %d drops", got)
	}
}

// TestRetryIntoFullClassDrops checks that a failed invalidation whose
// retry finds its class full is dropped and counted, and its Target
// reference released.
func TestRetryIntoFullClassDrops(t *testing.T) {
	q, m, _, _ := testQueue(t, small(1, 1, 1))
	tg := newTarget()
	b := q.Bind(tg)
	fill(t, b, invC, 1, func(int) *statestore.Write { return invalidation(1) })
	dst := make([]item, 1)
	if n, _ := q.next(dst); n != 1 {
		t.Fatal("next took nothing")
	}
	fill(t, b, invC, 1, func(int) *statestore.Write { return invalidation(2) }) // the class is full again
	dst[0].w.Err = statestore.ErrTimeout
	q.settle(invC, dst[0])
	if got := m.drops(statestore.OpCacheInvalidate); got != 1 {
		t.Errorf("cache_invalidate drops = %d, want 1", got)
	}
	if got := tg.refs.Load(); got != 1 {
		t.Errorf("target references = %d, want 1 for the queued invalidation", got)
	}
	if got := q.pending.Load(); got != 1 {
		t.Errorf("pending = %d, want 1", got)
	}
}

// TestPanickingTargetFailsBatch checks that a Target panicking in Write
// fails the writes it left without an outcome, logs, and leaves the
// writer serving later writes.
func TestPanickingTargetFailsBatch(t *testing.T) {
	q, m, logs := startedQueue(t, small(8, 8, 1))
	tg := newTarget()
	tg.answer = func(w *statestore.Write) {
		if w.Refund.Name == "boom" {
			panic("driver bug")
		}
		w.Applied = true
	}
	b := q.Bind(tg)
	boom := refund("boom")
	if !b.Enqueue(settleC, boom) {
		t.Fatal("Enqueue refused")
	}
	waitFor(t, "the panicking write", func() bool { return q.pending.Load() == 0 })
	if !errors.Is(boom.Err, statestore.ErrFailed) {
		t.Errorf("panicked write Err = %v, want RZ-STS-002", boom.Err)
	}
	if !logs.has("post-commit write panicked") {
		t.Error("the panic was not logged")
	}
	later := refund("later")
	if !b.Enqueue(settleC, later) {
		t.Fatal("Enqueue refused after the panic")
	}
	waitFor(t, "the later write", func() bool { return q.pending.Load() == 0 })
	if !later.Applied || later.Err != nil {
		t.Errorf("later write: applied %v, err %v", later.Applied, later.Err)
	}
	if m.drops(statestore.OpRefund) != 0 || tg.refs.Load() != 0 {
		t.Errorf("drops %d, references %d", m.drops(statestore.OpRefund), tg.refs.Load())
	}
}

// TestCloseFlushes checks that Close writes every queued write before it
// returns and stops the writers.
func TestCloseFlushes(t *testing.T) {
	q, m, _, _ := testQueue(t, small(64, 4, 2))
	tg := newTarget()
	tg.block = make(chan struct{})
	b := q.Bind(tg)
	fill(t, b, settleC, 20, func(i int) *statestore.Write { return refund(strconv.Itoa(i)) })
	q.start(t.Context())
	done := make(chan error, 1)
	go func() { done <- q.Close(context.Background()) }()
	close(tg.block)
	if err := <-done; err != nil {
		t.Fatalf("Close = %v", err)
	}
	if got := len(tg.written()); got != 20 {
		t.Errorf("written %d writes, want 20", got)
	}
	if m.drops(statestore.OpRefund) != 0 || tg.refs.Load() != 0 || m.depth.v.Load() != 0 {
		t.Errorf("drops %d, references %d, depth %d", m.drops(statestore.OpRefund), tg.refs.Load(), m.depth.v.Load())
	}
	if err := q.Close(t.Context()); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// TestCloseFlushBound checks the Drain flush bound: Close waits 5 s for
// writes that cannot finish, then stops the writers, drops and counts
// what is left, logs it and returns nil.
func TestCloseFlushBound(t *testing.T) {
	q, m, clk, logs := testQueue(t, small(64, 1, 1))
	tg := newTarget()
	tg.block, tg.entered = make(chan struct{}), make(chan struct{}, 8)
	b := q.Bind(tg)
	q.start(t.Context())
	fill(t, b, settleC, 1, func(int) *statestore.Write { return refund("stuck") })
	<-tg.entered
	fill(t, b, settleC, 3, func(i int) *statestore.Write { return refund(strconv.Itoa(i)) })
	done := make(chan error, 1)
	go func() { done <- q.Close(context.Background()) }()
	waitFor(t, "the flush timer", func() bool { return clk.Pending() == 1 })
	clk.Advance(DefaultFlush - time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Close returned %v before the flush bound", err)
	default:
	}
	clk.Advance(time.Millisecond)
	if err := <-done; err != nil {
		t.Fatalf("Close = %v, want nil at the flush bound", err)
	}
	if got := m.drops(statestore.OpRefund); got != 4 {
		t.Errorf("refund drops = %d, want 4 (the stuck write and 3 queued)", got)
	}
	if tg.refs.Load() != 0 || m.depth.v.Load() != 0 || q.Len() != 0 {
		t.Errorf("references %d, depth %d, Len %d", tg.refs.Load(), m.depth.v.Load(), q.Len())
	}
	if !logs.has("post-commit queue closed with writes left; dropped") {
		t.Error("the dropped writes were not logged")
	}
	if b.Enqueue(settleC, refund("late")) {
		t.Error("Enqueue after Close accepted a write")
	}
}

// TestCloseContextEnds checks that Close returns ctx's error when ctx
// ends the flush, and still drops and counts the writes left.
func TestCloseContextEnds(t *testing.T) {
	q, m, _, _ := testQueue(t, small(64, 1, 1))
	tg := newTarget()
	tg.block, tg.entered = make(chan struct{}), make(chan struct{}, 8)
	b := q.Bind(tg)
	q.start(t.Context())
	fill(t, b, settleC, 2, func(i int) *statestore.Write { return refund(strconv.Itoa(i)) })
	<-tg.entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := q.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v, want context.Canceled", err)
	}
	if got := m.drops(statestore.OpRefund); got != 2 {
		t.Errorf("refund drops = %d, want 2", got)
	}
	if tg.refs.Load() != 0 {
		t.Errorf("references left: %d", tg.refs.Load())
	}
}

// TestCloseEmptyQueue checks that Close on an idle queue returns at once,
// without waiting for the flush bound.
func TestCloseEmptyQueue(t *testing.T) {
	q, _, clk, _ := testQueue(t, small(8, 8, 2))
	q.start(t.Context())
	if err := q.Close(context.Background()); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if clk.Pending() != 0 {
		t.Errorf("Close left %d timers", clk.Pending())
	}
}

// TestConcurrentEnqueue checks, under the race detector, that writes
// enqueued from many goroutines while writers run are each written once
// or dropped and counted, and every Target reference is returned.
func TestConcurrentEnqueue(t *testing.T) {
	q, m, _ := startedQueue(t, small(32, 8, 4))
	tg := newTarget()
	b := q.Bind(tg)
	const workers, each = 8, 300
	accepted := make(chan uint64, workers)
	for g := range workers {
		go func() {
			var n uint64
			for i := range each {
				var w *statestore.Write
				var cl statestore.WriteClass
				switch i % 3 {
				case 0:
					w, cl = refund(strconv.Itoa(g)+"-"+strconv.Itoa(i)), settleC
				case 1:
					w, cl = store(strconv.Itoa(g)+"-"+strconv.Itoa(i), 16), storeC
				default:
					w, cl = invalidation(7), invC
				}
				if b.Enqueue(cl, w) {
					n++
				}
			}
			accepted <- n
		}()
	}
	var total uint64
	for range workers {
		total += <-accepted
	}
	waitFor(t, "every accepted write", func() bool { return q.pending.Load() == 0 })
	if got := tg.count.Load(); got != total {
		t.Errorf("written %d writes, accepted %d", got, total)
	}
	drops := m.drops(statestore.OpRefund) + m.drops(statestore.OpCacheSet) + m.drops(statestore.OpCacheInvalidate)
	if drops+total != workers*each {
		t.Errorf("accepted %d + dropped %d != offered %d", total, drops, workers*each)
	}
	if tg.refs.Load() != 0 || m.depth.v.Load() != 0 {
		t.Errorf("references %d, depth %d", tg.refs.Load(), m.depth.v.Load())
	}
}
