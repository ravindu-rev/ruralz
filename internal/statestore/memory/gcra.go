// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"math"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
)

// gcraParams returns the emission interval T and tolerance τ of one limit
// in microseconds, computed as the Node sends them to the script: T =
// window_us / requests, τ = burst × window_us / requests. ok is false for
// a limit the script would reject.
func gcraParams(l statestore.GCRALimit) (t, tau float64, ok bool) {
	wus := float64(l.Window.Microseconds())
	if l.Requests <= 0 || l.Burst < 0 || wus <= 0 {
		return 0, 0, false
	}
	req := float64(l.Requests)
	return wus / req, float64(l.Burst) * wus / req, true
}

// stackLimits is how many limits' TATs gcra keeps on the stack between
// its passes; more limits allocate.
const stackLimits = 16

// maxCount caps float counts converted to int64.
const maxCount = 1 << 62

// toInt64 converts a non-negative float count, saturating.
func toInt64(f float64) int64 {
	if f >= maxCount {
		return maxCount
	}
	return int64(f)
}

// micros converts a whole number of microseconds to a Duration.
func micros(us float64) time.Duration { return time.Duration(toInt64(us)) * time.Microsecond }

// tatOf returns max(stored TAT or now, now) of k and whether k is live.
// An expired key counts as absent, and as a new key for the shard bound:
// room may reclaim it before it is written again.
func tatOf(s *shard, k rlKey, now instant) (tat float64, live bool) {
	e, held := s.rl[k]
	live = held && !expired(e.exp, now.ms)
	if live && e.tat > now.us {
		return e.tat, true
	}
	return now.us, live
}

// gcraSlack returns how far TAT may lead now for cost units of a limit
// to conform, τ - (cost - 1) × T, rounded as the script rounds it: the
// explicit conversion keeps the product from fusing into the subtraction.
func gcraSlack(t, tau float64, cost int64) float64 { return tau - float64(float64(cost-1)*t) }

// gcraAdvance returns the TAT after an allow charging cost units,
// TAT + cost × T, rounded as the script rounds it.
func gcraAdvance(tat, t float64, cost int64) float64 { return tat + float64(float64(cost)*t) }

// gcra runs sub-operation g, GCRA virtual scheduling over every limit of
// one Policy, all or nothing, charging g.Cost emission intervals. The
// caller holds s.mu. False is an error reply (RZ-STS-002): an invalid
// limit or cost, an Out shorter than Limits, or a full shard.
func (d *Driver) gcra(s *shard, g *statestore.GCRA, now instant) bool {
	cost, ok := statestore.Cost(g.Cost)
	if len(g.Out) < len(g.Limits) || !ok {
		return false
	}
	// Every TAT is read before any is written, as the script does.
	var buf [stackLimits]float64
	tats := buf[:0]
	allowed, denied, retry, fresh := true, -1, 0.0, 0
	for i, l := range g.Limits {
		t, tau, ok := gcraParams(l)
		if !ok {
			return false
		}
		tat, live := tatOf(s, rlKey{g.Policy, l.Requests, l.Window, g.Digest}, now)
		tats = append(tats, tat)
		if !live {
			fresh++
		}
		if slack := gcraSlack(t, tau, cost); tat-now.us > slack {
			allowed = false
			// The limit with the largest retry-after sets it; the lowest
			// index wins ties.
			if r := tat - slack - now.us; denied < 0 || r > retry {
				denied, retry = i, r
			}
		}
	}
	if allowed && !d.room(s, fresh, now.ms) {
		return false
	}
	for i, l := range g.Limits {
		t, tau, _ := gcraParams(l)
		k := rlKey{g.Policy, l.Requests, l.Window, g.Digest}
		tat := tats[i]
		x, conforms := tat, tat-now.us <= tau
		if allowed {
			x = gcraAdvance(tat, t, cost)
			// PX max(1, ceil((new - now + τ) / 1000)): TAT - now + τ.
			px := max(1, int64(math.Ceil((x-now.us+tau)/1000)))
			s.rl[k] = tatEntry{tat: x, exp: now.ms + px}
			s.track(now.ms + px)
		}
		var remaining int64
		if conforms {
			remaining = toInt64(max(0, math.Floor((tau-(x-now.us))/t)+1))
		}
		g.Out[i] = statestore.GCRAOutcome{Remaining: remaining, ResetAfter: micros(math.Ceil(x - now.us))}
	}
	g.Allowed = allowed
	g.Denied, g.RetryAfter = -1, 0
	if !allowed {
		g.Denied, g.RetryAfter = denied, micros(math.Ceil(retry))
	}
	g.ServerNow = time.UnixMicro(epoch2026*1_000_000 + int64(now.us)).UTC()
	return true
}

// quotaKey returns the counter key of q's window starting at start: a
// calendar month carries no window length.
func quotaKey(name string, window time.Duration, month bool, start int64, d statestore.Digest) qtKey {
	if month {
		window = 0
	}
	return qtKey{name: name, window: window, month: month, start: start, digest: d}
}

// reserve runs sub-operation q, Cost Quota units, all or none. The Node
// offers its own window A and the nearer neighbor B
// (statestore.QuotaCandidates), each an epoch-aligned window or a UTC
// calendar month; the reservation charges the candidate holding the
// server clock's now_ms, else it is clock skew (skew true): an error
// reply the caller logs. A count plus Cost above Limit denies, reserving
// nothing. The caller holds s.mu.
func (d *Driver) reserve(s *shard, q *statestore.Quota, now instant, nodeMs int64) (ok, skew bool) {
	cost, valid := statestore.Cost(q.Cost)
	if !valid || q.Limit < 0 || (!q.Month && q.Window.Milliseconds() <= 0) {
		return false, false
	}
	a, b := statestore.QuotaCandidates(nodeMs, q.Window, q.Month)
	var win statestore.QuotaWindow
	switch {
	case a.Start <= now.ms && now.ms < a.End:
		win = a
	case b.Start <= now.ms && now.ms < b.End:
		win = b
	default:
		return false, true
	}
	k := quotaKey(q.Name, q.Window, q.Month, win.Start, q.Digest)
	e, live := s.qt[k]
	if live && expired(e.exp, now.ms) {
		e, live = countEntry{}, false
	}
	q.WindowStart, q.WindowEnd = time.UnixMilli(win.Start).UTC(), time.UnixMilli(win.End).UTC()
	q.ServerNow = time.UnixMilli(now.ms).UTC()
	if cost > q.Limit-e.n {
		q.Allowed, q.Used = false, e.n
		q.RetryAfter = time.Duration(win.End-now.ms) * time.Millisecond
		return true, false
	}
	if !live && !d.room(s, 1, now.ms) {
		return false, false
	}
	e.n += cost
	if !live {
		// PEXPIREAT end + (end - start): a window after the window closes.
		e.exp = win.End + (win.End - win.Start)
		s.track(e.exp)
	}
	s.qt[k] = e
	q.Allowed, q.Used, q.RetryAfter = true, e.n, 0
	return true, false
}

// refund runs sub-operation r: on the exact key the reservation
// charged, its Units back, at most the count; it never creates a key and
// never goes below 0. It reports whether it decremented; valid is false
// for negative Units, an error reply.
func (d *Driver) refund(r *statestore.Refund, now instant) (applied, valid bool) {
	units, valid := statestore.Cost(r.Units)
	if !valid {
		return false, false
	}
	k := quotaKey(r.Name, r.Window, r.Month, r.WindowStart.UnixMilli(), r.Digest)
	s := d.shardOf(int(keys.DigestSlot(r.Digest)))
	s.mu.Lock()
	defer s.mu.Unlock()
	e, held := s.qt[k]
	if !held {
		return false, true
	}
	if expired(e.exp, now.ms) {
		delete(s.qt, k)
		return false, true
	}
	if e.n <= 0 {
		return false, true
	}
	e.n -= min(units, e.n)
	s.qt[k] = e
	return true, true
}
