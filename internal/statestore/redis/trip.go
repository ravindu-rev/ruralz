// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/redis/rueidis"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/breaker"
)

// errNoNode reports a probe of a server the cluster client no longer
// knows.
var errNoNode = errors.New("redis: no client for the shard")

// Trip states: the goroutine sending the commands is running, it replied
// to a waiting caller, or the caller left at its timeout or cancellation
// and the goroutine finishes the trip alone.
const (
	tripRunning uint32 = iota
	tripReplied
	tripAbandoned
)

// outcome is how a caller's wait for a trip ended.
type outcome uint8

const (
	replied outcome = iota
	timedOut
	canceled
)

// ticket is one shard's breaker claim on a trip: refused when the
// shard's breaker admitted no call, failed when the caller's parsing found
// a command of the shard without a well-formed reply.
type ticket struct {
	sh      *shard
	t       breaker.Ticket
	refused bool
	failed  bool
}

// spec is one command as words: tok leading tokens, then keys, then args.
type spec struct {
	start, tok, keys, args int
	// ticket indexes the trip's tickets.
	ticket int
}

// trip is one round trip: its commands, the shard ticket of each command,
// and the results. A goroutine builds the commands with the State
// client's builder, sends them and waits for rueidis, so the in-flight
// slot is held until the reply or a connection reset, while the caller
// waits only for its timeout. Trips are pooled; the caller recycles a trip
// that replied, the goroutine one the caller left.
type trip struct {
	d *Driver
	// node, when set, sends through that server's client (probes);
	// otherwise the State client routes the commands.
	node    string
	words   []string
	specs   []spec
	cmds    []rueidis.Completed
	tickets []ticket
	res     []rueidis.RedisResult
	one     [1]rueidis.RedisResult
	errs    []rueidis.RedisResult
	state   atomic.Uint32
	done    chan struct{}
	timer   clock.Timer
	start   time.Time
}

// newTrip returns an empty trip with a stopped timer.
func (d *Driver) newTrip() *trip {
	t := d.clk.NewTimer(time.Hour)
	t.Stop()
	return &trip{d: d, done: make(chan struct{}, 1), timer: t}
}

// getTrip returns a pooled trip ready to fill.
func (d *Driver) getTrip() *trip {
	tr, _ := d.trips.Get().(*trip)
	if tr == nil {
		tr = d.newTrip()
	}
	tr.state.Store(tripRunning)
	return tr
}

// putTrip clears tr and returns it to the pool.
func (d *Driver) putTrip(tr *trip) {
	tr.node = ""
	clear(tr.words)
	tr.words = tr.words[:0]
	tr.specs = tr.specs[:0]
	clear(tr.cmds)
	tr.cmds = tr.cmds[:0]
	for i := range tr.tickets {
		tr.tickets[i].sh, tr.tickets[i].refused, tr.tickets[i].failed = nil, false, false
	}
	tr.tickets = tr.tickets[:0]
	tr.res = nil
	tr.one[0] = rueidis.RedisResult{}
	clear(tr.errs)
	tr.errs = tr.errs[:0]
	tr.drainTimer()
	select {
	case <-tr.done:
	default:
	}
	d.trips.Put(tr)
}

// setShards prepares one ticket per shard, in order. It must run before
// any ticket is admitted: growing the slice moves the tickets.
func (tr *trip) setShards(shs []*shard) {
	if cap(tr.tickets) < len(shs) {
		tr.tickets = make([]ticket, len(shs))
	}
	tr.tickets = tr.tickets[:len(shs)]
	for i, sh := range shs {
		tr.tickets[i].sh, tr.tickets[i].refused, tr.tickets[i].failed = sh, false, false
	}
}

// cmd starts a command for ticket k with its leading tokens; key and arg
// append to it, keys first.
func (tr *trip) cmd(k int, tokens ...string) {
	tr.specs = append(tr.specs, spec{start: len(tr.words), tok: len(tokens), ticket: k})
	tr.words = append(tr.words, tokens...)
}

// key appends a key to the last command.
func (tr *trip) key(k string) {
	tr.words = append(tr.words, k)
	tr.specs[len(tr.specs)-1].keys++
}

// arg appends an argument to the last command.
func (tr *trip) arg(a string) {
	tr.words = append(tr.words, a)
	tr.specs[len(tr.specs)-1].args++
}

// fail marks the ticket of command i failed.
func (tr *trip) fail(i int) {
	tr.tickets[tr.specs[i].ticket].failed = true
}

// shardOf returns the shard of command i.
func (tr *trip) shardOf(i int) *shard { return tr.tickets[tr.specs[i].ticket].sh }

// drainTimer stops the timer and empties its channel.
func (tr *trip) drainTimer() {
	tr.timer.Stop()
	select {
	case <-tr.timer.C():
	default:
	}
}

// admit takes an in-flight slot and admits every ticket. A full ceiling
// is RZ-STS-004 and admits nothing. A shard whose half-open breaker
// already has its probe refuses its ticket, and the slot stays held for
// the others; with nothing admitted the slot is released (RZ-STS-003).
func (d *Driver) admit(tr *trip) *statestore.Error {
	if !d.gate.TryAcquire() {
		return statestore.ErrNotAttempted
	}
	admitted := false
	for i := range tr.tickets {
		tk := &tr.tickets[i]
		ok := tk.sh.brk.Allow(&tk.t)
		tk.refused = !ok
		admitted = admitted || ok
	}
	if !admitted {
		d.gate.Release()
		return statestore.ErrBreakerOpen
	}
	return nil
}

// launch starts tr's goroutine. The caller holds an in-flight slot and
// has admitted every ticket of a command; the goroutine releases the slot
// when rueidis returns. False means the driver closed: the slot is
// released and the tickets fail.
func (d *Driver) launch(ctx context.Context, tr *trip) bool {
	tr.start = d.clk.Now()
	if !d.goStart() {
		d.gate.Release()
		tr.settle(false)
		return false
	}
	go tr.run(context.WithoutCancel(ctx))
	return true
}

// run builds and sends the commands, releases the in-flight slot, and
// hands the results to a waiting caller, or settles and recycles the trip
// when the caller left.
func (tr *trip) run(ctx context.Context) {
	d := tr.d
	defer d.goEnd()
	c := d.awaitClient()
	switch {
	case c == nil:
		tr.failAll(errClosed)
	case tr.node != "":
		if n := d.nodeClient(c, tr.node); n != nil {
			tr.send(ctx, n)
		} else {
			tr.failAll(errNoNode)
		}
	default:
		tr.send(ctx, c)
	}
	d.gate.Release()
	if tr.state.CompareAndSwap(tripRunning, tripReplied) {
		tr.done <- struct{}{}
		return
	}
	tr.settleLate(ctx)
	d.putTrip(tr)
}

// send builds the commands with c's builder and runs them through c.
func (tr *trip) send(ctx context.Context, c rueidis.Client) {
	b := c.B()
	for _, sp := range tr.specs {
		w := tr.words[sp.start : sp.start+sp.tok+sp.keys+sp.args]
		tr.cmds = append(tr.cmds, b.Arbitrary(w[:sp.tok]...).Keys(w[sp.tok:sp.tok+sp.keys]...).Args(w[sp.tok+sp.keys:]...).Build())
	}
	if len(tr.cmds) == 1 {
		tr.one[0] = c.Do(ctx, tr.cmds[0])
		tr.res = tr.one[:]
		return
	}
	tr.res = c.DoMulti(ctx, tr.cmds...)
}

// failAll answers every command with err.
func (tr *trip) failAll(err error) {
	for range tr.specs {
		tr.errs = append(tr.errs, rueidis.NewErrorResult(err))
	}
	tr.res = tr.errs
}

// wait waits for the reply, at most timeout on the driver's clock, or
// until ctx ends, and returns the round trip's elapsed time. A timeout is
// a failure of every ticket, recorded at the timeout. After timedOut or
// canceled the caller has left the trip: the goroutine settles and
// recycles it, so the caller must not touch it again.
func (tr *trip) wait(ctx context.Context, timeout time.Duration) (outcome, time.Duration) {
	tr.timer.Reset(timeout)
	select {
	case <-tr.done:
		tr.drainTimer()
		return replied, tr.d.clk.Since(tr.start)
	case <-tr.timer.C():
		tr.settle(false)
		if tr.state.CompareAndSwap(tripRunning, tripAbandoned) {
			return timedOut, timeout
		}
	case <-ctx.Done():
		elapsed := tr.d.clk.Since(tr.start)
		tr.drainTimer()
		if tr.state.CompareAndSwap(tripRunning, tripAbandoned) {
			return canceled, elapsed
		}
	}
	<-tr.done
	return replied, tr.d.clk.Since(tr.start)
}

// settle records every ticket's outcome: a failure for all when ok is
// false, else each ticket's own. Tickets their shard refused record
// nothing.
func (tr *trip) settle(ok bool) {
	for i := range tr.tickets {
		tk := &tr.tickets[i]
		tk.sh.brk.Done(&tk.t, ok && !tk.failed)
	}
}

// cancelUnused withdraws every admitted ticket no command uses, recording
// nothing (a probe claim is freed).
func (tr *trip) cancelUnused() {
	for i := range tr.tickets {
		used := false
		for _, sp := range tr.specs {
			if sp.ticket == i {
				used = true
				break
			}
		}
		if !used {
			tk := &tr.tickets[i]
			tk.sh.brk.Cancel(&tk.t)
		}
	}
}

// settleLate records the outcome of a trip its caller left: a ticket
// fails when one of its commands got a connection error or an error reply
// (a ticket the caller's timeout already failed ignores it), and the
// replies get their reactions (react).
func (tr *trip) settleLate(ctx context.Context) {
	for i, r := range tr.res {
		if i >= len(tr.specs) {
			break
		}
		if err := r.Error(); err != nil && !rueidis.IsRedisNil(err) {
			tr.fail(i)
			tr.react(ctx, i, err)
		}
	}
	tr.settle(true)
}

// react answers command i's error reply: NOSCRIPT schedules a script
// reload, READONLY a reconnect of the shard, whose server a failover
// demoted.
func (tr *trip) react(ctx context.Context, i int, err error) {
	switch {
	case isNoScript(err):
		tr.d.requestScripts(ctx, tr.shardOf(i))
	case isReadOnly(err):
		tr.d.reconnect(ctx, tr.shardOf(i))
	}
}

// result returns command i's message; null is true for a null reply, and
// err is a connection error or an error reply.
func (tr *trip) result(i int) (m rueidis.RedisMessage, null bool, err error) {
	m, err = tr.res[i].ToMessage()
	if rueidis.IsRedisNil(err) {
		return m, true, nil
	}
	return m, false, err
}

// failure maps command i's error to RZ-STS-002, marking its shard failed
// and reacting to the reply (react).
func (tr *trip) failure(ctx context.Context, i int, err error) *statestore.Error {
	tr.fail(i)
	tr.react(ctx, i, err)
	return statestore.ErrFailed
}
