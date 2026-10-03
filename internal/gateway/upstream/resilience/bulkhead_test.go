// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// waitPending polls until b has n waiters.
func waitPending(t *testing.T, b *Bulkhead, n int) {
	t.Helper()
	for i := 0; b.Pending() != n; i++ {
		if i > 5000 {
			t.Fatalf("Pending() = %d, want %d", b.Pending(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// isFull reports an RZ-UP-006 refusal.
func isFull(err error) bool {
	code, _ := errcode.CodeOf(err)
	return code == "RZ-UP-006" && errors.Is(err, ErrBulkheadFull)
}

// TestBulkheadFull_05Req36: slots up to maxConnections, then waiters up to
// maxPendingRequests, then 503 RZ-UP-006 at once; maxPendingRequests 0
// refuses at once when the slots are full.
func TestBulkheadFull_05Req36(t *testing.T) {
	ctx := context.Background()
	t.Run("no waiters", func(t *testing.T) {
		var b Bulkhead
		cfg := BulkheadConfig{MaxConnections: 2, MaxPendingRequests: 0}
		for i := range 2 {
			if err := b.Acquire(ctx, &cfg); err != nil {
				t.Fatalf("slot %d: %v", i+1, err)
			}
		}
		if err := b.Acquire(ctx, &cfg); !isFull(err) {
			t.Fatalf("third attempt: %v, want RZ-UP-006", err)
		}
		b.Release()
		if err := b.Acquire(ctx, &cfg); err != nil {
			t.Fatalf("after a release: %v", err)
		}
		if b.InFlight() != 2 {
			t.Fatalf("InFlight() = %d", b.InFlight())
		}
	})
	t.Run("FIFO waiters", func(t *testing.T) {
		var b Bulkhead
		cfg := BulkheadConfig{MaxConnections: 1, MaxPendingRequests: 2}
		if err := b.Acquire(ctx, &cfg); err != nil {
			t.Fatal(err)
		}
		order := make(chan int, 2)
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Go(func() {
				if err := b.Acquire(ctx, &cfg); err != nil {
					t.Errorf("waiter %d: %v", i, err)
					return
				}
				order <- i
			})
			waitPending(t, &b, i+1)
		}
		if err := b.Acquire(ctx, &cfg); !isFull(err) {
			t.Fatalf("with the queue full: %v, want RZ-UP-006", err)
		}
		b.Release()
		if first := <-order; first != 0 {
			t.Fatalf("waiter %d was granted first", first)
		}
		b.Release()
		if second := <-order; second != 1 {
			t.Fatalf("waiter %d was granted second", second)
		}
		wg.Wait()
		if b.InFlight() != 1 || b.Pending() != 0 {
			t.Fatalf("InFlight %d, Pending %d", b.InFlight(), b.Pending())
		}
	})
}

// TestBulkheadWaiterContext_05Req36: each waiter is bounded by its attempt
// context and leaves the queue when it ends.
func TestBulkheadWaiterContext_05Req36(t *testing.T) {
	var b Bulkhead
	cfg := BulkheadConfig{MaxConnections: 1, MaxPendingRequests: 1}
	if err := b.Acquire(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	clk := newClock()
	ctx, d := WithDeadline(context.Background(), clk, clk.Now().Add(time.Second), ErrAttemptTimeout)
	defer d.Cancel()
	errc := make(chan error, 1)
	go func() { errc <- b.Acquire(ctx, &cfg) }()
	waitPending(t, &b, 1)
	clk.Advance(time.Second)
	err := <-errc
	if !errors.Is(err, ErrAttemptTimeout) || Classify(ctx, err) != KindTimeout {
		t.Fatalf("waiter error %v", err)
	}
	if b.Pending() != 0 || b.InFlight() != 1 {
		t.Fatalf("Pending %d, InFlight %d after the waiter left", b.Pending(), b.InFlight())
	}
	// An already-ended context never queues.
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Acquire(ended, &cfg); !errors.Is(err, context.Canceled) || b.Pending() != 0 {
		t.Fatalf("ended context: %v, pending %d", err, b.Pending())
	}
	b.Release()
	if b.InFlight() != 0 {
		t.Fatalf("InFlight %d", b.InFlight())
	}
}

// TestBulkheadGrantRace: a slot granted together with the waiter's
// cancellation is handed on, never leaked (05 req 36).
func TestBulkheadGrantRace(t *testing.T) {
	for range 200 {
		var b Bulkhead
		cfg := BulkheadConfig{MaxConnections: 1, MaxPendingRequests: 4}
		if err := b.Acquire(context.Background(), &cfg); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- b.Acquire(ctx, &cfg) }()
		waitPending(t, &b, 1)
		go cancel()
		b.Release()
		if err := <-errc; err == nil {
			b.Release()
		}
		if b.InFlight() != 0 || b.Pending() != 0 {
			t.Fatalf("leak: InFlight %d, Pending %d", b.InFlight(), b.Pending())
		}
	}
}

// TestBulkheadHotReload_05Req2: a raised ceiling grants waiters at the next
// Acquire; a lowered one keeps the slots already held.
func TestBulkheadHotReload_05Req2(t *testing.T) {
	var b Bulkhead
	old := BulkheadConfig{MaxConnections: 1, MaxPendingRequests: 2}
	if err := b.Acquire(context.Background(), &old); err != nil {
		t.Fatal(err)
	}
	granted := make(chan error, 1)
	go func() { granted <- b.Acquire(context.Background(), &old) }()
	waitPending(t, &b, 1)
	raised := BulkheadConfig{MaxConnections: 3, MaxPendingRequests: 0}
	if err := b.Acquire(context.Background(), &raised); err != nil {
		t.Fatalf("new snapshot: %v", err)
	}
	if err := <-granted; err != nil {
		t.Fatalf("the waiter was not granted by the raised ceiling: %v", err)
	}
	if b.InFlight() != 3 {
		t.Fatalf("InFlight %d", b.InFlight())
	}
	lowered := BulkheadConfig{MaxConnections: 1, MaxPendingRequests: 0}
	if err := b.Acquire(context.Background(), &lowered); !isFull(err) {
		t.Fatalf("lowered ceiling admitted: %v", err)
	}
	b.Release()
	b.Release()
	if err := b.Acquire(context.Background(), &lowered); !isFull(err) {
		t.Fatalf("admitted at the lowered ceiling with one slot held: %v", err)
	}
	b.Release()
	if err := b.Acquire(context.Background(), &lowered); err != nil {
		t.Fatalf("empty bulkhead refused: %v", err)
	}
	b.Release()
	b.Release() // unpaired: stays at zero
	if b.InFlight() != 0 {
		t.Fatalf("InFlight %d", b.InFlight())
	}
}

// TestBulkheadConcurrent never holds more slots than maxConnections.
func TestBulkheadConcurrent_05Req36(t *testing.T) {
	var (
		b     Bulkhead
		held  atomic.Int64
		peak  atomic.Int64
		full  atomic.Int64
		wg    sync.WaitGroup
		limit = 4
	)
	cfg := BulkheadConfig{MaxConnections: limit, MaxPendingRequests: 8}
	for range 32 {
		wg.Go(func() {
			for range 200 {
				if err := b.Acquire(context.Background(), &cfg); err != nil {
					if !isFull(err) {
						t.Errorf("Acquire: %v", err)
					}
					full.Add(1)
					continue
				}
				n := held.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				held.Add(-1)
				b.Release()
			}
		})
	}
	wg.Wait()
	if peak.Load() > int64(limit) {
		t.Fatalf("%d slots held at once, limit %d", peak.Load(), limit)
	}
	if b.InFlight() != 0 || b.Pending() != 0 {
		t.Fatalf("InFlight %d, Pending %d", b.InFlight(), b.Pending())
	}
}

// TestEndpointCap_05Req36 is max(8, floor(min(2 × w/Σw, 0.5) × maxConnections)).
func TestEndpointCap_05Req36(t *testing.T) {
	tests := []struct {
		name          string
		max           int
		weight, total uint64
		want          int
	}{
		{"three equal Endpoints", 1024, 1, 3, 512},
		{"ten equal Endpoints", 1024, 1, 10, 204},
		{"minimum of 8", 10, 1, 100, 8},
		{"one Endpoint", 1024, 5, 5, 512},
		{"quarter share", 1000, 1, 4, 500},
		{"just under a quarter", 1000, 24, 100, 480},
		{"zero total", 1024, 0, 0, 512},
		{"weight over total", 1024, 9, 3, 512},
		{"zero weight", 1024, 0, 7, 8},
		{"negative ceiling", -1, 1, 2, 8},
		{"huge values", math.MaxInt32, math.MaxUint32, math.MaxUint64, 8},
		{"huge ceiling", math.MaxInt64, 1, 1 << 62, 8},
	}
	for _, tt := range tests {
		if got := EndpointCap(tt.max, tt.weight, tt.total); got != tt.want {
			t.Errorf("%s: EndpointCap(%d, %d, %d) = %d, want %d", tt.name, tt.max, tt.weight, tt.total, got, tt.want)
		}
	}
	for m := 1; m < 3000; m += 7 {
		for w := uint64(1); w < 20; w++ {
			got := EndpointCap(m, w, 20)
			exact := min(2*int(w)*m/20, m/2)
			if got != max(8, exact) {
				t.Fatalf("EndpointCap(%d, %d, 20) = %d, want %d", m, w, got, max(8, exact))
			}
		}
	}
}

// BenchmarkBulkhead measures an uncontended slot acquire and release.
func BenchmarkBulkhead(b *testing.B) {
	var bh Bulkhead
	cfg := DefaultBulkheadConfig()
	ctx := context.Background()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if bh.Acquire(ctx, &cfg) == nil {
				bh.Release()
			}
		}
	})
}
