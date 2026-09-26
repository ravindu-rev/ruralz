// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package clocktest provides a manually advanced clock.Clock for tests.
package clocktest

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Fake is a clock.Clock whose time moves only through Advance and Set.
// Timers with a positive duration fire synchronously inside Advance, in
// deadline order, and their AfterFunc callbacks run on the goroutine that
// calls Advance. Like clock.Real, a zero or negative duration fires at
// once: NewTimer's channel is already filled, AfterFunc runs its callback
// in a new goroutine, and Sleep returns without waiting.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

// New returns a Fake set to start.
func New(start time.Time) *Fake { return &Fake{now: start} }

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Since returns the fake time elapsed since t.
func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// NewTimer returns a timer whose channel receives on Advance, or at once
// when d <= 0.
func (f *Fake) NewTimer(d time.Duration) clock.Timer {
	return f.add(&fakeTimer{f: f, ch: make(chan time.Time, 1)}, d)
}

// AfterFunc returns a timer that calls fn on Advance, or at once in a new
// goroutine when d <= 0.
func (f *Fake) AfterFunc(d time.Duration, fn func()) clock.Timer {
	return f.add(&fakeTimer{f: f, fn: fn}, d)
}

// Sleep blocks until Advance passes d or ctx is done; d <= 0 returns at
// once (ctx.Err() when ctx is already done).
func (f *Fake) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := f.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C():
		return nil
	}
}

// Advance moves time forward by d and fires every timer due by then.
func (f *Fake) Advance(d time.Duration) { f.Set(f.Now().Add(d)) }

// Set moves time to t (never backwards) and fires every timer due by then.
func (f *Fake) Set(t time.Time) {
	for {
		f.mu.Lock()
		if t.Before(f.now) {
			t = f.now
		}
		slices.SortStableFunc(f.timers, func(a, b *fakeTimer) int { return a.at.Compare(b.at) })
		if len(f.timers) == 0 || f.timers[0].at.After(t) {
			f.now = t
			f.mu.Unlock()
			return
		}
		ft := f.timers[0]
		f.timers = f.timers[1:]
		f.now = ft.at
		f.mu.Unlock()
		ft.fire(false)
	}
}

// Pending returns the number of armed timers.
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

// add arms ft to fire after d; d <= 0 fires it at once without arming.
func (f *Fake) add(ft *fakeTimer, d time.Duration) *fakeTimer {
	f.mu.Lock()
	ft.at = f.now.Add(max(d, 0))
	if d > 0 {
		f.timers = append(f.timers, ft)
	}
	f.mu.Unlock()
	if d <= 0 {
		ft.fire(true)
	}
	return ft
}

func (f *Fake) remove(ft *fakeTimer) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.Index(f.timers, ft)
	if i < 0 {
		return false
	}
	f.timers = slices.Delete(f.timers, i, i+1)
	return true
}

type fakeTimer struct {
	f  *Fake
	at time.Time
	ch chan time.Time
	fn func()
}

// fire delivers the timer: a channel send that never blocks, or the
// callback (in a new goroutine when async, as time.AfterFunc does).
func (t *fakeTimer) fire(async bool) {
	switch {
	case t.fn != nil && async:
		go t.fn()
	case t.fn != nil:
		t.fn()
	default:
		select {
		case t.ch <- t.at:
		default:
		}
	}
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }
func (t *fakeTimer) Stop() bool          { return t.f.remove(t) }

func (t *fakeTimer) Reset(d time.Duration) bool {
	active := t.f.remove(t)
	t.f.add(t, d)
	return active
}
