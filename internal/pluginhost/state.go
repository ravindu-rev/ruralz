// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"time"

	"github.com/tetratelabs/wazero/api"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
)

// State Store Host Functions (docs/architecture/05-wasm-plugin-system.md
// "Host Function rules", State Store): one blocking call per Plugin Policy
// per request before commit, in chain order, a second returning the
// sticky ERR_BUDGET; upstream-leg Phases replay the first result for the
// same Host Function and key; the onLog write goes through the post-commit
// queue's Plugin class. Keys are rzplg:, the Environment name, ":", the
// length-prefixed Policy name and the guest key (keys.AppendPlugin).

// stateKey returns the State Store key of a guest key
// (keys.AppendPlugin).
func stateKey(environment, policy, key string) string {
	return string(keys.AppendPlugin(make([]byte, 0, 32+len(environment)+len(policy)+len(key)), environment, policy, key))
}

// stateResult is a blocking call's outcome as the guest sees it.
type stateResult struct {
	found bool
	value []byte
	n     int64
	err   *statestore.Error
}

// blocking runs the Policy's one blocking State Store call of fn on key:
// prepare fills the call; the guest is parked, its timer stopped, within
// the pool's parked bound. A second call in a client Phase is ERR_BUDGET;
// in an upstream-leg Phase the first call's result replays for the same
// Host Function and key, and any other is ERR_BUDGET. code is non-zero
// when no result exists.
func (c *call) blocking(fn, key string, kind statestore.OpKind, prepare func(*statestore.PluginState)) (stateResult, int32) {
	st := c.b.state
	if st == nil || st.store == nil {
		return stateResult{}, errUnavailable
	}
	if len(key) > stateKeyBytes {
		return stateResult{}, errLimit
	}
	if c.req.called {
		r := c.req.replay
		if c.x.Leg() != nil && r.fn == fn && r.key == key {
			return stateResult{found: r.found, value: r.value, n: r.n, err: r.err}, 0
		}
		return stateResult{}, c.stick(errBudget, "a second blocking State Store call")
	}
	if !st.keys.admit(key) {
		return stateResult{}, errLimit
	}
	p := c.inst.p
	if !p.park() {
		c.parkedRefused = true
		return stateResult{}, errLimit
	}
	resumed := false
	defer func() {
		if !resumed {
			p.unpark()
		}
	}()
	if c.timer != nil && !c.timer.pause() {
		return stateResult{}, errLimit
	}
	sc := &statestore.Call{Kind: kind}
	sc.Plugin.Key = stateKey(st.namespace, c.b.policy, key)
	prepare(&sc.Plugin)
	rb := c.x.StateBudget()
	if d, ok := rb.CallTimeout(c.reqCtx, c.e.clk.Now(), st.timeout); ok {
		sc.Timeout = d
		calls := []*statestore.Call{sc}
		if kind == statestore.OpPluginGet {
			st.store.Read(c.reqCtx, rb, calls)
		} else {
			st.store.Consume(c.reqCtx, rb, calls)
		}
	} else {
		sc.Err = statestore.ErrNotAttempted
	}
	resumed = true
	p.unpark()
	if c.timer != nil {
		c.timer.resume()
	}
	r := stateResult{found: sc.Plugin.Found, value: sc.Plugin.Value, n: sc.Plugin.N, err: sc.Err}
	c.req.called = true
	c.req.replay = stateReplay{fn: fn, key: key, found: r.found, value: r.value, n: r.n, err: r.err}
	return r, 0
}

// hostStateGet is state_get(key, o, c): the value of the Policy's guest
// key, ERR_ABSENT when it is missing, ERR_UNAVAILABLE when the call failed,
// timed out or was skipped.
func hostStateGet(c *call, m api.Memory, stack []uint64) int32 {
	key := memString(m, u32(stack, 0), u32(stack, 1))
	o, n := u32(stack, 2), u32(stack, 3)
	checkRange(m, o, n)
	r, code := c.blocking("state_get", key, statestore.OpPluginGet, func(*statestore.PluginState) {})
	if code != 0 {
		return code
	}
	if r.err != nil {
		c.stateErr = r.err
		return errUnavailable
	}
	if !r.found {
		return errAbsent
	}
	return output(m, o, n, r.value)
}

// hostStateIncr is state_incr(key, delta, ttlMillis, out): before commit
// it adds delta to the guest key, which takes ttlMillis as its expiry when
// it has none, and writes the new value at out; in onLog it queues the
// write asynchronously, once per request, writing nothing at out.
func hostStateIncr(c *call, m api.Memory, stack []uint64) int32 {
	key := memString(m, u32(stack, 0), u32(stack, 1))
	delta, ttlMillis, out := s64(stack, 2), s64(stack, 3), u32(stack, 4)
	checkRange(m, out, 8)
	ttl := time.Duration(ttlMillis) * time.Millisecond
	if ttlMillis < ttlMin.Milliseconds() || ttlMillis > ttlMax.Milliseconds() || len(key) > stateKeyBytes {
		return errLimit
	}
	if c.ph == phase.OnLog {
		return c.logWrite(key, delta, ttl)
	}
	r, code := c.blocking("state_incr", key, statestore.OpPluginIncr, func(ps *statestore.PluginState) {
		ps.Delta, ps.TTL = delta, ttl
	})
	if code != 0 {
		return code
	}
	if r.err != nil {
		c.stateErr = r.err
		return errUnavailable
	}
	m.WriteUint64Le(out, uint64(r.n)) //nolint:gosec // G115: an i64 travels as its bit pattern.
	return 0
}

// logWrite queues the onLog state_incr in the post-commit queue's Plugin
// class, which never displaces settlement writes; a second one in the
// request is ERR_LIMIT.
func (c *call) logWrite(key string, delta int64, ttl time.Duration) int32 {
	st := c.b.state
	if st == nil || st.enqueuer == nil {
		return errUnavailable
	}
	if c.req.logWrite {
		return errLimit
	}
	if !st.keys.admit(key) {
		return errLimit
	}
	c.req.logWrite = true
	w := &statestore.Write{Kind: statestore.OpPluginIncr, Timeout: st.timeout}
	w.Plugin = statestore.PluginState{Key: stateKey(st.namespace, c.b.policy, key), Delta: delta, TTL: ttl}
	st.enqueuer.Enqueue(statestore.ClassPlugin, w)
	return 0
}
