// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package source

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Defaults (target values).
const (
	// PollInterval is how often the Watcher polls the source.
	PollInterval = time.Second
	// DefaultDeadline bounds one load: the validation gate's compile
	// deadline, which the load's time counts against
	// (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
	// "Validation gate").
	DefaultDeadline = 60 * time.Second
)

// Errors.
var (
	// ErrUnavailable reports a source that cannot be read: missing,
	// unreadable, or neither a directory nor a regular file. Nothing was
	// loaded, so nothing is rejected.
	ErrUnavailable = errors.New("source: the configuration source cannot be read")
	// ErrDeadline reports a load that ran past its deadline: the transient
	// RZ-CFG-044.
	ErrDeadline = errors.New("source: the load did not finish within the compile deadline")
	// ErrConfig reports an unusable Config.
	ErrConfig = errors.New("source: invalid configuration")
	// ErrRunning reports a second Run.
	ErrRunning = errors.New("source: the watcher already runs")
)

// PanicError is a panic recovered while a load ran the pipeline: a fault
// of the release, which the activation loader rejects with RZ-CFG-045 and
// logs with its stack.
type PanicError struct {
	// Value is the recovered value.
	Value any
	// Stack is the panicking goroutine's stack.
	Stack []byte
}

// Error describes the panic.
func (e *PanicError) Error() string { return fmt.Sprintf("source: the load panicked: %v", e.Value) }

// Trigger says why a load ran.
type Trigger uint8

// Triggers.
const (
	// TriggerNone is a poll that found nothing to load.
	TriggerNone Trigger = iota
	// TriggerBoot is the first load: Load, or Run's first poll without one.
	TriggerBoot
	// TriggerReplaced is a replaced root, loaded at once.
	TriggerReplaced
	// TriggerSettled is a change the next poll saw no further change to.
	TriggerSettled
	// TriggerReload is Reload: SIGHUP, or the loader's retry of a load.
	TriggerReload
)

// String names the trigger in logs.
func (t Trigger) String() string {
	switch t {
	case TriggerBoot:
		return "boot"
	case TriggerReplaced:
		return "replaced"
	case TriggerSettled:
		return "settled"
	case TriggerReload:
		return "reload"
	default:
		return "none"
	}
}

// Load is one load of the source.
type Load struct {
	// Result is the pipeline's run; nil when Err is set.
	Result *pipeline.Result
	// Err is why there is no Result: ErrUnavailable, ErrDeadline, a
	// *PanicError, or another fault of the release.
	Err error
	// Trigger is why the load ran.
	Trigger Trigger
	// Start is when the load began.
	Start time.Time
	// Spent is how long it took, which counts against the compile
	// deadline of the activation that follows.
	Spent time.Duration
}

// Unavailable reports a load whose source could not be read.
func (l Load) Unavailable() bool { return errors.Is(l.Err, ErrUnavailable) }

// Pipeline is the configuration pipeline facade a Watcher loads with
// (*pipeline.Pipeline).
type Pipeline interface {
	// Run runs stages A to M on one Bundle.
	Run(ctx context.Context, req pipeline.Request) (*pipeline.Result, error)
	// Parse runs stages A to C, for AdminPort.
	Parse(ctx context.Context, src loader.Source) (*loader.Bundle, error)
}

// Config configures a Watcher.
type Config struct {
	// Path is RURALZ_CONFIG: a rendered Bundle directory or one-file
	// Bundle, which need not exist yet. Required.
	Path string
	// Pipeline loads the source; required.
	Pipeline Pipeline
	// Variables are the ${VAR} values: the process environment ruralzd
	// read at start (subst.Environ). Nil defines none.
	Variables subst.Source
	// Sink receives every Load Run makes, on Run's goroutine; it must not
	// block. Required for Run.
	Sink func(Load)
	// Clock times polls, settles and the load deadline; nil means
	// clock.Real().
	Clock clock.Clock
	// Logger receives discarded loads at debug; nil discards it.
	Logger *slog.Logger
	// Interval is the poll interval; zero means PollInterval.
	Interval time.Duration
	// Deadline bounds one load; zero means DefaultDeadline.
	Deadline time.Duration
}

// Watcher watches and loads the source. Run, Load and AdminPort may be
// called from any goroutine; loads run one at a time.
type Watcher struct {
	cfg      Config
	clk      clock.Clock
	log      *slog.Logger
	interval time.Duration
	deadline time.Duration

	running  atomic.Bool
	hup      chan struct{} // cap 1: a Reload waiting for Run
	stop     chan struct{}
	stopOnce sync.Once

	loadMu sync.Mutex // one load at a time

	mu        sync.Mutex
	seen      stamp // the last poll's stamp
	loaded    stamp // the stamp of the tree last delivered
	hasSeen   bool
	hasLoaded bool

	// afterPoll runs after each poll of Run with the trigger the poll
	// loaded with, TriggerNone when it loaded nothing. Tests only.
	afterPoll func(Trigger)
}

// New returns a Watcher of cfg.Path; Run starts it.
func New(cfg Config) (*Watcher, error) {
	switch {
	case cfg.Path == "":
		return nil, fmt.Errorf("%w: Path is required", ErrConfig)
	case cfg.Pipeline == nil:
		return nil, fmt.Errorf("%w: Pipeline is required", ErrConfig)
	case cfg.Interval < 0 || cfg.Deadline < 0:
		return nil, fmt.Errorf("%w: negative interval or deadline", ErrConfig)
	}
	w := &Watcher{
		cfg: cfg, clk: cfg.Clock, log: cfg.Logger, interval: cfg.Interval, deadline: cfg.Deadline,
		hup: make(chan struct{}, 1), stop: make(chan struct{}),
	}
	if w.clk == nil {
		w.clk = clock.Real()
	}
	if w.log == nil {
		w.log = slog.New(slog.DiscardHandler)
	}
	if w.interval == 0 {
		w.interval = PollInterval
	}
	if w.deadline == 0 {
		w.deadline = DefaultDeadline
	}
	return w, nil
}

// Path returns the source path.
func (w *Watcher) Path() string { return w.cfg.Path }

// Reload makes Run load the source at once, skipping the settle: SIGHUP,
// or the loader's retry of a load. A Reload before Run is kept for Run's
// first poll; one after Stop does nothing. It never blocks.
func (w *Watcher) Reload() {
	select {
	case w.hup <- struct{}{}:
	default:
	}
}

// Stop ends Run without waiting and keeps any later Run from loading: a
// Drain stops the source at its start. It is idempotent.
func (w *Watcher) Stop() { w.stopOnce.Do(func() { close(w.stop) }) }

// stopped reports whether Stop was called.
func (w *Watcher) stopped() bool {
	select {
	case <-w.stop:
		return true
	default:
		return false
	}
}

// Run polls the source every interval and delivers each Load to
// Config.Sink until ctx ends or Stop is called; it returns nil then. Its
// first poll loads at once unless Load already loaded the source, in
// which case only a change loads. A second Run returns ErrRunning.
func (w *Watcher) Run(ctx context.Context) error {
	if w.cfg.Sink == nil {
		return fmt.Errorf("%w: Run needs a Sink", ErrConfig)
	}
	if !w.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer w.running.Store(false)
	w.mu.Lock()
	trig := TriggerBoot
	if w.hasLoaded {
		trig = TriggerNone
	}
	w.mu.Unlock()
	select {
	case <-w.hup:
		// A Reload before Run, such as a SIGHUP during the boot.
		if trig == TriggerNone {
			trig = TriggerReload
		}
	default:
	}
	for {
		if w.stopped() || ctx.Err() != nil {
			return nil
		}
		polled := w.poll(ctx, trig)
		if w.afterPoll != nil {
			w.afterPoll(polled)
		}
		t := w.clk.NewTimer(w.interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-w.stop:
			t.Stop()
			return nil
		case <-w.hup:
			trig = TriggerReload
		case <-t.C():
			trig = TriggerNone
		}
		t.Stop()
	}
}

// poll stamps the source and loads it when a load is due: trig forces one
// (boot, Reload), a replaced root loads at once, and any other change
// loads once a poll sees no further change. It returns the trigger it
// loaded with, TriggerNone when it loaded nothing.
func (w *Watcher) poll(ctx context.Context, trig Trigger) Trigger {
	w.loadMu.Lock()
	defer w.loadMu.Unlock()
	cur := w.stamp()
	w.mu.Lock()
	seen, hasSeen := w.seen, w.hasSeen
	loaded, hasLoaded := w.loaded, w.hasLoaded
	w.mu.Unlock()
	if cur.missing {
		// Reported once per disappearance, and on every forced poll.
		w.setSeen(cur)
		if trig != TriggerNone || !hasSeen || !seen.missing {
			if trig == TriggerNone {
				trig = TriggerReplaced
			}
			w.deliver(Load{Err: unavailable(cur.err), Trigger: trig, Start: w.clk.Now()})
			return trig
		}
		return TriggerNone
	}
	if trig == TriggerNone {
		switch {
		case hasLoaded && cur.equal(loaded):
			w.setSeen(cur)
			return TriggerNone
		case !hasSeen || !cur.sameRoot(seen):
			trig = TriggerReplaced
		case cur.equal(seen):
			trig = TriggerSettled
		default:
			w.setSeen(cur)
			return TriggerNone
		}
	}
	w.load(ctx, cur, trig)
	return trig
}

// load loads the tree stamped before and delivers the Load, unless the
// tree changed meanwhile: then it is discarded and loaded again, at once
// for a replaced root, else once it settles.
func (w *Watcher) load(ctx context.Context, before stamp, trig Trigger) {
	for {
		if w.stopped() || ctx.Err() != nil {
			return
		}
		ld := w.loadOnce(ctx, trig)
		after := w.stamp()
		if after.equal(before) {
			w.mu.Lock()
			w.seen, w.hasSeen = before, true
			w.loaded, w.hasLoaded = before, true
			w.mu.Unlock()
			w.deliver(ld)
			return
		}
		w.log.DebugContext(ctx, "configuration source changed while it loaded; the load is discarded",
			catalog.KeyPath, w.cfg.Path, "trigger", trig.String())
		if after.missing {
			w.setSeen(after)
			w.deliver(Load{Err: unavailable(after.err), Trigger: TriggerReplaced, Start: w.clk.Now()})
			return
		}
		if !after.sameRoot(before) {
			before, trig = after, TriggerReplaced
			continue
		}
		// The settle debounce: the next poll that sees no further change
		// loads it.
		w.setSeen(after)
		return
	}
}

// Load loads the source once, outside Run, as a boot does, and records
// what it loaded, so Run's first poll loads only after a change. A tree
// that changes during the load is loaded again: at once when its root
// was replaced, else once a poll sees no further change. It returns when
// a load sees the tree unchanged, or with ctx's error.
func (w *Watcher) Load(ctx context.Context) Load {
	w.loadMu.Lock()
	defer w.loadMu.Unlock()
	before := w.stamp()
	for {
		if err := ctx.Err(); err != nil {
			return Load{Err: err, Trigger: TriggerBoot, Start: w.clk.Now()}
		}
		if before.missing {
			w.setSeen(before)
			return Load{Err: unavailable(before.err), Trigger: TriggerBoot, Start: w.clk.Now()}
		}
		ld := w.loadOnce(ctx, TriggerBoot)
		after := w.stamp()
		if after.equal(before) {
			w.mu.Lock()
			w.seen, w.hasSeen = before, true
			w.loaded, w.hasLoaded = before, true
			w.mu.Unlock()
			return ld
		}
		w.log.DebugContext(ctx, "configuration source changed while it loaded; the load is discarded",
			catalog.KeyPath, w.cfg.Path, "trigger", TriggerBoot.String())
		if !after.missing && after.sameRoot(before) {
			var err error
			if after, err = w.settle(ctx, after); err != nil {
				return Load{Err: err, Trigger: TriggerBoot, Start: w.clk.Now()}
			}
		}
		before = after
	}
}

// settle polls every interval until a poll sees no change since the one
// before, and returns that stamp; ctx's error when ctx ends first.
func (w *Watcher) settle(ctx context.Context, prev stamp) (stamp, error) {
	for {
		if err := w.clk.Sleep(ctx, w.interval); err != nil {
			return prev, err
		}
		cur := w.stamp()
		if cur.equal(prev) || cur.missing || !cur.sameRoot(prev) {
			return cur, nil
		}
		prev = cur
	}
}

// loadOnce runs the pipeline on the source under the deadline, recovering
// a panic as a *PanicError.
func (w *Watcher) loadOnce(ctx context.Context, trig Trigger) (ld Load) {
	start := w.clk.Now()
	ld = Load{Trigger: trig, Start: start}
	lctx, cancel := context.WithCancelCause(ctx)
	timer := w.clk.AfterFunc(w.deadline, func() { cancel(ErrDeadline) })
	defer func() {
		timer.Stop()
		cancel(nil)
		ld.Spent = w.clk.Since(start)
		if v := recover(); v != nil {
			ld.Result, ld.Err = nil, &PanicError{Value: v, Stack: debug.Stack()}
		}
	}()
	res, err := w.cfg.Pipeline.Run(lctx, pipeline.Request{
		Source: loader.Source{Path: w.cfg.Path}, Variables: w.cfg.Variables,
	})
	switch {
	case err == nil && res == nil:
		ld.Err = errors.New("source: the pipeline returned neither a result nor an error")
	case err == nil:
		ld.Result = res
	case errors.Is(context.Cause(lctx), ErrDeadline):
		ld.Err = fmt.Errorf("%w after %v", ErrDeadline, w.deadline)
	case errors.Is(err, loader.ErrSource):
		ld.Err = fmt.Errorf("%w: %w", ErrUnavailable, err)
	default:
		ld.Err = err
	}
	return ld
}

// deliver hands ld to the Sink.
func (w *Watcher) deliver(ld Load) {
	if w.cfg.Sink != nil && !w.stopped() {
		w.cfg.Sink(ld)
	}
}

// setSeen records the last poll's stamp.
func (w *Watcher) setSeen(s stamp) {
	w.mu.Lock()
	w.seen, w.hasSeen = s, true
	w.mu.Unlock()
}

// unavailable wraps the reason a source cannot be read.
func unavailable(err error) error {
	if err == nil {
		return ErrUnavailable
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}
