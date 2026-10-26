// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"bufio"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// fixedShape is the registry of the name identity gate: two listeners,
// three Routes (one cached), two Upstreams and Policies at each scope.
func fixedShape() emit.Shape {
	return emit.Shape{
		Listeners: []string{"http", "https"},
		Routes:    []string{"orders", "stock", "users"},
		Upstreams: []string{"orders-v1", "stock-v1"},
		Policies: []emit.PolicyShape{
			{Name: "cors-default", Type: "cors", Scopes: phase.Scopes(phase.ScopeGateway), Phases: phase.Of(phase.OnRequestHeaders, phase.OnResponse), Gateway: true},
			{Name: "jwt-default", Type: "auth.jwt", Scopes: phase.Scopes(phase.ScopeGateway), Phases: phase.Of(phase.OnRequestHeaders), Codes: []string{"RZ-AUTH-001"}, Gateway: true},
			{Name: "per-tenant", Type: "ratelimit", Scopes: phase.Scopes(phase.ScopeRoute), Phases: phase.Of(phase.OnRequestHeaders)},
			{Name: "orders-cache", Type: "cache", Scopes: phase.Scopes(phase.ScopeRoute), Phases: phase.Of(phase.OnRequestHeaders, phase.OnResponse)},
		},
		CachedRoutes: []string{"orders"},
	}
}

// familyNames returns the family names of a text exposition: the names of
// its # TYPE lines. In OpenMetrics a counter family drops _total, which its
// samples carry, so counters get it back to compare with OTLP names.
func familyNames(t *testing.T, body string, openMetrics bool) []string {
	t.Helper()
	var names []string
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 4 || f[0] != "#" || f[1] != "TYPE" {
			continue
		}
		name := f[2]
		if openMetrics && f[3] == "counter" && !strings.HasSuffix(name, "_total") {
			name += "_total"
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// TestMetricNamesMatchAcrossExports is the name identity gate of ADR-0018:
// one fixed registry exported through /metrics as text and as OpenMetrics
// and through OTLP into the test collector yields one name set, pinned in
// testdata/metric_names.golden (-update rewrites it).
func TestMetricNamesMatchAcrossExports(t *testing.T) {
	sink := startSink(t, otlpsink.Config{})
	r, _ := newRuntime(t, nil)
	plan, err := r.Meter().Admit(fixedShape())
	if err != nil {
		t.Fatal(err)
	}
	r.Meter().Bind(plan)
	plan.Route("orders").Requests.Inc(0, 200)
	plan.Upstream("orders-v1").AttemptDuration.Record(0, 2_000_000)
	plan.Listener("http").Requests.Inc(0, emit.ProtoHTTP1, 200, emit.OriginUpstream)
	r.Meter().Node().NodeResponses.Inc(0, "RZ-RT-006")
	applyEndpoint(t, r, cleartext(sink), nil)

	scrapeAfter(t, r)
	text, _ := scrapeText(t, r, "")
	om, _ := scrapeText(t, r, openMetrics)
	if err := r.pipe.mp.ForceFlush(waitCtx(t)); err != nil {
		t.Fatalf("metric flush: %v", err)
	}
	otlp := sink.MetricNames()
	slices.Sort(otlp)
	otlp = slices.Compact(otlp)

	textNames, omNames := familyNames(t, text, false), familyNames(t, om, true)
	if !slices.Equal(textNames, otlp) {
		t.Errorf("text and OTLP names differ:\nonly text: %v\nonly OTLP: %v", diff(textNames, otlp), diff(otlp, textNames))
	}
	if !slices.Equal(omNames, otlp) {
		t.Errorf("OpenMetrics and OTLP names differ:\nonly OpenMetrics: %v\nonly OTLP: %v", diff(omNames, otlp), diff(otlp, omNames))
	}
	got := strings.Join(otlp, "\n") + "\n"
	path := filepath.Join("testdata", "metric_names.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // a constant path under testdata
	if err != nil {
		t.Fatalf("read golden (run with -update): %v", err)
	}
	if got != string(want) {
		t.Errorf("metric names differ from %s (run with -update after review):\n%s", path, got)
	}
}

// diff returns the elements of a missing from b.
func diff(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}
