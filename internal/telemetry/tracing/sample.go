// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"encoding/binary"
	"sync/atomic"
	"time"
)

// DefaultTraceSampling is the root ratio when a Revision sets no
// traceSampling (OQ-observability-1 (a); spec 09 req 28).
const DefaultTraceSampling = 0.01

// Default per-Node caps on sampled decisions per second; bursts equal the
// rates (spec 09 req 29, target).
const (
	DefaultRootPerSecond   = 1000
	DefaultParentPerSecond = 500
)

// RatioSampled applies ratio to traceID exactly as the SDK's
// TraceIDRatioBased sampler does (spec 09 req 28): the low 8 bytes, big
// endian, shifted right by one, are below ratio × 2^63. A ratio of 0 or
// less, or NaN, never samples; 1 or more always samples.
func RatioSampled(traceID [16]byte, ratio float64) bool {
	if !(ratio > 0) {
		return false
	}
	if ratio >= 1 {
		return true
	}
	bound := uint64(ratio * (1 << 63))
	return binary.BigEndian.Uint64(traceID[8:])>>1 < bound
}

// Limiter is a lock-free GCRA token bucket on one atomic word (spec 09
// req 29): with emission interval T = 1 s / rate and tolerance τ = burst ×
// T, a call at time now is admitted iff max(tat, now) + T − now ≤ τ, and
// admission installs max(tat, now) + T as the new theoretical arrival
// time with a compare-and-swap. Over any window W it admits at most
// rate × W + burst calls. The zero Limiter admits nothing.
type Limiter struct {
	tat       atomic.Int64
	interval  int64
	tolerance int64
}

// NewLimiter returns a Limiter admitting rate calls per second, at most
// 10^9, with the given burst; rate or burst below 1 admits nothing.
func NewLimiter(rate, burst int) *Limiter {
	l := &Limiter{}
	if rate >= 1 && burst >= 1 {
		l.interval = max(int64(time.Second)/int64(rate), 1)
		l.tolerance = int64(burst) * l.interval
	}
	return l
}

// Allow reports whether a call at now, a monotonic offset from any fixed
// origin, is admitted. It never blocks and never allocates.
func (l *Limiter) Allow(now time.Duration) bool {
	if l.interval <= 0 {
		return false
	}
	n := int64(now)
	for {
		tat := l.tat.Load()
		next := max(tat, n) + l.interval
		if next-n > l.tolerance {
			return false
		}
		if l.tat.CompareAndSwap(tat, next) {
			return true
		}
	}
}
