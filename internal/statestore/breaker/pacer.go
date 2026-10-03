// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package breaker

import (
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// PacerConfig holds the reconnect pacing targets of spec 08 req 22.
type PacerConfig struct {
	// Base and Cap bound the full-jitter backoff: after n consecutive
	// connect failures the next dial waits uniform(0, min(Cap, Base×2^n))
	// (100 ms, 5 s).
	Base, Cap time.Duration
	// PerSecond is the most dial starts in any sliding second (4).
	PerSecond int
}

// DefaultPacerConfig returns the targets of spec 08 req 22.
func DefaultPacerConfig() PacerConfig {
	return PacerConfig{Base: 100 * time.Millisecond, Cap: 5 * time.Second, PerSecond: 4}
}

// Pacer gates the dials of one shard (spec 08 req 22,
// docs/operations/04-high-availability-and-disaster-recovery.md "State
// Store rebuild" step 5). A dial refused by the Pacer fails at once
// without touching the network and is not a connect failure.
type Pacer struct {
	clk clock.Clock
	cfg PacerConfig
	rnd Rand

	mu        sync.Mutex
	fails     int
	notBefore time.Time
	starts    []time.Time // ring of the last PerSecond dial starts
	next      int
}

// NewPacer returns a pacer with no failures. A nil rnd uses Uniform; zero
// PacerConfig fields take DefaultPacerConfig values.
func NewPacer(clk clock.Clock, cfg PacerConfig, rnd Rand) *Pacer {
	def := DefaultPacerConfig()
	if cfg.Base <= 0 {
		cfg.Base = def.Base
	}
	if cfg.Cap < cfg.Base {
		cfg.Cap = max(def.Cap, cfg.Base)
	}
	if cfg.PerSecond <= 0 {
		cfg.PerSecond = def.PerSecond
	}
	if rnd == nil {
		rnd = Uniform
	}
	return &Pacer{clk: clk, cfg: cfg, rnd: rnd, starts: make([]time.Time, cfg.PerSecond)}
}

// TryDial reports whether a dial may start now, and records its start:
// false while the backoff delay after the last failure runs or when
// PerSecond dials already started in the last second.
func (p *Pacer) TryDial() bool {
	now := p.clk.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Before(p.notBefore) {
		return false
	}
	if oldest := p.starts[p.next]; !oldest.IsZero() && now.Sub(oldest) < time.Second {
		return false
	}
	p.starts[p.next] = now
	p.next = (p.next + 1) % len(p.starts)
	return true
}

// Result records the outcome of a dial TryDial admitted: success resets
// the backoff, a failure (dial or TLS handshake error) delays the next
// dial by a full-jitter backoff.
func (p *Pacer) Result(ok bool) {
	now := p.clk.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if ok {
		p.fails, p.notBefore = 0, time.Time{}
		return
	}
	p.fails++
	p.notBefore = now.Add(p.rnd(0, p.ceiling(p.fails)))
}

// ceiling is min(Cap, Base×2^n) without overflow.
func (p *Pacer) ceiling(n int) time.Duration {
	c := p.cfg.Base
	for range n {
		if c >= p.cfg.Cap {
			break
		}
		c *= 2
	}
	return min(c, p.cfg.Cap)
}

// Failures returns the consecutive connect failures since the last
// success.
func (p *Pacer) Failures() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fails
}

// Wait returns how long until TryDial can succeed (0 when it can now), so
// the driver's maintenance loop can schedule background reconnects.
func (p *Pacer) Wait() time.Duration {
	now := p.clk.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	w := max(p.notBefore.Sub(now), 0)
	if oldest := p.starts[p.next]; !oldest.IsZero() {
		w = max(w, oldest.Add(time.Second).Sub(now))
	}
	return w
}
