// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"time"

	"github.com/redis/rueidis"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/breaker"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// lookupFields are the partition fields one lookup reads.
const lookupFields = 3

// Read runs Response Cache lookups as one pipelined batch: per lookup a
// GET of the URI's generation key and an HMGET of the partition's names,
// the variant's generation and its MAC-verified entry. Every other kind is
// an error reply. A lookup whose shard's breaker is open is skipped
// (RZ-STS-003); the round trip's timeout is the smallest of the batch,
// clamped by the per-request deadline.
func (d *Driver) Read(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) {
	if len(calls) == 0 {
		return
	}
	batch := -1
	if len(calls) > 1 {
		batch = 0
	}
	timeout := calls[0].Timeout
	for _, c := range calls {
		c.Done, c.Err, c.Elapsed, c.Batch, c.Trip = false, nil, 0, batch, statestore.TripPipeline
		timeout = min(timeout, c.Timeout)
	}
	stripe := stripeOf(rb)
	fail := func(cs []*statestore.Call, err *statestore.Error) {
		for _, c := range cs {
			if c.Err == nil {
				c.Err = err
				d.count(stripe, c.Kind.Label(), err.Result())
			}
		}
	}
	t, ok := d.callTimeout(ctx, rb, timeout)
	switch {
	case !ok:
		fail(calls, statestore.ErrNotAttempted)
		return
	case d.closed.Load():
		fail(calls, statestore.ErrFailed)
		return
	}
	invalid := false
	var sbuf [8]*shard
	shs := sbuf[:0]
	for _, c := range calls {
		if c.Kind != statestore.OpCacheGet {
			c.Err, invalid = statestore.ErrFailed, true
			continue
		}
		gs, ps := d.lookupShards(&c.Lookup)
		if gs.brk.State() == breaker.Open || ps.brk.State() == breaker.Open {
			c.Err = statestore.ErrBreakerOpen
			d.count(stripe, c.Kind.Label(), statestore.ResultSkipped)
			continue
		}
		shs = addShard(shs, gs)
		shs = addShard(shs, ps)
	}
	// noTrip counts the calls of another kind as one failed pipeline, as
	// when they would have crossed the network.
	noTrip := func() {
		if invalid {
			d.count(stripe, emit.StateOpPipeline, statestore.ResultError)
		}
	}
	if len(shs) == 0 {
		noTrip()
		return
	}
	tr := d.getTrip()
	tr.setShards(shs)
	if err := d.admit(tr); err != nil {
		d.putTrip(tr)
		fail(calls, err)
		noTrip()
		return
	}
	sent := d.encodeLookups(tr, calls, stripe)
	tr.cancelUnused()
	if sent == 0 {
		d.gate.Release()
		d.putTrip(tr)
		noTrip()
		return
	}
	if !d.launch(ctx, tr) {
		d.putTrip(tr)
		fail(calls, statestore.ErrFailed)
		noTrip()
		return
	}
	res := statestore.ResultOK
	if invalid {
		res = statestore.ResultError
	}
	out, elapsed := tr.wait(ctx, t)
	if out != replied {
		for _, c := range calls {
			if c.Err == nil {
				c.Err, c.Elapsed = statestore.ErrTimeout, elapsed
			}
		}
		d.finishRead(stripe, statestore.ResultTimeout, elapsed, sent)
		return
	}
	if !d.parseLookups(ctx, tr, calls, elapsed) {
		res = statestore.ResultError
	}
	tr.settle(true)
	d.putTrip(tr)
	d.finishRead(stripe, res, elapsed, sent)
}

// finishRead records one attempted lookup round trip of sent lookups.
func (d *Driver) finishRead(stripe emit.Stripe, res statestore.Result, elapsed time.Duration, sent int) {
	d.count(stripe, emit.StateOpPipeline, res)
	d.observe(stripe, emit.StateOpPipeline, elapsed)
	d.countOps(stripe, emit.StateOpCacheGet, uint64(2*sent)) //nolint:gosec // G115: a small non-negative count
}

// lookupShards returns the shards of a lookup's generation key and
// partition.
func (d *Driver) lookupShards(l *statestore.CacheLookup) (gen, part *shard) {
	return d.shardOfSlot(keys.DigestSlot(l.Key.URI)), d.shardOfSlot(keys.PartitionSlot(l.Key))
}

// addShard appends sh unless shs holds it.
func addShard(shs []*shard, sh *shard) []*shard {
	for _, s := range shs {
		if s == sh {
			return shs
		}
	}
	return append(shs, sh)
}

// ticketOf returns the index of sh's ticket.
func (tr *trip) ticketOf(sh *shard) int {
	for i := range tr.tickets {
		if tr.tickets[i].sh == sh {
			return i
		}
	}
	return -1
}

// encodeLookups writes the GET and HMGET of every lookup whose shards
// admitted the trip, skips the others with RZ-STS-003, and returns how
// many it wrote.
func (d *Driver) encodeLookups(tr *trip, calls []*statestore.Call, stripe emit.Stripe) int {
	var kb [keys.CachePartitionLen]byte
	var fb [keys.FieldLen]byte
	sent := 0
	for _, c := range calls {
		if c.Err != nil {
			continue
		}
		l := &c.Lookup
		gs, ps := d.lookupShards(l)
		gi, pi := tr.ticketOf(gs), tr.ticketOf(ps)
		if tr.tickets[gi].refused || tr.tickets[pi].refused {
			c.Err = statestore.ErrBreakerOpen
			d.count(stripe, c.Kind.Label(), statestore.ResultSkipped)
			continue
		}
		tr.cmd(gi, "GET")
		tr.key(string(keys.AppendCacheGeneration(kb[:0], l.Key.URI)))
		tr.cmd(pi, "HMGET")
		tr.key(string(keys.AppendCachePartition(kb[:0], l.Key)))
		tr.arg(keys.FieldNames)
		tr.arg(string(keys.AppendField(fb[:0], keys.FieldGeneration, l.Variant)))
		tr.arg(string(keys.AppendField(fb[:0], keys.FieldEntry, l.Variant)))
		sent++
	}
	return sent
}

// parseLookups fills every sent lookup from its two replies; false when
// one failed.
func (d *Driver) parseLookups(ctx context.Context, tr *trip, calls []*statestore.Call, elapsed time.Duration) bool {
	ok := true
	i := 0
	for _, c := range calls {
		if c.Err != nil {
			continue
		}
		c.Elapsed = elapsed
		if err := d.parseLookup(ctx, tr, i, &c.Lookup); err != nil {
			c.Err, ok = err, false
		} else {
			c.Done = true
		}
		i += 2
	}
	return ok
}

// parseLookup reads lookup replies i (GET) and i+1 (HMGET). A missing
// partition, variant or generation field is a miss; an entry failing its
// MAC check is a miss, logged at most once per interval.
func (d *Driver) parseLookup(ctx context.Context, tr *trip, i int, l *statestore.CacheLookup) *statestore.Error {
	gm, gnull, err := tr.result(i)
	if err != nil {
		return tr.failure(ctx, i, err)
	}
	var gen int64
	if !gnull {
		if gen, err = gm.AsInt64(); err != nil {
			tr.fail(i)
			return statestore.ErrFailed
		}
	}
	hm, _, err := tr.result(i + 1)
	if err != nil {
		return tr.failure(ctx, i+1, err)
	}
	fields, err := hm.ToArray()
	if err != nil || len(fields) != lookupFields {
		tr.fail(i + 1)
		return statestore.ErrFailed
	}
	names, ok := optString(&fields[0])
	if !ok {
		tr.fail(i + 1)
		return statestore.ErrFailed
	}
	l.Generation, l.Names, l.EntryGen, l.Found, l.Entry = gen, names, 0, false, l.Entry[:0]
	if fields[1].IsNil() || fields[2].IsNil() {
		return nil
	}
	entryGen, err := fields[1].AsInt64()
	if err != nil {
		tr.fail(i + 1)
		return statestore.ErrFailed
	}
	stored, err := fields[2].ToString()
	if err != nil {
		tr.fail(i + 1)
		return statestore.ErrFailed
	}
	l.Entry = append(l.Entry[:0], stored...)
	buf := l.Entry
	l.EntryGen, l.Entry = entryGen, buf[:0]
	entry := buf
	if d.mac != nil {
		var kb [keys.CachePartitionLen]byte
		var fb [keys.FieldLen]byte
		var valid bool
		entry, valid = d.mac.Open(keys.AppendCachePartition(kb[:0], l.Key), keys.AppendField(fb[:0], keys.FieldEntry, l.Variant), buf)
		if !valid {
			if d.macLog.allow(d.clk.Now()) {
				d.log.WarnContext(ctx, "state store cache entry failed its MAC check; served as a miss")
			}
			return nil
		}
	}
	l.Found, l.Entry = true, entry
	return nil
}

// optString returns a string reply, "" for null; false for another type.
func optString(m *rueidis.RedisMessage) (string, bool) {
	if m.IsNil() {
		return "", true
	}
	s, err := m.ToString()
	return s, err == nil
}
