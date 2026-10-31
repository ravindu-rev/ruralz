// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// DefaultPumpStep is how often a Pump advances the Nodes' clock: a 50 ms
// stateStoreTimeout then fires within one step of its deadline.
const DefaultPumpStep = 10 * time.Millisecond

// Advancer is the controllable clock of a topology
// (internal/testkit/topology.Topology).
type Advancer interface {
	// Advance moves every Node's clock forward by d and returns once each
	// fired every timer due.
	Advance(ctx context.Context, d time.Duration) error
	// Now returns the Nodes' clock reading.
	Now() time.Time
}

// Mark is one advance of a Pump: the real time it was sent and answered,
// and the Node time it set.
type Mark struct {
	Sent, Answered time.Time
	Node           time.Time
}

// Pump keeps a topology's controllable clock moving with real time while
// load runs (docs/engineering/03-testing-and-quality-strategy.md "Hermetic
// rule scope"): every step it advances the Nodes' clock to the real time
// elapsed since it started, plus every Jump, so local buckets refill,
// breakers half-open and Drains run on the time the test sets while
// redis-server, the balancer and the mocks run in real time. An advance
// another caller makes, such as a Stop's Drain steps, moves the pump's
// track forward with it, so the Nodes' time never stalls. Every advance
// is kept as a Mark, so a real instant maps to the Node time in force
// then (NodeTime).
type Pump struct {
	c    Advancer
	real clock.Clock
	step time.Duration

	mu        sync.Mutex
	offset    time.Duration // Node time minus real time
	marks     []Mark
	err       error
	maxBehind time.Duration // the largest gap an advance found

	stop chan struct{}
	done chan struct{}
}

// StartPump starts advancing c every step of realClock (DefaultPumpStep
// when 0) until Stop or the end of ctx.
func StartPump(ctx context.Context, c Advancer, realClock clock.Clock, step time.Duration) *Pump {
	if step <= 0 {
		step = DefaultPumpStep
	}
	now := realClock.Now()
	p := &Pump{
		c: c, real: realClock, step: step,
		offset: c.Now().Sub(now),
		marks:  []Mark{{Sent: now, Answered: now, Node: c.Now()}},
		stop:   make(chan struct{}), done: make(chan struct{}),
	}
	go p.loop(ctx)
	return p
}

func (p *Pump) loop(ctx context.Context) {
	defer close(p.done)
	timer := p.real.NewTimer(p.step)
	defer timer.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ctx.Done():
			return
		case <-timer.C():
		}
		if err := p.catchUp(ctx, 0); err != nil {
			p.mu.Lock()
			if p.err == nil {
				p.err = err
			}
			p.mu.Unlock()
			return
		}
		timer.Reset(p.step)
	}
}

// catchUp advances the clock to real time plus the offset grown by jump.
// Falling behind, as when a busy Node answers an advance late, it moves
// the clock at most one step per advance and drops the rest from its
// track: Node time never runs faster than real time, and a Node never
// reads a clock more than about a step stale, so a deadline on the Nodes'
// clock, such as a State Store call's stateStoreTimeout, never fires on a
// call younger in real time than its deadline less one step.
func (p *Pump) catchUp(ctx context.Context, jump time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.real.Now()
	p.offset += jump
	cur := p.c.Now()
	target := now.Add(p.offset)
	if cur.After(target) {
		// Another caller advanced the clock: follow from there.
		p.offset = cur.Sub(now)
		return nil
	}
	d := target.Sub(cur)
	if d <= 0 {
		return nil
	}
	if jump == 0 {
		p.maxBehind = max(p.maxBehind, d)
		if d > p.step {
			// Behind real time: the track follows the Nodes from here.
			p.offset -= d - p.step
			d = p.step
		}
	}
	if err := p.c.Advance(ctx, d); err != nil {
		return fmt.Errorf("chaos: clock pump: %w", err)
	}
	p.marks = append(p.marks, Mark{Sent: now, Answered: p.real.Now(), Node: p.c.Now()})
	return nil
}

// Jump advances the Nodes' clock by d at once, for a test that moves
// through a timeline faster than real time, such as the linger and Drain
// of a handover; the pump then keeps moving with real time from there.
func (p *Pump) Jump(ctx context.Context, d time.Duration) error {
	if d < 0 {
		return fmt.Errorf("chaos: clock pump: jump by %v", d)
	}
	return p.catchUp(ctx, d)
}

// Walk moves the Nodes' clock through a timeline faster than real time,
// such as a handover's linger and Drain under load: a jump of step, then
// wait of real time for the Nodes to act on the timers it fired, until
// done reports true, max has passed in Node time, or ctx ends. Unlike one
// jump over the whole timeline, a step fires only the timers due within
// it, so a request in flight meets none of its own deadlines longer than
// a step at once, only after the steps and waits that reach it (WalkStep).
// It reports whether done held.
func (p *Pump) Walk(ctx context.Context, step, wait, maxWalk time.Duration, done func() bool) (bool, error) {
	if step <= 0 || wait < 0 {
		return false, fmt.Errorf("chaos: clock pump: walk step %v, wait %v", step, wait)
	}
	timer := p.real.NewTimer(wait)
	defer timer.Stop()
	for walked := time.Duration(0); ; walked += step {
		if done() {
			return true, nil
		}
		if walked >= maxWalk {
			return false, nil
		}
		if err := p.Jump(ctx, step); err != nil {
			return false, err
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C():
		}
	}
}

// Stop stops the pump and returns the first advance that failed.
func (p *Pump) Stop() error {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Err returns the first advance that failed, nil while the pump runs.
func (p *Pump) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// MaxBehind returns the largest gap between the Nodes' clock and its
// real-time track an advance found, a step when the pump kept pace.
func (p *Pump) MaxBehind() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxBehind
}

// Marks returns the advances so far, oldest first.
func (p *Pump) Marks() []Mark {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Mark(nil), p.marks...)
}

// ErrBeforeStart is returned by NodeTime for an instant before the pump
// started.
var ErrBeforeStart = errors.New("chaos: clock pump: instant before the pump started")

// NodeTime returns the Node time in force at real instant t: the Node time
// of the last advance answered by t. ambiguous reports an advance sent
// but not answered by t, when Nodes may show either reading.
func (p *Pump) NodeTime(t time.Time) (node time.Time, ambiguous bool, err error) {
	return nodeTimeAt(p.Marks(), t)
}

// nodeTimeAt is NodeTime over marks.
func nodeTimeAt(marks []Mark, t time.Time) (time.Time, bool, error) {
	if len(marks) == 0 || t.Before(marks[0].Sent) {
		return time.Time{}, false, ErrBeforeStart
	}
	// The first mark answered after t.
	i := sort.Search(len(marks), func(i int) bool { return marks[i].Answered.After(t) })
	if i == 0 {
		return marks[0].Node, false, nil
	}
	prev := marks[i-1].Node
	if i < len(marks) && !marks[i].Sent.After(t) {
		return prev, true, nil
	}
	return prev, false, nil
}

// NodeSpan returns the Node times a Node may have read at real instant
// t: the Node time of the last advance answered by t, and, while an
// advance sent by t is not yet answered, the Node time that advance sets.
func (p *Pump) NodeSpan(t time.Time) (lo, hi time.Time, err error) {
	return nodeSpanAt(p.Marks(), t)
}

// nodeSpanAt is NodeSpan over marks.
func nodeSpanAt(marks []Mark, t time.Time) (time.Time, time.Time, error) {
	lo, ambiguous, err := nodeTimeAt(marks, t)
	if err != nil || !ambiguous {
		return lo, lo, err
	}
	i := sort.Search(len(marks), func(i int) bool { return marks[i].Answered.After(t) })
	return lo, marks[i].Node, nil
}

// CauseSpan returns the Node times at which a Node timer may have fired
// to cause an event seen at real instant t: from the Node time before the
// advance in force at t, since a timer fires within the advance that
// passes its deadline and its effects follow the answer, to NodeSpan's
// upper end.
func (p *Pump) CauseSpan(t time.Time) (lo, hi time.Time, err error) {
	return causeSpanAt(p.Marks(), t)
}

// causeSpanAt is CauseSpan over marks.
func causeSpanAt(marks []Mark, t time.Time) (time.Time, time.Time, error) {
	_, hi, err := nodeSpanAt(marks, t)
	if err != nil {
		return hi, hi, err
	}
	// marks[i-1] is the last advance answered by t.
	i := sort.Search(len(marks), func(i int) bool { return marks[i].Answered.After(t) })
	if i >= 2 {
		return marks[i-2].Node, hi, nil
	}
	return marks[0].Node, hi, nil
}

// RealTime returns the real instant by which the Nodes' clock had reached
// node: the answer of the first advance setting it at or past node; ok is
// false when the pump never got there.
func (p *Pump) RealTime(node time.Time) (time.Time, bool) {
	marks := p.Marks()
	i := sort.Search(len(marks), func(i int) bool { return !marks[i].Node.Before(node) })
	if i == len(marks) {
		return time.Time{}, false
	}
	return marks[i].Answered, true
}
