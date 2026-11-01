// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package allocs

import (
	"runtime"
	"strings"
	"testing"
)

// TestBytesIsExact checks the measure: a known number of small
// allocations is counted in full, one or many, where the unflushed
// counter reports none until a span leaves its cache.
func TestBytesIsExact(t *testing.T) {
	for _, n := range []uint64{1, 100, 10000} {
		keep := make([][]byte, n)
		got := Bytes(func() {
			for i := range keep {
				keep[i] = make([]byte, 64)
			}
		})
		runtime.KeepAlive(keep)
		if want := 64 * n; got < want || got > want+16<<10 {
			t.Errorf("Bytes(%d allocations of 64 bytes) = %d, want %d to %d", n, got, want, want+16<<10)
		}
	}
}

// TestBytesCountsOnlyF checks that the measure adds nothing of its own: no
// sample, no metrics table and no mark worker on another P reaches the
// count, so a function that allocates nothing measures 0 and a tiny
// allocation measures its size class, the smallest of three runs; a
// measure that counts its own sample never reaches 0.
func TestBytesCountsOnlyF(t *testing.T) {
	if got := Least(3, func() {}); got != 0 {
		t.Errorf("Least(3, nothing) = %d, want 0", got)
	}
	var keep []byte
	if got := Least(3, func() { keep = make([]byte, 48) }); got != 48 {
		t.Errorf("Least(3, one 48-byte allocation) = %d, want 48", got)
	}
	runtime.KeepAlive(keep)
}

// TestBytesAfterALargeAllocation checks the case a MemStats delta gets
// wrong: a function measured right after a 16 MiB allocation, whose
// collection's mark workers would otherwise finish, and allocate, while
// it runs, measures only its own allocation.
func TestBytesAfterALargeAllocation(t *testing.T) {
	var keep []byte
	low := ^uint64(0)
	for range 3 {
		large := strings.Repeat("1", 16<<20)
		low = min(low, Bytes(func() { keep = make([]byte, 48) }))
		runtime.KeepAlive(large)
	}
	runtime.KeepAlive(keep)
	if low != 48 {
		t.Errorf("Bytes(one 48-byte allocation after 16 MiB) = %d, want 48", low)
	}
}

// TestLeastMeasuresAtLeastOnce checks Least for n below 2.
func TestLeastMeasuresAtLeastOnce(t *testing.T) {
	calls := 0
	_ = Least(0, func() { calls++ })
	if calls != 1 {
		t.Errorf("Least(0) ran f %d times, want 1", calls)
	}
}
