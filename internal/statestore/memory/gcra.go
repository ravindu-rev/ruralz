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
// in microseconds, computed as the Node sends them to the script
// (spec 08 req 41): T = window_us / requests, τ = burst × window_us /
// requests. ok is false for a limit the script would reject.
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

// gcra runs sub-operation g, GCRA virtual scheduling over every limit of
// one Policy, all or nothing (spec 08 req 41). The caller holds s.mu.
// False is an error reply (RZ-STS-002): an invalid limit, an Out shorter
// than Limits, or a full shard.
func (d *Driver) gcra(s *shard, g *statestore.GCRA, now instant) bool {
	if len(g.Out) < len(g.Limits) {
		return false
	}
	// Every TAT is read before any is written, as the script does.
	var buf [stackLimits]float64
	tats := buf[:0]
	allowed, denied, retry, fresh := true, -1, 0.0, 0
	for i, l := range g.Limits {
		_, tau, ok := gcraParams(l)
		if !ok {
			return false
		}
		tat, live := tatOf(s, rlKey{g.Policy, l.Requests, l.Window, g.Digest}, now)
		tats = append(tats, tat)
		if !live {
			fresh++
		}
		if tat-now.us > tau {
			allowed = false
			// The limit with the largest retry-after sets it; the lowest
			// index wins ties.
			if r := tat - tau - now.us; denied < 0 || r > retry {
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
			x = tat + t
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

// floorDiv is floor(a / b) for b > 0.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && a < 0 {
		q--
	}
	return q
}

// reserve runs sub-operation q, one Quota unit (spec 08 req 42). Windows
// are fixed and epoch-aligned: ws = now_ms - now_ms mod W on the server
// clock. The Node offers its own window A = floor(node_ms / W) × W and the
// nearer neighbor B (A - W when node_ms - A < W / 2, else A + W); the
// reservation charges whichever equals ws, else it is clock skew (skew
// true): an error reply the caller logs. The caller holds s.mu.
func (d *Driver) reserve(s *shard, q *statestore.Quota, now instant, nodeMs int64) (ok, skew bool) {
	w := q.Window.Milliseconds()
	if w <= 0 {
		return false, false
	}
	a := floorDiv(nodeMs, w) * w
	b := a + w
	if 2*(nodeMs-a) < w {
		b = a - w
	}
	ws := floorDiv(now.ms, w) * w
	if ws != a && ws != b {
		return false, true
	}
	k := qtKey{q.Name, q.Window, ws, q.Digest}
	e, live := s.qt[k]
	if live && expired(e.exp, now.ms) {
		e, live = countEntry{}, false
	}
	q.WindowStart = time.UnixMilli(ws).UTC()
	q.ServerNow = time.UnixMilli(now.ms).UTC()
	if e.n >= q.Limit {
		q.Allowed, q.Used = false, e.n
		q.RetryAfter = time.Duration(ws+w-now.ms) * time.Millisecond
		return true, false
	}
	if !live && !d.room(s, 1, now.ms) {
		return false, false
	}
	e.n++
	if e.n == 1 {
		// PEXPIREAT ws + 2W: a window after the window closes.
		e.exp = ws + 2*w
		s.track(e.exp)
	}
	s.qt[k] = e
	q.Allowed, q.Used, q.RetryAfter = true, e.n, 0
	return true, false
}

// refund runs sub-operation r (spec 08 req 43): on the exact key the
// reservation charged, one unit back if the count is above 0; it never
// creates a key and never goes below 0. It reports whether it
// decremented.
func (d *Driver) refund(r *statestore.Refund, now instant) bool {
	k := qtKey{r.Name, r.Window, r.WindowStart.UnixMilli(), r.Digest}
	s := d.shardOf(int(keys.DigestSlot(r.Digest)))
	s.mu.Lock()
	defer s.mu.Unlock()
	e, held := s.qt[k]
	if !held {
		return false
	}
	if expired(e.exp, now.ms) {
		delete(s.qt, k)
		return false
	}
	if e.n <= 0 {
		return false
	}
	e.n--
	s.qt[k] = e
	return true
}
