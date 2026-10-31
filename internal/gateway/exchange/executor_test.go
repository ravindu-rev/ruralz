// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/executor"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The real executor (internal/gateway/executor) drives the exchange
// through a whole request, so the RequestState contract is checked as the
// data plane uses it.

// step is a scripted Filter answer for one Phase.
type step func(ctx context.Context, x filter.Exchange) filter.Result

// scripted is a Filter that logs its calls and answers from its script.
type scripted struct {
	name   string
	log    *callLog
	script map[phase.Phase]step
}

func (s *scripted) Handle(ctx context.Context, ph phase.Phase, x filter.Exchange) filter.Result {
	s.log.add(s.name + "." + ph.String())
	if fn := s.script[ph]; fn != nil {
		return fn(ctx, x)
	}
	return filter.Next()
}

// finishing is a scripted Filter with Finish.
type finishing struct {
	scripted
	finish func(x filter.Exchange)
}

func (f *finishing) Finish(_ context.Context, x filter.Exchange) { f.finish(x) }

// callLog is a goroutine-safe ordered log.
type callLog struct {
	mu  sync.Mutex
	evs []string
}

func (l *callLog) add(s string) {
	l.mu.Lock()
	l.evs = append(l.evs, s)
	l.mu.Unlock()
}

func (l *callLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.evs)
}

// compiled returns a compiled Policy over f.
func compiled(name string, class phase.Class, scope phase.Scope, f filter.Filter, phases ...phase.Phase) *snapshot.Policy {
	set := phase.Of(phases...)
	first, _ := set.First()
	return &snapshot.Policy{
		Name: name, Type: v1alpha1.PolicyTypeHeaders, Class: class, Scope: scope, FailureMode: v1alpha1.FailureModeClosed,
		FirstPhase: first, Phases: set, Filter: f,
	}
}

// chainOf compiles client Policies in request order and one leg chain, as
// internal/gateway/compile does: per-Phase lists, response Phases reversed.
func chainOf(client []*snapshot.Policy, upstream string, legPs []*snapshot.Policy) *snapshot.Chain {
	lists := func(ps []*snapshot.Policy) [phase.Count][]*snapshot.Policy {
		var out [phase.Count][]*snapshot.Policy
		for ph := range phase.Count {
			for _, p := range ps {
				if p.Phases.Has(ph) {
					out[ph] = append(out[ph], p)
				}
			}
			if ph.IsResponse() {
				slices.Reverse(out[ph])
			}
		}
		return out
	}
	c := &snapshot.Chain{Client: lists(client), Policies: client, Legs: map[string]*snapshot.LegChain{}}
	for i, p := range client {
		p.Index, p.Position = i, i
		if p.Class == phase.ClassAuth {
			c.AuthPolicies++
		}
	}
	for i, p := range legPs {
		p.Index, p.Position = i, i
	}
	c.Legs[upstream] = &snapshot.LegChain{Upstream: upstream, Phases: lists(legPs)}
	return c
}

// requestFixture is a Route chain of cors, auth, quota, a body-reading
// authz with a CEL when on request.body, validation and an upstream-leg
// headers Policy.
type requestFixture struct {
	log        *callLog
	ch         *snapshot.Chain
	rejected   []bool
	validation *scripted
}

func newRequestFixture(t *testing.T) *requestFixture {
	t.Helper()
	rf := &requestFixture{log: &callLog{}}
	when := celtest.Program(t, expr.Site{
		Place: expr.PlacePolicyWhen, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeAuthzCEL,
		FirstPhase: phase.OnRequestBody, GatesBody: true,
	}, `request.body.kind == "order" && consumer.name == "acme"`)
	cors := &scripted{name: "cors", log: rf.log, script: map[phase.Phase]step{
		phase.OnRequestHeaders: func(_ context.Context, x filter.Exchange) filter.Result {
			*x.PolicyState() = "preflight-checked"
			return filter.Next()
		},
		phase.OnResponse: func(_ context.Context, x filter.Exchange) filter.Result {
			if *x.PolicyState() != "preflight-checked" {
				t.Errorf("cors slot in onResponse = %v", *x.PolicyState())
			}
			x.Message().Header().Set("Access-Control-Allow-Origin", "*")
			return filter.Next()
		},
	}}
	auth := &scripted{name: "auth", log: rf.log, script: map[phase.Phase]step{
		phase.OnRequestHeaders: func(_ context.Context, x filter.Exchange) filter.Result {
			if err := x.SetIdentity(&filter.Identity{Method: filter.MethodAPIKey, Consumer: &expr.Consumer{Name: "acme"}}); err != nil {
				return filter.Undecided("RZ-AUTH-002", err)
			}
			return filter.Next()
		},
	}}
	quota := &finishing{
		scripted: scripted{name: "quota", log: rf.log},
		finish:   func(x filter.Exchange) { rf.rejected = append(rf.rejected, x.Final().RejectedAfter) },
	}
	authz := &scripted{name: "authz", log: rf.log}
	rf.validation = &scripted{name: "validation", log: rf.log}
	headers := &scripted{name: "headers-up", log: rf.log, script: map[phase.Phase]step{
		phase.OnUpstreamRequest: func(_ context.Context, x filter.Exchange) filter.Result {
			x.Message().Header().Set("X-Consumer", x.Vars().Consumer.Name)
			return filter.Next()
		},
	}}
	authzP := compiled("authz", phase.ClassAuthz, phase.ScopeRoute, authz, phase.OnRequestBody)
	authzP.When = when
	rf.ch = chainOf([]*snapshot.Policy{
		compiled("cors", phase.ClassCORS, phase.ScopeRoute, cors, phase.OnRequestHeaders, phase.OnResponse),
		compiled("auth", phase.ClassAuth, phase.ScopeRoute, auth, phase.OnRequestHeaders),
		authzP,
		compiled("quota", phase.ClassAdmission, phase.ScopeRoute, quota, phase.OnRequestHeaders, phase.OnLog),
		compiled("validation", phase.ClassValidation, phase.ScopeRoute, rf.validation, phase.OnRequestBody),
	}, "orders", []*snapshot.Policy{
		compiled("headers-up", phase.ClassTransform, phase.ScopeUpstream, headers, phase.OnUpstreamRequest),
	})
	return rf
}

// TestExecutorDrivesExchange runs a request through every Phase with the
// real executor: identity reaches later CEL, a when reading request.body
// sees the gated body, PolicyState slots persist across Phases, a leg
// writes its outgoing request, onResponse edits the response, and a
// request no later Policy rejected leaves Final.RejectedAfter false.
func TestExecutorDrivesExchange(t *testing.T) {
	rf := newRequestFixture(t)
	f := newFixture(t)
	rt := testRoute("orders", snapshot.BodyNeeds{RequestGate: true})
	rt.Chain = *rf.ch
	x := f.routed(serverRequest(http.MethodPost, "/v1/orders", `{"kind":"order"}`, jsonHeader()), rt)
	defer f.pool.Put(x)
	ctx := context.Background()
	run := executor.New(executor.Deps{}).Begin(x, &rt.Chain, nil)
	defer run.Release()
	if resp := run.Request(ctx, phase.OnRequestHeaders); resp != nil {
		t.Fatalf("onRequestHeaders responded %+v", resp)
	}
	if err := x.PrepareBody(ctx); err != nil {
		t.Fatal(err)
	}
	for _, ph := range []phase.Phase{phase.OnRequestBody, phase.OnRoute} {
		if resp := run.Request(ctx, ph); resp != nil {
			t.Fatalf("%s responded %+v", ph, resp)
		}
	}
	o := x.Outbound()
	o.Hooks = run
	lr := o.Hooks.BeginLeg(ctx, "orders", "")
	out := outgoing(t, x)
	if resp := lr.OnUpstreamRequest(ctx, &filter.Leg{Upstream: "orders", Attempt: 1, Request: out}); resp != nil {
		t.Fatalf("onUpstreamRequest responded %+v", resp)
	}
	lr.End(ctx)
	x.SetResponse(&snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{}, Upstream: "orders"})
	if repl := run.Response(ctx, executor.ResponseUpstream); repl != nil {
		t.Fatalf("onResponse replaced the response with %+v", repl)
	}
	x.SetFinal(filter.Final{Status: http.StatusOK, Committed: true}, 0)
	run.Log(ctx)

	want := []string{
		"cors.onRequestHeaders", "auth.onRequestHeaders", "quota.onRequestHeaders",
		"authz.onRequestBody", "validation.onRequestBody", "headers-up.onUpstreamRequest",
		"cors.onResponse", "quota.onLog",
	}
	if got := rf.log.list(); !slices.Equal(got, want) {
		t.Errorf("calls\n got %v\nwant %v", got, want)
	}
	if out.Header.Get("X-Consumer") != "acme" {
		t.Errorf("the leg's outgoing request = %v", out.Header)
	}
	if x.Reply().Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("onResponse edits did not reach the response")
	}
	if !slices.Equal(rf.rejected, []bool{false}) {
		t.Errorf("quota Finish saw RejectedAfter %v, want [false]", rf.rejected)
	}
	if _, _, _, ok := x.ShortCircuit(); ok || len(x.FailureModes()) != 0 {
		t.Error("a request without a short-circuit or failure recorded one")
	}
}

// TestExecutorShortCircuitFeedsRecords checks that a later Policy's denial
// recorded through the executor is the access record's short_circuit and
// makes Final.RejectedAfter true for the earlier quota (refund), and that
// a closed failure is listed in failure_modes.
func TestExecutorShortCircuitFeedsRecords(t *testing.T) {
	rf := newRequestFixture(t)
	rf.validation.script = map[phase.Phase]step{
		phase.OnRequestBody: func(context.Context, filter.Exchange) filter.Result {
			return filter.Deny(http.StatusBadRequest, "RZ-RT-009", nil)
		},
	}
	f := newFixture(t)
	rt := testRoute("orders", snapshot.BodyNeeds{RequestGate: true})
	rt.Chain = *rf.ch
	x := f.routed(serverRequest(http.MethodPost, "/v1/orders", `{"kind":"other"}`, jsonHeader()), rt)
	defer f.pool.Put(x)
	ctx := context.Background()
	run := executor.New(executor.Deps{}).Begin(x, &rt.Chain, nil)
	defer run.Release()
	run.Request(ctx, phase.OnRequestHeaders)
	if err := x.PrepareBody(ctx); err != nil {
		t.Fatal(err)
	}
	resp := run.Request(ctx, phase.OnRequestBody)
	if resp == nil || resp.Status != http.StatusBadRequest {
		t.Fatalf("onRequestBody = %+v, want the 400", resp)
	}
	x.SetGenerated(resp)
	run.Response(ctx, executor.ResponseGenerated)
	x.SetFinal(filter.Final{Status: resp.Status, Code: resp.Code, Committed: true}, 0)
	run.Log(ctx)
	if name, ph, status, ok := x.ShortCircuit(); !ok || name != "validation" || ph != phase.OnRequestBody || status != http.StatusBadRequest {
		t.Errorf("ShortCircuit() = %q %s %d %v", name, ph, status, ok)
	}
	if !slices.Equal(rf.rejected, []bool{true}) {
		t.Errorf("quota Finish saw RejectedAfter %v, want [true]", rf.rejected)
	}
	if slices.Contains(rf.log.list(), "authz.onRequestBody") {
		t.Error(`authz ran although its when (request.body.kind == "order") is false`)
	}
	x.Enter(rf.ch.Policies[0], phase.OnResponse, nil)
	x.RecordFailure(rf.ch.Policies[0], phase.OnResponse, v1alpha1.FailureModeClosed)
	if fm := x.FailureModes(); len(fm) != 1 || fm[0].Policy != "cors" || fm[0].Mode != "closed" {
		t.Errorf("FailureModes() = %+v", fm)
	}
}
