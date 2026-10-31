// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// deadlineSpy is a CEL program that records the request deadline
// (expr.Vars.Deadline) of each evaluation and answers ok.
type deadlineSpy struct {
	ok   bool
	mu   sync.Mutex
	seen []time.Time
}

func (*deadlineSpy) Place() expr.PlaceID  { return expr.PlacePolicyWhen }
func (*deadlineSpy) Source() string       { return "deadline" }
func (*deadlineSpy) Refs() expr.Refs      { return expr.Refs{} }
func (*deadlineSpy) Cost() expr.CostRange { return expr.CostRange{} }

func (s *deadlineSpy) record(v *expr.Vars) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, v.Deadline)
}

func (s *deadlineSpy) EvalBool(_ context.Context, v *expr.Vars) (bool, error) {
	s.record(v)
	return s.ok, nil
}

func (s *deadlineSpy) EvalString(_ context.Context, v *expr.Vars) (string, error) {
	s.record(v)
	return "", nil
}

func (s *deadlineSpy) EvalInt(_ context.Context, v *expr.Vars) (int64, error) {
	s.record(v)
	return 0, nil
}

func (s *deadlineSpy) EvalValue(_ context.Context, v *expr.Vars) (expr.Value, error) {
	s.record(v)
	return nil, nil
}

// deadlines returns the deadlines recorded so far.
func (s *deadlineSpy) deadlines() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

// TestCELGetsTheRouteDeadline checks the request deadline every CEL
// evaluation of a request gets, on the handler's clock, at which a tracked
// comprehension stops (docs/architecture/02-configuration-model.md
// "Limits"): each candidate's match.when its own Route deadline, the
// request start plus that Route's timeout; after the match, spec.when, a
// Filter's activation, the Forwarder's activation and accessLog.when the
// matched Route's, which is also the Forwarder's Outbound deadline; an
// unmatched request's accessLog.when none.
func TestCELGetsTheRouteDeadline(t *testing.T) {
	k := newKit(t)
	const shortTimeout, longTimeout = 30 * time.Millisecond, 2 * time.Second
	short, long := &deadlineSpy{}, &deadlineSpy{ok: true}
	policyWhen, accessWhen := &deadlineSpy{ok: true}, &deadlineSpy{ok: true}
	var filterSaw, forwardSaw, outboundSaw time.Time
	p := handlertest.Policy("hdr", phase.ClassTransform, "", handlertest.FilterFunc(func(_ context.Context, _ phase.Phase, x filter.Exchange) filter.Result {
		filterSaw = x.Vars().Deadline
		return filter.Next()
	}), phase.OnRequestHeaders)
	p.When = policyWhen
	fwd := handlertest.ForwardFunc(func(_ context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		forwardSaw, outboundSaw = o.X.Vars().Deadline, o.Deadline
		return &snapshot.UpstreamResponse{Status: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Upstream: "orders"}, nil
	})
	k.publish(handlertest.Spec{
		Routes: []handlertest.Route{
			{Name: "a-short", Match: &v1alpha1.RouteMatch{Path: &v1alpha1.PathMatch{Prefix: "/r"}, When: "short"}, When: short, Timeout: shortTimeout, Forward: fwd},
			{Name: "b-long", Match: &v1alpha1.RouteMatch{Path: &v1alpha1.PathMatch{Prefix: "/r"}, When: "long"}, When: long, Timeout: longTimeout, Forward: fwd, Policies: []*snapshot.Policy{p}},
		},
		AccessLogWhen: accessWhen,
	})
	start := k.clk.Now()

	if res := k.get("http://api.example/r"); res.Code != http.StatusOK {
		t.Fatalf("GET /r = %d %q, want 200 from b-long", res.Code, res.Body.String())
	}
	matched := start.Add(longTimeout)
	if got, want := short.deadlines(), []time.Time{start.Add(shortTimeout)}; !slices.Equal(got, want) {
		t.Errorf("a-short's match.when saw deadlines %v, want its own %v", got, want)
	}
	if got, want := long.deadlines(), []time.Time{matched}; !slices.Equal(got, want) {
		t.Errorf("b-long's match.when saw deadlines %v, want its own %v", got, want)
	}
	if got, want := policyWhen.deadlines(), []time.Time{matched}; !slices.Equal(got, want) {
		t.Errorf("spec.when saw deadlines %v, want the Route deadline %v", got, want)
	}
	if !filterSaw.Equal(matched) || !forwardSaw.Equal(matched) || !outboundSaw.Equal(matched) {
		t.Errorf("Filter activation %v, Forwarder activation %v, Outbound %v; want the Route deadline %v", filterSaw, forwardSaw, outboundSaw, matched)
	}

	if res := k.get("http://api.example/nowhere"); res.Code != http.StatusNotFound {
		t.Fatalf("GET /nowhere = %d, want 404", res.Code)
	}
	if got, want := accessWhen.deadlines(), []time.Time{matched, {}}; !slices.Equal(got, want) {
		t.Errorf("accessLog.when saw deadlines %v, want the Route deadline, then none for the unmatched request", got)
	}
}
