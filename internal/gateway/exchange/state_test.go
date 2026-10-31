// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"log/slog"
	"net/http"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// TestPolicyStateSlots checks filter.Exchange.PolicyState: the slot of the
// Policy the last Enter selected, nil at its first call, kept across its
// Phases, one per Policy; a leg's slots are its own; a call outside any
// Policy gets a spare slot, never nil.
func TestPolicyStateSlots(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	xe := x.Exchange()
	if xe.PolicyState() == nil {
		t.Fatal("PolicyState() = nil before any Enter")
	}
	a := testPolicy("a", 0, phase.ClassAuth, phase.ScopeRoute, 0)
	b := testPolicy("b", 7, phase.ClassAdmission, phase.ScopeRoute, 1)
	x.Enter(a, phase.OnRequestHeaders, nil)
	if got := *xe.PolicyState(); got != nil {
		t.Fatalf("a's slot holds %v at its first call", got)
	}
	*xe.PolicyState() = "a-state"
	x.Enter(b, phase.OnRequestHeaders, nil)
	if got := *xe.PolicyState(); got != nil {
		t.Fatalf("b's slot holds %v at its first call", got)
	}
	*xe.PolicyState() = "b-state"
	x.Enter(a, phase.OnResponse, nil)
	if got := *xe.PolicyState(); got != "a-state" {
		t.Errorf("a's slot in onResponse = %v, want a-state", got)
	}
	x.Enter(b, phase.OnLog, nil)
	if got := *xe.PolicyState(); got != "b-state" {
		t.Errorf("b's slot in onLog = %v, want b-state", got)
	}

	l := x.Leg("orders", "")
	defer l.Release()
	l.Enter(b, phase.OnUpstreamRequest, nil)
	if got := *l.Exchange().PolicyState(); got != nil {
		t.Errorf("the leg's slot for index 7 holds %v, want its own empty slot", got)
	}
	*l.Exchange().PolicyState() = "leg"
	if got := *xe.PolicyState(); got != "b-state" {
		t.Errorf("a leg write reached the client slot: %v", got)
	}
	neg := testPolicy("neg", -1, phase.ClassCustom, phase.ScopeRoute, 0)
	x.Enter(neg, phase.OnRequestHeaders, nil)
	if xe.PolicyState() == nil {
		t.Error("PolicyState() = nil for a Policy without a valid index")
	}
}

// TestWhenDecisions checks RequestState.When and SetWhen: a Policy is
// undecided until SetWhen, then runs or is skipped; decisions are kept per
// Policy.Index, and a leg records its own (one decision per leg).
func TestWhenDecisions(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	a := testPolicy("a", 0, phase.ClassCORS, phase.ScopeRoute, 0)
	b := testPolicy("b", 40, phase.ClassTransform, phase.ScopeRoute, 1)
	for _, tc := range []struct {
		name          string
		p             *snapshot.Policy
		decided, skip bool
	}{
		{"a before SetWhen", a, false, false},
		{"b far index before SetWhen", b, false, false},
	} {
		if d, s := x.When(tc.p); d != tc.decided || s != tc.skip {
			t.Errorf("%s: When() = %v, %v", tc.name, d, s)
		}
	}
	x.SetWhen(a, false)
	x.SetWhen(b, true)
	if d, s := x.When(a); !d || s {
		t.Errorf("When(a) = %v, %v, want decided and running", d, s)
	}
	if d, s := x.When(b); !d || !s {
		t.Errorf("When(b) = %v, %v, want decided and skipped", d, s)
	}
	l := x.Leg("u", "s1")
	defer l.Release()
	if d, _ := l.When(b); d {
		t.Error("a leg sees the client's decision")
	}
	l.SetWhen(a, true)
	if _, s := x.When(a); s {
		t.Error("a leg decision reached the client")
	}
}

// TestAnnotateWritesCurrentSpan checks that Annotate sets an attribute on
// the span the last Enter selected and does nothing for an unsampled call
// (nil span).
func TestAnnotateWritesCurrentSpan(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	p := testPolicy("p", 0, phase.ClassAuth, phase.ScopeRoute, 0)
	x.Enter(p, phase.OnRequestHeaders, nil)
	x.Exchange().Annotate("k", slog.StringValue("unsampled"))
	s := &recSpan{}
	x.Enter(p, phase.OnRequestHeaders, s)
	x.Exchange().Annotate("k", slog.StringValue("v"))
	if got := s.attr("k"); got != "v" {
		t.Errorf("span attribute = %q, want v", got)
	}
	l := x.Leg("u", "")
	defer l.Release()
	ls := &recSpan{}
	l.Enter(p, phase.OnUpstreamRequest, ls)
	l.Exchange().Annotate("leg", slog.IntValue(1))
	if ls.attr("leg") != "1" || s.attr("leg") != "" {
		t.Error("a leg annotation did not reach the leg's own span")
	}
}

// TestMessageFollowsPhase checks filter.Message selection: the client
// request in onRequestHeaders, onRequestBody and onRoute, the client
// response in onResponse and onLog; on a leg, the outgoing request in
// onUpstreamRequest and the attempt's response in the response Phases.
func TestMessageFollowsPhase(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", http.Header{"X-In": {"req"}}))
	defer f.pool.Put(x)
	x.SetResponse(&snapshot.UpstreamResponse{Status: http.StatusCreated, Header: http.Header{"X-Out": {"resp"}}})
	p := testPolicy("p", 0, phase.ClassTransform, phase.ScopeRoute, 0)
	for _, tc := range []struct {
		ph     phase.Phase
		status int
		field  string
	}{
		{phase.OnRequestHeaders, 0, "X-In"},
		{phase.OnRequestBody, 0, "X-In"},
		{phase.OnRoute, 0, "X-In"},
		{phase.OnResponse, http.StatusCreated, "X-Out"},
		{phase.OnLog, http.StatusCreated, "X-Out"},
	} {
		x.Enter(p, tc.ph, nil)
		m := x.Exchange().Message()
		if m.Status() != tc.status || m.Header().Get(tc.field) == "" {
			t.Errorf("%s: Message() status %d, header %v", tc.ph, m.Status(), m.Header())
		}
	}

	l := x.Leg("u", "")
	defer l.Release()
	resp, _ := upstreamResponse(http.StatusAccepted, "{}")
	defer func() { _ = resp.Body.Close() }()
	out := &http.Request{Header: http.Header{"X-Leg": {"out"}}}
	l.SetLeg(&filter.Leg{Upstream: "u", Attempt: 1, Request: out, Response: resp})
	for _, tc := range []struct {
		ph     phase.Phase
		status int
		field  string
	}{
		{phase.OnUpstreamRequest, 0, "X-Leg"},
		{phase.OnUpstreamResponseHeaders, http.StatusAccepted, "Content-Type"},
		{phase.OnUpstreamResponseBody, http.StatusAccepted, "Content-Type"},
		{phase.OnLog, http.StatusAccepted, "Content-Type"},
	} {
		l.Enter(p, tc.ph, nil)
		m := l.Exchange().Message()
		if m.Status() != tc.status || m.Header().Get(tc.field) == "" || m.Generated() {
			t.Errorf("leg %s: Message() status %d, header %v", tc.ph, m.Status(), m.Header())
		}
	}
	if l.Exchange().Leg().Attempt != 1 || x.Exchange().Leg() != nil {
		t.Error("Leg() does not follow SetLeg, or the client view has a leg")
	}
}
