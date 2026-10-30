// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package revalidate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Background revalidation bounds ("Response caching" and "Stateless data
// plane"; targets).
const (
	// Workers is the number of revalidation workers per Node.
	Workers = 4
	// QueueSize bounds the queued revalidations per Node; Enqueue drops
	// past it.
	QueueSize = 256
	// Deadline bounds one revalidation, from the lease take through the
	// refresh or end-stale write. It equals the revalidation lease's life
	// (statestore.LeaseTake, SET NX PX 5000), so every write lands while
	// the lease can still hold the job's token.
	Deadline = 5 * time.Second
	// MaxHeld bounds the partitions a Node keeps queued, running or held
	// after a lease attempt; Enqueue drops a new partition past it.
	MaxHeld = 4096
	// MaxQueueWait drops a job that waited longer in the queue, unrun.
	MaxQueueWait = Deadline
	// MaxEntry is the largest encoded entry ("Response caching", Size row:
	// 128 KiB, its header blocks and saved request included): a worker
	// reads at most one byte more of a replacing response's body, and a
	// replacement past it ends the stale window.
	MaxEntry = 128 << 10
)

// Saturation: the cache_revalidation_saturated degraded reason
// (docs/architecture/10-observability.md "Degraded states"; targets).
const (
	// SaturatedPercent is the share of offered revalidations dropped, for
	// any Drop* reason, over SaturatedWindow past which the reason is
	// raised.
	SaturatedPercent = 10
	// SaturatedWindow is the rolling window of that share.
	SaturatedWindow = time.Minute
	// SaturatedCheck is how often Run re-evaluates the share, so the
	// reason clears once drops stop.
	SaturatedCheck = time.Second
	// SaturatedSource is the source under which the Revalidator holds the
	// reason.
	SaturatedSource = "cache_revalidation"
)

// Drop reasons: the indexes of Options.Drops, in the label order of
// ruralz_cache_revalidation_drops_total
// (catalog.CacheRevalidationDropReasons).
const (
	// DropQueueFull: the QueueSize queue was full.
	DropQueueFull = iota
	// DropQueueWait: the job waited longer than MaxQueueWait.
	DropQueueWait
	// DropPartitionsFull: the Node already tracked MaxHeld partitions
	// queued, running or held.
	DropPartitionsFull
	// NumDropReasons is the number of reasons.
	NumDropReasons
)

// DropCounters returns the Options.Drops handles from counter, the
// telemetry registry's lookup of a Node-wide counter label set
// (aggregate.Registry.Counter): one per reason, in the label order of
// catalog.CacheRevalidationDropReasons.
func DropCounters(counter func(name string, values ...string) (emit.Counter, error)) ([NumDropReasons]emit.Counter, error) {
	var out [NumDropReasons]emit.Counter
	reasons := catalog.CacheRevalidationDropReasons()
	if len(reasons) != NumDropReasons {
		return out, fmt.Errorf("revalidate: %s has %d reasons, the Revalidator %d", catalog.CacheRevalidationDropsTotal, len(reasons), NumDropReasons)
	}
	for i, reason := range reasons {
		c, err := counter(catalog.CacheRevalidationDropsTotal, reason)
		if err != nil {
			return out, fmt.Errorf("revalidate: %s reason %s: %w", catalog.CacheRevalidationDropsTotal, reason, err)
		}
		out[i] = c
	}
	return out, nil
}

// ComponentName is the Revalidator's filter.Component name.
const ComponentName = "cache revalidation workers"

// ErrRunning reports a Run while another Run of the same Revalidator is
// active.
var ErrRunning = errors.New("revalidate: revalidator already running")

// Internal failures, logged only.
var (
	// errDeadline is the cause of a revalidation canceled at its Deadline.
	errDeadline = errors.New("revalidate: revalidation deadline passed")
	// errNoResponse reports a Replayer that returned neither a response
	// nor an error.
	errNoResponse = errors.New("revalidate: replay returned no response")
)

// Revalidator states.
const (
	stateIdle int32 = iota
	stateRunning
	stateStopped
)

// Job is one background revalidation of a stale variant, queued by the
// cache Filter when it serves the variant stale (UseStale).
type Job struct {
	// Key is the partition whose revalidation lease the job takes; a Node
	// keeps one job per Key.
	Key statestore.CacheKey
	// Variant is the stale variant the refresh or end-stale write targets.
	Variant statestore.Digest
	// Store is the State Store connection holding the entry
	// (filter.BuildEnv.CacheStore).
	Store statestore.Store
	// Timeout is the Policy's effective stateStoreTimeout; each lease
	// write takes the smaller of it and the time left before Deadline.
	Timeout time.Duration
	// Replayer sends the replay (filter.BuildEnv.Replayer).
	Replayer filter.Replayer
	// Route and Upstream name the Route the entry was stored on and the
	// Upstream whose response it holds: the replay's target. A composition
	// entry has no Upstream: its replay runs the whole composition.
	Route, Upstream string
	// Entry is the stale entry.
	Entry Entry
	// Metrics are the handles of the Route that queued the job
	// (filter.Exchange.RouteMetrics), whose Revalidations count the
	// outcome in ruralz_cache_revalidations_total; nil records nothing.
	Metrics *emit.RouteMetrics
	// StoreSkipped are the queuing cache Policy's
	// ruralz_cache_store_skipped_total handles, which count a replacement
	// the size cap (size_limit) or the memory rules (memory) refuse; nil
	// records nothing.
	StoreSkipped *[emit.NumSkipReasons]emit.Counter
}

// valid reports whether j can run: a zero Timeout would leave every lease
// write unattempted (RZ-STS-004).
func (j *Job) valid() bool {
	return j.Store != nil && j.Replayer != nil && j.Entry != nil && j.Timeout > 0 && j.Route != ""
}

// skipped counts a refused replacement for reason (an emit.Skip* index).
func (j *Job) skipped(reason int) {
	if j.StoreSkipped != nil && j.StoreSkipped[reason] != nil {
		j.StoreSkipped[reason].Add(0, 1)
	}
}

// Entry is a stale entry as the cache Filter stored it. A worker calls it
// after Enqueue returned, from one goroutine, so the Filter hands each Job
// an Entry over bytes no request reuses.
type Entry interface {
	// Request returns the saved upstream request to replay, as the
	// request Phases left it minus upstream credentials: a new request
	// with its own header map on each call, without a body.
	Request(ctx context.Context) (*http.Request, error)
	// Header returns the stored response header; it is only read.
	Header() http.Header
	// Authorized reports that the request that stored the entry carried
	// Authorization (Storable).
	Authorized() bool
	// Token returns the generation token the entry was written under: the
	// lease writes apply only while the URI's generation key holds it, and
	// a refreshed or replacing entry is written under it again.
	Token() statestore.Token
	// Refresh returns the entry bytes of r, the stored response freshened
	// by a 304, for statestore.LeaseRefresh.
	Refresh(r *Refreshed) ([]byte, error)
	// Replace returns the entry bytes of r, a full response that replaces
	// the stored one, for statestore.LeaseRefresh: saved as the leg
	// delivered it, over the stored entry's saved request.
	Replace(r *Replacement) ([]byte, error)
}

// Replacement is a full response that replaces a stale entry.
type Replacement struct {
	// Status, Header and Body are the response; Body holds at most
	// MaxEntry bytes.
	Status int
	Header http.Header
	Body   []byte
	// Freshness is FreshnessOf(Header, Received).
	Freshness Freshness
	// InitialAge is InitialAge(Header, Requested, Received).
	InitialAge time.Duration
	// Requested and Received are when the replay was sent and its response
	// arrived, on the Node clock.
	Requested, Received time.Time
}

// Refreshed is a stored response freshened by a 304.
type Refreshed struct {
	// Header is the stored header updated by the 304 (Freshen).
	Header http.Header
	// Freshness is FreshnessOf(Header, Received).
	Freshness Freshness
	// InitialAge is InitialAge(Header, Requested, Received): the age
	// restarts at the revalidation.
	InitialAge time.Duration
	// Requested and Received are when the replay was sent and its 304
	// arrived, on the Node clock.
	Requested, Received time.Time
}

// Outcome is how one job ended.
type Outcome uint8

// Outcomes.
const (
	// OutcomeRefreshed: a 304 selected the stored response and the
	// refresh applied under the lease.
	OutcomeRefreshed Outcome = iota + 1
	// OutcomeEnded: the stale window ended under the lease, the variant
	// deleted unstored: a full response other than a 5xx that the store
	// rules refuse (an unstorable status or header, a partial composition
	// response, other Vary names, no TTL, the size cap or the memory
	// rules), a 304 that does not select the stored response or leaves it
	// unstorable, with other Vary names or no TTL, or an entry that could
	// not be read or encoded.
	OutcomeEnded
	// OutcomeKept: the replay got a 5xx or no response (an error, the
	// Deadline, a missing Route or Upstream); the entry stays for
	// stale-if-error and its TTL.
	OutcomeKept
	// OutcomeLeaseHeld: another holder had the partition's lease.
	OutcomeLeaseHeld
	// OutcomeLeaseFailed: the lease take failed (an RZ-STS failure).
	OutcomeLeaseFailed
	// OutcomeWriteFailed: the refresh or end-stale write failed, or the
	// lease no longer held the job's token.
	OutcomeWriteFailed
	// OutcomeExpired: the job waited longer than MaxQueueWait; not run.
	OutcomeExpired
	// OutcomeCanceled: the Revalidator stopped first; not run.
	OutcomeCanceled
	// OutcomeReplaced: a full response the store rules accept replaced the
	// entry under the lease, as the leg delivered it.
	OutcomeReplaced
	// NumOutcomes sizes Stats.Outcomes; it is no Outcome.
	NumOutcomes
)

// String returns the name of o.
func (o Outcome) String() string {
	switch o {
	case OutcomeRefreshed:
		return "refreshed"
	case OutcomeEnded:
		return "ended"
	case OutcomeKept:
		return "kept"
	case OutcomeLeaseHeld:
		return "lease_held"
	case OutcomeLeaseFailed:
		return "lease_failed"
	case OutcomeWriteFailed:
		return "write_failed"
	case OutcomeExpired:
		return "expired"
	case OutcomeCanceled:
		return "canceled"
	case OutcomeReplaced:
		return "replaced"
	default:
		return "unknown"
	}
}

// result returns o's ruralz_cache_revalidations_total result index: a
// refresh or a replacement is refreshed; a job that ran without changing
// the entry (lease held or failed, write failed) is skipped; expired and
// canceled jobs never ran and have none.
func (o Outcome) result() (int, bool) {
	switch o {
	case OutcomeRefreshed, OutcomeReplaced:
		return emit.RevalidationRefreshed, true
	case OutcomeEnded:
		return emit.RevalidationEnded, true
	case OutcomeKept:
		return emit.RevalidationKept, true
	case OutcomeLeaseHeld, OutcomeLeaseFailed, OutcomeWriteFailed:
		return emit.RevalidationSkipped, true
	default:
		return 0, false
	}
}

// Stats counts Enqueue results and job outcomes since New.
type Stats struct {
	// Queued counts accepted jobs.
	Queued uint64
	// Dropped counts jobs refused because the queue was full, MaxHeld was
	// reached or the Revalidator had stopped.
	Dropped uint64
	// Duplicate counts jobs refused because one for the partition was
	// queued, running or held.
	Duplicate uint64
	// Outcomes counts finished jobs, indexed by Outcome.
	Outcomes [NumOutcomes]uint64
}

// Options configure a Revalidator.
type Options struct {
	// Clock drives the Deadline, queue waits and holds; nil uses
	// clock.Real().
	Clock clock.Clock
	// Rand draws lease tokens; nil uses the math/rand/v2 runtime
	// generator.
	Rand rand.Source
	// Logger receives failed jobs at debug level; nil discards them.
	Logger *slog.Logger
	// Drops are the ruralz_cache_revalidation_drops_total handles by Drop*
	// reason (DropCounters looks them up); a nil handle records nothing.
	Drops [NumDropReasons]emit.Counter
	// Status receives the cache_revalidation_saturated degraded reason;
	// nil reports none.
	Status emit.NodeStatus
}

// item is one queued job.
type item struct {
	job Job
	at  time.Time
}

// Revalidator is the Node-wide filter.Component of background
// revalidation (package documentation, "Background revalidation").
// internal/filter/builtin constructs it once per process and hands it to
// the cache Factory; it is safe for concurrent use.
type Revalidator struct {
	clk   clock.Clock
	rnd   rand.Source
	log   *slog.Logger
	queue chan item

	drops  [NumDropReasons]emit.Counter
	status emit.NodeStatus

	// mu guards state transitions to stopped, held, soonest, win and
	// saturated.
	mu    sync.Mutex
	state atomic.Int32
	// held maps a partition to the end of its hold; the zero time means
	// queued or running.
	held map[statestore.CacheKey]time.Time
	// soonest is no later than the end of any timed hold.
	soonest time.Time
	maxHeld int
	// win counts offered and dropped revalidations over SaturatedWindow;
	// saturated is whether the degraded reason is raised.
	win       satWindow
	saturated bool

	queued, dropped, duplicate atomic.Uint64
	outcomes                   [NumOutcomes]atomic.Uint64

	// finished, set by in-package tests before Run, is called after every
	// job with its outcome.
	finished func(Outcome)
}

var _ filter.Component = (*Revalidator)(nil)

// New returns an idle Revalidator; the supervisor runs it (Run).
func New(o Options) *Revalidator {
	r := &Revalidator{
		clk: o.Clock, rnd: o.Rand, log: o.Logger, drops: o.Drops, status: o.Status,
		queue:   make(chan item, QueueSize),
		held:    map[statestore.CacheKey]time.Time{},
		maxHeld: MaxHeld,
	}
	if r.clk == nil {
		r.clk = clock.Real()
	}
	if r.rnd == nil {
		r.rnd = runtimeSource{}
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	return r
}

// runtimeSource draws from math/rand/v2's runtime generator, which is safe
// for concurrent use.
type runtimeSource struct{}

// Uint64 returns a uniform 64-bit value.
func (runtimeSource) Uint64() uint64 {
	return rand.Uint64() //nolint:gosec // G404: a lease token must be unique across Nodes, not unpredictable.
}

// Name implements filter.Component.
func (*Revalidator) Name() string { return ComponentName }

// Enqueue queues j without blocking and reports whether it was accepted.
// It refuses j when a job for j.Key is queued or running or was attempted
// less than Deadline ago (Duplicate), and when MaxHeld partitions are
// tracked, the queue is full or the Revalidator has stopped (Dropped). The
// first two count in ruralz_cache_revalidation_drops_total, as
// partitions_full and queue_full, and in the saturation share; a stopping
// Node's refusals do not. A job without a Store, Replayer, Entry, Route or
// Timeout is refused uncounted; a composition entry's job has no Upstream.
// Enqueue before Run queues the job for it.
func (r *Revalidator) Enqueue(j Job) bool {
	if !j.valid() {
		return false
	}
	now := r.clk.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.Load() == stateStopped {
		r.dropped.Add(1)
		return false
	}
	if until, ok := r.held[j.Key]; ok {
		if until.IsZero() || now.Before(until) {
			r.duplicate.Add(1)
			return false
		}
	} else if !r.room(now) {
		r.refuse(DropPartitionsFull, now)
		return false
	}
	select {
	case r.queue <- item{job: j, at: now}:
	default:
		r.refuse(DropQueueFull, now)
		return false
	}
	r.held[j.Key] = time.Time{}
	r.queued.Add(1)
	r.win.add(now, 1, 0)
	r.evaluate(now)
	return true
}

// Pending reports whether a job for k is queued or running, or was
// attempted less than Deadline ago, so Enqueue would refuse another as a
// duplicate: the cache Filter then builds no job.
func (r *Revalidator) Pending(k statestore.CacheKey) bool {
	now := r.clk.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	until, ok := r.held[k]
	return ok && (until.IsZero() || now.Before(until))
}

// refuse counts a job Enqueue refused for reason (DropPartitionsFull or
// DropQueueFull): offered and dropped. r.mu is held.
func (r *Revalidator) refuse(reason int, now time.Time) {
	r.dropped.Add(1)
	if c := r.drops[reason]; c != nil {
		c.Add(0, 1)
	}
	r.win.add(now, 1, 1)
	r.evaluate(now)
}

// expire ends the hold of a job dropped for its queue wait and counts it
// dropped (it was counted offered when queued).
func (r *Revalidator) expire(k statestore.CacheKey, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.held, k)
	if c := r.drops[DropQueueWait]; c != nil {
		c.Add(0, 1)
	}
	r.win.add(now, 0, 1)
	r.evaluate(now)
}

// evaluate raises or clears cache_revalidation_saturated: raised while,
// over the last SaturatedWindow, the dropped revalidations exceed
// SaturatedPercent of those offered (a job dropped for its queue wait was
// offered earlier, so drops alone raise it). r.mu is held.
func (r *Revalidator) evaluate(now time.Time) {
	offered, dropped := r.win.sums(now)
	r.setSaturated(dropped*100 > offered*SaturatedPercent)
}

// setSaturated records on and tells Status when it changed. r.mu is held.
func (r *Revalidator) setSaturated(on bool) {
	if on == r.saturated {
		return
	}
	r.saturated = on
	if r.status != nil {
		r.status.SetDegraded(catalog.ReasonCacheRevalidationSaturated, SaturatedSource, on)
	}
}

// satWindow counts offered and dropped revalidations per second over the
// last SaturatedWindow.
type satWindow struct {
	sec, offered, dropped [satBuckets]int64
}

// satBuckets is the number of one-second buckets of SaturatedWindow.
const satBuckets = int64(SaturatedWindow / time.Second)

// add counts offered and dropped revalidations in now's second.
func (w *satWindow) add(now time.Time, offered, dropped int64) {
	s := now.Unix()
	i := ((s % satBuckets) + satBuckets) % satBuckets
	if w.sec[i] != s {
		w.sec[i], w.offered[i], w.dropped[i] = s, 0, 0
	}
	w.offered[i] += offered
	w.dropped[i] += dropped
}

// sums returns the counts of the last SaturatedWindow before now.
func (w *satWindow) sums(now time.Time) (offered, dropped int64) {
	s := now.Unix()
	for i := range w.sec {
		if age := s - w.sec[i]; age >= 0 && age < satBuckets {
			offered += w.offered[i]
			dropped += w.dropped[i]
		}
	}
	return offered, dropped
}

// room reports whether held can take a new partition, first deleting the
// holds that ended when one may have. r.mu is held.
func (r *Revalidator) room(now time.Time) bool {
	if len(r.held) < r.maxHeld {
		return true
	}
	if r.soonest.IsZero() || now.Before(r.soonest) {
		return false
	}
	var next time.Time
	for k, until := range r.held {
		switch {
		case until.IsZero():
		case !now.Before(until):
			delete(r.held, k)
		case next.IsZero() || until.Before(next):
			next = until
		}
	}
	r.soonest = next
	return len(r.held) < r.maxHeld
}

// Run serves the queue with Workers workers until ctx is done, re-checking
// the saturation share every SaturatedCheck so the degraded reason clears
// once drops stop. It then waits for the workers, clears the reason, drops
// the jobs still queued (OutcomeCanceled) and returns nil; later Enqueue
// calls drop. A Run while another is active returns ErrRunning; a stopped
// Revalidator may run again.
func (r *Revalidator) Run(ctx context.Context) error {
	if !r.state.CompareAndSwap(stateIdle, stateRunning) && !r.state.CompareAndSwap(stateStopped, stateRunning) {
		return ErrRunning
	}
	var wg sync.WaitGroup
	for range Workers {
		wg.Go(func() { r.work(ctx) })
	}
	tm := r.clk.NewTimer(SaturatedCheck)
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-tm.C():
			r.mu.Lock()
			r.evaluate(r.clk.Now())
			r.mu.Unlock()
			tm.Reset(SaturatedCheck)
		}
	}
	tm.Stop()
	wg.Wait()
	r.mu.Lock()
	r.state.Store(stateStopped)
	r.setSaturated(false)
	r.mu.Unlock()
	for {
		select {
		case it := <-r.queue:
			r.release(it.job.Key)
			r.finish(OutcomeCanceled, nil)
		default:
			return nil
		}
	}
}

// Stats returns the counters.
func (r *Revalidator) Stats() Stats {
	s := Stats{Queued: r.queued.Load(), Dropped: r.dropped.Load(), Duplicate: r.duplicate.Load()}
	for i := range s.Outcomes {
		s.Outcomes[i] = r.outcomes[i].Load()
	}
	return s
}

// work runs queued jobs until ctx is done.
func (r *Revalidator) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-r.queue:
			r.handle(ctx, &it)
		}
	}
}

// handle runs one dequeued job and holds its partition for Deadline after
// the lease attempt; a job not run releases its partition at once.
func (r *Revalidator) handle(ctx context.Context, it *item) {
	start := r.clk.Now()
	var o Outcome
	switch {
	case ctx.Err() != nil:
		o = OutcomeCanceled
		r.release(it.job.Key)
	case start.Sub(it.at) > MaxQueueWait:
		o = OutcomeExpired
		r.expire(it.job.Key, start)
	default:
		o = r.revalidate(ctx, &it.job, start)
		r.hold(it.job.Key, start.Add(Deadline))
	}
	r.finish(o, it.job.Metrics)
}

// hold keeps k's partition held until until.
func (r *Revalidator) hold(k statestore.CacheKey, until time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.held[k] = until
	if r.soonest.IsZero() || until.Before(r.soonest) {
		r.soonest = until
	}
}

// release ends k's hold.
func (r *Revalidator) release(k statestore.CacheKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.held, k)
}

// finish counts o, in ruralz_cache_revalidations_total on m (the queuing
// Route's handles) when the job ran, and calls the test hook.
func (r *Revalidator) finish(o Outcome, m *emit.RouteMetrics) {
	r.outcomes[o].Add(1)
	if i, ok := o.result(); ok && m != nil && m.Revalidations[i] != nil {
		m.Revalidations[i].Add(0, 1)
	}
	if r.finished != nil {
		r.finished(o)
	}
}

// token returns a nonzero lease token.
func (r *Revalidator) token() uint64 {
	for {
		if t := r.rnd.Uint64(); t != 0 {
			return t
		}
	}
}

// revalidate runs j within Deadline of start: take the lease, replay the
// conditional request, then refresh or end the stale window under the
// lease.
func (r *Revalidator) revalidate(ctx context.Context, j *Job, start time.Time) Outcome {
	jctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// Cancellation alone bounds the job: the clock may be a fake one, so
	// the context carries no wall-clock deadline.
	tm := r.clk.AfterFunc(Deadline, func() { cancel(errDeadline) })
	defer tm.Stop()
	deadline := start.Add(Deadline)
	token := r.token()

	take := r.lease(jctx, j, deadline, statestore.CacheLease{Mode: statestore.LeaseTake, Token: token})
	switch {
	case take.Err != nil:
		r.log.DebugContext(ctx, "cache revalidation lease take failed",
			catalog.KeyUpstream, j.Upstream, catalog.KeyCode, take.Err.Code())
		return OutcomeLeaseFailed
	case !take.Applied:
		return OutcomeLeaseHeld
	}

	stored := j.Entry.Header()
	req, err := j.Entry.Request(jctx)
	if err != nil || req == nil {
		r.log.DebugContext(ctx, "cache revalidation could not read the saved request; ending the stale window",
			catalog.KeyUpstream, j.Upstream)
		return r.end(jctx, j, deadline, token)
	}
	// The stored validators replace the saved request's own preconditions
	// and Range; an entry without validators, such as a merged aggregate
	// response, replays unconditionally, so only a full response can
	// renew it.
	Conditional(req.Header, stored)
	requested := r.clk.Now()
	resp, err := j.Replayer.Replay(jctx, j.Route, j.Upstream, req)
	received := r.clk.Now()
	if err != nil || resp == nil {
		if err == nil {
			err = errNoResponse
		}
		r.log.DebugContext(ctx, "cache revalidation replay got no response; entry kept",
			catalog.KeyUpstream, j.Upstream, catalog.KeyError, err.Error())
		return OutcomeKept
	}
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return r.refresh(jctx, j, deadline, token, stored, resp.Header, requested, received)
	case resp.StatusCode >= http.StatusInternalServerError:
		return OutcomeKept
	default:
		return r.replace(jctx, j, deadline, token, stored, resp, requested, received)
	}
}

// replace applies a full response below 500: it replaces the entry under
// the lease, as the leg delivered it, when the request-path store rules
// accept it (a storable status and header, not a partial composition
// response, the same Vary names, TTL left, the size cap and the memory
// rules), and otherwise ends the stale window unstored. A body that
// cannot be read whole is no response: the entry is kept.
func (r *Revalidator) replace(ctx context.Context, j *Job, deadline time.Time, token uint64, stored http.Header, resp *http.Response, requested, received time.Time) Outcome {
	h := resp.Header
	if h == nil {
		h = http.Header{}
	}
	if !StorableStatus(resp.StatusCode) || Partial(h) || !sameVary(stored, h) || !Storable(h, j.Entry.Authorized()) {
		return r.end(ctx, j, deadline, token)
	}
	f := FreshnessOf(h, received)
	age := InitialAge(h, requested, received)
	ttl := f.TTL(age)
	if ttl <= 0 {
		return r.end(ctx, j, deadline, token)
	}
	var body []byte
	if resp.Body != nil {
		var err error
		if body, err = io.ReadAll(io.LimitReader(resp.Body, MaxEntry+1)); err != nil {
			r.log.DebugContext(ctx, "cache revalidation could not read the replacing body; entry kept",
				catalog.KeyUpstream, j.Upstream, catalog.KeyError, err.Error())
			return OutcomeKept
		}
	}
	if len(body) > MaxEntry {
		j.skipped(emit.SkipSizeLimit)
		return r.end(ctx, j, deadline, token)
	}
	entry, err := j.Entry.Replace(&Replacement{
		Status: resp.StatusCode, Header: h, Body: body, Freshness: f, InitialAge: age,
		Requested: requested, Received: received,
	})
	switch {
	case err != nil:
		r.log.DebugContext(ctx, "cache revalidation could not encode the replacing entry; ending the stale window",
			catalog.KeyUpstream, j.Upstream, catalog.KeyError, err.Error())
		return r.end(ctx, j, deadline, token)
	case len(entry) > MaxEntry:
		j.skipped(emit.SkipSizeLimit)
		return r.end(ctx, j, deadline, token)
	case !j.Store.AdmitStoreBytes(j.Key, len(entry)):
		j.skipped(emit.SkipMemory)
		return r.end(ctx, j, deadline, token)
	}
	w := r.lease(ctx, j, deadline, statestore.CacheLease{Mode: statestore.LeaseRefresh, Token: token, Entry: entry, TTL: ttl, Received: received})
	if w.Err != nil || !w.Applied {
		return OutcomeWriteFailed
	}
	return OutcomeReplaced
}

// refresh applies a 304 with header nm: the freshened entry under the
// lease, or the end of the stale window when the 304 does not select the
// stored response, leaves it unstorable, changes its Vary names or leaves
// no TTL.
func (r *Revalidator) refresh(ctx context.Context, j *Job, deadline time.Time, token uint64, stored, nm http.Header, requested, received time.Time) Outcome {
	h, ok := Freshen(stored, nm, received)
	if !ok || !sameVary(stored, h) || !Storable(h, j.Entry.Authorized()) {
		return r.end(ctx, j, deadline, token)
	}
	rf := Refreshed{
		Header: h, Freshness: FreshnessOf(h, received), InitialAge: InitialAge(h, requested, received),
		Requested: requested, Received: received,
	}
	ttl := rf.Freshness.TTL(rf.InitialAge)
	if ttl <= 0 {
		return r.end(ctx, j, deadline, token)
	}
	entry, err := j.Entry.Refresh(&rf)
	if err != nil {
		r.log.DebugContext(ctx, "cache revalidation could not encode the refreshed entry; ending the stale window",
			catalog.KeyUpstream, j.Upstream, catalog.KeyError, err.Error())
		return r.end(ctx, j, deadline, token)
	}
	w := r.lease(ctx, j, deadline, statestore.CacheLease{Mode: statestore.LeaseRefresh, Token: token, Entry: entry, TTL: ttl, Received: received})
	if w.Err != nil || !w.Applied {
		return OutcomeWriteFailed
	}
	return OutcomeRefreshed
}

// end ends the stale window under the lease: the variant is deleted.
func (r *Revalidator) end(ctx context.Context, j *Job, deadline time.Time, token uint64) Outcome {
	w := r.lease(ctx, j, deadline, statestore.CacheLease{Mode: statestore.LeaseEndStale, Token: token})
	if w.Err != nil || !w.Applied {
		return OutcomeWriteFailed
	}
	return OutcomeEnded
}

// lease sends one lease operation on j's partition and variant, with the
// smaller of the Policy's timeout and the time left before deadline; none
// left leaves it unattempted (RZ-STS-004). A refresh, replacement or
// end-stale names the entry's generation token, so it applies only while
// the URI's generation key holds it.
func (r *Revalidator) lease(ctx context.Context, j *Job, deadline time.Time, l statestore.CacheLease) *statestore.Write {
	l.Key, l.Variant = j.Key, j.Variant
	if l.Mode != statestore.LeaseTake {
		l.Generation = j.Entry.Token()
	}
	w := &statestore.Write{
		Kind:    statestore.OpCacheSet,
		Timeout: max(min(j.Timeout, deadline.Sub(r.clk.Now())), 0),
		Lease:   l,
	}
	j.Store.Write(ctx, []*statestore.Write{w})
	return w
}
