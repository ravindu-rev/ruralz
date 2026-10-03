// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"fmt"
	"math/rand/v2"
	"sync/atomic"
)

// view is a test View: grades and loads by index (missing entries are
// Eligible with load 0). Its counters are atomic so concurrent tests can
// share it.
type view struct {
	grades []Grade
	loads  []int64
	calls  atomic.Int64
}

func (v *view) Grade(i int) Grade {
	v.calls.Add(1)
	if i < len(v.grades) {
		return v.grades[i]
	}
	return Eligible
}

func (v *view) Load(i int) int64 {
	if i < len(v.loads) {
		return v.loads[i]
	}
	return 0
}

// staticView is a View without call counting, for benchmarks.
type staticView struct {
	grades []Grade
	loads  []int64
}

func (v *staticView) Grade(i int) Grade { return v.grades[i] }
func (v *staticView) Load(i int) int64  { return v.loads[i] }

// eps returns n normalized Endpoints ep-00000 ... with weight w(i).
func eps(n int, w func(i int) uint32) []Endpoint {
	out := make([]Endpoint, n)
	for i := range out {
		out[i] = Endpoint{Identity: fmt.Sprintf("ep-%05d", i), Weight: w(i)}
	}
	return out
}

// ones is the weight function 1.
func ones(int) uint32 { return 1 }

// pcg returns a deterministic source.
func pcg(seed uint64) *rand.PCG { return rand.NewPCG(seed, seed^0x9e3779b97f4a7c15) }

// counts tallies picks per index.
func counts(n, picks int, pick func() int) []int {
	c := make([]int, n)
	for range picks {
		c[pick()]++
	}
	return c
}

// u32 converts a small test value.
func u32(i int) uint32 {
	return uint32(i) //nolint:gosec // G115: test weights are small and non-negative.
}

// intn returns a uniform int in [0, n) from src.
func intn(src rand.Source, n int) int {
	return int(uint64n(src, uint64(n))) //nolint:gosec // G115: the draw is below n, an int.
}

// shuffle permutes s with src (Fisher-Yates).
func shuffle[T any](src rand.Source, s []T) {
	for i := len(s) - 1; i > 0; i-- {
		j := intn(src, i+1)
		s[i], s[j] = s[j], s[i]
	}
}

// allAlgorithms lists the algorithms.
var allAlgorithms = []Algorithm{LeastRequest, RoundRobin, RingHash, Random}
