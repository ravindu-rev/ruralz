// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"cmp"
	"container/heap"
	"context"
	"errors"
	"math"
	"math/bits"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// ProbesSkippedSource is the source under which the Prober holds the
// probes_skipped Node degraded reason.
const ProbesSkippedSource = "upstream_probes"

// ErrProbeTimeout is the cancel cause of a probe's context at its timeout.
var ErrProbeTimeout = errors.New("health: probe timed out")

// Probe names one active check.
type Probe struct {
	// Upstream is the Upstream's metadata.name.
	Upstream string
	// Endpoint is the Endpoint identity; the Upstream layer maps it to the
	// Endpoint's cached addresses and Host.
	Endpoint string
	// Path is healthCheck.active.path.
	Path string
}

// ProbeFunc sends one active check: GET p.Path to the Endpoint with the
// Upstream's TLS settings, dialed through the egress guard like any Node
// connection (05 req 9), taking no bulkhead slot (05 req 20). It returns
// the response status or an error; ctx ends at the probe timeout (cause
// ErrProbeTimeout) or when the Prober stops. The Upstream layer provides
// it per Upstream.
type ProbeFunc func(ctx context.Context, p Probe) (status int, err error)

// ProberConfig configures a Prober.
type ProberConfig struct {
	// Clock schedules probes and bounds them; nil means clock.Real(). The
	// schedule runs on the Clock's elapsed time (Since), so a wall-clock
	// step neither halts probing nor bursts every probe at once. Attached
	// Trackers must share it.
	Clock clock.Clock
	// Rand draws jitter on Run's goroutine; nil means the math/rand/v2
	// global source.
	Rand rand.Source
	// Status receives the probes_skipped Node degraded reason; nil
	// disables it.
	Status emit.NodeStatus
	// MaxSlots caps the probe slots; 0 means MaxProbeSlots.
	MaxSlots int
}

// Prober runs the active checks of every attached Upstream on the Node
// (05 req 20). Run is its one scheduling goroutine; each probe runs on a
// goroutine Run starts, bounded by the probe slots and joined before Run
// returns.
type Prober struct {
	clk      clock.Clock
	base     time.Time // clk reading at creation: instant 0 of the schedule
	rnd      rand.Source
	status   emit.NodeStatus
	maxSlots int
	wake     chan struct{}
	running  atomic.Bool

	mu   sync.Mutex // guards atts
	atts map[*Tracker]*attachment

	limit  atomic.Int64
	inUse  atomic.Int64
	probes atomic.Uint64 // probes started
	skips  atomic.Uint64 // probes skipped for lack of a slot

	degraded atomic.Bool
	// waits counts the times Run blocked waiting for work (tests use it to
	// know when Run is idle).
	waits atomic.Uint64

	// Run's goroutine only.
	win skipWindow
}

// attachment is one Upstream's Tracker with its probe function.
type attachment struct {
	t        *Tracker
	probe    ProbeFunc
	detached atomic.Bool

	// Run's goroutine only.
	scheduled  map[*endpoint]bool
	seenTab    *table
	seenActive *ActivePolicy
	load       float64 // probes per second × timeout
}

// NewProber returns a Prober with nothing attached.
func NewProber(cfg ProberConfig) *Prober {
	p := &Prober{
		clk:      cfg.Clock,
		rnd:      cfg.Rand,
		status:   cfg.Status,
		maxSlots: cfg.MaxSlots,
		wake:     make(chan struct{}, 1),
		atts:     map[*Tracker]*attachment{},
	}
	if p.clk == nil {
		p.clk = clock.Real()
	}
	p.base = p.clk.Now()
	if p.rnd == nil {
		p.rnd = globalSource{}
	}
	if p.maxSlots <= 0 {
		p.maxSlots = MaxProbeSlots
	}
	return p
}

// Attach makes the Prober check t's Endpoints with probe while t has an
// active policy; it follows t's Endpoint and policy changes. Attaching a
// Tracker again replaces its probe function.
func (p *Prober) Attach(t *Tracker, probe ProbeFunc) {
	a := &attachment{t: t, probe: probe, scheduled: map[*endpoint]bool{}}
	p.mu.Lock()
	if old := p.atts[t]; old != nil {
		old.detached.Store(true)
	}
	p.atts[t] = a
	p.mu.Unlock()
	poke := p.poke
	t.notify.Store(&poke)
	p.poke()
}

// Detach stops checking t's Endpoints; probes in flight finish without a
// verdict once t is closed.
func (p *Prober) Detach(t *Tracker) {
	p.mu.Lock()
	if a := p.atts[t]; a != nil {
		a.detached.Store(true)
		delete(p.atts, t)
	}
	p.mu.Unlock()
	t.notify.Store(nil)
	p.poke()
}

// Slots returns the current probe slot count: min(MaxSlots, ceil(Σ
// probes per second × timeout)) over the attached Upstreams with an active
// policy, at least 1 while any probes (05 req 20). Run recomputes it.
func (p *Prober) Slots() int { return int(p.limit.Load()) }

// Stats returns the probes started and skipped since the Prober was made.
func (p *Prober) Stats() (started, skipped uint64) { return p.probes.Load(), p.skips.Load() }

// elapsed returns the schedule's current instant: the clock's elapsed
// time since the Prober was made, in nanoseconds.
func (p *Prober) elapsed() int64 { return max(int64(p.clk.Since(p.base)), 0) }

// poke wakes Run to pick up attachment, Endpoint and policy changes.
func (p *Prober) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run schedules and launches probes until ctx ends, then waits for the
// probes in flight and clears probes_skipped. Only one Run may be active;
// a second call returns at once. A later Run starts a fresh schedule for
// every attached Upstream.
func (p *Prober) Run(ctx context.Context) {
	if !p.running.CompareAndSwap(false, true) {
		return
	}
	defer p.running.Store(false)
	defer p.setDegraded(false)
	p.resetSchedule()
	var wg sync.WaitGroup
	defer wg.Wait()
	var h entryHeap
	var timer clock.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		now := p.elapsed()
		p.sync(now, &h)
		p.launch(ctx, now, &h, &wg)
		p.evaluate(now)
		next, armed := int64(0), len(h) > 0
		if armed {
			next = h[0].due
		}
		if p.degraded.Load() && (!armed || next > now+int64(time.Second)) {
			// Re-evaluate the window as it slides, so the reason clears.
			next, armed = now+int64(time.Second), true
		}
		var due <-chan time.Time
		if armed {
			d := time.Duration(next - now)
			if timer == nil {
				timer = p.clk.NewTimer(d)
			} else {
				timer.Reset(d)
			}
			due = timer.C()
		}
		p.waits.Add(1)
		select {
		case <-ctx.Done():
			return
		case <-due:
		case <-p.wake:
		}
	}
}

// resetSchedule forgets what an earlier Run scheduled, so this Run
// schedules every Endpoint again; only the active Run touches these
// fields.
func (p *Prober) resetSchedule() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.atts {
		clear(a.scheduled)
		a.seenTab, a.seenActive, a.load = nil, nil, 0
	}
	p.win = skipWindow{}
}

// sync schedules the Endpoints of new or changed attachments, drops closed
// Trackers and recomputes the probe slots.
func (p *Prober) sync(now int64, h *entryHeap) {
	p.mu.Lock()
	atts := make([]*attachment, 0, len(p.atts))
	for _, a := range p.atts {
		atts = append(atts, a)
	}
	p.mu.Unlock()
	slices.SortFunc(atts, func(x, y *attachment) int { return cmp.Compare(x.t.upstream, y.t.upstream) })
	var sum float64
	for _, a := range atts {
		if a.t.isClosed() {
			p.Detach(a.t)
			continue
		}
		act, tab := a.t.active.Load(), a.t.tab.Load()
		if act != a.seenActive || tab != a.seenTab {
			a.seenActive, a.seenTab, a.load = act, tab, 0
			if act != nil {
				a.load = float64(len(tab.eps)) * act.Timeout.Seconds() / act.Interval.Seconds()
				for _, e := range tab.eps {
					if !a.scheduled[e] {
						// The first probe of a new Endpoint falls anywhere in
						// its first interval, so probes spread evenly.
						a.scheduled[e] = true
						heap.Push(h, entry{due: now + int64(uniform(p.rnd, act.Interval)), att: a, ep: e})
					}
				}
			}
		}
		sum += a.load
	}
	var limit int64
	if sum > 0 {
		limit = int64(min(float64(p.maxSlots), max(1, math.Ceil(sum))))
	}
	p.limit.Store(limit)
}

// launch starts every probe due by now that finds a slot, skips the others
// and reschedules each Endpoint interval ±10% later.
func (p *Prober) launch(ctx context.Context, now int64, h *entryHeap, wg *sync.WaitGroup) {
	for len(*h) > 0 && (*h)[0].due <= now {
		en := heap.Pop(h).(entry) //nolint:forcetypeassert // entryHeap holds entries only.
		a, e := en.att, en.ep
		act := a.t.active.Load()
		if a.detached.Load() || act == nil || e.removed.Load() {
			delete(a.scheduled, e)
			continue
		}
		heap.Push(h, entry{due: now + int64(jitter(p.rnd, act.Interval)), att: a, ep: e})
		// An Endpoint whose previous probe is still running is not probed
		// twice; that is not a slot shortage.
		if !e.probing.CompareAndSwap(false, true) {
			continue
		}
		if !p.acquire() {
			e.probing.Store(false)
			p.win.add(now, true)
			p.skips.Add(1)
			if m := a.t.metrics.Load(); m != nil && m.ProbesSkipped != nil {
				m.ProbesSkipped.Add(0, 1)
			}
			continue
		}
		p.win.add(now, false)
		p.probes.Add(1)
		pol := *act
		wg.Go(func() {
			defer func() {
				e.probing.Store(false)
				p.inUse.Add(-1)
			}()
			p.probeOnce(ctx, a, e, pol)
		})
	}
}

// acquire takes a probe slot if one is free.
func (p *Prober) acquire() bool {
	for {
		n := p.inUse.Load()
		if n >= p.limit.Load() {
			return false
		}
		if p.inUse.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// probeOnce runs one probe bounded by the policy timeout on the Prober's
// clock and records its verdict: a status from 200 to 399 within the
// timeout succeeds (05 req 20). A probe cut short by the Prober stopping
// records nothing.
func (p *Prober) probeOnce(ctx context.Context, a *attachment, e *endpoint, act ActivePolicy) {
	pctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	tm := p.clk.AfterFunc(act.Timeout, func() { cancel(ErrProbeTimeout) })
	status, err := a.probe(pctx, Probe{Upstream: a.t.upstream, Endpoint: e.identity, Path: act.Path})
	timedOut := !tm.Stop()
	if ctx.Err() != nil {
		return
	}
	ok := err == nil && !timedOut && status >= 200 && status <= 399
	a.t.recordProbe(e, ok, a.t.elapsed())
}

// evaluate raises or clears probes_skipped: more than ProbesSkippedPercent
// of the probes due over the last ProbesSkippedWindow were skipped (05 req
// 20).
func (p *Prober) evaluate(now int64) {
	skipped, total := p.win.sums(now)
	p.setDegraded(total > 0 && skipped*100 > total*ProbesSkippedPercent)
}

func (p *Prober) setDegraded(on bool) {
	if p.degraded.Swap(on) == on {
		return
	}
	if p.status != nil {
		p.status.SetDegraded(catalog.ReasonProbesSkipped, ProbesSkippedSource, on)
	}
}

// Degraded reports whether the Prober holds probes_skipped.
func (p *Prober) Degraded() bool { return p.degraded.Load() }

// jitter returns d ± ProbeJitterPercent, uniformly (05 req 20).
func jitter(src rand.Source, d time.Duration) time.Duration {
	spread := d * ProbeJitterPercent / 100
	return d - spread + uniform(src, 2*spread)
}

// uniform returns a duration uniformly drawn from [0, d].
func uniform(src rand.Source, d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	hi, _ := bits.Mul64(src.Uint64(), uint64(d)+1)
	return time.Duration(hi) //nolint:gosec // G115: hi ≤ d, a positive Duration.
}

// entry is one scheduled probe; due is an instant of the Prober
// (Prober.elapsed).
type entry struct {
	due int64
	att *attachment
	ep  *endpoint
}

// entryHeap is a min-heap of entries by due time.
type entryHeap []entry

func (h entryHeap) Len() int           { return len(h) }
func (h entryHeap) Less(i, j int) bool { return h[i].due < h[j].due }
func (h entryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

// Push implements heap.Interface.
func (h *entryHeap) Push(x any) { *h = append(*h, x.(entry)) } //nolint:forcetypeassert // only entries are pushed.

// Pop implements heap.Interface.
func (h *entryHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = entry{}
	*h = old[:n-1]
	return x
}

// skipWindow counts due probes and skipped ones per second over the last
// ProbesSkippedWindow.
type skipWindow struct {
	sec     [skipBuckets]int64
	total   [skipBuckets]int64
	skipped [skipBuckets]int64
}

const skipBuckets = int64(ProbesSkippedWindow / time.Second)

func (w *skipWindow) add(now int64, skipped bool) {
	s := now / int64(time.Second)
	i := ((s % skipBuckets) + skipBuckets) % skipBuckets
	if w.sec[i] != s {
		w.sec[i], w.total[i], w.skipped[i] = s, 0, 0
	}
	w.total[i]++
	if skipped {
		w.skipped[i]++
	}
}

func (w *skipWindow) sums(now int64) (skipped, total int64) {
	s := now / int64(time.Second)
	for i := range w.sec {
		if age := s - w.sec[i]; age >= 0 && age < skipBuckets && w.total[i] > 0 {
			skipped += w.skipped[i]
			total += w.total[i]
		}
	}
	return skipped, total
}

// globalSource draws from the math/rand/v2 global generator, which is safe
// for concurrent use.
type globalSource struct{}

// Uint64 implements rand.Source.
func (globalSource) Uint64() uint64 {
	return rand.Uint64() //nolint:gosec // G404: probe jitter needs a uniform draw, not an unpredictable one.
}
