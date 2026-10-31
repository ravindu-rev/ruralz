// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sdnotify

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// interval is the T5 unit's WatchdogSec=30s.
const interval = 30 * time.Second

var _ Pinger = (*Notifier)(nil)

// fakePinger records the fake time of every ping.
type fakePinger struct {
	clk *clocktest.Fake
	mu  sync.Mutex
	at  []time.Time
	err error
}

func (p *fakePinger) Watchdog(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.at = append(p.at, p.clk.Now())
	return p.err
}

func (p *fakePinger) setErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

// pings returns the ping times so far.
func (p *fakePinger) pings() []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Time(nil), p.at...)
}

// probeCtl is a part's probe the test answers call by call.
type probeCtl struct {
	calls chan chan error
}

func newProbeCtl() *probeCtl { return &probeCtl{calls: make(chan chan error, 1)} }

func (p *probeCtl) probe(ctx context.Context) error {
	reply := make(chan error, 1)
	select {
	case p.calls <- reply:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dog runs a Watchdog on a fake clock until the end of the test.
type dog struct {
	w      *Watchdog
	clk    *clocktest.Fake
	pinger *fakePinger
	logs   *captureHandler
	start  time.Time
}

func runDog(t *testing.T, parts ...Part) *dog {
	t.Helper()
	clk := clocktest.New(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	d := &dog{clk: clk, pinger: &fakePinger{clk: clk}, logs: &captureHandler{}, start: clk.Now()}
	w, err := NewWatchdog(WatchdogOptions{Notifier: d.pinger, Interval: interval, Parts: parts, Clock: clk, Logger: slog.New(d.logs)})
	if err != nil {
		t.Fatal(err)
	}
	d.w = w
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := recv(t, "Run to return", done); err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	})
	return d
}

// answer takes the next call of ctl's probe, answers it with success and
// waits until the Watchdog recorded the result, so its time is the current
// fake time.
func (d *dog) answer(t *testing.T, name string, ctl *probeCtl) {
	t.Helper()
	reply := recv(t, "a "+name+" probe", ctl.calls)
	reply <- nil
	d.settle(t, name)
}

// settle waits until part name has no probe in flight.
func (d *dog) settle(t *testing.T, name string) {
	t.Helper()
	waitFor(t, name+"'s probe to return", func() bool {
		d.w.mu.Lock()
		defer d.w.mu.Unlock()
		for _, p := range d.w.parts {
			if p.Name == name {
				return !p.running
			}
		}
		return false
	})
}

// tick advances the fake clock to the next tick, once the current one
// armed its timer.
func (d *dog) tick(t *testing.T) {
	t.Helper()
	waitFor(t, "the tick timer", func() bool { return d.clk.Pending() == 1 })
	d.clk.Advance(interval / 2)
}

// wantPings checks the ping times, as offsets from the start.
func (d *dog) wantPings(t *testing.T, offsets ...time.Duration) {
	t.Helper()
	got := d.pinger.pings()
	if len(got) != len(offsets) {
		t.Fatalf("%d pings at %v, want %d at %v", len(got), d.offsets(got), len(offsets), offsets)
	}
	for i, at := range got {
		if at.Sub(d.start) != offsets[i] {
			t.Fatalf("pings at %v, want %v", d.offsets(got), offsets)
		}
	}
}

func (d *dog) offsets(ts []time.Time) []time.Duration {
	out := make([]time.Duration, len(ts))
	for i, at := range ts {
		out[i] = at.Sub(d.start)
	}
	return out
}

// TestWatchdogInterval checks WATCHDOG_USEC and WATCHDOG_PID as
// sd_watchdog_enabled(3) reads them: microseconds; empty or 0 means no
// watchdog; a WATCHDOG_PID naming another process, which passed the
// variables on, means no watchdog; malformed values are errors.
func TestWatchdogInterval(t *testing.T) {
	const self = 4242
	for _, tt := range []struct {
		usec, pid string
		want      time.Duration
		err       error
	}{
		{usec: "", want: 0},
		{usec: "0", want: 0},
		{usec: "30000000", want: 30 * time.Second},
		{usec: "30000000", pid: "4242", want: 30 * time.Second},
		{usec: "30000000", pid: "4241", want: 0},
		{usec: "1", want: time.Microsecond},
		{usec: "9223372036854775", want: 9223372036854775 * time.Microsecond},
		{usec: "9223372036854776", err: ErrWatchdogUsec},
		{usec: "-1", err: ErrWatchdogUsec},
		{usec: "30s", err: ErrWatchdogUsec},
		{usec: " 30", err: ErrWatchdogUsec},
		{usec: "30000000", pid: "init", err: ErrWatchdogPID},
		{usec: "30000000", pid: "0", err: ErrWatchdogPID},
	} {
		got, err := WatchdogInterval(tt.usec, tt.pid, self)
		if !errors.Is(err, tt.err) || got != tt.want {
			t.Errorf("WatchdogInterval(%q, %q) = %v, %v; want %v, %v", tt.usec, tt.pid, got, err, tt.want, tt.err)
		}
	}
}

// TestReadEnv checks the variables systemd sets for its ExecStart=
// process, and that an invalid interval keeps the socket.
func TestReadEnv(t *testing.T) {
	env := map[string]string{EnvNotifySocket: "/run/systemd/notify", EnvWatchdogUsec: "30000000", EnvWatchdogPID: "77"}
	got, err := ReadEnv(func(k string) string { return env[k] }, 77)
	if err != nil || got.Socket != "/run/systemd/notify" || got.Watchdog != 30*time.Second {
		t.Errorf("ReadEnv = %+v, %v; want the socket and 30s", got, err)
	}
	env[EnvWatchdogUsec] = "soon"
	got, err = ReadEnv(func(k string) string { return env[k] }, 77)
	if !errors.Is(err, ErrWatchdogUsec) || got.Socket != "/run/systemd/notify" || got.Watchdog != 0 {
		t.Errorf("ReadEnv with a bad interval = %+v, %v; want the socket, no watchdog and ErrWatchdogUsec", got, err)
	}
	got, err = ReadEnv(func(string) string { return "" }, 77)
	if err != nil || got != (Env{}) {
		t.Errorf("ReadEnv outside systemd = %+v, %v; want nothing", got, err)
	}
}

// TestNewWatchdogRefusesBadOptions checks the options NewWatchdog
// refuses: a negative interval, an interval without a Notifier, a part
// without a name or probe and a part named twice.
func TestNewWatchdogRefusesBadOptions(t *testing.T) {
	ok := func(context.Context) error { return nil }
	n := &fakePinger{}
	for name, o := range map[string]WatchdogOptions{
		"negative interval": {Notifier: n, Interval: -time.Second},
		"no notifier":       {Interval: interval},
		"part without name": {Notifier: n, Interval: interval, Parts: []Part{{Probe: ok}}},
		"part without probe": {
			Notifier: n, Interval: interval, Parts: []Part{{Name: "retirer"}},
		},
		"part named twice": {
			Notifier: n, Interval: interval, Parts: []Part{{Name: "retirer", Probe: ok}, {Name: "retirer", Probe: ok}},
		},
	} {
		if _, err := NewWatchdog(o); !errors.Is(err, ErrWatchdog) {
			t.Errorf("%s: NewWatchdog = %v, want ErrWatchdog", name, err)
		}
	}
}

// TestWatchdogWithoutIntervalReturns checks that without WATCHDOG_USEC
// Run returns at once, pinging nothing.
func TestWatchdogWithoutIntervalReturns(t *testing.T) {
	p := &fakePinger{clk: clocktest.New(time.Unix(0, 0))}
	w, err := NewWatchdog(WatchdogOptions{Notifier: p})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Run(t.Context()); err != nil || len(p.pings()) != 0 || w.Interval() != 0 {
		t.Errorf("Run without an interval = %v with %d pings, want nil and none", err, len(p.pings()))
	}
}

// TestWatchdogPingsEveryHalfInterval checks that a Watchdog without parts
// pings at once and then every half interval.
func TestWatchdogPingsEveryHalfInterval(t *testing.T) {
	d := runDog(t)
	waitFor(t, "the first ping", func() bool { return len(d.pinger.pings()) == 1 })
	for i := 2; i <= 4; i++ {
		d.tick(t)
		waitFor(t, "the next ping", func() bool { return len(d.pinger.pings()) == i })
	}
	d.wantPings(t, 0, 15*time.Second, 30*time.Second, 45*time.Second)
}

// TestWatchdogPingsWhileEveryPartRuns checks the condition of the T5
// watchdog: no ping before every part ran, then a ping every half
// interval while every part's probe succeeds.
func TestWatchdogPingsWhileEveryPartRuns(t *testing.T) {
	admin, retirer := newProbeCtl(), newProbeCtl()
	d := runDog(t, Part{Name: "admin", Probe: admin.probe}, Part{Name: "retirer", Probe: retirer.probe})
	d.answer(t, "admin", admin)
	d.answer(t, "retirer", retirer)
	d.wantPings(t)
	for range 3 {
		d.tick(t)
		d.answer(t, "admin", admin)
		d.answer(t, "retirer", retirer)
	}
	d.wantPings(t, 15*time.Second, 30*time.Second, 45*time.Second)
}

// TestWatchdogStopsWhileAPartHangs checks that a part whose probe stops
// returning still counts for one interval after its last success, then
// stops the pings and is logged once; its next success resumes them.
func TestWatchdogStopsWhileAPartHangs(t *testing.T) {
	fine, stuck := newProbeCtl(), newProbeCtl()
	d := runDog(t, Part{Name: "fine", Probe: fine.probe}, Part{Name: "stuck", Probe: stuck.probe})
	d.answer(t, "fine", fine)
	d.answer(t, "stuck", stuck) // last success at 0 s

	d.tick(t) // 15 s: ping; the stuck probe hangs from here
	d.answer(t, "fine", fine)
	held := recv(t, "the stuck probe", stuck.calls)
	d.tick(t) // 30 s: 0 s is within the interval: ping
	d.answer(t, "fine", fine)
	d.tick(t) // 45 s: no ping
	d.answer(t, "fine", fine)
	d.tick(t) // 60 s: no ping
	d.answer(t, "fine", fine)
	d.wantPings(t, 15*time.Second, 30*time.Second)
	if n := d.logs.count("ERROR", "watchdog part has not run within the interval", "part=stuck"); n != 1 {
		t.Errorf("%d records of the stuck part, want 1", n)
	}

	held <- nil // the stuck part runs again at 60 s
	d.settle(t, "stuck")
	d.tick(t) // 75 s: ping
	d.answer(t, "fine", fine)
	d.answer(t, "stuck", stuck)
	d.wantPings(t, 15*time.Second, 30*time.Second, 75*time.Second)
}

// TestWatchdogNeverRunPartBlocksPings checks that a part that never ran
// keeps every ping back and is logged once a whole interval has passed.
func TestWatchdogNeverRunPartBlocksPings(t *testing.T) {
	fine, never := newProbeCtl(), newProbeCtl()
	d := runDog(t, Part{Name: "fine", Probe: fine.probe}, Part{Name: "activation", Probe: never.probe})
	d.answer(t, "fine", fine)
	recv(t, "the activation probe", never.calls) // never answered
	d.tick(t)                                    // 15 s
	d.answer(t, "fine", fine)
	if n := d.logs.count("watchdog part has not run within the interval"); n != 0 {
		t.Errorf("a part was reported half an interval after the start")
	}
	d.tick(t) // 30 s
	d.answer(t, "fine", fine)
	d.wantPings(t)
	if n := d.logs.count("ERROR", "watchdog part has not run within the interval", "part=activation"); n != 1 {
		t.Errorf("%d records of the part that never ran, want 1", n)
	}
}

// TestWatchdogFailedPingIsLoggedOnce checks that failing pings are logged
// once until a ping succeeds again.
func TestWatchdogFailedPingIsLoggedOnce(t *testing.T) {
	d := runDog(t)
	d.pinger.setErr(errors.New("no service manager"))
	waitFor(t, "the first ping", func() bool { return len(d.pinger.pings()) == 1 })
	d.tick(t)
	waitFor(t, "the second ping", func() bool { return len(d.pinger.pings()) == 2 })
	if n := d.logs.count("WARN", "watchdog notification failed"); n != 1 {
		t.Errorf("%d failure records after two failed pings, want 1", n)
	}
	d.pinger.setErr(nil)
	d.tick(t)
	waitFor(t, "the third ping", func() bool { return len(d.pinger.pings()) == 3 })
	waitFor(t, "the recovery record", func() bool { return d.logs.count("watchdog notification recovered") == 1 })
}

// TestWatchdogProbePanicIsAFailure checks that a probe that panics counts
// as a failed probe: no ping, and the panic is logged.
func TestWatchdogProbePanicIsAFailure(t *testing.T) {
	var calls atomic.Int64
	d := runDog(t, Part{Name: "admin", Probe: func(context.Context) error {
		calls.Add(1)
		panic("admin probe broke")
	}})
	waitFor(t, "the panic record", func() bool { return d.logs.count("ERROR", "watchdog probe panicked", "part=admin") == 1 })
	d.settle(t, "admin")
	d.tick(t)
	waitFor(t, "the second probe", func() bool { return calls.Load() == 2 })
	d.settle(t, "admin")
	d.wantPings(t)
}

// TestWatchdogRunWaitsForItsProbes checks that Run ends its probes and
// returns only after they did.
func TestWatchdogRunWaitsForItsProbes(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	var returned atomic.Bool
	started := make(chan struct{})
	w, err := NewWatchdog(WatchdogOptions{
		Notifier: &fakePinger{clk: clk},
		Interval: interval,
		Clock:    clk,
		Parts: []Part{{Name: "retirer", Probe: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			returned.Store(true)
			return ctx.Err()
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	recv(t, "the probe", started)
	cancel()
	if err := recv(t, "Run to return", done); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if !returned.Load() {
		t.Error("Run returned before its probe")
	}
}

// attemptPinger pings through a Notifier and reports each attempt once
// the Notifier returned.
type attemptPinger struct {
	n        *Notifier
	attempts chan error
}

func (p *attemptPinger) Watchdog(ctx context.Context) error {
	err := p.n.Watchdog(ctx)
	p.attempts <- err
	return err
}

// TestWatchdogThroughHeldNotifier checks a handover successor's watchdog:
// its pings are held back until MainPID, then reach the service manager.
func TestWatchdogThroughHeldNotifier(t *testing.T) {
	s := newSink(t)
	n, _ := openNotifier(t, s, true)
	p := &attemptPinger{n: n, attempts: make(chan error, 1)}
	clk := clocktest.New(time.Unix(0, 0))
	w, err := NewWatchdog(WatchdogOptions{Notifier: p, Interval: interval, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer func() {
		cancel()
		_ = recv(t, "Run to return", done)
	}()
	if err := recv(t, "the first ping", p.attempts); err != nil {
		t.Fatalf("held ping = %v, want nil", err)
	}
	s.nothingSince(t) // the first tick's ping was held back

	if err := n.MainPID(t.Context(), 99); err != nil {
		t.Fatal(err)
	}
	if got := s.next(t); got != "MAINPID=99\nREADY=1\nSTATUS=not ready: no_revision" {
		t.Fatalf("MainPID sent %q", got)
	}
	clk.Advance(interval / 2)
	if err := recv(t, "the second ping", p.attempts); err != nil {
		t.Fatalf("ping after MainPID = %v", err)
	}
	if got := s.next(t); got != "WATCHDOG=1" {
		t.Errorf("after MainPID the watchdog sent %q, want WATCHDOG=1", got)
	}
}
