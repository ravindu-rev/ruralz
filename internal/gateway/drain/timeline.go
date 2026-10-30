// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package drain

import (
	"math/bits"
	"math/rand/v2"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/listener"
)

// Drain timeline defaults, target values of
// docs/operations/02-zero-downtime-upgrades-and-hot-reload.md "Drain
// timeline defaults".
const (
	// AcceptWindow is how long a stop-signal or failure Drain keeps
	// accepting after readiness fails, while balancers observe /readyz.
	AcceptWindow = 5 * time.Second
	// JitterWindow is the window, from stop-accepting, across which the
	// server shards of the client listeners shut down in Waves waves.
	JitterWindow = 10 * time.Second
	// Deadline is the Drain deadline after stop-accepting: the remaining
	// work ends with RZ-RT-016.
	Deadline = 20 * time.Second
	// FlushBound bounds everything after the deadline: ending the
	// remaining work, closing the listeners and the shutdown steps, the
	// post-commit queue and telemetry flushes included.
	FlushBound = 5 * time.Second
	// ExitBound is the exit bound after a stop signal, 30 s.
	ExitBound = AcceptWindow + Deadline + FlushBound
	// HandoverExitBound is the exit bound after a handover's Drain starts
	// at the linger's end, 25 s: the deadline 20 s later, 23 s after
	// steering, then the flush.
	HandoverExitBound = Deadline + FlushBound
)

// Waves is the number of shard waves: K, the server shards of each client
// listener.
const Waves = listener.Shards

// wave is one wave's share of the jitter window: shard i shuts down within
// [i × wave, (i + 1) × wave) after stop-accepting.
const wave = JitterWindow / Waves

// kind is what started a Drain.
type kind uint8

const (
	kindNone kind = iota
	// kindSignal is a stop signal: SIGTERM or SIGINT.
	kindSignal
	// kindHandover is an accepted handover, at the linger's end.
	kindHandover
	// kindFailure is a part of the Node that stopped after boot.
	kindFailure
)

// String returns the reason logged for the Drain kind.
func (k kind) String() string {
	switch k {
	case kindSignal:
		return "signal"
	case kindHandover:
		return "handover"
	case kindFailure:
		return "failure"
	default:
		return "none"
	}
}

// schedule is one Drain's timeline, on the Drainer's clock.
type schedule struct {
	// skip goes straight to the deadline: a Node never seen ready with no
	// client listener bound.
	skip          bool
	stopAccepting time.Time
	// waves holds the time shard i of every client listener shuts down.
	waves    [Waves]time.Time
	deadline time.Time
}

// exitBy returns the exit bound of the schedule.
func (s *schedule) exitBy() time.Time { return s.deadline.Add(FlushBound) }

// plan returns the timeline of a Drain of kind k started at start. A
// handover's Drain starts where its listeners already stopped accepting;
// any other keeps the accept window. Shard i shuts down at a time drawn
// uniformly from src within the i-th wave of the jitter window. A skipped
// Drain reaches its deadline at once.
func plan(k kind, start time.Time, skip bool, src rand.Source) schedule {
	s := schedule{skip: skip, stopAccepting: start, deadline: start}
	if skip {
		return s
	}
	if k != kindHandover {
		s.stopAccepting = start.Add(AcceptWindow)
	}
	for i := range s.waves {
		s.waves[i] = s.stopAccepting.Add(time.Duration(i)*wave + uniform(src, wave))
	}
	s.deadline = s.stopAccepting.Add(Deadline)
	return s
}

// uniform returns a duration drawn uniformly from [0, d).
func uniform(src rand.Source, d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	hi, _ := bits.Mul64(src.Uint64(), uint64(d))
	return time.Duration(hi) //nolint:gosec // G115: hi < d, a positive Duration.
}

// globalSource draws from the math/rand/v2 global generator, which is safe
// for concurrent use.
type globalSource struct{}

// Uint64 implements rand.Source.
func (globalSource) Uint64() uint64 {
	return rand.Uint64() //nolint:gosec // G404: the wave jitter needs a uniform draw, not an unpredictable one.
}
