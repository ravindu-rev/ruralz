// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"math"
	"testing"
	"time"
)

// Tests for the bucket arithmetic: the continuous refill in whole tokens,
// the fail-open refill per epoch-aligned window, its 60 steps past a
// minute, and the waits a denial reports.

const sec = int64(time.Second)

// TestRefillWholeTokens checks the continuous refill: ceiling tokens per
// window, one token per window / ceiling, the fraction of the next token
// carried in last, and the capacity bound.
func TestRefillWholeTokens(t *testing.T) {
	tokens, last := int64(0), int64(0)
	// 10 per second: one token per 100 ms.
	refill(&tokens, &last, 99*int64(time.Millisecond), 10, sec, 10)
	if tokens != 0 || last != 0 {
		t.Fatalf("after 99ms: %d tokens, last %d; want none yet", tokens, last)
	}
	refill(&tokens, &last, 250*int64(time.Millisecond), 10, sec, 10)
	if tokens != 2 || last != 200*int64(time.Millisecond) {
		t.Fatalf("after 250ms: %d tokens, last %v; want 2, the fraction kept from 200ms", tokens, time.Duration(last))
	}
	refill(&tokens, &last, 300*int64(time.Millisecond), 10, sec, 10)
	if tokens != 3 {
		t.Fatalf("after 300ms: %d tokens, want 3: the carried fraction completed a token", tokens)
	}
	refill(&tokens, &last, 10*sec, 10, sec, 10)
	if tokens != 10 || last != 10*sec {
		t.Fatalf("after 10s: %d tokens, last %v; want full at 10, no fraction", tokens, time.Duration(last))
	}
	// A bucket above a lowered capacity is clamped.
	tokens = 50
	refill(&tokens, &last, 11*sec, 10, sec, 10)
	if tokens != 10 {
		t.Fatalf("over capacity: %d tokens, want 10", tokens)
	}
	// A clock that steps back adds nothing.
	tokens, last = 3, 20*sec
	refill(&tokens, &last, 19*sec, 10, sec, 10)
	if tokens != 3 || last != 20*sec {
		t.Fatalf("clock back: %d tokens, last %v", tokens, time.Duration(last))
	}
}

// TestRefillUnevenRate checks a rate whose token interval is not a whole
// nanosecond count: 3 per second over many small steps never runs ahead of
// the rate, and runs late by at most a nanosecond per refill that added
// tokens.
func TestRefillUnevenRate(t *testing.T) {
	tokens, last := int64(0), int64(0)
	adds := int64(0)
	for now := int64(0); now <= 10*sec; now += 7_000_001 {
		before := tokens
		refill(&tokens, &last, now, 3, sec, 1_000)
		if allowed := now * 3 / sec; tokens > allowed {
			t.Fatalf("at %v: %d tokens, ahead of the %d the rate allows", time.Duration(now), tokens, allowed)
		}
		if tokens > before {
			adds++
		}
	}
	refill(&tokens, &last, 10*sec+adds, 3, sec, 1_000)
	if tokens != 30 {
		t.Fatalf("3 per second over 10 s plus %dns = %d tokens, want 30", adds, tokens)
	}
}

// TestRefillHugeElapsed checks that an elapsed time whose product with the
// rate overflows 64 bits fills the bucket instead of wrapping.
func TestRefillHugeElapsed(t *testing.T) {
	tokens, last := int64(0), int64(0)
	refill(&tokens, &last, math.MaxInt64, math.MaxInt64/2, 1, 5)
	if tokens != 5 || last != math.MaxInt64 {
		t.Fatalf("overflowing refill: %d tokens, last %d; want full", tokens, last)
	}
}

// TestFillTime checks the wait until a bucket holds a count.
func TestFillTime(t *testing.T) {
	ms := int64(time.Millisecond)
	for _, tc := range []struct {
		tokens, last, now, want int64
		got                     time.Duration
	}{
		{tokens: 5, last: 0, now: 0, want: 5, got: 0},
		{tokens: 0, last: 0, now: 0, want: 1, got: 100 * time.Millisecond},
		{tokens: 0, last: 0, now: 30 * ms, want: 1, got: 70 * time.Millisecond},
		{tokens: 2, last: 0, now: 0, want: 4, got: 200 * time.Millisecond},
		{tokens: 0, last: 0, now: 0, want: 10, got: time.Second},
	} {
		if got := fillTime(tc.tokens, tc.last, tc.now, tc.want, 10, sec); got != tc.got {
			t.Errorf("fillTime(%d tokens, last %d, now %d, want %d) = %v, want %v", tc.tokens, tc.last, tc.now, tc.want, got, tc.got)
		}
	}
}

// TestReleasedSteps checks the fail-open release: a window up to a minute
// releases the whole allowance at its start, a longer one in 60 equal
// steps, the last step releasing the remainder.
func TestReleasedSteps(t *testing.T) {
	if got := released(0, int64(time.Minute), 7); got != 7 {
		t.Errorf("released at a 1m window's start = %d, want the whole 7", got)
	}
	hour := int64(time.Hour)
	step := hour / 60
	for _, tc := range []struct{ into, allowance, want int64 }{
		{0, 600, 10},
		{step - 1, 600, 10},
		{step, 600, 20},
		{30 * step, 600, 310},
		{hour - 1, 600, 600},
		{0, 30, 0},
		{step, 30, 1},
		{59 * step, 30, 30},
	} {
		if got := released(tc.into, hour, tc.allowance); got != tc.want {
			t.Errorf("released(%v into a 1h window, allowance %d) = %d, want %d", time.Duration(tc.into), tc.allowance, got, tc.want)
		}
	}
}

// TestFailOpenRefillArithmetic checks the fail-open refill: the window of the switch
// releases nothing, keeping the tokens held; each later window brings its
// allowance without carry-over.
func TestFailOpenRefillArithmetic(t *testing.T) {
	// The switch at 10.5 s with 3 tokens held, window 1 s, allowance 5.
	tokens, rel, start := int64(3), int64(-1), int64(10*sec)
	failOpenRefill(&tokens, &rel, &start, 10*sec+900*int64(time.Millisecond), sec, 5)
	if tokens != 3 {
		t.Fatalf("within the switch window: %d tokens, want the 3 held", tokens)
	}
	tokens = 1
	failOpenRefill(&tokens, &rel, &start, 11*sec, sec, 5)
	if tokens != 5 || start != 11*sec {
		t.Fatalf("next window: %d tokens from %v, want the allowance 5 from 11s", tokens, time.Duration(start))
	}
	tokens = 4
	failOpenRefill(&tokens, &rel, &start, 13*sec+1, sec, 5)
	if tokens != 5 {
		t.Fatalf("two windows later: %d tokens, want 5 without carry-over", tokens)
	}
}

// TestFailOpenRefillSteps checks a window past a minute: the allowance
// arrives in 60 steps, unused releases stay within the window, and the
// next window starts over.
func TestFailOpenRefillSteps(t *testing.T) {
	hour := int64(time.Hour)
	step := hour / 60
	tokens, rel, start := int64(0), int64(0), int64(0)
	failOpenRefill(&tokens, &rel, &start, hour, hour, 120) // a new window: step 0
	if tokens != 2 || rel != 2 {
		t.Fatalf("first step: %d tokens, released %d; want 2", tokens, rel)
	}
	tokens--
	failOpenRefill(&tokens, &rel, &start, hour+10*step+1, hour, 120)
	if tokens != 21 || rel != 22 {
		t.Fatalf("step 10: %d tokens, released %d; want 21 (22 released, 1 taken)", tokens, rel)
	}
	failOpenRefill(&tokens, &rel, &start, 2*hour, hour, 120)
	if tokens != 2 || rel != 2 {
		t.Fatalf("next window: %d tokens, released %d; want 2, no carry-over", tokens, rel)
	}
}

// TestFailOpenWait checks the wait a fail-open bucket reports: a later
// step of the window, or the next window's first sufficient step.
func TestFailOpenWait(t *testing.T) {
	hour := int64(time.Hour)
	step := hour / 60
	for _, tc := range []struct {
		name                    string
		tokens, rel, start, now int64
		window, allowance, want int64
		got                     time.Duration
	}{
		{"holds", 3, 3, 0, 5, sec, 5, 3, 0},
		{"switch window", 0, -1, 0, sec / 4, sec, 5, 1, 750 * time.Millisecond},
		{"whole window next", 0, 5, 0, sec / 2, sec, 5, 1, 500 * time.Millisecond},
		{"later step", 0, 2, 0, 1, hour, 120, 2, time.Duration(step - 1)},
		{"switch stepped", 0, -1, 0, 0, hour, 120, 4, time.Duration(hour + step)},
		{"above allowance", 0, 5, 0, sec / 2, sec, 5, 6, 500 * time.Millisecond},
	} {
		if got := failOpenWait(tc.tokens, tc.rel, tc.start, tc.now, tc.window, tc.allowance, tc.want); got != tc.got {
			t.Errorf("%s: failOpenWait = %v, want %v", tc.name, got, tc.got)
		}
	}
}

// TestCeilingOf checks the per-Node ceiling and fail-open allowance of
// each mode, with the Per-Node ceiling examples: 10 per 1s on 100 Nodes is
// a ceiling of 10 (the floor) but an allowance of 1.
func TestCeilingOf(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		requests, perNode, n       int64
		control                    bool
		wantCeiling, wantAllowance int64
	}{
		{"declared", 100, 7, 50, true, 7, 7},
		{"declared in file mode", 100, 7, 0, false, 7, 7},
		{"file mode", 100, 0, 0, false, 100, 100},
		{"derived floor", 10, 0, 100, true, 10, 1},
		{"derived", 1000, 0, 10, true, 200, 200},
		{"derived capped by requests", 15, 0, 2, true, 15, 15},
		{"derived ceil", 1000, 0, 3, true, 667, 666},
		{"countless", 1000, 0, 0, true, 1000, 10},
		{"countless floor", 50, 0, 0, true, 50, 1},
	} {
		c, a := ceilingOf(tc.requests, tc.perNode, tc.n, tc.control)
		if c != tc.wantCeiling || a != tc.wantAllowance {
			t.Errorf("%s: ceilingOf = %d, %d; want %d, %d", tc.name, c, a, tc.wantCeiling, tc.wantAllowance)
		}
	}
}
