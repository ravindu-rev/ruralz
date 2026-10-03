// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// committedDir is deploy/grafana relative to this package.
func committedDir() string { return filepath.Join("..", "..", "..", "deploy", "grafana") }

// TestGeneratedUpToDate is the stage 3 drift check (spec 09 test 20): a
// fresh in-memory run equals the committed files byte for byte, and no
// committed file carries the generated-file marker without being generated
// (a renamed or dropped dashboard left behind).
func TestGeneratedUpToDate(t *testing.T) {
	files, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		got, err := os.ReadFile(filepath.Join(committedDir(), filepath.FromSlash(f.path)))
		if err != nil {
			t.Errorf("%s: %v (run go generate ./internal/tool/telemetrygen)", f.path, err)
			continue
		}
		if !bytes.Equal(got, f.data) {
			t.Errorf("deploy/grafana/%s is stale: run go generate ./internal/tool/telemetrygen", f.path)
		}
		if !bytes.Contains(bytes.ToLower(f.data), []byte(generatedMarker)) {
			t.Errorf("%s lacks the generated-file marker %q", f.path, generatedMarker)
		}
	}
	stale, err := staleGenerated(committedDir(), files)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range stale {
		t.Errorf("deploy/grafana/%s carries the telemetrygen marker but is no longer generated: run go generate ./internal/tool/telemetrygen", rel)
	}
}

// TestCommittedDashboards applies the dashboard check to every committed
// dashboard, including hand-written ones (09 req 73).
func TestCommittedDashboards(t *testing.T) {
	problems, err := checkDashboardDir(filepath.Join(committedDir(), "dashboards"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// TestDashboardFixtureFails: a panel on a metric outside the catalog fails
// the check (spec 09 test 20), and so do a non-Ruralz metric in a panel or
// an annotation, a bare histogram family name, an unknown label, an
// enumeration value or le bound the family does not export, and variables
// over an unknown metric or a histogram family name (09 req 73). Loki and
// test data queries, custom, interval, textbox, constant and ad hoc
// variables and PromQL comments raise nothing.
func TestDashboardFixtureFails(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "bad-dashboard.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := checkDashboardJSON("bad-dashboard.json", data)
	want := []string{
		"$route: ruralz_route_requests_total is not a metric of the telemetry catalog",
		"$upstream: ruralz_upstream_attempt_duration_seconds is a histogram with no series of that name",
		"annotation Restarts: process_start_time_seconds is not a metric of the telemetry catalog",
		"Unknown family > A: ruralz_unknown_total is not a metric of the telemetry catalog",
		"Not a Ruralz metric > A: up is not a metric of the telemetry catalog",
		"Histogram family name > A: ruralz_http_request_duration_seconds is a histogram with no series of that name: select ruralz_http_request_duration_seconds_bucket",
		`Row > Wrong label > A: ruralz_http_requests_total has no label "listener"`,
		`Row > Wrong reason > A: ruralz_node_degraded_info{reason="bogus_reason"}: "bogus_reason" is not a value of reason`,
		`Row > Bound that is not exported > A: ruralz_http_gateway_duration_seconds_bucket{le=~"0.001|9"}: "9" is not a value of le`,
	}
	for _, w := range want {
		if !slices.ContainsFunc(got, func(g string) bool { return strings.Contains(g, w) }) {
			t.Errorf("missing problem %q in:\n%s", w, strings.Join(got, "\n"))
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d problems, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
}

func TestCheckDashboardJSONMalformed(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{"not json", `{`, "not JSON"},
		{"not an object", `[]`, "not a dashboard object"},
		{"no title", `{"panels": []}`, "no title"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := checkDashboardJSON("d.json", []byte(tc.data))
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Errorf("got %q, want one problem containing %q", got, tc.want)
			}
		})
	}
}

// TestCheckExpr covers the expression check behind 09 req 73 and the
// alert rule check of 09 req 72.
func TestCheckExpr(t *testing.T) {
	for _, tc := range []struct {
		name, expr string
		want       []string // substrings, one per expected problem
	}{
		{"counter", `sum by (route) (rate(ruralz_http_requests_total{route=~"$route",status_class="5xx"}[5m]))`, nil},
		{"histogram bucket", `histogram_quantile(0.99, sum by (le) (rate(ruralz_http_gateway_duration_seconds_bucket{le="0.001"}[$__rate_interval])))`, nil},
		{"histogram sum and count", `ruralz_filter_duration_seconds_sum / ruralz_filter_duration_seconds_count`, nil},
		{"integer bound in OpenMetrics form", `ruralz_config_activation_duration_seconds_bucket{le="2.0"}`, nil},
		{"small bound in g form", `ruralz_http_gateway_duration_seconds_bucket{le="1e-05"}`, nil},
		{"plus inf bound", `ruralz_http_gateway_duration_seconds_bucket{le="+Inf"}`, nil},
		{"target labels", `ruralz_runtime_goroutines{instance="n1",job="ruralz"}`, nil},
		{"identifier containing ruralz", `x_is_not_checked_here_but_ruralz_is > 0`, []string{"x_is_not_checked_here_but_ruralz_is is not a metric"}},
		{"unless offset clause", `delta(ruralz_telemetry_folded_label_sets[15m]) > 0 or (ruralz_telemetry_folded_label_sets > 0 unless ruralz_telemetry_folded_label_sets offset 15m)`, nil},
		{"aggregation keywords", `topk(5, sum without (instance) (ruralz_state_ops_total)) and on (kind) group_left count(ruralz_state_ops_total) by (kind)`, nil},
		{"plain alternation", `ruralz_security_cleartext_hops{hop=~"state_store|telemetry|admin"}`, nil},
		{"regex matching a value", `ruralz_security_cleartext_hops{hop=~"state_.*"}`, nil},
		{"regex with an alternation in a group", `ruralz_security_cleartext_hops{hop=~"(state_store|admin)|tele.+"}`, nil},
		{"regex with a class", `ruralz_security_cleartext_hops{hop=~"[a-z|]+_store"}`, nil},
		{"empty alternative and value select a missing label", `ruralz_node_degraded_info{reason=~"|lkg_boot"} + ruralz_node_degraded_info{reason!=""}`, nil},
		{"le alternation with dots", `ruralz_http_gateway_duration_seconds_bucket{le=~"0.001|0.00015"}`, nil},
		{"le alternation with escapes", `ruralz_http_gateway_duration_seconds_bucket{le=~"0\\.001|\\+Inf"}`, nil},
		{"comments", "# gateway time\nsum(ruralz_http_active_requests) # by (nothing_total)\n# ruralz_unknown_total", nil},
		{"comment inside a label list", "sum by (route # one ( per Route\n) (ruralz_http_requests_total)", nil},
		{"code label is not enumerated", `ruralz_http_node_responses_total{code="RZ-RT-005"}`, nil},
		{"name matcher", `{__name__="ruralz_tap_events_dropped_total"}`, nil},
		{"single-quoted and backquoted", "ruralz_state_calls_total{op='gcra',result=`ok`}", nil},
		{"braced variable", `rate(ruralz_state_calls_total{op="${op}"}[${__range}])`, nil},
		{"numbers", `ruralz_telemetry_series > 1e-05 * 0x10 + .5`, nil},
		{"grafana label_values", `label_values(ruralz_http_requests_total, route)`, nil},
		{"grafana label_values without metric", `label_values(route)`, nil},
		{"unknown metric", `rate(ruralz_unknown_total[5m])`, []string{"ruralz_unknown_total is not a metric"}},
		{"foreign metric", `up == 0`, []string{"up is not a metric"}},
		{"suffix on a counter", `ruralz_http_requests_total_count`, []string{"ruralz_http_requests_total_count is not a metric"}},
		{"unknown label", `ruralz_http_requests_total{listener="a"}`, []string{`has no label "listener"`}},
		{"le on a counter", `ruralz_http_requests_total{le="1"}`, []string{`has no label "le"`}},
		{"le on the count series", `ruralz_http_gateway_duration_seconds_count{le="0.001"}`, []string{`has no label "le"`}},
		{"bound in text form", `ruralz_config_activation_duration_seconds_bucket{le="2"}`, []string{`"2" is not a value of le`}},
		{"bound of another set", `ruralz_http_gateway_duration_seconds_bucket{le="60.0"}`, []string{`"60.0" is not a value of le`}},
		{"bad enumeration value", `ruralz_node_degraded_info{reason="detached"}`, []string{`"detached" is not a value of reason`}},
		{"bad alternation value", `ruralz_node_degraded_info{reason!~"lkg_boot|bogus|crl_stale"}`, []string{`"bogus" is not a value of reason`}},
		{"bound alternation", `ruralz_http_gateway_duration_seconds_bucket{le=~"0.001|9"}`, []string{`"9" is not a value of le`}},
		{"unescaped plus in a bound", `ruralz_http_request_body_bytes_bucket{le=~"1.048576e+06"}`, []string{`"1.048576e+06" matches no value of le`}},
		{"regex matching nothing", `ruralz_security_cleartext_hops{hop=~"state_.*|bogus.*"}`, []string{`"bogus.*" matches no value of hop`}},
		{"invalid regex", `ruralz_security_cleartext_hops{hop=~"(admin"}`, []string{"invalid regular expression"}},
		{"regex valid only inside the anchor group", `ruralz_security_cleartext_hops{hop=~"admin)|(state_store"}`, []string{"invalid regular expression"}},
		{"histogram family name", `rate(ruralz_http_request_duration_seconds[5m])`, []string{"is a histogram with no series of that name"}},
		{"histogram family name matcher", `{__name__="ruralz_filter_duration_seconds"}`, []string{"is a histogram with no series of that name"}},
		{"label_values unknown label", `label_values(ruralz_http_requests_total, upstream)`, []string{`has no label "upstream"`}},
		{"selector without a name", `{route="a"}`, []string{"a selector has no metric name"}},
		{"unterminated string", `ruralz_http_requests_total{route="a}`, []string{"unterminated string"}},
		{"matcher without operator", `ruralz_http_requests_total{route}`, []string{"no matcher operator"}},
		{"matcher without quotes", `ruralz_http_requests_total{route=a}`, []string{"no quoted value"}},
		{"malformed matcher", `ruralz_http_requests_total{,}`, []string{"malformed label matcher"}},
		{"unbalanced range", `rate(ruralz_http_requests_total[5m`, []string{"unbalanced"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := checkExpr(tc.expr)
			if len(got) != len(tc.want) {
				t.Fatalf("checkExpr(%q) = %q, want %d problem(s)", tc.expr, got, len(tc.want))
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("problem %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}

// TestOpenMetricsFloat pins the le rendering the rules and panels match
// (09 section 9 risk 6).
func TestOpenMetricsFloat(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{2, "2.0"},
		{1, "1.0"},
		{60, "60.0"},
		{3600, "3600.0"},
		{0.001, "0.001"},
		{0.00015, "0.00015"},
		{0.00001, "1e-05"},
		{0.000025, "2.5e-05"},
		{1048576, "1.048576e+06"},
		{2.5, "2.5"},
	} {
		if got := openMetricsFloat(tc.in); got != tc.want {
			t.Errorf("openMetricsFloat(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for in, want := range map[string]string{"+Inf": "+Inf", "-Inf": "-Inf", "NaN": "NaN"} {
		f := map[string]float64{"+Inf": posInf(), "-Inf": -posInf(), "NaN": posInf() - posInf()}[in]
		if got := openMetricsFloat(f); got != want {
			t.Errorf("openMetricsFloat(%s) = %q", in, got)
		}
	}
}

func posInf() float64 {
	zero := 0.0
	return 1 / zero
}

// TestM1RuleExpressions pins the six M1 rules to the exact expressions, for
// durations and severities of OBS "Alert rules" (09 req 72).
// RuralzSeriesFolded keeps OBS's "new folding only" expression without the
// rare-event clause.
func TestM1RuleExpressions(t *testing.T) {
	want := map[string]alertRule{
		"RuralzNodeDegradedPage": {
			expr: `ruralz_node_degraded_info{reason=~"state_store_breaker_open|upstream_panic|snapshot_ending_overdue"} == 1`, forDur: "5m", severity: severityPage,
		},
		"RuralzNodeDegraded": {
			expr: `ruralz_node_degraded_info{reason!~"state_store_breaker_open|upstream_panic|snapshot_ending_overdue|cleartext_hop"} == 1`, forDur: "5m", severity: severityTicket,
		},
		"RuralzCleartextHop": {
			expr: `ruralz_security_cleartext_hops{hop=~"state_store|telemetry|admin"} > 0`, forDur: "15m", severity: severityTicket,
		},
		"RuralzGatewayP50": {
			expr:   `sum(rate(ruralz_http_gateway_duration_seconds_bucket{le="0.00015"}[1h])) / sum(rate(ruralz_http_gateway_duration_seconds_count[1h])) < 0.5`,
			forDur: "30m", severity: severityTicket,
		},
		"RuralzStateWritesDropped": {
			expr: `rate(ruralz_state_writes_dropped_total[5m]) > 0`, forDur: "10m", severity: severityTicket,
		},
		"RuralzSeriesFolded": {
			expr:     `delta(ruralz_telemetry_folded_label_sets[15m]) > 0`,
			severity: severityTicket,
		},
	}
	rules := m1Rules()
	if len(rules) != len(want) {
		t.Fatalf("got %d M1 rules, want %d", len(rules), len(want))
	}
	for _, r := range rules {
		w, ok := want[r.name]
		if !ok {
			t.Errorf("unexpected rule %s", r.name)
			continue
		}
		if r.expr != w.expr || r.forDur != w.forDur || r.severity != w.severity {
			t.Errorf("%s = (%q, for %q, %s), want (%q, for %q, %s)", r.name, r.expr, r.forDur, r.severity, w.expr, w.forDur, w.severity)
		}
	}
}

// TestBurnRateRules: a page and a ticket rule for every SLO objective of
// 90% or more, none for SLO-GW-3 (50%), at 14 and 6 times the budget
// (09 req 71, 72).
func TestBurnRateRules(t *testing.T) {
	thresholds := map[string][2]string{
		"SLO-GW-1": {"0.007", "0.003"},
		"SLO-GW-2": {"0.14", "0.06"},
		"SLO-GW-5": {"0.14", "0.06"},
		"SLO-GW-6": {"0.14", "0.06"},
		"SLO-GW-7": {"0.014", "0.006"},
	}
	rules := burnRateRules()
	if len(rules) != 2*len(thresholds) {
		t.Fatalf("got %d burn-rate rules, want %d", len(rules), 2*len(thresholds))
	}
	for _, r := range rules {
		id := r.extra[0].value
		th, ok := thresholds[id]
		if !ok {
			t.Errorf("%s: unexpected burn-rate rule for %s", r.name, id)
			continue
		}
		long, short, v := "[1h]", "[5m]", th[0]
		if r.severity == severityTicket {
			long, short, v = "[6h]", "[30m]", th[1]
		}
		if strings.Count(r.expr, "> "+v) != 2 || !strings.Contains(r.expr, long) || !strings.Contains(r.expr, short) {
			t.Errorf("%s %s: %q lacks windows %s, %s or threshold %s", r.name, id, r.expr, long, short, v)
		}
	}
	for _, s := range slos() {
		if s.id == "SLO-GW-3" && s.budget() != "0.5" {
			t.Errorf("SLO-GW-3 budget = %s", s.budget())
		}
		if s.id == "SLO-GW-1" && s.budget() != "0.0005" {
			t.Errorf("SLO-GW-1 budget = %s", s.budget())
		}
	}
}

func TestCheckRulesProblems(t *testing.T) {
	bad := []ruleGroup{{name: "g", rules: []alertRule{
		{name: "lowercase", expr: "ruralz_unknown_total > 0", forDur: "5 minutes", severity: "info", dashboard: "Nope"},
		{name: "RuralzDup", expr: "ruralz_tap_events_dropped_total > 0", severity: severityTicket, dashboard: dashTelemetry, summary: "s"},
		{name: "RuralzDup", expr: "ruralz_tap_events_dropped_total > 0", severity: severityTicket, dashboard: dashTelemetry, summary: "s"},
	}}}
	got := checkRules(bad, dashboardTitles())
	for _, w := range []string{
		"is not a metric", "alert names match", `severity "info"`, `for "5 minutes"`,
		`dashboard "Nope"`, "no summary", "duplicate rule RuralzDup",
	} {
		if !slices.ContainsFunc(got, func(g string) bool { return strings.Contains(g, w) }) {
			t.Errorf("missing problem %q in %q", w, got)
		}
	}
	if got := checkRules(ruleGroups(), dashboardTitles()); len(got) != 0 {
		t.Errorf("generated rules: %q", got)
	}
}

// TestDashboardPack: the eight M1 dashboards of OBS "Grafana dashboard
// pack" in order (09 req 73), each a valid Grafana model with unique panel
// IDs inside the 24-column grid.
func TestDashboardPack(t *testing.T) {
	want := []string{
		"Ruralz Gateway overview", "Route detail", "Policies and Filter Chain", "Upstreams and resilience",
		"State Store, Rate Limits and Quotas", "Node runtime and configuration", "Telemetry health", "SLO burn rates",
	}
	if got := dashboardTitles(); !slices.Equal(got, want) {
		t.Fatalf("dashboard titles = %q, want %q", got, want)
	}
	for _, d := range dashboards() {
		data, err := renderDashboard(d)
		if err != nil {
			t.Fatal(err)
		}
		var m gDashboard
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("%s: %v", d.uid, err)
		}
		if m.UID != d.uid || m.Title != d.title || !slices.Contains(m.Tags, packTag) {
			t.Errorf("%s: uid %q, title %q, tags %q", d.uid, m.UID, m.Title, m.Tags)
		}
		ids := map[int]bool{}
		for _, p := range m.Panels {
			if ids[p.ID] || p.GridPos.X+p.GridPos.W > gridColumns || len(p.Targets) == 0 {
				t.Errorf("%s: panel %d %q: duplicate ID, outside the grid or no target", d.uid, p.ID, p.Title)
			}
			ids[p.ID] = true
		}
		if problems := checkDashboardJSON(d.uid, data); len(problems) != 0 {
			t.Errorf("%s: %q", d.uid, problems)
		}
	}
	// The SLO dashboard has a budget and a burn-rate panel per M1 SLO. Burn
	// rates draw the ticket and page lines only where burn-rate rules exist;
	// SLO-GW-3, whose burn rate cannot exceed 2, draws its threshold rule's
	// line at 1.
	slo := sloDashboard()
	if len(slo.panels) != 2*len(slos()) {
		t.Fatalf("SLO dashboard has %d panels, want %d", len(slo.panels), 2*len(slos()))
	}
	for i, s := range slos() {
		p := slo.panels[2*i+1]
		want := []float64{ticketFactor, pageFactor}
		if s.id == "SLO-GW-3" {
			want = []float64{1}
			if !strings.Contains(p.description, "RuralzGatewayP50") || !strings.Contains(p.description, "at 2.") {
				t.Errorf("%s: description %q", p.title, p.description)
			}
		}
		if !slices.Equal(p.thresholds, want) {
			t.Errorf("%s: thresholds %v, want %v", p.title, p.thresholds, want)
		}
	}
	// Route detail charts duration per Route.
	for _, p := range routeDashboard().panels {
		if p.title != "Duration" {
			continue
		}
		for _, tg := range p.targets {
			if !strings.Contains(tg.expr, "sum by (le, route)") || !strings.HasSuffix(tg.legend, " {{route}}") {
				t.Errorf("Route detail duration target %q (%q) is not per Route", tg.expr, tg.legend)
			}
		}
	}
}

func TestCheckDashboardDefsProblems(t *testing.T) {
	defs := []dashboardDef{
		{uid: "a", title: "A", vars: []queryVar{{name: "route", metric: "ruralz_nope_total"}}, panels: []panelDef{
			{title: "empty"},
			ts("bad", unitShort, q("ruralz_nope_total", "")),
		}},
		{uid: "a", title: "B"},
	}
	got := checkDashboardDefs(defs)
	for _, w := range []string{"variable route: ruralz_nope_total", "needs a title, a unit and a query", `panel "bad": ruralz_nope_total`, "duplicate uid"} {
		if !slices.ContainsFunc(got, func(g string) bool { return strings.Contains(g, w) }) {
			t.Errorf("missing problem %q in %q", w, got)
		}
	}
}

// TestPromtoolFixtureCoverage: the promtool test file pre-creates every
// series the M1 rules alert on at 0 (09 req 45: every degraded reason, hop,
// dropped-write kind and foldable family), and every generated alert has a
// firing case (spec 09 test 43).
func TestPromtoolFixtureCoverage(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "ruralz.rules.test.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	var want []string
	for _, r := range catalog.ReasonNames() {
		want = append(want, `ruralz_node_degraded_info{instance="n1",reason="`+r+`"}'`+"\n        values: '0x40'")
	}
	for _, h := range catalog.HopNames() {
		want = append(want, `ruralz_security_cleartext_hops{instance="n1",hop="`+h+`"}'`+"\n        values: '0x40'")
	}
	for _, k := range catalog.WriteKinds() {
		want = append(want, `ruralz_state_writes_dropped_total{instance="n1",kind="`+k+`"}'`+"\n        values: '0x40'")
	}
	for _, f := range catalog.Families() {
		if f.Class != catalog.ClassListener {
			want = append(want, `ruralz_telemetry_folded_label_sets{instance="n1",instrument="`+f.Name+`"}'`+"\n        values: '0x40'")
		}
	}
	for _, w := range want {
		if !strings.Contains(src, w) {
			t.Errorf("testdata/ruralz.rules.test.yaml does not pre-create %s", strings.SplitN(w, "'", 2)[0])
		}
	}
	for _, g := range ruleGroups() {
		for _, r := range g.rules {
			fires := "alertname: " + r.name + "\n        exp_alerts:\n          - exp_labels:\n              severity: " + r.severity
			if !strings.Contains(src, fires) {
				t.Errorf("no promtool case where %s (%s) fires", r.name, r.severity)
			}
		}
	}
	for _, s := range slos() {
		if s.objective >= burnMinimum && !strings.Contains(src, "slo: "+s.id) {
			t.Errorf("no promtool case where the %s burn-rate rules fire", s.id)
		}
	}
}

func TestYAMLString(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"page", "page"},
		{"5m", "5m"},
		{"15m", "15m"},
		{"05m", "'05m'"},
		{"", "''"},
		{"yes", "'yes'"},
		{"Null", "'Null'"},
		{"1", "'1'"},
		{"a: b", "'a: b'"},
		{"a #b", "'a #b'"},
		{"trailing ", "'trailing '"},
		{"colon:", "'colon:'"},
		{"{{ $labels.x }}", "'{{ $labels.x }}'"},
		{"it's", "it's"},
		{"'quoted'", "'''quoted'''"},
		{"150 µs", "'150 µs'"},
		{"tab\there", `"tab\there"`},
		{`x{a="b"} == 1`, `x{a="b"} == 1`},
		{`(x) > 1`, `'(x) > 1'`},
	} {
		if got := yamlString(tc.in); got != tc.want {
			t.Errorf("yamlString(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestRun(t *testing.T) {
	if err := run("", &bytes.Buffer{}); err == nil {
		t.Error("run without -out: want an error")
	}
	dir := t.TempDir()
	var w bytes.Buffer
	if err := run(dir, &w); err != nil {
		t.Fatalf("run: %v\n%s", err, w.String())
	}
	files, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.path))) //nolint:gosec // G304: generated paths under t.TempDir.
		if err != nil || !bytes.Equal(got, f.data) {
			t.Errorf("%s not written: %v", f.path, err)
		}
	}
	// Generated files the tables no longer produce are removed; hand-written
	// files and directories are kept.
	stale := map[string]string{
		"dashboards/ruralz-renamed.json": `{"title": "Old", "description": "Generated by telemetrygen from the Ruralz telemetry catalog."}`,
		"rules/old.rules.yaml":           "# Code generated by telemetrygen from internal/telemetry/catalog. DO NOT EDIT.\ngroups: []\n",
	}
	kept := map[string]string{
		"dashboards/mine.json": `{"title": "Mine", "panels": [{"title": "Goroutines", "targets": [{"refId": "A", "expr": "ruralz_runtime_goroutines"}]}]}`,
		"rules/mine.yaml":      "groups: []\n",
	}
	for _, m := range []map[string]string{stale, kept} {
		for rel, data := range m {
			if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "dashboards", "drafts"), 0o750); err != nil {
		t.Fatal(err)
	}
	w.Reset()
	if err := run(dir, &w); err != nil {
		t.Fatalf("run with stale files: %v\n%s", err, w.String())
	}
	for rel := range stale {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("stale %s not removed: %v", rel, err)
		}
		if !strings.Contains(w.String(), "removed stale generated file "+rel) {
			t.Errorf("removal of %s not reported in %q", rel, w.String())
		}
	}
	for rel := range kept {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("hand-written %s: %v", rel, err)
		}
	}

	// A hand-written dashboard in the output directory is checked too.
	bad, err := os.ReadFile(filepath.Join("testdata", "bad-dashboard.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dashboards", "hand-written.json"), bad, 0o600); err != nil { //nolint:gosec // G703: a fixed name under t.TempDir.
		t.Fatal(err)
	}
	w.Reset()
	if err := run(dir, &w); err == nil || !strings.Contains(w.String(), "ruralz_unknown_total") {
		t.Errorf("run with a bad dashboard: err %v, output %q", err, w.String())
	}
}

func TestStaleGeneratedWithoutOutput(t *testing.T) {
	got, err := staleGenerated(t.TempDir(), nil)
	if err != nil || len(got) != 0 {
		t.Errorf("staleGenerated(empty dir) = %q, %v", got, err)
	}
}

// TestClassifyDatasource: only Prometheus queries are checked (09 req 73);
// a missing reference falls back to the panel and then to Grafana's
// default data source, which is checked.
func TestClassifyDatasource(t *testing.T) {
	prom := []string{"datasource", "DS_PROM"}
	for _, tc := range []struct {
		name string
		ref  any
		want dsClass
	}{
		{"absent", nil, dsDefault},
		{"empty object", map[string]any{}, dsDefault},
		{"mixed", map[string]any{"type": "datasource", "uid": "-- Mixed --"}, dsDefault},
		{"mixed name", "-- Mixed --", dsDefault},
		{"prometheus type", map[string]any{"type": "prometheus", "uid": "abc"}, dsPrometheus},
		{"variable uid", map[string]any{"uid": "${datasource}"}, dsPrometheus},
		{"variable with format", "${datasource:text}", dsPrometheus},
		{"bracket variable", "[[datasource]]", dsPrometheus},
		{"dollar variable", "$datasource", dsPrometheus},
		{"input", "${DS_PROM}", dsPrometheus},
		{"legacy name", "Prometheus EU", dsPrometheus},
		{"loki type", map[string]any{"type": "loki", "uid": "${datasource}"}, dsOther},
		{"grafana", map[string]any{"type": "datasource", "uid": "-- Grafana --"}, dsOther},
		{"other variable", "${loki}", dsOther},
		{"other name", "Loki", dsOther},
		{"uid without type", map[string]any{"uid": "P1809F7CD0C75ACF3"}, dsOther},
		{"number", 7.0, dsOther},
	} {
		if got := classifyDatasource(tc.ref, prom); got != tc.want {
			t.Errorf("%s: classifyDatasource(%v) = %d, want %d", tc.name, tc.ref, got, tc.want)
		}
	}
	if name, ok := variableName("plain"); ok || name != "" {
		t.Errorf("variableName(plain) = %q, %v", name, ok)
	}
}

// FuzzCheckExpr: the expression scanner never panics or loops on any input
// and is deterministic.
func FuzzCheckExpr(f *testing.F) {
	for _, g := range ruleGroups() {
		for _, r := range g.rules {
			f.Add(r.expr)
		}
	}
	for _, seed := range []string{
		`label_values(ruralz_http_requests_total, route)`, `{__name__="x"}`, `"unterminated`, `x{a=`, `$`, `${`,
		`[`, `x{a!~'b\'c'}`, "x{a=`b`}", `1e+`, `.5e-3`, `label_values(`, `sum by (`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		a, b := checkExpr(expr), checkExpr(expr)
		if !slices.Equal(a, b) {
			t.Fatalf("checkExpr(%q) is not deterministic", expr)
		}
	})
}

func BenchmarkGenerate(b *testing.B) {
	for b.Loop() {
		if _, err := generate(); err != nil {
			b.Fatal(err)
		}
	}
}
