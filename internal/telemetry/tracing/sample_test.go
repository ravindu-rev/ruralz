// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// idWithLow returns a trace ID whose low 8 bytes are low.
func idWithLow(low uint64) [16]byte {
	var id [16]byte
	id[0] = 1
	binary.BigEndian.PutUint64(id[8:], low)
	return id
}

// TestRatioSampled covers spec 09 req 28: 0 never, 1 always, the bound
// exact at the edge, and agreement with the SDK's TraceIDRatioBased.
func TestRatioSampled(t *testing.T) {
	onePercent := 0.01
	bound := uint64(onePercent * (1 << 63))
	for _, tc := range []struct {
		name  string
		id    [16]byte
		ratio float64
		want  bool
	}{
		{"ratio 0", idWithLow(0), 0, false},
		{"ratio negative", idWithLow(0), -1, false},
		{"ratio NaN", idWithLow(0), math.NaN(), false},
		{"ratio 1 max ID", idWithLow(math.MaxUint64), 1, true},
		{"ratio above 1", idWithLow(math.MaxUint64), 2, true},
		{"just below bound", idWithLow((bound - 1) << 1), onePercent, true},
		{"at bound", idWithLow(bound << 1), onePercent, false},
		{"low bit ignored", idWithLow((bound-1)<<1 | 1), onePercent, true},
		{"high bytes ignored", [16]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, 0.5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RatioSampled(tc.id, tc.ratio); got != tc.want {
				t.Fatalf("RatioSampled = %v, want %v", got, tc.want)
			}
		})
	}
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: deterministic test data
	for _, ratio := range []float64{0, 0.001, 0.01, 0.05, 0.5, 0.999, 1} {
		sampler := sdktrace.TraceIDRatioBased(ratio)
		for range 2000 {
			var id [16]byte
			binary.BigEndian.PutUint64(id[:8], rng.Uint64())
			binary.BigEndian.PutUint64(id[8:], rng.Uint64())
			sdk := sampler.ShouldSample(sdktrace.SamplingParameters{TraceID: trace.TraceID(id)}).Decision == sdktrace.RecordAndSample
			if got := RatioSampled(id, ratio); got != sdk {
				t.Fatalf("ratio %v, id %x: RatioSampled %v, SDK %v", ratio, id, got, sdk)
			}
		}
	}
}

// TestRatioSampledFraction is spec 09 property test 25: over 10^6 random
// IDs the sampled fraction is within 4 standard deviations of the ratio.
func TestRatioSampledFraction(t *testing.T) {
	const n = 1_000_000
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // G404: deterministic test data
	for _, ratio := range []float64{0.01, 0.05, 0.25, 0.5} {
		hits := 0
		for range n {
			var id [16]byte
			binary.BigEndian.PutUint64(id[:8], rng.Uint64())
			binary.BigEndian.PutUint64(id[8:], rng.Uint64())
			if RatioSampled(id, ratio) {
				hits++
			}
		}
		sigma := math.Sqrt(ratio * (1 - ratio) / n)
		if got := float64(hits) / n; math.Abs(got-ratio) > 4*sigma {
			t.Errorf("ratio %v: sampled fraction %v outside 4σ (%v)", ratio, got, 4*sigma)
		}
	}
}

// TestLimiter covers the GCRA arithmetic of spec 09 req 29.
func TestLimiter(t *testing.T) {
	t.Run("burst then rate", func(t *testing.T) {
		l := NewLimiter(1000, 1000)
		admitted := 0
		for range 5000 {
			if l.Allow(0) {
				admitted++
			}
		}
		if admitted != 1000 {
			t.Fatalf("burst admitted %d, want 1000", admitted)
		}
		if l.Allow(500 * time.Microsecond) {
			t.Fatal("admitted before one emission interval passed")
		}
		if !l.Allow(time.Millisecond) || l.Allow(time.Millisecond) {
			t.Fatal("one interval must admit exactly one")
		}
		admitted = 0
		for i := range 2000 {
			if l.Allow(time.Millisecond + time.Duration(i)*time.Millisecond/2) {
				admitted++
			}
		}
		if admitted < 999 || admitted > 1001 {
			t.Fatalf("admitted %d over one second at twice the rate, want about 1000", admitted)
		}
	})
	t.Run("idle refills to burst only", func(t *testing.T) {
		l := NewLimiter(500, 500)
		admitted := 0
		for range 2000 {
			if l.Allow(time.Hour) {
				admitted++
			}
		}
		if admitted != 500 {
			t.Fatalf("after idle admitted %d, want 500", admitted)
		}
	})
	t.Run("zero rate admits nothing", func(t *testing.T) {
		for _, l := range []*Limiter{NewLimiter(0, 10), NewLimiter(10, 0), {}} {
			if l.Allow(time.Hour) {
				t.Fatal("admitted")
			}
		}
	})
}

// TestLimiterWindowProperty is spec 09 property test 24: over any window W
// the admitted count is at most rate × W + burst, under concurrent callers
// (run with -race).
func TestLimiterWindowProperty(t *testing.T) {
	const (
		rate    = 1000
		burst   = 1000
		workers = 8
		calls   = 4000
	)
	l := NewLimiter(rate, burst)
	var clock atomic.Int64 // simulated time, ns
	var mu sync.Mutex
	var admitted []int64
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 3)) //nolint:gosec // G404: deterministic test data
			var mine []int64
			for range calls {
				now := clock.Add(int64(rng.IntN(300)) * int64(time.Microsecond))
				if l.Allow(time.Duration(now)) {
					mine = append(mine, now)
				}
			}
			mu.Lock()
			admitted = append(admitted, mine...)
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.Sort(admitted)
	// For every admitted call as a window start, count the admissions in
	// windows of several lengths.
	for _, w := range []time.Duration{time.Millisecond, 10 * time.Millisecond, 100 * time.Millisecond, time.Second, 3 * time.Second} {
		limit := int(float64(rate)*w.Seconds()) + burst
		j := 0
		for i := range admitted {
			for j < len(admitted) && admitted[j] < admitted[i]+int64(w) {
				j++
			}
			if n := j - i; n > limit {
				t.Fatalf("window %v from %d admitted %d, limit %d", w, admitted[i], n, limit)
			}
		}
	}
	if len(admitted) == 0 {
		t.Fatal("nothing admitted")
	}
}
