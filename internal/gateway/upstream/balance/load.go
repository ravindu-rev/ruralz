// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"math/bits"
	"sync/atomic"
	"time"
)

// The failure window is failBuckets buckets of failBucket each; a bucket
// packs its epoch (bucket number since the Unix epoch, 40 bits) above a
// 24-bit saturating count, so one compare-and-swap updates both.
const (
	failBuckets = 10
	failBucket  = FailureWindow / failBuckets
	countBits   = 24
	countMask   = 1<<countBits - 1
	epochMask   = 1<<(64-countBits) - 1
)

// LoadCounter tracks one Endpoint's least-request load inputs (05 req 13):
// attempts in flight and attempts that matched failureWhen in the last
// second, the latter in ten 100 ms buckets. It is lock-free, safe for
// concurrent use and its zero value is ready. The Upstream runtime keeps one
// per Endpoint identity, so the counts survive rebuilds and Hot Reloads,
// and reports LoadCounter.Score through View.Load.
type LoadCounter struct {
	inFlight atomic.Int64
	fails    [failBuckets]atomic.Uint64
}

// Begin records an attempt start.
func (c *LoadCounter) Begin() { c.inFlight.Add(1) }

// End records an attempt end: its response body closed, failed or was
// abandoned (in-flight counts the whole attempt, 05 req 13).
func (c *LoadCounter) End() { c.inFlight.Add(-1) }

// InFlight returns the attempts in flight (never below 0).
func (c *LoadCounter) InFlight() int64 { return max(c.inFlight.Load(), 0) }

// Fail records an attempt that matched failureWhen at now.
func (c *LoadCounter) Fail(now time.Time) {
	e := epoch(now)
	b := &c.fails[e%failBuckets]
	for {
		old := b.Load()
		var next uint64
		switch {
		case old>>countBits != e:
			next = e<<countBits | 1
		case old&countMask == countMask:
			return
		default:
			next = old + 1
		}
		if b.CompareAndSwap(old, next) {
			return
		}
	}
}

// Failures returns the failures recorded within the last second before now
// (the current 100 ms bucket and the nine before it).
func (c *LoadCounter) Failures(now time.Time) int64 {
	e := epoch(now)
	var n int64
	for i := range c.fails {
		v := c.fails[i].Load()
		if (e-v>>countBits)&epochMask < failBuckets {
			n += int64(v & countMask)
		}
	}
	return n
}

// Score returns InFlight + Failures, the numerator of the least-request
// score.
func (c *LoadCounter) Score(now time.Time) int64 { return c.InFlight() + c.Failures(now) }

// epoch returns now's bucket number, 0 before the Unix epoch.
func epoch(now time.Time) uint64 {
	ns := max(now.UnixNano(), 0)
	return uint64(ns/int64(failBucket)) & epochMask
}

// EndpointCap is the per-Endpoint in-flight cap of 05 req 36,
// max(8, floor(min(2 × w_i/Σw, 0.5) × maxConnections)): the load bound of
// the balancer. An Endpoint at its cap is graded Avoided, so ring-hash walks
// past it to the next Endpoint on the ring (a consistent hash with bounded
// load) and every algorithm skips it while another Endpoint remains.
func EndpointCap(weight uint32, totalWeight uint64, maxConnections int) int {
	const floor = 8
	if totalWeight == 0 || maxConnections <= 0 {
		return floor
	}
	if 4*uint64(weight) >= totalWeight {
		return max(floor, maxConnections/2)
	}
	// 2 × w × maxConnections ÷ Σw < maxConnections ÷ 2 here, so it fits an
	// int.
	hi, lo := bits.Mul64(2*uint64(weight), uint64(maxConnections))
	q, _ := bits.Div64(hi, lo, totalWeight)
	return max(floor, int(q)) //nolint:gosec // G115: q < maxConnections/2, an int.
}
