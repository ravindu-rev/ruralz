// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/composition"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// stepLeg is a composition leg runner answering for one Upstream.
type stepLeg struct {
	name string
	fn   func(o *snapshot.Outbound) (*snapshot.UpstreamResponse, error)
	// reached marks Outbound.Reached as a leg that connected does.
	reached bool
	// hinted and via record the step's Early Hints relay and Via entry.
	hinted atomic.Bool
	via    atomic.Value
}

// Leg implements composition.Leg.
func (l *stepLeg) Leg(_ context.Context, o *snapshot.Outbound, _ upstream.Target) (*snapshot.UpstreamResponse, error) {
	if l.reached && o.Reached != nil {
		o.Reached.Store(true)
	}
	l.hinted.Store(o.Hints != nil)
	l.via.Store(o.Via)
	resp, err := l.fn(o)
	if resp != nil {
		resp.Upstream, resp.Endpoint, resp.Attempts = l.name, "10.0.0.9:80", 1
	}
	return resp, err
}

// textResp is an Upstream response of status with a text body.
func textResp(status int, s string) *snapshot.UpstreamResponse {
	return &snapshot.UpstreamResponse{Status: status, Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader(s))}
}

// compose compiles c with a real composition Engine over legs, sharing
// the kit's clock, buffer budget and in-flight units.
func compose(t *testing.T, k *kit, c *v1alpha1.Composition, legs ...*stepLeg) *composition.Composition {
	t.Helper()
	byName := map[string]*stepLeg{}
	for _, l := range legs {
		byName[l.name] = l
	}
	e := composition.New(composition.Deps{Clock: k.clk, Budget: k.budget, Units: k.units, Decoder: celtest.Compiler(t)})
	comp, err := e.Compile(composition.Input{
		Route: "c", Composition: c,
		Upstream: func(name string) (composition.Leg, bool) {
			l, ok := byName[name]
			return l, ok
		},
		Builder: celtest.Compiler(t).NewBuilder(nil, expr.InputCaps{}),
		Limits:  handlertest.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("compile the composition: %v", err)
	}
	return comp
}

// optionalStep returns an optional step to upstream at path.
func optionalStep(name, path string) v1alpha1.CompositionStep {
	yes := true
	return v1alpha1.CompositionStep{Name: name, Upstream: name, Path: path, Optional: &yes}
}

// TestCompositionRoute checks a composition Route through the handler: the
// final step's response passes through with the Ruralz-Partial field the
// engine set when an optional step was left out, the access record names
// the last step that ran, and Final.UpstreamReached holds for the steps'
// shared Reached.
func TestCompositionRoute(t *testing.T) {
	k := newKit(t)
	a := &stepLeg{name: "a", fn: func(*snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		return nil, &upstream.LegError{Upstream: "a", Err: errUpstream("RZ-UP-001")}
	}}
	b := &stepLeg{name: "b", reached: true, fn: func(*snapshot.Outbound) (*snapshot.UpstreamResponse, error) { return textResp(http.StatusOK, "b"), nil }}
	comp := compose(t, k, &v1alpha1.Composition{Mode: v1alpha1.CompositionModeSequential, Steps: []v1alpha1.CompositionStep{
		optionalStep("a", "/a"), {Name: "b", Upstream: "b", Path: "/b"},
	}}, a, b)
	var reached atomic.Bool
	logger := &finisher{f: func(x filter.Exchange) { reached.Store(x.Final().UpstreamReached) }}
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("c", comp,
		handlertest.Policy("quota", phase.ClassAdmission, v1alpha1.PolicyTypeQuota, logger, phase.OnRequestHeaders, phase.OnLog))}})
	res := k.get("/orders")
	if res.Code != http.StatusOK || res.Body.String() != "b" || res.Header().Get("Ruralz-Partial") != "true" {
		t.Fatalf("response = %d %q %v, want b's 200 marked partial", res.Code, res.Body.String(), res.Header())
	}
	if rec := k.record(); rec.Upstream != "b" || rec.Endpoint != "10.0.0.9:80" || rec.Attempts != 1 {
		t.Errorf("record = %s %s %d, want the last step b", rec.Upstream, rec.Endpoint, rec.Attempts)
	}
	if !reached.Load() {
		t.Error("Final.UpstreamReached = false, want true after step b reached its Upstream")
	}
	for _, l := range []*stepLeg{a, b} {
		if l.hinted.Load() || l.via.Load() != "1.1 ruralz" {
			t.Errorf("step %s: Early Hints relay %t, Via %v; want no relay and the client hop's entry", l.name, l.hinted.Load(), l.via.Load())
		}
	}
}

// TestCompositionFailure checks a failed composition: the problem document
// of the deciding step's code at its status, the access record naming the
// last step whose leg ran.
func TestCompositionFailure(t *testing.T) {
	k := newKit(t)
	a := &stepLeg{name: "a", fn: func(*snapshot.Outbound) (*snapshot.UpstreamResponse, error) { return textResp(http.StatusOK, "a"), nil }}
	b := &stepLeg{name: "b", fn: func(*snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		return nil, &upstream.LegError{Upstream: "b", Endpoint: "10.0.0.2:80", Attempts: 2, Err: errcode.Wrap("RZ-UP-003", errors.New("timeout"))}
	}}
	comp := compose(t, k, &v1alpha1.Composition{Mode: v1alpha1.CompositionModeSequential, Steps: []v1alpha1.CompositionStep{
		{Name: "a", Upstream: "a", Path: "/a"}, {Name: "b", Upstream: "b", Path: "/b"},
	}}, a, b)
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("c", comp)}})
	wantProblem(t, k.get("/orders"), http.StatusGatewayTimeout, "RZ-UP-003")
	if rec := k.record(); rec.Code != "RZ-UP-003" || rec.Upstream != "b" {
		t.Errorf("record = %s from %s, want RZ-UP-003 from b", rec.Code, rec.Upstream)
	}
}

// TestCompositionMergedPartial checks a merged aggregate response with an
// optional step left out: the merged body, marked partial.
func TestCompositionMergedPartial(t *testing.T) {
	k := newKit(t)
	jsonResp := func(s string) *snapshot.UpstreamResponse {
		return &snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(s))}
	}
	a := &stepLeg{name: "a", fn: func(*snapshot.Outbound) (*snapshot.UpstreamResponse, error) { return jsonResp(`{"a":1}`), nil }}
	b := &stepLeg{name: "b", fn: func(*snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		return nil, &upstream.LegError{Upstream: "b", Err: errUpstream("RZ-UP-001")}
	}}
	comp := compose(t, k, &v1alpha1.Composition{Mode: v1alpha1.CompositionModeAggregate, Steps: []v1alpha1.CompositionStep{
		{Name: "a", Upstream: "a", Path: "/a"}, optionalStep("b", "/b"),
	}}, a, b)
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("c", comp)}})
	res := k.get("/orders")
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"a":1`) || res.Header().Get("Ruralz-Partial") != "true" {
		t.Errorf("response = %d %q %v, want the merged body marked partial", res.Code, res.Body.String(), res.Header())
	}
}
