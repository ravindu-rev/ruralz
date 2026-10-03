// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"math"
	"math/bits"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// BackoffCeiling returns the backoff ceiling of retry n (1-based):
// min(250 ms, 25 ms × 2^(n−1)) (05 req 33). n below 1 counts as 1.
func BackoffCeiling(n int) time.Duration {
	switch {
	case n <= 1:
		return BackoffBase
	case n > 5:
		// 25 ms × 2^5 = 800 ms is past the cap already.
		return BackoffCap
	default:
		return min(BackoffCap, BackoffBase<<(n-1))
	}
}

// Backoff returns the full-jitter sleep of retry n (1-based) for one
// uniform 64-bit draw: uniform(0, BackoffCeiling(n)), both ends included
// (05 req 33). A zero draw sleeps 0 and the largest draw sleeps the
// ceiling; the mapping is monotonic in draw.
func Backoff(n int, draw uint64) time.Duration {
	return uniform(draw, BackoffCeiling(n))
}

// uniform maps a uniform 64-bit draw onto [0, span] without modulo bias:
// the high word of draw × (span + 1). A negative span yields 0.
func uniform(draw uint64, span time.Duration) time.Duration {
	if span <= 0 {
		return 0
	}
	hi, _ := bits.Mul64(draw, uint64(span)+1)
	return time.Duration(hi) //nolint:gosec // G115: hi <= span, a positive Duration.
}

// jitterOpen returns openDuration jittered by OpenJitterPercent either way,
// uniform over [d − 20%, d + 20%] for one draw (05 req 37). The result
// saturates instead of overflowing.
func jitterOpen(d time.Duration, draw uint64) time.Duration {
	if d <= 0 {
		return 0
	}
	spread := d / 100 * OpenJitterPercent
	spread += d % 100 * OpenJitterPercent / 100
	lo := d - spread
	j := uniform(draw, 2*spread)
	if lo > math.MaxInt64-j {
		return math.MaxInt64
	}
	return lo + j
}

// maxRetryAfterSeconds bounds a parsed delta-seconds value so the
// Duration never overflows; anything this large is far past MaxRetryAfter.
const maxRetryAfterSeconds = 1 << 32

// ParseRetryAfter parses an Upstream Retry-After field value (RFC 9110
// section 10.2.3): delta-seconds or an HTTP-date, which it turns into the
// delay from now (0 for a date in the past). ok is false for an empty or
// invalid value, which the retry decision ignores. Delays past about 136
// years saturate.
func ParseRetryAfter(value string, now time.Time) (delay time.Duration, ok bool) {
	v := strings.Trim(value, " \t")
	if v == "" {
		return 0, false
	}
	if isDigits(v) {
		var secs int64
		for i := 0; i < len(v); i++ {
			secs = min(secs*10+int64(v[i]-'0'), maxRetryAfterSeconds)
		}
		return time.Duration(secs) * time.Second, true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	return max(t.Sub(now), 0), true
}

// isDigits reports whether s is one or more ASCII digits.
func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// draw returns one uniform 64-bit value from src, or from the math/rand/v2
// global source when src is nil.
func draw(src rand.Source) uint64 {
	if src == nil {
		return rand.Uint64() //nolint:gosec // G404: backoff and open jitter need a uniform draw, not an unpredictable one.
	}
	return src.Uint64()
}
