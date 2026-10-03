// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// 04 req 79 (10 records/s) and 09 req 14: a token bucket with a burst of
// the rate; the first record after a gap carries the suppressed count.
func TestRateLimit_Req79(t *testing.T) {
	fake := clocktest.New(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC))
	s, out := newSink(t, Options{Clock: fake})
	h := RateLimit(s.Handler("c"), fake, 3)
	log := slog.New(h)
	derived := slog.New(h.WithAttrs([]slog.Attr{slog.Int("d", 1)}).WithGroup("g"))
	ctx := t.Context()
	for range 3 {
		log.WarnContext(ctx, "burst")
	}
	log.WarnContext(ctx, "suppressed")
	derived.WarnContext(ctx, "suppressed too (shared bucket)")
	fake.Advance(500 * time.Millisecond) // 1.5 tokens
	log.WarnContext(ctx, "after gap")
	log.WarnContext(ctx, "suppressed")
	fake.Advance(10 * time.Second) // refill caps at the burst
	for range 4 {
		derived.WarnContext(ctx, "refilled", slog.Int("x", 1))
	}
	if !h.Enabled(ctx, slog.LevelInfo) || h.Enabled(ctx, slog.LevelDebug) {
		t.Fatal("Enabled must follow the inner handler")
	}
	drain(s)
	lines := out.lines(t)
	if len(lines) != 7 {
		t.Fatalf("wrote %d lines, want 7: %q", len(lines), lines)
	}
	if m := object(t, lines[3]); m["msg"] != "after gap" || m[KeySuppressed] != float64(2) {
		t.Fatalf("line after gap = %s", lines[3])
	}
	if m := object(t, lines[4]); m["msg"] != "refilled" || m["d"] != float64(1) {
		t.Fatalf("derived line = %s", lines[4])
	}
	if g, _ := object(t, lines[4])["g"].(map[string]any); g[KeySuppressed] != float64(1) {
		t.Fatalf("suppressed count in derived line = %s", lines[4])
	}
	if RateLimit(s.Handler(""), nil, 0).(*rateLimited).l.rate != 1 {
		t.Fatal("rate below 1 must become 1")
	}
}

// scriptedClock returns the scripted times from Now, in order, as racing
// callers that read the clock before the lock would observe them.
type scriptedClock struct {
	*clocktest.Fake
	times []time.Time
}

func (c *scriptedClock) Now() time.Time {
	t := c.times[0]
	c.times = c.times[1:]
	return t
}

// 04 req 79: an older reading (a caller that read the clock before a newer
// one refilled) never moves the bucket back, so no interval is refilled
// twice.
func TestRateLimitClockOrder_Req79(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	c := &scriptedClock{Fake: clocktest.New(t0), times: []time.Time{
		t0, t0, // the burst of 2
		t0.Add(time.Second), t0.Add(time.Second), // refilled: 2 tokens
		t0.Add(500 * time.Millisecond), // a stale reading: no refill
		t0.Add(time.Second),            // the same instant again: no refill
	}}
	l := &limiter{clock: c, rate: 2, tokens: 2}
	var passed []bool
	for range 6 {
		_, ok := l.allow()
		passed = append(passed, ok)
	}
	if want := []bool{true, true, true, true, false, false}; !slices.Equal(passed, want) {
		t.Fatalf("allow = %v, want %v", passed, want)
	}
}
