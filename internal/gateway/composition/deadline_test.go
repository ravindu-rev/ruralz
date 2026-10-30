// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

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

// deadlineSpy is a compiled program that records the request deadline
// (expr.Vars.Deadline) of each evaluation before it evaluates.
type deadlineSpy struct {
	expr.Program
	mu   sync.Mutex
	seen []time.Time
}

func (s *deadlineSpy) record(v *expr.Vars) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, v.Deadline)
}

func (s *deadlineSpy) EvalBool(ctx context.Context, v *expr.Vars) (bool, error) {
	s.record(v)
	return s.Program.EvalBool(ctx, v)
}

func (s *deadlineSpy) EvalString(ctx context.Context, v *expr.Vars) (string, error) {
	s.record(v)
	return s.Program.EvalString(ctx, v)
}

// deadlines returns the deadlines recorded so far.
func (s *deadlineSpy) deadlines() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

// spyBuilder is a Builder whose programs are deadline spies, kept by
// source.
type spyBuilder struct {
	expr.Builder
	spies map[string]*deadlineSpy
}

func (b *spyBuilder) Compile(site expr.Site, src string) (expr.Program, []expr.Issue) {
	prg, iss := b.Builder.Compile(site, src)
	if prg == nil {
		return nil, iss
	}
	s := &deadlineSpy{Program: prg}
	b.spies[src] = s
	return s, iss
}

// TestStepCELRunsUnderRouteDeadline checks that a composition evaluates
// its steps' when and pathExpression under the Route deadline its
// Outbound carries, the request deadline at which a tracked comprehension
// stops (docs/architecture/02-configuration-model.md "Limits"), whatever
// deadline the client activation it copies holds, and never writes that
// activation.
func TestStepCELRunsUnderRouteDeadline(t *testing.T) {
	const when, path = `request.method == "GET"`, `"/v1/" + request.method`
	f := newFixture(t, "orders")
	in := f.input(composition(v1alpha1.CompositionModeSequential,
		v1alpha1.CompositionStep{Name: "one", Upstream: "orders", When: when, PathExpression: path}))
	b := &spyBuilder{Builder: in.Builder, spies: map[string]*deadlineSpy{}}
	in.Builder = b
	comp, err := f.e.Compile(in)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	x := exchange(http.MethodGet, "/v1", "")
	client := epoch().Add(time.Hour)
	x.VarsV.Deadline = client
	o := f.outbound(x, "")
	resp, err := comp.Forward(t.Context(), o)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	read(t, resp)
	if calls := f.leg("orders").list(); len(calls) != 1 || calls[0].target.Path != "/v1/GET" {
		t.Fatalf("leg calls = %+v, want one to /v1/GET", calls)
	}
	want := []time.Time{o.Deadline}
	for _, src := range []string{when, path} {
		s := b.spies[src]
		if s == nil {
			t.Fatalf("%q was not compiled", src)
		}
		if got := s.deadlines(); !slices.Equal(got, want) {
			t.Errorf("%q saw deadlines %v, want the Route deadline %v", src, got, want)
		}
	}
	if !x.VarsV.Deadline.Equal(client) {
		t.Errorf("the client activation's deadline = %v after the composition, want it untouched at %v", x.VarsV.Deadline, client)
	}
}
