// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/bits"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/gateway/lkg"
	"github.com/ravindu-rev/ruralz/internal/gateway/readiness"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/gateway/source"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Defaults (target values of docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "Validation gate" and docs/architecture/03-data-plane.md "Sources,
// Last-Known-Good and Drain").
const (
	// CompileDeadline bounds validation gate steps 1 to 12 of one
	// Revision, its load included and its wait for a retirement excluded:
	// a Revision not ready to publish when it expires is rejected with the
	// transient RZ-CFG-044.
	CompileDeadline = 60 * time.Second
	// RetryMin and RetryMax bound the full-jitter backoff of a load whose
	// every failure is transient: the n-th retry waits uniformly from
	// RetryMin to min(RetryMax, RetryMin × 2^n).
	RetryMin = time.Second
	RetryMax = 60 * time.Second
	// maxLoggedDiagnostics is how many diagnostics the warn record of a
	// rejected load carries; the rest go to debug records.
	maxLoggedDiagnostics = 10
)

// RZ codes of the activation, each registered in internal/errcode.
const (
	codeSecret     = "RZ-CFG-026"
	codeBind       = "RZ-CFG-039"
	codeStoreURL   = "RZ-CFG-042"
	codeDeadline   = "RZ-CFG-044"
	codeFault      = "RZ-CFG-045"
	codeNoRevision = "RZ-RT-024"
)

// statusSource is the NodeStatus source of lkg_boot.
const statusSource = "reload"

// Errors.
var (
	// ErrConfig reports an unusable Config.
	ErrConfig = errors.New("reload: invalid configuration")
	// ErrRunning reports a second Run.
	ErrRunning = errors.New("reload: the loader already runs")
	// ErrStopped reports a request after Stop: the Drain stopped the
	// source and nothing activates any more.
	ErrStopped = errors.New("reload: the loader stopped")
	// errDeadline is the cause of an activation canceled at the compile
	// deadline.
	errDeadline = errors.New("reload: compile deadline")
	// errStopped is the cause of an activation canceled by Stop.
	errStopped = errors.New("reload: stopped")
)

// Boot says how a process boots its first Revision (foundation pack
// section 8.2, Boot order).
type Boot uint8

// Boot orders.
const (
	// BootHolder is a process that took the data-directory lock at start:
	// the source, else Last-Known-Good, else not ready. It watches the
	// source once its boot attempt ends.
	BootHolder Boot = iota + 1
	// BootSuccessor is a handover successor: the holder's active Revision
	// from its candidate, or by the file-mode order when the holder has
	// none. Its activations hold their switches until Bound.Listen, so
	// nothing listens before the handover is accepted, and it watches the
	// source only once it took the lock (TookLock).
	BootSuccessor
)

// Origin says where an activated or rejected Revision came from.
type Origin uint8

// Origins.
const (
	// OriginSource is a load of RURALZ_CONFIG.
	OriginSource Origin = iota + 1
	// OriginLastKnownGood is the Revision lkg.json names, booted when no
	// source answered (degraded reason lkg_boot).
	OriginLastKnownGood
	// OriginCandidate is a handover holder's candidate.
	OriginCandidate
)

// String names the origin in logs.
func (o Origin) String() string {
	switch o {
	case OriginSource:
		return "source"
	case OriginLastKnownGood:
		return "last_known_good"
	case OriginCandidate:
		return "candidate"
	default:
		return "none"
	}
}

// Source is the file-mode configuration source (*source.Watcher), whose
// Sink delivers each Load to Loader.Offer.
type Source interface {
	// Run watches the source until ctx ends or Stop is called.
	Run(ctx context.Context) error
	// Load loads the source once, as a boot does.
	Load(ctx context.Context) source.Load
	// Reload loads the source at once.
	Reload()
	// Stop ends Run for good.
	Stop()
}

// Canonical re-enters ruralz.canonical.v1 content through the
// configuration pipeline (*pipeline.Pipeline): Last-Known-Good and
// candidates.
type Canonical interface {
	FromCanonical(ctx context.Context, req pipeline.CanonicalRequest) (*pipeline.Result, error)
}

// Binder binds a snapshot's metric label sets at the swap (emit.Meter).
type Binder interface {
	Bind(p emit.Plan) emit.Binding
}

// StatusSyncer sends STATUS= after a readiness change
// (sdnotify.Notifier).
type StatusSyncer interface {
	SyncStatus(ctx context.Context) error
}

// Config configures a Loader.
type Config struct {
	// Boot is the boot order; required.
	Boot Boot
	// Source is the watched RURALZ_CONFIG; required. Its Sink must call
	// Offer.
	Source Source
	// Pipeline re-enters Last-Known-Good and candidate content; required.
	Pipeline Canonical
	// Compiler compiles each Revision (CompilerOf); required.
	Compiler Compiler
	// Holder publishes and retires the snapshots; required.
	Holder *retire.Holder
	// Readiness is the /readyz state the activation reports to; required.
	Readiness *readiness.Tracker
	// LKG reads and writes Last-Known-Good; required.
	LKG *lkg.Store
	// Lock returns the data-directory lock while the process holds it
	// (handover.Lock.Held), nil otherwise: only the lock holder writes
	// Last-Known-Good. Nil never writes.
	Lock func() *nodedir.Lock
	// Parts are switched at each activation: the client listeners
	// (ListenerPart), the admin port (AdminSocket) and the telemetry
	// settings (TelemetryPart), in that order.
	Parts []Part
	// Meter binds each snapshot's label sets at the swap; nil binds none.
	Meter Binder
	// Metrics are the configuration handles of the Node metrics
	// (emit.NodeMetrics.Config); nil records nothing.
	Metrics *emit.ConfigMetrics
	// Status raises and clears lkg_boot; nil reports nothing.
	Status emit.NodeStatus
	// Notifier sends STATUS= after every readiness change; nil sends none.
	Notifier StatusSyncer
	// RequestID returns the requestId of the /config/dump problem
	// document (admin.RequestID); required.
	RequestID func(*http.Request) string
	// Clock times the deadline, the retries and the records; nil means
	// clock.Real().
	Clock clock.Clock
	// Rand draws the retry jitter; nil means the math/rand/v2 runtime
	// generator. It is called under the Loader's lock only.
	Rand rand.Source
	// Logger receives the activation's records; nil discards them.
	Logger *slog.Logger
	// Deadline is the compile deadline; zero means CompileDeadline.
	Deadline time.Duration
	// FileLimit returns the soft RLIMIT_NOFILE, false where there is none;
	// nil reads the process's own.
	FileLimit func() (uint64, bool)
}

// Loader is the activation loader of a Node: it boots the first
// Revision, activates every later one, keeps Last-Known-Good, and serves
// /debug/snapshots and /config/dump. Its methods are safe for concurrent
// use.
type Loader struct {
	cfg      Config
	clk      clock.Clock
	log      *slog.Logger
	rnd      rand.Source
	deadline time.Duration
	metrics  emit.ConfigMetrics
	reqID    func(*http.Request) string

	running atomic.Bool
	kick    chan struct{} // cap 1
	booted  chan struct{} // closed once the first boot attempt ended

	mu sync.Mutex
	// active is the published Revision and origin its origin.
	active revision.Revision
	origin Origin
	// lkgBoot is set while lkg_boot is raised.
	lkgBoot bool
	// next is the latest Revision waiting to activate: pending a
	// retirement or a hold, or a transient rejection's retry.
	next *candidate
	// reloadDue is when the source is loaded again after a load that ran
	// past the compile deadline; reloadAttempt counts such loads.
	reloadDue     time.Time
	reloadAttempt int
	// lastRejected is the last rejected load, nil before one.
	lastRejected *adminapi.Rejection
	// lkgCandidate and lkgPointer are the digests candidate.json and
	// lkg.json name, as this process read or wrote them; lkgFailures
	// counts failed writes in a row, and lkgDue is when the next runs.
	lkgCandidate, lkgPointer revision.Digest
	lkgFailures              int
	lkgDue                   time.Time
	// holds counts the handover holds; activations wait while positive.
	holds int
	// stopped is set by Stop; closed when the retirer closed.
	stopped, closed bool
	// bootDone is set when the boot job ended; lockTaken when a handover
	// successor took the lock; bootIssues when a rejected source load set
	// the readiness reasons while no Revision was active.
	bootDone, lockTaken, bootIssues bool
	// watching is set once the source watcher started; waiting while a
	// retirement waiter runs.
	watching, waiting bool
	// job is the running job, nil when idle.
	job *job
	// deferred holds a handover successor's switches until Listen; held
	// are the switches its last activation holds.
	deferred bool
	held     []Switch
	// probes are the Probe calls waiting for the loop; requests the
	// Reload calls waiting for a job.
	probes   map[chan struct{}]struct{}
	requests []*request
	// changed is closed and replaced on every job change.
	changed chan struct{}
}

// job is the one job running at a time: the boot, an activation, a
// handover reload or a Last-Known-Good write.
type job struct {
	cancel context.CancelCauseFunc
	// since is when the job, or its current activation, started: past
	// since plus the deadline the loop stops answering probes.
	since time.Time
}

// request is a handover successor's Reload waiting for its job.
type request struct {
	digest revision.Digest
	done   chan error // cap 1
}

// New returns a Loader; Run starts it.
func New(cfg Config) (*Loader, error) {
	missing := ""
	switch {
	case cfg.Boot != BootHolder && cfg.Boot != BootSuccessor:
		missing = "Boot"
	case cfg.Source == nil:
		missing = "Source"
	case cfg.Pipeline == nil:
		missing = "Pipeline"
	case cfg.Compiler == nil:
		missing = "Compiler"
	case cfg.Holder == nil:
		missing = "Holder"
	case cfg.Readiness == nil:
		missing = "Readiness"
	case cfg.LKG == nil:
		missing = "LKG"
	case cfg.RequestID == nil:
		missing = "RequestID"
	}
	if missing != "" {
		return nil, fmt.Errorf("%w: %s is required", ErrConfig, missing)
	}
	for _, p := range cfg.Parts {
		if p == nil {
			return nil, fmt.Errorf("%w: a nil Part", ErrConfig)
		}
	}
	if cfg.Deadline < 0 {
		return nil, fmt.Errorf("%w: negative Deadline", ErrConfig)
	}
	l := &Loader{
		cfg: cfg, clk: cfg.Clock, log: cfg.Logger, rnd: cfg.Rand, deadline: cfg.Deadline, reqID: cfg.RequestID,
		kick: make(chan struct{}, 1), booted: make(chan struct{}), changed: make(chan struct{}),
		deferred: cfg.Boot == BootSuccessor,
	}
	if l.clk == nil {
		l.clk = clock.Real()
	}
	if l.log == nil {
		l.log = slog.New(slog.DiscardHandler)
	}
	if l.rnd == nil {
		l.rnd = runtimeSource{}
	}
	if l.deadline == 0 {
		l.deadline = CompileDeadline
	}
	l.metrics = nopMetrics()
	if m := cfg.Metrics; m != nil {
		fill(&l.metrics, *m)
	}
	return l, nil
}

// Booted returns a channel closed once the first boot attempt ended,
// activated or not: a process that took the lock at start then sends
// READY=1 (sdnotify.Notifier.Ready), so a start never waits on usable
// configuration.
func (l *Loader) Booted() <-chan struct{} { return l.booted }

// Run boots the first Revision, then activates every later one until ctx
// ends: the activation loop. It returns nil once ctx ended and every
// goroutine it started returned. A second Run returns ErrRunning.
func (l *Loader) Run(ctx context.Context) error {
	if !l.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	jctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		l.mu.Lock()
		if l.job != nil {
			l.job.cancel(context.Canceled)
		}
		l.mu.Unlock()
		wg.Wait()
	}()
	l.initPointers(ctx)
	l.checkFileLimit(ctx, 0)
	l.mu.Lock()
	l.startJobLocked(jctx, &wg, l.boot)
	l.mu.Unlock()
	for {
		due := l.plan(jctx, &wg)
		var fire <-chan time.Time
		var t clock.Timer
		if !due.IsZero() {
			t = l.clk.NewTimer(due.Sub(l.clk.Now()))
			fire = t.C()
		}
		select {
		case <-ctx.Done():
			if t != nil {
				t.Stop()
			}
			return nil
		case <-l.kick:
		case <-fire:
		}
		if t != nil {
			t.Stop()
		}
	}
}

// plan answers the waiting probes and starts what is due, at most one
// job: a handover Reload, the latest pending Revision once the retirer
// allows it, a Last-Known-Good write, a source reload or the watcher. It
// returns when it next has something due, zero for nothing.
func (l *Loader) plan(ctx context.Context, wg *sync.WaitGroup) time.Time {
	holder := l.cfg.Lock != nil && l.cfg.Lock() != nil
	now := l.clk.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.job == nil || now.Sub(l.job.since) <= l.deadline {
		for ch := range l.probes {
			close(ch)
		}
		l.probes = nil
	}
	if l.stopped || l.closed {
		return time.Time{}
	}
	if l.watchDueLocked() {
		l.watching = true
		wg.Go(func() {
			if err := l.cfg.Source.Run(ctx); err != nil {
				l.log.ErrorContext(ctx, "configuration source watcher failed", "error", err.Error())
			}
		})
	}
	var due time.Time
	if !l.reloadDue.IsZero() {
		if now.Before(l.reloadDue) {
			due = earliest(due, l.reloadDue)
		} else {
			l.reloadDue = time.Time{}
			l.cfg.Source.Reload()
		}
	}
	if l.job != nil {
		return due
	}
	if len(l.requests) > 0 {
		req := l.requests[0]
		l.requests = l.requests[1:]
		l.startJobLocked(ctx, wg, func(ctx context.Context) { req.done <- l.reload(ctx, req.digest) })
		return due
	}
	if l.holds > 0 || !l.bootDone {
		return due
	}
	if c := l.next; c != nil {
		switch {
		case c.digest == l.active.Digest:
			l.next = nil
		case now.Before(c.due):
			due = earliest(due, c.due)
		case !l.cfg.Holder.CanActivate():
			l.waitRetirementLocked(ctx, wg)
		default:
			l.next = nil
			l.startJobLocked(ctx, wg, func(ctx context.Context) { l.activate(ctx, c) })
			return due
		}
	}
	if holder && l.lkgOwedLocked() {
		if now.Before(l.lkgDue) {
			due = earliest(due, l.lkgDue)
		} else {
			l.startJobLocked(ctx, wg, l.writeLKG)
		}
	}
	return due
}

// watchDueLocked reports whether the watcher should start now: a lock
// holder at start once its boot attempt ended, a handover successor once
// it took the lock.
func (l *Loader) watchDueLocked() bool {
	if l.watching {
		return false
	}
	switch l.cfg.Boot {
	case BootHolder:
		return l.bootDone
	default:
		return l.bootDone && l.lockTaken
	}
}

// waitRetirementLocked starts the goroutine that waits until the retirer
// accepts a new snapshot, so grace is never cut short; the loop plans
// again when it returns.
func (l *Loader) waitRetirementLocked(ctx context.Context, wg *sync.WaitGroup) {
	if l.waiting {
		return
	}
	l.waiting = true
	wg.Go(func() {
		err := l.cfg.Holder.WaitActivate(ctx)
		l.mu.Lock()
		l.waiting = false
		if errors.Is(err, retire.ErrClosed) {
			l.closed, l.next = true, nil
		}
		l.mu.Unlock()
		l.wake()
	})
}

// startJobLocked runs fn as the one job; the loop plans again when it
// ends.
func (l *Loader) startJobLocked(ctx context.Context, wg *sync.WaitGroup, fn func(ctx context.Context)) {
	jctx, cancel := context.WithCancelCause(ctx)
	j := &job{cancel: cancel, since: l.clk.Now()}
	l.job = j
	wg.Go(func() {
		defer func() {
			cancel(nil)
			l.mu.Lock()
			if l.job == j {
				l.job = nil
			}
			l.broadcastLocked()
			l.mu.Unlock()
			l.wake()
		}()
		fn(jctx)
	})
}

// mark restarts the running job's overdue clock: each activation of a
// job gets its own compile deadline.
func (l *Loader) mark() {
	now := l.clk.Now()
	l.mu.Lock()
	if l.job != nil {
		l.job.since = now
	}
	l.mu.Unlock()
}

// wake nudges the loop without blocking.
func (l *Loader) wake() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// broadcastLocked wakes every Hold and Listen waiting for the job to end.
func (l *Loader) broadcastLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// waitIdleLocked waits, with l.mu held, until no job runs; it returns
// with l.mu held, or ctx's error with l.mu released.
func (l *Loader) waitIdleLocked(ctx context.Context) error {
	for l.job != nil {
		ch := l.changed
		l.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		l.mu.Lock()
	}
	return nil
}

// Probe returns nil once the activation loop has run after the call
// began: it is the loop's watchdog probe (sdnotify.Probe). A job that
// runs past its compile deadline keeps the loop from answering, as does a
// loop that does not run; Probe then returns ctx's error when ctx ends.
func (l *Loader) Probe(ctx context.Context) error {
	ch := make(chan struct{})
	l.mu.Lock()
	if l.probes == nil {
		l.probes = make(map[chan struct{}]struct{})
	}
	l.probes[ch] = struct{}{}
	l.mu.Unlock()
	l.wake()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		l.mu.Lock()
		delete(l.probes, ch)
		l.mu.Unlock()
		return fmt.Errorf("reload: probe: %w", ctx.Err())
	}
}

// Stop stops the source at a Drain's start, without blocking: nothing
// activates afterwards, a pending Revision and every retry are dropped,
// an activation still before its swap is abandoned and a handover
// Reload fails with ErrStopped. The lock and Last-Known-Good pass to any
// successor. It is idempotent.
func (l *Loader) Stop() {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return
	}
	l.stopped = true
	l.next, l.reloadDue = nil, time.Time{}
	if l.job != nil {
		l.job.cancel(errStopped)
	}
	reqs := l.requests
	l.requests = nil
	l.mu.Unlock()
	for _, r := range reqs {
		r.done <- ErrStopped
	}
	l.cfg.Source.Stop()
	l.wake()
}

// Active returns the active Revision's digest, zero before the first
// activation (handover.SuccessorConfig.Active).
func (l *Loader) Active() revision.Digest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active.Digest
}

// earliest returns the earlier of a and b, treating zero as none.
func earliest(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

// backoffLocked returns the full-jitter wait before retry n (n ≥ 1):
// uniform from RetryMin to min(RetryMax, RetryMin × 2^n), so every wait
// lies in [1 s, 60 s].
func (l *Loader) backoffLocked(n int) time.Duration {
	ceiling := RetryMax
	if n < 6 {
		ceiling = min(RetryMax, RetryMin<<max(n, 1))
	}
	return RetryMin + uniform(l.rnd, ceiling-RetryMin)
}

// uniform returns a duration uniformly drawn from [0, d].
func uniform(src rand.Source, d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	hi, _ := bits.Mul64(src.Uint64(), uint64(d)+1)
	return time.Duration(hi) //nolint:gosec // G115: hi ≤ d, a positive Duration.
}

// runtimeSource is the math/rand/v2 runtime generator, safe for
// concurrent use.
type runtimeSource struct{}

func (runtimeSource) Uint64() uint64 { return rand.Uint64() }

// stamp formats t in RFC 3339 UTC.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
