// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package drain

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/setting"
)

const sec = time.Second

// TestNewRequiresParts checks that New refuses a Config without
// readiness, listeners or retirer, or with a step lacking a name or a
// function, and accepts the required parts alone.
func TestNewRequiresParts(t *testing.T) {
	r := newRig(t)
	ok := Config{Readiness: r.ready, Listeners: r.lis, Retirer: &fakeRetirer{ev: r.ev}}
	if _, err := New(ok); err != nil {
		t.Fatalf("New with the required parts only: %v", err)
	}
	noop := func(context.Context) error { return nil }
	for name, cfg := range map[string]Config{
		"no readiness":          {Listeners: ok.Listeners, Retirer: ok.Retirer},
		"no listeners":          {Readiness: ok.Readiness, Retirer: ok.Retirer},
		"no retirer":            {Readiness: ok.Readiness, Listeners: ok.Listeners},
		"unnamed step":          {Readiness: ok.Readiness, Listeners: ok.Listeners, Retirer: ok.Retirer, Steps: []Step{{Run: noop}}},
		"step without function": {Readiness: ok.Readiness, Listeners: ok.Listeners, Retirer: ok.Retirer, Steps: []Step{{Name: "flush"}}},
	} {
		if _, err := New(cfg); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: New = %v, want ErrConfig", name, err)
		}
	}
}

// TestSignalDrainTimeline checks the stop-signal timeline of "Drain
// timeline defaults": at 0 s readiness fails, STOPPING=1 is sent, the
// source stops and the lock is released; stop-accepting at 5 s; shard i at
// its draw within the i-th eighth of 5 to 15 s; at 25 s the deadline ends
// the remaining work with the drain reason, closes the retirer and the
// listeners and runs the steps in order; exit 0. The deadline's record
// reports the connections left and the requests ended.
func TestSignalDrainTimeline(t *testing.T) {
	r := newRig(t, func(_ *rig, c *Config) { c.Connections = func() int64 { return 2 } })
	r.run(t.Context())
	r.d.Signal()
	r.stepWaves(t, 5*sec, Waves)
	r.step(t, 25*sec)
	if code := r.finished(t); code != setting.ExitOK {
		t.Errorf("exit code = %d, want %d", code, setting.ExitOK)
	}
	want := eventsAt(0, "readiness", "stopping", "source", "lock")
	want = append(want, wavesEvents(5*sec, Waves)...)
	want = append(want, deadlineEvents(25*sec)...)
	r.wantEvents(t, want)
	if n := r.logs.count("INFO drain started", "reason=signal"); n != 1 {
		t.Errorf("%d drain start records with reason signal, want 1", n)
	}
	if n := r.logs.count("drain deadline reached", "connections=2", "ended=3"); n != 1 {
		t.Errorf("%d deadline records with the connections left and the requests ended, want 1", n)
	}
}

// TestNothingBeforeItsTime checks that each timeline step waits for its
// time: nothing stops accepting before 5 s, and no shard shuts down
// before its draw.
func TestNothingBeforeItsTime(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	r.d.Signal()
	r.step(t, 5*sec-time.Nanosecond)
	r.waitArmed(t)
	if r.ev.has("stop@") {
		t.Fatal("stopped accepting before the accept window ended")
	}
	r.step(t, 5*sec)
	r.step(t, waveAt(5*sec, 0)-time.Nanosecond)
	r.waitArmed(t)
	if r.ev.has("shard0@") {
		t.Fatal("shard 0 shut down before its draw")
	}
	r.d.Signal() // end the test's Drain
	r.finished(t)
}

// TestSecondSignalJumpsToDeadline checks that a second stop signal during
// the accept window runs the deadline phase at once, every shard shut down
// then, and that the steps' context ends 5 s after the jump.
func TestSecondSignalJumpsToDeadline(t *testing.T) {
	ended := make(chan time.Time, 1)
	r := newRig(t, func(r *rig, c *Config) {
		c.Steps = append(c.Steps, Step{Name: "flush", Run: func(ctx context.Context) error {
			r.ev.add("step:flush")
			<-ctx.Done()
			ended <- r.clk.Now()
			return ctx.Err()
		}})
	})
	r.run(t.Context())
	r.d.Signal()
	r.step(t, 2*sec)
	r.d.Signal()
	waitFor(t, "the flush step", func() bool { return r.ev.has("step:flush@") })
	r.clk.Set(r.at(7*sec - time.Nanosecond))
	select {
	case at := <-ended:
		t.Fatalf("the steps' context ended at %v, before 5 s after the jump", at.Sub(r.ev.start))
	default:
	}
	r.clk.Set(r.at(7 * sec))
	if at := recv(t, "the flush step's context to end", ended); !at.Equal(r.at(7 * sec)) {
		t.Errorf("the steps' context ended at %v, want 7s", at.Sub(r.ev.start))
	}
	if code := r.finished(t); code != setting.ExitOK {
		t.Errorf("exit code = %d, want %d", code, setting.ExitOK)
	}
	want := eventsAt(0, "readiness", "stopping", "source", "lock")
	want = append(want, deadlineEvents(2*sec)...)
	want = append(want, "step:flush@2s")
	r.wantEvents(t, want)
	if n := r.logs.count("drain jumped to its deadline"); n != 1 {
		t.Errorf("%d jump records, want 1", n)
	}
}

// TestSecondSignalDuringWaves checks a jump in the jitter window: the
// waves already due shut their shards down at their times, the rest at the
// jump.
func TestSecondSignalDuringWaves(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	r.d.Signal()
	r.stepWaves(t, 5*sec, 3)
	r.waitArmed(t)
	r.clk.Set(r.at(9 * sec))
	r.d.Signal()
	r.finished(t)
	want := eventsAt(0, "readiness", "stopping", "source", "lock")
	want = append(want, wavesEvents(5*sec, 3)...)
	want = append(want, deadlineEvents(9*sec)...)
	r.wantEvents(t, want)
}

// TestLaterSignalsDoNothing checks that stop signals after the second, and
// any after the Drain, change nothing.
func TestLaterSignalsDoNothing(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	r.d.Signal()
	r.d.Signal()
	r.d.Signal()
	r.d.Signal()
	r.finished(t)
	r.d.Signal()
	want := eventsAt(0, "readiness", "stopping", "source", "lock")
	want = append(want, deadlineEvents(0)...)
	r.wantEvents(t, want)
}

// TestFlushBoundEndsSteps checks the exit bound of a stop-signal Drain:
// the steps' context ends 30 s after the signal, 5 s after the deadline.
func TestFlushBoundEndsSteps(t *testing.T) {
	ended := make(chan time.Time, 1)
	r := newRig(t, func(r *rig, c *Config) {
		c.Steps = []Step{{Name: "telemetry", Run: func(ctx context.Context) error {
			r.ev.add("step:telemetry")
			<-ctx.Done()
			ended <- r.clk.Now()
			return nil
		}}}
	})
	r.run(t.Context())
	r.d.Signal()
	r.stepWaves(t, 5*sec, Waves)
	r.step(t, 25*sec)
	waitFor(t, "the telemetry step", func() bool { return r.ev.has("step:telemetry@") })
	r.clk.Set(r.at(ExitBound))
	if at := recv(t, "the step's context to end", ended); !at.Equal(r.at(30 * sec)) {
		t.Errorf("the steps' context ended at %v, want 30s", at.Sub(r.ev.start))
	}
	r.finished(t)
}

// TestHandoverDrainTimeline checks a handover's Drain from the linger's
// end: the Notifier is muted before anything, so no STOPPING=1 or STATUS=
// is sent; readiness fails, the source stops and the lock is released; no
// accept window, the waves across the first 10 s and the deadline at 20 s;
// exit 0.
func TestHandoverDrainTimeline(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	r.d.Handover()
	r.stepWaves(t, 0, Waves)
	r.step(t, 20*sec)
	if code := r.finished(t); code != setting.ExitOK {
		t.Errorf("exit code = %d, want %d", code, setting.ExitOK)
	}
	want := eventsAt(0, "mute", "readiness", "source", "lock")
	want = append(want, wavesEvents(0, Waves)...)
	want = append(want, deadlineEvents(20*sec)...)
	r.wantEvents(t, want)
}

// TestHandoverExitBound checks that a handover's steps end 25 s after its
// start.
func TestHandoverExitBound(t *testing.T) {
	ended := make(chan time.Time, 1)
	r := newRig(t, func(r *rig, c *Config) {
		c.Steps = []Step{{Name: "telemetry", Run: func(ctx context.Context) error {
			r.ev.add("step:telemetry")
			<-ctx.Done()
			ended <- r.clk.Now()
			return nil
		}}}
	})
	r.run(t.Context())
	r.d.Handover()
	r.stepWaves(t, 0, Waves)
	r.step(t, 20*sec)
	waitFor(t, "the telemetry step", func() bool { return r.ev.has("step:telemetry@") })
	r.clk.Set(r.at(HandoverExitBound))
	if at := recv(t, "the step's context to end", ended); !at.Equal(r.at(25 * sec)) {
		t.Errorf("the steps' context ended at %v, want 25s", at.Sub(r.ev.start))
	}
	r.finished(t)
}

// TestHandoverCountsFirstSignal checks that a stop signal during a
// handover's Drain counts as its first, and the second jumps.
func TestHandoverCountsFirstSignal(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	r.d.Handover()
	r.stepWaves(t, 0, 2)
	r.d.Signal()
	r.waitArmed(t)
	if r.ev.has("endall") {
		t.Fatal("the first stop signal of a handover's Drain jumped to the deadline")
	}
	r.d.Signal()
	r.finished(t)
	want := eventsAt(0, "mute", "readiness", "source", "lock")
	want = append(want, wavesEvents(0, 2)...)
	want = append(want, deadlineEvents(waveAt(0, 1))...)
	r.wantEvents(t, want)
}

// TestHoldDefersSignalToHandover checks a stop signal while a handover
// steers: nothing starts; the handover's Drain then starts at the
// linger's end without STOPPING=1, counting the signal as its first, so
// the next one jumps.
func TestHoldDefersSignalToHandover(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	if !r.d.Hold() {
		t.Fatal("Hold before any Drain = false")
	}
	r.d.Signal()
	r.clk.Set(r.at(3 * sec))
	select {
	case <-r.d.Started():
		t.Fatal("a stop signal during the Hold started a Drain")
	default:
	}
	if got := r.ev.all(); len(got) != 0 {
		t.Fatalf("events during the Hold: %q", got)
	}
	r.d.Handover()
	recv(t, "the Drain to start", r.d.Started())
	r.waitArmed(t)
	r.d.Signal()
	r.finished(t)
	want := eventsAt(3*sec, "mute", "readiness", "source", "lock", "stop")
	want = append(want, deadlineEvents(3*sec)...)
	r.wantEvents(t, want)
}

// TestHoldWithTwoSignalsJumpsAtHandover checks that two stop signals
// during the Hold make the handover's Drain jump to its deadline at once.
func TestHoldWithTwoSignalsJumpsAtHandover(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	r.d.Hold()
	r.d.Signal()
	r.d.Signal()
	r.d.Handover()
	r.finished(t)
	want := eventsAt(0, "mute", "readiness", "source", "lock")
	want = append(want, deadlineEvents(0)...)
	r.wantEvents(t, want)
}

// TestHoldRefusedOnceDraining checks that Hold fails once a Drain started,
// and that Handover then changes nothing.
func TestHoldRefusedOnceDraining(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	r.d.Signal()
	if r.d.Hold() {
		t.Error("Hold during a Drain = true, want false so the holder refuses with draining")
	}
	r.d.Handover()
	r.d.Signal()
	r.finished(t)
	if slices.Contains(r.ev.all(), "mute@0s") {
		t.Error("Handover during a stop-signal Drain muted the Notifier")
	}
}

// TestResumeStartsDeferredDrain checks a handover lost during the linger:
// Resume starts the stop-signal Drain a signal asked for during the Hold,
// with STOPPING=1, timed from the Resume.
func TestResumeStartsDeferredDrain(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	r.d.Hold()
	r.d.Signal()
	r.clk.Set(r.at(2 * sec))
	r.d.Resume()
	r.step(t, 7*sec)
	r.waitArmed(t)
	r.d.Signal()
	r.finished(t)
	want := eventsAt(2*sec, "readiness", "stopping", "source", "lock")
	want = append(want, eventsAt(7*sec, "stop")...)
	want = append(want, deadlineEvents(7*sec)...)
	r.wantEvents(t, want)
}

// TestResumeWithoutSignalKeepsServing checks that a Resume without a
// deferred signal starts nothing, and a later signal drains as usual.
func TestResumeWithoutSignalKeepsServing(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	r.d.Hold()
	r.d.Resume()
	r.d.Resume()
	select {
	case <-r.d.Started():
		t.Fatal("Resume without a signal started a Drain")
	default:
	}
	r.d.Signal()
	r.d.Signal()
	r.finished(t)
	if !slices.Contains(r.ev.all(), "stopping@0s") {
		t.Errorf("events %q, want a stop-signal Drain", r.ev.all())
	}
}

// TestResumeStartsDeferredFailure checks that a failure during the Hold
// starts the failure Drain at the Resume, exiting 1.
func TestResumeStartsDeferredFailure(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	r.d.Hold()
	r.d.Fail(errors.New("retirer stopped"))
	select {
	case <-r.d.Started():
		t.Fatal("a failure during the Hold started a Drain")
	default:
	}
	r.d.Resume()
	r.waitArmed(t)
	r.d.Signal()
	r.d.Signal()
	if code := r.finished(t); code != setting.ExitRuntime {
		t.Errorf("exit code = %d, want %d", code, setting.ExitRuntime)
	}
	if got := r.ev.all(); !slices.Contains(got, "status@0s") || slices.Contains(got, "stopping@0s") {
		t.Errorf("events %q, want a failure Drain: STATUS=, no STOPPING=1", got)
	}
}

// TestFailureDrainExitsOne checks the supervisor's Drain: the stop-signal
// timeline with STATUS=draining instead of STOPPING=1, the error logged,
// exit 1.
func TestFailureDrainExitsOne(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	r.d.Fail(errors.New("admin server stopped"))
	r.stepWaves(t, 5*sec, Waves)
	r.step(t, 25*sec)
	if code := r.finished(t); code != setting.ExitRuntime {
		t.Errorf("exit code = %d, want %d", code, setting.ExitRuntime)
	}
	want := eventsAt(0, "readiness", "status", "source", "lock")
	want = append(want, wavesEvents(5*sec, Waves)...)
	want = append(want, deadlineEvents(25*sec)...)
	r.wantEvents(t, want)
	if n := r.logs.count("WARN drain started", "reason=failure", "error=admin server stopped"); n != 1 {
		t.Errorf("%d failure start records, want 1", n)
	}
}

// TestFailDuringDrainExitsOne checks that a failure during a stop-signal
// Drain keeps its timeline and makes the exit code 1; a stop signal after
// a failure Drain started counts as its first.
func TestFailDuringDrainExitsOne(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	r.d.Signal()
	r.stepWaves(t, 5*sec, 2)
	r.d.Fail(errors.New("retirer stopped"))
	r.waitArmed(t)
	if r.ev.has("endall") {
		t.Fatal("a failure during a Drain jumped to the deadline")
	}
	r.d.Signal()
	if code := r.finished(t); code != setting.ExitRuntime {
		t.Errorf("exit code = %d, want %d", code, setting.ExitRuntime)
	}

	f := newRig(t)
	f.run(t.Context())
	f.d.Fail(errors.New("retirer stopped"))
	f.waitArmed(t)
	f.d.Signal()
	f.waitArmed(t)
	if f.ev.has("endall") {
		t.Fatal("the first stop signal of a failure Drain jumped to the deadline")
	}
	f.d.Signal()
	f.finished(t)
}

// TestNeverReadyGoesToDeadline checks a Node that never answered /readyz
// with 200 and has no client listener bound: no accept window and no
// waves, the deadline phase at once, the steps ending 5 s later.
func TestNeverReadyGoesToDeadline(t *testing.T) {
	ended := make(chan time.Time, 1)
	r := newRig(t, func(r *rig, c *Config) {
		r.ready.answered, r.lis.bound = false, false
		c.Steps = []Step{{Name: "telemetry", Run: func(ctx context.Context) error {
			r.ev.add("step:telemetry")
			<-ctx.Done()
			ended <- r.clk.Now()
			return nil
		}}}
	})
	r.run(t.Context())
	r.d.Signal()
	waitFor(t, "the telemetry step", func() bool { return r.ev.has("step:telemetry@") })
	r.clk.Set(r.at(FlushBound))
	if at := recv(t, "the step's context to end", ended); !at.Equal(r.at(5 * sec)) {
		t.Errorf("the steps' context ended at %v, want 5s", at.Sub(r.ev.start))
	}
	r.finished(t)
	want := eventsAt(0, "readiness", "stopping", "source", "lock")
	want = append(want, eventsAt(0, "stop", "shard0", "shard1", "shard2", "shard3", "shard4", "shard5", "shard6", "shard7", "endall", "retirer", "shutdown", "close", "step:telemetry")...)
	r.wantEvents(t, want)
	if n := r.logs.count("drain goes to its deadline"); n != 1 {
		t.Errorf("%d skip records, want 1", n)
	}
}

// TestSeenReadyOrBoundKeepsTimeline checks that the skip needs both
// conditions: a Node seen ready, or one with a listener bound, keeps the
// accept window; a handover's Drain never skips its waves.
func TestSeenReadyOrBoundKeepsTimeline(t *testing.T) {
	for _, tt := range []struct {
		name            string
		answered, bound bool
		handover        bool
		firstStop       string
	}{
		{name: "seen ready, nothing bound", answered: true, firstStop: "stop@5s"},
		{name: "never ready, bound", bound: true, firstStop: "stop@5s"},
		{name: "handover, never ready, nothing bound", handover: true, firstStop: "stop@0s"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, func(r *rig, _ *Config) { r.ready.answered, r.lis.bound = tt.answered, tt.bound })
			r.run(t.Context())
			if tt.handover {
				r.d.Handover()
			} else {
				r.d.Signal()
				r.step(t, 5*sec)
			}
			r.waitArmed(t)
			if got := r.ev.all(); !slices.Contains(got, tt.firstStop) || slices.Contains(got, "endall@0s") {
				t.Errorf("events %q, want %s and no deadline yet", got, tt.firstStop)
			}
			r.d.Signal()
			r.d.Signal()
			r.finished(t)
		})
	}
}

// TestRunReturnsWhenContextEndsFirst checks that Run returns ExitRuntime
// without draining when its context ends before any trigger.
func TestRunReturnsWhenContextEndsFirst(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	r.run(ctx)
	cancel()
	if code := r.finished(t); code != setting.ExitRuntime {
		t.Errorf("exit code = %d, want %d", code, setting.ExitRuntime)
	}
	if got := r.ev.all(); len(got) != 0 {
		t.Errorf("events %q, want none", got)
	}
}

// TestRunContextEndingCutsWaits checks that Run's context ending during a
// Drain ends every wait at once and runs the deadline phase with a context
// that ends with it.
func TestRunContextEndingCutsWaits(t *testing.T) {
	stepErr := make(chan error, 1)
	r := newRig(t, func(_ *rig, c *Config) {
		c.Steps = []Step{{Name: "telemetry", Run: func(ctx context.Context) error {
			<-ctx.Done()
			stepErr <- ctx.Err()
			return nil
		}}}
	})
	ctx, cancel := context.WithCancel(t.Context())
	r.run(ctx)
	r.d.Signal()
	r.waitArmed(t)
	cancel()
	if err := recv(t, "the step", stepErr); !errors.Is(err, context.Canceled) {
		t.Errorf("step context error = %v, want context.Canceled", err)
	}
	r.finished(t)
	if r.ev.has("stop@5s") {
		t.Error("the accept window ran after the context ended")
	}
}

// TestSignalBeforeRun checks that a Drain triggered before Run starts is
// timed from its trigger.
func TestSignalBeforeRun(t *testing.T) {
	r := newRig(t)
	r.d.Signal()
	r.clk.Set(r.at(2 * sec))
	r.run(t.Context())
	r.step(t, 5*sec)
	r.waitArmed(t)
	if !r.ev.has("stop@5s") {
		t.Errorf("events %q, want stop-accepting at 5s from the signal", r.ev.all())
	}
	r.d.Signal()
	r.finished(t)
}

// TestRunTwiceReturnsSameCode checks that a second Run returns the first
// one's code without draining again.
func TestRunTwiceReturnsSameCode(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	r.d.Fail(errors.New("boom"))
	r.d.Signal()
	r.d.Signal()
	first := r.finished(t)
	n := len(r.ev.all())
	if second := r.d.Run(t.Context()); second != first || len(r.ev.all()) != n {
		t.Errorf("second Run = %d with %d new events, want %d and none", second, len(r.ev.all())-n, first)
	}
}

// TestStartedClosesAtStart checks Started.
func TestStartedClosesAtStart(t *testing.T) {
	r := newRig(t)
	select {
	case <-r.d.Started():
		t.Fatal("Started closed before a trigger")
	default:
	}
	r.run(t.Context())
	r.d.Signal()
	recv(t, "Started", r.d.Started())
	r.d.Signal()
	r.finished(t)
}

// TestPartFailuresDoNotStopTheDrain checks that a part's error is logged
// and the Drain goes on with exit 0, and that a panicking step is logged,
// the next steps still run and the exit code is 1.
func TestPartFailuresDoNotStopTheDrain(t *testing.T) {
	r := newRig(t, func(r *rig, c *Config) {
		r.lock.err = errors.New("holder.json: read-only file system")
		c.Steps = []Step{
			{Name: "admin", Run: func(context.Context) error { panic("admin shutdown broke") }},
			{Name: "telemetry", Run: func(context.Context) error { r.ev.add("step:telemetry"); return nil }},
		}
	})
	r.run(t.Context())
	r.d.Signal()
	r.d.Signal()
	if code := r.finished(t); code != setting.ExitRuntime {
		t.Errorf("exit code = %d, want %d after a panicking step", code, setting.ExitRuntime)
	}
	if !r.ev.has("step:telemetry@") {
		t.Error("the step after the panicking one did not run")
	}
	if n := r.logs.count("WARN drain step failed", "step=lock", "read-only file system"); n != 1 {
		t.Errorf("%d lock failure records, want 1", n)
	}
	if n := r.logs.count("ERROR drain step panicked", "step=admin", "admin shutdown broke"); n != 1 {
		t.Errorf("%d panic records, want 1", n)
	}

	ok := newRig(t, func(r *rig, _ *Config) { r.lock.err = errors.New("busy") })
	ok.run(t.Context())
	ok.d.Signal()
	ok.d.Signal()
	if code := ok.finished(t); code != setting.ExitOK {
		t.Errorf("exit code = %d after a logged part error, want %d", code, setting.ExitOK)
	}
}

// TestOptionalPartsMayBeMissing checks a Drainer without Notifier, lock,
// source or steps, as in a process that holds no lock.
func TestOptionalPartsMayBeMissing(t *testing.T) {
	r := newRig(t, func(_ *rig, c *Config) {
		c.Notifier, c.Lock, c.StopSource, c.Steps = nil, nil, nil, nil
	})
	r.run(t.Context())
	r.d.Handover()
	r.waitArmed(t)
	r.d.Signal()
	r.d.Signal()
	if code := r.finished(t); code != setting.ExitOK {
		t.Errorf("exit code = %d, want %d", code, setting.ExitOK)
	}
	want := eventsAt(0, "readiness")
	want = append(want, eventsAt(0, "stop")...)
	want = append(want, deadlineEvents(0)[:len(deadlineEvents(0))-2]...)
	r.wantEvents(t, want)
}
