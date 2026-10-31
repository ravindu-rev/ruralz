// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package health

import "sync/atomic"

// Failure window geometry: FailureWindow split into windowBuckets buckets.
const (
	windowBuckets = 10
	bucketWidth   = int64(FailureWindow / windowBuckets)
)

// window counts events over the last FailureWindow in lock-free buckets.
// Each bucket packs its tick (bucket number, low 32 bits) above its count.
type window struct {
	b [windowBuckets]atomic.Uint64
}

// tickOf returns the bucket number of now (nanoseconds since the Tracker's
// base), wrapped to 32 bits.
func tickOf(now int64) uint64 {
	return uint64(max(now, 0)/bucketWidth) & 0xffff_ffff
}

// add counts one event at now.
func (w *window) add(now int64) {
	tick := tickOf(now)
	slot := &w.b[tick%windowBuckets]
	for {
		old := slot.Load()
		next := tick<<32 | 1
		if old>>32 == tick {
			next = old + 1
		}
		if slot.CompareAndSwap(old, next) {
			return
		}
	}
}

// sum returns the events counted in the buckets of the last FailureWindow
// ending at now.
func (w *window) sum(now int64) int64 {
	tick := tickOf(now)
	var n int64
	for i := range w.b {
		v := w.b[i].Load()
		if age := (tick - v>>32) & 0xffff_ffff; age < windowBuckets {
			n += int64(v & 0xffff_ffff)
		}
	}
	return n
}
