// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// TestForEach covers the bounded worker pool: every index once, inline
// and pooled, more workers than units, and cancellation before and
// during dispatch with every started call joined.
func TestForEach(t *testing.T) {
	for _, workers := range []int{0, 1, 3, 64} {
		for _, n := range []int{0, 1, 2, 17} {
			seen := make([]int32, n)
			if err := forEach(t.Context(), workers, n, func(i int) { atomic.AddInt32(&seen[i], 1) }); err != nil {
				t.Fatalf("workers %d n %d: %v", workers, n, err)
			}
			for i, c := range seen {
				if c != 1 {
					t.Errorf("workers %d n %d: index %d called %d times", workers, n, i, c)
				}
			}
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, workers := range []int{1, 4} {
		var calls atomic.Int32
		if err := forEach(ctx, workers, 10, func(int) { calls.Add(1) }); !errors.Is(err, context.Canceled) || calls.Load() != 0 {
			t.Errorf("workers %d: canceled before dispatch: err %v, %d calls", workers, err, calls.Load())
		}
	}

	for _, workers := range []int{1, 4} {
		ctx, cancel := context.WithCancel(t.Context())
		var calls, running atomic.Int32
		err := forEach(ctx, workers, 1000, func(i int) {
			running.Add(1)
			defer running.Add(-1)
			calls.Add(1)
			if i == 5 {
				cancel()
			}
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("workers %d: err = %v, want context.Canceled", workers, err)
		}
		if c := calls.Load(); c < 6 || c >= 1000 {
			t.Errorf("workers %d: %d calls, want dispatch to stop soon after the cancel", workers, c)
		}
		if r := running.Load(); r != 0 {
			t.Errorf("workers %d: %d calls still running after forEach returned", workers, r)
		}
		cancel()
	}
}
