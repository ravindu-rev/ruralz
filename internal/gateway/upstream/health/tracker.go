// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// ErrNotNormalized reports an identity list that is not sorted with unique
// entries (the order of discovery.Set and balance.Normalize).
var ErrNotNormalized = errors.New("health: endpoint identities must be sorted and unique")

// endpoint is the Node-lifetime state of one Endpoint. Every instant in it
// is in nanoseconds since its Tracker's base (see Tracker.at).
type endpoint struct {
	identity string

	// Passive ejection (05 reqs 18, 19). ejectedUntil is 0 when never
	// ejected; ejections and decayFrom are guarded by Tracker.mu.
	consecutive  atomic.Int32
	ejectedUntil atomic.Int64
	ejections    int
	decayFrom    int64

	// Active health (05 req 20); the counters are guarded by Tracker.mu.
	unhealthy atomic.Bool
	probeOK   int
	probeFail int
	probing   atomic.Bool

	// Stall and suspect (05 req 21): attempts to this Endpoint that are
	// stalled now, and the last time any attempt to it got headers (0:
	// none since the base; a stall needs StallAfter past an attempt's
	// start, so that reads as long enough ago).
	stalled     atomic.Int32
	lastHeaders atomic.Int64

	// failures counts attempts matching failureWhen (05 req 13).
	failures window

	// removed is set when the Endpoint leaves the set.
	removed atomic.Bool
}

// down reports 05 req 11 step (b): passively ejected or actively
// unhealthy at now.
func (e *endpoint) down(now int64) bool {
	return e.ejectedUntil.Load() > now || e.unhealthy.Load()
}

// suspect reports 05 req 21: holding a stalled attempt and no response
// headers for SuspectAfter.
func (e *endpoint) suspect(now int64) bool {
	return e.stalled.Load() > 0 && now-e.lastHeaders.Load() >= int64(SuspectAfter)
}

// table is one immutable Endpoint list, indexed like the Upstream's
// discovery.Set and balancer list.
type table struct {
	eps []*endpoint
}

// Config configures a Tracker.
type Config struct {
	// Upstream is the Upstream's metadata.name.
	Upstream string
	// Clock drives ejection expiry and stall timers; nil means
	// clock.Real(). The Tracker measures time as elapsed since its own
	// creation (Clock.Since), so a wall-clock step moves no deadline.
	Clock clock.Clock
	// Passive is the current snapshot's healthCheck.passive.
	Passive PassivePolicy
	// Active is the current snapshot's healthCheck.active; nil: off.
	Active *ActivePolicy
	// MaxConnections is circuitBreaker.maxConnections (0: the default).
	MaxConnections int
	// Metrics are the current snapshot's Upstream handles (may be nil
	// until the first activation; see SetMetrics).
	Metrics *emit.UpstreamMetrics
	// OnHealthy, when set, is called with the Endpoints that are neither
	// passively ejected nor actively unhealthy, and the Endpoint count,
	// whenever either changes. healthy == 0 with total > 0 is panic mode
	// (05 req 11 step b). It runs on the goroutine that caused the change
	// (an attempt, a probe, the ejection expiry timer or SetEndpoints),
	// one call at a time, and the last call always carries the current
	// count. It must be quick and must not call the Tracker's mutating
	// methods (SetEndpoints, SetPolicy, RecordProbe, Close, attempt
	// outcomes).
	OnHealthy func(healthy, total int)
}

// Tracker holds the health state of one Upstream's Endpoints. It is safe
// for concurrent use; the request-path methods (View, Attempt) take no
// lock and allocate nothing.
//
// The methods taking a now expect a reading of the Tracker's Clock (on
// clock.Real it carries the monotonic reading, so durations between such
// readings ignore wall-clock steps); the Tracker's own timers read the
// Clock's elapsed time since its creation.
type Tracker struct {
	upstream  string
	clk       clock.Clock
	base      time.Time // clk reading at creation: instant 0
	onHealthy func(healthy, total int)

	tab      atomic.Pointer[table]
	passive  atomic.Pointer[PassivePolicy]
	active   atomic.Pointer[ActivePolicy]
	maxConns atomic.Int64
	metrics  atomic.Pointer[emit.UpstreamMetrics]
	notify   atomic.Pointer[func()]

	// stalled counts stalled attempts across the Upstream (05 req 21).
	stalled atomic.Int64

	mu       sync.Mutex // ejections, probe verdicts, recounts, the expiry timer
	expiry   clock.Timer
	expiryAt int64
	healthy  int
	total    int
	closed   bool

	// reportMu serializes OnHealthy deliveries and guards the last one;
	// it is taken before t.mu, never while t.mu is held.
	reportMu    sync.Mutex
	reported    bool
	lastHealthy int
	lastTotal   int
}

// NewTracker returns a Tracker with no Endpoints.
func NewTracker(cfg Config) *Tracker {
	t := &Tracker{upstream: cfg.Upstream, clk: cfg.Clock, onHealthy: cfg.OnHealthy}
	if t.clk == nil {
		t.clk = clock.Real()
	}
	t.base = t.clk.Now()
	t.tab.Store(&table{})
	t.SetPolicy(cfg.Passive, cfg.Active, cfg.MaxConnections)
	if cfg.Metrics != nil {
		t.SetMetrics(cfg.Metrics)
	}
	return t
}

// Upstream returns the Upstream's name.
func (t *Tracker) Upstream() string { return t.upstream }

// at converts a reading of the Tracker's clock to nanoseconds since its
// base. Between two clock.Real readings the difference is monotonic, so
// deadlines kept in this form survive wall-clock steps (an NTP step, a VM
// resume, a manual date change). Readings before the base count as the
// base.
func (t *Tracker) at(now time.Time) int64 { return max(int64(now.Sub(t.base)), 0) }

// elapsed returns the current instant: the clock's elapsed time since the
// base, which is what the Tracker's timers run on.
func (t *Tracker) elapsed() int64 { return max(int64(t.clk.Since(t.base)), 0) }

// SetPolicy installs a newly activated snapshot's thresholds (05 req 2:
// they apply from the new snapshot on). Removing the active check marks
// every Endpoint healthy again.
func (t *Tracker) SetPolicy(p PassivePolicy, a *ActivePolicy, maxConnections int) {
	pp := p.WithDefaults()
	t.passive.Store(&pp)
	if maxConnections <= 0 {
		maxConnections = DefaultMaxConnections
	}
	t.maxConns.Store(int64(maxConnections))
	if a != nil {
		ap := a.WithDefaults()
		t.active.Store(&ap)
		t.poke()
		return
	}
	if t.active.Swap(nil) == nil {
		return
	}
	t.mu.Lock()
	for _, e := range t.tab.Load().eps {
		e.unhealthy.Store(false)
		e.probeOK, e.probeFail = 0, 0
	}
	changed := t.recountLocked(t.elapsed())
	t.mu.Unlock()
	t.report(changed)
	t.poke()
}

// Passive returns the current passive policy.
func (t *Tracker) Passive() PassivePolicy { return *t.passive.Load() }

// Active returns the current active policy, nil when probing is off.
func (t *Tracker) Active() *ActivePolicy { return t.active.Load() }

// SetMetrics switches to a newly activated snapshot's Upstream handles and
// sets their healthy-Endpoint gauge to the current value.
func (t *Tracker) SetMetrics(m *emit.UpstreamMetrics) {
	t.mu.Lock()
	t.metrics.Store(m)
	changed := t.recountLocked(t.elapsed())
	t.mu.Unlock()
	t.report(changed)
}

// SetEndpoints replaces the Endpoint list with ids, which must be sorted
// with unique entries, like the discovery.Set it comes from. State is kept
// for identities that remain (05 req 2); new Endpoints start healthy and
// unejected (05 req 20). Indexes of Views and Begin follow the new list.
func (t *Tracker) SetEndpoints(ids []string) error {
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			return fmt.Errorf("%w: %q at %d follows %q", ErrNotNormalized, ids[i], i, ids[i-1])
		}
	}
	t.mu.Lock()
	old := t.tab.Load().eps
	eps := make([]*endpoint, len(ids))
	j := 0
	for i, id := range ids {
		for j < len(old) && old[j].identity < id {
			old[j].removed.Store(true)
			j++
		}
		if j < len(old) && old[j].identity == id {
			eps[i] = old[j]
			j++
			continue
		}
		eps[i] = &endpoint{identity: id}
	}
	for ; j < len(old); j++ {
		old[j].removed.Store(true)
	}
	t.tab.Store(&table{eps: eps})
	changed := t.recountLocked(t.elapsed())
	t.mu.Unlock()
	t.report(changed)
	t.poke()
	return nil
}

// Len returns the current Endpoint count.
func (t *Tracker) Len() int { return len(t.tab.Load().eps) }

// Index returns the position of identity in the current list.
func (t *Tracker) Index(identity string) (int, bool) {
	eps := t.tab.Load().eps
	lo, hi := 0, len(eps)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if eps[m].identity < identity {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo, lo < len(eps) && eps[lo].identity == identity
}

// Healthy returns the Endpoints neither passively ejected nor actively
// unhealthy at now, and the Endpoint count.
func (t *Tracker) Healthy(now time.Time) (healthy, total int) {
	n := t.at(now)
	eps := t.tab.Load().eps
	for _, e := range eps {
		if !e.down(n) {
			healthy++
		}
	}
	return healthy, len(eps)
}

// Close stops the ejection expiry timer and detaches the Tracker from its
// Prober; a closed Tracker ejects nothing more. The Upstream layer closes
// it when no snapshot references the Upstream any more.
func (t *Tracker) Close() {
	t.mu.Lock()
	t.closed = true
	if t.expiry != nil {
		t.expiry.Stop()
	}
	t.mu.Unlock()
	t.poke()
}

func (t *Tracker) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// poke tells the attached Prober that Endpoints or the active policy
// changed.
func (t *Tracker) poke() {
	if f := t.notify.Load(); f != nil {
		(*f)()
	}
}

// recountLocked recomputes the healthy count, sets the healthy-Endpoint
// gauge (05 req 23) and arms the expiry timer for the earliest ejection
// end; t.mu is held. It reports whether the count or total changed.
func (t *Tracker) recountLocked(now int64) (changed bool) {
	eps := t.tab.Load().eps
	var next int64
	healthy := 0
	for _, e := range eps {
		if u := e.ejectedUntil.Load(); u > now {
			if next == 0 || u < next {
				next = u
			}
			continue
		}
		if !e.unhealthy.Load() {
			healthy++
		}
	}
	total := len(eps)
	// The gauge counts the Endpoints eligible after step (b) of 05 req 11:
	// with none healthy, panic mode makes every Endpoint eligible.
	eligible := healthy
	if eligible == 0 {
		eligible = total
	}
	if m := t.metrics.Load(); m != nil && m.HealthyEndpoints != nil {
		m.HealthyEndpoints.Set(int64(eligible))
	}
	t.armLocked(now, next)
	changed = healthy != t.healthy || total != t.total
	t.healthy, t.total = healthy, total
	return changed
}

// armLocked arms the expiry timer for at (0: none); t.mu is held.
func (t *Tracker) armLocked(now, at int64) {
	if t.closed || at == t.expiryAt {
		return
	}
	t.expiryAt = at
	if at == 0 {
		if t.expiry != nil {
			t.expiry.Stop()
		}
		return
	}
	d := time.Duration(at - now)
	if t.expiry == nil {
		t.expiry = t.clk.AfterFunc(d, t.onExpiry)
		return
	}
	t.expiry.Reset(d)
}

// onExpiry runs when the earliest ejection ends.
func (t *Tracker) onExpiry() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.expiryAt = -1 // force re-arming for the next ejection end
	changed := t.recountLocked(t.elapsed())
	t.mu.Unlock()
	t.report(changed)
}

// report calls OnHealthy after a change; the caller holds no Tracker
// lock. Deliveries are serialized by reportMu and read the count under
// t.mu, so when changes on different goroutines report out of order the
// last delivery still carries the latest state (the Upstream layer's
// upstream_panic reason never sticks); a repeated value is not delivered
// again.
func (t *Tracker) report(changed bool) {
	if !changed || t.onHealthy == nil {
		return
	}
	t.reportMu.Lock()
	defer t.reportMu.Unlock()
	t.mu.Lock()
	h, n := t.healthy, t.total
	t.mu.Unlock()
	if t.reported && h == t.lastHealthy && n == t.lastTotal {
		return
	}
	t.reported, t.lastHealthy, t.lastTotal = true, h, n
	t.onHealthy(h, n)
}

// View is the health state one attempt consults, indexed like the Endpoint
// list of the last SetEndpoints before it was taken. It is a small value;
// taking and reading it allocates nothing.
type View struct {
	t         *Tracker
	tab       *table
	now       int64
	stallSkip bool
}

// View returns the state at now for one attempt's Endpoint selection.
func (t *Tracker) View(now time.Time) View {
	return View{
		t:         t,
		tab:       t.tab.Load(),
		now:       t.at(now),
		stallSkip: t.stalled.Load()*100 >= t.maxConns.Load()*StalledSharePercent,
	}
}

// Len returns the Endpoint count of the View.
func (v View) Len() int { return len(v.tab.eps) }

func (v View) ep(i int) *endpoint {
	if i < 0 || i >= len(v.tab.eps) {
		return nil
	}
	return v.tab.eps[i]
}

// Identity returns Endpoint i's identity ("" out of range).
func (v View) Identity(i int) string {
	if e := v.ep(i); e != nil {
		return e.identity
	}
	return ""
}

// Down reports 05 req 11 step (b) for Endpoint i: passively ejected or
// actively unhealthy.
func (v View) Down(i int) bool {
	e := v.ep(i)
	return e != nil && e.down(v.now)
}

// Suspect reports whether Endpoint i is suspect (05 req 21).
func (v View) Suspect(i int) bool {
	e := v.ep(i)
	return e != nil && e.suspect(v.now)
}

// Avoid reports the health part of 05 req 11 step (c) for Endpoint i:
// suspect, or holding a stalled attempt while stalled attempts hold
// StalledSharePercent of maxConnections (05 req 21). Selection skips such
// an Endpoint while another remains.
func (v View) Avoid(i int) bool {
	e := v.ep(i)
	if e == nil {
		return false
	}
	return e.suspect(v.now) || v.stallSkip && e.stalled.Load() > 0
}

// Failures returns Endpoint i's attempts matching failureWhen in the last
// FailureWindow, the second term of its least-request load (05 req 13).
func (v View) Failures(i int) int64 {
	e := v.ep(i)
	if e == nil {
		return 0
	}
	return e.failures.sum(v.now)
}

// Begin starts tracking an attempt to Endpoint i of this View at now (see
// Attempt). It reports false, and tracks nothing, when i is out of range.
func (v View) Begin(a *Attempt, i int, now time.Time) bool {
	e := v.ep(i)
	if e == nil {
		return false
	}
	a.begin(v.t, e, now)
	return true
}

// EndpointStatus is one Endpoint's health for /debug/upstreams (05 req 96).
type EndpointStatus struct {
	Identity string
	// Healthy is the active-check verdict (true without active checks).
	Healthy bool
	// Ejected reports a passive ejection in force.
	Ejected bool
	// EjectedUntil is when the ejection ends (zero when not ejected).
	EjectedUntil time.Time
	// Ejections is the current (decayed) ejection count.
	Ejections int
	// ConsecutiveErrors is the current run of failures.
	ConsecutiveErrors int
	// Suspect reports 05 req 21.
	Suspect bool
	// Stalled counts the Endpoint's stalled attempts.
	Stalled int
	// Failures1s counts attempts matching failureWhen in the last second.
	Failures1s int64
}

// Status returns every Endpoint's health at now, in list order.
func (t *Tracker) Status(now time.Time) []EndpointStatus {
	n := t.at(now)
	pol := t.Passive()
	t.mu.Lock()
	defer t.mu.Unlock()
	eps := t.tab.Load().eps
	out := make([]EndpointStatus, len(eps))
	for i, e := range eps {
		s := EndpointStatus{
			Identity:          e.identity,
			Healthy:           !e.unhealthy.Load(),
			Ejections:         decayed(e, n, pol.EjectionTime),
			ConsecutiveErrors: int(e.consecutive.Load()),
			Suspect:           e.suspect(n),
			Stalled:           int(e.stalled.Load()),
			Failures1s:        e.failures.sum(n),
		}
		if u := e.ejectedUntil.Load(); u > n {
			s.Ejected = true
			s.EjectedUntil = t.base.Add(time.Duration(u)).UTC()
		}
		out[i] = s
	}
	return out
}
