// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package expr

import (
	"errors"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for architecture section 2.7 (WP-01) and spec 03 section B
// (requirements 9 to 16, the place table): Places sorted and unique, every
// place with the variables, result, rule and runtime of 03 B req 9, lazy
// request.query, Vars.Reset and header joining.

func TestPlacesTable(t *testing.T) {
	base := VarsBase
	// 03 B req 9, row by row.
	want := map[PlaceID]Place{
		PlaceRouteMatchWhen:         {PlaceRouteMatchWhen, ResultBool, VarRequest | VarSource | VarNow, RuleInternalError, "M1"},
		PlacePolicyWhen:             {PlacePolicyWhen, ResultBool, base | VarResponse, RulePolicyWhen, "M1"},
		PlaceStepPathExpression:     {PlaceStepPathExpression, ResultString, base | VarSteps, RuleStepFails, "M1"},
		PlaceStepWhen:               {PlaceStepWhen, ResultBool, base | VarSteps, RuleStepFails, "M1"},
		PlaceAuthzCELRule:           {PlaceAuthzCELRule, ResultBool, base, RuleAuthzUndecided, "M1"},
		PlaceRateLimitKey:           {PlaceRateLimitKey, ResultString, base, RuleFailureMode, "M1"},
		PlaceQuotaKey:               {PlaceQuotaKey, ResultString, base, RuleFailureMode, "M1"},
		PlaceHeadersRequestValue:    {PlaceHeadersRequestValue, ResultString, base, RuleFailureMode, "M1"},
		PlaceHeadersResponseValue:   {PlaceHeadersResponseValue, ResultString, base | VarResponse | VarUpstream, RuleFailureMode, "M1"},
		PlaceCacheKey:               {PlaceCacheKey, ResultString, base, RuleCacheBypass, "M1"},
		PlaceTransformRequestBody:   {PlaceTransformRequestBody, ResultDyn, base | VarUpstream, RuleFailureMode, "M1"},
		PlaceTransformRequestValue:  {PlaceTransformRequestValue, ResultString, base | VarUpstream, RuleFailureMode, "M1"},
		PlaceTransformResponseBody:  {PlaceTransformResponseBody, ResultDyn, base | VarResponse | VarUpstream, RuleFailureMode, "M1"},
		PlaceTransformResponseValue: {PlaceTransformResponseValue, ResultString, base | VarResponse | VarUpstream, RuleFailureMode, "M1"},
		PlaceHashKey:                {PlaceHashKey, ResultString, base, RuleRandomEndpoint, "M1"},
		PlaceRetryOn:                {PlaceRetryOn, ResultBool, VarRequest | VarResponse | VarError | VarAttempt | VarUpstream, RuleNoRetry, "M1"},
		PlaceFailureWhen:            {PlaceFailureWhen, ResultBool, VarRequest | VarResponse | VarError | VarUpstream, RuleCountFailure, "M1"},
		PlaceAccessLogWhen:          {PlaceAccessLogWhen, ResultBool, base | VarResponse | VarUpstream | VarDuration, RuleWriteEntry, "M1"},
		PlaceSemanticCacheKey:       {PlaceSemanticCacheKey, ResultString, base | VarAI, RuleCacheBypass, "M3"},
		PlaceCandidateWhen:          {PlaceCandidateWhen, ResultBool, base | VarAI, RuleSkipCandidate, "M3"},
		PlaceMessagingKey:           {PlaceMessagingKey, ResultString, base, RuleBadGateway, "M4"},
	}
	places := Places()
	if len(places) != 21 || len(want) != 21 {
		t.Fatalf("%d places, want the 21 of 03 B req 9", len(places))
	}
	if !slices.IsSortedFunc(places, func(a, b Place) int { return strings.Compare(string(a.ID), string(b.ID)) }) {
		t.Fatal("Places() is not sorted by ID")
	}
	seen := map[PlaceID]bool{}
	for _, p := range places {
		if seen[p.ID] {
			t.Fatalf("place %s listed twice", p.ID)
		}
		seen[p.ID] = true
		w, ok := want[p.ID]
		if !ok {
			t.Errorf("unexpected place %s", p.ID)
			continue
		}
		if p != w {
			t.Errorf("place %s = %+v (vars %v), want %+v (vars %v)", p.ID, p, p.Vars.Names(), w, w.Vars.Names())
		}
		if p.Vars&VarSelf != 0 {
			t.Errorf("place %s declares self, reserved for M2", p.ID)
		}
		got, ok := LookupPlace(p.ID)
		if !ok || got != p {
			t.Errorf("LookupPlace(%s) = %+v, %v", p.ID, got, ok)
		}
	}
	if _, ok := LookupPlace("Route.spec.timeout"); ok {
		t.Fatal("LookupPlace of a non-CEL field succeeded")
	}
	// Places returns a fresh table (no package state).
	places[0].Runtime = "changed"
	if Places()[0].Runtime == "changed" {
		t.Fatal("Places returned shared state")
	}
}

func TestVarNames(t *testing.T) {
	if got := VarsBase.Names(); !slices.Equal(got, []string{"request", "source", "route", "consumer", "auth", "now"}) {
		t.Fatalf("VarsBase.Names() = %v", got)
	}
	all := Var(0)
	for v := VarRequest; v <= VarSelf; v <<= 1 {
		all |= v
	}
	want := []string{"request", "source", "route", "consumer", "auth", "now", "response", "error", "upstream", "attempt", "steps", "duration", "ai", "self"}
	if got := all.Names(); !slices.Equal(got, want) {
		t.Fatalf("Names() of every variable = %v", got)
	}
	if Var(0).Names() != nil {
		t.Fatal("no variables should give no names")
	}
}

func TestRequestQuery(t *testing.T) {
	r := &Request{}
	if len(r.Query()) != 0 {
		t.Fatal("empty query has parameters")
	}
	r.SetRawQuery("a=1&b=x%20y&a=2&c=&a=3&d=%2C")
	if r.RawQuery() != "a=1&b=x%20y&a=2&c=&a=3&d=%2C" {
		t.Fatalf("RawQuery = %q", r.RawQuery())
	}
	want := map[string]string{"a": "1,2,3", "b": "x y", "c": "", "d": ","}
	if got := r.Query(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Query = %v, want %v (repeated values joined by \",\", decoded)", got, want)
	}
	// Parsed once: the same map is returned until SetRawQuery.
	first := r.Query()
	first["probe"] = "x"
	if _, ok := r.Query()["probe"]; !ok {
		t.Fatal("Query reparsed instead of returning its cache")
	}
	r.SetRawQuery("z=9")
	if got := r.Query(); !reflect.DeepEqual(got, map[string]string{"z": "9"}) {
		t.Fatalf("Query after SetRawQuery = %v", got)
	}
	// A malformed pair is skipped, the rest kept.
	r.SetRawQuery("ok=1&bad=%zz&also=2")
	if got := r.Query(); got["ok"] != "1" || got["also"] != "2" {
		t.Fatalf("Query with a malformed pair = %v", got)
	}
}

func TestRequestQueryConcurrent(t *testing.T) {
	r := &Request{}
	r.SetRawQuery("k=1&k=2&j=3")
	var wg sync.WaitGroup
	results := make([]map[string]string, 16)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = r.Query()
		}()
	}
	wg.Wait()
	for i, m := range results {
		if m["k"] != "1,2" || m["j"] != "3" {
			t.Fatalf("reader %d saw %v", i, m)
		}
		if reflect.ValueOf(m).UnsafePointer() != reflect.ValueOf(results[0]).UnsafePointer() {
			t.Fatalf("reader %d got a different map: the query was parsed twice", i)
		}
	}
}

func TestRequestReset(t *testing.T) {
	h := http.Header{"X-A": {"1"}}
	params := make([]Param, 0, 4)
	r := &Request{Method: "GET", Scheme: "https", Host: "a.example", Path: "/x", Header: h, PathParams: append(params, Param{"id", "7"})}
	r.SetRawQuery("a=1")
	_ = r.Query()
	r.Reset()
	if r.Method != "" || r.Scheme != "" || r.Host != "" || r.Path != "" || r.Body != nil || r.RawQuery() != "" {
		t.Fatalf("Reset left fields: %+v", r)
	}
	if len(r.Query()) != 0 {
		t.Fatal("Reset kept the parsed query")
	}
	if r.Header == nil || len(r.Header) != 0 || reflect.ValueOf(r.Header).UnsafePointer() != reflect.ValueOf(h).UnsafePointer() {
		t.Fatal("Reset did not clear and keep the header map")
	}
	if len(r.PathParams) != 0 || cap(r.PathParams) != 4 {
		t.Fatalf("Reset did not keep the params capacity: len %d cap %d", len(r.PathParams), cap(r.PathParams))
	}
}

func TestVarsReset(t *testing.T) {
	v := &Vars{
		Request: &Request{}, Source: &Source{IP: netip.MustParseAddr("192.0.2.1")}, Route: &Route{Name: "r"},
		Consumer: &Consumer{Name: "c"}, Auth: &Auth{Method: "jwt"}, Response: &Response{Status: 200},
		Error: &AttemptError{Kind: "connect"}, Upstream: &Upstream{Name: "u"}, Attempt: 2, Steps: &Steps{},
		AI: &AI{Model: "m"}, Now: time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), Duration: time.Second,
	}
	rv := reflect.ValueOf(v).Elem()
	for i := range rv.NumField() {
		if rv.Field(i).IsZero() {
			t.Fatalf("test fixture leaves %s unset; set every field", rv.Type().Field(i).Name)
		}
	}
	v.Reset()
	for i := range rv.NumField() {
		if !rv.Field(i).IsZero() {
			t.Errorf("Reset left %s set", rv.Type().Field(i).Name)
		}
	}
}

func TestJoinedHeader(t *testing.T) {
	h := http.Header{}
	h.Add("X-Tenant", "acme")
	h.Add("Accept", "text/html")
	h.Add("Accept", "application/json")
	h["x-raw-lower"] = []string{"a", "b"} // set directly, not canonical
	h["X-Empty"] = []string{}
	tests := []struct {
		name, key, want string
		ok              bool
	}{
		{"single", "x-tenant", "acme", true},
		{"canonical key", "X-Tenant", "acme", true},
		{"repeated lines joined", "accept", "text/html, application/json", true},
		{"non-canonical map key", "X-Raw-Lower", "a, b", true},
		{"non-canonical lookup", "x-RAW-lower", "a, b", true},
		{"present without values", "x-empty", "", true},
		{"absent", "x-missing", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := JoinedHeader(h, tt.key)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("JoinedHeader(%q) = %q, %v; want %q, %v", tt.key, got, ok, tt.want, tt.ok)
			}
		})
	}
	if _, ok := JoinedHeader(nil, "a"); ok {
		t.Fatal("JoinedHeader(nil) succeeded")
	}
}

func TestSteps(t *testing.T) {
	var s Steps
	if _, ok := s.Get("stock"); ok || len(s.Names()) != 0 {
		t.Fatal("empty Steps has a step")
	}
	s.Add("stock", Step{Status: 200})
	s.Add("price", Step{Status: 404, Header: http.Header{"A": {"1"}}})
	if st, ok := s.Get("price"); !ok || st.Status != 404 {
		t.Fatalf("Get(price) = %+v, %v", st, ok)
	}
	if !slices.Equal(s.Names(), []string{"stock", "price"}) {
		t.Fatalf("Names = %v, want list order", s.Names())
	}
	s.Reset()
	if len(s.Names()) != 0 {
		t.Fatal("Reset kept steps")
	}
	if _, ok := s.Get("stock"); ok {
		t.Fatal("Get after Reset succeeded")
	}
}

func TestErrorKinds(t *testing.T) {
	want := map[ErrorKind]string{
		KindCostLimit: "cost_limit", KindDeadline: "deadline", KindNull: "null", KindNoSuchKey: "no_such_key",
		KindNoSuchOverload: "no_such_overload", KindConversion: "conversion", KindArithmetic: "arithmetic",
		KindResultType: "result_type", KindBody: "body", KindOther: "other", 0: "other", 99: "other",
	}
	for k, s := range want {
		if k.String() != s {
			t.Errorf("ErrorKind(%d).String() = %q, want %q", k, k.String(), s)
		}
	}
}

func TestEvalError(t *testing.T) {
	// The message never carries request data; Detail does (03 req 43-46).
	e := NewEvalError(PlaceRateLimitKey, KindNoSuchKey, `no such key: "x-secret-header"`)
	if e.Error() != "cel: ratelimit config.key: no_such_key" {
		t.Fatalf("Error = %q", e.Error())
	}
	if strings.Contains(e.Error(), "x-secret-header") {
		t.Fatal("Error leaked the detail")
	}
	if e.Detail() != `no such key: "x-secret-header"` || e.Place != PlaceRateLimitKey || e.Kind != KindNoSuchKey {
		t.Fatalf("EvalError = %+v, detail %q", e, e.Detail())
	}
	var target *EvalError
	if !errors.As(error(e), &target) || target != e {
		t.Fatal("errors.As failed")
	}
	if errors.Is(ErrTooLarge, e) {
		t.Fatal("ErrTooLarge matches an EvalError")
	}
}

func TestDefaults(t *testing.T) {
	// R-21: the runtime defaults of retryOn and failureWhen read only their
	// place's variables.
	for src, place := range map[string]PlaceID{DefaultRetryOn: PlaceRetryOn, DefaultFailureWhen: PlaceFailureWhen} {
		p, _ := LookupPlace(place)
		for _, name := range []string{"request", "source", "route", "consumer", "auth", "now", "response", "error", "upstream", "attempt", "steps", "duration", "ai"} {
			used := strings.Contains(src, name+".") || strings.Contains(src, name+" ")
			declared := slices.Contains(p.Vars.Names(), name)
			if used && !declared {
				t.Errorf("%s default reads %s, not declared at the place", place, name)
			}
		}
	}
}
