// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package drain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/gateway/setting"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Readiness is the /readyz state the Drain fails (readiness.Tracker).
type Readiness interface {
	// Drain fails readiness with reason draining, for good.
	Drain()
	// Answered reports whether /readyz ever answered 200.
	Answered() bool
}

// Notifier sends the Drain's sd_notify messages (sdnotify.Notifier).
type Notifier interface {
	// Stopping sends STOPPING=1 with STATUS=draining.
	Stopping(ctx context.Context) error
	// SyncStatus sends the current STATUS= when it changed.
	SyncStatus(ctx context.Context) error
	// Mute stops every later message.
	Mute()
}

// Lock is the data-directory lock the Node holds (nodedir.Lock). A
// handover wraps it to send released once Release returns.
type Lock interface {
	// Release removes holder.json and drops the lock.
	Release() error
}

// Listeners are the client listeners (listener.Set).
type Listeners interface {
	// Bound reports whether a client listener socket accepts.
	Bound() bool
	// StopAccepting closes every dispatching listener; idempotent.
	StopAccepting()
	// ShutdownShard starts the graceful shutdown of shard i of every
	// client listener without waiting.
	ShutdownShard(i int)
	// Shutdown shuts down every shard left and waits for every connection
	// within ctx, then closes the rest.
	Shutdown(ctx context.Context) error
	// Close ends everything and waits for the listeners' goroutines.
	Close() error
}

// Retirer holds the snapshots requests pin (retire.Holder).
type Retirer interface {
	// EndAll ends every pinned request, and every later pin, with reason
	// through the ending protocol.
	EndAll(ctx context.Context, reason snapshot.EndReason) (int, error)
	// Close frees every snapshot once its pins are gone, within ctx.
	Close(ctx context.Context) error
}

// Step is one shutdown step after the Drain deadline, such as the admin
// server's shutdown or the telemetry flush.
type Step struct {
	// Name names the step in logs.
	Name string
	// Run performs the step; it must return once ctx, which ends at the
	// exit bound, is done.
	Run func(ctx context.Context) error
}

// Config configures a Drainer.
type Config struct {
	// Readiness fails /readyz at the Drain's start; required.
	Readiness Readiness
	// Listeners are the client listeners; required.
	Listeners Listeners
	// Retirer ends the remaining work at the deadline; required.
	Retirer Retirer
	// Notifier sends STOPPING=1 and STATUS=; nil without a service
	// manager.
	Notifier Notifier
	// Lock is released at the Drain's start; nil when the process holds
	// no lock.
	Lock Lock
	// StopSource stops the configuration source at the Drain's start,
	// without blocking: nothing activates afterwards and a pending
	// Revision is dropped. nil without a source.
	StopSource func()
	// Connections returns the client connections open now
	// (admission.ConnLimiter.Open): the deadline's record reports those
	// left at the deadline, which the verification runbook expects to be
	// none. nil leaves the count out.
	Connections func() int64
	// Steps run in order after the deadline, within the exit bound: the
	// admin server's shutdown, the Node-wide Components, the post-commit
	// queue flush, then the telemetry flush.
	Steps []Step
	// Clock times the Drain; nil means clock.Real().
	Clock clock.Clock
	// Rand draws the wave jitter; nil means the math/rand/v2 global
	// generator. Only Run's goroutine draws.
	Rand rand.Source
	// Logger receives the Drain's records; nil discards them.
	Logger *slog.Logger
}

// ErrConfig reports an unusable Config.
var ErrConfig = errors.New("drain: invalid configuration")

// Drainer runs a Node's Drain. Its trigger methods are safe for
// concurrent use with each other and with Run.
type Drainer struct {
	readiness   Readiness
	listeners   Listeners
	retirer     Retirer
	notifier    Notifier
	lock        Lock
	stopSource  func()
	connections func() int64
	steps       []Step
	clk         clock.Clock
	rnd         rand.Source
	log         *slog.Logger

	// wake tells Run that the Drain started or jumped to its deadline.
	wake chan struct{}
	// started is closed when the Drain starts.
	started chan struct{}

	mu      sync.Mutex
	kind    kind      // kindNone until the Drain starts
	at      time.Time // when it started
	held    bool      // a handover steers: stop signals and failures wait
	signals int       // stop signals counted toward the jump
	failed  bool      // the exit code is ExitRuntime
	failErr error     // the first failure
	jumped  bool      // a second stop signal moved the deadline to jumpAt
	jumpAt  time.Time
	final   bool // the deadline phase began: a jump does nothing

	once sync.Once
	code int
}

// New returns a Drainer waiting for its trigger.
func New(cfg Config) (*Drainer, error) {
	switch {
	case cfg.Readiness == nil:
		return nil, fmt.Errorf("%w: Readiness is required", ErrConfig)
	case cfg.Listeners == nil:
		return nil, fmt.Errorf("%w: Listeners is required", ErrConfig)
	case cfg.Retirer == nil:
		return nil, fmt.Errorf("%w: Retirer is required", ErrConfig)
	}
	for _, s := range cfg.Steps {
		if s.Name == "" || s.Run == nil {
			return nil, fmt.Errorf("%w: a step needs a name and a function", ErrConfig)
		}
	}
	d := &Drainer{
		readiness:   cfg.Readiness,
		listeners:   cfg.Listeners,
		retirer:     cfg.Retirer,
		notifier:    cfg.Notifier,
		lock:        cfg.Lock,
		stopSource:  cfg.StopSource,
		connections: cfg.Connections,
		steps:       append([]Step(nil), cfg.Steps...),
		clk:         cfg.Clock,
		rnd:         cfg.Rand,
		log:         cfg.Logger,
		wake:        make(chan struct{}, 1),
		started:     make(chan struct{}),
	}
	if d.clk == nil {
		d.clk = clock.Real()
	}
	if d.rnd == nil {
		d.rnd = globalSource{}
	}
	if d.log == nil {
		d.log = slog.New(slog.DiscardHandler)
	}
	return d, nil
}

// Signal records a stop signal, SIGTERM or SIGINT. The first starts a
// Drain on the stop-signal timeline, unless a Drain runs, which counts it
// as its first, or a handover holds, which defers it; the second jumps to
// the deadline; later ones do nothing.
func (d *Drainer) Signal() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.signals++
	switch {
	case d.kind == kindNone && !d.held:
		d.startLocked(kindSignal)
	case d.kind != kindNone && d.signals == 2:
		d.jumpLocked()
	}
}

// Fail reports a part of the Node that stopped after boot: it starts a
// Drain on the stop-signal timeline without STOPPING=1 and makes Run
// return setting.ExitRuntime, so the process manager restarts the Node.
// During a Drain it only changes the exit code; while a handover holds,
// the Drain waits for Handover or Resume.
func (d *Drainer) Fail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.failed {
		d.failed, d.failErr = true, err
	}
	if d.kind == kindNone && !d.held {
		d.startLocked(kindFailure)
	}
}

// Hold defers stop signals and failures while a handover steers, from
// accepted until the linger's end. It reports false, holding nothing,
// once a Drain has started, so the holder refuses the handover with
// draining instead of accepting it.
func (d *Drainer) Hold() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.kind != kindNone {
		return false
	}
	d.held = true
	return true
}

// Handover starts the Drain of an accepted handover at the linger's end,
// where the listeners already stopped accepting, and ends a Hold. A stop
// signal received during the Hold counts as the Drain's first, and two
// jump to the deadline at once. Once a Drain has started it does nothing.
func (d *Drainer) Handover() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.held = false
	if d.kind != kindNone {
		return
	}
	d.startLocked(kindHandover)
	if d.signals >= 2 {
		d.jumpLocked()
	}
}

// Resume ends a Hold without a handover, as when the successor is lost
// during the linger and the holder keeps serving: a stop signal received
// meanwhile starts the stop-signal Drain now, two jumping to its deadline
// at once, and a failure alone starts the failure Drain.
func (d *Drainer) Resume() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.held {
		return
	}
	d.held = false
	switch {
	case d.kind != kindNone:
	case d.signals > 0:
		d.startLocked(kindSignal)
		if d.signals >= 2 {
			d.jumpLocked()
		}
	case d.failed:
		d.startLocked(kindFailure)
	}
}

// Started returns a channel closed when the Drain starts.
func (d *Drainer) Started() <-chan struct{} { return d.started }

// startLocked starts the Drain of kind k now.
func (d *Drainer) startLocked(k kind) {
	d.kind, d.at = k, d.clk.Now()
	close(d.started)
	d.kick()
}

// jumpLocked moves the deadline to now, unless the deadline phase began.
func (d *Drainer) jumpLocked() {
	if d.jumped || d.final {
		return
	}
	d.jumped, d.jumpAt = true, d.clk.Now()
	d.kick()
}

// kick wakes Run without blocking.
func (d *Drainer) kick() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run waits for the Drain to start, runs it and returns the process exit
// code: setting.ExitOK, or setting.ExitRuntime after Fail or a panic in a
// part. When ctx ends before a Drain starts, Run returns
// setting.ExitRuntime at once without draining; when it ends during the
// Drain, every remaining wait ends at once and the remaining steps run
// with ctx done. Start Run before the Node boots, so a stop signal during
// the boot drains at once: the timeline counts from the trigger. Later
// calls wait for the first and return its code.
func (d *Drainer) Run(ctx context.Context) int {
	d.once.Do(func() { d.code = d.run(ctx) })
	return d.code
}

// run is one Drain.
func (d *Drainer) run(ctx context.Context) int {
	k, start, ok := d.awaitStart(ctx)
	if !ok {
		return setting.ExitRuntime
	}
	// A Node no balancer saw ready, with nothing listening, has nothing to
	// drain; a handover's holder always drains its connections.
	skip := false
	if k != kindHandover {
		d.safely(ctx, "readiness", func(context.Context) error {
			skip = !d.readiness.Answered() && !d.listeners.Bound()
			return nil
		})
	}
	s := plan(k, start, skip, d.rnd)
	life, end := d.until(ctx, s.exitBy())
	defer end()
	d.begin(life, k, s)
	if !s.skip && d.waitUntil(ctx, s.stopAccepting) {
		d.safely(ctx, "stop accepting", func(context.Context) error {
			d.listeners.StopAccepting()
			return nil
		})
		d.log.InfoContext(ctx, "drain stopped accepting")
		for i, at := range s.waves {
			if !d.waitUntil(ctx, at) {
				break
			}
			d.safely(ctx, "shard shutdown", func(context.Context) error {
				d.listeners.ShutdownShard(i)
				return nil
			})
			d.log.DebugContext(ctx, "drain shut down a server shard", catalog.KeyShard, i)
		}
		d.waitUntil(ctx, s.deadline)
	}
	return d.finish(life)
}

// awaitStart waits until the Drain starts and returns its kind and start
// time; false when ctx ends first.
func (d *Drainer) awaitStart(ctx context.Context) (kind, time.Time, bool) {
	for {
		d.mu.Lock()
		k, at := d.kind, d.at
		d.mu.Unlock()
		if k != kindNone {
			// The start's wake is consumed, so only a jump wakes the
			// waits below.
			select {
			case <-d.wake:
			default:
			}
			return k, at, true
		}
		select {
		case <-ctx.Done():
			return kindNone, time.Time{}, false
		case <-d.wake:
		}
	}
}

// begin runs the Drain's start: a handover's Drain mutes the Notifier
// first; readiness fails; a stop signal sends STOPPING=1 with
// STATUS=draining and a failure STATUS=draining; the source stops; the
// lock and holder.json are released.
func (d *Drainer) begin(ctx context.Context, k kind, s schedule) {
	d.mu.Lock()
	failErr := d.failErr
	d.mu.Unlock()
	if k == kindFailure && failErr != nil {
		d.log.WarnContext(ctx, "drain started", catalog.KeyReason, k.String(), catalog.KeyError, failErr.Error())
	} else {
		d.log.InfoContext(ctx, "drain started", catalog.KeyReason, k.String())
	}
	if k == kindHandover && d.notifier != nil {
		d.safely(ctx, "notifier", func(context.Context) error {
			d.notifier.Mute()
			return nil
		})
	}
	d.safely(ctx, "readiness", func(context.Context) error {
		d.readiness.Drain()
		return nil
	})
	if d.notifier != nil {
		switch k {
		case kindSignal:
			d.safely(ctx, "notifier", d.notifier.Stopping)
		case kindFailure:
			d.safely(ctx, "notifier", d.notifier.SyncStatus)
		case kindHandover, kindNone:
		}
	}
	if d.stopSource != nil {
		d.safely(ctx, "source", func(context.Context) error {
			d.stopSource()
			return nil
		})
	}
	if d.lock != nil {
		d.safely(ctx, "lock", func(context.Context) error { return d.lock.Release() })
	}
	if s.skip {
		d.log.InfoContext(ctx, "drain goes to its deadline: never ready and no client listener bound")
	}
}

// finish is the deadline phase: every shard left shuts down, the remaining
// work ends with RZ-RT-016, the retirer and the listeners close, then the
// steps run, all within the exit bound: ctx's, the planned one, or the
// jump plus FlushBound after a jump.
func (d *Drainer) finish(ctx context.Context) int {
	d.mu.Lock()
	d.final = true
	jumped, jumpAt := d.jumped, d.jumpAt
	d.mu.Unlock()
	ectx := ctx
	if jumped {
		var end context.CancelFunc
		ectx, end = d.until(ctx, jumpAt.Add(FlushBound))
		defer end()
		d.log.InfoContext(ctx, "drain jumped to its deadline")
	}
	d.safely(ectx, "stop accepting", func(context.Context) error {
		d.listeners.StopAccepting()
		for i := range Waves {
			d.listeners.ShutdownShard(i)
		}
		return nil
	})
	attrs := make([]any, 0, 4)
	if d.connections != nil {
		d.safely(ectx, "connections", func(context.Context) error {
			attrs = append(attrs, "connections", d.connections())
			return nil
		})
	}
	d.safely(ectx, "end requests", func(ctx context.Context) error {
		n, err := d.retirer.EndAll(ctx, snapshot.EndDrain)
		attrs = append(attrs, "ended", n)
		return err
	})
	d.log.InfoContext(ctx, "drain deadline reached", attrs...)
	d.safely(ectx, "retirer", d.retirer.Close)
	d.safely(ectx, "listeners", d.listeners.Shutdown)
	d.safely(ectx, "listeners close", func(context.Context) error { return d.listeners.Close() })
	for _, s := range d.steps {
		d.safely(ectx, s.Name, s.Run)
	}
	d.mu.Lock()
	code := setting.ExitOK
	if d.failed {
		code = setting.ExitRuntime
	}
	d.mu.Unlock()
	d.log.InfoContext(ctx, "drain finished", "exit_code", code)
	return code
}

// safely runs one action of the Drain: an error is logged and the Drain
// goes on; a panic is logged with the step's name, makes the exit code
// setting.ExitRuntime, and the Drain goes on.
func (d *Drainer) safely(ctx context.Context, name string, f func(context.Context) error) {
	defer func() {
		if v := recover(); v != nil {
			d.mu.Lock()
			d.failed = true
			d.mu.Unlock()
			d.log.ErrorContext(ctx, "drain step panicked", "step", name, catalog.KeyError, fmt.Sprint(v))
		}
	}()
	if err := f(ctx); err != nil {
		d.log.WarnContext(ctx, "drain step failed", "step", name, catalog.KeyError, err.Error())
	}
}

// waitUntil waits until at on the clock; false when the Drain jumped to
// its deadline or ctx ended first.
func (d *Drainer) waitUntil(ctx context.Context, at time.Time) bool {
	for {
		d.mu.Lock()
		jumped := d.jumped
		d.mu.Unlock()
		if jumped || ctx.Err() != nil {
			return false
		}
		if !d.clk.Now().Before(at) {
			return true
		}
		t := d.clk.NewTimerAt(at)
		select {
		case <-t.C():
		case <-d.wake:
		case <-ctx.Done():
		}
		t.Stop()
	}
}

// until returns a context that ends with parent or at, on the Drainer's
// clock.
func (d *Drainer) until(parent context.Context, at time.Time) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	t := d.clk.AfterFuncAt(at, cancel)
	return ctx, func() {
		t.Stop()
		cancel()
	}
}
