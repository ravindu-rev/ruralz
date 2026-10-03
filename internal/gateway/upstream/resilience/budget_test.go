// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestRetryBudgetLimit_05Req34 is max(3, floor(20% × originals)).
func TestRetryBudgetLimit_05Req34(t *testing.T) {
	tests := map[int]int{0: 3, 1: 3, 14: 3, 15: 3, 19: 3, 20: 4, 24: 4, 25: 5, 99: 19, 100: 20, 1001: 200, 1 << 40: (1 << 40) / 5}
	for originals, want := range tests {
		if got := RetryBudgetLimit(originals); got != want {
			t.Errorf("RetryBudgetLimit(%d) = %d, want %d", originals, got, want)
		}
	}
}

// TestRetryBudget_05Req34 admits retries up to the limit and releases them
// at leg end.
func TestRetryBudget_05Req34(t *testing.T) {
	var b RetryBudget
	for range 15 {
		b.BeginLeg()
	}
	for i := range 3 {
		if !b.TryRetry() {
			t.Fatalf("retry %d refused within max(3, 20%% of 15)", i+1)
		}
	}
	if b.TryRetry() {
		t.Fatal("a 4th retry was admitted with 15 originals")
	}
	for range 5 {
		b.BeginLeg()
	}
	if !b.TryRetry() {
		t.Fatal("a 4th retry was refused with 20 originals")
	}
	if b.TryRetry() {
		t.Fatal("a 5th retry was admitted with 20 originals")
	}
	if o, r := b.InFlight(); o != 20 || r != 4 {
		t.Fatalf("InFlight() = %d, %d", o, r)
	}
	b.EndLeg(1)
	b.EndLeg(0)
	if o, r := b.InFlight(); o != 18 || r != 3 {
		t.Fatalf("after two leg ends: %d, %d", o, r)
	}
	b.EndLeg(2) // a leg that made two retries releases both
	if o, r := b.InFlight(); o != 17 || r != 1 {
		t.Fatalf("after a two-retry leg ended: %d, %d", o, r)
	}
	// Unpaired ends never go below zero.
	b.EndLeg(-1)
	for range 40 {
		b.EndLeg(5)
	}
	if o, r := b.InFlight(); o != 0 || r != 0 {
		t.Fatalf("after draining: %d, %d", o, r)
	}
}

// TestRetryBudgetAmplification_05Req34: from 15 originals in flight the
// retries in flight stay within 20% of them, so amplification is at most
// 1.2×.
func TestRetryBudgetAmplification_05Req34(t *testing.T) {
	for originals := 15; originals <= 2000; originals++ {
		var b RetryBudget
		for range originals {
			b.BeginLeg()
		}
		retries := 0
		for b.TryRetry() {
			retries++
		}
		if float64(originals+retries) > 1.2*float64(originals) {
			t.Fatalf("%d originals admitted %d retries: amplification over 1.2", originals, retries)
		}
		if retries != RetryBudgetLimit(originals) {
			t.Fatalf("%d originals admitted %d retries, want %d", originals, retries, RetryBudgetLimit(originals))
		}
	}
}

// TestRetryBudgetConcurrent races legs through the budget: with n legs in
// flight, at most the limit of retries is admitted, and every count returns
// to zero.
func TestRetryBudgetConcurrent_05Req34(t *testing.T) {
	const legs = 64
	var (
		b       RetryBudget
		admit   atomic.Int64
		started sync.WaitGroup
		tried   sync.WaitGroup
		done    sync.WaitGroup
	)
	started.Add(legs)
	tried.Add(legs)
	done.Add(legs)
	gate := make(chan struct{})
	for range legs {
		go func() {
			defer done.Done()
			b.BeginLeg()
			started.Done()
			<-gate
			ok := b.TryRetry()
			if ok {
				admit.Add(1)
			}
			tried.Done()
			tried.Wait()
			n := 0
			if ok {
				n = 1
			}
			b.EndLeg(n)
		}()
	}
	started.Wait()
	close(gate)
	done.Wait()
	if got, limit := admit.Load(), int64(RetryBudgetLimit(legs)); got != limit {
		t.Fatalf("admitted %d retries, want exactly the limit %d", got, limit)
	}
	if o, r := b.InFlight(); o != 0 || r != 0 {
		t.Fatalf("InFlight() = %d, %d after every leg ended", o, r)
	}
}

// FuzzRetryBudget is 05 section 6 test 26: random legs never exceed
// max(3, 20%) retries in flight when a retry is admitted, and the budget's
// counts equal a model's after every operation. The model counts every
// retry a leg makes, from its admission to the leg's end (05 req 34), so a
// leg may hold several.
func FuzzRetryBudget(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 1, 1, 1, 3, 4})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 2, 2, 2, 1})
	f.Add([]byte{4, 4, 1, 0, 1, 2, 1, 1, 2, 2, 3, 2})
	f.Fuzz(func(t *testing.T, ops []byte) {
		var (
			b    RetryBudget
			live []int // retries each leg in flight holds
			rets int
		)
		for i, op := range ops {
			switch op % 5 {
			case 0: // a leg begins
				b.BeginLeg()
				live = append(live, 0)
			case 1, 2: // the oldest (1) or newest (2) leg wants one more retry
				if len(live) == 0 {
					continue
				}
				j := 0
				if op%5 == 2 {
					j = len(live) - 1
				}
				ok := b.TryRetry()
				if want := rets+1 <= RetryBudgetLimit(len(live)); ok != want {
					t.Fatalf("op %d: TryRetry = %v with %d legs and %d retries", i, ok, len(live), rets)
				}
				if ok {
					live[j]++
					rets++
					// The limit holds when a retry is admitted; legs ending
					// later may leave more retries than 20% of the rest.
					if rets > RetryBudgetLimit(len(live)) {
						t.Fatalf("op %d: %d retries in flight over the limit of %d legs", i, rets, len(live))
					}
				}
			default: // a leg ends (op 3: the oldest, op 4: the newest)
				if len(live) == 0 {
					continue
				}
				j := 0
				if op%5 == 4 {
					j = len(live) - 1
				}
				b.EndLeg(live[j])
				rets -= live[j]
				live = append(live[:j], live[j+1:]...)
			}
			if o, r := b.InFlight(); o != len(live) || r != rets {
				t.Fatalf("op %d: InFlight() = %d, %d; model %d, %d", i, o, r, len(live), rets)
			}
		}
	})
}

// BenchmarkRetryBudget measures one leg's budget bookkeeping.
func BenchmarkRetryBudget(b *testing.B) {
	var rb RetryBudget
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rb.BeginLeg()
			n := 0
			if rb.TryRetry() {
				n = 1
			}
			rb.EndLeg(n)
		}
	})
}
