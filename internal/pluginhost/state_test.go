// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"fmt"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	ht "github.com/ravindu-rev/ruralz/internal/pluginhost/pluginhosttest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// stateStore answers Plugin calls from a map and records keys.
func stateStore(values map[string]string, fail *statestore.Error) *filtertest.Store {
	s := &filtertest.Store{}
	answer := func(calls []*statestore.Call) {
		for _, c := range calls {
			if fail != nil {
				c.Err = fail
				continue
			}
			c.Done = true
			switch c.Kind {
			case statestore.OpPluginGet:
				v, ok := values[c.Plugin.Key]
				c.Plugin.Found, c.Plugin.Value = ok, []byte(v)
			case statestore.OpPluginIncr:
				c.Plugin.N = 41 + c.Plugin.Delta
			default:
				panic(fmt.Sprintf("the Plugin host made a %v State Store call", c.Kind))
			}
		}
	}
	s.ReadFn = answer
	s.ConsumeFn = func(calls []*statestore.Call) int { answer(calls); return len(calls) }
	return s
}

// enqueuer records post-commit writes by class.
type enqueuer struct {
	writes map[statestore.WriteClass][]*statestore.Write
}

func (q *enqueuer) Enqueue(c statestore.WriteClass, w *statestore.Write) bool {
	if q.writes == nil {
		q.writes = map[statestore.WriteClass][]*statestore.Write{}
	}
	q.writes[c] = append(q.writes[c], w)
	return true
}

// stateGuest returns a guest whose rz_on_request_headers calls state_get
// on key twice when twice is set, checking the first result.
func stateGuest(key string, want int32, twice bool) []byte {
	return simple([]string{"state_get"}, nil, func(g *ht.Guest) [][]byte {
		body := [][]byte{ht.Check(g.Call("state_get", g.Str(key), ht.I32Const(int32(ht.Scratch)), ht.I32Const(64)), want)}
		if twice {
			body = append(body, ht.Check(g.Call("state_get", g.Str(key+"2"), ht.I32Const(int32(ht.Scratch)), ht.I32Const(64)), errBudget))
		}
		return append(body, ht.I32Const(0))
	})
}

// TestStateGet: the value of the Policy's rzplg: key, in its Environment
// namespace, length-prefixed by the Policy name.
func TestStateGet(t *testing.T) {
	h := newHarness(t)
	store := stateStore(map[string]string{"rzplg:prod:3:polsession": "abc"}, nil)
	f := h.one(headersPlugin(stateGuest("session", 3, false), v1alpha1.CapabilityStateRead), buildOpts{store: store, env: "prod"})
	if r := handle(f, phase.OnRequestHeaders, exchange(nil)); r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue", r)
	}
	if store.Trips != 1 {
		t.Errorf("round trips = %d, want 1", store.Trips)
	}
}

// TestStateSecondCallIsBudget: a second blocking call in a client Phase is
// the sticky ERR_BUDGET, RZ-PLG-006 whatever the guest returns.
func TestStateSecondCallIsBudget(t *testing.T) {
	h := newHarness(t)
	store := stateStore(nil, nil)
	f := h.one(headersPlugin(stateGuest("k", errAbsent, true), v1alpha1.CapabilityStateRead), buildOpts{store: store})
	if r := handle(f, phase.OnRequestHeaders, exchange(nil)); codeOf(r) != CodeMisuse {
		t.Fatalf("Handle = %+v, want %s", r, CodeMisuse)
	}
}

// TestStateFailureUsesSTS: a failed State Store call returns
// ERR_UNAVAILABLE, and a guest that then cannot decide takes the call's
// RZ-STS code.
func TestStateFailureUsesSTS(t *testing.T) {
	h := newHarness(t)
	store := stateStore(nil, statestore.ErrTimeout)
	bin := simple([]string{"state_get"}, []ht.Locals{{N: 1, Type: ht.I32}}, func(g *ht.Guest) [][]byte {
		return [][]byte{g.Call("state_get", g.Str("k"), ht.I32Const(int32(ht.Scratch)), ht.I32Const(64))}
	})
	f := h.one(headersPlugin(bin, v1alpha1.CapabilityStateRead), buildOpts{store: store})
	r := handle(f, phase.OnRequestHeaders, exchange(nil))
	if r.Outcome != filter.CannotDecide || codeOf(r) != "RZ-STS-001" {
		t.Fatalf("Handle = %+v, want CannotDecide RZ-STS-001", r)
	}
}

// TestStateIncr: before commit, state_incr writes the new value at out.
func TestStateIncr(t *testing.T) {
	h := newHarness(t)
	store := stateStore(nil, nil)
	bin := simple([]string{"state_incr"}, nil, func(g *ht.Guest) [][]byte {
		return [][]byte{
			ht.Check(g.Call("state_incr", g.Str("n"), ht.I64Const(1), ht.I64Const(60000), ht.I32Const(int32(ht.Scratch))), 0),
			ht.CheckMem(ht.Scratch, []byte{42, 0, 0, 0, 0, 0, 0, 0}),
			ht.Check(g.Call("state_incr", g.Str("n"), ht.I64Const(1), ht.I64Const(0), ht.I32Const(int32(ht.Scratch))), errLimit),
			ht.I32Const(0),
		}
	})
	f := h.one(headersPlugin(bin, v1alpha1.CapabilityStateWrite), buildOpts{store: store})
	if r := handle(f, phase.OnRequestHeaders, exchange(nil)); r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue", r)
	}
}

// TestStateOnLogWrite: in onLog, state_incr queues one asynchronous write
// in the post-commit queue's Plugin class; a second is ERR_LIMIT.
func TestStateOnLogWrite(t *testing.T) {
	h := newHarness(t)
	q := &enqueuer{}
	g := ht.NewGuest("state_incr")
	g.Phase("rz_on_log", nil,
		ht.Check(g.Call("state_incr", g.Str("hits"), ht.I64Const(2), ht.I64Const(1000), ht.I32Const(int32(ht.Scratch))), 0),
		ht.Check(g.Call("state_incr", g.Str("hits"), ht.I64Const(2), ht.I64Const(1000), ht.I32Const(int32(ht.Scratch))), errLimit),
		ht.I32Const(0))
	p := pluginOpts{bin: g.Encode(), phases: []v1alpha1.Phase{v1alpha1.PhaseOnLog}, caps: []v1alpha1.Capability{v1alpha1.CapabilityStateWrite}}
	f := h.one(p, buildOpts{store: stateStore(nil, nil), enqueue: q, env: "e"})
	if r := handle(f, phase.OnLog, exchange(nil)); r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue", r)
	}
	ws := q.writes[statestore.ClassPlugin]
	if len(ws) != 1 || ws[0].Kind != statestore.OpPluginIncr || ws[0].Plugin.Key != "rzplg:e:3:polhits" || ws[0].Plugin.TTL != time.Second {
		t.Fatalf("Plugin class writes = %+v, want one increment of rzplg:e:3:polhits", ws)
	}
}

// TestStateReplayAcrossLegs: an Upstream-scope Policy's later attempts and
// composition steps replay the first result for the same Host Function
// and key without a round trip.
func TestStateReplayAcrossLegs(t *testing.T) {
	h := newHarness(t)
	store := stateStore(map[string]string{"rzplg::3:polk": "v"}, nil)
	g := ht.NewGuest("state_get")
	g.Phase("rz_on_upstream_request", nil,
		ht.Check(g.Call("state_get", g.Str("k"), ht.I32Const(int32(ht.Scratch)), ht.I32Const(8)), 1),
		ht.I32Const(0))
	p := pluginOpts{bin: g.Encode(), phases: []v1alpha1.Phase{v1alpha1.PhaseOnUpstreamRequest}, caps: []v1alpha1.Capability{v1alpha1.CapabilityStateRead}}
	f := h.one(p, buildOpts{store: store, scope: phase.ScopeUpstream})
	shared := new(filter.SharedSlot)
	for leg := range 3 {
		x := exchange(nil)
		x.LegV = &filter.Leg{Upstream: "u", Attempt: leg + 1}
		x.Shared = shared
		if r := handle(f, phase.OnUpstreamRequest, x); r.Outcome != filter.Continue {
			t.Fatalf("leg %d = %+v, want Continue", leg, r)
		}
	}
	if store.Trips != 1 {
		t.Errorf("round trips = %d, want 1 replayed by later legs", store.Trips)
	}
}

// TestParkedBound: past the parked bound no call is attempted, state_get
// returns ERR_LIMIT, and a guest that cannot decide gets RZ-PLG-010.
func TestParkedBound(t *testing.T) {
	h := newHarness(t)
	store := stateStore(nil, nil)
	bin := simple([]string{"state_get"}, nil, func(g *ht.Guest) [][]byte {
		return [][]byte{g.Call("state_get", g.Str("k"), ht.I32Const(int32(ht.Scratch)), ht.I32Const(8))}
	})
	f := h.one(headersPlugin(bin, v1alpha1.CapabilityStateRead), buildOpts{store: store})
	f.pool.mu.Lock()
	f.pool.parked = f.pool.bounds().parked
	f.pool.mu.Unlock()
	r := handle(f, phase.OnRequestHeaders, exchange(nil))
	if codeOf(r) != CodeParkedBound {
		t.Fatalf("Handle = %+v, want %s", r, CodeParkedBound)
	}
	if store.Trips != 0 {
		t.Errorf("round trips = %d, want 0 past the parked bound", store.Trips)
	}
}

// TestParkingStopsTheTimer: the call timer stops while the guest waits on
// its State Store call, so a slow round trip outlasting limits.timeout is
// no RZ-PLG-002.
func TestParkingStopsTheTimer(t *testing.T) {
	h := newHarness(t)
	store := &filtertest.Store{}
	store.ReadFn = func(calls []*statestore.Call) {
		h.clk.Advance(10 * DefaultTimeout)
		for _, c := range calls {
			c.Done = true
		}
	}
	f := h.one(headersPlugin(stateGuest("k", errAbsent, false), v1alpha1.CapabilityStateRead), buildOpts{store: store})
	if r := handle(f, phase.OnRequestHeaders, exchange(nil)); r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue after a parked call longer than limits.timeout", r)
	}
}
