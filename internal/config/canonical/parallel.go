// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"context"
	"sync"
	"sync/atomic"
)

// maxWorkers is the most goroutines one Encode or Decode runs: a larger
// Workers means 256, so no worker count overflows the arithmetic of the
// chunks or sizes a channel or a slice by it.
const maxWorkers = 256

// workerCount returns the worker count of the option v: 1 when v is less
// than 1 and maxWorkers at most.
func workerCount(v int) int { return min(max(1, v), maxWorkers) }

// chunksPerWorker is how many chunks each worker gets on average, so
// resources of uneven size still balance.
const chunksPerWorker = 4

// chunkCount returns how many contiguous chunks parallel splits n items
// into for workers goroutines: one for a single worker or at most one
// item, otherwise up to chunksPerWorker per worker and never more than n.
// It never multiplies past n, so no worker count overflows it.
func chunkCount(n, workers int) int {
	switch {
	case workers <= 1 || n <= 1:
		return 1
	case workers > n/chunksPerWorker:
		return n
	default:
		return workers * chunksPerWorker
	}
}

// parallel calls fn(worker, chunk, lo, hi) for each of the chunks
// contiguous ranges [lo, hi) of [0, n), on min(workers, chunks)
// goroutines that it owns and waits for before returning (02 req 78:
// a bounded pool per call, no goroutine outliving it). worker is in
// [0, min(workers, chunks)), so fn can use per-worker scratch. Chunks are
// handed out in order; once ctx is done or a call fails or panics no
// further chunk starts. The result is the error of the lowest failing
// chunk, or ctx's error, so it does not depend on scheduling when fn is
// deterministic. A panic in fn is raised again on the calling goroutine
// once every goroutine has returned, as a *workPanic, the same way with
// one worker and with several (the lowest panicking chunk's when several
// panic).
func parallel(ctx context.Context, n, workers, chunks int, fn func(worker, chunk, lo, hi int) error) error {
	// one runs chunk k on worker w, ctx checked first, and returns its
	// panic or error.
	one := func(w, k int) (*workPanic, error) {
		var err error
		wp := capture(func() {
			if err = ctx.Err(); err == nil {
				lo, hi := k*n/chunks, (k+1)*n/chunks
				err = fn(w, k, lo, hi)
			}
		})
		return wp, err
	}
	if workers <= 1 || chunks <= 1 {
		for k := range chunks {
			wp, err := one(0, k)
			if wp != nil {
				panic(wp)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	errs := make([]error, chunks)
	panicked := make([]*workPanic, chunks)
	var next atomic.Int64
	var failed atomic.Bool
	var wg sync.WaitGroup
	for w := range min(workers, chunks) {
		wg.Go(func() {
			for !failed.Load() {
				k := int(next.Add(1) - 1)
				if k >= chunks {
					return
				}
				wp, err := one(w, k)
				if wp != nil || err != nil {
					panicked[k], errs[k] = wp, err
					failed.Store(true)
					return
				}
			}
		})
	}
	wg.Wait()
	for _, wp := range panicked {
		if wp != nil {
			panic(wp)
		}
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
