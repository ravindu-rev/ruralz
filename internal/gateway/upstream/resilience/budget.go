// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import "sync/atomic"

// RetryBudgetLimit returns the in-flight retries the budget allows with
// originals legs in flight: max(3, floor(20% × originals)) (05 req 34).
func RetryBudgetLimit(originals int) int {
	return max(RetryBudgetMin, originals/100*RetryBudgetPercent+originals%100*RetryBudgetPercent/100)
}

// budgetShift splits RetryBudget's word: in-flight originals in the high
// 32 bits, in-flight retries in the low 32 bits.
const (
	budgetShift = 32
	budgetMask  = 1<<budgetShift - 1
)

// RetryBudget is the retry budget of one Upstream on one Node (05 req 34):
// in-flight retries never exceed max(3, floor(20% × in-flight originals))
// when one is admitted. An original is a leg in flight, from BeginLeg to
// EndLeg; a retry is in flight from the start of its backoff to the end of
// its leg, so every retry a leg makes takes its own unit (TryRetry) and the
// leg releases them all when it ends (EndLeg): a leg on its third retry
// holds three. A leg alone in flight therefore makes at most RetryBudgetMin
// retries, whatever retries.attempts allows.
//
// The zero value is ready to use; it is safe for concurrent use and never
// blocks (one compare-and-swap loop per call). It lives for the Node's
// lifetime and is carried across Hot Reloads by Upstream name (05 req 2).
type RetryBudget struct {
	state atomic.Uint64
}

// BeginLeg counts a leg in flight.
func (b *RetryBudget) BeginLeg() {
	for {
		s := b.state.Load()
		if s>>budgetShift == budgetMask {
			return // saturated; unreachable with real leg counts.
		}
		if b.state.CompareAndSwap(s, s+1<<budgetShift) {
			return
		}
	}
}

// TryRetry takes one retry unit for a retry about to start its backoff: it
// reports false, taking nothing, when one more retry in flight would pass
// the limit.
func (b *RetryBudget) TryRetry() bool {
	for {
		s := b.state.Load()
		originals, retries := int(s>>budgetShift), int(s&budgetMask)
		if retries+1 > RetryBudgetLimit(originals) {
			return false
		}
		if b.state.CompareAndSwap(s, s+1) {
			return true
		}
	}
}

// EndLeg ends a leg that took retries units with TryRetry, releasing them
// and the leg's original. Counts never go below zero, so an unpaired call
// cannot corrupt the budget.
func (b *RetryBudget) EndLeg(retries int) {
	n := uint64(max(retries, 0))
	for {
		s := b.state.Load()
		originals, inFlight := s>>budgetShift, s&budgetMask
		if originals > 0 {
			originals--
		}
		inFlight -= min(n, inFlight)
		if b.state.CompareAndSwap(s, originals<<budgetShift|inFlight) {
			return
		}
	}
}

// InFlight returns the in-flight originals and retries.
func (b *RetryBudget) InFlight() (originals, retries int) {
	s := b.state.Load()
	return int(s >> budgetShift), int(s & budgetMask)
}
