// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"errors"
	"time"

	"github.com/redis/rueidis"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/breaker"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// lookupFields are the partition fields one lookup reads: n, t:<V>, e:<V>.
const lookupFields = 3

// Read runs Response Cache lookups as one pipelined batch of read-only
// commands: per lookup a GET of the URI's generation key (unless
// SkipToken), an HMGET of the partition's names, the variant's store time
// and its MAC-verified entry, and TIME on the partition's shard, from
// which the variant's resident time follows. Every other kind is an error
// reply. A lookup whose shard's breaker is open is skipped (RZ-STS-003);
// the round trip's timeout is the smallest of the batch, clamped by the
// per-request deadline.
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
		if (!c.Lookup.SkipToken && gs.brk.State() == breaker.Open) || ps.brk.State() == breaker.Open {
			c.Err = statestore.ErrBreakerOpen
			d.count(stripe, c.Kind.Label(), statestore.ResultSkipped)
			continue
		}
		if !c.Lookup.SkipToken {
			shs = addShard(shs, gs)
		}
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
	sent, reads := d.encodeLookups(tr, calls, stripe)
	tr.cancelUnused()
	if sent == 0 {
		d.gate.Release()
		d.putTrip(tr)
		noTrip()
		return
	}
	zone := d.tripZone(tr)
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
		d.finishRead(stripe, zone, statestore.ResultTimeout, elapsed, reads)
		return
	}
	if !d.parseLookups(ctx, tr, calls, elapsed) {
		res = statestore.ResultError
	}
	tr.settle(true)
	d.putTrip(tr)
	d.finishRead(stripe, zone, res, elapsed, reads)
}

// finishRead records one attempted lookup round trip of reads key reads
// (ruralz_state_ops_total{kind="cache_get"}: each HMGET and each GET of a
// generation key).
func (d *Driver) finishRead(stripe emit.Stripe, zone int, res statestore.Result, elapsed time.Duration, reads int) {
	d.count(stripe, emit.StateOpPipeline, res)
	d.observe(stripe, emit.StateOpPipeline, zone, elapsed)
	d.countOps(stripe, emit.StateOpCacheGet, uint64(reads)) //nolint:gosec // G115: a small non-negative count
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

// encodeLookups writes the commands of every lookup whose shards admitted
// the trip (lookupCommands of them: the GET unless SkipToken, the HMGET
// and the TIME routed to the partition's shard), skips the others with
// RZ-STS-003, and returns how many lookups it wrote and their key reads.
func (d *Driver) encodeLookups(tr *trip, calls []*statestore.Call, stripe emit.Stripe) (sent, reads int) {
	var kb [keys.CachePartitionLen]byte
	var fb [keys.FieldLen]byte
	for _, c := range calls {
		if c.Err != nil {
			continue
		}
		l := &c.Lookup
		gs, ps := d.lookupShards(l)
		gi, pi := -1, tr.ticketOf(ps)
		if !l.SkipToken {
			gi = tr.ticketOf(gs)
		}
		if (gi >= 0 && tr.tickets[gi].refused) || tr.tickets[pi].refused {
			c.Err = statestore.ErrBreakerOpen
			d.count(stripe, c.Kind.Label(), statestore.ResultSkipped)
			continue
		}
		if gi >= 0 {
			tr.cmd(gi, "GET")
			tr.key(string(keys.AppendCacheGeneration(kb[:0], l.Key.URI)))
			reads++
		}
		part := string(keys.AppendCachePartition(kb[:0], l.Key))
		tr.cmd(pi, "HMGET")
		tr.key(part)
		tr.arg(keys.FieldNames)
		tr.arg(string(keys.AppendField(fb[:0], keys.FieldTime, l.Variant)))
		tr.arg(string(keys.AppendField(fb[:0], keys.FieldEntry, l.Variant)))
		tr.cmd(pi, "TIME")
		tr.routeBy(part)
		reads++
		sent++
	}
	return sent, reads
}

// lookupCommands returns how many commands a lookup sends.
func lookupCommands(l *statestore.CacheLookup) int {
	if l.SkipToken {
		return 2
	}
	return 3
}

// parseLookups fills every sent lookup from its replies; false when one
// failed.
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
		i += lookupCommands(&c.Lookup)
	}
	return ok
}

// parseLookup reads the replies of the lookup starting at command i: the
// GET unless SkipToken, the HMGET and the TIME. A missing generation key,
// partition or variant is a miss; a generation key whose value is not a
// token is unparsable (RZ-STS-002); an entry failing its MAC check, or
// marked for the other key setting, is a miss, logged at most once per
// interval.
func (d *Driver) parseLookup(ctx context.Context, tr *trip, i int, l *statestore.CacheLookup) *statestore.Error {
	if !l.SkipToken {
		gm, gnull, err := tr.result(i)
		if err != nil {
			return tr.failure(ctx, i, err)
		}
		l.Token, l.HasToken = statestore.Token{}, false
		if !gnull {
			s, err := gm.ToString()
			tok, ok := keys.ParseToken(s)
			if err != nil || !ok {
				tr.fail(i)
				return statestore.ErrFailed
			}
			l.Token, l.HasToken = tok, true
		}
		i++
	}
	hm, _, err := tr.result(i)
	if err != nil {
		return tr.failure(ctx, i, err)
	}
	fields, err := hm.ToArray()
	if err != nil || len(fields) != lookupFields {
		tr.fail(i)
		return statestore.ErrFailed
	}
	names, ok := optString(&fields[0])
	if !ok {
		tr.fail(i)
		return statestore.ErrFailed
	}
	nowMs, terr := serverMs(tr, i+1)
	if terr != nil {
		return tr.failure(ctx, i+1, terr)
	}
	l.Partition, l.Names, l.Found, l.Entry, l.Resident = !fields[0].IsNil(), names, false, l.Entry[:0], 0
	if fields[2].IsNil() {
		return nil
	}
	var at int64
	if !fields[1].IsNil() {
		if at, err = fields[1].AsInt64(); err != nil {
			tr.fail(i)
			return statestore.ErrFailed
		}
	}
	stored, err := fields[2].ToString()
	if err != nil {
		tr.fail(i)
		return statestore.ErrFailed
	}
	resident := time.Duration(max(nowMs-at, 0)) * time.Millisecond
	// The payload, all after the tagged-entry mark, goes into the Call's
	// storage, so the entry starts where the storage does.
	var mark byte
	l.Entry = l.Entry[:0]
	if len(stored) > 0 {
		mark = stored[0]
		l.Entry = append(l.Entry, stored[1:]...)
	}
	buf := l.Entry
	l.Entry = buf[:0]
	var entry []byte
	valid := false
	if len(stored) > 0 {
		var kb [keys.CachePartitionLen]byte
		var fb [keys.FieldLen]byte
		entry, valid = d.mac.OpenPayload(keys.AppendCachePartition(kb[:0], l.Key), keys.AppendField(fb[:0], keys.FieldEntry, l.Variant), mark, buf)
	}
	if !valid {
		if d.macLog.allow(d.clk.Now()) {
			d.log.WarnContext(ctx, d.mac.Refusal(mark))
		}
		return nil
	}
	l.Found, l.Entry, l.Resident = true, entry, resident
	return nil
}

// errBadTime reports a TIME reply that is not two integers.
var errBadTime = errors.New("redis: malformed TIME reply")

// serverMs reads reply i, a TIME, as Unix milliseconds; a malformed reply
// is errBadTime, an error reply or connection error is returned as is.
func serverMs(tr *trip, i int) (int64, error) {
	m, _, err := tr.result(i)
	if err != nil {
		return 0, err
	}
	parts, err := m.ToArray()
	if err != nil || len(parts) != 2 {
		return 0, errBadTime
	}
	sec, err1 := parts[0].AsInt64()
	usec, err2 := parts[1].AsInt64()
	if err1 != nil || err2 != nil || sec < 0 || usec < 0 {
		return 0, errBadTime
	}
	return sec*1000 + usec/1000, nil
}

// optString returns a string reply, "" for null; false for another type.
func optString(m *rueidis.RedisMessage) (string, bool) {
	if m.IsNil() {
		return "", true
	}
	s, err := m.ToString()
	return s, err == nil
}
