// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// hour is the window of most tests: it ends 3600 s after testStart, and a
// denial's Retry-After spreads up to 1% of it, 36 s, past the end.
const hour = time.Hour

// wantDecided fails unless m counted want.
func wantDecided(t *testing.T, what string, m *metrics, want map[string]uint64) {
	t.Helper()
	if got := m.decided(); !maps.Equal(got, want) {
		t.Errorf("%s: decisions %v, want %v", what, got, want)
	}
}

// wantField fails unless o recorded exactly field.
func wantField(t *testing.T, what string, o outcome, field filter.RateLimitField) {
	t.Helper()
	if fs := o.fields(); len(fs) != 1 || fs[0] != field {
		t.Errorf("%s: fields %+v, want [%+v]", what, fs, field)
	}
}

// wantDenied fails unless o is a 429 RZ-RL-003 whose Retry-After lies from
// end seconds to end plus spread seconds, listed in its field with r =
// units.
func wantDenied(t *testing.T, what string, o outcome, units, end, spread int64) {
	t.Helper()
	ra := o.retryAfter()
	if o.status() != http.StatusTooManyRequests || o.code() != CodeExhausted || ra < end || ra > end+spread {
		t.Errorf("%s: status %d code %q Retry-After %d; want 429 %s within [%d, %d]", what, o.status(), o.code(), ra, CodeExhausted, end, end+spread)
	}
	fs := o.fields()
	if len(fs) != 1 || !fs[0].Denied || !fs[0].Known || fs[0].R != units || fs[0].T != ra {
		t.Errorf("%s: fields %+v; want the denied quota with r=%d t=%d", what, fs, units, ra)
	}
}

// TestAdmitThenExhaust covers a window's units: each admission reserves
// one unit through one script and reports the units left and the seconds
// to the window's end; the request past the limit gets 429 RZ-RL-003 with
// Retry-After at the window's end plus up to 1% of the window, cached, so
// the next denial needs no script.
func TestAdmitThenExhaust(t *testing.T) {
	fx := newFixture(t)
	f, m := fx.build(t, "quota-daily", config("daily"))
	acme := consumer("acme", windowQuota("daily", 3, hour))
	for i := range 3 {
		o := fx.request(t, f, vars(acme, "GET", ""), served())
		if !o.called || o.r.Outcome != filter.Continue {
			t.Fatalf("request %d: outcome %+v, called %v; want an admission by script", i+1, o.r, o.called)
		}
		wantField(t, "admission", o, filter.RateLimitField{Name: "quota-daily", Q: 3, W: 3600, R: int64(2 - i), T: 3600, Known: true})
	}
	o := fx.request(t, f, vars(acme, "GET", ""), served())
	if !o.called {
		t.Error("the first denial skipped the script")
	}
	wantDenied(t, "script denial", o, 0, 3600, 36)
	o = fx.request(t, f, vars(acme, "GET", ""), served())
	if o.called {
		t.Error("a cached denial called the script")
	}
	wantDenied(t, "cached denial", o, 0, 3600, 36)
	if n := fx.store.roundTrips(); n != 4 {
		t.Errorf("%d round trips, want 4", n)
	}
	wantDecided(t, "decisions", m, map[string]uint64{"allow": 3, "deny": 2})
}

// TestCostUnits covers config.cost: a request reserves its cost, all or
// none; a cost above the units left is denied, reserving nothing, and the
// cached denial still sends a cost within its units to the script.
func TestCostUnits(t *testing.T) {
	fx := newFixture(t)
	f, m := fx.build(t, "quota-writes", config("daily", `request.method == "POST" ? 5 : 1`))
	acme := consumer("acme", windowQuota("daily", 6, hour))
	o := fx.request(t, f, vars(acme, "POST", ""), served())
	if o.call.Quota.Cost != 5 {
		t.Fatalf("POST cost %d, want 5", o.call.Quota.Cost)
	}
	wantField(t, "POST", o, filter.RateLimitField{Name: "quota-writes", Q: 6, W: 3600, R: 1, T: 3600, Known: true})
	wantDenied(t, "POST past the units left", fx.request(t, f, vars(acme, "POST", ""), served()), 1, 3600, 36)
	if o := fx.request(t, f, vars(acme, "POST", ""), served()); o.called {
		t.Error("a cost above the cached units called the script")
	}
	o = fx.request(t, f, vars(acme, "GET", ""), served())
	if !o.called || o.r.Outcome != filter.Continue {
		t.Fatalf("GET within the cached units: outcome %+v, called %v; want an admission by script", o.r, o.called)
	}
	wantField(t, "GET", o, filter.RateLimitField{Name: "quota-writes", Q: 6, W: 3600, R: 0, T: 3600, Known: true})
	if o := fx.request(t, f, vars(acme, "GET", ""), served()); !o.called {
		t.Error("the admission did not forget the cached denial")
	}
	wantDecided(t, "decisions", m, map[string]uint64{"allow": 2, "deny": 3})
}

// TestEvaluationFailures covers a config.cost runtime error, a cost below
// 1 and a config.key runtime error: failureMode open admits unmetered,
// listing the quota without a count; closed answers RZ-RL-005. Nothing is
// reserved, so nothing is refunded.
func TestEvaluationFailures(t *testing.T) {
	tests := []struct {
		name, cost, header, key string
	}{
		{"cost error", `int(request.headers["x-cost"])`, "", ""},
		{"cost zero", `int(request.headers["x-cost"])`, "0", ""},
		{"negative cost", `int(request.headers["x-cost"])`, "-3", ""},
		{"key error", "", "", `request.headers["x-tenant"]`},
	}
	for _, tc := range tests {
		for _, mode := range []v1alpha1.FailureMode{v1alpha1.FailureModeOpen, v1alpha1.FailureModeClosed} {
			fx := newFixture(t)
			cfg := config("daily")
			if tc.cost != "" {
				cfg.Cost = &tc.cost
			}
			if tc.key != "" {
				cfg.Key = &tc.key
			}
			f, m := fx.buildMode(t, "quota-daily", mode, cfg)
			o := fx.request(t, f, vars(consumer("acme", windowQuota("daily", 5, hour)), "GET", tc.header), filter.Final{Code: "RZ-UP-001"})
			what := tc.name + " " + string(mode)
			if o.called || o.r.Outcome != filter.CannotDecide || o.code() != CodeUndecided {
				t.Errorf("%s: outcome %+v, called %v; want undecided %s without a call", what, o.r, o.called, CodeUndecided)
			}
			wantField(t, what, o, filter.RateLimitField{Name: "quota-daily", Q: 5, W: 3600})
			result := "fail_open"
			if mode == v1alpha1.FailureModeClosed {
				result = "fail_closed"
			}
			wantDecided(t, what, m, map[string]uint64{result: 1})
			if len(fx.queue.writes) != 0 {
				t.Errorf("%s: %d refunds of nothing reserved", what, len(fx.queue.writes))
			}
		}
	}
}

// TestNoQuota covers RZ-RL-004: no Consumer, a Consumer lacking the quota
// and one holding it with unit tokens get 403 before config.key and
// config.cost are evaluated, without a call or a field.
func TestNoQuota(t *testing.T) {
	tokens := windowQuota("daily", 5, hour)
	tokens.Unit = v1alpha1.QuotaUnitTokens
	tests := []struct {
		name string
		c    *expr.Consumer
	}{
		{"no Consumer", nil},
		{"no quota", consumer("acme")},
		{"other quotas", consumer("acme", windowQuota("monthly", 5, hour))},
		{"unit tokens", consumer("acme", tokens)},
	}
	for _, tc := range tests {
		fx := newFixture(t)
		f, m := fx.build(t, "quota-daily", config("daily", `int(request.headers["x-cost"])`))
		o := fx.request(t, f, vars(tc.c, "GET", ""), served())
		if o.called || o.status() != http.StatusForbidden || o.code() != CodeNoQuota || len(o.fields()) != 0 {
			t.Errorf("%s: status %d code %q called %v fields %v; want 403 %s alone", tc.name, o.status(), o.code(), o.called, o.fields(), CodeNoQuota)
		}
		wantDecided(t, tc.name, m, map[string]uint64{"no_quota": 1})
	}
}

// TestDenialCacheLife covers a cached denial's life: at most 60 s, then
// the script runs again; never past the window's end, where the next
// window admits.
func TestDenialCacheLife(t *testing.T) {
	fx := newFixture(t)
	f, _ := fx.build(t, "quota-daily", config("daily"))
	acme := consumer("acme", windowQuota("daily", 1, hour))
	fx.request(t, f, vars(acme, "GET", ""), served())
	wantDenied(t, "script denial", fx.request(t, f, vars(acme, "GET", ""), served()), 0, 3600, 36)
	fx.clk.Advance(59 * time.Second)
	if o := fx.request(t, f, vars(acme, "GET", ""), served()); o.called {
		t.Error("a denial 59 s old called the script")
	}
	fx.clk.Advance(time.Second)
	if o := fx.request(t, f, vars(acme, "GET", ""), served()); !o.called {
		t.Error("a denial 60 s old still answered")
	}

	// Near the window's end the denial ends with the window.
	fx = newFixture(t)
	f, _ = fx.build(t, "quota-daily", config("daily"))
	fx.clk.Advance(hour - 30*time.Second)
	fx.request(t, f, vars(acme, "GET", ""), served())
	wantDenied(t, "denial 30 s before the end", fx.request(t, f, vars(acme, "GET", ""), served()), 0, 30, 36)
	fx.clk.Advance(30 * time.Second)
	o := fx.request(t, f, vars(acme, "GET", ""), served())
	if !o.called || o.r.Outcome != filter.Continue {
		t.Errorf("next window: outcome %+v, called %v; want an admission by script", o.r, o.called)
	}
}

// TestRaisedLimit covers the denial cache keyed with the limit: a raised
// limit applies at once, as does a changed window.
func TestRaisedLimit(t *testing.T) {
	fx := newFixture(t)
	f, _ := fx.build(t, "quota-daily", config("daily"))
	fx.request(t, f, vars(consumer("acme", windowQuota("daily", 1, hour)), "GET", ""), served())
	wantDenied(t, "limit 1", fx.request(t, f, vars(consumer("acme", windowQuota("daily", 1, hour)), "GET", ""), served()), 0, 3600, 36)
	o := fx.request(t, f, vars(consumer("acme", windowQuota("daily", 2, hour)), "GET", ""), served())
	if !o.called || o.r.Outcome != filter.Continue {
		t.Errorf("raised limit: outcome %+v, called %v; want an admission by script", o.r, o.called)
	}
}

// TestRefunds covers onLog: the charging Policy returns its cost to the
// window it charged when the request ends refunded, through the settle
// class with its stateStoreTimeout, and the State Store then admits that
// many more; a served request, a cache hit and a request an attempt
// reached are charged.
func TestRefunds(t *testing.T) {
	tests := []struct {
		name   string
		final  filter.Final
		refund bool
	}{
		{"connect failed", filter.Final{Status: http.StatusBadGateway, Code: "RZ-UP-001"}, true},
		{"body too large", filter.Final{Status: http.StatusRequestEntityTooLarge, Code: "RZ-RT-003"}, true},
		{"rejected later", filter.Final{Status: http.StatusForbidden, Code: "RZ-AUTH-001", RejectedAfter: true}, true},
		{"client gone", filter.Final{}, true},
		{"served", served(), false},
		{"cache hit", filter.Final{Status: http.StatusOK, Committed: true}, false},
		{"reset after connecting", filter.Final{Status: http.StatusBadGateway, Code: "RZ-UP-004", UpstreamReached: true}, false},
	}
	for _, tc := range tests {
		fx := newFixture(t)
		f, _ := fx.build(t, "quota-writes", config("daily", `request.method == "POST" ? 3 : 1`))
		acme := consumer("acme", windowQuota("daily", 5, hour))
		fx.request(t, f, vars(acme, "POST", ""), tc.final)
		if !tc.refund {
			if len(fx.queue.writes) != 0 {
				t.Errorf("%s: %d refunds, want none", tc.name, len(fx.queue.writes))
			}
			continue
		}
		if len(fx.queue.writes) != 1 || fx.queue.class[0] != statestore.ClassSettle {
			t.Fatalf("%s: writes %v in classes %v, want one settle write", tc.name, fx.queue.writes, fx.queue.class)
		}
		w := fx.queue.writes[0]
		want := statestore.Refund{Name: "daily", Window: hour, WindowStart: testStart(), Units: 3, Digest: statestore.DigestOf("acme")}
		if w.Kind != statestore.OpRefund || w.Timeout != stateTimeout || w.Refund != want {
			t.Errorf("%s: write %+v, want a refund %+v with timeout %v", tc.name, *w, want, stateTimeout)
		}
		fx.mem.Write(context.Background(), []*statestore.Write{w})
		if !w.Applied || w.Err != nil {
			t.Fatalf("%s: refund applied %v, err %v", tc.name, w.Applied, w.Err)
		}
		o := fx.request(t, f, vars(acme, "GET", ""), served())
		if fs := o.fields(); len(fs) != 1 || fs[0].R != 4 {
			t.Errorf("%s: after the refund fields %+v, want r=4", tc.name, fs)
		}
	}
}

// TestRefundNeedsReservation covers the refunds that do not happen: a
// denial, an admission unmetered by a failing State Store and a request
// without a queue reserve nothing to return; a full queue drops the
// refund, which only over-charges.
func TestRefundNeedsReservation(t *testing.T) {
	failed := filter.Final{Status: http.StatusBadGateway, Code: "RZ-UP-001"}
	fx := newFixture(t)
	f, _ := fx.build(t, "quota-daily", config("daily"))
	acme := consumer("acme", windowQuota("daily", 1, hour))
	fx.request(t, f, vars(acme, "GET", ""), served())
	fx.request(t, f, vars(acme, "GET", ""), failed)
	if len(fx.queue.writes) != 0 {
		t.Errorf("a denial refunded %d writes", len(fx.queue.writes))
	}
	fx.store.failWith(statestore.ErrFailed)
	fx.request(t, f, vars(consumer("beta", windowQuota("daily", 1, hour)), "GET", ""), failed)
	if len(fx.queue.writes) != 0 {
		t.Errorf("an unmetered admission refunded %d writes", len(fx.queue.writes))
	}

	fx = newFixture(t)
	fx.queue.full = true
	f, _ = fx.build(t, "quota-daily", config("daily"))
	fx.request(t, f, vars(acme, "GET", ""), failed)
	if len(fx.queue.writes) != 0 {
		t.Errorf("a full queue kept %d refunds", len(fx.queue.writes))
	}
	if o := fx.request(t, f, vars(acme, "GET", ""), served()); o.status() != http.StatusTooManyRequests {
		t.Errorf("after a dropped refund: status %d, want 429 with the unit still charged", o.status())
	}
	env := fx.env(policy("quota-noqueue", v1alpha1.FailureModeOpen), config("daily"), nil)
	env.Enqueuer = nil
	built, err := Factory{Cache: fx.cache}.Build(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if o := fx.request(t, built.(*Filter), vars(consumer("gamma", windowQuota("daily", 1, hour)), "GET", ""), failed); o.r.Outcome != filter.Continue {
		t.Errorf("without a queue: %+v, want an admission whose refund is skipped", o.r)
	}
}

// TestFollower covers a request's counter charged once: a later quota
// Policy whose counter an earlier one charged takes its decision without
// a call, listing its quota without a count, and counts the decision, at
// once when known, else in Finish, nothing when the charging Policy never
// decided; only the charging Policy refunds.
func TestFollower(t *testing.T) {
	refund := filter.Final{Status: http.StatusBadGateway, Code: "RZ-UP-001"}
	fx := newFixture(t)
	gw, mg := fx.build(t, "quota-gateway", config("daily"))
	rt, mr := fx.build(t, "quota-route", config("daily", "2"))
	acme := consumer("acme", windowQuota("daily", 5, hour))
	pair := func() (*filtertest.Exchange, *filtertest.Exchange) {
		v := vars(acme, "GET", "")
		a, b := exchange("quota-gateway", v), exchange("quota-route", v)
		a.Charged = &filtertest.Charges{}
		b.Charged, b.StateDeadline = a.Charged, a.StateDeadline
		return a, b
	}

	// One batch: the follower prepares while the charger waits.
	a, b := pair()
	ca, cb := statestore.Call{Batch: -1}, statestore.Call{Batch: -1}
	if _, done := gw.Prepare(context.Background(), a, &ca); done {
		t.Fatal("the charger decided without a call")
	}
	if r, done := rt.Prepare(context.Background(), b, &cb); !done || r.Outcome != filter.Continue {
		t.Fatalf("the follower: %+v, done %v; want an admission without a call", r, done)
	}
	if len(mr.decided()) != 0 {
		t.Errorf("the follower counted %v before the charger decided", mr.decided())
	}
	fx.store.Consume(context.Background(), a.StateDeadline, []*statestore.Call{&ca})
	if r := gw.Complete(context.Background(), a, &ca); r.Outcome != filter.Continue {
		t.Fatalf("the charger: %+v", r)
	}
	a.FinalV, b.FinalV = refund, refund
	gw.Handle(context.Background(), phase.OnLog, a)
	rt.Handle(context.Background(), phase.OnLog, b)
	gw.Finish(context.Background(), a)
	rt.Finish(context.Background(), b)
	follower := filter.RateLimitField{Name: "quota-route", Q: 5, W: 3600}
	if fx.store.roundTrips() != 1 || len(a.RateLimits) != 1 || !a.RateLimits[0].Known || len(b.RateLimits) != 1 || b.RateLimits[0] != follower {
		t.Errorf("%d round trips, fields %v and %v; want one call, the charger's count and the follower's field without one", fx.store.roundTrips(), a.RateLimits, b.RateLimits)
	}
	if len(fx.queue.writes) != 1 || fx.queue.writes[0].Refund.Units != 1 {
		t.Errorf("refunds %v, want the charger's one unit alone", fx.queue.writes)
	}
	wantDecided(t, "charger", mg, map[string]uint64{"allow": 1})
	wantDecided(t, "follower in Finish", mr, map[string]uint64{"allow": 1})

	// The charger decided before the follower prepared: counted at once.
	a, b = pair()
	fx.decide(t, gw, a)
	fx.decide(t, rt, b)
	wantDecided(t, "follower in Prepare", mr, map[string]uint64{"allow": 2})
	gw.Finish(context.Background(), a)
	rt.Finish(context.Background(), b)
	wantDecided(t, "follower counted once", mr, map[string]uint64{"allow": 2})

	// The charger never decided: its round trip was never sent.
	a, b = pair()
	ca, cb = statestore.Call{Batch: -1}, statestore.Call{Batch: -1}
	gw.Prepare(context.Background(), a, &ca)
	rt.Prepare(context.Background(), b, &cb)
	gw.Undo(a)
	gw.Finish(context.Background(), a)
	rt.Finish(context.Background(), b)
	wantDecided(t, "follower of an undone charger", mr, map[string]uint64{"allow": 2})
}

// TestCountersApart covers counters that differ: another consumerQuota,
// or another key value, charges on its own.
func TestCountersApart(t *testing.T) {
	fx := newFixture(t)
	daily, _ := fx.build(t, "quota-daily", config("daily"))
	other, _ := fx.build(t, "quota-other", config("other"))
	acme := consumer("acme", windowQuota("daily", 5, hour), windowQuota("other", 5, hour))
	v := vars(acme, "GET", "")
	a, b := exchange("quota-daily", v), exchange("quota-other", v)
	a.Charged = &filtertest.Charges{}
	b.Charged = a.Charged
	if !fx.decide(t, daily, a).called || !fx.decide(t, other, b).called {
		t.Error("a quota of another name took the first's decision")
	}
}

// TestKeyPartitions covers config.key: by default the Consumer's name,
// whose digest names the counter, so Consumers count apart; a constant
// key shares one counter; an empty key puts every request in one.
func TestKeyPartitions(t *testing.T) {
	fx := newFixture(t)
	f, _ := fx.build(t, "quota-daily", config("daily"))
	for _, name := range []string{"acme", "beta"} {
		o := fx.request(t, f, vars(consumer(name, windowQuota("daily", 1, hour)), "GET", ""), served())
		if o.r.Outcome != filter.Continue || o.call.Quota.Digest != statestore.DigestOf(name) {
			t.Errorf("Consumer %s: outcome %+v digest %x; want an admission under its name", name, o.r, o.call.Quota.Digest)
		}
	}
	for _, key := range []string{`"shared"`, ""} {
		fx := newFixture(t)
		cfg := config("daily")
		cfg.Key = &key
		f, _ := fx.build(t, "quota-shared", cfg)
		fx.request(t, f, vars(consumer("acme", windowQuota("daily", 1, hour)), "GET", ""), served())
		if o := fx.request(t, f, vars(consumer("beta", windowQuota("daily", 1, hour)), "GET", ""), served()); o.status() != http.StatusTooManyRequests {
			t.Errorf("key %q: a second Consumer got %d, want 429 on the shared counter", key, o.status())
		}
	}
}

// TestMonthQuota covers a calendar quota: the call names the month, w is
// the month's length and t the seconds to its end, and a refund names the
// month it charged.
func TestMonthQuota(t *testing.T) {
	fx := newFixture(t)
	f, _ := fx.build(t, "quota-monthly", config("monthly"))
	acme := consumer("acme", monthQuota("monthly", 2))
	const october, left = 31 * 86400, 14*86400 + 14*3600
	o := fx.request(t, f, vars(acme, "GET", ""), filter.Final{Code: "RZ-RT-007"})
	if !o.call.Quota.Month || o.call.Quota.Window != 0 {
		t.Errorf("call %+v, want a calendar month", o.call.Quota)
	}
	wantField(t, "month admission", o, filter.RateLimitField{Name: "quota-monthly", Q: 2, W: october, R: 1, T: left, Known: true})
	want := statestore.Refund{Name: "monthly", Month: true, WindowStart: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Units: 1, Digest: statestore.DigestOf("acme")}
	if len(fx.queue.writes) != 1 || fx.queue.writes[0].Refund != want {
		t.Errorf("refunds %v, want %+v", fx.queue.writes, want)
	}
	fx.request(t, f, vars(acme, "GET", ""), served())
	fx.request(t, f, vars(acme, "GET", ""), served())
	wantDenied(t, "month denial", fx.request(t, f, vars(acme, "GET", ""), served()), 0, left, 60)
}

// TestStateStoreFailures covers a failing call: failureMode open admits
// unmetered with the quota listed without a count and nothing to refund;
// closed answers with the call's RZ-STS code. A follower takes the
// failure's decision.
func TestStateStoreFailures(t *testing.T) {
	for _, mode := range []v1alpha1.FailureMode{v1alpha1.FailureModeOpen, v1alpha1.FailureModeClosed} {
		fx := newFixture(t)
		fx.store.failWith(statestore.ErrFailed)
		f, m := fx.buildMode(t, "quota-daily", mode, config("daily"))
		o := fx.request(t, f, vars(consumer("acme", windowQuota("daily", 5, hour)), "GET", ""), filter.Final{Code: "RZ-UP-001"})
		if o.r.Outcome != filter.CannotDecide || o.code() != statestore.ErrFailed.Code() {
			t.Errorf("%s: outcome %+v, want undecided %s", mode, o.r, statestore.ErrFailed.Code())
		}
		wantField(t, string(mode), o, filter.RateLimitField{Name: "quota-daily", Q: 5, W: 3600})
		result := map[v1alpha1.FailureMode]string{v1alpha1.FailureModeOpen: "fail_open", v1alpha1.FailureModeClosed: "fail_closed"}[mode]
		wantDecided(t, string(mode), m, map[string]uint64{result: 1})
		if len(fx.queue.writes) != 0 {
			t.Errorf("%s: an unanswered call refunded", mode)
		}
		if ch := o.x.Charged.List; len(ch) != 1 || !ch[0].Decided {
			t.Errorf("%s: record %+v, want the failure decided for followers", mode, ch)
		}
	}
}

// TestNoWindow covers a Consumer quota with neither a window nor a
// calendar month, which validation refuses: failureMode applies.
func TestNoWindow(t *testing.T) {
	fx := newFixture(t)
	f, m := fx.build(t, "quota-daily", config("daily"))
	q := v1alpha1.Quota{Name: "daily", Unit: v1alpha1.QuotaUnitRequests, Limit: 5}
	o := fx.request(t, f, vars(consumer("acme", q), "GET", ""), served())
	var it interface{ ErrorType() string }
	if o.called || o.r.Outcome != filter.CannotDecide || !errors.As(o.r.Err, &it) || it.ErrorType() != errTypeInvalidValue {
		t.Errorf("outcome %+v, called %v; want an invalid value without a call", o.r, o.called)
	}
	wantDecided(t, "decisions", m, map[string]uint64{"fail_open": 1})
}

// TestHandle covers a caller without batching: Handle decides
// onRequestHeaders with its own round trip, settles onLog, and continues
// in any other Phase or an onLog without a decision.
func TestHandle(t *testing.T) {
	fx := newFixture(t)
	f, m := fx.build(t, "quota-daily", config("daily"))
	x := exchange("quota-daily", vars(consumer("acme", windowQuota("daily", 5, hour)), "GET", ""))
	if r := f.Handle(context.Background(), phase.OnRequestHeaders, x); r.Outcome != filter.Continue || fx.store.roundTrips() != 1 {
		t.Fatalf("onRequestHeaders: %+v after %d round trips", r, fx.store.roundTrips())
	}
	if r := f.Handle(context.Background(), phase.OnResponse, x); r.Outcome != filter.Continue {
		t.Errorf("onResponse: %+v", r)
	}
	x.FinalV = filter.Final{Code: "RZ-UP-005", Status: http.StatusServiceUnavailable}
	f.Handle(context.Background(), phase.OnLog, x)
	f.Handle(context.Background(), phase.OnLog, x)
	f.Finish(context.Background(), x)
	if len(fx.queue.writes) != 1 {
		t.Errorf("%d refunds, want one however many onLog calls", len(fx.queue.writes))
	}
	if r := f.Handle(context.Background(), phase.OnLog, exchange("quota-daily", nil)); r.Outcome != filter.Continue || len(fx.queue.writes) != 1 {
		t.Errorf("onLog without a decision: %+v", r)
	}
	wantDecided(t, "decisions", m, map[string]uint64{"allow": 1})

	// Without a store or a budget the call is not attempted.
	env := fx.env(policy("quota-nostore", v1alpha1.FailureModeClosed), config("daily"), nil)
	env.StateStore = nil
	built, err := Factory{Cache: fx.cache}.Build(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	x = exchange("quota-nostore", vars(consumer("acme", windowQuota("daily", 5, hour)), "GET", ""))
	if r := built.Handle(context.Background(), phase.OnRequestHeaders, x); r.Outcome != filter.CannotDecide || r.Code != statestore.ErrNotAttempted.Code() {
		t.Errorf("no store: %+v, want undecided %s", r, statestore.ErrNotAttempted.Code())
	}
}

// TestBuildRefusals covers Build: a config of another type (RZ-CFG-005),
// Upstream scope or a placement without onRequestHeaders (RZ-CFG-020),
// expressions that do not compile (RZ-CFG-014) or exceed the cost ceiling
// (RZ-CFG-015), and the programming errors without a code.
func TestBuildRefusals(t *testing.T) {
	fx := newFixture(t)
	bad := func(s string) *string { return &s }
	tests := []struct {
		name string
		edit func(*filter.BuildEnv)
		code string
		err  error
	}{
		{"config type", func(e *filter.BuildEnv) { e.Config = &v1alpha1.RateLimitConfig{} }, CodeConfig, nil},
		{"Upstream scope", func(e *filter.BuildEnv) { e.Scope = phase.ScopeUpstream }, CodeScope, nil},
		{"no onRequestHeaders", func(e *filter.BuildEnv) { e.Phases = phase.Of(phase.OnLog) }, CodeScope, nil},
		{"key syntax", func(e *filter.BuildEnv) { e.Config.(*v1alpha1.QuotaConfig).Key = bad(`request.`) }, CodeCEL, nil},
		{"cost type", func(e *filter.BuildEnv) { e.Config.(*v1alpha1.QuotaConfig).Cost = bad(`"one"`) }, CodeCEL, nil},
		{"cost ceiling", func(e *filter.BuildEnv) {
			e.Config.(*v1alpha1.QuotaConfig).Cost = bad(`request.headers.all(a, request.headers.all(b, request.headers.all(c, a + b + c != ""))) ? 1 : 2`)
		}, CodeCost, nil},
		{"no CEL", func(e *filter.BuildEnv) { e.CEL = nil; e.Config.(*v1alpha1.QuotaConfig).Cost = bad("2") }, "", ErrNoCEL},
	}
	for _, tc := range tests {
		env := fx.env(policy("quota-daily", v1alpha1.FailureModeOpen), config("daily"), nil)
		tc.edit(&env)
		_, err := Factory{Cache: fx.cache}.Build(context.Background(), env)
		if code, _ := errcode.CodeOf(err); code != tc.code || (tc.err != nil && !errors.Is(err, tc.err)) || err == nil {
			t.Errorf("%s: Build = %v (code %q), want code %q or %v", tc.name, err, code, tc.code, tc.err)
		}
	}
	if _, err := (Factory{}).Build(context.Background(), fx.env(policy("q", v1alpha1.FailureModeOpen), config("daily"), nil)); !errors.Is(err, ErrNoCache) {
		t.Errorf("no cache: Build = %v, want ErrNoCache", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Factory{Cache: fx.cache}).Build(ctx, fx.env(policy("q", v1alpha1.FailureModeOpen), config("daily"), nil)); !errors.Is(err, context.Canceled) {
		t.Errorf("ended ctx: Build = %v, want context.Canceled", err)
	}
	env := fx.env(policy("q", v1alpha1.FailureModeOpen), config("daily"), nil)
	env.CEL = nil
	if _, err := (Factory{Cache: fx.cache}).Build(context.Background(), env); err != nil {
		t.Errorf("default key and cost without CEL: Build = %v, want no program needed", err)
	}
}
