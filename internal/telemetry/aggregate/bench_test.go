// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// requestHandles are the handles one request records through.
type requestHandles struct {
	l        *emit.ListenerMetrics
	route    *emit.RouteMetrics
	up       *emit.UpstreamMetrics
	policies []*emit.PolicyMetrics
	state    *emit.StateMetrics
}

// s2Handles binds the S2 scenario (SM-4): auth.jwt and ratelimit at the
// Gateway, one Route, one Upstream, memory driver.
func s2Handles(tb testing.TB, r *Registry, extra int) (requestHandles, emit.Binding) {
	tb.Helper()
	req := phases(phase.OnRequestHeaders)
	pols := []emit.PolicyShape{
		{Name: "jwt", Type: "auth.jwt", Phases: req, Codes: []string{"RZ-AUTH-001"}, Gateway: true},
		{Name: "rl", Type: "ratelimit", Phases: req, Gateway: true},
	}
	for i := range extra {
		pols = append(pols, emit.PolicyShape{
			Name: fmt.Sprintf("f%02d", i), Type: "headers", Phases: phases(phase.OnRequestHeaders, phase.OnResponse), Gateway: true,
		})
	}
	pl, b := admitBind(tb, r, emit.Shape{Listeners: []string{"public"}, Routes: []string{"orders"}, Upstreams: []string{"orders-svc"}, Policies: pols})
	h := requestHandles{l: pl.Listener("public"), route: pl.Route("orders"), up: pl.Upstream("orders-svc"), state: &r.Node().State}
	for _, p := range pols {
		h.policies = append(h.policies, pl.Policy(p.Name))
	}
	return h, b
}

// request15 records the 15 metric operations of an S2 request.
func (h *requestHandles) request15(s emit.Stripe, v uint64) {
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

// request40 records 40 operations: the 10 operations of the listener,
// Route and Upstream, plus three per Filter for ten Filters.
func (h *requestHandles) request40(s emit.Stripe, v uint64) {
	h.l.Active.Add(s, 1)
	h.up.Attempts.Inc(s, 200, emit.ErrNone)
	h.up.AttemptDuration.Record(s, v*10)
	h.route.Requests.Inc(s, 200)
	h.route.Duration.Record(s, v*12)
	h.l.Requests.Inc(s, emit.ProtoHTTP1, 200, emit.OriginUpstream)
	h.l.GatewayDuration.Record(s, v)
	h.l.RequestBody.Record(s, 512)
	h.l.ResponseBody.Record(s, 2048)
	h.l.Active.Add(s, -1)
	for _, p := range h.policies[2:12] {
		p.Duration[phase.OnRequestHeaders].Record(s, v)
		p.Duration[phase.OnResponse].Record(s, v)
		p.ShortCircuits[phase.OnRequestHeaders].Inc(s, 200)
	}
}

// Done when and spec 09 req 47: hot-path recording allocates nothing, on
// the plain, exemplar, folded and code paths.
func TestHotPathZeroAllocs_Req47(t *testing.T) {
	r, _ := newTestRegistry(t, func(o *Options) { o.Limits = Limits{HistogramFamily: 1} })
	h, b := s2Handles(t, r, 10)
	defer b.Release()
	ex := emit.Exemplar{TraceID: [16]byte{1}, SpanID: [8]byte{2}, Time: epoch()}
	h.policies[0].Auth.Deny(0, "RZ-AUTH-001") // first record installs nothing: pre-created
	r.Node().NodeResponses.Inc(0, "RZ-RT-005")
	cases := map[string]func(){
		"15 operations": func() { h.request15(1, 100_000) },
		"40 operations": func() { h.request40(2, 100_000) },
		"exemplar":      func() { h.route.Duration.RecordExemplar(3, 1_000_000, ex) },
		"folded":        func() { h.policies[5].Duration[phase.OnRequestHeaders].Record(0, 1) },
		"deny code":     func() { h.policies[0].Auth.Deny(0, "RZ-AUTH-001") },
		"node code":     func() { r.Node().NodeResponses.Inc(0, "RZ-RT-005") },
		"gauge set":     func() { h.up.HealthyEndpoints.Set(3) },
		"gateway timer": func() {
			var g emit.GatewayTimer
			g.Reset(epoch())
			g.Enter(epoch().Add(time.Microsecond))
			g.Leave(epoch().Add(2 * time.Microsecond))
			g.Observe(epoch().Add(3*time.Microsecond), 0, h.l.GatewayDuration, r.Node().GatewayDurationSkipped)
		},
	}
	for name, f := range cases {
		if n := testing.AllocsPerRun(1000, f); n != 0 {
			t.Errorf("%s: %v allocations per run, want 0", name, n)
		}
	}
}

// BenchmarkMetricsPerRequest is spec 09 test 44: the 15 metric operations
// of an S2 request; 0 allocs/op.
func BenchmarkMetricsPerRequest(b *testing.B) {
	r, _ := newTestRegistry(b, func(o *Options) { o.Stripes = 0 })
	h, bind := s2Handles(b, r, 0)
	defer bind.Release()
	s := r.NewStripe()
	b.ReportAllocs()
	var i uint64
	for b.Loop() {
		i++
		h.request15(s+emit.Stripe(i), i)
	}
}

// BenchmarkMetricsPerRequest10Filters is spec 09 test 44: 40 operations.
func BenchmarkMetricsPerRequest10Filters(b *testing.B) {
	r, _ := newTestRegistry(b, func(o *Options) { o.Stripes = 0 })
	h, bind := s2Handles(b, r, 10)
	defer bind.Release()
	s := r.NewStripe()
	b.ReportAllocs()
	var i uint64
	for b.Loop() {
		i++
		h.request40(s+emit.Stripe(i), i)
	}
}

// BenchmarkHotRouteContention is spec 09 test 45: one hot Route through
// the five Gateway Policies of the worked example from every P; run with
// -cpu 4,32.
func BenchmarkHotRouteContention(b *testing.B) {
	r, _ := newTestRegistry(b, func(o *Options) { o.Stripes = 0 })
	pl, bind := admitBind(b, r, emit.Shape{
		Listeners: []string{"public"}, Routes: []string{"orders"}, Upstreams: []string{"orders-svc"}, Policies: gatewayPolicies(),
	})
	defer bind.Release()
	var pms []*emit.PolicyMetrics
	for _, p := range gatewayPolicies() {
		pms = append(pms, pl.Policy(p.Name))
	}
	l, route, up := pl.Listener("public"), pl.Route("orders"), pl.Upstream("orders-svc")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		s := r.NewStripe()
		var i uint64
		for pb.Next() {
			i++
			for _, pm := range pms {
				pm.Duration[phase.OnRequestHeaders].Record(s, i)
			}
			pms[1].Auth.Allow(s)
			pms[2].RateLimit[emit.RateLimitAllow].Add(s, 1)
			pms[3].Quota[emit.QuotaAllow].Add(s, 1)
			up.Attempts.Inc(s, 200, emit.ErrNone)
			up.AttemptDuration.Record(s, i)
			route.Requests.Inc(s, 200)
			route.Duration.Record(s, i)
			l.Requests.Inc(s, emit.ProtoHTTP1, 200, emit.OriginUpstream)
			l.GatewayDuration.Record(s, i)
		}
	})
}

// capSeries is the Node cap of spec 09 req 56: 100,000 series of the
// active Revision plus 25,000 retiring ones.
const capSeries = 125000

// bigRegistry builds a registry at the Node cap with S = 8: a Revision
// whose Policies, Routes and Upstreams fill the 100,000-series Revision
// limit (the rest folds), and a retired one holding 961 Routes (24,986
// series, just under the 25,000 retiring ceiling).
func bigRegistry(b *testing.B) *Registry {
	r, _ := newTestRegistry(b, func(o *Options) { o.Stripes = 8 })
	shape := func(prefix string, routes, upstreams, policies int) emit.Shape {
		var s emit.Shape
		s.Listeners = []string{"public", "internal"}
		for i := range routes {
			s.Routes = append(s.Routes, fmt.Sprintf("%sr%04d", prefix, i))
		}
		s.CachedRoutes = s.Routes
		for i := range upstreams {
			s.Upstreams = append(s.Upstreams, fmt.Sprintf("%su%04d", prefix, i))
		}
		for i := range policies {
			s.Policies = append(s.Policies, emit.PolicyShape{
				Name: fmt.Sprintf("%sp%04d", prefix, i), Type: "ratelimit", Phases: phases(phase.OnRequestHeaders),
			})
		}
		return s
	}
	_, old := admitBind(b, r, shape("old-", 961, 0, 0))
	_, _ = admitBind(b, r, shape("", 2000, 1000, 1000))
	old.Retire()
	return r
}

// BenchmarkCollect125k is spec 09 test 46: one collection of a registry at
// the Node cap; reports the live heap of the registry and its pooled
// collection.
func BenchmarkCollect125k(b *testing.B) {
	heap := func() uint64 {
		var ms runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	before := heap()
	r := bigRegistry(b)
	clk := r.clk.(interface{ Advance(time.Duration) })
	p := r.Producer()
	aggregates := heap()
	sm, err := p.Produce(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	points := 0
	for _, m := range sm[0].Metrics {
		points += dataPoints(m.Data)
	}
	pts := flatten(b, sm)
	live, retiring := pts[catalog.TelemetrySeries+`{state="live"}`].value, pts[catalog.TelemetrySeries+`{state="retiring"}`].value
	series := live + retiring
	if series < capSeries || retiring > float64(r.limits.Retiring) {
		b.Fatalf("registry holds %v live and %v retiring series, want at least %d in all", live, retiring, capSeries)
	}
	p.Release(sm)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		clk.Advance(ReuseWindow)
		sm, err := p.Produce(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		p.Release(sm)
	}
	b.StopTimer()
	after := heap()
	// MiB-heap is the whole registry: aggregates, striping and handles
	// of both bound Revisions (MiB-aggregates) plus the pooled collection.
	b.ReportMetric(float64(after-before)/(1<<20), "MiB-heap")
	b.ReportMetric(float64(aggregates-before)/(1<<20), "MiB-aggregates")
	b.ReportMetric(float64(points), "points")
	b.ReportMetric(series, "series")
	runtime.KeepAlive(r)
}

// BenchmarkProduceConcurrent is the collection side of spec 09 test 46:
// four or five readers at once, each holding its result until all have
// one, share one collection of the 125,000-series registry. MiB-heap is
// the live heap while the readers hold their results; it does not grow
// with the fifth reader.
func BenchmarkProduceConcurrent(b *testing.B) {
	for _, readers := range []int{4, 5} {
		b.Run(fmt.Sprintf("readers=%d", readers), func(b *testing.B) {
			r := bigRegistry(b)
			clk := r.clk.(interface{ Advance(time.Duration) })
			p := r.Producer()
			round := func(held func()) {
				clk.Advance(ReuseWindow)
				results := make([][]metricdata.ScopeMetrics, readers)
				var wg sync.WaitGroup
				for i := range readers {
					wg.Go(func() {
						if sm, err := p.Produce(context.Background()); err == nil {
							results[i] = sm
						}
					})
				}
				wg.Wait()
				held()
				for _, sm := range results {
					p.Release(sm)
				}
			}
			round(func() {}) // the first collection allocates the pooled structure
			b.ReportAllocs()
			for b.Loop() {
				round(func() {})
			}
			b.StopTimer()
			var ms runtime.MemStats
			round(func() {
				runtime.GC()
				runtime.ReadMemStats(&ms)
			})
			b.ReportMetric(float64(ms.HeapAlloc)/(1<<20), "MiB-heap")
			runtime.KeepAlive(r)
		})
	}
}

// BenchmarkAdmit5000Routes measures admission, binding and release of a
// 5,000-Route Revision (the activation budget PB-7).
func BenchmarkAdmit5000Routes(b *testing.B) {
	r, _ := newTestRegistry(b, nil)
	var s emit.Shape
	for i := range 5000 {
		s.Routes = append(s.Routes, fmt.Sprintf("route-%04d", i))
	}
	s.Policies = gatewayPolicies()
	b.ReportAllocs()
	for b.Loop() {
		pl, err := r.Admit(s)
		if err != nil {
			b.Fatal(err)
		}
		r.Bind(pl).Release()
	}
}

// dataPoints counts the data points of one aggregation.
func dataPoints(a metricdata.Aggregation) int {
	switch d := a.(type) {
	case metricdata.Sum[float64]:
		return len(d.DataPoints)
	case metricdata.Gauge[int64]:
		return len(d.DataPoints)
	case metricdata.Histogram[float64]:
		return len(d.DataPoints)
	default:
		return 0
	}
}
