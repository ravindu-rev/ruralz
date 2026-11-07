// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// breakerState is a fault breaker's state.
type breakerState uint8

// Fault breaker states.
const (
	breakerClosed breakerState = iota
	breakerTripped
	breakerHalfOpen
)

// faultBreaker is a pool's fault breaker (docs/architecture/05-wasm-plugin-
// system.md "Default limits", Fault breaker): faults (traps, timeouts and
// memory limits) above 50% of at least 200 calls in a rolling 10 s trip
// it for 5 s, during which no call reaches the guest (RZ-PLG-009); it then
// half-opens, letting every twentieth call, 5%, reach the guest for one
// window: at the window's end it closes when at most half of those probes
// faulted, else trips again. Calls are counted in one-second buckets on
// the Node clock.
type faultBreaker struct {
	clk clock.Clock
	mu  sync.Mutex
	// buckets hold calls and faults per second of the rolling window,
	// indexed by the second modulo the window.
	calls, faults [breakerWindow / time.Second]int
	// second is the Unix second of the newest bucket.
	second int64
	state  breakerState
	// until is when a trip ends, or a half-open window closes.
	until time.Time
	// probe counts half-open calls to pick every twentieth; probes and
	// probeFaults count the probes of the half-open window.
	probe, probes, probeFaults int
	// onChange reports a change between closed and not closed.
	onChange func(open bool)
}

// newFaultBreaker returns a closed breaker.
func newFaultBreaker(clk clock.Clock, onChange func(open bool)) *faultBreaker {
	return &faultBreaker{clk: clk, onChange: onChange}
}

// advance moves the buckets to now, clearing the seconds that passed.
func (b *faultBreaker) advance(now time.Time) {
	s := now.Unix()
	n := int64(len(b.calls))
	if s <= b.second {
		return
	}
	if s-b.second >= n {
		clear(b.calls[:])
		clear(b.faults[:])
	} else {
		for t := b.second + 1; t <= s; t++ {
			b.calls[t%n] = 0
			b.faults[t%n] = 0
		}
	}
	b.second = s
}

// allow reports whether a call may reach the guest, and whether it is a
// half-open probe.
func (b *faultBreaker) allow() (ok, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clk.Now()
	b.transition(now)
	switch b.state {
	case breakerTripped:
		return false, false
	case breakerHalfOpen:
		b.probe++
		if b.probe%(100/breakerProbePercent) != 0 {
			return false, false
		}
		return true, true
	default:
		return true, false
	}
}

// transition ends a trip or a half-open window that is over.
func (b *faultBreaker) transition(now time.Time) {
	switch b.state {
	case breakerTripped:
		if !now.Before(b.until) {
			b.state, b.until = breakerHalfOpen, now.Add(breakerWindow)
			b.probe, b.probes, b.probeFaults = 0, 0, 0
		}
	case breakerHalfOpen:
		if !now.Before(b.until) {
			if b.probes > 0 && 100*b.probeFaults > breakerFaultPercent*b.probes {
				b.trip(now)
				return
			}
			b.state = breakerClosed
			clear(b.calls[:])
			clear(b.faults[:])
			if b.onChange != nil {
				b.onChange(false)
			}
		}
	default:
	}
}

// record counts a call that reached the guest, and whether it faulted.
func (b *faultBreaker) record(fault, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clk.Now()
	b.transition(now)
	if probe && b.state == breakerHalfOpen {
		b.probes++
		if fault {
			b.probeFaults++
		}
		return
	}
	if b.state != breakerClosed {
		return
	}
	b.advance(now)
	i := now.Unix() % int64(len(b.calls))
	b.calls[i]++
	if fault {
		b.faults[i]++
	}
	var calls, faults int
	for j := range b.calls {
		calls += b.calls[j]
		faults += b.faults[j]
	}
	if calls >= breakerMinCalls && 100*faults > breakerFaultPercent*calls {
		b.trip(now)
	}
}

// trip opens the breaker for breakerOpen.
func (b *faultBreaker) trip(now time.Time) {
	was := b.state
	b.state, b.until = breakerTripped, now.Add(breakerOpen)
	if was == breakerClosed && b.onChange != nil {
		b.onChange(true)
	}
}

// open reports whether the breaker is not closed.
func (b *faultBreaker) open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transition(b.clk.Now())
	return b.state != breakerClosed
}
