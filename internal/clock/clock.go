// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package clock abstracts time for every timer-driven component (snapshot
// retirement, Drain timelines, JWKS refresh, breakers, discovery, token
// buckets). Components take a Clock in their constructor and never call
// time.Now or time.AfterFunc directly, so tests drive them with
// clocktest.Fake.
package clock

import (
	"context"
	"time"
)

// Clock is the time source. Implementations are safe for concurrent use.
type Clock interface {
	// Now returns the current time, with a monotonic reading when real.
	Now() time.Time
	// Since returns the time elapsed since t.
	Since(t time.Time) time.Duration
	// NewTimer returns a Timer that fires once after d.
	NewTimer(d time.Duration) Timer
	// AfterFunc calls f in its own goroutine after d, like time.AfterFunc.
	// f must not block for long; the owner of f cancels it with Stop.
	AfterFunc(d time.Duration, f func()) Timer
	// Sleep blocks for d or until ctx is done, returning ctx.Err() then.
	Sleep(ctx context.Context, d time.Duration) error
}

// Timer is a stoppable one-shot timer.
type Timer interface {
	// C returns the channel that receives the fire time; nil for AfterFunc timers.
	C() <-chan time.Time
	// Stop prevents the timer from firing; it reports whether it was active.
	Stop() bool
	// Reset changes the timer to fire after d; it reports whether it was active.
	Reset(d time.Duration) bool
}

// Real returns the wall clock backed by package time.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time                  { return time.Now() }
func (realClock) Since(t time.Time) time.Duration { return time.Since(t) }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{t: time.NewTimer(d)} }

func (realClock) AfterFunc(d time.Duration, f func()) Timer {
	return realTimer{t: time.AfterFunc(d, f)}
}

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time        { return r.t.C }
func (r realTimer) Stop() bool                 { return r.t.Stop() }
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }
