// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// KeySuppressed counts the records a rate-limited handler dropped since
// the previous record it passed.
const KeySuppressed = "suppressed"

// limiter is a token bucket shared by a rate-limited handler and the
// handlers derived from it. Its lock guards arithmetic only, never I/O.
type limiter struct {
	clock      clock.Clock
	rate       float64
	mu         sync.Mutex
	tokens     float64
	last       time.Time
	suppressed uint64
}

// allow takes a token; it returns the records suppressed since the last
// allowed one, and false when the bucket is empty.
//
// The clock is read under the lock and last only moves forward, so
// concurrent callers never refill the same interval twice.
func (l *limiter) allow() (uint64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	switch {
	case l.last.IsZero():
		l.last = now
	case now.After(l.last):
		l.tokens = min(l.rate, l.tokens+now.Sub(l.last).Seconds()*l.rate)
		l.last = now
	}
	if l.tokens < 1 {
		l.suppressed++
		return 0, false
	}
	l.tokens--
	n := l.suppressed
	l.suppressed = 0
	return n, true
}

// rateLimited passes at most rate records per second to its inner handler
// (a burst of rate), adding KeySuppressed to the first record after a gap.
type rateLimited struct {
	inner slog.Handler
	l     *limiter
}

// RateLimit returns a handler that passes at most perSecond records per
// second to h, with bursts up to perSecond; the first record passed after
// others were suppressed carries their number as KeySuppressed. It is used
// for http.Server.ErrorLog (04 req 79) and the OpenTelemetry and gRPC
// error adapters (09 req 14). A perSecond below 1 is treated as 1.
func RateLimit(h slog.Handler, c clock.Clock, perSecond int) slog.Handler {
	if c == nil {
		c = clock.Real()
	}
	rate := float64(max(perSecond, 1))
	return &rateLimited{inner: h, l: &limiter{clock: c, rate: rate, tokens: rate}}
}

// Enabled reports the inner handler's decision.
func (r *rateLimited) Enabled(ctx context.Context, l slog.Level) bool {
	return r.inner.Enabled(ctx, l)
}

// Handle passes rec when a token is available.
func (r *rateLimited) Handle(ctx context.Context, rec slog.Record) error {
	n, ok := r.l.allow()
	if !ok {
		return nil
	}
	if n > 0 {
		rec = rec.Clone()
		rec.AddAttrs(slog.Uint64(KeySuppressed, n))
	}
	return r.inner.Handle(ctx, rec)
}

// WithAttrs derives the inner handler; the bucket is shared.
func (r *rateLimited) WithAttrs(as []slog.Attr) slog.Handler {
	return &rateLimited{inner: r.inner.WithAttrs(as), l: r.l}
}

// WithGroup derives the inner handler; the bucket is shared.
func (r *rateLimited) WithGroup(name string) slog.Handler {
	return &rateLimited{inner: r.inner.WithGroup(name), l: r.l}
}
