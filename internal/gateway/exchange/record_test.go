// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/accesslog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestShortCircuitIsFirstResponder checks the access record's
// short_circuit: the Policy and Phase that responded; a request Phase ends
// at its short-circuit, so the first recorded is the request's.
func TestShortCircuitIsFirstResponder(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	if _, _, _, ok := x.ShortCircuit(); ok {
		t.Fatal("ShortCircuit() reports a responder before any")
	}
	auth := testPolicy("jwt", 0, phase.ClassAuth, phase.ScopeRoute, 0)
	x.RecordShortCircuit(auth, phase.OnRequestHeaders, http.StatusUnauthorized)
	x.RecordShortCircuit(testPolicy("late", 1, phase.ClassCache, phase.ScopeRoute, 0), phase.OnRequestBody, http.StatusOK)
	name, ph, status, ok := x.ShortCircuit()
	if !ok || name != "jwt" || ph != phase.OnRequestHeaders || status != http.StatusUnauthorized {
		t.Errorf("ShortCircuit() = %q %s %d %v, want jwt onRequestHeaders 401", name, ph, status, ok)
	}
}

// TestFailureModesCapped checks the access record's failure_modes: the
// undecided Policies with the failureMode applied, in record order, at
// most 8 (target) plus one entry when more were recorded, so the writer
// flags the list as truncated.
func TestFailureModesCapped(t *testing.T) {
	for _, n := range []int{0, 1, maxFailureModes, maxFailureModes + 1, 12} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			f := newFixture(t)
			x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
			defer f.pool.Put(x)
			for i := range n {
				p := testPolicy(fmt.Sprint("p", i), i, phase.ClassTransform, phase.ScopeRoute, i)
				x.RecordFailure(p, phase.OnResponse, v1alpha1.FailureModeOpen)
			}
			got := x.FailureModes()
			if want := min(n, maxFailureModes+1); len(got) != want {
				t.Fatalf("len(FailureModes()) = %d, want %d", len(got), want)
			}
			for i, e := range got {
				if e != (emit.FailureModeEntry{Policy: fmt.Sprint("p", i), Phase: phase.OnResponse, Mode: "open"}) {
					t.Errorf("entry %d = %+v", i, e)
				}
			}
		})
	}
}

// TestFillAccessFeedsAccessRecord checks that FillAccess sets the access
// record fields the exchange owns (docs/architecture/10-observability.md
// "Access logs") and that the writer's encoding of them names the
// short-circuit, lists 8 failure modes and flags the longer list as
// truncated.
func TestFillAccessFeedsAccessRecord(t *testing.T) {
	f := newFixture(t)
	r := serverRequest(http.MethodPost, "/v1/orders?x=1", "", http.Header{"User-Agent": {"curl/8"}})
	rt := testRoute("orders", snapshot.BodyNeeds{})
	x := f.routed(r, rt)
	defer f.pool.Put(x)
	_ = x.Exchange().SetIdentity(&filter.Identity{Method: filter.MethodAPIKey, Consumer: &expr.Consumer{Name: "acme", Tier: "gold"}})
	x.RecordShortCircuit(testPolicy("rl", 0, phase.ClassAdmission, phase.ScopeRoute, 0), phase.OnRequestHeaders, http.StatusTooManyRequests)
	for i := range 10 {
		x.RecordFailure(testPolicy(fmt.Sprint("f", i), i+1, phase.ClassTransform, phase.ScopeRoute, i), phase.OnRequestHeaders, v1alpha1.FailureModeOpen)
	}
	var rec emit.AccessRecord
	x.FillAccess(&rec)
	if rec.Route != "orders" || rec.Listener != "public" || rec.Method != http.MethodPost || rec.Host != "api.example.com" ||
		rec.Path != "/v1/orders" || rec.UserAgent != "curl/8" || rec.Consumer != "acme" || rec.Tier != "gold" ||
		rec.AuthMethod != filter.MethodAPIKey || rec.TraceID != testTraceID() || !rec.Start.Equal(testStart()) {
		t.Errorf("FillAccess() = %+v", rec)
	}
	if rec.ShortCircuitPolicy != "rl" || rec.ShortCircuitPhase != phase.OnRequestHeaders || len(rec.FailureModes) != maxFailureModes+1 {
		t.Errorf("records: %q %s %d failure modes", rec.ShortCircuitPolicy, rec.ShortCircuitPhase, len(rec.FailureModes))
	}
	var line struct {
		Truncated    []string `json:"truncated"`
		ShortCircuit struct {
			Policy string `json:"policy"`
			Phase  string `json:"phase"`
		} `json:"short_circuit"`
		FailureModes []struct {
			Policy, Phase, Mode string
		} `json:"failure_modes"`
	}
	if err := json.Unmarshal(accesslog.AppendJSON(nil, &rec, "node"), &line); err != nil {
		t.Fatal(err)
	}
	if line.ShortCircuit.Policy != "rl" || line.ShortCircuit.Phase != "onRequestHeaders" {
		t.Errorf("short_circuit = %+v", line.ShortCircuit)
	}
	if len(line.FailureModes) != maxFailureModes || !slices.Contains(line.Truncated, "failure_modes") {
		t.Errorf("failure_modes %d entries, truncated %v", len(line.FailureModes), line.Truncated)
	}

	unmatched := f.begin(serverRequest(http.MethodGet, "/x", "", nil))
	defer f.pool.Put(unmatched)
	var u emit.AccessRecord
	unmatched.FillAccess(&u)
	if u.Route != "_unmatched" || u.ShortCircuitPolicy != "" || len(u.FailureModes) != 0 {
		t.Errorf("unmatched record = %+v", u)
	}
}

// TestLegRecordsMergeAtRelease checks that a leg's failures and
// short-circuit join the request's records when the leg is released, from
// concurrent legs too (run under -race); the short-circuit kept is the
// first in request order, whatever order the legs end in.
func TestLegRecordsMergeAtRelease(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	early := testPolicy("upstream-oauth", 0, phase.ClassUpstreamAuth, phase.ScopeUpstream, 0)
	late := testPolicy("headers-up", 1, phase.ClassTransform, phase.ScopeUpstream, 0)
	const legs = 6
	var wg sync.WaitGroup
	for i := range legs {
		l := x.Leg("u", fmt.Sprint("s", i))
		wg.Go(func() {
			p := late
			if i == legs-1 {
				p = early
			}
			l.RecordFailure(p, phase.OnUpstreamRequest, v1alpha1.FailureModeOpen)
			l.RecordShortCircuit(p, phase.OnUpstreamRequest, http.StatusForbidden)
			if fm := x.FailureModes(); len(fm) > legs {
				t.Errorf("records grew past the legs: %d", len(fm))
			}
			l.Release()
			l.Release() // a second Release does nothing
		})
	}
	wg.Wait()
	if n := len(x.FailureModes()); n != legs {
		t.Errorf("merged %d failure modes, want %d", n, legs)
	}
	if name, ph, _, ok := x.ShortCircuit(); !ok || name != "upstream-oauth" || ph != phase.OnUpstreamRequest {
		t.Errorf("ShortCircuit() = %q %s %v, want the first in request order", name, ph, ok)
	}
}

// TestRejectedAfter checks Final.RejectedAfter, which quota refunds read
// (docs/architecture/09-traffic-management-and-resilience.md "Quotas":
// refunds after a later Filter rejection, cache hits are charged): true
// for the calling Policy when a Policy later in request order ended the
// request in a request Phase with a status of 400 or more or a closed
// failure, including a leg of plain upstreams; never for a cache hit, an
// open failure, a response-Phase failure, an earlier Policy, the rejector
// itself or a composition step's leg.
func TestRejectedAfter(t *testing.T) {
	cors := testPolicy("cors", 0, phase.ClassCORS, phase.ScopeRoute, 0)
	quota := testPolicy("quota", 1, phase.ClassAdmission, phase.ScopeRoute, 0)
	schema := testPolicy("schema", 2, phase.ClassValidation, phase.ScopeRoute, 0)
	cache := testPolicy("cache", 3, phase.ClassCache, phase.ScopeRoute, 0)
	oauth := testPolicy("oauth", 0, phase.ClassUpstreamAuth, phase.ScopeUpstream, 0)
	type event func(x *Request)
	short := func(p *snapshot.Policy, ph phase.Phase, status int) event {
		return func(x *Request) { x.RecordShortCircuit(p, ph, status) }
	}
	fail := func(p *snapshot.Policy, ph phase.Phase, mode v1alpha1.FailureMode) event {
		return func(x *Request) { x.RecordFailure(p, ph, mode) }
	}
	legFail := func(step string) event {
		return func(x *Request) {
			l := x.Leg("orders", step)
			l.RecordFailure(oauth, phase.OnUpstreamRequest, v1alpha1.FailureModeClosed)
			l.Release()
		}
	}
	for _, tc := range []struct {
		name   string
		ev     event
		caller *snapshot.Policy
		want   bool
	}{
		{"later deny", short(schema, phase.OnRequestBody, http.StatusBadRequest), quota, true},
		{"rejector itself", short(schema, phase.OnRequestBody, http.StatusBadRequest), schema, false},
		{"Policy after the rejector", short(schema, phase.OnRequestBody, http.StatusBadRequest), cache, false},
		{"cache hit", short(cache, phase.OnRequestHeaders, http.StatusOK), quota, false},
		{"only-if-cached miss", short(cache, phase.OnRequestHeaders, http.StatusGatewayTimeout), quota, true},
		{"earlier deny", short(cors, phase.OnRequestHeaders, http.StatusForbidden), quota, false},
		{"later closed failure", fail(schema, phase.OnRequestBody, v1alpha1.FailureModeClosed), quota, true},
		{"later open failure", fail(schema, phase.OnRequestBody, v1alpha1.FailureModeOpen), quota, false},
		{"response Phase failure", fail(cache, phase.OnResponse, v1alpha1.FailureModeClosed), quota, false},
		{"plain upstreams leg", legFail(""), quota, true},
		{"composition step leg", legFail("inventory"), quota, false},
		{"no calling Policy", short(schema, phase.OnRequestBody, http.StatusBadRequest), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
			defer f.pool.Put(x)
			tc.ev(x)
			x.Enter(tc.caller, phase.OnLog, nil)
			if got := x.Exchange().Final().RejectedAfter; got != tc.want {
				t.Errorf("RejectedAfter = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCacheResultFeedsAccessRecord checks filter.Exchange.SetCacheResult:
// the client view records the Response Cache result the access record's
// cache field reads (a later call replaces it), a leg view records
// nothing, and Put forgets it.
func TestCacheResultFeedsAccessRecord(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	x.Exchange().SetCacheResult("miss")
	l := x.Leg("orders", "")
	l.Exchange().SetCacheResult("hit")
	l.Release()
	var rec emit.AccessRecord
	x.FillAccess(&rec)
	if rec.Cache != "miss" {
		t.Errorf("cache = %q, want miss (a leg records nothing)", rec.Cache)
	}
	x.Exchange().SetCacheResult("stale")
	x.FillAccess(&rec)
	if rec.Cache != "stale" {
		t.Errorf("cache after a second result = %q, want stale", rec.Cache)
	}
	f.pool.Put(x)
	y := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(y)
	var next emit.AccessRecord
	y.FillAccess(&next)
	if next.Cache != "" {
		t.Errorf("cache of the next request = %q, want none", next.Cache)
	}
}
