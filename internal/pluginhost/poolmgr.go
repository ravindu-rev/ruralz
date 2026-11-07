// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// poolSpec is what one plugin Policy's Build asks of the Engine.
type poolSpec struct {
	digest   *digestEntry
	art      *artifact
	pages    uint32
	memBytes int64
	timeout  time.Duration
	state    bool
	n        int
	conf     *configured
	binding  *binding
	// previous is the pool the Policy used in the previous snapshot.
	previous *pool
	policy   string
}

// pool returns the pool of s, sharing an existing one of equal digest,
// configured state and limits; a new pool compiles the digest at its page
// limit, within the compiled-code limit, and warms before the snapshot
// publishes (docs/architecture/05-wasm-plugin-system.md "Hot-swap
// lifecycle", step 4): a replacement pool to its predecessor's 1-minute
// peak busy count plus the idle headroom, another to 1, within the share
// and the swap headroom, or to fewer when they do not fit, never a
// failure for lack of memory. Its predecessor then trims its idle
// instances to max(1, busy).
func (e *Engine) pool(ctx context.Context, s poolSpec) (*pool, error) {
	key := poolKey{digest: s.digest.digest, config: configHash(s.conf.input), pages: s.pages, timeout: s.timeout}
	e.mu.Lock()
	if p, ok := e.pools[key]; ok {
		p.mu.Lock()
		closed := p.closed
		if !closed {
			p.refs++
			p.observers = append(p.observers, s.binding.tel)
		}
		p.mu.Unlock()
		if !closed {
			e.mu.Unlock()
			return p, nil
		}
	}
	newCode := int64(0)
	if s.digest.refs == 0 {
		newCode = s.digest.accounted
	}
	e.mu.Unlock()
	if err := e.checkCode(s.digest.accounted, newCode); err != nil {
		return nil, err
	}
	rt, err := e.measureCompile(ctx, s.digest, s.pages)
	if err != nil {
		return nil, err
	}
	e.retain(s.digest)
	b := boundsOf(s.n, e.procs, 0, s.state)
	p := &pool{
		e: e, key: key, d: s.digest, rt: rt,
		pages: s.pages, memBytes: s.memBytes, timeout: s.timeout, phases: s.art.phases, state: s.state, n: s.n,
		config: s.conf.input, keys: s.conf.keys, secrets: s.conf.secrets,
		idle: make(chan *instance, s.n), compute: make(chan struct{}, b.compute),
		refs: 1, observers: []Telemetry{s.binding.tel},
	}
	for range b.compute {
		p.compute <- struct{}{}
	}
	cb := *s.binding
	cb.state = nil
	p.configBinding = &cb
	p.breaker = newFaultBreaker(e.clk, func(bool) { go e.poolHealthChanged() })

	count := warmMinimum
	if prev := s.previous; prev != nil && prev.key != key {
		_, busy := prev.peakBusy()
		count = max(warmMinimum, busy+idleTarget(busy))
	}
	if _, err := p.warm(ctx, count); err != nil {
		p.closeInstances(ctx)
		e.drop(ctx, s.digest)
		return nil, warmError(s.policy, err)
	}
	e.mu.Lock()
	e.pools[key] = p
	e.mu.Unlock()
	e.checkMemLimit()
	if s.state {
		if g := guaranteedParked(s.n); g < parkedBoundLow {
			e.parkedLow(ctx, p, s.policy, g)
		}
	}
	if prev := s.previous; prev != nil && prev != p {
		prev.trim(ctx)
	}
	return p, nil
}

// peakBusy returns the pool's 1-minute peaks.
func (p *pool) peakBusy() (computing, busy int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.peaks.note(p.e.clk.Now(), p.computing, p.busy)
	return p.peaks.peak()
}

// warmError maps a warm failure to its activation code: rz_configure
// refusing the config, or the guest trapping while it configures, is
// RZ-CFG-051; an instantiation past its limit is RZ-CFG-051 transient, as
// the online stage decides whether it reproduces; another ABI version is
// RZ-CFG-028.
func warmError(policy string, err error) error {
	switch {
	case errors.Is(err, errABIVersion):
		return fail(CodeArtifact, "Policy %q: the Plugin artifact's rz_abi_version does not return %d", policy, abiVersion)
	case errors.Is(err, errInstantiateDeadline):
		return failTransient(CodeConfigRejected, "Policy %q: %v", policy, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return fail(CodeConfigRejected, "Policy %q: rz_configure did not accept the Policy config: %s", policy, clip(err.Error()))
	}
}

// releasePool releases one Filter's hold on p: at the last hold the pool
// closes, its instances return their memory, and the digest's code goes
// with its last pool.
func (e *Engine) releasePool(ctx context.Context, p *pool, t Telemetry) {
	e.mu.Lock()
	p.mu.Lock()
	p.refs--
	for i, o := range p.observers {
		if o == t {
			p.observers = append(p.observers[:i], p.observers[i+1:]...)
			break
		}
	}
	last := p.refs == 0
	p.mu.Unlock()
	if last && e.pools[p.key] == p {
		delete(e.pools, p.key)
	}
	e.mu.Unlock()
	if !last {
		return
	}
	p.closeInstances(ctx)
	e.drop(ctx, p.d)
	e.poolHealthChanged()
	e.checkMemLimit()
	if e.status != nil {
		e.status.SetDegraded(catalog.ReasonPluginParkedBoundLow, p.parkedSource(), false)
	}
}

// parkedSource names the pool's plugin_parked_bound_low source.
func (p *pool) parkedSource() string { return fmt.Sprintf("pluginhost:%p", p) }
