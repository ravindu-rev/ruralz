// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// Tests for spec 04 req 19 (in-flight units: 20,000, one atomic add, 503
// RZ-RT-005 at once, never queued), spec 04 req 67 (ceiling lowered by the
// other process's usage during a handover) and spec 05 req 43 (k units for
// parallel composition steps); test plan item 9.

// TestReq19UnitsExactCeilingUnderContention makes 40,000 concurrent
// acquisition attempts against the 20,000 ceiling: exactly 20,000 hold a
// unit, the rest are rejected at once. Attempts run in batches of 4,000
// goroutines because the race detector allows at most 8,128 live
// goroutines; the units stay held across batches, so the ceiling is
// contended by every attempt.
func TestReq19UnitsExactCeilingUnderContention(t *testing.T) {
	const attempts, batch = 40_000, 4_000
	u := NewUnits(DefaultUnits)
	var ok, rejected atomic.Int64
	for range attempts / batch {
		var start, wg sync.WaitGroup
		start.Add(1)
		for range batch {
			wg.Go(func() {
				start.Wait()
				if u.TryAcquire(1) {
					ok.Add(1)
				} else {
					rejected.Add(1)
				}
			})
		}
		start.Done()
		wg.Wait()
	}
	if ok.Load() != DefaultUnits || rejected.Load() != attempts-DefaultUnits {
		t.Fatalf("acquired %d, rejected %d; want %d and %d", ok.Load(), rejected.Load(), DefaultUnits, attempts-DefaultUnits)
	}
	if u.InUse() != DefaultUnits {
		t.Fatalf("InUse = %d, want %d", u.InUse(), DefaultUnits)
	}
	u.Release(ok.Load())
	if u.InUse() != 0 {
		t.Fatalf("InUse after release = %d, want 0", u.InUse())
	}
}

// TestReq19UnitsChurnNeverExceedsCeiling acquires and releases from many
// goroutines while counting the holders: they never exceed the ceiling and
// the count returns to zero.
func TestReq19UnitsChurnNeverExceedsCeiling(t *testing.T) {
	const ceiling, workers, rounds = 64, 256, 400
	u := NewUnits(ceiling)
	var holders, peak atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range rounds {
				if !u.TryAcquire(1) {
					continue
				}
				raise(&peak, holders.Add(1))
				holders.Add(-1)
				u.Release(1)
			}
		})
	}
	wg.Wait()
	if p := peak.Load(); p > ceiling || p == 0 {
		t.Fatalf("peak holders %d, want 1..%d", p, ceiling)
	}
	if u.InUse() != 0 {
		t.Fatalf("InUse = %d after churn, want 0", u.InUse())
	}
}

// raise stores v in peak when it is larger.
func raise(peak *atomic.Int64, v int64) {
	for {
		p := peak.Load()
		if v <= p || peak.CompareAndSwap(p, v) {
			return
		}
	}
}

func TestReq19UnitsTryAcquire(t *testing.T) {
	tests := []struct {
		name     string
		limit    int64
		held     int64 // taken first
		external int64
		k        int64
		want     bool
		inUse    int64
	}{
		{"one of many", 10, 0, 0, 1, true, 1},
		{"last unit", 10, 9, 0, 1, true, 10},
		{"full", 10, 10, 0, 1, false, 10},
		{"k steps fit exactly (05 req 43)", 10, 6, 0, 4, true, 10},
		{"k steps over by one take nothing", 10, 7, 0, 4, false, 7},
		{"k zero takes nothing", 10, 10, 0, 0, true, 10},
		{"k negative takes nothing", 10, 3, 0, -2, true, 3},
		{"zero ceiling rejects", 0, 0, 0, 1, false, 0},
		{"negative limit is zero", -5, 0, 0, 1, false, 0},
		{"handover lowers the ceiling (req 67)", 10, 5, 5, 1, false, 5},
		{"handover leaves room", 10, 4, 5, 1, true, 5},
		{"external above the limit", 10, 0, 50, 1, false, 0},
		{"negative external is zero", 10, 9, -3, 1, true, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := NewUnits(tt.limit)
			if tt.held > 0 && !u.TryAcquire(tt.held) {
				t.Fatalf("setup: TryAcquire(%d) failed", tt.held)
			}
			u.SetExternal(tt.external)
			if got := u.TryAcquire(tt.k); got != tt.want {
				t.Fatalf("TryAcquire(%d) = %v, want %v", tt.k, got, tt.want)
			}
			if u.InUse() != tt.inUse {
				t.Fatalf("InUse = %d, want %d", u.InUse(), tt.inUse)
			}
		})
	}
}

// TestReq67UnitsSetExternal lowers the ceiling while units are held: held
// units stay, new ones wait for room, and clearing the usage restores the
// full ceiling.
func TestReq67UnitsSetExternal(t *testing.T) {
	u := NewUnits(100)
	if !u.TryAcquire(60) {
		t.Fatal("TryAcquire(60) failed")
	}
	u.SetExternal(70)
	if u.Ceiling() != 30 {
		t.Fatalf("Ceiling = %d, want 30", u.Ceiling())
	}
	if u.TryAcquire(1) {
		t.Fatal("a unit was admitted above the lowered ceiling")
	}
	if u.InUse() != 60 {
		t.Fatalf("held units changed: %d", u.InUse())
	}
	u.Release(35)
	if !u.TryAcquire(5) || u.TryAcquire(1) {
		t.Fatal("the lowered ceiling of 30 was not applied exactly")
	}
	u.SetExternal(0)
	if u.Ceiling() != 100 || !u.TryAcquire(70) {
		t.Fatalf("full ceiling not restored: %d", u.Ceiling())
	}
}

// TestReq19UnitsAcquireError is the RZ-RT-005 mapping: 503, registered.
func TestReq19UnitsAcquireError(t *testing.T) {
	u := NewUnits(1)
	if err := u.Acquire(1); err != nil {
		t.Fatalf("Acquire = %v", err)
	}
	err := u.Acquire(1)
	if !errors.Is(err, ErrFull) {
		t.Fatalf("Acquire at the ceiling = %v, want ErrFull", err)
	}
	code, ok := errcode.CodeOf(err)
	if !ok || code != "RZ-RT-005" || CodeFull != "RZ-RT-005" {
		t.Fatalf("code %q, want RZ-RT-005", code)
	}
	if s := errcode.Status(code); s != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", s)
	}
	u.Release(0) // no-op
	u.Release(1)
	if u.InUse() != 0 {
		t.Fatalf("InUse = %d", u.InUse())
	}
}

// BenchmarkUnitsAcquireRelease is the admission step of every request (the
// RZ-RT-005 path budget of spec 04 req 80 starts here).
func BenchmarkUnitsAcquireRelease(b *testing.B) {
	u := NewUnits(DefaultUnits)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if u.TryAcquire(1) {
				u.Release(1)
			}
		}
	})
}

// BenchmarkUnitsReject measures a rejection at a full ceiling.
func BenchmarkUnitsReject(b *testing.B) {
	u := NewUnits(0)
	b.ReportAllocs()
	for b.Loop() {
		if u.TryAcquire(1) {
			b.Fatal("admitted at a zero ceiling")
		}
	}
}
