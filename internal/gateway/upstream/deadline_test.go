// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// deadlineProgram is a CEL program that records the request deadline
// (expr.Vars.Deadline) of each evaluation and answers str or ok.
type deadlineProgram struct {
	place expr.PlaceID
	str   string
	ok    bool
	mu    sync.Mutex
	seen  []time.Time
}

func (p *deadlineProgram) Place() expr.PlaceID { return p.place }
func (*deadlineProgram) Source() string        { return "deadline" }
func (*deadlineProgram) Refs() expr.Refs       { return expr.Refs{} }
func (*deadlineProgram) Cost() expr.CostRange  { return expr.CostRange{} }

func (p *deadlineProgram) record(v *expr.Vars) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, v.Deadline)
}

func (p *deadlineProgram) EvalBool(_ context.Context, v *expr.Vars) (bool, error) {
	p.record(v)
	return p.ok, nil
}

func (p *deadlineProgram) EvalString(_ context.Context, v *expr.Vars) (string, error) {
	p.record(v)
	return p.str, nil
}

func (p *deadlineProgram) EvalInt(_ context.Context, v *expr.Vars) (int64, error) {
	p.record(v)
	return 0, nil
}

func (p *deadlineProgram) EvalValue(_ context.Context, v *expr.Vars) (expr.Value, error) {
	p.record(v)
	return nil, nil
}

// deadlines returns the deadlines recorded so far.
func (p *deadlineProgram) deadlines() []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.seen)
}

// TestLegCELRunsUnderRouteDeadline checks that the leg evaluates hashKey,
// failureWhen and retryOn under the Route deadline its Outbound carries,
// the request deadline at which a tracked comprehension stops
// (docs/architecture/02-configuration-model.md "Limits"), whatever
// deadline the client activation it copies holds, and never writes that
// shared activation.
func TestLegCELRunsUnderRouteDeadline(t *testing.T) {
	srv := newServer(t, status(http.StatusInternalServerError, ""))
	f := newFixture(t)
	u := newUpstream("orders", srv.addr)
	u.Spec.LoadBalancing = &v1alpha1.LoadBalancing{Algorithm: ptr(v1alpha1.LoadBalancingRingHash), HashKey: "deadline"}
	hashKey := &deadlineProgram{place: expr.PlaceHashKey, str: "k"}
	failureWhen := &deadlineProgram{place: expr.PlaceFailureWhen}
	retryOn := &deadlineProgram{place: expr.PlaceRetryOn}
	s := f.compile(nil, Config{Upstream: u, HashKey: hashKey, RetryOn: retryOn, FailureWhen: failureWhen, Metrics: newMetrics().m})
	x := exchange(http.MethodGet, "/")
	client := epoch().Add(time.Hour)
	x.VarsV.Deadline = client
	o := outbound(x, nil)
	r, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	want := []time.Time{o.Deadline}
	for _, p := range []*deadlineProgram{hashKey, failureWhen, retryOn} {
		if got := p.deadlines(); !slices.Equal(got, want) {
			t.Errorf("%s saw deadlines %v, want the Route deadline %v", p.place, got, want)
		}
	}
	if !x.VarsV.Deadline.Equal(client) {
		t.Errorf("the client activation's deadline = %v after the leg, want it untouched at %v", x.VarsV.Deadline, client)
	}
}
