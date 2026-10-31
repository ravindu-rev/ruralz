// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"strconv"
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

// Tests for the decision path of
// docs/architecture/09-traffic-management-and-resilience.md "Rate
// limiting", "Per-Node ceiling" and "RateLimit response headers".

// wantDecisions fails the test unless m counted exactly want.
func wantDecisions(t *testing.T, m *metrics, want map[string]uint64) {
	t.Helper()
	if got := m.decided(); !maps.Equal(got, want) {
		t.Errorf("decisions = %v, want %v", got, want)
	}
}

// TestFirstSeenAdmitsThroughGCRA checks a first-seen key within the
// budget: full buckets, one GCRA call for the applied limit with the
// default burst, cost and the Policy's stateStoreTimeout, and the counts
// the script returned in the fields.
func TestFirstSeenAdmitsThroughGCRA(t *testing.T) {
	fx := newFixture(t, nil)
	f, m := fx.build(t, "rl", keyed(limit(10, time.Second)))
	o := fx.request(t, f, vars("a", ""))
	if o.r.Outcome != filter.Continue || !o.called {
		t.Fatalf("first request = %+v, called %v; want a GCRA call that admits", o.r, o.called)
	}
	c := o.call
	if c.Kind != statestore.OpGCRA || c.Timeout != stateTimeout || c.GCRA.Policy != "rl" || c.GCRA.Cost != 1 ||
		c.GCRA.Digest != statestore.DigestOf("a") ||
		!reflect.DeepEqual(c.GCRA.Limits, []statestore.GCRALimit{{Requests: 10, Window: time.Second, Burst: 10}}) {
		t.Fatalf("GCRA call = %+v", c)
	}
	want := []filter.RateLimitField{{Name: "rl", Q: 10, W: 1, R: 10, T: 1, Known: true}}
	if !reflect.DeepEqual(o.x.RateLimits, want) {
		t.Errorf("fields = %+v, want %+v", o.x.RateLimits, want)
	}
	if n, ok := fx.tokens(f, "a", 0); !ok || n != 9 {
		t.Errorf("bucket holds %d (%v), want 9 after one request", n, ok)
	}
	wantDecisions(t, m, map[string]uint64{"allow": 1})
}

// TestGCRADenyCachesOverLimit checks a GCRA denial: 429 RZ-RL-002 with
// Retry-After, the denied limit marked in the fields, then denials from
// the over-limit cache without a State Store call until the retry-after,
// and GCRA again after it.
func TestGCRADenyCachesOverLimit(t *testing.T) {
	fx := newFixture(t, nil)
	l := limit(2, time.Second)
	l.Burst = ptr(int64(0))
	f, m := fx.build(t, "rl", keyed(l))
	if o := fx.request(t, f, vars("a", "")); o.r.Outcome != filter.Continue {
		t.Fatalf("first request = %+v, want admitted", o.r)
	}
	o := fx.request(t, f, vars("a", ""))
	if o.status() != http.StatusTooManyRequests || o.code() != CodeGlobalDeny || !o.called || o.retryAfter() != "1" {
		t.Fatalf("second request = %d %s, called %v, Retry-After %q; want 429 RZ-RL-002 after a call, Retry-After 1",
			o.status(), o.code(), o.called, o.retryAfter())
	}
	want := []filter.RateLimitField{{Name: "rl", Q: 2, W: 1, R: 0, T: 1, Known: true, Denied: true}}
	if !reflect.DeepEqual(o.x.RateLimits, want) {
		t.Errorf("deny fields = %+v, want %+v", o.x.RateLimits, want)
	}
	trips := fx.store.roundTrips()
	o = fx.request(t, f, vars("a", ""))
	if o.code() != CodeGlobalDeny || o.called || fx.store.roundTrips() != trips {
		t.Fatalf("cached request = %s, called %v; want RZ-RL-002 from the over-limit cache, no call", o.code(), o.called)
	}
	fx.clk.Advance(500 * time.Millisecond)
	if o := fx.request(t, f, vars("a", "")); o.r.Outcome != filter.Continue || !o.called {
		t.Fatalf("after the retry-after = %+v, called %v; want admitted by GCRA", o.r, o.called)
	}
	// Another key is not affected by the cache.
	if o := fx.request(t, f, vars("b", "")); o.r.Outcome != filter.Continue {
		t.Fatalf("other key = %+v, want admitted", o.r)
	}
	wantDecisions(t, m, map[string]uint64{"allow": 3, "deny_global": 2})
}

// TestLocalBucketDeny checks a bucket that lacks the cost: 429 RZ-RL-001
// without a State Store call, Retry-After the wait for a token.
func TestLocalBucketDeny(t *testing.T) {
	fx := newFixture(t, nil)
	f, m := fx.build(t, "rl", keyed(limit(3, time.Second)))
	for i := range 3 {
		if o := fx.request(t, f, vars("a", "")); o.r.Outcome != filter.Continue {
			t.Fatalf("request %d = %+v, want admitted", i+1, o.r)
		}
	}
	trips := fx.store.roundTrips()
	o := fx.request(t, f, vars("a", ""))
	if o.status() != http.StatusTooManyRequests || o.code() != CodeLocalDeny || o.called || fx.store.roundTrips() != trips {
		t.Fatalf("fourth request = %d %s, called %v; want 429 RZ-RL-001 without a call", o.status(), o.code(), o.called)
	}
	if o.retryAfter() != "1" {
		t.Errorf("Retry-After %q, want 1 (a token in 333ms, rounded up)", o.retryAfter())
	}
	want := []filter.RateLimitField{{Name: "rl", Q: 3, W: 1, R: 0, T: 1, Known: true, Denied: true}}
	if !reflect.DeepEqual(o.x.RateLimits, want) {
		t.Errorf("fields = %+v, want %+v", o.x.RateLimits, want)
	}
	wantDecisions(t, m, map[string]uint64{"allow": 3, "deny_local": 1})
}

// TestCostTakesFromEveryBucketOrNone checks config.cost: each applied
// bucket gives cost tokens, the GCRA call charges cost intervals, and a
// bucket that lacks them denies without taking from the others.
func TestCostTakesFromEveryBucketOrNone(t *testing.T) {
	fx := newFixture(t, nil)
	cfg := keyed(limit(10, time.Second), limit(4, time.Second))
	cfg.Cost = ptr("3")
	f, m := fx.build(t, "rl", cfg)
	o := fx.request(t, f, vars("a", ""))
	if o.r.Outcome != filter.Continue || o.call.GCRA.Cost != 3 {
		t.Fatalf("first request = %+v with cost %d; want admitted charging 3", o.r, o.call.GCRA.Cost)
	}
	o = fx.request(t, f, vars("a", ""))
	if o.code() != CodeLocalDeny || o.called {
		t.Fatalf("second request = %s, called %v; want RZ-RL-001 by the 4/1s bucket", o.code(), o.called)
	}
	if n, _ := fx.tokens(f, "a", 0); n != 7 {
		t.Errorf("the 10/1s bucket holds %d after the denial, want 7: a denial takes from no bucket", n)
	}
	if !o.x.RateLimits[1].Denied || o.x.RateLimits[0].Denied || o.x.RateLimits[0].Known {
		t.Errorf("fields %+v: want the second limit denied and the first listed without a count", o.x.RateLimits)
	}
	wantDecisions(t, m, map[string]uint64{"allow": 1, "deny_local": 1})
}

// TestCostAboveCeilingNeverFits checks that a cost above an entry's
// ceiling is always denied there, the wait reported as the window.
func TestCostAboveCeilingNeverFits(t *testing.T) {
	fx := newFixture(t, nil)
	cfg := keyed(limit(10, time.Second))
	cfg.Cost = ptr("20")
	f, _ := fx.build(t, "rl", cfg)
	for range 2 {
		o := fx.request(t, f, vars("a", ""))
		if o.code() != CodeLocalDeny || o.called || o.retryAfter() != "1" {
			t.Fatalf("cost 20 over a ceiling of 10 = %s, called %v, Retry-After %q; want RZ-RL-001, 1", o.code(), o.called, o.retryAfter())
		}
		fx.clk.Advance(time.Hour)
	}
}

// TestCostExpression checks a computed cost: a POST costs five.
func TestCostExpression(t *testing.T) {
	fx := newFixture(t, nil)
	cfg := keyed(limit(10, time.Second))
	cfg.Cost = ptr(`request.method == "POST" ? 5 : 1`)
	f, _ := fx.build(t, "rl", cfg)
	v := vars("a", "")
	v.Request.Method = http.MethodPost
	if o := fx.request(t, f, v); o.call.GCRA.Cost != 5 {
		t.Fatalf("POST charged %d, want 5", o.call.GCRA.Cost)
	}
	if n, _ := fx.tokens(f, "a", 0); n != 5 {
		t.Errorf("bucket holds %d, want 5", n)
	}
}

// TestWhenSelectsLimits checks limits[].when: a false entry takes no part
// in the decision and lists in no field; the item names keep the entry's
// position.
func TestWhenSelectsLimits(t *testing.T) {
	fx := newFixture(t, nil)
	gold := limit(100, time.Second)
	gold.When = `consumer != null && consumer.tier == "gold"`
	free := limit(10, time.Second)
	free.When = `consumer == null || consumer.tier != "gold"`
	f, _ := fx.build(t, "rl", keyed(gold, free))
	o := fx.request(t, f, vars("acme", "gold"))
	if len(o.call.GCRA.Limits) != 1 || o.call.GCRA.Limits[0].Requests != 100 {
		t.Fatalf("gold GCRA limits = %+v, want the 100/1s entry only", o.call.GCRA.Limits)
	}
	if len(o.x.RateLimits) != 1 || o.x.RateLimits[0].Name != "rl.1" {
		t.Errorf("gold fields = %+v, want rl.1 only", o.x.RateLimits)
	}
	o = fx.request(t, f, vars("beta", "free"))
	if len(o.call.GCRA.Limits) != 1 || o.call.GCRA.Limits[0].Requests != 10 || o.x.RateLimits[0].Name != "rl.2" {
		t.Fatalf("free call %+v fields %+v, want the 10/1s entry, rl.2", o.call.GCRA.Limits, o.x.RateLimits)
	}
}

// TestNoAppliedLimitAdmits checks that a Policy whose every limit's when
// is false admits without a bucket, a call or a field.
func TestNoAppliedLimitAdmits(t *testing.T) {
	fx := newFixture(t, nil)
	l := limit(1, time.Second)
	l.When = `consumer != null`
	f, m := fx.build(t, "rl", keyed(l))
	for range 3 {
		o := fx.request(t, f, vars("a", ""))
		if o.r.Outcome != filter.Continue || o.called || len(o.x.RateLimits) != 0 {
			t.Fatalf("no applied limit = %+v, called %v, fields %v; want admitted without either", o.r, o.called, o.x.RateLimits)
		}
	}
	if _, ok := fx.tokens(f, "a", 0); ok {
		t.Error("a Policy without an applied limit created a bucket")
	}
	wantDecisions(t, m, map[string]uint64{"allow": 3})
}

// TestRuntimeErrorAppliesFailureMode checks that a runtime error of
// config.key, config.cost or a limits[].when, or a cost below 1, cannot
// decide with RZ-RL-005 under either failureMode, takes no bucket, counts
// fail_open or fail_closed, and lists the limits known to apply without a
// count.
func TestRuntimeErrorAppliesFailureMode(t *testing.T) {
	broken := limit(5, time.Second)
	broken.When = `request.headers["absent"] == "x"`
	cases := []struct {
		name    string
		cfg     *v1alpha1.RateLimitConfig
		errType string
		fields  []string
	}{
		{"key", &v1alpha1.RateLimitConfig{Key: `request.headers["absent"]`, Limits: []v1alpha1.RateLimit{limit(5, time.Second)}}, errTypeCEL, []string{"rl"}},
		{"cost", &v1alpha1.RateLimitConfig{Key: `request.headers["x-key"]`, Cost: ptr(`int(request.headers["absent"])`), Limits: []v1alpha1.RateLimit{limit(5, time.Second)}}, errTypeCEL, []string{"rl"}},
		{"cost type", &v1alpha1.RateLimitConfig{Key: `request.headers["x-key"]`, Cost: ptr(`dyn("1")`), Limits: []v1alpha1.RateLimit{limit(5, time.Second)}}, errTypeResultType, []string{"rl"}},
		{"cost below 1", &v1alpha1.RateLimitConfig{Key: `request.headers["x-key"]`, Cost: ptr(`0`), Limits: []v1alpha1.RateLimit{limit(5, time.Second)}}, errTypeInvalidValue, []string{"rl"}},
		{"when", &v1alpha1.RateLimitConfig{Key: `request.headers["x-key"]`, Limits: []v1alpha1.RateLimit{broken, limit(9, time.Second)}}, errTypeCEL, []string{"rl.2"}},
	}
	for _, tc := range cases {
		for _, mode := range []v1alpha1.FailureMode{v1alpha1.FailureModeOpen, v1alpha1.FailureModeClosed} {
			t.Run(tc.name+"/"+string(mode), func(t *testing.T) {
				fx := newFixture(t, nil)
				f, m := fx.buildMode(t, "rl", mode, tc.cfg)
				o := fx.request(t, f, vars("a", ""))
				if o.r.Outcome != filter.CannotDecide || o.code() != CodeUndecided || o.called {
					t.Fatalf("result = %+v, called %v; want CannotDecide RZ-RL-005 without a call", o.r, o.called)
				}
				var typed interface{ ErrorType() string }
				if !errors.As(o.r.Err, &typed) || typed.ErrorType() != tc.errType {
					t.Errorf("cause %v, want error.type %s", o.r.Err, tc.errType)
				}
				var names []string
				for _, rf := range o.x.RateLimits {
					if rf.Known {
						t.Errorf("field %+v carries a count", rf)
					}
					names = append(names, rf.Name)
				}
				if !reflect.DeepEqual(names, tc.fields) {
					t.Errorf("fields %v, want %v", names, tc.fields)
				}
				if _, ok := fx.tokens(f, "a", 0); ok {
					t.Error("a failed evaluation created a bucket")
				}
				want := "fail_open"
				if mode == v1alpha1.FailureModeClosed {
					want = "fail_closed"
				}
				wantDecisions(t, m, map[string]uint64{want: 1})
			})
		}
	}
}

// TestAbsentKeyIsOnePartition checks a service limit: without config.key,
// and for a key that evaluates empty, every request shares one partition.
func TestAbsentKeyIsOnePartition(t *testing.T) {
	fx := newFixture(t, nil)
	f, _ := fx.build(t, "svc", &v1alpha1.RateLimitConfig{Limits: []v1alpha1.RateLimit{limit(2, time.Second)}})
	if o := fx.request(t, f, vars("a", "")); o.call.GCRA.Digest != statestore.DigestOf("") {
		t.Fatalf("absent key digest %x, want the empty key's", o.call.GCRA.Digest)
	}
	fx.request(t, f, vars("b", ""))
	if o := fx.request(t, f, vars("c", "")); o.code() != CodeLocalDeny {
		t.Fatalf("third request on a 2/1s service limit = %s, want RZ-RL-001", o.code())
	}
	g, _ := fx.build(t, "empty", keyed(limit(1, time.Second)))
	if o := fx.request(t, g, vars("", "")); o.call.GCRA.Digest != statestore.DigestOf("") {
		t.Fatalf("empty key digest %x, want the empty key's", o.call.GCRA.Digest)
	}
}

// TestLocalOnlyPolicy checks config.localOnly: local buckets alone decide,
// with no State Store call, over-limit cache or first-seen budget, and the
// fields report the Node's bucket.
func TestLocalOnlyPolicy(t *testing.T) {
	fx := newFixture(t, nil)
	cfg := keyed(limit(10, time.Second))
	cfg.LocalOnly = ptr(true)
	f, m := fx.build(t, "rl", cfg)
	budget := fx.table.budgetTokens
	o := fx.request(t, f, vars("a", ""))
	want := []filter.RateLimitField{{Name: "rl", Q: 10, W: 1, R: 9, T: 1, Known: true}}
	if o.r.Outcome != filter.Continue || o.called || !reflect.DeepEqual(o.x.RateLimits, want) {
		t.Fatalf("localOnly request = %+v, called %v, fields %+v; want admitted locally with %+v", o.r, o.called, o.x.RateLimits, want)
	}
	for range 9 {
		fx.request(t, f, vars("a", ""))
	}
	if o := fx.request(t, f, vars("a", "")); o.code() != CodeLocalDeny || o.called {
		t.Fatalf("11th request = %s, called %v; want RZ-RL-001", o.code(), o.called)
	}
	if fx.store.roundTrips() != 0 || fx.table.budgetTokens != budget {
		t.Errorf("localOnly made %d calls and spent the first-seen budget (%d -> %d)", fx.store.roundTrips(), budget, fx.table.budgetTokens)
	}
	wantDecisions(t, m, map[string]uint64{"allow_local": 10, "deny_local": 1})
}

// TestFirstSeenPastBudget checks the first-seen budget: in file mode 200
// new keys per second get full buckets; the next key gets a local-only
// entry that decides without GCRA for 10 s, then is first-seen again.
func TestFirstSeenPastBudget(t *testing.T) {
	fx := newFixture(t, nil)
	f, m := fx.build(t, "rl", keyed(limit(5, time.Second)))
	for i := range FirstSeenNoCount {
		if o := fx.request(t, f, vars("k"+strconv.Itoa(i), "")); !o.called {
			t.Fatalf("key %d within the budget made no GCRA call", i)
		}
	}
	trips := fx.store.roundTrips()
	o := fx.request(t, f, vars("late", ""))
	want := []filter.RateLimitField{{Name: "rl", Q: 5, W: 1, R: 4, T: 1, Known: true}}
	if o.r.Outcome != filter.Continue || o.called || !reflect.DeepEqual(o.x.RateLimits, want) {
		t.Fatalf("key past the budget = %+v, called %v, fields %+v; want a local-only admission %+v", o.r, o.called, o.x.RateLimits, want)
	}
	for range 4 {
		fx.request(t, f, vars("late", ""))
	}
	if o := fx.request(t, f, vars("late", "")); o.code() != CodeLocalDeny {
		t.Fatalf("local-only entry past its ceiling = %s, want RZ-RL-001", o.code())
	}
	fx.clk.Advance(LocalOnlyLife - time.Millisecond)
	if o := fx.request(t, f, vars("late", "")); o.called {
		t.Fatal("the local-only entry ended before 10 s")
	}
	if fx.store.roundTrips() != trips {
		t.Fatalf("local-only entry made %d calls", fx.store.roundTrips()-trips)
	}
	fx.clk.Advance(time.Millisecond)
	if o := fx.request(t, f, vars("late", "")); !o.called {
		t.Fatal("after 10 s the key, first-seen again with budget, made no GCRA call")
	}
	wantDecisions(t, m, map[string]uint64{"allow": 201, "allow_local": 6, "deny_local": 1})
}

// TestFirstSeenBudgetWithCount checks the budget with a published count:
// min(500, B / N) per second.
func TestFirstSeenBudgetWithCount(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want int
	}{{10, 500}, {100, 200}, {1000, 20}} {
		clk := newFixture(t, nil).clk
		tbl := smallTable(t, clk, &nodeCount{n: tc.n, control: true})
		now := clk.Now().UnixNano()
		got := 0
		for tbl.takeFirstSeen(now) {
			got++
		}
		if got != tc.want {
			t.Errorf("N = %d: %d first-seen keys in one instant, want %d", tc.n, got, tc.want)
		}
	}
}

// TestFailOpenRefill checks failureMode open on a failed call: the
// request is admitted (CannotDecide with the call's RZ-STS code), and the
// key's buckets switch to the fail-open refill: no refill for the rest of
// the epoch-aligned window, the allowance at the next window without
// carry-over, until a GCRA answer switches them back.
func TestFailOpenRefill(t *testing.T) {
	fx := newFixture(t, nil)
	f, m := fx.build(t, "rl", keyed(limit(10, time.Second)))
	fx.store.failWith(statestore.ErrTimeout)
	o := fx.request(t, f, vars("a", ""))
	if o.r.Outcome != filter.CannotDecide || o.code() != "RZ-STS-001" || !errors.Is(o.r.Err, statestore.ErrTimeout) {
		t.Fatalf("failed call = %+v, want CannotDecide RZ-STS-001", o.r)
	}
	if len(o.x.RateLimits) != 1 || o.x.RateLimits[0].Known {
		t.Errorf("fields %+v, want the limit without a count", o.x.RateLimits)
	}
	if !fx.failingOpen(f) {
		t.Fatal("the bucket is not on the fail-open refill")
	}
	for range 9 {
		fx.request(t, f, vars("a", ""))
	}
	if o := fx.request(t, f, vars("a", "")); o.code() != CodeLocalDeny {
		t.Fatalf("11th request in the window = %s, want RZ-RL-001", o.code())
	}
	// The continuous refill would have added 4 tokens in 400 ms.
	fx.clk.Advance(400 * time.Millisecond)
	if o := fx.request(t, f, vars("a", "")); o.code() != CodeLocalDeny {
		t.Fatalf("400ms later in the same window = %s, want RZ-RL-001: no refill on the fail-open refill", o.code())
	}
	fx.clk.Advance(100 * time.Millisecond) // the next window
	if n, _ := fx.tokens(f, "a", 0); n != 10 {
		t.Fatalf("next window holds %d, want the allowance 10", n)
	}
	fx.store.failWith(nil)
	if o := fx.request(t, f, vars("a", "")); o.r.Outcome != filter.Continue || fx.failingOpen(f) {
		t.Fatalf("after a GCRA answer = %+v, fail-open %v; want admitted on the continuous refill", o.r, fx.failingOpen(f))
	}
	wantDecisions(t, m, map[string]uint64{"fail_open": 10, "deny_local": 2, "allow": 1})
}

// TestFailOpenClampsDerivedAllowance checks the fail-open allowance of a
// derived ceiling: 10 per 1s on 100 Nodes is a ceiling of 10, the floor,
// but each Node admits max(1, 2 × 10 / 100) = 1 per aligned window while
// failing open.
func TestFailOpenClampsDerivedAllowance(t *testing.T) {
	fx := newFixture(t, &nodeCount{n: 100, control: true})
	f, _ := fx.build(t, "rl", keyed(limit(10, time.Second)))
	fx.store.failWith(statestore.ErrBreakerOpen)
	if o := fx.request(t, f, vars("a", "")); o.code() != "RZ-STS-003" {
		t.Fatalf("breaker open = %s, want RZ-STS-003", o.code())
	}
	if n, _ := fx.tokens(f, "a", 0); n != 1 {
		t.Fatalf("after the switch the bucket holds %d, want the allowance 1", n)
	}
	fx.request(t, f, vars("a", ""))
	if o := fx.request(t, f, vars("a", "")); o.code() != CodeLocalDeny {
		t.Fatalf("third request = %s, want RZ-RL-001 at the allowance", o.code())
	}
	fx.clk.Advance(500 * time.Millisecond)
	if n, _ := fx.tokens(f, "a", 0); n != 1 {
		t.Fatalf("next window holds %d, want 1", n)
	}
}

// TestFailOpenCountlessAllowance checks a Control-mode Node without a
// count: the full limit as ceiling, max(1, requests / 100) while failing
// open.
func TestFailOpenCountlessAllowance(t *testing.T) {
	fx := newFixture(t, &nodeCount{control: true})
	f, _ := fx.build(t, "rl", keyed(limit(500, time.Second)))
	fx.store.failWith(statestore.ErrNotAttempted)
	fx.request(t, f, vars("a", ""))
	if n, _ := fx.tokens(f, "a", 0); n != 5 {
		t.Fatalf("countless fail-open bucket holds %d, want min(499, 500 / 100) = 5", n)
	}
}

// TestClosedFailure checks failureMode closed on a failed call: CannotDecide
// with the call's RZ-STS code (the executor answers 503), fail_closed, and
// no switch to the fail-open refill.
func TestClosedFailure(t *testing.T) {
	fx := newFixture(t, nil)
	f, m := fx.buildMode(t, "rl", v1alpha1.FailureModeClosed, keyed(limit(10, time.Second)))
	fx.store.failWith(statestore.ErrFailed)
	o := fx.request(t, f, vars("a", ""))
	if o.r.Outcome != filter.CannotDecide || o.code() != "RZ-STS-002" {
		t.Fatalf("closed failure = %+v, want CannotDecide RZ-STS-002", o.r)
	}
	if fx.failingOpen(f) {
		t.Error("a closed failure switched the bucket to the fail-open refill")
	}
	wantDecisions(t, m, map[string]uint64{"fail_closed": 1})
}

// TestUndoReturnsTokens checks Consumptive.Undo: the tokens Prepare took
// go back to every applied bucket, once.
func TestUndoReturnsTokens(t *testing.T) {
	fx := newFixture(t, nil)
	cfg := keyed(limit(10, time.Second), limit(5, time.Minute))
	cfg.Cost = ptr("2")
	f, _ := fx.build(t, "rl", cfg)
	x := &filtertest.Exchange{VarsV: vars("a", "")}
	var call statestore.Call
	if _, done := f.Prepare(context.Background(), x, &call); done {
		t.Fatal("Prepare decided; want a pending GCRA call")
	}
	if n, _ := fx.tokens(f, "a", 1); n != 3 {
		t.Fatalf("after Prepare the 5/1m bucket holds %d, want 3", n)
	}
	f.Undo(x)
	f.Undo(x)
	for i, want := range []int64{10, 5} {
		if n, _ := fx.tokens(f, "a", i); n != want {
			t.Errorf("after Undo bucket %d holds %d, want %d", i, n, want)
		}
	}
	f.Finish(context.Background(), x)
	if x.State != nil {
		t.Error("Finish left the request state in the slot")
	}
}

// TestResponseHeadersNone checks config.responseHeaders none: no field is
// recorded, and a 429 still carries Retry-After.
func TestResponseHeadersNone(t *testing.T) {
	fx := newFixture(t, nil)
	cfg := keyed(limit(1, time.Second))
	cfg.ResponseHeaders = ptr(v1alpha1.RateLimitResponseHeadersNone)
	f, _ := fx.build(t, "rl", cfg)
	if o := fx.request(t, f, vars("a", "")); len(o.x.RateLimits) != 0 {
		t.Fatalf("none recorded %+v", o.x.RateLimits)
	}
	o := fx.request(t, f, vars("a", ""))
	if o.status() != http.StatusTooManyRequests || o.retryAfter() != "1" || len(o.x.RateLimits) != 0 {
		t.Fatalf("429 under none = %d, Retry-After %q, fields %+v", o.status(), o.retryAfter(), o.x.RateLimits)
	}
}

// TestFieldNamesAndWindows checks the RateLimit-Policy items: the Policy
// name suffixed by position when it holds several limits, q the requests,
// w the window in seconds rounded up.
func TestFieldNamesAndWindows(t *testing.T) {
	fx := newFixture(t, nil)
	f, _ := fx.build(t, "ratelimit-gold", keyed(limit(100, time.Second), limit(5000, time.Minute), limit(7, 500*time.Millisecond)))
	o := fx.request(t, f, vars("a", ""))
	var got []string
	for _, rf := range o.x.RateLimits {
		got = append(got, rf.Name+";q="+strconv.FormatInt(rf.Q, 10)+";w="+strconv.FormatInt(rf.W, 10))
	}
	want := []string{"ratelimit-gold.1;q=100;w=1", "ratelimit-gold.2;q=5000;w=60", "ratelimit-gold.3;q=7;w=1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
}

// TestDerivedCeiling checks a derived per-Node ceiling: 100 per 1s on 10
// Nodes allows ceil(2 × 100 / 10) = 20 per Node.
func TestDerivedCeiling(t *testing.T) {
	fx := newFixture(t, &nodeCount{n: 10, control: true})
	f, _ := fx.build(t, "rl", keyed(limit(100, time.Second)))
	for i := range 20 {
		if o := fx.request(t, f, vars("a", "")); o.r.Outcome != filter.Continue {
			t.Fatalf("request %d = %+v, want admitted within the ceiling", i+1, o.r)
		}
	}
	if o := fx.request(t, f, vars("a", "")); o.code() != CodeLocalDeny {
		t.Fatalf("21st request = %s, want RZ-RL-001 at the derived ceiling", o.code())
	}
}

// TestDeclaredCeiling checks limits[].perNodeCeiling as the bucket's
// capacity and refill.
func TestDeclaredCeiling(t *testing.T) {
	fx := newFixture(t, nil)
	l := limit(100, time.Second)
	l.PerNodeCeiling = ptr(int64(3))
	f, _ := fx.build(t, "rl", keyed(l))
	for range 3 {
		fx.request(t, f, vars("a", ""))
	}
	if o := fx.request(t, f, vars("a", "")); o.code() != CodeLocalDeny {
		t.Fatalf("4th request = %s, want RZ-RL-001 at the declared ceiling", o.code())
	}
}

// TestHandleStandalone checks Handle: it decides in onRequestHeaders with
// its own round trip, and continues in any other Phase.
func TestHandleStandalone(t *testing.T) {
	fx := newFixture(t, nil)
	f, _ := fx.build(t, "rl", keyed(limit(1, time.Second)))
	rb := statestore.NewRequestBudget(time.Second, 0)
	x := &filtertest.Exchange{VarsV: vars("a", ""), StateDeadline: &rb}
	if r := f.Handle(context.Background(), phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue", r)
	}
	if fx.store.roundTrips() != 1 {
		t.Fatalf("Handle made %d round trips, want 1", fx.store.roundTrips())
	}
	if r := f.Handle(context.Background(), phase.OnResponse, x); r.Outcome != filter.Continue {
		t.Fatalf("Handle in onResponse = %+v, want Continue", r)
	}
	f.Finish(context.Background(), x)
	// Without a budget the call is not attempted: failureMode open admits.
	x = &filtertest.Exchange{VarsV: vars("b", "")}
	if r := f.Handle(context.Background(), phase.OnRequestHeaders, x); r.Outcome != filter.CannotDecide || r.Code != "RZ-STS-004" {
		t.Fatalf("Handle without a budget = %+v, want CannotDecide RZ-STS-004", r)
	}
}

// TestHotReloadKeepsBuckets checks that a Filter built again for the same
// Policy and limits shares its buckets, that closing the older Filter
// keeps them, and that a changed limit starts fresh.
func TestHotReloadKeepsBuckets(t *testing.T) {
	fx := newFixture(t, nil)
	cfg := keyed(limit(3, time.Second))
	cfg.LocalOnly = ptr(true)
	f1, _ := fx.build(t, "rl", cfg)
	fx.request(t, f1, vars("a", ""))
	fx.request(t, f1, vars("a", ""))
	f2, _ := fx.build(t, "rl", cfg)
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}
	fx.request(t, f2, vars("a", ""))
	if o := fx.request(t, f2, vars("a", "")); o.code() != CodeLocalDeny {
		t.Fatalf("4th request across the reload = %s, want RZ-RL-001: the bucket survived", o.code())
	}
	changed := keyed(limit(4, time.Second))
	changed.LocalOnly = ptr(true)
	f3, _ := fx.build(t, "rl", changed)
	if o := fx.request(t, f3, vars("a", "")); o.r.Outcome != filter.Continue {
		t.Fatalf("a changed limit = %+v, want a fresh bucket", o.r)
	}
	if fx.table.limits() != 2 {
		t.Errorf("table holds %d limits, want 2", fx.table.limits())
	}
	if err := f2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f3.Close(); err != nil {
		t.Fatal(err)
	}
	if fx.table.limits() != 0 {
		t.Errorf("after every Close the table holds %d limits", fx.table.limits())
	}
}

// TestBuildRefusals checks the Build errors: the config rules and an
// empty cost, with or without a CEL builder (RZ-CFG-005), expressions that
// do not compile (RZ-CFG-014, RZ-CFG-015), a placement where the Policy
// never decides (RZ-CFG-020), and the programming errors without a code.
func TestBuildRefusals(t *testing.T) {
	fx := newFixture(t, nil)
	nine := make([]v1alpha1.RateLimit, 9)
	for i := range nine {
		nine[i] = limit(int64(i+1), time.Second)
	}
	withCeil := limit(5, time.Second)
	withCeil.PerNodeCeiling = ptr(int64(6))
	withBurst := limit(5, time.Second)
	withBurst.Burst = ptr(int64(6))
	badWhen := limit(5, time.Second)
	badWhen.When = `request.method`
	cases := []struct {
		name string
		cfg  any
		edit func(*filter.BuildEnv)
		code string
	}{
		{"wrong config type", &v1alpha1.QuotaConfig{}, nil, CodeConfig},
		{"nine limits", &v1alpha1.RateLimitConfig{Limits: nine}, nil, CodeConfig},
		{"zero window", keyed(limit(5, 0)), nil, CodeConfig},
		{"fractional ms window", keyed(limit(5, 1500*time.Microsecond)), nil, CodeConfig},
		{"interval below 1µs", keyed(limit(2000, time.Millisecond)), nil, CodeConfig},
		{"ceiling above requests", keyed(withCeil), nil, CodeConfig},
		{"burst above requests", keyed(withBurst), nil, CodeConfig},
		{"duplicate limits", keyed(limit(5, time.Second), limit(5, time.Second)), nil, CodeConfig},
		{"key does not compile", &v1alpha1.RateLimitConfig{Key: `request.`, Limits: []v1alpha1.RateLimit{limit(5, time.Second)}}, nil, CodeCEL},
		{"empty cost", &v1alpha1.RateLimitConfig{Cost: ptr(""), Limits: []v1alpha1.RateLimit{limit(5, time.Second)}}, nil, CodeConfig},
		{"empty cost without CEL", &v1alpha1.RateLimitConfig{Cost: ptr(""), Limits: []v1alpha1.RateLimit{limit(5, time.Second)}}, func(e *filter.BuildEnv) { e.CEL = nil }, CodeConfig},
		{"cost not an int", &v1alpha1.RateLimitConfig{Cost: ptr(`"1"`), Limits: []v1alpha1.RateLimit{limit(5, time.Second)}}, nil, CodeCEL},
		{"when not a bool", keyed(badWhen), nil, CodeCEL},
		{"cost over the ceiling", &v1alpha1.RateLimitConfig{Cost: ptr(`size(request.headers.filter(a, request.headers.exists(b, request.headers.exists(c, a + b + c == ""))))`), Limits: []v1alpha1.RateLimit{limit(5, time.Second)}}, nil, CodeCost},
		{"upstream scope", keyed(limit(5, time.Second)), func(e *filter.BuildEnv) { e.Scope = phase.ScopeUpstream }, CodeScope},
		{"no onRequestHeaders", keyed(limit(5, time.Second)), func(e *filter.BuildEnv) { e.Phases = phase.Of(phase.OnLog) }, CodeScope},
	}
	for _, tc := range cases {
		env := fx.env(policy("rl", v1alpha1.FailureModeOpen), nil, nil)
		env.Config = tc.cfg
		if tc.edit != nil {
			tc.edit(&env)
		}
		_, err := Factory{Table: fx.table}.Build(context.Background(), env)
		if got, _ := errcode.CodeOf(err); got != tc.code {
			t.Errorf("%s: Build error %v (code %q), want %s", tc.name, err, got, tc.code)
		}
	}
	if fx.table.limits() != 0 {
		t.Errorf("refused Builds hold %d table limits", fx.table.limits())
	}
	env := fx.env(policy("rl", v1alpha1.FailureModeOpen), keyed(limit(5, time.Second)), nil)
	if _, err := (Factory{}).Build(context.Background(), env); !errors.Is(err, ErrNoTable) {
		t.Errorf("Build without a table = %v, want ErrNoTable", err)
	}
	env.CEL = nil
	if _, err := (Factory{Table: fx.table}).Build(context.Background(), env); !errors.Is(err, ErrNoCEL) {
		t.Errorf("Build of a key without CEL = %v, want ErrNoCEL", err)
	}
	env.Config = &v1alpha1.RateLimitConfig{Limits: []v1alpha1.RateLimit{limit(5, time.Second)}}
	if _, err := (Factory{Table: fx.table}).Build(context.Background(), env); err != nil {
		t.Errorf("Build of a keyless default-cost Policy without CEL = %v, want none", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Factory{Table: fx.table}).Build(ctx, env); !errors.Is(err, context.Canceled) {
		t.Errorf("Build under an ended context = %v", err)
	}
}

// TestDigestOfMatchesStatestore checks the stack-buffer digest against
// statestore.DigestOf, for short and long key values.
func TestDigestOfMatchesStatestore(t *testing.T) {
	long := string(make([]byte, digestBuffer+1))
	for _, s := range []string{"", "a", "2001:db8:1:2::/64", long[:digestBuffer], long} {
		if digestOf(s) != statestore.DigestOf(s) {
			t.Errorf("digestOf of %d bytes differs from statestore.DigestOf", len(s))
		}
	}
}

// TestSiteRefinesPlaces checks that Build compiles at the sites
// BuildEnv.Site gives, one per place.
func TestSiteRefinesPlaces(t *testing.T) {
	fx := newFixture(t, nil)
	cfg := keyed(limit(5, time.Second))
	cfg.Cost = ptr("2")
	cfg.Limits[0].When = "true"
	env := fx.env(policy("rl", v1alpha1.FailureModeOpen), cfg, nil)
	var places []expr.PlaceID
	env.Site = func(p expr.PlaceID) expr.Site {
		places = append(places, p)
		return expr.Site{Place: p, Scope: phase.ScopeGateway, PolicyType: Type}
	}
	if _, err := (Factory{Table: fx.table}).Build(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	want := []expr.PlaceID{expr.PlaceRateLimitKey, expr.PlaceRateLimitCost, expr.PlaceRateLimitWhen}
	if !reflect.DeepEqual(places, want) {
		t.Errorf("sites asked for %v, want %v", places, want)
	}
}
