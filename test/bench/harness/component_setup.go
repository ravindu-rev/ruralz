// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/jwt"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/jwt/jwks"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/filter/ratelimit"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/internal/telemetry/aggregate"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
	"github.com/ravindu-rev/ruralz/test/bench/gen"
)

// The component benchmarks the M1 benchmark scenarios add (PBB "Suite
// layers"): the loader compile ladder over test/bench/gen's Bundles, JWT
// per algorithm, token bucket shards, metric aggregates at 4 P and 32 P,
// and metric recording per request, whose per-request CPU PB-10 adds.
// None of them carries a target of its own: their values are reported
// and gated by the 5% rule against their baselines.

// loaderComponents returns one loader compile rung per ladder size: one
// call runs the configuration pipeline, decode to CEL compile, on the
// size's generated Bundle with the loader's max(1, GOMAXPROCS/2) workers.
func loaderComponents() []Component {
	calls := map[int]int{100: 200, 1000: 40, 5000: 8, 10000: 4, 17000: 3}
	tests := map[int]int{100: 10, 1000: 2}
	var out []Component
	for _, r := range gen.Sizes() {
		out = append(out, Component{Name: "loader-" + strconv.Itoa(r), Calls: calls[r], Warm: 1, TestCalls: max(tests[r], 1), Setup: loaderCall(r)})
	}
	return out
}

// loaderCall compiles the size's Bundle once per call.
func loaderCall(routes int) func(context.Context, int) (func(int) error, func(), error) {
	return func(ctx context.Context, _ int) (func(int) error, func(), error) {
		c, err := cel.New()
		if err != nil {
			return nil, nil, err
		}
		p, err := pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: c, Workers: max(1, runtime.GOMAXPROCS(0)/2)})
		if err != nil {
			return nil, nil, err
		}
		fsys, err := gen.FS(gen.Spec{Routes: routes, Lab: gen.LoopbackLab()})
		if err != nil {
			return nil, nil, err
		}
		return func(int) error {
			res, err := p.Run(ctx, pipeline.Request{Source: loader.Source{FS: fsys, Path: "."}})
			if err != nil {
				return err
			}
			if res.HasErrors() || res.Validated == nil {
				return fmt.Errorf("harness: the %d-Route Bundle does not compile: %v", routes, res.Diagnostics)
			}
			return nil
		}, nil, nil
	}
}

// jwtAlgs are the auth.jwt algorithms the mock IdP signs with.
func jwtAlgs() []mockidp.Alg {
	return []mockidp.Alg{mockidp.RS256, mockidp.PS256, mockidp.ES256, mockidp.EdDSA}
}

// jwtComponents returns one auth.jwt component per algorithm: one call
// authenticates a bearer token from the cached key set, the
// onRequestHeaders auth.jwt row of the per-stage latency budget (RS256
// with a 2,048-bit key, OQ-performance-budgets-and-benchmarking-8).
func jwtComponents() []Component {
	var out []Component
	for _, alg := range jwtAlgs() {
		out = append(out, Component{Name: "jwt-" + strings.ToLower(string(alg)), Calls: 100_000, Warm: 10_000, Setup: jwtCall(alg)})
	}
	return out
}

// jwtCall builds an auth.jwt Filter through its Factory over a JWKS
// manager fetching from a mock IdP over TLS on loopback, mints a token
// with alg and authenticates it once per call.
func jwtCall(alg mockidp.Alg) func(context.Context, int) (func(int) error, func(), error) {
	return func(ctx context.Context, workers int) (call func(int) error, release func(), err error) {
		var closers []func()
		release = func() {
			for i := len(closers) - 1; i >= 0; i-- {
				closers[i]()
			}
		}
		defer func() {
			if err != nil {
				release()
			}
		}()
		ca, err := pki.NewCA(pki.CAOptions{})
		if err != nil {
			return nil, nil, err
		}
		leaf, err := ca.Server("127.0.0.1")
		if err != nil {
			return nil, nil, err
		}
		srvTLS, err := leaf.ServerTLS(nil)
		if err != nil {
			return nil, nil, err
		}
		var keys []*mockidp.Key
		for _, a := range jwtAlgs() {
			k, err := mockidp.GenerateKey(a)
			if err != nil {
				return nil, nil, err
			}
			keys = append(keys, k)
		}
		idp, err := mockidp.Start(ctx, mockidp.Config{TLS: srvTLS, Keys: keys})
		if err != nil {
			return nil, nil, err
		}
		closers = append(closers, func() { _ = idp.Close() })
		guard, err := egress.ParseAllow("127.0.0.1")
		if err != nil {
			return nil, nil, err
		}
		m := jwks.New(jwks.Options{Guard: guard, TLS: tlsconf.Client(ca.Pool())})
		mctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { // owned by release, which cancels and waits for it
			defer close(done)
			_ = m.Run(mctx)
		}()
		closers = append(closers, func() { cancel(); <-done })
		f, err := jwt.NewFactory(m, nil)
		if err != nil {
			return nil, nil, err
		}
		cfg := &v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{{Issuer: idp.Issuer(), JWKSURL: idp.JWKSURL(), Audiences: []string{BenchAudience}}}}
		env := filter.BuildEnv{
			Policy: &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "jwt-bench"}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeAuthJWT}},
			Config: cfg, Scope: phase.ScopeGateway, Phases: phase.Of(phase.OnRequestHeaders),
			Bundle: hub.NewBundle(nil), Consumers: identity.CompileConsumers(nil).Map(),
		}
		flt, err := f.Build(ctx, env)
		if err != nil {
			return nil, nil, err
		}
		closers = append(closers, func() { closeFilter(flt) })
		tok, err := idp.Mint(alg, map[string]any{"aud": BenchAudience, "sub": "bench-1"})
		if err != nil {
			return nil, nil, err
		}
		xs := make([]*filtertest.Exchange, workers)
		for i := range xs {
			xs[i] = &filtertest.Exchange{Start: time.Now(), Hdr: http.Header{"Authorization": {"Bearer " + tok}}}
		}
		return func(w int) error {
			x := xs[w]
			x.Ident = nil
			if r := flt.Handle(ctx, phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
				return fmt.Errorf("harness: auth.jwt %s did not admit its token: %+v", alg, r)
			}
			return nil
		}, release, nil
	}
}

// tokenBucketCall builds S2's ratelimit-bench Filter on the memory driver
// and decides one request per call, each goroutine's keys cycling over
// S2's 100,000 values: the local token bucket shard, then GCRA, the
// onRequestHeaders ratelimit row of the per-stage latency budget, with
// GOMAXPROCS goroutines contending for the key table's shards.
func tokenBucketCall(ctx context.Context, workers int) (func(int) error, func(), error) {
	clk := clock.Real()
	table, err := ratelimit.NewTable(clk, ratelimit.FileMode{}, ratelimit.DefaultTableOptions())
	if err != nil {
		return nil, nil, err
	}
	store, err := memory.New(ctx, statestore.Config{Driver: statestore.DriverMemory},
		statestore.Deps{Clock: clk, Logger: slog.New(slog.DiscardHandler)}, memory.Options{})
	if err != nil {
		return nil, nil, err
	}
	c, err := cel.New()
	if err != nil {
		_ = store.Close(ctx)
		return nil, nil, err
	}
	cfg := &v1alpha1.RateLimitConfig{
		Key:    `request.headers["x-bench-key"]`,
		Limits: []v1alpha1.RateLimit{{Requests: 20000, Window: v1alpha1.Duration(time.Second)}},
	}
	env := filter.BuildEnv{
		Policy: &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "ratelimit-bench"}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeRateLimit}},
		Config: cfg, Scope: phase.ScopeGateway, Phases: phase.Of(phase.OnRequestHeaders),
		CEL: c.NewBuilder(nil, expr.InputCaps{RequestHeaderBytes: 64 << 10}), StateStore: store, StateStoreTimeout: time.Second, Clock: clk,
	}
	flt, err := ratelimit.Factory{Table: table}.Build(ctx, env)
	if err != nil {
		_ = store.Close(ctx)
		return nil, nil, err
	}
	release := func() { closeFilter(flt); _ = store.Close(context.WithoutCancel(ctx)) }
	xs := make([]*filtertest.Exchange, workers)
	seq := make([]int, workers)
	budgets := make([]statestore.RequestBudget, workers)
	for i := range xs {
		xs[i] = &filtertest.Exchange{Start: time.Now(), Hdr: http.Header{}, VarsV: &expr.Vars{Request: &expr.Request{Method: "GET", Path: "/bench/1k", Header: http.Header{}}}}
	}
	return func(w int) error {
		x := xs[w]
		seq[w]++
		key := "key-" + strconv.Itoa((w*BenchKeyCardinality/max(workers, 1)+seq[w])%BenchKeyCardinality)
		x.Hdr.Set("X-Bench-Key", key)
		x.VarsV.Request.Header = x.Hdr
		x.Start = time.Now()
		// Each request's State Store budget starts fresh at bench-1k's
		// 5 s Route timeout.
		budgets[w] = statestore.NewRequestBudget(5*time.Second, 0)
		x.StateDeadline = &budgets[w]
		if r := flt.Handle(ctx, phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
			return fmt.Errorf("harness: ratelimit-bench denied %s: %+v", key, r)
		}
		return nil
	}, release, nil
}

// closeFilter closes a Filter that holds resources (filter.Closer).
func closeFilter(f filter.Filter) {
	if c, ok := f.(filter.Closer); ok {
		_ = c.Close()
	}
}

// requestHandles are the metric handles one S2 request records through.
type requestHandles struct {
	l        *emit.ListenerMetrics
	route    *emit.RouteMetrics
	up       *emit.UpstreamMetrics
	policies []*emit.PolicyMetrics
	state    *emit.StateMetrics
}

// s2Shape is S2's metric shape: auth.jwt and ratelimit at the Gateway, one
// listener, Route and Upstream.
func s2Shape() emit.Shape {
	req := phase.Of(phase.OnRequestHeaders)
	return emit.Shape{
		Listeners: []string{"http"}, Routes: []string{"bench-1k"}, Upstreams: []string{"mock"},
		Policies: []emit.PolicyShape{
			{Name: "jwt-bench", Type: "auth.jwt", Phases: req, Codes: []string{"RZ-AUTH-001"}, Gateway: true},
			{Name: "ratelimit-bench", Type: "ratelimit", Phases: req, Gateway: true},
		},
	}
}

// bindS2 admits and binds S2's shape on a fresh registry.
func bindS2() (*aggregate.Registry, requestHandles, emit.Binding, error) {
	r, err := aggregate.New(aggregate.Options{})
	if err != nil {
		return nil, requestHandles{}, nil, err
	}
	pl, err := r.Admit(s2Shape())
	if err != nil {
		return nil, requestHandles{}, nil, err
	}
	plan, ok := pl.(*aggregate.Plan)
	if !ok {
		return nil, requestHandles{}, nil, errors.New("harness: the registry admitted no aggregate plan")
	}
	b := r.Bind(pl)
	h := requestHandles{l: plan.Listener("http"), route: plan.Route("bench-1k"), up: plan.Upstream("mock"), state: &r.Node().State}
	for _, p := range s2Shape().Policies {
		h.policies = append(h.policies, plan.Policy(p.Name))
	}
	return r, h, b, nil
}

// request records the 15 metric operations of an S2 request (Observability
// "Overhead per signal": 15 in the SM-4 scenario).
func (h *requestHandles) request(s emit.Stripe, v uint64) {
	h.l.Active.Add(s, 1)
	h.policies[0].Duration[phase.OnRequestHeaders].Record(s, v)
	h.policies[0].Auth.Allow(s)
	h.policies[1].Duration[phase.OnRequestHeaders].Record(s, v)
	h.policies[1].RateLimit[emit.RateLimitAllow].Add(s, 1)
	h.state.Calls[emit.StateOpGCRA][emit.StateResultOK].Add(s, 1)
	h.up.Attempts.Inc(s, 200, emit.ErrNone)
	h.up.AttemptDuration.Record(s, v*10)
	h.route.Requests.Inc(s, 200)
	h.route.Duration.Record(s, v*12)
	h.l.Requests.Inc(s, emit.ProtoHTTP1, 200, emit.OriginUpstream)
	h.l.GatewayDuration.Record(s, v)
	h.l.RequestBody.Record(s, 512)
	h.l.ResponseBody.Record(s, 2048)
	h.l.Active.Add(s, -1)
}

// metricRecordingCall records one S2 request's metric operations per call,
// each on the next connection's stripe: the per-request CPU of metric
// recording PB-10 adds, since metric recording has no switch.
func metricRecordingCall(context.Context, int) (func(int) error, func(), error) {
	r, h, b, err := bindS2()
	if err != nil {
		return nil, nil, err
	}
	s := r.NewStripe()
	var i uint64
	return func(int) error {
		i++
		h.request(s+emit.Stripe(i), i) //nolint:gosec // G115: stripes wrap by design.
		return nil
	}, b.Release, nil
}

// metricAggregatesCall records S2's request operations from every
// goroutine at once, each on a stripe of its own, the hot Route of a
// throughput-scaling check (PBB "From symptom to profile": at 32 P).
func metricAggregatesCall(_ context.Context, workers int) (func(int) error, func(), error) {
	r, h, b, err := bindS2()
	if err != nil {
		return nil, nil, err
	}
	stripes := make([]emit.Stripe, workers)
	for i := range stripes {
		stripes[i] = r.NewStripe()
	}
	var n atomic.Uint64
	return func(w int) error {
		h.request(stripes[w], n.Add(1))
		return nil
	}, b.Release, nil
}

// m1Components returns the component benchmarks of this file.
func m1Components() []Component {
	out := loaderComponents()
	out = append(out, jwtComponents()...)
	return append(out,
		Component{Name: "token-bucket-shards", Calls: 400_000, Warm: 40_000, Parallel: runtime.GOMAXPROCS(0), Setup: tokenBucketCall},
		Component{Name: "metric-aggregates-4p", Calls: 1_000_000, Warm: 100_000, Parallel: 4, Procs: 4, Setup: metricAggregatesCall},
		Component{Name: "metric-aggregates-32p", Calls: 1_000_000, Warm: 100_000, Parallel: 32, Procs: 32, Setup: metricAggregatesCall},
		Component{Name: "metric-recording", Calls: 1_000_000, Warm: 100_000, Setup: metricRecordingCall},
	)
}
