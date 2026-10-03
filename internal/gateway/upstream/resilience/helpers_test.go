// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/expr"
)

// epoch is the fake clocks' start.
var epoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// newClock returns a fake clock at epoch.
func newClock() *clocktest.Fake { return clocktest.New(epoch) }

// fixedSource always draws v: zero-jitter (v = 0) and full-jitter
// (v = MaxUint64) draws make backoff and open jitter exact.
type fixedSource struct{ v uint64 }

func (s fixedSource) Uint64() uint64 { return s.v }

// lockedSource is a seeded PCG safe for concurrent use.
type lockedSource struct {
	mu  sync.Mutex
	src *rand.PCG
}

func newLockedSource(seed uint64) *lockedSource {
	return &lockedSource{src: rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)}
}

func (s *lockedSource) Uint64() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.src.Uint64()
}

// edge is one recorded breaker transition.
type edge struct{ from, to State }

// recorder collects breaker transitions.
type recorder struct {
	mu    sync.Mutex
	edges []edge
}

func (r *recorder) record(from, to State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.edges = append(r.edges, edge{from, to})
}

func (r *recorder) all() []edge {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]edge(nil), r.edges...)
}

// fakeProgram is an expr.Program returning a fixed result.
type fakeProgram struct {
	place expr.PlaceID
	ok    bool
	err   error
	calls int
}

func (p *fakeProgram) Place() expr.PlaceID  { return p.place }
func (p *fakeProgram) Source() string       { return "fake" }
func (p *fakeProgram) Refs() expr.Refs      { return expr.Refs{} }
func (p *fakeProgram) Cost() expr.CostRange { return expr.CostRange{} }

func (p *fakeProgram) EvalBool(context.Context, *expr.Vars) (bool, error) {
	p.calls++
	return p.ok, p.err
}

func (p *fakeProgram) EvalString(context.Context, *expr.Vars) (string, error) {
	return "", p.err
}

func (p *fakeProgram) EvalValue(context.Context, *expr.Vars) (expr.Value, error) {
	return nil, p.err
}

var _ expr.Program = (*fakeProgram)(nil)
