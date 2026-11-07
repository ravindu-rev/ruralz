// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// poolKey identifies a pool: Policies with equal digest, configured state
// (the rz_configure input, so a rotated secret configures a new pool) and
// limits share one.
type poolKey struct {
	digest  revision.Digest
	config  [32]byte
	pages   uint32
	timeout time.Duration
}

// pool is one configuration's instances on this Node
// (docs/architecture/05-wasm-plugin-system.md "Runtime" and "Default
// limits"): at most N instances, each reserving its whole memory limit
// from the Node cap; at most min(2 × GOMAXPROCS, N) computing; in a
// state.* pool a parked bound of N − R while guests wait on the State
// Store; idle kept at least max(1, 25% of busy) by the pool manager.
type pool struct {
	e   *Engine
	key poolKey
	d   *digestEntry
	rt  *runtimeEntry

	pages    uint32
	memBytes int64
	timeout  time.Duration
	phases   phase.Set
	// state marks a pool whose Plugin holds state.read or state.write.
	state bool
	n     int

	// config is the rz_configure input and keys the host-held keys its
	// handles name; configBinding is rz_configure's binding.
	config []byte
	keys   []*hostKey
	// secrets are the resolved secret values guest logs redact.
	secrets       [][]byte
	configBinding *binding

	// idle holds the idle instances; compute the free compute slots.
	idle    chan *instance
	compute chan struct{}

	mu sync.Mutex
	// total counts the live instances, busy those serving a call, parked
	// those waiting on the State Store.
	total, busy, parked int
	// computing is busy minus parked.
	computing int
	peaks     peakTracker
	// refs counts the Filters holding the pool; closed is set when the
	// last one released it.
	refs   int
	closed bool
	// capRefused marks a fill the Node cap or the share refused, so a
	// failed acquire is RZ-PLG-004; configFailed a background
	// instantiation or rz_configure failure (RZ-PLG-008).
	capRefused   bool
	configFailed bool
	// refill is the fault refill budget: refillPerSecond per second.
	refill     tokenBucket
	refillWait clock.Timer

	breaker *faultBreaker
	queued  atomic.Bool
	// observers are the Policies' telemetry of the pool.
	observers []Telemetry
}

// tokenBucket is a refill budget on the Node clock.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

// take takes one token at now, refilling refillPerSecond per second up to
// refillPerSecond; false when none is left, with the wait for one.
func (t *tokenBucket) take(now time.Time) (bool, time.Duration) {
	if t.last.IsZero() {
		t.tokens, t.last = refillPerSecond, now
	}
	if d := now.Sub(t.last); d > 0 {
		t.tokens = min(refillPerSecond, t.tokens+d.Seconds()*refillPerSecond)
		t.last = now
	}
	if t.tokens >= 1 {
		t.tokens--
		return true, 0
	}
	return false, time.Duration((1 - t.tokens) / refillPerSecond * float64(time.Second))
}

// peakTracker keeps per-second maxima of a count over the last minute.
type peakTracker struct {
	computing, busy [60]int
	second          int64
}

// note records the current counts at now.
func (t *peakTracker) note(now time.Time, computing, busy int) {
	s := now.Unix()
	if s != t.second {
		n := int64(len(t.busy))
		if s-t.second >= n || s < t.second {
			clear(t.computing[:])
			clear(t.busy[:])
		} else {
			for x := t.second + 1; x <= s; x++ {
				t.computing[x%n], t.busy[x%n] = 0, 0
			}
		}
		t.second = s
	}
	i := s % int64(len(t.busy))
	t.computing[i] = max(t.computing[i], computing)
	t.busy[i] = max(t.busy[i], busy)
}

// peak returns the 1-minute peaks.
func (t *peakTracker) peak() (computing, busy int) {
	for i := range t.busy {
		computing = max(computing, t.computing[i])
		busy = max(busy, t.busy[i])
	}
	return computing, busy
}

// bounds returns the pool's current bounds: R follows the 1-minute peak
// computing count.
func (p *pool) bounds() poolBounds {
	peak, _ := p.peaks.peak()
	return boundsOf(p.n, p.e.procs, peak, p.state)
}

// errAcquire are the acquire failures, carrying their codes.
var (
	errNoInstance  = errors.New("no idle instance within the wait")
	errCapRefused  = errors.New("no idle instance within the wait, and the pool manager was refused at the Node cap or the pool share")
	errBreakerOpen = errors.New("the pool's fault breaker is open")
	errPoolClosed  = errors.New("the pool is closed")
)

// acquire takes a compute slot and an idle instance, waiting at most 1 ms
// in all (RZ-PLG-005, RZ-PLG-004 after a refused fill).
func (p *pool) acquire(ctx context.Context) (*instance, error) {
	var timer clock.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	expired := func() <-chan time.Time {
		if timer == nil {
			timer = p.e.clk.NewTimer(poolWait)
		}
		return timer.C()
	}
	select {
	case <-p.compute:
	default:
		p.wake()
		select {
		case <-p.compute:
		case <-expired():
			return nil, p.acquireError()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var inst *instance
	select {
	case inst = <-p.idle:
	default:
		p.wake()
		select {
		case inst = <-p.idle:
		case <-expired():
			p.compute <- struct{}{}
			return nil, p.acquireError()
		case <-ctx.Done():
			p.compute <- struct{}{}
			return nil, ctx.Err()
		}
	}
	p.mu.Lock()
	p.busy++
	p.computing++
	p.peaks.note(p.e.clk.Now(), p.computing, p.busy)
	idle := len(p.idle)
	busy := p.busy
	p.mu.Unlock()
	p.observe(idle, busy)
	p.wake()
	return inst, nil
}

// acquireError returns the acquire failure: RZ-PLG-004 when the last fill
// was refused at the Node cap or the share, else RZ-PLG-005.
func (p *pool) acquireError() error {
	p.mu.Lock()
	refused := p.capRefused
	closed := p.closed
	p.mu.Unlock()
	switch {
	case closed:
		return errPoolClosed
	case refused:
		return errCapRefused
	default:
		return errNoInstance
	}
}

// release returns a healthy instance after its call: it is recycled after
// recycleAfter calls.
func (p *pool) release(ctx context.Context, inst *instance) {
	inst.calls++
	p.mu.Lock()
	p.busy--
	p.computing--
	closed := p.closed
	p.mu.Unlock()
	p.compute <- struct{}{}
	if closed || inst.calls >= recycleAfter {
		p.destroy(ctx, inst)
		p.wake()
		return
	}
	p.idle <- inst
	p.observe(len(p.idle), p.busyCount())
}

// discard destroys an instance after a fault or a call the request
// ended, and schedules its refill, rate-limited after a fault.
func (p *pool) discard(ctx context.Context, inst *instance, fault bool) {
	p.mu.Lock()
	p.busy--
	p.computing--
	p.mu.Unlock()
	p.compute <- struct{}{}
	p.destroy(ctx, inst)
	if fault {
		p.mu.Lock()
		ok, wait := p.refill.take(p.e.clk.Now())
		if !ok && p.refillWait == nil && !p.closed {
			p.refillWait = p.e.clk.AfterFunc(wait, func() {
				p.mu.Lock()
				p.refillWait = nil
				p.mu.Unlock()
				p.wake()
			})
		}
		p.mu.Unlock()
		if !ok {
			return
		}
	}
	p.wake()
}

// destroy closes an instance and returns its reservation.
func (p *pool) destroy(ctx context.Context, inst *instance) {
	inst.close(ctx)
	p.mu.Lock()
	p.total--
	p.mu.Unlock()
	p.e.unreserve(p.memBytes)
	p.observe(len(p.idle), p.busyCount())
}

// busyCount returns busy.
func (p *pool) busyCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.busy
}

// park moves a computing call to the parked set within the parked bound
// N − R, freeing its compute slot; false when the bound is reached.
func (p *pool) park() bool {
	p.mu.Lock()
	b := p.bounds()
	if p.parked >= b.parked {
		p.mu.Unlock()
		return false
	}
	p.parked++
	p.computing--
	p.mu.Unlock()
	p.compute <- struct{}{}
	return true
}

// unpark returns a parked call to computing, waiting for a compute slot.
func (p *pool) unpark() {
	<-p.compute
	p.mu.Lock()
	p.parked--
	p.computing++
	p.peaks.note(p.e.clk.Now(), p.computing, p.busy)
	p.mu.Unlock()
}

// wake queues the pool for the pool manager, once.
func (p *pool) wake() {
	if p.queued.CompareAndSwap(false, true) {
		select {
		case p.e.work <- p:
		default:
			p.queued.Store(false)
		}
	}
}

// idleTarget returns the instances the pool keeps idle: max(1, 25% of
// busy).
func idleTarget(busy int) int { return max(1, ceilDiv(busy*idlePercent, 100)) }

// fill creates instances until idle reaches its target, within N and the
// Node cap; a fault refill takes the refill budget, and a failure marks
// the pool degraded (RZ-PLG-008).
func (p *pool) fill(ctx context.Context) {
	p.queued.Store(false)
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		want := min(p.busy+idleTarget(p.busy), p.n)
		if p.total >= want {
			p.mu.Unlock()
			return
		}
		p.total++
		p.mu.Unlock()
		if !p.e.reserve(p.memBytes, false) {
			p.mu.Lock()
			p.total--
			p.capRefused = true
			p.mu.Unlock()
			return
		}
		inst, err := p.newInstance(ctx)
		if err != nil {
			p.e.unreserve(p.memBytes)
			p.mu.Lock()
			p.total--
			p.configFailed = true
			p.mu.Unlock()
			p.failed(ctx, err)
			return
		}
		p.mu.Lock()
		p.capRefused = false
		wasFailed := p.configFailed
		p.configFailed = false
		closed := p.closed
		p.mu.Unlock()
		if wasFailed {
			p.e.poolHealthChanged()
		}
		if closed {
			p.destroy(ctx, inst)
			return
		}
		p.idle <- inst
		p.observe(len(p.idle), p.busyCount())
	}
}

// failed records a background instantiation or rz_configure failure:
// RZ-PLG-008 counted on every Policy of the pool, logged, and the pool
// degraded until an instance succeeds.
func (p *pool) failed(ctx context.Context, err error) {
	for _, t := range p.observersCopy() {
		t.Failure(CodeBackground)
	}
	p.e.log.LogAttrs(ctx, slog.LevelWarn, "plugin instantiation failed",
		slog.String(catalog.KeyPolicy, p.configBinding.policy), slog.String(catalog.KeyCode, CodeBackground),
		slog.String(catalog.KeyError, err.Error()))
	p.e.poolHealthChanged()
}

// observe reports the instance counts to every Policy of the pool.
func (p *pool) observe(idle, busy int) {
	for _, t := range p.observersCopy() {
		t.Instances(idle, busy)
	}
}

// observersCopy returns the observers.
func (p *pool) observersCopy() []Telemetry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Telemetry(nil), p.observers...)
}

// degraded reports a pool whose fault breaker is open or whose last
// background instantiation failed (plugin_pool_degraded).
func (p *pool) degraded() bool {
	p.mu.Lock()
	failed := p.configFailed
	p.mu.Unlock()
	return failed || p.breaker.open()
}

// warm creates count instances now, before the snapshot publishes,
// reserving within the swap headroom; it stops at the first instance the
// cap refuses. A failure is returned as is: a config rejection
// deterministic, a timeout transient.
func (p *pool) warm(ctx context.Context, count int) (int, error) {
	made := 0
	for made < count {
		p.mu.Lock()
		if p.total >= p.n {
			p.mu.Unlock()
			break
		}
		p.total++
		p.mu.Unlock()
		if !p.e.reserve(p.memBytes, true) {
			p.mu.Lock()
			p.total--
			p.mu.Unlock()
			break
		}
		inst, err := p.newInstance(ctx)
		if err != nil {
			p.e.unreserve(p.memBytes)
			p.mu.Lock()
			p.total--
			p.mu.Unlock()
			return made, err
		}
		p.idle <- inst
		made++
	}
	p.observe(len(p.idle), p.busyCount())
	return made, nil
}

// trim closes idle instances down to max(1, busy): a pool a swap replaced
// gives memory back to its successor while it serves pinned requests.
func (p *pool) trim(ctx context.Context) {
	for {
		p.mu.Lock()
		keep := max(1, p.busy)
		if p.total <= keep {
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
		select {
		case inst := <-p.idle:
			p.destroy(ctx, inst)
		default:
			return
		}
	}
}

// closeInstances closes the pool: its idle instances now, a busy one when
// its call returns.
func (p *pool) closeInstances(ctx context.Context) {
	p.mu.Lock()
	p.closed = true
	if p.refillWait != nil {
		p.refillWait.Stop()
		p.refillWait = nil
	}
	p.mu.Unlock()
	for {
		select {
		case inst := <-p.idle:
			p.destroy(ctx, inst)
		default:
			return
		}
	}
}

// callResult is the outcome of one Phase call as the Filter applies it.
type callResult struct {
	result int32
	code   string
	err    error
	// resp is the generated response of RESPOND.
	resp *filter.Response
}

// invoke runs one Phase call: the fault breaker, an instance within the
// wait, the call under its timer, the result's classification and the
// instance's release.
func (p *pool) invoke(ctx context.Context, b *binding, x filter.Exchange, ph phase.Phase, req *perRequest) callResult {
	t0 := p.e.clk.Now()
	ok, probe := p.breaker.allow()
	if !ok {
		return callResult{code: CodeBreakerOpen, err: errBreakerOpen}
	}
	inst, err := p.acquire(ctx)
	wait := p.e.clk.Since(t0)
	b.tel.Wait(wait)
	if err != nil {
		code := CodeNoInstance
		if errors.Is(err, errCapRefused) {
			code = CodeCapRefused
		}
		return callResult{code: code, err: err}
	}
	fn := inst.phases[ph]
	if fn == nil {
		p.release(ctx, inst)
		return callResult{code: CodeInvalidResult, err: fmt.Errorf("the guest exports no %s", phaseExport(ph))}
	}
	cctx, cancel := context.WithCancel(ctx)
	c := &call{e: p.e, b: b, x: x, ph: ph, req: req, reqCtx: ctx, inst: inst}
	c.timer = startTimer(p.e.clk, p.timeout, cancel)
	g0 := p.e.clk.Now()
	res, callErr := fn.Call(context.WithValue(cctx, callKey{}, c))
	g1 := p.e.clk.Now()
	c.timer.stop()
	cancel()
	var r int32
	if callErr == nil && len(res) > 0 {
		r = int32(uint32(res[0])) //nolint:gosec // G115: an i32 result is its low 32 bits.
	}
	atLimit := int64(inst.mod.Memory().Size())+pageBytes > p.memBytes
	o := c.classify(r, callErr, atLimit)
	if !o.discard {
		p.breaker.record(o.fault, probe)
	}
	switch {
	case o.fault || o.discard || inst.mod.IsClosed():
		p.discard(ctx, inst, o.fault)
	default:
		p.release(ctx, inst)
	}
	t1 := p.e.clk.Now()
	b.tel.Call(ph, t1.Sub(t0), t1.Sub(t0)-g1.Sub(g0))
	if o.code != "" {
		b.tel.Failure(o.code)
	}
	cr := callResult{result: o.result, code: o.code, err: o.err}
	if o.code == "" && o.result == resultRespond {
		cr.resp = c.response()
	}
	return cr
}
