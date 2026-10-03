// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/otlptranslator"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

// encodeReference is the reference encoding of produced metrics: one
// header per family and one line per data point in produced order, times
// relative to the fake clock's epoch. Runtime families are left out
// (process-dependent).
func encodeReference(sm []metricdata.ScopeMetrics) string {
	var b strings.Builder
	rel := func(t time.Time) string { return t.Sub(epoch()).String() }
	f := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	for _, s := range sm {
		fmt.Fprintf(&b, "scope %s %q\n", s.Scope.Name, s.Scope.Version)
		for _, m := range s.Metrics {
			if strings.HasPrefix(m.Name, "ruralz_runtime_") {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[float64]:
				fmt.Fprintf(&b, "# %s counter %s monotonic=%v cumulative=%v\n", m.Name, m.Unit, d.IsMonotonic, d.Temporality == metricdata.CumulativeTemporality)
				for _, dp := range d.DataPoints {
					fmt.Fprintf(&b, "%s %s start=%s\n", seriesKey(m.Name, dp.Attributes), f(dp.Value), rel(dp.StartTime))
				}
			case metricdata.Gauge[int64]:
				fmt.Fprintf(&b, "# %s gauge %s\n", m.Name, m.Unit)
				for _, dp := range d.DataPoints {
					fmt.Fprintf(&b, "%s %d start=%s\n", seriesKey(m.Name, dp.Attributes), dp.Value, rel(dp.StartTime))
				}
			case metricdata.Histogram[float64]:
				fmt.Fprintf(&b, "# %s histogram %s bounds=%v cumulative=%v\n", m.Name, m.Unit, d.DataPoints[0].Bounds, d.Temporality == metricdata.CumulativeTemporality)
				for _, dp := range d.DataPoints {
					fmt.Fprintf(&b, "%s count=%d sum=%s buckets=%v start=%s", seriesKey(m.Name, dp.Attributes), dp.Count, f(dp.Sum), dp.BucketCounts, rel(dp.StartTime))
					for _, ex := range dp.Exemplars {
						fmt.Fprintf(&b, " exemplar=%s@%s trace=%s span=%s", f(ex.Value), rel(ex.Time), hex.EncodeToString(ex.TraceID), hex.EncodeToString(ex.SpanID))
					}
					b.WriteByte('\n')
				}
			default:
				fmt.Fprintf(&b, "# %s unexpected %T\n", m.Name, m.Data)
			}
		}
	}
	return b.String()
}

// goldenRegistry records a fixed workload: two listeners, three Routes of
// which one folds by the retiring ceiling, two Upstreams, the five Gateway
// Policies of the configuration model worked example, and Node-wide
// families (spec 09 test 32's registry).
func goldenRegistry(t testing.TB) *Registry {
	r, clk := newTestRegistry(t, func(o *Options) {
		o.Limits = Limits{Retiring: 20}
		o.ScopeVersion = "v0.1.0-test"
	})
	shape := emit.Shape{
		Listeners: []string{"public", "internal"},
		Routes:    []string{"orders", "users", "legacy"},
		Upstreams: []string{"orders-svc", "users-svc"},
		Policies:  gatewayPolicies(),
	}
	p1, b1 := admitBind(t, r, shape)
	p1.Route("legacy").Requests.Inc(0, 200) // ends with the fold
	clk.Advance(time.Second)
	shape.Routes = []string{"orders", "users"}
	p2, _ := admitBind(t, r, shape)
	b1.Retire() // legacy retires, past the ceiling of 20 series: folds
	p1.Route("legacy").Requests.Inc(1, 404)

	pub := p2.Listener("public")
	pub.Requests.Inc(0, emit.ProtoHTTP1, 200, emit.OriginUpstream)
	pub.Requests.Inc(1, emit.ProtoHTTP2, 503, emit.OriginDependency)
	pub.GatewayDuration.Record(0, 120_000)
	pub.RequestBody.Record(0, 512)
	pub.ResponseBody.Record(2, 70_000)
	pub.Active.Add(0, 1)
	pub.Conns[emit.ProtoHTTP1][emit.ConnAccepted].Add(0, 3)
	pub.OpenConns[emit.ProtoHTTP1].Add(0, 2)
	pub.TLSHandshake.Record(0, 3_000_000)
	orders := p2.Route("orders")
	orders.Requests.Inc(0, 200)
	orders.Requests.Inc(1, 201)
	orders.Requests.Inc(2, 429)
	orders.Duration.Record(0, 1_000_000)
	orders.Duration.RecordExemplar(1, 2_500_000, emit.Exemplar{
		TraceID: [16]byte{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
		SpanID:  [8]byte{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7},
		Time:    epoch().Add(1500 * time.Millisecond),
	})
	orders.Cache[emit.CacheMiss].Add(0, 1)
	p2.Route(Unmatched).Requests.Inc(0, 404)
	up := p2.Upstream("orders-svc")
	up.Attempts.Inc(0, 200, emit.ErrNone)
	up.Attempts.Inc(0, 0, emit.ErrTimeout)
	up.AttemptDuration.Record(0, 900_000)
	up.Retries.Add(0, 1)
	up.BreakerState[emit.BreakerClosed].Set(1)
	up.HealthyEndpoints.Set(3)
	up.PoolConnections[emit.PoolIdle].Set(4)
	jwt := p2.Policy("jwt-default")
	jwt.Duration[phase.OnRequestHeaders].Record(0, 40_000)
	jwt.Auth.Allow(0)
	jwt.Auth.Deny(1, "RZ-AUTH-001")
	jwt.ShortCircuits[phase.OnRequestHeaders].Inc(1, 401)
	jwt.JWKSAge.Set(30)
	p2.Policy("ratelimit-default").RateLimit[emit.RateLimitAllow].Add(0, 2)
	p2.Policy("quota-default").Quota[emit.QuotaAllow].Add(0, 1)
	p2.Policy("headers-default").Duration[phase.OnResponse].Record(0, 9_000)

	n := r.Node()
	n.NodeResponses.Inc(0, "RZ-RT-001")
	n.State.Calls[emit.StateOpGCRA][emit.StateResultOK].Add(0, 1)
	n.State.CallDuration[emit.StateOpGCRA].Record(0, 700_000)
	n.Config.Activated.Add(0, 2)
	n.Config.ActivationDuration("compile", "le1000").Record(0, 250_000_000)
	n.Config.RevisionInfo("active", "rev-0123456789ab")
	deg, _ := r.Gauge(catalog.NodeDegradedInfo, "cleartext_hop")
	deg.Set(1)
	hop, _ := r.Gauge(catalog.SecurityCleartextHops, "client")
	hop.Set(1)
	clk.Advance(time.Second)
	return r
}

// Done when: the producer output equals a reference encoding (golden).
func TestProduceGolden(t *testing.T) {
	p := goldenRegistry(t).Producer()
	sm, err := p.Produce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := encodeReference(sm)
	p.Release(sm)
	path := filepath.Join("testdata", "produce.golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a fixed golden file under testdata.
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := range min(len(gl), len(wl)) {
			if gl[i] != wl[i] {
				t.Fatalf("golden mismatch at line %d:\ngot  %s\nwant %s", i+1, gl[i], wl[i])
			}
		}
		t.Fatalf("golden mismatch: %d lines, want %d", len(gl), len(wl))
	}
	// The same registry encodes identically on a second run.
	sm2, err := goldenRegistry(t).Producer().Produce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if encodeReference(sm2) != got {
		t.Error("two identical registries produced different output")
	}
}

// Spec 09 req 51: one collection in flight; readers within the reuse
// window share it; released structures are reused; a structure never
// released is abandoned after 30 s instead of reused.
func TestProduceReuseAndPooling_Req51(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	p := r.Producer()
	ctx := context.Background()
	produce := func() []metricdata.ScopeMetrics {
		t.Helper()
		sm, err := p.Produce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return sm
	}
	id := func(sm []metricdata.ScopeMetrics) *metricdata.Metrics { return &sm[0].Metrics[0] }

	gen := func() uint64 {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.gen
	}
	held := func() int {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.held)
	}

	a := produce()
	a2 := produce()
	if id(a) != id(a2) || gen() != 1 {
		t.Fatal("two readers within the reuse window got different collections")
	}
	clk.Advance(ReuseWindow - time.Nanosecond)
	if id(produce()) != id(a) || gen() != 1 {
		t.Fatal("collection not reused just inside the window")
	}
	p.Release(a)
	p.Release(a2)
	p.Release(a) // the third reader's
	clk.Advance(time.Nanosecond)
	r.Node().TapEventsDropped.Add(0, 1)
	b := produce()
	if gen() != 2 {
		t.Fatal("no new collection past the window")
	}
	if id(b) != id(a) {
		t.Error("a released structure was not refilled")
	}
	if got := mustGet(t, flatten(t, b), catalog.TapEventsDroppedTotal+"{}").value; got != 1 {
		t.Errorf("refilled collection is stale: %v", got)
	}
	// b is never released: it must not be refilled while held.
	clk.Advance(ReuseWindow)
	c := produce()
	if id(c) == id(b) {
		t.Fatal("a held structure was refilled")
	}
	if held() != 1 {
		t.Fatalf("held = %d, want 1", held())
	}
	p.Release(c)
	clk.Advance(ReuseWindow)
	d := produce() // c was released: refilled in place
	if id(d) != id(c) {
		t.Error("released structure not reused")
	}
	p.Release(d)
	clk.Advance(AbandonAfter)
	p.Release(produce())
	if held() != 0 {
		t.Errorf("held = %d after %v, want 0 (abandoned)", held(), AbandonAfter)
	}
	// Releasing a held structure frees it for the next collection.
	e := produce()
	clk.Advance(ReuseWindow)
	f := produce() // e held
	p.Release(f)
	p.Release(e)
	p.mu.Lock()
	free := len(p.free)
	p.mu.Unlock()
	if free == 0 {
		t.Error("a released held structure did not become free")
	}
	// Readers that never release fill at most keepHeld held slots.
	for range keepHeld + 3 {
		clk.Advance(ReuseWindow)
		produce()
	}
	if held() > keepHeld {
		t.Errorf("held = %d, cap %d", held(), keepHeld)
	}
	// Releasing other producers' scopes and nil is ignored.
	p.Release([]metricdata.ScopeMetrics{{}, {Metrics: []metricdata.Metrics{{Name: "x"}}}})
	p.ReleaseResource(nil)
	p.ReleaseResource(&metricdata.ResourceMetrics{ScopeMetrics: b})
}

// Spec 09 req 51: concurrent readers share one collection.
func TestProduceSingleFlight_Req51(t *testing.T) {
	r, _ := newTestRegistry(t, nil)
	p := r.Producer()
	var wg sync.WaitGroup
	ids := make([]*metricdata.Metrics, 16)
	for i := range ids {
		wg.Go(func() {
			sm, err := p.Produce(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = &sm[0].Metrics[0]
		})
	}
	wg.Wait()
	for _, x := range ids {
		if x != ids[0] {
			t.Fatal("readers at one instant got different collections")
		}
	}
	if p.gen != 1 {
		t.Errorf("collections = %d, want 1", p.gen)
	}
}

// A reader whose context ends while another collection runs gives up.
func TestProduceContextCanceled(t *testing.T) {
	r, _ := newTestRegistry(t, nil)
	p := r.Producer()
	p.sem <- struct{}{} // a collection in flight
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Produce(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	<-p.sem
}

// OQ-observability-16 (a): the producer plugs into an SDK reader.
func TestManualReaderProducer(t *testing.T) {
	r, _ := newTestRegistry(t, nil)
	reader := sdkmetric.NewManualReader(sdkmetric.WithProducer(r.Producer()))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	r.Node().TapEventsDropped.Add(0, 5)
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	pts := flatten(t, rm.ScopeMetrics)
	if got := mustGet(t, pts, catalog.TapEventsDroppedTotal+"{}").value; got != 5 {
		t.Errorf("tap events dropped = %v", got)
	}
	r.Producer().ReleaseResource(&rm)
	if rm.ScopeMetrics[0].Scope.Name != ScopeName {
		t.Errorf("scope = %q", rm.ScopeMetrics[0].Scope.Name)
	}
}

// Spec 09 req 39: /metrics family names equal the OTLP instrument names
// (no suffixes appended) when the Prometheus exporter reads the producer
// as WP-40 configures it; histograms carry exemplars (req 52).
func TestPrometheusNamesIdentical_Req39(t *testing.T) {
	p := goldenRegistry(t).Producer()
	reg := prometheus.NewRegistry()
	exp, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		otelprom.WithProducer(p),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithoutSuffixes),
		otelprom.WithoutScopeInfo(),
		otelprom.WithoutTargetInfo(),
	)
	if err != nil {
		t.Fatal(err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var promNames []string
	exemplars := 0
	for _, mf := range mfs {
		promNames = append(promNames, mf.GetName())
		for _, m := range mf.GetMetric() {
			for _, bk := range m.GetHistogram().GetBucket() {
				if bk.GetExemplar() != nil {
					exemplars++
				}
			}
		}
	}
	sm, err := p.Produce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var otlpNames []string
	for _, m := range sm[0].Metrics {
		otlpNames = append(otlpNames, m.Name)
	}
	p.Release(sm)
	slices.Sort(promNames)
	slices.Sort(otlpNames)
	if !slices.Equal(promNames, otlpNames) {
		t.Errorf("/metrics names %v\nOTLP names %v", promNames, otlpNames)
	}
	if exemplars != 1 {
		t.Errorf("exemplars on /metrics = %d, want 1", exemplars)
	}
}

// Code label sets start at the collection before their first record.
func TestCodeSeriesStartTime(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	collectNow(t, r, clk)
	before := clk.Now()
	clk.Advance(5 * time.Second)
	r.Node().NodeResponses.Inc(0, "RZ-RT-002")
	p := mustGet(t, collectNow(t, r, clk), catalog.HTTPNodeResponsesTotal+`{code="RZ-RT-002"}`)
	if !p.start.Equal(before) {
		t.Errorf("start = %v, want the previous collection %v", p.start, before)
	}
}

// Spec 09 req 51 and test 46: readers arriving within the reuse window
// share one collection, and a released structure is refilled in place,
// so a fifth concurrent reader adds no structure (and no heap) over four.
func TestProduceSharedByConcurrentReaders_Test46(t *testing.T) {
	distinct := map[int]int{}
	for _, readers := range []int{4, 5} {
		r, clk := newTestRegistry(t, nil)
		_, b := admitBind(t, r, emit.Shape{Listeners: []string{"public"}, Routes: []string{"a", "b"}, Policies: gatewayPolicies()})
		p := r.Producer()
		seen := map[*metricdata.Metrics]bool{}
		for round := range 5 {
			clk.Advance(ReuseWindow)
			results := make([][]metricdata.ScopeMetrics, readers)
			var wg sync.WaitGroup
			for i := range readers {
				wg.Go(func() {
					sm, err := p.Produce(context.Background())
					if err != nil {
						t.Errorf("Produce: %v", err)
						return
					}
					results[i] = sm
				})
			}
			wg.Wait()
			for i, sm := range results {
				if len(sm) == 0 || len(sm[0].Metrics) == 0 {
					t.Fatalf("readers=%d round %d: reader %d got no metrics", readers, round, i)
				}
				if &sm[0].Metrics[0] != &results[0][0].Metrics[0] {
					t.Errorf("readers=%d round %d: reader %d got its own collection", readers, round, i)
				}
				seen[&sm[0].Metrics[0]] = true
			}
			for _, sm := range results {
				p.Release(sm)
			}
		}
		b.Release()
		distinct[readers] = len(seen)
	}
	if distinct[4] != 1 || distinct[5] != distinct[4] {
		t.Errorf("distinct collection structures: %v, want 1 for 4 and 5 readers", distinct)
	}
}

// retiringOf reads ruralz_telemetry_series{state="retiring"} from a
// collection without the testing.T helpers, for use off the test
// goroutine.
func retiringOf(sm []metricdata.ScopeMetrics) (float64, bool) {
	for _, sc := range sm {
		for _, m := range sc.Metrics {
			if m.Name != catalog.TelemetrySeries {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				return 0, false
			}
			for _, dp := range g.DataPoints {
				if v, _ := dp.Attributes.Value("state"); v.AsString() == "retiring" {
					return float64(dp.Value), true
				}
			}
		}
	}
	return 0, false
}

// Recording, Hot Reloads and collection run concurrently without races
// (go test -race): several collectors advance the clock past the reuse
// window so every Produce collects while Admit, Bind, Retire, Release and
// ceiling folds run, with K = 2 retired plus one closing snapshot kept
// and requests recording through every pinned snapshot. Each collection
// keeps the exported retiring series within the ceiling (spec 09 req 56),
// and counts are exact once quiescent.
func TestConcurrentRecordReloadCollect(t *testing.T) {
	const ceiling, reloads = 60, 300
	r, clk := newTestRegistry(t, func(o *Options) { o.Limits = Limits{Retiring: ceiling} })
	pl, b := admitBind(t, r, emit.Shape{Routes: []string{"hot"}, Policies: gatewayPolicies()})
	defer b.Release()

	// current is the newest reloaded plan, for requests pinned to it.
	var current atomic.Pointer[Plan]
	stop := make(chan struct{})
	var wg sync.WaitGroup
	const workers, perWorker = 4, 5000
	for w := range workers {
		wg.Go(func() {
			m := pl.Route("hot")
			pm := pl.Policy("jwt-default")
			for i := range perWorker {
				s := stripe(w + i)
				m.Requests.Inc(s, 200)
				m.Duration.Record(s, uint64(i))
				pm.Auth.Allow(s)
				if i%100 == 0 {
					m.Duration.RecordExemplar(s, 1, emit.Exemplar{Time: epoch()})
					pm.Auth.Deny(s, "RZ-AUTH-001")
				}
				if p := current.Load(); p != nil {
					for k := range 5 {
						p.Route(fmt.Sprintf("r%d", k)).Requests.Inc(s, 500)
					}
				}
			}
		})
	}
	var advance sync.Mutex
	var collections, folds atomic.Int64
	for range 3 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				advance.Lock()
				clk.Advance(ReuseWindow)
				advance.Unlock()
				sm, err := r.Producer().Produce(context.Background())
				if err != nil {
					t.Errorf("Produce: %v", err)
					return
				}
				if ret, ok := retiringOf(sm); !ok || ret > ceiling {
					t.Errorf("exported retiring %v (found %v) over the ceiling %d", ret, ok, ceiling)
				}
				r.Producer().Release(sm)
				collections.Add(1)
			}
		})
	}
	wg.Go(func() {
		defer close(stop)
		var pinned []emit.Binding
		for i := range reloads {
			// Two of the five reload Routes are renamed each time.
			p, err := r.Admit(emit.Shape{
				Routes:   []string{"hot", fmt.Sprintf("r%d", i%5), fmt.Sprintf("r%d", (i+1)%5), fmt.Sprintf("x%d", i)},
				Policies: gatewayPolicies(),
			})
			if err != nil {
				t.Errorf("Admit: %v", err)
				return
			}
			bb := r.Bind(p)
			current.Store(p.(*Plan))
			p.(*Plan).Route("hot").Requests.Inc(0, 500)
			if n := len(pinned); n > 0 {
				pinned[n-1].Retire()
			}
			r.mu.Lock()
			if r.byName[catalog.HTTPRequestsTotal].ceilingFolded > 0 {
				folds.Add(1)
			}
			r.mu.Unlock()
			pinned = append(pinned, bb)
			if len(pinned) > 3 {
				pinned[0].Release()
				pinned = pinned[1:]
			}
		}
		for _, bb := range pinned {
			bb.Retire()
			bb.Release()
		}
	})
	wg.Wait()
	if n := collections.Load(); n < 3 {
		t.Errorf("only %d collections ran during the reloads", n)
	}
	if folds.Load() == 0 {
		t.Error("the retiring ceiling never folded during the reloads")
	}
	t.Logf("%d collections, %d reloads with ceiling folds", collections.Load(), folds.Load())
	pts := collectNow(t, r, clk)
	if got := mustGet(t, pts, reqKey("hot", "2xx")).value; got != workers*perWorker {
		t.Errorf("hot 2xx = %v, want %d", got, workers*perWorker)
	}
	if got := mustGet(t, pts, catalog.AuthDecisionsTotal+`{code="RZ-AUTH-001",policy="jwt-default",result="deny"}`).value; got != workers*perWorker/100 {
		t.Errorf("denies = %v", got)
	}
	if got := mustGet(t, pts, reqKey("hot", "5xx")).value; got != reloads {
		t.Errorf("hot 5xx = %v, want %d", got, reloads)
	}
	if _, ret := seriesState(t, pts); ret != 0 {
		t.Errorf("retiring after every reload snapshot was released = %v", ret)
	}
	checkRetiringQueue(t, r)
}
