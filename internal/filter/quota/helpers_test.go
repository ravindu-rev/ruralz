// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
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

// testStart is the fake clock's start: 10:00 UTC, so a 1h window ends an
// hour later and October 2026's month ends in 31 days and 14 hours.
func testStart() time.Time { return time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC) }

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

// metrics are one Policy's recorded decision counts.
type metrics struct {
	decisions [emit.NumQuotaResults]*counter
}

// newMetrics returns PolicyMetrics recording into a metrics.
func newMetrics() (*emit.PolicyMetrics, *metrics) {
	m := &metrics{}
	pm := &emit.PolicyMetrics{}
	for i := range m.decisions {
		m.decisions[i] = &counter{}
		pm.Quota[i] = m.decisions[i]
	}
	return pm, m
}

// decided returns the decision counts by result name.
func (m *metrics) decided() map[string]uint64 {
	names := [...]string{"allow", "deny", "no_quota", "fail_open", "fail_closed"}
	out := map[string]uint64{}
	for i, c := range m.decisions {
		if n := c.value(); n > 0 {
			out[names[i]] = n
		}
	}
	return out
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

// enqueuer records post-commit writes, refusing all with full.
type enqueuer struct {
	mu     sync.Mutex
	writes []*statestore.Write
	class  []statestore.WriteClass
	full   bool
}

func (e *enqueuer) Enqueue(class statestore.WriteClass, w *statestore.Write) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.full {
		return false
	}
	e.writes = append(e.writes, w)
	e.class = append(e.class, class)
	return true
}

// fixture is one Node: a fake clock, its denial cache, a memory State
// Store behind a counting store and a post-commit queue fake.
type fixture struct {
	clk   *clocktest.Fake
	cache *Cache
	mem   statestore.Driver
	store *countingStore
	queue *enqueuer
	cel   expr.Compiler
}

// newFixture returns a fixture with a small denial cache.
func newFixture(tb testing.TB) *fixture {
	tb.Helper()
	clk := clocktest.New(testStart())
	mem, err := memory.New(context.Background(), statestore.Config{Driver: statestore.DriverMemory},
		statestore.Deps{Clock: clk, Logger: slog.New(slog.DiscardHandler)}, memory.Options{Shards: 4, MaxEntries: 4096})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = mem.Close(context.Background()) })
	cache, err := NewCache(CacheOptions{Entries: 64, Shards: 2, Seed: 11})
	if err != nil {
		tb.Fatal(err)
	}
	return &fixture{clk: clk, cache: cache, mem: mem, store: &countingStore{Store: mem}, queue: &enqueuer{}, cel: celtest.Compiler(tb)}
}

// policy returns a quota Policy named name with failureMode mode.
func policy(name string, mode v1alpha1.FailureMode) *v1alpha1.Policy {
	return &v1alpha1.Policy{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.PolicySpec{Type: Type, FailureMode: &mode},
	}
}

// env returns the BuildEnv of Policy p with cfg at Route scope.
func (fx *fixture) env(p *v1alpha1.Policy, cfg *v1alpha1.QuotaConfig, pm *emit.PolicyMetrics) filter.BuildEnv {
	return filter.BuildEnv{
		Policy: p, Config: cfg, Scope: phase.ScopeRoute, Phases: phase.Of(phase.OnRequestHeaders, phase.OnLog),
		CEL: fx.cel.NewBuilder(nil, expr.InputCaps{}), StateStore: fx.store, Enqueuer: fx.queue,
		StateStoreTimeout: stateTimeout, Metrics: pm, Clock: fx.clk,
	}
}

// build compiles cfg as Policy name (failureMode open) or stops the test.
func (fx *fixture) build(tb testing.TB, name string, cfg *v1alpha1.QuotaConfig) (*Filter, *metrics) {
	tb.Helper()
	return fx.buildMode(tb, name, v1alpha1.FailureModeOpen, cfg)
}

// buildMode compiles cfg as Policy name with failureMode mode.
func (fx *fixture) buildMode(tb testing.TB, name string, mode v1alpha1.FailureMode, cfg *v1alpha1.QuotaConfig) (*Filter, *metrics) {
	tb.Helper()
	pm, m := newMetrics()
	f, err := Factory{Cache: fx.cache}.Build(context.Background(), fx.env(policy(name, mode), cfg, pm))
	if err != nil {
		tb.Fatalf("Build: %v", err)
	}
	return f.(*Filter), m
}

// config returns a quota config of consumerQuota with the default key and
// cost, or cost when given.
func config(consumerQuota string, cost ...string) *v1alpha1.QuotaConfig {
	c := &v1alpha1.QuotaConfig{ConsumerQuota: consumerQuota}
	if len(cost) > 0 {
		c.Cost = &cost[0]
	}
	return c
}

// windowQuota returns a Consumer quota of unit requests with limit per
// window.
func windowQuota(name string, limit int64, window time.Duration) v1alpha1.Quota {
	w := v1alpha1.Duration(window)
	return v1alpha1.Quota{Name: name, Unit: v1alpha1.QuotaUnitRequests, Limit: limit, Window: &w}
}

// monthQuota returns a calendar-month Consumer quota of unit requests.
func monthQuota(name string, limit int64) v1alpha1.Quota {
	return v1alpha1.Quota{Name: name, Unit: v1alpha1.QuotaUnitRequests, Limit: limit, Calendar: v1alpha1.QuotaCalendarMonth}
}

// consumer returns the compiled view of a Consumer named name with qs.
func consumer(name string, qs ...v1alpha1.Quota) *expr.Consumer {
	c := expr.Consumer{Name: name, Tags: []string{}, Labels: map[string]string{}, Quotas: []string{}}
	if len(qs) > 0 {
		c.QuotaByName = map[string]v1alpha1.Quota{}
		for _, q := range qs {
			c.Quotas = append(c.Quotas, q.Name)
			c.QuotaByName[q.Name] = q
		}
	}
	return celtest.Consumer(c)
}

// vars returns the activation of a request by c (nil for none) with
// method, carrying x-cost when not empty.
func vars(c *expr.Consumer, method, cost string) *expr.Vars {
	h := http.Header{}
	if cost != "" {
		h.Set("X-Cost", cost)
	}
	return &expr.Vars{
		Request:  &expr.Request{Method: method, Scheme: "https", Host: "api.example", Path: "/orders", Header: h},
		Source:   &expr.Source{},
		Consumer: c,
		Now:      testStart(),
	}
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

// exchange returns a filtertest.Exchange of one Policy named name in a
// request with activation v.
func exchange(name string, v *expr.Vars) *filtertest.Exchange {
	rb := statestore.NewRequestBudget(time.Second, 0)
	return &filtertest.Exchange{VarsV: v, StateDeadline: &rb, PolicyName: name}
}

// decide runs onRequestHeaders of f on x as the executor's batch does:
// Prepare, then for a prepared call one round trip and Complete.
func (fx *fixture) decide(tb testing.TB, f *Filter, x *filtertest.Exchange) outcome {
	tb.Helper()
	o := outcome{x: x}
	call := statestore.Call{Batch: -1}
	r, done := f.Prepare(context.Background(), x, &call)
	if !done {
		o.called, o.call = true, call
		fx.store.Consume(context.Background(), x.StateDeadline, []*statestore.Call{&call})
		if !call.Done && call.Err == nil {
			call.Err = statestore.ErrNotAttempted
		}
		r = f.Complete(context.Background(), x, &call)
	}
	o.r = r
	return o
}

// request runs one whole request through f: onRequestHeaders, then onLog
// with final and Finish.
func (fx *fixture) request(tb testing.TB, f *Filter, v *expr.Vars, final filter.Final) outcome {
	tb.Helper()
	x := exchange(f.name, v)
	o := fx.decide(tb, f, x)
	x.FinalV = final
	f.Handle(context.Background(), phase.OnLog, x)
	f.Finish(context.Background(), x)
	return o
}

// served is the Final of a request an Upstream answered.
func served() filter.Final {
	return filter.Final{Status: http.StatusOK, Committed: true, UpstreamReached: true}
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

// retryAfter returns the Retry-After of a Respond result in seconds, -1
// without one.
func (o outcome) retryAfter() int64 {
	if o.r.Response == nil {
		return -1
	}
	n, err := strconv.ParseInt(o.r.Response.Header.Get(headerRetryAfter), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// fields returns the RateLimit fields the request recorded.
func (o outcome) fields() []filter.RateLimitField { return o.x.RateLimits }
