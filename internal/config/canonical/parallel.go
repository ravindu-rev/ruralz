// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"context"
	"sync"
	"sync/atomic"
)

// chunksPerWorker is how many chunks each worker gets on average, so
// resources of uneven size still balance.
const chunksPerWorker = 4

// chunkCount returns how many contiguous chunks parallel splits n items
// into for workers goroutines: one for a single worker or at most one
// item, otherwise up to chunksPerWorker per worker and never more than n.
func chunkCount(n, workers int) int {
	if workers <= 1 || n <= 1 {
		return 1
	}
	return min(n, workers*chunksPerWorker)
}

// parallel calls fn(worker, chunk, lo, hi) for each of the chunks
// contiguous ranges [lo, hi) of [0, n), on min(workers, chunks)
// goroutines that it owns and waits for before returning (02 req 78:
// a bounded pool per call, no goroutine outliving it). worker is in
// [0, min(workers, chunks)), so fn can use per-worker scratch. Chunks are
// handed out in order; once ctx is done or a call fails no further chunk
// starts. The result is the error of the lowest failing chunk, or ctx's
// error, so it does not depend on scheduling when fn is deterministic.
func parallel(ctx context.Context, n, workers, chunks int, fn func(worker, chunk, lo, hi int) error) error {
	bounds := func(k int) (int, int) { return k * n / chunks, (k + 1) * n / chunks }
	if workers <= 1 || chunks <= 1 {
		for k := range chunks {
			if err := ctx.Err(); err != nil {
				return err
			}
			lo, hi := bounds(k)
			if err := fn(0, k, lo, hi); err != nil {
				return err
			}
		}
		return nil
	}
	errs := make([]error, chunks)
	var next atomic.Int64
	var failed atomic.Bool
	var wg sync.WaitGroup
	for w := range min(workers, chunks) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !failed.Load() {
				k := int(next.Add(1) - 1)
				if k >= chunks {
					return
				}
				err := ctx.Err()
				if err == nil {
					lo, hi := bounds(k)
					err = fn(w, k, lo, hi)
				}
				if err != nil {
					errs[k] = err
					failed.Store(true)
					return
				}
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
