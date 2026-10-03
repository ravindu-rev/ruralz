// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package statestoretest

import (
	"math"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Epoch2026 is 2026-01-01T00:00:00Z, the GCRA time base of spec 08 req
// 41: a TAT is microseconds since it, as an exact float64.
const Epoch2026 = 1767225600

// Model is the reference arithmetic of the ruralz_v1.lua sub-operations
// g (GCRA), q (Quota reserve) and r (Quota refund), spec 08 reqs 41 to
// 43, written for clarity rather than speed. Drivers must agree with it
// exactly for identical inputs (spec 08 req 46): the conformance suite
// feeds it the ServerNow each reply reports and compares every outcome,
// and property tests drive it with a fake clock.
//
// Two choices the spec leaves open are fixed here, and the drivers follow
// them: a denial names the non-conforming limit with the largest raw
// retry-after (TAT - τ - now before rounding up), the lowest index on
// ties; the Quota window candidates compare 2 × (node_ms - A) < W, which
// is node_ms - A < W / 2 without integer division.
//
// The Model keeps no TTLs: an expired TAT is always at or before now - τ,
// so it decides exactly as an absent one, and a Quota counter is only
// read inside its own window. A Model is not safe for concurrent use.
type Model struct {
	tats   map[ModelLimit]float64
	counts map[ModelWindow]int64
}

// ModelLimit names one GCRA key: rz:rl:<policy>:<requests>/<window>:{<digest>}.
type ModelLimit struct {
	Policy   string
	Requests int64
	Window   time.Duration
	Digest   statestore.Digest
}

// ModelWindow names one Quota counter: rz:qt:<name>:<window>:<start>:{<digest>}.
type ModelWindow struct {
	Name   string
	Window time.Duration
	Start  int64 // Unix milliseconds
	Digest statestore.Digest
}

// NewModel returns an empty model.
func NewModel() *Model {
	return &Model{tats: map[ModelLimit]float64{}, counts: map[ModelWindow]int64{}}
}

// GCRAMicros returns t as the script's time base: microseconds since the
// 2026 epoch, truncated to the microsecond as Redis TIME reports it.
func GCRAMicros(t time.Time) float64 {
	return float64((t.Unix()-Epoch2026)*1_000_000 + int64(t.Nanosecond()/1000))
}

// GCRAParams returns one limit's emission interval T and tolerance τ in
// microseconds as the Node computes them: T = window_us / requests,
// τ = burst × window_us / requests. ok is false for a limit a driver
// rejects with RZ-STS-002.
func GCRAParams(l statestore.GCRALimit) (t, tau float64, ok bool) {
	w := float64(l.Window.Microseconds())
	if l.Requests <= 0 || l.Burst < 0 || w <= 0 {
		return 0, 0, false
	}
	return w / float64(l.Requests), float64(l.Burst) * w / float64(l.Requests), true
}

// GCRA decides g at server time now like sub-operation g, fills its
// outcome fields (Out must have len(Limits)) and, on allow, advances
// every TAT. It returns false where a driver answers RZ-STS-002.
func (m *Model) GCRA(now time.Time, g *statestore.GCRA) bool {
	if len(g.Out) < len(g.Limits) {
		return false
	}
	n := GCRAMicros(now)
	type limit struct {
		key         ModelLimit
		t, tau, tat float64
	}
	ls := make([]limit, len(g.Limits))
	for i, l := range g.Limits {
		t, tau, ok := GCRAParams(l)
		if !ok {
			return false
		}
		key := ModelLimit{g.Policy, l.Requests, l.Window, g.Digest}
		tat := n
		if stored, ok := m.tats[key]; ok {
			tat = math.Max(stored, n)
		}
		ls[i] = limit{key, t, tau, tat}
	}
	g.Allowed, g.Denied, g.RetryAfter = true, -1, 0
	worst := math.Inf(-1)
	for i, l := range ls {
		if l.tat-n > l.tau {
			g.Allowed = false
			if r := l.tat - l.tau - n; r > worst {
				worst, g.Denied = r, i
			}
		}
	}
	if !g.Allowed {
		g.RetryAfter = time.Duration(math.Ceil(worst)) * time.Microsecond
	}
	for i, l := range ls {
		x := l.tat
		if g.Allowed {
			x = l.tat + l.t
			m.tats[l.key] = x
		}
		var remaining float64
		if l.tat-n <= l.tau {
			remaining = math.Max(0, math.Floor((l.tau-(x-n))/l.t)+1)
		}
		g.Out[i] = statestore.GCRAOutcome{
			Remaining:  int64(remaining),
			ResetAfter: time.Duration(math.Ceil(x-n)) * time.Microsecond,
		}
	}
	g.ServerNow = time.UnixMicro(Epoch2026*1_000_000 + int64(n)).UTC()
	return true
}

// TAT returns the stored TAT of one limit, microseconds since the 2026
// epoch.
func (m *Model) TAT(policy string, l statestore.GCRALimit, d statestore.Digest) (float64, bool) {
	v, ok := m.tats[ModelLimit{policy, l.Requests, l.Window, d}]
	return v, ok
}

// QuotaWindows returns the Node's two candidate window starts in Unix
// milliseconds for window w: its own window A and the nearer neighbor B.
func QuotaWindows(node time.Time, w time.Duration) (a, b int64) {
	ms, wm := node.UnixMilli(), w.Milliseconds()
	a = ms - ms%wm
	if ms%wm < 0 {
		a -= wm
	}
	if 2*(ms-a) < wm {
		return a, a - wm
	}
	return a, a + wm
}

// Quota reserves one unit like sub-operation q at server time now, with
// the window candidates the Node computes from its clock node. skew is
// true when neither candidate is the server's window: the driver fails
// the call with RZ-STS-002. ok is false for a window under 1 ms.
func (m *Model) Quota(node, now time.Time, q *statestore.Quota) (ok, skew bool) {
	wm := q.Window.Milliseconds()
	if wm <= 0 {
		return false, false
	}
	nowMs := now.UnixMilli()
	ws := nowMs - nowMs%wm
	if a, b := QuotaWindows(node, q.Window); ws != a && ws != b {
		return false, true
	}
	key := ModelWindow{q.Name, q.Window, ws, q.Digest}
	c := m.counts[key]
	q.WindowStart, q.ServerNow = time.UnixMilli(ws).UTC(), time.UnixMilli(nowMs).UTC()
	if c >= q.Limit {
		q.Allowed, q.Used = false, c
		q.RetryAfter = time.Duration(ws+wm-nowMs) * time.Millisecond
		return true, false
	}
	c++
	m.counts[key] = c
	q.Allowed, q.Used, q.RetryAfter = true, c, 0
	return true, false
}

// Refund returns one unit to the window r names, never below 0; it
// reports whether it decremented (spec 08 req 43).
func (m *Model) Refund(r statestore.Refund) bool {
	key := ModelWindow{r.Name, r.Window, r.WindowStart.UnixMilli(), r.Digest}
	if m.counts[key] <= 0 {
		return false
	}
	m.counts[key]--
	return true
}

// Count returns the counter of one Quota window.
func (m *Model) Count(name string, window time.Duration, start time.Time, d statestore.Digest) int64 {
	return m.counts[ModelWindow{name, window, start.UnixMilli(), d}]
}
