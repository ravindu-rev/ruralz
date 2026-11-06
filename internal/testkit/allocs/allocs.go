// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package allocs measures the heap bytes a function allocates, exactly
// enough for tests that bound a function's allocations in bytes, such as
// a clip of a 16 MiB value that may allocate a few hundred. A delta of
// runtime.MemStats.TotalAlloc around the function counts what the runtime
// allocates meanwhile, a few kilobytes when a collection's mark workers
// finish during it, and misses the objects still in a P's cached span.
package allocs

import (
	"runtime"
	"runtime/metrics"
)

// Bytes returns the bytes allocated while f runs, counted exactly. Three
// things beside f would land in the count, and none does:
//
//   - a P adds the objects it allocates from a cached span only when the
//     span leaves its cache, so runtime.GC flushes every P's cached spans
//     into the count before and after f, and the collection a large
//     allocation before f started ends before f runs;
//   - the process's first metrics.Read builds the runtime's table of
//     metrics, about 19 KB on Go 1.26, and each Read needs its sample, so
//     the sample is allocated and read once before the first runtime.GC,
//     and the reads around f allocate nothing;
//   - on many Ps, a collection's mark workers finish together and queue on
//     the runtime's mark-done semaphore, and each worker that waits
//     allocates a 112-byte sudog when its P has none cached, so the
//     measurement runs at GOMAXPROCS 1, as testing.AllocsPerRun does.
//
// The caller's tests run one at a time, so f is then all that allocates in
// between.
func Bytes(f func()) uint64 {
	s := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
	metrics.Read(s)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	runtime.GC()
	metrics.Read(s)
	before := s[0].Value.Uint64()
	f()
	runtime.GC()
	metrics.Read(s)
	return s[0].Value.Uint64() - before
}

// Least returns the smallest of n measurements of f by Bytes, at least
// one: a stray runtime allocation, such as the scavenger growing its timer
// heap, may still land in one measurement in hundreds.
func Least(n int, f func()) uint64 {
	low := Bytes(f)
	for range n - 1 {
		low = min(low, Bytes(f))
	}
	return low
}
