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

// Write sends one post-commit batch as one pipelined round trip: refunds,
// cache stores, lease takes, refreshes and ends, and invalidations. A
// write whose timeout is 0 or whose context ended is not attempted
// (RZ-STS-004), one to a shard whose breaker is open is skipped
// (RZ-STS-003) without a call; the per-request deadline does not apply.
// The round trip's timeout is the smallest of the attempted writes'. It is
// labeled with the write's op, or pipeline when several were attempted.
func (d *Driver) Write(ctx context.Context, batch []*statestore.Write) {
	skip := func(w *statestore.Write, err *statestore.Error) {
		w.Err = err
		d.count(0, w.Kind.Label(), err.Result())
	}
	var sbuf [8]*shard
	shs := sbuf[:0]
	var timeout time.Duration
	for _, w := range batch {
		w.Err, w.Applied = nil, false
		if ctx.Err() != nil || w.Timeout <= 0 {
			skip(w, statestore.ErrNotAttempted)
			continue
		}
		if d.closed.Load() {
			skip(w, statestore.ErrFailed)
			continue
		}
		sh, script, ok := d.writeShard(w)
		switch {
		case !ok:
			skip(w, statestore.ErrFailed)
			continue
		case script && sh.noScripts.Load():
			skip(w, statestore.ErrUnsupported)
			continue
		case sh.brk.State() == breaker.Open:
			skip(w, statestore.ErrBreakerOpen)
			continue
		}
		shs = addShard(shs, sh)
		if timeout == 0 || w.Timeout < timeout {
			timeout = w.Timeout
		}
	}
	if len(shs) == 0 {
		return
	}
	tr := d.getTrip()
	tr.setShards(shs)
	if err := d.admit(tr); err != nil {
		d.putTrip(tr)
		for _, w := range batch {
			if w.Err == nil {
				skip(w, err)
			}
		}
		return
	}
	sent, label := d.encodeWrites(tr, batch, skip)
	tr.cancelUnused()
	if sent == 0 {
		d.gate.Release()
		d.putTrip(tr)
		return
	}
	if !d.launch(ctx, tr) {
		d.putTrip(tr)
		for _, w := range batch {
			if w.Err == nil {
				skip(w, statestore.ErrFailed)
			}
		}
		return
	}
	if sent > 1 {
		label = emit.StateOpPipeline
	}
	res := statestore.ResultOK
	out, elapsed := tr.wait(ctx, timeout)
	switch out {
	case timedOut, canceled:
		for _, w := range batch {
			if w.Err == nil {
				w.Err = statestore.ErrTimeout
				d.countOps(0, w.Kind.Label(), 1)
			}
		}
		res = statestore.ResultTimeout
	case replied:
		i := 0
		for _, w := range batch {
			if w.Err != nil {
				continue
			}
			d.countOps(0, w.Kind.Label(), 1)
			if err := d.parseWrite(ctx, tr, i, w); err != nil {
				w.Err, res = err, statestore.ResultError
			}
			i++
		}
		tr.settle(true)
		d.putTrip(tr)
	}
	d.count(0, label, res)
	d.observe(0, label, elapsed)
}

// writeShard returns the shard of w's keys and whether a script writes
// it; ok is false for a write of an unknown kind or lease mode.
func (d *Driver) writeShard(w *statestore.Write) (sh *shard, script, ok bool) {
	slot := keys.WriteSlot(w)
	if slot < 0 {
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

// encodeWrites writes the command of every write still pending whose
// shard admitted the trip, skips the others with RZ-STS-003, and returns
// how many it wrote and the op label of the last.
func (d *Driver) encodeWrites(tr *trip, batch []*statestore.Write, skip func(*statestore.Write, *statestore.Error)) (sent, label int) {
	label = -1
	for _, w := range batch {
		if w.Err != nil {
			continue
		}
		sh, _, _ := d.writeShard(w)
		k := tr.ticketOf(sh)
		if tr.tickets[k].refused {
			skip(w, statestore.ErrBreakerOpen)
			continue
		}
		d.encodeWrite(tr, k, w)
		sent++
		label = w.Kind.Label()
	}
	return sent, label
}

// encodeWrite writes one write's command for ticket k.
func (d *Driver) encodeWrite(tr *trip, k int, w *statestore.Write) {
	var kb, lb [keys.CachePartitionLen + len(keys.SuffixLease)]byte
	var hb [keys.HexLen]byte
	switch w.Kind {
	case statestore.OpRefund:
		r := &w.Refund
		var qb [512]byte
		tr.cmd(k, "EVALSHA", d.sha, "1")
		tr.key(string(keys.AppendQuota(qb[:0], r.Name, r.Window, r.WindowStart, r.Digest)))
		tr.arg(opRefund)
	case statestore.OpCacheInvalidate:
		tr.cmd(k, "EVALSHA", d.sha, "1")
		tr.key(string(keys.AppendCacheGeneration(kb[:0], w.Invalidate)))
		tr.arg(opInvalidate)
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
			tr.arg(strconv.FormatInt(s.Generation, 10))
			tr.arg(strconv.FormatInt(entryTTL(s.TTL), 10))
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
			return
		}
		tr.arg(opEndStale)
		tr.arg(strconv.FormatUint(l.Token, 10))
		tr.arg(string(keys.AppendHex(hb[:0], l.Variant)))
	}
}

// seal returns the stored form of entry under k's partition and variant
// field e:<V>: the entry, then its MAC tag when a key is set.
func (d *Driver) seal(k statestore.CacheKey, v statestore.Digest, entry []byte) string {
	if d.mac == nil {
		return string(entry)
	}
	var kb [keys.CachePartitionLen]byte
	var fb [keys.FieldLen]byte
	dst := make([]byte, 0, len(entry)+keys.TagSize)
	return string(d.mac.Seal(dst, keys.AppendCachePartition(kb[:0], k), keys.AppendField(fb[:0], keys.FieldEntry, v), entry))
}

// parseWrite reads reply i of write w: 1 or OK applied, 0 or null not
// applied; anything else is unparsable.
func (d *Driver) parseWrite(ctx context.Context, tr *trip, i int, w *statestore.Write) *statestore.Error {
	m, null, err := tr.result(i)
	if err != nil {
		return tr.failure(ctx, i, err)
	}
	if w.Kind == statestore.OpCacheSet && w.Lease.Mode == statestore.LeaseTake {
		if null {
			return nil
		}
		if s, err := m.ToString(); err == nil && s == "OK" {
			w.Applied = true
			return nil
		}
		tr.fail(i)
		return statestore.ErrFailed
	}
	n, err := m.ToInt64()
	if null || err != nil || (n != 0 && n != 1) {
		tr.fail(i)
		return statestore.ErrFailed
	}
	w.Applied = n == 1
	return nil
}
