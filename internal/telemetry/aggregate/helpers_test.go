// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Compile-time checks of the emit contracts this package implements.
var (
	_ emit.Meter            = (*Registry)(nil)
	_ emit.Plan             = (*Plan)(nil)
	_ emit.Binding          = (*Binding)(nil)
	_ emit.Counter          = (*counterH)(nil)
	_ emit.Gauge            = (*gaugeH)(nil)
	_ emit.Histogram        = (*histH)(nil)
	_ emit.StatusCounter    = (*statusH)(nil)
	_ emit.CodeCounter      = (*codeH)(nil)
	_ emit.AuthDecisions    = (*authH)(nil)
	_ emit.UpstreamAttempts = (*attemptsH)(nil)
	_ emit.ListenerRequests = (*listenerReqH)(nil)
)

// epoch is the fake clock's start.
func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// registeredCodes returns every registered RZ code.
func registeredCodes() []string {
	var out []string
	for _, c := range errcode.All() {
		out = append(out, c.ID)
	}
	return out
}

// newTestRegistry returns a registry on a fake clock with four stripes and
// the registered codes; mod adjusts the options.
func newTestRegistry(t testing.TB, mod func(*Options)) (*Registry, *clocktest.Fake) {
	t.Helper()
	clk := clocktest.New(epoch())
	o := Options{Clock: clk, Stripes: 4, Codes: registeredCodes()}
	if mod != nil {
		mod(&o)
	}
	r, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, clk
}

// point is one flattened data point.
type point struct {
	kind      string // "sum", "gauge" or "hist"
	value     float64
	count     uint64
	sum       float64
	buckets   []uint64
	start     time.Time
	exemplars []metricdata.Exemplar[float64]
}

// seriesKey renders name{k="v",...} with keys in attribute.Set order.
func seriesKey(name string, attrs attribute.Set) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, kv := range attrs.ToSlice() {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", kv.Key, kv.Value.AsString())
	}
	b.WriteByte('}')
	return b.String()
}

// flatten indexes every data point by series key.
func flatten(t testing.TB, sm []metricdata.ScopeMetrics) map[string]point {
	t.Helper()
	out := make(map[string]point)
	for _, s := range sm {
		for _, m := range s.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					out[seriesKey(m.Name, dp.Attributes)] = point{kind: "sum", value: dp.Value, start: dp.StartTime}
				}
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					out[seriesKey(m.Name, dp.Attributes)] = point{kind: "gauge", value: float64(dp.Value), start: dp.StartTime}
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					out[seriesKey(m.Name, dp.Attributes)] = point{
						kind: "hist", count: dp.Count, sum: dp.Sum, start: dp.StartTime,
						buckets: slices.Clone(dp.BucketCounts), exemplars: slices.Clone(dp.Exemplars),
					}
				}
			default:
				t.Fatalf("%s: unexpected aggregation %T", m.Name, m.Data)
			}
		}
	}
	return out
}

// collectNow produces a fresh collection (advancing past the reuse
// window) and flattens it.
func collectNow(t testing.TB, r *Registry, clk *clocktest.Fake) map[string]point {
	t.Helper()
	clk.Advance(ReuseWindow)
	sm, err := r.Producer().Produce(context.Background())
	if err != nil {
		t.Fatalf("Produce: %v", err)
	}
	out := flatten(t, sm)
	r.Producer().Release(sm)
	return out
}

// familySeries returns the flattened points of one family.
func familySeries(points map[string]point, name string) map[string]point {
	out := make(map[string]point)
	for k, v := range points {
		if strings.HasPrefix(k, name+"{") {
			out[k] = v
		}
	}
	return out
}

// mustGet returns the point of key or fails.
func mustGet(t testing.TB, points map[string]point, key string) point {
	t.Helper()
	p, ok := points[key]
	if !ok {
		var near []string
		name, _, _ := strings.Cut(key, "{")
		for k := range familySeries(points, name) {
			near = append(near, k)
		}
		slices.Sort(near)
		t.Fatalf("no series %s; family has %d: %v", key, len(near), near)
	}
	return p
}

// admitBind admits and binds s.
func admitBind(t testing.TB, r *Registry, s emit.Shape) (*Plan, emit.Binding) {
	t.Helper()
	pl, err := r.Admit(s)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	return pl.(*Plan), r.Bind(pl)
}

// phases returns the Phase set of ps.
func phases(ps ...phase.Phase) phase.Set { return phase.Of(ps...) }

// gatewayPolicies are the five Gateway Policies of the configuration
// model worked example (spec 09 test 32).
func gatewayPolicies() []emit.PolicyShape {
	req := phases(phase.OnRequestHeaders)
	return []emit.PolicyShape{
		{Name: "cors-default", Type: "cors", Phases: phases(phase.OnRequestHeaders, phase.OnResponse), Gateway: true},
		{Name: "jwt-default", Type: "auth.jwt", Phases: req, Codes: []string{"RZ-AUTH-001", "RZ-AUTH-003"}, Gateway: true},
		{Name: "ratelimit-default", Type: "ratelimit", Phases: req, Gateway: true},
		{Name: "quota-default", Type: "quota", Phases: phases(phase.OnRequestHeaders, phase.OnLog), Gateway: true},
		{Name: "headers-default", Type: "headers", Phases: phases(phase.OnResponse), Gateway: true},
	}
}

// stripe converts a test index to a stripe; stripe values wrap at 256 by
// design (emit.Stripe is a uint8 offset per request).
func stripe(i int) emit.Stripe { return emit.Stripe(i) } //nolint:gosec // G115: stripes wrap by design.

// testRand returns a deterministic generator for property tests.
func testRand(a, b uint64) *rand.Rand { return rand.New(rand.NewPCG(a, b)) } //nolint:gosec // G404: reproducible test data, not security.

// approx reports whether a and b are equal within a relative 1e-12.
func approx(a, b float64) bool {
	return math.Abs(a-b) <= 1e-12*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}
