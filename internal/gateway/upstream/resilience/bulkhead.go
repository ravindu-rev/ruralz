// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"context"
	"fmt"
	"math/bits"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// Bulkhead is the in-flight ceiling of one Upstream on one Node (05 req
// 36): at most maxConnections attempts hold a slot, taken before RoundTrip
// (so HTTP/2 streams count) and released when the attempt's response body
// closes (05 reqs 30 and 38: a streaming leg holds its slot for life, up to
// the Route timeout). At most maxPendingRequests attempts wait for a slot,
// first in first out, each bounded by its attempt context; with the queue
// full an attempt fails at once with RZ-UP-006.
//
// Every Acquire takes the BulkheadConfig of the caller's snapshot: a Hot
// Reload's new ceiling applies from the next Acquire on (waiters are
// granted at once when it rises) while slots already held stay held (05
// req 2). The zero value is ready to use; it is safe for concurrent use
// and starts no goroutine.
type Bulkhead struct {
	mu       sync.Mutex
	inFlight int
	limit    int
	pending  int
	head     *bulkheadWaiter
	tail     *bulkheadWaiter
}

// bulkheadWaiter is one queued attempt; ready receives its slot.
type bulkheadWaiter struct {
	ready      chan struct{}
	next, prev *bulkheadWaiter
	granted    bool
	queued     bool
}

// Acquire takes a slot for one attempt. It returns nil with a slot, an
// *errcode.Error with RZ-UP-006 wrapping ErrBulkheadFull when every slot
// and the waiter queue are full, or, when ctx ends while waiting, an error
// wrapping context.Cause(ctx) (the attempt, leg or Route deadline: error
// kind timeout). Every nil return must be paired with one Release.
func (b *Bulkhead) Acquire(ctx context.Context, cfg *BulkheadConfig) error {
	b.mu.Lock()
	b.limit = max(cfg.MaxConnections, 1)
	b.grantLocked()
	if b.head == nil && b.inFlight < b.limit {
		b.inFlight++
		b.mu.Unlock()
		return nil
	}
	if b.pending >= cfg.MaxPendingRequests {
		b.mu.Unlock()
		return errcode.Wrap(CodeBulkheadFull, ErrBulkheadFull)
	}
	if ctx.Err() != nil {
		b.mu.Unlock()
		return fmt.Errorf("resilience: waiting for a bulkhead slot: %w", context.Cause(ctx))
	}
	w := &bulkheadWaiter{ready: make(chan struct{}, 1)}
	b.pushLocked(w)
	b.mu.Unlock()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
	}
	b.mu.Lock()
	if w.granted {
		// The slot arrived with the cancellation; hand it on.
		b.releaseLocked()
	} else {
		b.removeLocked(w)
	}
	b.mu.Unlock()
	return fmt.Errorf("resilience: waiting for a bulkhead slot: %w", context.Cause(ctx))
}

// Release returns a slot taken by Acquire, granting it to the first
// waiter when the ceiling allows.
func (b *Bulkhead) Release() {
	b.mu.Lock()
	b.releaseLocked()
	b.mu.Unlock()
}

// releaseLocked frees one slot and grants waiters.
func (b *Bulkhead) releaseLocked() {
	if b.inFlight > 0 {
		b.inFlight--
	}
	b.grantLocked()
}

// grantLocked hands free slots to waiters in queue order.
func (b *Bulkhead) grantLocked() {
	for b.head != nil && b.inFlight < b.limit {
		w := b.head
		b.removeLocked(w)
		w.granted = true
		b.inFlight++
		w.ready <- struct{}{}
	}
}

// pushLocked appends w to the queue.
func (b *Bulkhead) pushLocked(w *bulkheadWaiter) {
	w.queued = true
	w.prev = b.tail
	if b.tail != nil {
		b.tail.next = w
	} else {
		b.head = w
	}
	b.tail = w
	b.pending++
}

// removeLocked unlinks a queued w.
func (b *Bulkhead) removeLocked(w *bulkheadWaiter) {
	if !w.queued {
		return
	}
	if w.prev != nil {
		w.prev.next = w.next
	} else {
		b.head = w.next
	}
	if w.next != nil {
		w.next.prev = w.prev
	} else {
		b.tail = w.prev
	}
	w.next, w.prev, w.queued = nil, nil, false
	b.pending--
}

// InFlight returns the slots held.
func (b *Bulkhead) InFlight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inFlight
}

// Pending returns the attempts waiting for a slot.
func (b *Bulkhead) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pending
}

// EndpointCap returns the in-flight cap of one Endpoint (05 req 36):
// max(8, floor(min(2 × weight/total, 0.5) × maxConnections)). A capped
// Endpoint is excluded from selection while another remains (05 req 11
// step c). A zero total, or a weight of at least the total, gives half
// the ceiling.
func EndpointCap(maxConnections int, weight, total uint64) int {
	m := uint64(max(maxConnections, 0))
	half := m/100*EndpointCapSharePercent + m%100*EndpointCapSharePercent/100
	share := half
	if total > 0 && weight < total {
		// floor(2 × weight × m / total); weight < total keeps the quotient
		// within 64 bits.
		hi, lo := bits.Mul64(weight, 2*m)
		q, _ := bits.Div64(hi, lo, total)
		share = min(q, half)
	}
	return max(EndpointCapMin, int(share)) //nolint:gosec // G115: share <= maxConnections/2, an int.
}
