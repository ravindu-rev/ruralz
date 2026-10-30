// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/breaker"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Response Cache targets the driver writes
// (docs/architecture/09-traffic-management-and-resilience.md "Response
// caching"); the library holds the fill lease (1 s) and the generation
// TTL (26 h).
const (
	// maxPartitionTTLMs is a partition's longest life, 25 h.
	maxPartitionTTLMs = 90_000_000
	// revalidateLease is the revalidation lease, SET NX PX 5000.
	revalidateLease = "5000"
)

// ceilMs returns d in milliseconds, rounded up.
func ceilMs(d time.Duration) int64 {
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

// entryTTL is a store's TTL in milliseconds within [1 ms, 25 h]: no key is
// ever written without a TTL.
func entryTTL(d time.Duration) int64 { return min(max(ceilMs(d), 1), maxPartitionTTLMs) }

// Write steps. A refund, an invalidation or a lease take is one command
// (stepOne); a store, refresh or end-stale is two, the generation step on
// the URI's generation key and, only when it applied, the partition step
// on the partition's shard (statestore.CacheStore): the two keys hash to
// different Redis Cluster slots, so one script cannot hold both. stepDone
// marks a write with nothing left to send.
const (
	stepDone uint8 = iota
	stepOne
	stepGeneration
	stepPartition
)

// twoStep reports a write that runs a generation step first: a store, a
// refresh or an end-stale.
func twoStep(w *statestore.Write) bool {
	return w.Kind == statestore.OpCacheSet && w.Lease.Mode != statestore.LeaseTake
}

// Write sends one post-commit batch: refunds, cache stores, lease takes,
// refreshes and ends, and invalidations, as one pipelined round trip, then
// a second one for the partition steps of the two-step writes whose
// generation step applied. A write whose timeout is 0 or whose context
// ended is not attempted (RZ-STS-004), one to a shard whose breaker is
// open is skipped (RZ-STS-003) without a call; the per-request deadline
// does not apply. Both round trips together wait at most the smallest of
// the attempted writes' timeouts; a partition step with no time left is
// not attempted. Each round trip is labeled with its write's op, or
// pipeline when it carried several commands; a write counts once in
// ruralz_state_ops_total.
func (d *Driver) Write(ctx context.Context, batch []*statestore.Write) {
	var sbuf [64]uint8
	steps := sbuf[:0]
	if len(batch) > len(sbuf) {
		steps = make([]uint8, 0, len(batch))
	}
	var timeout time.Duration
	for _, w := range batch {
		w.Err, w.Applied = nil, false
		st := stepOne
		if twoStep(w) {
			st = stepGeneration
		}
		steps = append(steps, st)
		switch {
		case ctx.Err() != nil || w.Timeout <= 0:
			d.skipWrite(w, statestore.ErrNotAttempted)
		case d.closed.Load():
			d.skipWrite(w, statestore.ErrFailed)
		case timeout == 0 || w.Timeout < timeout:
			timeout = w.Timeout
		}
	}
	if timeout == 0 {
		return
	}
	start := d.clk.Now()
	if !d.writeRound(ctx, batch, steps, timeout, true) {
		return
	}
	if left := timeout - d.clk.Since(start); left > 0 {
		d.writeRound(ctx, batch, steps, left, false)
		return
	}
	for i, w := range batch {
		if steps[i] == stepPartition && w.Err == nil {
			d.skipWrite(w, statestore.ErrNotAttempted)
		}
	}
}

// skipWrite fails w with err without a call.
func (d *Driver) skipWrite(w *statestore.Write, err *statestore.Error) {
	w.Err = err
	d.count(0, w.Kind.Label(), err.Result())
}

// inRound reports whether a write at step st sends a command in the first
// round trip (its only command or its generation step) or in the second
// (its partition step).
func inRound(st uint8, first bool) bool {
	if first {
		return st == stepOne || st == stepGeneration
	}
	return st == stepPartition
}

// writeRound sends one round trip of batch's writes that are in it (each
// one command, inRound) and applies the replies within timeout. It reports
// whether a generation step applied, leaving its write's partition step
// for the second round trip.
func (d *Driver) writeRound(ctx context.Context, batch []*statestore.Write, steps []uint8, timeout time.Duration, first bool) (pending bool) {
	var sbuf [8]*shard
	shs := sbuf[:0]
	for i, w := range batch {
		if w.Err != nil || !inRound(steps[i], first) {
			continue
		}
		sh, script, ok := d.stepShard(w, steps[i])
		switch {
		case !ok:
			d.skipWrite(w, statestore.ErrFailed)
			continue
		case script && sh.noScripts.Load():
			d.skipWrite(w, statestore.ErrUnsupported)
			continue
		case sh.brk.State() == breaker.Open:
			d.skipWrite(w, statestore.ErrBreakerOpen)
			continue
		}
		shs = addShard(shs, sh)
	}
	if len(shs) == 0 {
		return false
	}
	skipRound := func(err *statestore.Error) {
		for i, w := range batch {
			if w.Err == nil && inRound(steps[i], first) {
				d.skipWrite(w, err)
			}
		}
	}
	tr := d.getTrip()
	tr.setShards(shs)
	if err := d.admit(tr); err != nil {
		d.putTrip(tr)
		skipRound(err)
		return false
	}
	sent, label := d.encodeWrites(tr, batch, steps, first)
	tr.cancelUnused()
	if sent == 0 {
		d.gate.Release()
		d.putTrip(tr)
		return false
	}
	zone := d.tripZone(tr)
	if !d.launch(ctx, tr) {
		d.putTrip(tr)
		skipRound(statestore.ErrFailed)
		return false
	}
	if sent > 1 {
		label = emit.StateOpPipeline
	}
	res := statestore.ResultOK
	out, elapsed := tr.wait(ctx, timeout)
	switch out {
	case timedOut, canceled:
		for i, w := range batch {
			if w.Err == nil && inRound(steps[i], first) {
				w.Err = statestore.ErrTimeout
				if first {
					d.countOps(0, w.Kind.Label(), 1)
				}
			}
		}
		res = statestore.ResultTimeout
	case replied:
		k := 0
		for i, w := range batch {
			if w.Err != nil || !inRound(steps[i], first) {
				continue
			}
			if first {
				d.countOps(0, w.Kind.Label(), 1)
			}
			applied, err := d.parseWrite(ctx, tr, k, w, steps[i])
			k++
			switch {
			case err != nil:
				w.Err, res = err, statestore.ResultError
			case steps[i] == stepGeneration && applied:
				steps[i], pending = stepPartition, true
				continue
			default:
				w.Applied = applied
			}
			steps[i] = stepDone
		}
		tr.settle(true)
		d.putTrip(tr)
	}
	d.count(0, label, res)
	d.observe(0, label, zone, elapsed)
	return pending
}

// stepShard returns the shard of w's command at step st and whether a
// script writes it: the generation key's shard for a generation step, else
// writeShard's. ok is false for a write of an unknown kind or lease mode.
func (d *Driver) stepShard(w *statestore.Write, st uint8) (sh *shard, script, ok bool) {
	if st != stepGeneration {
		return d.writeShard(w)
	}
	switch w.Lease.Mode {
	case 0:
		return d.shardOfSlot(keys.DigestSlot(w.Cache.Key.URI)), true, true
	case statestore.LeaseRefresh, statestore.LeaseEndStale:
		return d.shardOfSlot(keys.DigestSlot(w.Lease.Key.URI)), true, true
	default:
		return nil, false, false
	}
}

// writeShard returns the shard of w's keys and whether a script writes
// it; ok is false for a write of an unknown kind or lease mode, or a
// refund of negative units.
func (d *Driver) writeShard(w *statestore.Write) (sh *shard, script, ok bool) {
	slot := keys.WriteSlot(w)
	if slot < 0 {
		return nil, false, false
	}
	if _, valid := statestore.Cost(w.Refund.Units); w.Kind == statestore.OpRefund && !valid {
		return nil, false, false
	}
	script = true
	if w.Kind == statestore.OpCacheSet {
		switch w.Lease.Mode {
		case 0, statestore.LeaseRefresh, statestore.LeaseEndStale:
		case statestore.LeaseTake:
			script = false
		default:
			return nil, false, false
		}
	}
	return d.shardOfSlot(uint16(slot)), script, true //nolint:gosec // G115: a slot below 16384
}

// encodeWrites writes the command of every write in the round whose shard
// admitted the trip, skips the others with RZ-STS-003, and returns how
// many it wrote and the op label of the last.
func (d *Driver) encodeWrites(tr *trip, batch []*statestore.Write, steps []uint8, first bool) (sent, label int) {
	label = -1
	for i, w := range batch {
		if w.Err != nil || !inRound(steps[i], first) {
			continue
		}
		sh, _, _ := d.stepShard(w, steps[i])
		k := tr.ticketOf(sh)
		if tr.tickets[k].refused {
			d.skipWrite(w, statestore.ErrBreakerOpen)
			continue
		}
		if steps[i] == stepGeneration {
			d.encodeGeneration(tr, k, w)
		} else {
			d.encodeWrite(tr, k, w)
		}
		sent++
		label = w.Kind.Label()
	}
	return sent, label
}

// encodeGeneration writes the generation step of a store (create when its
// lookup found no generation key, else hold), a refresh (hold) or an
// end-stale (check) for ticket k.
func (d *Driver) encodeGeneration(tr *trip, k int, w *statestore.Write) {
	var kb [keys.CacheGenerationLen]byte
	var tb [keys.TokenLen]byte
	uri, tok, mode := w.Lease.Key.URI, w.Lease.Generation, genHold
	switch w.Lease.Mode {
	case 0:
		uri, tok = w.Cache.Key.URI, w.Cache.Generation
		if w.Cache.Create {
			mode = genCreate
		}
	case statestore.LeaseEndStale:
		mode = genCheck
	}
	tr.cmd(k, "EVALSHA", d.sha, "1")
	tr.key(string(keys.AppendCacheGeneration(kb[:0], uri)))
	tr.arg(opGeneration)
	tr.arg(mode)
	tr.arg(string(keys.AppendToken(tb[:0], tok)))
}

// delayArg returns the receipt delay of a response that arrived at
// received, in whole milliseconds as text: the store time t:<V> is the
// script's TIME less it.
func (d *Driver) delayArg(received time.Time) string {
	return strconv.FormatInt(int64(statestore.ReceiptDelay(received, d.clk.Now())/time.Millisecond), 10)
}

// encodeWrite writes the only command, or the partition step, of a write
// for ticket k.
func (d *Driver) encodeWrite(tr *trip, k int, w *statestore.Write) {
	var kb, lb [keys.CachePartitionLen + len(keys.SuffixLease)]byte
	var hb [keys.HexLen]byte
	switch w.Kind {
	case statestore.OpRefund:
		r := &w.Refund
		units, _ := statestore.Cost(r.Units)
		var qb [512]byte
		tr.cmd(k, "EVALSHA", d.sha, "1")
		tr.key(string(keys.AppendQuotaCounter(qb[:0], r.Name, r.Window, r.Month, r.WindowStart, r.Digest)))
		tr.arg(opRefund)
		tr.arg(strconv.FormatInt(units, 10))
	case statestore.OpCacheInvalidate:
		var tb [keys.TokenLen]byte
		tr.cmd(k, "EVALSHA", d.sha, "1")
		tr.key(string(keys.AppendCacheGeneration(kb[:0], w.Invalidate)))
		tr.arg(opInvalidate)
		tr.arg(string(keys.AppendToken(tb[:0], w.Bump)))
	default: // statestore.OpCacheSet: writeShard accepts no other kind
		l := &w.Lease
		if l.Mode == 0 {
			s := &w.Cache
			tr.cmd(k, "EVALSHA", d.sha, "2")
			tr.key(string(keys.AppendCachePartition(kb[:0], s.Key)))
			tr.key(string(keys.AppendCacheLease(lb[:0], s.Key)))
			tr.arg(opStore)
			tr.arg(strconv.FormatUint(s.Token, 10))
			tr.arg(s.Names)
			tr.arg(string(keys.AppendHex(hb[:0], s.Variant)))
			tr.arg(strconv.FormatInt(entryTTL(s.TTL), 10))
			tr.arg(d.delayArg(s.Received))
			tr.arg(d.seal(s.Key, s.Variant, s.Entry))
			return
		}
		lease := string(keys.AppendCacheLease(lb[:0], l.Key))
		if l.Mode == statestore.LeaseTake {
			tr.cmd(k, "SET")
			tr.key(lease)
			tr.arg(strconv.FormatUint(l.Token, 10))
			tr.arg("NX")
			tr.arg("PX")
			tr.arg(revalidateLease)
			return
		}
		tr.cmd(k, "EVALSHA", d.sha, "2")
		tr.key(string(keys.AppendCachePartition(kb[:0], l.Key)))
		tr.key(lease)
		if l.Mode == statestore.LeaseRefresh {
			ttl := int64(0)
			if l.TTL > 0 {
				ttl = entryTTL(l.TTL)
			}
			tr.arg(opRefresh)
			tr.arg(strconv.FormatUint(l.Token, 10))
			tr.arg(string(keys.AppendHex(hb[:0], l.Variant)))
			tr.arg(d.seal(l.Key, l.Variant, l.Entry))
			tr.arg(strconv.FormatInt(ttl, 10))
			tr.arg(d.delayArg(l.Received))
			return
		}
		tr.arg(opEndStale)
		tr.arg(strconv.FormatUint(l.Token, 10))
		tr.arg(string(keys.AppendHex(hb[:0], l.Variant)))
	}
}

// seal returns the stored form of entry under k's partition and variant
// field e:<V>: the tagged-entry mark, the entry, then its MAC tag when a
// key is set (keys.MAC.Seal).
func (d *Driver) seal(k statestore.CacheKey, v statestore.Digest, entry []byte) string {
	var kb [keys.CachePartitionLen]byte
	var fb [keys.FieldLen]byte
	dst := make([]byte, 0, d.mac.SealedLen(len(entry)))
	return string(d.mac.Seal(dst, keys.AppendCachePartition(kb[:0], k), keys.AppendField(fb[:0], keys.FieldEntry, v), entry))
}

// parseWrite reads reply i of w's command at step st: 1 or OK applied, 0
// or null not applied; anything else is unparsable.
func (d *Driver) parseWrite(ctx context.Context, tr *trip, i int, w *statestore.Write, st uint8) (bool, *statestore.Error) {
	m, null, err := tr.result(i)
	if err != nil {
		return false, tr.failure(ctx, i, err)
	}
	if st != stepGeneration && w.Kind == statestore.OpCacheSet && w.Lease.Mode == statestore.LeaseTake {
		if null {
			return false, nil
		}
		if s, err := m.ToString(); err == nil && s == "OK" {
			return true, nil
		}
		tr.fail(i)
		return false, statestore.ErrFailed
	}
	n, err := m.ToInt64()
	if null || err != nil || (n != 0 && n != 1) {
		tr.fail(i)
		return false, statestore.ErrFailed
	}
	return n == 1, nil
}
