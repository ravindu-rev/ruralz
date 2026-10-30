// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/redis/rueidis"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/breaker"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// epoch2026 is 2026-01-01T00:00:00Z in Unix seconds, the GCRA time base:
// microseconds since it stay exact in a float64.
const epoch2026 = 1767225600

// stackLimits is how many limits' TATs a GCRA outcome keeps on the stack;
// more limits allocate.
const stackLimits = 16

// maxCount caps float counts converted to int64.
const maxCount = 1 << 62

// instant is one server TIME reading in the units the library uses.
type instant struct {
	// us is GCRA time: microseconds since the 2026 epoch.
	us float64
	// ms is Unix milliseconds: Quota windows.
	ms int64
}

// instantOf reads TIME's seconds and microseconds as the library does.
func instantOf(sec, usec int64) instant {
	return instant{us: float64((sec-epoch2026)*1_000_000 + usec), ms: sec*1000 + usec/1000}
}

// gcraParams returns one limit's emission interval T and tolerance τ in
// microseconds, as the Node sends them: T = window_us / requests,
// τ = burst × window_us / requests. ok is false for a limit the library
// refuses.
func gcraParams(l statestore.GCRALimit) (t, tau float64, ok bool) {
	wus := float64(l.Window.Microseconds())
	if l.Requests <= 0 || l.Burst < 0 || wus <= 0 {
		return 0, 0, false
	}
	req := float64(l.Requests)
	return wus / req, float64(l.Burst) * wus / req, true
}

// gcraSlack returns how far TAT may lead now for cost units of a limit
// to conform, τ - (cost - 1) × T, rounded as the library rounds it: the
// explicit conversion keeps the product from fusing into the subtraction.
func gcraSlack(t, tau float64, cost int64) float64 { return tau - float64(float64(cost-1)*t) }

// gcraAdvance returns the TAT after an allow charging cost units,
// TAT + cost × T, rounded as the library rounds it.
func gcraAdvance(tat, t float64, cost int64) float64 { return tat + float64(float64(cost)*t) }

// toInt64 converts a non-negative float count, saturating.
func toInt64(f float64) int64 {
	if f >= maxCount {
		return maxCount
	}
	return int64(f)
}

// micros converts a whole number of microseconds to a Duration.
func micros(us float64) time.Duration { return time.Duration(toInt64(us)) * time.Microsecond }

// floorDiv is floor(a / b) for b > 0.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && a < 0 {
		q--
	}
	return q
}

// quotaWindows returns the Node clock's window start A and its nearer
// neighbor B, in Unix milliseconds, for a window of w milliseconds.
func quotaWindows(nodeMs, w int64) (a, b int64) {
	a = floorDiv(nodeMs, w) * w
	if 2*(nodeMs-a) < w {
		return a, a - w
	}
	return a, a + w
}

// formatFloat writes f so a strtod reads it back exactly.
func formatFloat(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// Consume runs, as one EVALSHA, the longest prefix of calls whose keys
// share a hash slot, in order, stopping at the first deny: every call of
// the group is Done up to and including the deny, calls after it stay
// !Done, and a failure fails every call of the group. The round trip's
// timeout is the smallest of the group, clamped by the per-request
// deadline.
func (d *Driver) Consume(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) int {
	if len(calls) == 0 {
		return 0
	}
	first := calls[0]
	slot := keys.SlotOf(first)
	n := 1
	if slot >= 0 {
		for n < len(calls) && keys.SlotOf(calls[n]) == slot {
			n++
		}
	}
	group := calls[:n]
	rt, batch := statestore.TripSingle, -1
	if n > 1 {
		rt, batch = statestore.TripScriptMulti, 0
	}
	timeout := first.Timeout
	for _, c := range group {
		c.Done, c.Err, c.Elapsed, c.Batch, c.Trip = false, nil, 0, batch, rt
		timeout = min(timeout, c.Timeout)
	}
	stripe := stripeOf(rb)
	if slot < 0 {
		// Not a consumptive call: an error reply.
		first.Err = statestore.ErrFailed
		d.count(stripe, first.Kind.Label(), statestore.ResultError)
		return n
	}
	d.consume(ctx, rb, group, uint16(slot), timeout, rt.Label(first.Kind), stripe) //nolint:gosec // G115: a slot below 16384
	return n
}

// consume runs one consumptive group.
func (d *Driver) consume(ctx context.Context, rb *statestore.RequestBudget, group []*statestore.Call, slot uint16, timeout time.Duration, label int, stripe emit.Stripe) {
	skip := func(err *statestore.Error) {
		for _, c := range group {
			c.Err = err
			d.count(stripe, c.Kind.Label(), err.Result())
		}
	}
	sh := d.shardOfSlot(slot)
	if sh.noScripts.Load() {
		skip(statestore.ErrUnsupported)
		return
	}
	t, ok := d.callTimeout(ctx, rb, timeout)
	if !ok {
		skip(statestore.ErrNotAttempted)
		return
	}
	if d.closed.Load() {
		skip(statestore.ErrFailed)
		return
	}
	tr := d.getTrip()
	if !d.encodeConsume(tr, group) {
		d.putTrip(tr)
		for _, c := range group {
			c.Err = statestore.ErrFailed
		}
		d.count(stripe, label, statestore.ResultError)
		return
	}
	if sh.brk.State() == breaker.Open {
		d.putTrip(tr)
		skip(statestore.ErrBreakerOpen)
		return
	}
	shs := [1]*shard{sh}
	tr.setShards(shs[:])
	if err := d.admit(tr); err != nil {
		d.putTrip(tr)
		skip(err)
		return
	}
	if !d.launch(ctx, tr) {
		d.putTrip(tr)
		skip(statestore.ErrFailed)
		return
	}
	out, elapsed := tr.wait(ctx, t)
	if out != replied {
		for _, c := range group {
			c.Err, c.Elapsed = statestore.ErrTimeout, elapsed
		}
		d.finishConsume(stripe, label, group, statestore.ResultTimeout, elapsed)
		return
	}
	res := statestore.ResultOK
	m, _, err := tr.result(0)
	switch {
	case err != nil:
		d.logSkew(ctx, group, err)
		e := tr.failure(ctx, 0, err)
		for _, c := range group {
			c.Done, c.Err = false, e
		}
		res = statestore.ResultError
	case !d.parseConsume(m, group):
		tr.fail(0)
		for _, c := range group {
			c.Done, c.Err = false, statestore.ErrFailed
		}
		res = statestore.ResultError
	}
	for _, c := range group {
		c.Elapsed = elapsed
	}
	tr.settle(true)
	d.putTrip(tr)
	d.finishConsume(stripe, label, group, res, elapsed)
}

// finishConsume records one attempted consumptive round trip.
func (d *Driver) finishConsume(stripe emit.Stripe, label int, group []*statestore.Call, res statestore.Result, elapsed time.Duration) {
	d.count(stripe, label, res)
	d.observe(stripe, label, elapsed)
	for _, c := range group {
		d.countOps(stripe, c.Kind.Label(), 1)
	}
}

// encodeConsume writes the group's EVALSHA: KEYS are every limit's TAT
// key, then each Quota's two window keys (the Node clock's window and its
// nearer neighbor); ARGV is c, the call count, then per call g, the cost,
// the limit count and each limit's T and τ, or q, the limit, the window
// and the two window starts. False is a call the library would refuse.
func (d *Driver) encodeConsume(tr *trip, group []*statestore.Call) bool {
	nkeys := 0
	for _, c := range group {
		switch c.Kind {
		case statestore.OpGCRA:
			g := &c.GCRA
			if _, ok := statestore.GCRACost(g.Cost); !ok || len(g.Out) < len(g.Limits) {
				return false
			}
			for _, l := range g.Limits {
				if _, _, ok := gcraParams(l); !ok {
					return false
				}
			}
			nkeys += len(g.Limits)
		case statestore.OpQuota:
			if c.Quota.Window.Milliseconds() <= 0 {
				return false
			}
			nkeys += 2
		default:
			return false
		}
	}
	nodeMs := d.clk.Now().UnixMilli()
	tr.cmd(0, "EVALSHA", d.sha, strconv.Itoa(nkeys))
	var buf [512]byte
	for _, c := range group {
		if c.Kind == statestore.OpGCRA {
			g := &c.GCRA
			for _, l := range g.Limits {
				tr.key(string(keys.AppendRateLimit(buf[:0], g.Policy, l, g.Digest)))
			}
			continue
		}
		q := &c.Quota
		a, b := quotaWindows(nodeMs, q.Window.Milliseconds())
		tr.key(string(keys.AppendQuota(buf[:0], q.Name, q.Window, time.UnixMilli(a), q.Digest)))
		tr.key(string(keys.AppendQuota(buf[:0], q.Name, q.Window, time.UnixMilli(b), q.Digest)))
	}
	tr.arg(opConsume)
	tr.arg(strconv.Itoa(len(group)))
	for _, c := range group {
		if c.Kind == statestore.OpGCRA {
			g := &c.GCRA
			cost, _ := statestore.GCRACost(g.Cost)
			tr.arg(kindGCRA)
			tr.arg(strconv.FormatInt(cost, 10))
			tr.arg(strconv.Itoa(len(g.Limits)))
			for _, l := range g.Limits {
				t, tau, _ := gcraParams(l)
				tr.arg(formatFloat(t))
				tr.arg(formatFloat(tau))
			}
			continue
		}
		q := &c.Quota
		w := q.Window.Milliseconds()
		a, b := quotaWindows(nodeMs, w)
		tr.arg(kindQuota)
		tr.arg(strconv.FormatInt(q.Limit, 10))
		tr.arg(strconv.FormatInt(w, 10))
		tr.arg(strconv.FormatInt(a, 10))
		tr.arg(strconv.FormatInt(b, 10))
	}
	return true
}

// parseConsume reads the library's reply: TIME's seconds and microseconds,
// then one entry per call it ran, the last a deny unless every call ran.
// It fills the calls' outcomes and marks them Done; false is an
// unparsable reply, which leaves every call to the caller to fail.
func (d *Driver) parseConsume(m rueidis.RedisMessage, group []*statestore.Call) bool {
	arr, err := m.ToArray()
	if err != nil || len(arr) < 3 || len(arr)-2 > len(group) {
		return false
	}
	sec, err := arr[0].AsInt64()
	if err != nil {
		return false
	}
	usec, err := arr[1].AsInt64()
	if err != nil || usec < 0 || usec >= 1_000_000 {
		return false
	}
	now := instantOf(sec, usec)
	ran := arr[2:]
	for i := range ran {
		sub, err := ran[i].ToArray()
		if err != nil || len(sub) == 0 {
			return false
		}
		flag, err := sub[0].AsInt64()
		if err != nil || (flag != 0 && flag != 1) {
			return false
		}
		allowed, last := flag == 1, i == len(ran)-1
		if (!allowed && !last) || (allowed && last && len(ran) < len(group)) {
			// A deny ends the script; only a deny ends it early.
			return false
		}
		c := group[i]
		ok := false
		if c.Kind == statestore.OpGCRA {
			ok = gcraOutcome(&c.GCRA, sub[1:], now, allowed)
		} else {
			ok = quotaOutcome(&c.Quota, sub[1:], now, allowed)
		}
		if !ok {
			return false
		}
	}
	for i := range ran {
		group[i].Done = true
	}
	return true
}

// gcraOutcome fills g from the TATs the library read (each max(stored
// TAT, now)) with the memory driver's arithmetic and g.Cost: the denying
// limit has the largest raw retry-after, the lowest index on ties;
// remaining and reset follow each limit's TAT after the decision.
func gcraOutcome(g *statestore.GCRA, tatMsgs []rueidis.RedisMessage, now instant, allowed bool) bool {
	if len(tatMsgs) != len(g.Limits) {
		return false
	}
	var buf [stackLimits]float64
	tats := buf[:0]
	for i := range tatMsgs {
		s, err := tatMsgs[i].ToString()
		if err != nil {
			return false
		}
		tat, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(tat) || math.IsInf(tat, 0) || tat < now.us {
			return false
		}
		tats = append(tats, tat)
	}
	cost, _ := statestore.GCRACost(g.Cost)
	denied, retry := -1, 0.0
	for i, l := range g.Limits {
		t, tau, _ := gcraParams(l)
		if slack := gcraSlack(t, tau, cost); tats[i]-now.us > slack {
			if r := tats[i] - slack - now.us; denied < 0 || r > retry {
				denied, retry = i, r
			}
		}
	}
	if allowed != (denied < 0) {
		return false
	}
	for i, l := range g.Limits {
		t, tau, _ := gcraParams(l)
		tat := tats[i]
		x, conforms := tat, tat-now.us <= tau
		if allowed {
			x = gcraAdvance(tat, t, cost)
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

// quotaOutcome fills q from the count and window start the library
// returned.
func quotaOutcome(q *statestore.Quota, msgs []rueidis.RedisMessage, now instant, allowed bool) bool {
	if len(msgs) != 2 {
		return false
	}
	used, err := msgs[0].AsInt64()
	if err != nil {
		return false
	}
	ws, err := msgs[1].AsInt64()
	w := q.Window.Milliseconds()
	if err != nil || w <= 0 || ws != floorDiv(now.ms, w)*w {
		return false
	}
	// An admission counts 1 to limit units; a denial found limit or more.
	if (allowed && (used < 1 || used > q.Limit)) || (!allowed && (used < 0 || used < q.Limit)) {
		return false
	}
	q.WindowStart = time.UnixMilli(ws).UTC()
	q.ServerNow = time.UnixMilli(now.ms).UTC()
	q.Allowed, q.Used, q.RetryAfter = allowed, used, 0
	if !allowed {
		q.RetryAfter = time.Duration(ws+w-now.ms) * time.Millisecond
	}
	return true
}

// logSkew logs, at most once per interval, a Quota call the library
// refused because the Node clock is more than half a window off.
func (d *Driver) logSkew(ctx context.Context, group []*statestore.Call, err error) {
	re, ok := rueidis.IsRedisErr(err)
	if !ok {
		return
	}
	idx, found := strings.CutPrefix(re.Error(), skewPrefix)
	if !found {
		return
	}
	i, perr := strconv.Atoi(strings.TrimSpace(idx))
	if perr != nil || i < 1 || i > len(group) || group[i-1].Kind != statestore.OpQuota || !d.skewLog.allow(d.clk.Now()) {
		return
	}
	q := &group[i-1].Quota
	d.log.WarnContext(ctx, "state store clock skew beyond half a quota window; call failed",
		catalog.KeyCode, statestore.ErrFailed.Code(), "quota", q.Name, "window", q.Window.String())
}
