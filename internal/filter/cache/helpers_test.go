// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/cache/revalidate"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Fixtures.
const (
	testRoute    = "catalog"
	testUpstream = "catalog-svc"
	testPolicy   = "cache-test"
	testTimeout  = 50 * time.Millisecond
	testPath     = "/v1/catalog"
)

// t0 is the fake clock's start.
var t0 = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

// date formats t as an HTTP-date.
func date(t time.Time) string { return t.UTC().Format(http.TimeFormat) }

// seqSource returns 1, 2, 3, ... safely across goroutines.
type seqSource struct{ n atomic.Uint64 }

// Uint64 implements rand.Source.
func (s *seqSource) Uint64() uint64 { return s.n.Add(1) }

// counter is an emit.Counter that counts.
type counter struct{ n atomic.Uint64 }

// Add implements emit.Counter.
func (c *counter) Add(_ emit.Stripe, n uint64) { c.n.Add(n) }

// testStore wraps the memory driver: it counts Read batches, can fail
// them, and answers AdmitStoreBytes itself, recording what it was asked.
type testStore struct {
	statestore.Store
	mu      sync.Mutex
	reads   int
	readErr *statestore.Error
	refuse  bool
	admits  []int
}

// Read implements statestore.Store.
func (s *testStore) Read(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) {
	s.mu.Lock()
	s.reads++
	err := s.readErr
	s.mu.Unlock()
	if err != nil {
		for _, c := range calls {
			c.Err = err
		}
		return
	}
	s.Store.Read(ctx, rb, calls)
}

// AdmitStoreBytes implements statestore.Store.
func (s *testStore) AdmitStoreBytes(_ statestore.CacheKey, n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admits = append(s.admits, n)
	return !s.refuse
}

// readCount returns the Read batches so far.
func (s *testStore) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// queued is one post-commit write.
type queued struct {
	class statestore.WriteClass
	w     *statestore.Write
}

// testQueue writes every enqueued write to its store at once, recording
// it; drop makes it refuse.
type testQueue struct {
	mu     sync.Mutex
	store  statestore.Store
	writes []queued
	drop   bool
}

// Enqueue implements statestore.Enqueuer.
func (q *testQueue) Enqueue(class statestore.WriteClass, w *statestore.Write) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.drop {
		return false
	}
	q.store.Write(context.Background(), []*statestore.Write{w})
	q.writes = append(q.writes, queued{class: class, w: w})
	return true
}

// recorded returns the writes so far.
func (q *testQueue) recorded() []queued {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.writes)
}

// harness is one cache Filter over the memory driver on a fake clock.
type harness struct {
	t       *testing.T
	clk     *clocktest.Fake
	mem     *memory.Driver
	store   *testStore
	queue   *testQueue
	rv      *revalidate.Revalidator
	fac     *Factory
	f       *Filter
	metrics *emit.RouteMetrics
	results [emit.NumCacheResults]*counter
	skips   [emit.NumSkipReasons]*counter
	replay  filter.Replayer
}

// option changes a harness's BuildEnv before Build.
type option func(env *filter.BuildEnv)

// withKey sets config.key, compiled by a real CEL builder.
func withKey(src string) option {
	return func(env *filter.BuildEnv) {
		env.Config = &v1alpha1.CacheConfig{Key: src}
	}
}

// withoutReplayer builds the Filter without a Replayer.
func withoutReplayer() option {
	return func(env *filter.BuildEnv) { env.Replayer = nil }
}

// withBundle builds the Filter within Bundle b.
func withBundle(b *hub.Bundle) option {
	return func(env *filter.BuildEnv) { env.Bundle = b }
}

// newHarness returns a harness whose Replayer answers 304 to every replay.
func newHarness(t *testing.T, opts ...option) *harness {
	t.Helper()
	clk := clocktest.New(t0)
	mem, err := memory.New(t.Context(), statestore.Config{}, statestore.Deps{Clock: clk}, memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close(context.Background()) })
	h := &harness{t: t, clk: clk, mem: mem, store: &testStore{Store: mem}, metrics: &emit.RouteMetrics{}}
	h.queue = &testQueue{store: h.store}
	for i := range h.results {
		h.results[i] = &counter{}
		h.metrics.Cache[i] = h.results[i]
	}
	h.rv = revalidate.New(revalidate.Options{Clock: clk, Rand: &seqSource{}})
	h.fac, err = NewFactory(h.rv, Options{Clock: clk, Rand: &seqSource{}})
	if err != nil {
		t.Fatal(err)
	}
	h.replay = filtertest.ReplayFunc(func(context.Context, string, string, *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotModified, Header: http.Header{}, Body: http.NoBody}, nil
	})
	h.f = h.build(opts...)
	return h
}

// build compiles a cache Policy in h's environment.
func (h *harness) build(opts ...option) *Filter {
	h.t.Helper()
	env := h.env()
	for _, o := range opts {
		o(&env)
	}
	flt, err := h.fac.Build(context.Background(), env)
	if err != nil {
		h.t.Fatalf("Build: %v", err)
	}
	return flt.(*Filter)
}

// env returns the BuildEnv of the test Policy.
func (h *harness) env() filter.BuildEnv {
	pm := &emit.PolicyMetrics{}
	for i := range h.skips {
		if h.skips[i] == nil {
			h.skips[i] = &counter{}
		}
		pm.StoreSkipped[i] = h.skips[i]
	}
	return filter.BuildEnv{
		Policy: &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: testPolicy}, Spec: v1alpha1.PolicySpec{Type: Type}},
		Config: &v1alpha1.CacheConfig{}, Scope: phase.ScopeRoute, Phases: phase.Of(phase.OnRequestHeaders, phase.OnResponse),
		CEL:        celtest.Compiler(h.t).NewBuilder(nil, expr.InputCaps{}),
		CacheStore: h.store, CacheEnqueuer: h.queue, StateStoreTimeout: testTimeout,
		Metrics: pm, Clock: h.clk, Replayer: h.replay,
	}
}

// request returns an Exchange of a request with method, query and header
// hdr on the test Route.
func (h *harness) request(method, query string, hdr http.Header) *filtertest.Exchange {
	if hdr == nil {
		hdr = http.Header{}
	}
	rb := statestore.NewRequestBudget(time.Second, 0)
	r := &expr.Request{Method: method, Scheme: "https", Host: "shop.example", Path: testPath, Header: hdr}
	r.SetRawQuery(query)
	route := &expr.Route{Name: testRoute}
	return &filtertest.Exchange{
		MethodV: method, SchemeV: "https", HostV: "shop.example", PathV: testPath,
		ReceivedV: filter.Received{Authority: "shop.example", RawQuery: query},
		Hdr:       hdr, Msg: &filtertest.Message{Hdr: hdr, Query: query},
		RouteV: route, VarsV: &expr.Vars{Request: r, Route: route},
		StateDeadline: &rb, RouteMetricsV: h.metrics,
	}
}

// get returns an Exchange of a GET with header hdr.
func (h *harness) get(hdr http.Header) *filtertest.Exchange {
	return h.request(http.MethodGet, "page=2", hdr)
}

// leg is the response the Upstream gives a forwarded request: a status,
// header and body, or a Node-generated failure status.
type leg struct {
	status int
	header http.Header
	body   string
	// failed is the status of a Node-generated failure of the leg: no leg
	// response.
	failed int
	// incomplete stops the tee before the body ends; noTee refuses it.
	incomplete, noTee bool
}

// outcome is what a client received.
type outcome struct {
	// result is the onRequestHeaders Result.
	result filter.Result
	// status, header and body are the client response after onResponse.
	status int
	header http.Header
	body   []byte
	// cached reports a response the cache served.
	cached bool
	// forwarded reports a request sent to the Upstream.
	forwarded bool
	x         *filtertest.Exchange
}

// do runs x through the Filter as the executor runs a request whose
// forward the Upstream answers with l: onRequestHeaders; a served response
// runs onResponse when Cached; a forward offers the leg response to
// Substitute, then runs onResponse; Finish last.
func (h *harness) do(x *filtertest.Exchange, l leg) outcome {
	h.t.Helper()
	ctx := context.Background()
	o := outcome{x: x}
	o.result = h.f.Handle(ctx, phase.OnRequestHeaders, x)
	switch o.result.Outcome {
	case filter.Respond:
		resp := o.result.Response
		o.status, o.header, o.body, o.cached = resp.Status, resp.Header, resp.Body, resp.Cached
		if resp.Cached {
			x.Msg = &filtertest.Message{Hdr: resp.Header, Code: resp.Status, Raw: resp.Body}
			h.f.Handle(ctx, phase.OnResponse, x)
		}
	case filter.Continue:
		o.forwarded = true
		h.respond(ctx, x, l, &o)
	default:
	}
	h.f.Finish(ctx, x)
	return o
}

// respond delivers l to a forwarded request: the leg response, then
// Substitute and onResponse.
func (h *harness) respond(ctx context.Context, x *filtertest.Exchange, l leg, o *outcome) {
	if l.failed != 0 {
		x.LegStatusV, x.LegHeaderV = 0, nil
		x.Msg = &filtertest.Message{Hdr: http.Header{}, Code: l.failed, Gen: true}
	} else {
		hdr := l.header
		if hdr == nil {
			hdr = http.Header{}
		}
		x.LegStatusV, x.LegHeaderV = l.status, hdr.Clone()
		x.Msg = &filtertest.Message{Hdr: hdr.Clone(), Code: l.status, Raw: []byte(l.body)}
		x.VarsV.Upstream = &expr.Upstream{Name: testUpstream}
		if !l.noTee {
			x.TeeV = &filtertest.Tee{Body: []byte(l.body), Complete: !l.incomplete}
		}
	}
	if sub := h.f.Substitute(ctx, x); sub != nil {
		x.ReplaceResponse(sub)
		x.Msg = &filtertest.Message{Hdr: sub.Header, Code: sub.Status, Raw: sub.Body}
		o.cached = true
	}
	h.f.Handle(ctx, phase.OnResponse, x)
	m := x.Msg.(*filtertest.Message)
	o.status, o.header, o.body = m.Code, m.Hdr, m.Raw
}

// fresh60 is a storable response header: 60 s fresh, with validators.
func fresh60(now time.Time) http.Header {
	return http.Header{
		"Cache-Control": {"max-age=60"},
		"Etag":          {`"v1"`},
		"Last-Modified": {date(now.Add(-time.Hour))},
		"Date":          {date(now)},
		"Content-Type":  {"application/json"},
	}
}

// prime stores body under hdr for a GET with request header req, through
// a miss, and checks it was queued.
func (h *harness) prime(req, hdr http.Header, body string) {
	h.t.Helper()
	before := len(h.queue.recorded())
	o := h.do(h.get(req), leg{status: http.StatusOK, header: hdr, body: body})
	if !o.forwarded {
		h.t.Fatalf("priming request was not forwarded: %+v", o.result)
	}
	if n := len(h.queue.recorded()); n != before+1 {
		h.t.Fatalf("priming queued %d writes, want 1", n-before)
	}
}

// count returns the cache results counted so far, by emit.Cache* index.
func (h *harness) count() [emit.NumCacheResults]uint64 {
	var out [emit.NumCacheResults]uint64
	for i, c := range h.results {
		out[i] = c.n.Load()
	}
	return out
}

// results returns a count with n of each result index named.
func results(kv ...int) [emit.NumCacheResults]uint64 {
	var out [emit.NumCacheResults]uint64
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = uint64(kv[i+1])
	}
	return out
}

// cacheStatus returns the Cache-Status lines of h.
func cacheStatus(h http.Header) []string { return h["Cache-Status"] }
