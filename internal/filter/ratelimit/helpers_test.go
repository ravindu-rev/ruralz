// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// testStart is the fake clock's start: half a second into a second, so a
// test sees the epoch-aligned window boundaries the fail-open refill uses.
func testStart() time.Time { return time.Date(2026, 10, 29, 9, 0, 0, 500_000_000, time.UTC) }

// stateTimeout is the Policies' stateStoreTimeout.
const stateTimeout = 50 * time.Millisecond

// counter records emit.Counter adds.
type counter struct {
	mu sync.Mutex
	n  uint64
}

func (c *counter) Add(_ emit.Stripe, n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n += n
}

func (c *counter) value() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// metrics are one Policy's recorded handles.
type metrics struct {
	decisions [emit.NumRateLimitResults]*counter
	evictions [emit.NumBucketSegments]*counter
}

// newMetrics returns PolicyMetrics recording into a metrics.
func newMetrics() (*emit.PolicyMetrics, *metrics) {
	m := &metrics{}
	pm := &emit.PolicyMetrics{}
	for i := range m.decisions {
		m.decisions[i] = &counter{}
		pm.RateLimit[i] = m.decisions[i]
	}
	for i := range m.evictions {
		m.evictions[i] = &counter{}
		pm.BucketEvictions[i] = m.evictions[i]
	}
	return pm, m
}

// decided returns the decision counts by result name.
func (m *metrics) decided() map[string]uint64 {
	names := [...]string{"allow", "deny_local", "deny_global", "fail_open", "fail_closed", "allow_local"}
	out := map[string]uint64{}
	for i, c := range m.decisions {
		if n := c.value(); n > 0 {
			out[names[i]] = n
		}
	}
	return out
}

// nodeCount is a NodeCount fake.
type nodeCount struct {
	mu      sync.Mutex
	n       int64
	control bool
}

func (c *nodeCount) Count() (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n, c.control
}

// smallTable returns a one-shard table of 40 entries, 20 of them
// local-only, on clk.
func smallTable(tb testing.TB, clk *clocktest.Fake, count NodeCount) *Table {
	tb.Helper()
	t, err := NewTable(clk, count, TableOptions{Entries: 40, LocalOnlyEntries: 20, Shards: 1, Seed: 7})
	if err != nil {
		tb.Fatal(err)
	}
	return t
}

// countingStore is a statestore.Store that counts Consume round trips and
// passes them to next, or fails every call with fail.
type countingStore struct {
	statestore.Store
	mu    sync.Mutex
	trips int
	fail  *statestore.Error
}

func (s *countingStore) Consume(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) int {
	s.mu.Lock()
	s.trips++
	fail := s.fail
	s.mu.Unlock()
	if fail != nil {
		for _, c := range calls {
			c.Done, c.Err = false, fail
		}
		return len(calls)
	}
	return s.Store.Consume(ctx, rb, calls)
}

func (s *countingStore) roundTrips() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trips
}

func (s *countingStore) failWith(err *statestore.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

// fixture is one Node: a fake clock, its key table and a memory State
// Store behind a counting store.
type fixture struct {
	clk   *clocktest.Fake
	table *Table
	store *countingStore
	cel   expr.Compiler
}

// newFixture returns a fixture with a small table in file mode, or with
// count when given.
func newFixture(tb testing.TB, count NodeCount) *fixture {
	tb.Helper()
	clk := clocktest.New(testStart())
	mem, err := memory.New(context.Background(), statestore.Config{Driver: statestore.DriverMemory},
		statestore.Deps{Clock: clk, Logger: slog.New(slog.DiscardHandler)}, memory.Options{Shards: 4, MaxEntries: 4096})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = mem.Close(context.Background()) })
	return &fixture{clk: clk, table: smallTable(tb, clk, count), store: &countingStore{Store: mem}, cel: celtest.Compiler(tb)}
}

// policy returns a ratelimit Policy named name with failureMode mode.
func policy(name string, mode v1alpha1.FailureMode) *v1alpha1.Policy {
	return &v1alpha1.Policy{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.PolicySpec{Type: Type, FailureMode: &mode},
	}
}

// env returns the BuildEnv of Policy p with cfg at Route scope.
func (fx *fixture) env(p *v1alpha1.Policy, cfg *v1alpha1.RateLimitConfig, pm *emit.PolicyMetrics) filter.BuildEnv {
	return filter.BuildEnv{
		Policy: p, Config: cfg, Scope: phase.ScopeRoute, Phases: phase.Of(phase.OnRequestHeaders),
		CEL: fx.cel.NewBuilder(nil, expr.InputCaps{}), StateStore: fx.store, StateStoreTimeout: stateTimeout,
		Metrics: pm, Clock: fx.clk,
	}
}

// build compiles cfg as Policy name (failureMode open) or stops the test.
func (fx *fixture) build(tb testing.TB, name string, cfg *v1alpha1.RateLimitConfig) (*Filter, *metrics) {
	tb.Helper()
	return fx.buildMode(tb, name, v1alpha1.FailureModeOpen, cfg)
}

// buildMode compiles cfg as Policy name with failureMode mode.
func (fx *fixture) buildMode(tb testing.TB, name string, mode v1alpha1.FailureMode, cfg *v1alpha1.RateLimitConfig) (*Filter, *metrics) {
	tb.Helper()
	pm, m := newMetrics()
	f, err := Factory{Table: fx.table}.Build(context.Background(), fx.env(policy(name, mode), cfg, pm))
	if err != nil {
		tb.Fatalf("Build: %v", err)
	}
	return f.(*Filter), m
}

// vars returns the activation of a request carrying x-key: key, and the
// Consumer tier when not empty.
func vars(key, tier string) *expr.Vars {
	v := &expr.Vars{
		Request: &expr.Request{Method: "GET", Scheme: "https", Host: "api.example", Path: "/orders", Header: http.Header{"X-Key": {key}}},
		Source:  &expr.Source{},
		Now:     testStart(),
	}
	if tier != "" {
		v.Consumer = celtest.Consumer(expr.Consumer{Name: key, Tier: tier})
	}
	return v
}

// outcome is one request's result as the executor would apply it.
type outcome struct {
	r filter.Result
	x *filtertest.Exchange
	// called reports a State Store round trip; call is the call Prepare
	// filled.
	called bool
	call   statestore.Call
}

// request runs one request through f as the executor's batch does:
// Prepare, then for a prepared call one round trip and Complete, then
// Finish.
func (fx *fixture) request(tb testing.TB, f *Filter, v *expr.Vars) outcome {
	tb.Helper()
	rb := statestore.NewRequestBudget(time.Second, 0)
	x := &filtertest.Exchange{VarsV: v, StateDeadline: &rb}
	o := outcome{x: x}
	call := statestore.Call{Batch: -1}
	r, done := f.Prepare(context.Background(), x, &call)
	if !done {
		o.called, o.call = true, call
		o.call.GCRA.Limits = append([]statestore.GCRALimit(nil), call.GCRA.Limits...)
		calls := []*statestore.Call{&call}
		fx.store.Consume(context.Background(), &rb, calls)
		if !call.Done && call.Err == nil {
			call.Err = statestore.ErrNotAttempted
		}
		r = f.Complete(context.Background(), x, &call)
	}
	f.Finish(context.Background(), x)
	o.r = r
	return o
}

// status returns the response status of a Respond result, 0 otherwise.
func (o outcome) status() int {
	if o.r.Outcome != filter.Respond || o.r.Response == nil {
		return 0
	}
	return o.r.Response.Status
}

// code returns the RZ code of a Respond or CannotDecide result.
func (o outcome) code() string {
	if o.r.Outcome == filter.Respond && o.r.Response != nil {
		return o.r.Response.Code
	}
	return o.r.Code
}

// retryAfter returns the Retry-After header of a Respond result.
func (o outcome) retryAfter() string {
	if o.r.Response == nil {
		return ""
	}
	return o.r.Response.Header.Get(headerRetryAfter)
}

// tokens returns the tokens limit i of f holds for key, refilled to now,
// and whether the table holds its entry.
func (fx *fixture) tokens(f *Filter, key string, i int) (int64, bool) {
	d := digestOf(key)
	if key == "" {
		d = f.emptyKey
	}
	sh := fx.table.shardOf(f.seed, &d)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	_, v := fx.table.find(sh, f.limits[i].id, &d)
	if v == 0 {
		return 0, false
	}
	e := sh.entry(v)
	n, ctrl := fx.table.count.Count()
	c, a := ceilingOf(f.limits[i].requests, f.limits[i].perNode, n, ctrl)
	now := fx.clk.Now().UnixNano()
	tokens, last, until := e.tokens, e.last, e.until
	if e.flags&flagFailOpen != 0 {
		failOpenRefill(&tokens, &last, &until, now, int64(f.limits[i].window), a)
	} else {
		refill(&tokens, &last, now, c, int64(f.limits[i].window), c)
	}
	return tokens, true
}

// failingOpen reports whether the first limit of f is on the fail-open
// refill for key a.
func (fx *fixture) failingOpen(f *Filter) bool {
	d := digestOf("a")
	sh := fx.table.shardOf(f.seed, &d)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	_, v := fx.table.find(sh, f.limits[0].id, &d)
	return v != 0 && sh.entry(v).flags&flagFailOpen != 0
}

// limit returns a limits[] entry.
func limit(requests int64, window time.Duration) v1alpha1.RateLimit {
	return v1alpha1.RateLimit{Requests: requests, Window: v1alpha1.Duration(window)}
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// keyed returns a config keyed by the x-key header.
func keyed(limits ...v1alpha1.RateLimit) *v1alpha1.RateLimitConfig {
	return &v1alpha1.RateLimitConfig{Key: `request.headers["x-key"]`, Limits: limits}
}
