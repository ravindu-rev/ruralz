// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clocktest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Tests for Fake ordering, Stop and Reset results, AfterFunc in Advance,
// Sleep cancellation, zero or negative durations firing at once like
// clock.Real, and BlockUntil waking on every change to the armed timers.

var _ clock.Clock = (*Fake)(nil)

func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// waitFor polls cond with a real-time bound; it only waits for goroutines
// the test started, never for fake time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestNowSinceAdvanceSet(t *testing.T) {
	f := New(epoch())
	if got := f.Now(); !got.Equal(epoch()) {
		t.Fatalf("Now = %v, want %v", got, epoch())
	}
	f.Advance(90 * time.Second)
	if got := f.Since(epoch()); got != 90*time.Second {
		t.Fatalf("Since = %v, want 90s", got)
	}
	f.Set(epoch().Add(time.Hour))
	if got := f.Now(); !got.Equal(epoch().Add(time.Hour)) {
		t.Fatalf("Now after Set = %v", got)
	}
	// Set never moves time backwards.
	f.Set(epoch())
	if got := f.Now(); !got.Equal(epoch().Add(time.Hour)) {
		t.Fatalf("Set moved backwards to %v", got)
	}
	f.Advance(-time.Minute)
	if got := f.Now(); !got.Equal(epoch().Add(time.Hour)) {
		t.Fatalf("negative Advance moved backwards to %v", got)
	}
}

func TestTimersFireInDeadlineOrder(t *testing.T) {
	f := New(epoch())
	var order []string
	add := func(name string, d time.Duration) {
		f.AfterFunc(d, func() {
			order = append(order, name)
			if got, want := f.Now(), epoch().Add(d); !got.Equal(want) {
				t.Errorf("%s ran at %v, want its deadline %v", name, got, want)
			}
		})
	}
	add("c", 3*time.Second)
	add("a", time.Second)
	add("b", 2*time.Second)
	// Equal deadlines fire in creation order.
	add("d1", 4*time.Second)
	add("d2", 4*time.Second)
	add("d3", 4*time.Second)
	add("late", time.Minute)

	f.Advance(4 * time.Second)
	want := []string{"a", "b", "c", "d1", "d2", "d3"}
	if len(order) != len(want) {
		t.Fatalf("fired %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("fired %v, want %v", order, want)
		}
	}
	if got := f.Now(); !got.Equal(epoch().Add(4 * time.Second)) {
		t.Fatalf("Now after Advance = %v", got)
	}
	if f.Pending() != 1 {
		t.Fatalf("Pending = %d, want 1 (the late timer)", f.Pending())
	}
}

func TestEqualDeadlineChannelsAllFire(t *testing.T) {
	f := New(epoch())
	timers := make([]clock.Timer, 3)
	for i := range timers {
		timers[i] = f.NewTimer(time.Second)
	}
	f.Advance(time.Second)
	for i, tm := range timers {
		select {
		case at := <-tm.C():
			if !at.Equal(epoch().Add(time.Second)) {
				t.Errorf("timer %d delivered %v", i, at)
			}
		default:
			t.Errorf("timer %d did not fire", i)
		}
	}
}

func TestAfterFuncRunsInsideAdvance(t *testing.T) {
	f := New(epoch())
	ran := false
	tm := f.AfterFunc(time.Second, func() { ran = true })
	if tm.C() != nil {
		t.Error("AfterFunc timer C() is not nil")
	}
	f.Advance(999 * time.Millisecond)
	if ran {
		t.Fatal("AfterFunc ran before its deadline")
	}
	f.Advance(time.Millisecond)
	// The callback runs on the goroutine calling Advance, so it is done now.
	if !ran {
		t.Fatal("AfterFunc did not run inside Advance")
	}
}

func TestCallbackMayRearmDuringAdvance(t *testing.T) {
	f := New(epoch())
	var n int
	var tm clock.Timer
	tm = f.AfterFunc(time.Second, func() {
		n++
		tm.Reset(time.Second)
	})
	f.Advance(3500 * time.Millisecond)
	if n != 3 {
		t.Fatalf("periodic callback ran %d times, want 3", n)
	}
	if f.Pending() != 1 {
		t.Fatalf("Pending = %d, want 1", f.Pending())
	}
}

func TestStopResults(t *testing.T) {
	tests := []struct {
		name string
		run  func(f *Fake) bool
		want bool
	}{
		{"armed timer", func(f *Fake) bool { return f.NewTimer(time.Second).Stop() }, true},
		{"stopped twice", func(f *Fake) bool {
			tm := f.NewTimer(time.Second)
			tm.Stop()
			return tm.Stop()
		}, false},
		{"after firing", func(f *Fake) bool {
			tm := f.NewTimer(time.Second)
			f.Advance(time.Second)
			return tm.Stop()
		}, false},
		{"armed AfterFunc", func(f *Fake) bool { return f.AfterFunc(time.Second, func() {}).Stop() }, true},
		{"zero-duration timer", func(f *Fake) bool { return f.NewTimer(0).Stop() }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.run(New(epoch())); got != tt.want {
				t.Fatalf("Stop = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStoppedTimerNeverFires(t *testing.T) {
	f := New(epoch())
	tm := f.NewTimer(time.Second)
	fn := f.AfterFunc(time.Second, func() { t.Error("stopped AfterFunc ran") })
	tm.Stop()
	fn.Stop()
	f.Advance(time.Hour)
	select {
	case <-tm.C():
		t.Fatal("stopped timer fired")
	default:
	}
	if f.Pending() != 0 {
		t.Fatalf("Pending = %d, want 0", f.Pending())
	}
}

func TestResetResults(t *testing.T) {
	f := New(epoch())
	tm := f.NewTimer(time.Second)
	if !tm.Reset(2 * time.Second) {
		t.Fatal("Reset of an armed timer reported inactive")
	}
	f.Advance(time.Second)
	select {
	case <-tm.C():
		t.Fatal("timer fired at its old deadline")
	default:
	}
	f.Advance(time.Second)
	select {
	case <-tm.C():
	default:
		t.Fatal("timer did not fire at its new deadline")
	}
	if tm.Reset(time.Second) {
		t.Fatal("Reset of a fired timer reported active")
	}
	if f.Pending() != 1 {
		t.Fatalf("Pending after Reset = %d, want 1", f.Pending())
	}
	tm.Stop()
	if tm.Reset(time.Second) {
		t.Fatal("Reset of a stopped timer reported active")
	}
}

func TestFullChannelNeverBlocksAdvance(t *testing.T) {
	f := New(epoch())
	tm := f.NewTimer(time.Second)
	f.Advance(time.Second) // the channel now holds one value
	tm.Reset(time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Advance(time.Second) // second fire is dropped, as time.Timer does
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Advance blocked on a full timer channel")
	}
	if at := <-tm.C(); !at.Equal(epoch().Add(time.Second)) {
		t.Fatalf("delivered %v, want the first fire time", at)
	}
}

func TestSleepCompletesOnAdvance(t *testing.T) {
	f := New(epoch())
	errc := make(chan error, 1)
	go func() { errc <- f.Sleep(t.Context(), time.Minute) }()
	waitFor(t, "Sleep to arm its timer", func() bool { return f.Pending() == 1 })
	f.Advance(time.Minute)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Sleep = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Sleep did not return after Advance")
	}
	waitFor(t, "Sleep to release its timer", func() bool { return f.Pending() == 0 })
}

func TestSleepCancellation(t *testing.T) {
	f := New(epoch())
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- f.Sleep(ctx, time.Hour) }()
	waitFor(t, "Sleep to arm its timer", func() bool { return f.Pending() == 1 })
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Sleep = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Sleep did not return on cancellation")
	}
	// The deferred Stop disarms the timer.
	waitFor(t, "Sleep to stop its timer", func() bool { return f.Pending() == 0 })
}

// Zero and negative durations fire at once like clock.Real, so a full-jitter
// backoff that draws 0 never hangs a test.
func TestZeroAndNegativeDurations(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Nanosecond, -time.Hour} {
		t.Run(d.String(), func(t *testing.T) {
			f := New(epoch())

			tm := f.NewTimer(d)
			select {
			case at := <-tm.C():
				if !at.Equal(epoch()) {
					t.Errorf("NewTimer(%v) delivered %v, want now", d, at)
				}
			default:
				t.Fatalf("NewTimer(%v) channel not ready", d)
			}

			if err := f.Sleep(t.Context(), d); err != nil {
				t.Fatalf("Sleep(%v) = %v, want nil", d, err)
			}
			if f.Pending() != 0 {
				t.Fatalf("Pending = %d, want 0: zero durations are never armed", f.Pending())
			}
		})
	}
}

func TestSleepZeroOnDoneContext(t *testing.T) {
	f := New(epoch())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.Sleep(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep(done ctx, 0) = %v, want context.Canceled", err)
	}
}

func TestAfterFuncZeroRunsOnNewGoroutine(t *testing.T) {
	f := New(epoch())
	release := make(chan struct{})
	ran := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		// If f ran on this goroutine, AfterFunc would block on release,
		// which is closed only after AfterFunc returns.
		f.AfterFunc(0, func() {
			<-release
			close(ran)
		})
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("AfterFunc(0, f) ran f synchronously")
	}
	close(release)
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("AfterFunc(0, f) never ran f")
	}
}

func TestResetZeroFires(t *testing.T) {
	f := New(epoch())
	tm := f.NewTimer(time.Hour)
	if !tm.Reset(0) {
		t.Fatal("Reset(0) of an armed timer reported inactive")
	}
	select {
	case <-tm.C():
	default:
		t.Fatal("Reset(0) did not fire the timer at once")
	}
	if f.Pending() != 0 {
		t.Fatalf("Pending = %d, want 0", f.Pending())
	}

	ran := make(chan struct{})
	fn := f.AfterFunc(time.Hour, func() { close(ran) })
	if !fn.Reset(-time.Second) {
		t.Fatal("Reset(<0) of an armed AfterFunc reported inactive")
	}
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("Reset(<0) did not run the AfterFunc callback")
	}
}

func TestConcurrentUse(t *testing.T) {
	f := New(epoch())
	var fired atomic.Int64
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				d := time.Duration(i*50+j+1) * time.Millisecond
				if j%2 == 0 {
					f.AfterFunc(d, func() { fired.Add(1) })
				} else {
					tm := f.NewTimer(d)
					tm.Stop()
				}
				_ = f.Now()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			f.Advance(time.Millisecond)
		}
	}()
	wg.Wait()
	f.Advance(time.Hour)
	if got := fired.Load(); got != 8*25 {
		t.Fatalf("fired %d AfterFunc callbacks, want %d", got, 8*25)
	}
	if f.Pending() != 0 {
		t.Fatalf("Pending = %d, want 0", f.Pending())
	}
}

// A component resets or stops its own timer on its own goroutine while the
// test goroutine advances time (breakers, JWKS refresh, the retirer). Under
// -race this must not report a data race on the timer's deadline (a Clock
// is safe for concurrent use).
func TestConcurrentResetStopAndAdvance(t *testing.T) {
	tests := []struct {
		name string
		mk   func(f *Fake) clock.Timer
	}{
		{"NewTimer", func(f *Fake) clock.Timer { return f.NewTimer(time.Millisecond) }},
		{"AfterFunc", func(f *Fake) clock.Timer { return f.AfterFunc(time.Millisecond, func() {}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := New(epoch())
			tm := tt.mk(f)
			const rounds = 500
			var wg sync.WaitGroup
			wg.Add(3)
			go func() {
				defer wg.Done()
				for range rounds {
					f.Advance(time.Millisecond)
				}
			}()
			go func() {
				defer wg.Done()
				for i := range rounds {
					if i%3 == 0 {
						tm.Stop()
					}
					tm.Reset(time.Millisecond)
				}
			}()
			go func() {
				defer wg.Done()
				if ch := tm.C(); ch != nil {
					for range rounds {
						select {
						case <-ch:
						default:
						}
					}
				}
			}()
			wg.Wait()
			tm.Stop()
			if f.Pending() != 0 {
				t.Fatalf("Pending = %d, want 0 after Stop", f.Pending())
			}
		})
	}
}

// The delivered time is the deadline the timer had when it fired, even when
// a concurrent Reset rearms it right after.
func TestFireTimeIsDeadlineAtFire(t *testing.T) {
	f := New(epoch())
	tm := f.NewTimer(time.Second)
	f.Advance(time.Second)
	tm.Reset(time.Hour)
	select {
	case at := <-tm.C():
		if !at.Equal(epoch().Add(time.Second)) {
			t.Fatalf("delivered %v, want %v", at, epoch().Add(time.Second))
		}
	default:
		t.Fatal("timer did not fire")
	}
}

// Jump shows only its end: callbacks of the timers it fires read the new
// time, where Advance shows each deadline, channels still get their
// deadlines, and a timer armed while they fire waits for a later move.
func TestJumpShowsOnlyItsEnd(t *testing.T) {
	f := New(epoch())
	var seen []time.Duration
	f.AfterFunc(100*time.Millisecond, func() {
		seen = append(seen, f.Since(epoch()))
		f.AfterFunc(100*time.Millisecond, func() { seen = append(seen, f.Since(epoch())) })
	})
	ch := f.NewTimer(200 * time.Millisecond)
	f.Jump(time.Second)
	if want := []time.Duration{time.Second}; !slices.Equal(seen, want) {
		t.Errorf("callbacks saw %v, want %v", seen, want)
	}
	select {
	case at := <-ch.C():
		if !at.Equal(epoch().Add(200 * time.Millisecond)) {
			t.Errorf("delivered %v, want the deadline %v", at, epoch().Add(200*time.Millisecond))
		}
	default:
		t.Error("the timer due inside the jump did not fire")
	}
	if got := f.Pending(); got != 1 {
		t.Fatalf("%d timers armed after the jump, want the one armed at its end", got)
	}
	f.Jump(100 * time.Millisecond)
	if want := []time.Duration{time.Second, 1100 * time.Millisecond}; !slices.Equal(seen, want) {
		t.Errorf("callbacks saw %v, want %v", seen, want)
	}
	f.Jump(-time.Hour)
	if got := f.Since(epoch()); got != 1100*time.Millisecond {
		t.Errorf("Since after a negative jump = %v, want 1.1s", got)
	}

	g := New(epoch())
	var advanced time.Duration
	g.AfterFunc(100*time.Millisecond, func() { advanced = g.Since(epoch()) })
	g.Advance(time.Second)
	if advanced != 100*time.Millisecond {
		t.Errorf("Advance showed %v to the callback, want its deadline 100ms", advanced)
	}
}

// waiting reports whether a BlockUntil call is blocked on f, waiting for
// the armed timers to change.
func waiting(f *Fake) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed != nil
}

// blockUntil runs f.BlockUntil(ctx, n) in a goroutine, waits until it
// blocks, and returns the channel that receives its result.
func blockUntil(ctx context.Context, t *testing.T, f *Fake, n int) <-chan error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- f.BlockUntil(ctx, n) }()
	waitFor(t, "BlockUntil to block", func() bool { return waiting(f) })
	return errc
}

// returned waits for a BlockUntil result.
func returned(t *testing.T, errc <-chan error) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("BlockUntil did not return")
		return nil
	}
}

// BlockUntil returns at once when exactly n timers are armed, even on a
// done context, since nothing is left to wait for.
func TestBlockUntilReturnsAtOnceWhenArmed(t *testing.T) {
	f := New(epoch())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.BlockUntil(ctx, 0); err != nil {
		t.Fatalf("BlockUntil(0) on a clock without timers = %v, want nil", err)
	}
	f.NewTimer(time.Second)
	f.AfterFunc(time.Minute, func() {})
	if err := f.BlockUntil(ctx, 2); err != nil {
		t.Fatalf("BlockUntil(2) with 2 armed timers = %v, want nil", err)
	}
}

// BlockUntil wakes when a goroutine arms the timer it then blocks on, so
// the advance that follows fires that timer.
func TestBlockUntilWaitsForAGoroutineToArm(t *testing.T) {
	f := New(epoch())
	errc := blockUntil(t.Context(), t, f, 1)
	slept := make(chan error, 1)
	go func() { slept <- f.Sleep(t.Context(), time.Minute) }()
	if err := returned(t, errc); err != nil {
		t.Fatalf("BlockUntil(1) = %v, want nil", err)
	}
	if f.Pending() != 1 {
		t.Fatalf("Pending after BlockUntil(1) = %d, want 1", f.Pending())
	}
	f.Advance(time.Minute)
	select {
	case err := <-slept:
		if err != nil {
			t.Fatalf("Sleep = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the advance after BlockUntil did not end the Sleep")
	}
}

// BlockUntil wakes for each way a timer stops being armed: Stop, firing
// in Advance, and Reset to a non-positive duration.
func TestBlockUntilCountsDisarmedTimers(t *testing.T) {
	f := New(epoch())
	stopped := f.NewTimer(time.Hour)
	fired := f.AfterFunc(time.Second, func() {})
	reset := f.NewTimer(time.Hour)
	steps := []struct {
		name   string
		disarm func()
	}{
		{"Stop", func() { stopped.Stop() }},
		{"Advance", func() { f.Advance(time.Second) }},
		{"Reset(0)", func() { reset.Reset(0) }},
	}
	for i, step := range steps {
		want := len(steps) - 1 - i
		errc := blockUntil(t.Context(), t, f, want)
		step.disarm()
		if err := returned(t, errc); err != nil {
			t.Fatalf("after %s: BlockUntil(%d) = %v, want nil", step.name, want, err)
		}
	}
	if fired.Stop() {
		t.Fatal("the AfterFunc timer was still armed after its Advance")
	}
}

// BlockUntil returns ctx.Err() once its context ends while the count
// differs.
func TestBlockUntilCancellation(t *testing.T) {
	f := New(epoch())
	ctx, cancel := context.WithCancel(t.Context())
	errc := blockUntil(ctx, t, f, 1)
	cancel()
	if err := returned(t, errc); !errors.Is(err, context.Canceled) {
		t.Fatalf("BlockUntil after cancel = %v, want context.Canceled", err)
	}
}
