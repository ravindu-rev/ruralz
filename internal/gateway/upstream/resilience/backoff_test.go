// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"math"
	"net/http"
	"testing"
	"time"
)

// TestBackoffCeiling_05Req33 is min(250 ms, 25 ms × 2^(n−1)).
func TestBackoffCeiling_05Req33(t *testing.T) {
	want := map[int]time.Duration{
		-1: 25 * time.Millisecond, 0: 25 * time.Millisecond, 1: 25 * time.Millisecond,
		2: 50 * time.Millisecond, 3: 100 * time.Millisecond, 4: 200 * time.Millisecond,
		5: 250 * time.Millisecond, 6: 250 * time.Millisecond, 64: 250 * time.Millisecond,
		math.MaxInt: 250 * time.Millisecond,
	}
	for n, w := range want {
		if got := BackoffCeiling(n); got != w {
			t.Errorf("BackoffCeiling(%d) = %v, want %v", n, got, w)
		}
	}
}

// TestBackoffBounds_05Req33 is the backoff part of 05 section 6 test 3:
// full jitter within [0, min(250 ms, 25 ms × 2^(n−1))] with fixed draws,
// zero-jitter draws included.
func TestBackoffBounds_05Req33(t *testing.T) {
	src := newLockedSource(7)
	for n := 1; n <= 8; n++ {
		ceiling := BackoffCeiling(n)
		if got := Backoff(n, 0); got != 0 {
			t.Errorf("zero draw, retry %d: %v", n, got)
		}
		if got := Backoff(n, math.MaxUint64); got != ceiling {
			t.Errorf("largest draw, retry %d: %v, want %v", n, got, ceiling)
		}
		if got := Backoff(n, 1<<63); got != ceiling/2 && got != ceiling/2+1 {
			t.Errorf("middle draw, retry %d: %v", n, got)
		}
		prev := time.Duration(-1)
		for i := range 1000 {
			d := uint64(i) << 54
			got := Backoff(n, d)
			if got < prev {
				t.Fatalf("retry %d: Backoff is not monotonic in the draw", n)
			}
			prev = got
			if r := Backoff(n, src.Uint64()); r < 0 || r > ceiling {
				t.Fatalf("retry %d: %v outside [0, %v]", n, r, ceiling)
			}
		}
	}
}

// TestJitterOpen_05Req37 spreads openDuration by ±20%.
func TestJitterOpen_05Req37(t *testing.T) {
	d := 30 * time.Second
	if got := jitterOpen(d, 0); got != 24*time.Second {
		t.Errorf("zero draw: %v", got)
	}
	if got := jitterOpen(d, math.MaxUint64); got != 36*time.Second {
		t.Errorf("largest draw: %v", got)
	}
	src := newLockedSource(3)
	for range 10000 {
		if got := jitterOpen(d, src.Uint64()); got < 24*time.Second || got > 36*time.Second {
			t.Fatalf("%v outside ±20%%", got)
		}
	}
	if got := jitterOpen(0, 5); got != 0 {
		t.Errorf("zero duration: %v", got)
	}
	if got := jitterOpen(-time.Second, 5); got != 0 {
		t.Errorf("negative duration: %v", got)
	}
	if got := jitterOpen(math.MaxInt64, math.MaxUint64); got != math.MaxInt64 {
		t.Errorf("saturation: %v", got)
	}
	if got := jitterOpen(99, math.MaxUint64); got != 99+19 {
		t.Errorf("small duration: %v", got)
	}
}

// TestParseRetryAfter_05Req33 covers both forms of Retry-After (05
// section 6 test 3: Retry-After 11, 2, HTTP-date).
func TestParseRetryAfter_05Req33(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"2", 2 * time.Second, true},
		{"11", 11 * time.Second, true},
		{"0", 0, true},
		{" 10\t", 10 * time.Second, true},
		{"007", 7 * time.Second, true},
		{"99999999999999999999999", maxRetryAfterSeconds * time.Second, true},
		{now.Add(5 * time.Second).Format(http.TimeFormat), 5 * time.Second, true},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0, true},
		{now.Add(3 * time.Second).Format(time.RFC850), 3 * time.Second, true},
		{now.Add(4 * time.Second).Format(time.ANSIC), 4 * time.Second, true},
		{"", 0, false},
		{"   ", 0, false},
		{"-1", 0, false},
		{"1.5", 0, false},
		{"+3", 0, false},
		{"2, 3", 0, false},
		{"soon", 0, false},
		{"Fri, 31 Dec 1999 23:59:59", 0, false},
	}
	for _, tt := range tests {
		got, ok := ParseRetryAfter(tt.value, now)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ParseRetryAfter(%q) = %v, %v; want %v, %v", tt.value, got, ok, tt.want, tt.ok)
		}
	}
}

// FuzzParseRetryAfter is the Retry-After parser fuzz target of 05 section
// 6 test 28: it never panics, never returns a negative delay, and a valid
// delta-seconds value parses to its seconds (saturated).
func FuzzParseRetryAfter(f *testing.F) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for _, s := range []string{"0", "2", "11", "120", "", " 5 ", "-1", "1e3", "Sat, 26 Sep 2026 12:00:05 GMT", "Saturday, 26-Sep-26 12:00:05 GMT", "Sat Sep 26 12:00:05 2026", "99999999999999999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, ok := ParseRetryAfter(s, now)
		if d < 0 {
			t.Fatalf("negative delay %v for %q", d, s)
		}
		if !ok && d != 0 {
			t.Fatalf("invalid %q returned %v", s, d)
		}
		trimmed := s
		for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t') {
			trimmed = trimmed[1:]
		}
		for len(trimmed) > 0 && (trimmed[len(trimmed)-1] == ' ' || trimmed[len(trimmed)-1] == '\t') {
			trimmed = trimmed[:len(trimmed)-1]
		}
		if isDigits(trimmed) {
			if !ok || d%time.Second != 0 || d > maxRetryAfterSeconds*time.Second {
				t.Fatalf("delta-seconds %q gave %v, %v", s, d, ok)
			}
		}
	})
}
