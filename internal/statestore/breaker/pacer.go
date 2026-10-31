// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package breaker

import (
	"slices"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// PacerConfig holds the reconnect pacing targets.
type PacerConfig struct {
	// Base and Cap bound the full-jitter backoff: after n consecutive
	// connect failures the next dial waits uniform(0, min(Cap, Base×2^n))
	// (100 ms, 5 s).
	Base, Cap time.Duration
	// PerSecond is the most dial starts in any sliding second (4).
	PerSecond int
}

// DefaultPacerConfig returns the targets.
func DefaultPacerConfig() PacerConfig {
	return PacerConfig{Base: 100 * time.Millisecond, Cap: 5 * time.Second, PerSecond: 4}
}

// MaxPacedAddrs bounds the server addresses whose backoff a Pacer keeps:
// a sentinel deployment's primary shard moves among its few servers.
const MaxPacedAddrs = 16

// Pacer gates the dials of one shard
// (docs/operations/04-high-availability-and-disaster-recovery.md
// "State Store rebuild" step 5;
// docs/architecture/11-scalability-and-distributed-state.md "State
// client and RZ-STS error codes"). The per-second limit holds for the
// shard, while the backoff after connect failures belongs to the server
// address that failed: the primary shard of a sentinel deployment moves
// to the replica the sentinels promote, and a dial to it must not wait
// out the dead primary's backoff. A dial refused by the Pacer fails at
// once without touching the network and is not a connect failure.
type Pacer struct {
	clk clock.Clock
	cfg PacerConfig
	rnd Rand

	mu sync.Mutex
	// backoffs holds the addresses with consecutive connect failures,
	// the most recent failure last, at most MaxPacedAddrs.
	backoffs []backoff
	starts   []time.Time // ring of the last PerSecond dial starts
	next     int
}

// backoff is one server address's consecutive connect failures and the
// time before which its next dial waits.
type backoff struct {
	addr      string
	fails     int
	notBefore time.Time
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

// TryDial is TryDialAddr for a shard that dials one address.
func (p *Pacer) TryDial() bool { return p.TryDialAddr("") }

// Result is ResultAddr for a shard that dials one address.
func (p *Pacer) Result(ok bool) { p.ResultAddr("", ok) }

// TryDialAddr reports whether a dial to addr may start now, and records
// its start: false while addr's backoff delay after its last failure runs
// or when PerSecond dials of the shard already started in the last
// second.
func (p *Pacer) TryDialAddr(addr string) bool {
	now := p.clk.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if i := p.find(addr); i >= 0 && now.Before(p.backoffs[i].notBefore) {
		return false
	}
	if oldest := p.starts[p.next]; !oldest.IsZero() && now.Sub(oldest) < time.Second {
		return false
	}
	p.starts[p.next] = now
	p.next = (p.next + 1) % len(p.starts)
	return true
}

// ResultAddr records the outcome of a dial to addr TryDialAddr admitted:
// success resets addr's backoff, a failure (dial or TLS handshake error)
// delays addr's next dial by a full-jitter backoff. Past MaxPacedAddrs
// addresses, the one whose failure is oldest is forgotten.
func (p *Pacer) ResultAddr(addr string, ok bool) {
	now := p.clk.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.find(addr)
	if ok {
		if i >= 0 {
			p.backoffs = slices.Delete(p.backoffs, i, i+1)
		}
		return
	}
	b := backoff{addr: addr}
	if i >= 0 {
		b = p.backoffs[i]
		p.backoffs = slices.Delete(p.backoffs, i, i+1)
	} else if len(p.backoffs) == MaxPacedAddrs {
		p.backoffs = slices.Delete(p.backoffs, 0, 1)
	}
	b.fails++
	b.notBefore = now.Add(p.rnd(0, p.ceiling(b.fails)))
	p.backoffs = append(p.backoffs, b)
}

// find returns the index of addr's backoff, -1 for none.
func (p *Pacer) find(addr string) int {
	return slices.IndexFunc(p.backoffs, func(b backoff) bool { return b.addr == addr })
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

// Failures returns the consecutive connect failures of the address that
// failed last, since its last success; 0 when no address is backing off.
func (p *Pacer) Failures() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.backoffs) == 0 {
		return 0
	}
	return p.backoffs[len(p.backoffs)-1].fails
}

// Wait returns how long until a dial to the address that failed last, or
// to any address when none is backing off, can start (0 when it can now).
func (p *Pacer) Wait() time.Duration {
	now := p.clk.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	var w time.Duration
	if n := len(p.backoffs); n > 0 {
		w = max(p.backoffs[n-1].notBefore.Sub(now), 0)
	}
	if oldest := p.starts[p.next]; !oldest.IsZero() {
		w = max(w, oldest.Add(time.Second).Sub(now))
	}
	return w
}
