// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"context"
	"sync"
)

// forEach calls fn(i) for every i in [0, n) on at most workers goroutines
// (inline when workers is below 2 or n below 2). Work is fed through a
// channel bounded by the worker count; dispatch stops when ctx ends, and
// forEach waits for every started call before it returns, so no goroutine
// outlives it. It returns ctx.Err() when ctx ended before every index was
// dispatched, and nil otherwise. fn must write only state owned by its
// index.
func forEach(ctx context.Context, workers, n int, fn func(i int)) error {
	if workers < 2 || n < 2 {
		for i := range n {
			if err := ctx.Err(); err != nil {
				return err
			}
			fn(i)
		}
		return nil
	}
	workers = min(workers, n)
	jobs := make(chan int, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for i := range jobs {
				fn(i)
			}
		}()
	}
	var err error
	for i := range n {
		if err = ctx.Err(); err != nil {
			break
		}
		select {
		case jobs <- i:
		case <-ctx.Done():
			err = ctx.Err()
		}
		if err != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	return err
}
