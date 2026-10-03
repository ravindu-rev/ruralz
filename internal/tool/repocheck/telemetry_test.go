// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// TestValidateCatalog covers 09 req 77 (d), catalog.Validate() of spec 09
// section 3: the linked catalog is clean and each rule has a failing case.
func TestValidateCatalog(t *testing.T) {
	if got := validateCatalog(linkedCatalog()); len(got) != 0 {
		t.Fatalf("linked catalog: %q", got)
	}
	good := catalog.Family{Name: "ruralz_fixture_requests_total", Kind: catalog.Counter, Unit: "{requests}"}
	for _, tc := range []struct {
		name   string
		mutate func(*catalogView)
		want   string
	}{
		{"pack grammar", func(c *catalogView) { c.families[0].Name = "ruralz_Requests_total" }, "does not match the pack grammar"},
		{"no component", func(c *catalogView) { c.families[0].Name = "ruralz_total" }, "does not match the pack grammar"},
		{"counter suffix", func(c *catalogView) { c.families[0].Name = "ruralz_x_count" }, "counter ruralz_x_count must end in _total"},
		{"gauge suffix", func(c *catalogView) { c.families[0].Kind = catalog.Gauge }, "must not end in _total"},
		{"histogram suffix", func(c *catalogView) {
			c.families[0].Kind, c.families[0].Bounds = catalog.Histogram, catalog.BoundsFast
		}, "must end in _seconds, _bytes or _ratio"},
		{"histogram without bounds", func(c *catalogView) {
			c.families[0] = catalog.Family{Name: "ruralz_x_seconds", Kind: catalog.Histogram, Unit: "s"}
		}, "name a bound set"},
		{"counter with bounds", func(c *catalogView) { c.families[0].Bounds = catalog.BoundsBytes }, "name a bound set"},
		{"no kind", func(c *catalogView) { c.families[0].Kind = 0 }, "has no instrument type"},
		{"duplicate", func(c *catalogView) { c.families = append(c.families, good) }, "is defined twice"},
		{"unit form", func(c *catalogView) { c.families[0].Unit = "requests" }, "is not s, By, 1"},
		{"unit 1", func(c *catalogView) { c.families[0].Unit = "1" }, "must end in _info or _ratio"},
		{"seconds unit", func(c *catalogView) {
			c.families[0] = catalog.Family{Name: "ruralz_x_age_seconds", Kind: catalog.Gauge, Unit: "{s}"}
		}, "must have unit s"},
		{"bytes unit", func(c *catalogView) {
			c.families[0] = catalog.Family{Name: "ruralz_x_heap_bytes", Kind: catalog.Gauge, Unit: "s"}
		}, "must have unit By"},
		{"info kind", func(c *catalogView) {
			c.families[0] = catalog.Family{Name: "ruralz_x_state_info", Kind: catalog.Counter, Unit: "1"}
		}, "must be a gauge with unit 1"},
		{"label case", func(c *catalogView) { c.families[0].Labels = []catalog.Label{{Name: "statusClass"}} }, `label "statusClass"`},
		{"label repeated", func(c *catalogView) {
			c.families[0].Labels = []catalog.Label{{Name: "route"}, {Name: "route"}}
		}, `label "route"`},
		{"reason repeated", func(c *catalogView) { c.reasons = []string{"lkg_boot", "lkg_boot"} }, `degraded reason "lkg_boot"`},
		{"hop case", func(c *catalogView) { c.hops = []string{"State-Store"} }, `hop "State-Store"`},
		{"route span", func(c *catalogView) { c.spanRouteMatch = "ruralz.route" }, "route match span"},
		{"filter prefix", func(c *catalogView) { c.spanFilterPrefix = "ruralz.policy." }, "Filter spans"},
		{"filter builder", func(c *catalogView) { c.filterSpanSample = "sample" }, "Filter spans"},
		{"upstream prefix", func(c *catalogView) { c.upstreamSpanSample = "ruralz.upstreams.sample" }, "Upstream spans"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := linkedCatalog()
			c.families = []catalog.Family{good}
			tc.mutate(&c)
			got := validateCatalog(c)
			if !slices.ContainsFunc(got, func(g string) bool { return strings.Contains(g, tc.want) }) {
				t.Errorf("validateCatalog = %q, want a problem containing %q", got, tc.want)
			}
		})
	}
}

// TestCheckTelemetryNames covers 09 req 77 (a) to (c) and their
// exemptions.
func TestCheckTelemetryNames(t *testing.T) {
	src := `package p

const (
	metric   = "ruralz_http_requests_total"
	sentence = "see ruralz_http_requests_total"
	attr     = "ruralz.route"
	route    = "ruralz.route.match"
	up       = ` + "`ruralz.upstream.orders`" + `
	char     = 'r'
	number   = 7
)

func f() {
	tr.Start(ctx, "custom")
	tr.Start(ctx, name)
	tr.Start(ctx)
	Start(ctx, "not a method")
	x.Stop(ctx, "other")
}
`
	for _, tc := range []struct {
		rel  string
		want []string
	}{
		{"internal/telemetry/tracing/span.go", []string{
			`p.go-like:4: metric name "ruralz_http_requests_total"`,
			`:7: span name "ruralz.route.match"`,
			`:8: span name "ruralz.upstream.orders"`,
			`:14: span started with the literal name "custom"`,
		}},
		{"internal/telemetry/tracing/span_test.go", nil},
		{"internal/telemetry/catalog/extra.go", nil},
	} {
		t.Run(tc.rel, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, tc.rel, src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			got := checkTelemetryNames(fset, tc.rel, f)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d findings, want %d: %v", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				w = strings.Replace(w, "p.go-like", tc.rel, 1)
				if !strings.Contains(got[i].String(), strings.TrimPrefix(w, ":")) {
					t.Errorf("finding %d = %q, want %q", i, got[i], w)
				}
			}
		})
	}
}

func TestSplitRowAndTables(t *testing.T) {
	cells := splitRow("| `a` | b \\| c | `x`, `y` |")
	if want := []string{"`a`", "b | c", "`x`, `y`"}; !slices.Equal(cells, want) {
		t.Errorf("splitRow = %q, want %q", cells, want)
	}
	lines := strings.Split(`intro
| Metric | Type |
|---|:---:|
| one | two |

### Ruralz Gateway metrics
`+"```markdown"+`
## Fenced heading
| Metric | Type |
|---|---|
| fenced | ignored |
`+"```"+`
#hashtag is not a heading
| A | B |
| c | d |`, "\n")
	tables := parseTables(lines)
	if len(tables) != 2 {
		t.Fatalf("parseTables found %d tables, want 2: %+v", len(tables), tables)
	}
	if tables[0].column("Type") != 1 || len(tables[0].rows) != 1 || tables[0].rows[0].line != 4 || tables[0].heading != "" {
		t.Errorf("first table = %+v", tables[0])
	}
	if tables[1].header[0] != "A" || tables[1].rows[0].cells[1] != "d" || tables[1].heading != gatewayMetricsHeading {
		t.Errorf("second table = %+v", tables[1])
	}
	if isSeparator([]string{"a"}) || isSeparator([]string{" "}) || !isSeparator([]string{"---", ":-:"}) {
		t.Error("isSeparator")
	}
}

// TestPick covers the per-name cells of multi-name OBS rows (09 req 74).
func TestPick(t *testing.T) {
	two := []string{"ruralz_a_total", "ruralz_b_total"}
	runtime := []string{"ruralz_runtime_goroutines", "ruralz_runtime_heap_bytes", "ruralz_runtime_gc_cycles_total"}
	for _, tc := range []struct {
		cell   string
		names  []string
		index  int
		seps   []string
		want   string
		wantOK bool
	}{
		{"Histogram, bytes", two, 1, []string{";"}, "Histogram, bytes", true},
		{"operations, writes", two, 1, []string{";", ","}, "writes", true},
		{"Gauge; Gauge; Counter", runtime, 2, []string{";"}, "Counter", true},
		{"goroutines, bytes, cycles", runtime, 0, []string{";", ","}, "goroutines", true},
		{"spans", two, 1, []string{";", ","}, "spans", true},
		{" seconds ", []string{"ruralz_a_seconds"}, 0, []string{","}, "seconds", true},
		{"a; b", []string{"ruralz_a_seconds"}, 0, []string{";"}, "a; b", true},
		// A value for every name with exceptions.
		{"Gauge; Counter for cycles", runtime, 0, []string{";"}, "Gauge", true},
		{"Gauge; Counter for cycles", runtime, 1, []string{";"}, "Gauge", true},
		{"Gauge; Counter for cycles", runtime, 2, []string{";"}, "Counter", true},
		// Ambiguous: neither one part per name nor exceptions that name one.
		{"Gauge; Counter", runtime, 2, []string{";"}, "", false},
		{"Gauge; Counter for nothing", runtime, 0, []string{";"}, "", false},
		{"goroutines, bytes", runtime, 0, []string{";", ","}, "", false},
	} {
		got, ok := pick(tc.cell, tc.names, tc.index, tc.seps...)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("pick(%q, %d of %d) = %q, %v, want %q, %v", tc.cell, tc.index, len(tc.names), got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestDocUnitAndWords(t *testing.T) {
	for _, tc := range []struct {
		f    catalog.Family
		want string
	}{
		{catalog.Family{Name: "ruralz_a_seconds", Unit: "s"}, "seconds"},
		{catalog.Family{Name: "ruralz_a_bytes", Unit: "By"}, "bytes"},
		{catalog.Family{Name: "ruralz_a_info", Unit: "1"}, "info"},
		{catalog.Family{Name: "ruralz_a_error_ratio", Unit: "1"}, "ratio"},
		{catalog.Family{Name: "ruralz_a_label_sets", Unit: "{label_sets}"}, "label sets"},
	} {
		if got := docUnit(tc.f); got != tc.want {
			t.Errorf("docUnit(%s) = %q, want %q", tc.f.Unit, got, tc.want)
		}
	}
	if kindWord(0) != "unknown" || boundsWord(catalog.BoundsNone) != "none" || boundsWord(catalog.BoundsRatio) != "ratio" ||
		boundsWord(catalog.BoundsControl) != "control" || boundsWord(catalog.BoundsBytes) != "bytes" {
		t.Error("kindWord or boundsWord")
	}
}

// TestDocGate covers 09 req 75 token and reason recognition.
func TestDocGate(t *testing.T) {
	obs := obsCatalog{
		metrics: map[string]obsMetric{
			"ruralz_http_requests_total":           {},
			"ruralz_http_gateway_duration_seconds": {histogram: true},
			"ruralz_ai_tokens_total":               {},
			"ruralz_node_degraded_info":            {},
			"ruralz_state_ops_total":               {},
		},
		reasons: map[string]obsReason{"lkg_boot": {}, "jwks_stale": {}, "cleartext_hop": {}},
	}
	for _, tc := range []struct {
		line string
		want []string
	}{
		{"`ruralz_http_requests_total` counts requests.", nil},
		{"`ruralz_http_gateway_duration_seconds_bucket` and `_count`.", nil},
		{"`ruralz_ai_*` metrics and ruralz_ai metrics.", nil},
		{"the `ruralz_v1.lua` library", nil},
		{"xruralz_bogus_total is part of another word", nil},
		{"`ruralz_bogus_total`.", []string{"ruralz_bogus_total is not in the metrics catalog"}},
		{"`ruralz_state_ops_total_count`", []string{"ruralz_state_ops_total_count is not"}},
		{"`ruralz_plugin_*` metrics", []string{"ruralz_plugin_ is not"}},
		{"`ruralz_node_degraded_info{reason=\"lkg_boot\"}`", nil},
		{"ruralz_node_degraded_info{reason=~\"lkg_boot\\|detached\"} == 1", []string{"degraded reason detached"}},
		{"ruralz_node_degraded_info{reason!~\"state_.*\"}", nil},
		{"raises the degraded reason `jwks_stale`.", nil},
		{"degraded reasons `lkg_boot`, `jwks_stale` and `detached`", []string{"degraded reason detached"}},
		{"a degraded state `plugin_memlimit`", []string{"degraded reason plugin_memlimit"}},
		{"`ruralz_node_degraded_info` (reason `discovery_stale`)", []string{"degraded reason discovery_stale"}},
	} {
		got := docGate("docs/architecture/x.md", []string{tc.line}, obs)
		if len(got) != len(tc.want) {
			t.Errorf("%q: got %v, want %d finding(s)", tc.line, got, len(tc.want))
			continue
		}
		for i, w := range tc.want {
			if !strings.Contains(got[i].msg, w) {
				t.Errorf("%q: finding %q, want %q", tc.line, got[i].msg, w)
			}
		}
	}
}

// TestDocGateTempDoc is spec 09 test 2's negative fixture against the real
// Observability document: a new architecture document naming
// ruralz_bogus_total fails the doc gate, and nothing else does.
func TestDocGateTempDoc(t *testing.T) {
	obs, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(obsDoc)))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, filepath.FromSlash(archDir))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"10-observability.md": obs,
		"99-temp.md":          []byte("# Temp\n\nIt exports `ruralz_bogus_total`.\n"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := loadRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := checkObservabilityDoc(r, linkedCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].String() != "docs/architecture/99-temp.md:3: ruralz_bogus_total is not in the metrics catalog of "+obsDoc {
		t.Errorf("findings = %v", got)
	}
}

func TestObservabilityDocStructure(t *testing.T) {
	root := t.TempDir()
	r := &repo{root: root}
	got, err := checkObservabilityDoc(r, linkedCatalog())
	if err != nil || len(got) != 1 || !strings.Contains(got[0].msg, "missing") {
		t.Errorf("no OBS: %v, %v", got, err)
	}
	_, findings := parseObsCatalog(strings.Split(`| Metric | Kind |
|---|---|
| `+"`ruralz_a_total`"+` | Counter |

| Metric | Type | Unit | Labels |
|---|---|---|---|
| `+"`ruralz_b_total`"+` | Counter | b |
| `+"`ruralz_c_total`"+` | Counter | c | none |
| `+"`ruralz_c_total`"+` | Counter | c | none |`, "\n"))
	for _, w := range []string{"lacks a Type, Unit or Labels column", "row has 3 cells", "listed twice", "no degraded states table"} {
		if !slices.ContainsFunc(findings, func(f finding) bool { return strings.Contains(f.msg, w) }) {
			t.Errorf("missing %q in %v", w, findings)
		}
	}
	_, findings = parseObsCatalog([]string{"no tables"})
	if len(findings) != 2 {
		t.Errorf("empty document: %v", findings)
	}
}
