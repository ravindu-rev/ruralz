// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sdnotify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Watchdog errors.
var (
	// ErrWatchdogUsec reports a WATCHDOG_USEC that is not a whole number of
	// microseconds a time.Duration holds.
	ErrWatchdogUsec = errors.New("sdnotify: WATCHDOG_USEC is not a number of microseconds")
	// ErrWatchdogPID reports a WATCHDOG_PID that is not a process ID.
	ErrWatchdogPID = errors.New("sdnotify: WATCHDOG_PID is not a process ID")
	// ErrWatchdog reports unusable WatchdogOptions.
	ErrWatchdog = errors.New("sdnotify: invalid watchdog options")
)

// Env is what the service manager tells this process through its
// environment.
type Env struct {
	// Socket is NOTIFY_SOCKET; empty without a service manager.
	Socket string
	// Watchdog is the watchdog interval (WatchdogInterval); 0 without one.
	Watchdog time.Duration
}

// ReadEnv reads NOTIFY_SOCKET, WATCHDOG_USEC and WATCHDOG_PID through
// getenv (os.Getenv) for the process pid (os.Getpid()). On an error the
// Env still holds the socket.
func ReadEnv(getenv func(string) string, pid int) (Env, error) {
	e := Env{Socket: getenv(EnvNotifySocket)}
	iv, err := WatchdogInterval(getenv(EnvWatchdogUsec), getenv(EnvWatchdogPID), pid)
	if err != nil {
		return e, err
	}
	e.Watchdog = iv
	return e, nil
}

// WatchdogInterval returns the watchdog interval the service manager asks
// of the process pid, as sd_watchdog_enabled(3) decides: WATCHDOG_USEC
// microseconds when WATCHDOG_PID is empty or names pid; 0, no watchdog,
// when WATCHDOG_USEC is empty or 0, or when WATCHDOG_PID names another
// process, from which the variables were inherited.
func WatchdogInterval(usec, watchdogPID string, pid int) (time.Duration, error) {
	if usec == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(usec, 10, 64)
	if err != nil || v < 0 || v > int64(math.MaxInt64/time.Microsecond) {
		return 0, fmt.Errorf("%w: %q", ErrWatchdogUsec, usec)
	}
	if watchdogPID != "" {
		p, err := strconv.Atoi(watchdogPID)
		if err != nil || p <= 0 {
			return 0, fmt.Errorf("%w: %q", ErrWatchdogPID, watchdogPID)
		}
		if p != pid {
			return 0, nil
		}
	}
	return time.Duration(v) * time.Microsecond, nil
}

// Probe checks that one part of the Node runs: it returns nil once the
// part has run after the call began, and an error when it cannot tell. It
// must return once ctx is done.
type Probe func(ctx context.Context) error

// Part is one part of the Node the watchdog requires to have run within
// each interval.
type Part struct {
	// Name names the part in logs, such as "retirer".
	Name string
	// Probe checks that the part runs.
	Probe Probe
}

// Pinger sends WATCHDOG=1 (Notifier.Watchdog).
type Pinger interface {
	Watchdog(ctx context.Context) error
}

// WatchdogOptions configure a Watchdog.
type WatchdogOptions struct {
	// Notifier sends the pings; required with an Interval.
	Notifier Pinger
	// Interval is the watchdog interval (WatchdogInterval); 0 means no
	// watchdog, and Run returns at once.
	Interval time.Duration
	// Parts are the admin server, the retirer and the activation loop:
	// every one must have run within the interval for a ping. Names are
	// unique and every Part has a Probe.
	Parts []Part
	// Clock times the pings and probe results; nil means clock.Real().
	Clock clock.Clock
	// Logger receives a record when a part stops running within the
	// interval and when a ping fails; nil discards them.
	Logger *slog.Logger
}

// Watchdog feeds the service manager's watchdog.
type Watchdog struct {
	pinger   Pinger
	interval time.Duration
	clk      clock.Clock
	log      *slog.Logger

	mu    sync.Mutex
	parts []*part
}

// part is one Part's probe state; Watchdog.mu guards its fields.
type part struct {
	Part
	ok      bool      // a probe succeeded once
	last    time.Time // when the last successful probe returned
	running bool      // a probe is in flight
	late    bool      // reported as not run within the interval
}

// NewWatchdog returns a Watchdog; it starts nothing, Run does.
func NewWatchdog(o WatchdogOptions) (*Watchdog, error) {
	if o.Interval < 0 {
		return nil, fmt.Errorf("%w: negative interval %v", ErrWatchdog, o.Interval)
	}
	if o.Interval > 0 && o.Notifier == nil {
		return nil, fmt.Errorf("%w: no Notifier", ErrWatchdog)
	}
	w := &Watchdog{pinger: o.Notifier, interval: o.Interval, clk: o.Clock, log: o.Logger}
	if w.clk == nil {
		w.clk = clock.Real()
	}
	if w.log == nil {
		w.log = slog.New(slog.DiscardHandler)
	}
	names := make(map[string]bool, len(o.Parts))
	for _, p := range o.Parts {
		switch {
		case p.Name == "" || p.Probe == nil:
			return nil, fmt.Errorf("%w: a part needs a name and a probe", ErrWatchdog)
		case names[p.Name]:
			return nil, fmt.Errorf("%w: part %q twice", ErrWatchdog, p.Name)
		}
		names[p.Name] = true
		w.parts = append(w.parts, &part{Part: p})
	}
	return w, nil
}

// Interval returns the watchdog interval; 0 means none.
func (w *Watchdog) Interval() time.Duration { return w.interval }

// Run feeds the watchdog until ctx ends. Every half interval, starting at
// once, it sends WATCHDOG=1 when every part's last probe succeeded within
// the interval, the send bounded by the next tick, then starts the probe
// of every part with none in flight, so a probe that hangs only keeps its
// part from counting as run. Until its first WATCHDOG=1, a tick sends one
// as soon as every part has run, so the first follows the start by the
// probes' time, not a half interval: a handover successor starts its
// Watchdog once it took the lock, while the service manager's timer runs
// from its predecessor's last ping, up to a half interval earlier.
// Without an interval it returns at once. It returns nil once ctx is done
// and every probe it started has returned.
func (w *Watchdog) Run(ctx context.Context) error {
	if w.interval <= 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	var probes sync.WaitGroup
	defer probes.Wait()
	defer cancel()
	half := w.interval / 2
	started := w.clk.Now()
	// ran wakes the wait for the first ping when a probe succeeds.
	ran := make(chan struct{}, 1)
	pinged, failing := false, false
	for {
		// One timer per tick: it starts the next tick and ends this
		// tick's send.
		tick, endTick := context.WithCancel(ctx)
		t := w.clk.AfterFunc(half, endTick)
		sent := w.alive(ctx, started)
		if sent {
			failing = w.ping(ctx, tick, failing)
		}
		w.startProbes(ctx, &probes, ran)
		if !sent && !pinged {
			sent, failing = w.awaitFirst(ctx, tick, started, ran, failing)
		}
		pinged = pinged || sent
		<-tick.Done()
		if ctx.Err() != nil {
			t.Stop()
			endTick()
			return nil
		}
	}
}

// awaitFirst waits, until tick ends, for every part to have run, then
// sends the first WATCHDOG=1 within tick. It returns whether it sent one
// and whether pings are failing.
func (w *Watchdog) awaitFirst(ctx, tick context.Context, started time.Time, ran <-chan struct{}, failing bool) (sent, stillFailing bool) {
	for {
		select {
		case <-ran:
			if w.alive(ctx, started) {
				return true, w.ping(ctx, tick, failing)
			}
		case <-tick.Done():
			return false, failing
		}
	}
}

// ping sends WATCHDOG=1 within tick and returns whether it failed. A failure
// is logged once until a ping succeeds again, which is logged too.
func (w *Watchdog) ping(ctx, tick context.Context, failing bool) bool {
	err := w.pinger.Watchdog(tick)
	switch {
	case err != nil && !failing:
		w.log.WarnContext(ctx, "watchdog notification failed", catalog.KeyError, err.Error())
	case err == nil && failing:
		w.log.InfoContext(ctx, "watchdog notification recovered")
	}
	return err != nil
}

// alive reports whether every part's last probe succeeded within the
// interval. It logs, once until the part runs again, each part that has
// not run within the interval: one whose last success is older, or one
// that never succeeded a whole interval after Run started.
func (w *Watchdog) alive(ctx context.Context, started time.Time) bool {
	since := w.clk.Now().Add(-w.interval)
	w.mu.Lock()
	defer w.mu.Unlock()
	ok := true
	for _, p := range w.parts {
		ran := p.ok && !p.last.Before(since)
		switch {
		case ran:
			p.late = false
		case !p.late && (p.ok || !started.After(since)):
			p.late = true
			w.log.ErrorContext(ctx, "watchdog part has not run within the interval", "part", p.Name)
		}
		ok = ok && ran
	}
	return ok
}

// startProbes starts the probe of every part with none in flight; each
// records its success time on the Watchdog's clock and then signals ran
// without blocking.
func (w *Watchdog) startProbes(ctx context.Context, probes *sync.WaitGroup, ran chan<- struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.parts {
		if p.running {
			continue
		}
		p.running = true
		probes.Go(func() {
			err := w.probe(ctx, p)
			w.mu.Lock()
			defer w.mu.Unlock()
			p.running = false
			if err != nil {
				return
			}
			p.ok, p.last = true, w.clk.Now()
			select {
			case ran <- struct{}{}:
			default:
			}
		})
	}
}

// probe runs one part's probe; a panic counts as a failed probe.
func (w *Watchdog) probe(ctx context.Context, p *part) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("sdnotify: probe %q panicked: %v", p.Name, v)
			w.log.ErrorContext(ctx, "watchdog probe panicked", "part", p.Name, catalog.KeyError, err.Error())
		}
	}()
	return p.Probe(ctx)
}
