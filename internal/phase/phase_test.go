// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package phase

import (
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for architecture section 2.2 (WP-01): round trip of every
// v1alpha1.Phase and v1alpha1.FilterClass, and the Phase predicates against
// spec 04 requirements 39 (Phases and legs), 40 (response Phases run in
// reverse) and 42 (short-circuit).

func TestPhaseRoundTrip(t *testing.T) {
	// 04 req 39: the fixed Phase order, plus onChunk.
	want := []v1alpha1.Phase{
		v1alpha1.PhaseOnRequestHeaders,
		v1alpha1.PhaseOnRequestBody,
		v1alpha1.PhaseOnRoute,
		v1alpha1.PhaseOnUpstreamRequest,
		v1alpha1.PhaseOnUpstreamResponseHeaders,
		v1alpha1.PhaseOnUpstreamResponseBody,
		v1alpha1.PhaseOnResponse,
		v1alpha1.PhaseOnLog,
		v1alpha1.PhaseOnChunk,
	}
	if int(Count) != len(want) {
		t.Fatalf("Count = %d, want %d", Count, len(want))
	}
	for i, v := range want {
		p := Phase(i)
		if p.V1alpha1() != v {
			t.Errorf("Phase(%d).V1alpha1() = %q, want %q", i, p.V1alpha1(), v)
		}
		if p.String() != string(v) {
			t.Errorf("Phase(%d).String() = %q, want %q", i, p.String(), v)
		}
		got, ok := Parse(v)
		if !ok || got != p {
			t.Errorf("Parse(%q) = %v, %v; want %v, true", v, got, ok, p)
		}
	}
	for _, bad := range []v1alpha1.Phase{"", "OnRequestHeaders", "onrequestheaders", "onStream"} {
		if _, ok := Parse(bad); ok {
			t.Errorf("Parse(%q) succeeded", bad)
		}
	}
	if Count.V1alpha1() != "" || Count.String() != "" {
		t.Errorf("Count spells %q, want empty", Count.String())
	}
}

func TestClassRoundTrip(t *testing.T) {
	// 04 req 40 and foundation pack 8.12: class order within a Phase.
	want := []v1alpha1.FilterClass{
		v1alpha1.FilterClassCORS,
		v1alpha1.FilterClassAuth,
		v1alpha1.FilterClassAuthz,
		v1alpha1.FilterClassAdmission,
		v1alpha1.FilterClassValidation,
		v1alpha1.FilterClassCache,
		v1alpha1.FilterClassUpstreamAuth,
		v1alpha1.FilterClassTransform,
		v1alpha1.FilterClassCustom,
	}
	if int(NumClasses) != len(want) {
		t.Fatalf("NumClasses = %d, want %d", NumClasses, len(want))
	}
	for i, v := range want {
		c := Class(i)
		if c.V1alpha1() != v || c.String() != string(v) {
			t.Errorf("Class(%d) = %q, want %q", i, c.String(), v)
		}
		got, ok := ParseClass(v)
		if !ok || got != c {
			t.Errorf("ParseClass(%q) = %v, %v; want %v, true", v, got, ok, c)
		}
	}
	for _, bad := range []v1alpha1.FilterClass{"", "CORS", "upstream_auth", "plugin"} {
		if _, ok := ParseClass(bad); ok {
			t.Errorf("ParseClass(%q) succeeded", bad)
		}
	}
	if NumClasses.String() != "" {
		t.Errorf("NumClasses spells %q, want empty", NumClasses.String())
	}
}

func TestPhasePredicates(t *testing.T) {
	// Truth table from 04 req 39 (client leg: onRequestHeaders,
	// onRequestBody, onRoute, onResponse, onLog; per upstream leg:
	// onUpstreamRequest, onUpstreamResponseHeaders, onUpstreamResponseBody;
	// onChunk on both), req 40 (response Phases reversed, onChunk ordered as
	// a response Phase in M1) and req 42 (short-circuit before onResponse).
	tests := []struct {
		p                                        Phase
		response, shortCircuit, client, upstream bool
	}{
		{OnRequestHeaders, false, true, true, false},
		{OnRequestBody, false, true, true, false},
		{OnRoute, false, true, true, false},
		{OnUpstreamRequest, false, true, false, true},
		{OnUpstreamResponseHeaders, true, false, false, true},
		{OnUpstreamResponseBody, true, false, false, true},
		{OnResponse, true, false, true, false},
		{OnLog, false, false, true, false},
		{OnChunk, true, false, true, true},
	}
	if len(tests) != int(Count) {
		t.Fatalf("table covers %d Phases, want %d", len(tests), Count)
	}
	for _, tt := range tests {
		t.Run(tt.p.String(), func(t *testing.T) {
			if got := tt.p.IsResponse(); got != tt.response {
				t.Errorf("IsResponse = %v, want %v", got, tt.response)
			}
			if got := tt.p.CanShortCircuit(); got != tt.shortCircuit {
				t.Errorf("CanShortCircuit = %v, want %v", got, tt.shortCircuit)
			}
			if got := tt.p.ClientLeg(); got != tt.client {
				t.Errorf("ClientLeg = %v, want %v", got, tt.client)
			}
			if got := tt.p.UpstreamLeg(); got != tt.upstream {
				t.Errorf("UpstreamLeg = %v, want %v", got, tt.upstream)
			}
		})
	}
}

func TestSet(t *testing.T) {
	var empty Set
	if !empty.Empty() {
		t.Fatal("zero Set is not empty")
	}
	if _, ok := empty.First(); ok {
		t.Fatal("First of an empty Set succeeded")
	}
	if len(empty.Phases()) != 0 {
		t.Fatalf("Phases of an empty Set = %v", empty.Phases())
	}

	s := Of(OnResponse, OnRequestBody, OnResponse)
	if s.Empty() {
		t.Fatal("Of(...) is empty")
	}
	if !s.Has(OnRequestBody) || !s.Has(OnResponse) || s.Has(OnRequestHeaders) {
		t.Fatalf("Has is wrong for %b", s)
	}
	if p, ok := s.First(); !ok || p != OnRequestBody {
		t.Fatalf("First = %v, %v; want onRequestBody", p, ok)
	}
	s2 := s.Add(OnRequestHeaders)
	if s.Has(OnRequestHeaders) {
		t.Fatal("Add modified its receiver")
	}
	if p, _ := s2.First(); p != OnRequestHeaders {
		t.Fatalf("First after Add = %v", p)
	}
	if got, want := s2.Phases(), []Phase{OnRequestHeaders, OnRequestBody, OnResponse}; !slices.Equal(got, want) {
		t.Fatalf("Phases = %v, want %v in execution order", got, want)
	}

	all := Of()
	for p := OnRequestHeaders; p < Count; p++ {
		all = all.Add(p)
	}
	if len(all.Phases()) != int(Count) {
		t.Fatalf("a Set of every Phase lists %d", len(all.Phases()))
	}
	if p, _ := Of(OnChunk).First(); p != OnChunk {
		t.Fatalf("First of {onChunk} = %v", p)
	}
}

func TestScope(t *testing.T) {
	tests := []struct {
		s    Scope
		want string
	}{
		{ScopeNone, ""},
		{ScopeGateway, "Gateway"},
		{ScopeRoute, "Route"},
		{ScopeUpstream, "Upstream"},
		{Scope(9), ""},
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("Scope(%d).String() = %q, want %q", tt.s, got, tt.want)
		}
	}
	// Chain order (04 req 40): Gateway, Route, Upstream.
	if ScopeGateway >= ScopeRoute || ScopeRoute >= ScopeUpstream {
		t.Fatal("scopes are not in chain order")
	}

	set := Scopes(ScopeGateway, ScopeUpstream)
	if !set.Has(ScopeGateway) || !set.Has(ScopeUpstream) || set.Has(ScopeRoute) || set.Has(ScopeNone) {
		t.Fatalf("Scopes(Gateway, Upstream) = %b", set)
	}
	if Scopes().Has(ScopeGateway) {
		t.Fatal("empty ScopeSet has Gateway")
	}
}
