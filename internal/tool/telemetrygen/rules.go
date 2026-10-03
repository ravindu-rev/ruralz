// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Alert severities (docs/architecture/10-observability.md "Alert rules").
const (
	severityPage   = "page"
	severityTicket = "ticket"
)

// Dashboard titles of the M1 Grafana dashboard pack (09 req 73).
const (
	dashOverview  = "Ruralz Gateway overview"
	dashRoute     = "Route detail"
	dashPolicies  = "Policies and Filter Chain"
	dashUpstreams = "Upstreams and resilience"
	dashState     = "State Store, Rate Limits and Quotas"
	dashNode      = "Node runtime and configuration"
	dashTelemetry = "Telemetry health"
	dashSLO       = "SLO burn rates"
)

// objectiveScale is the denominator of slo.objective: objectives are
// integers in thousandths of a percent, so budgets and burn thresholds are
// exact decimals (99.95% is 99950).
const objectiveScale = 100000

// Burn-rate windows and factors (09 req 72, target): page when the 1 h and
// 5 min windows both burn at 14 times the budget rate, ticket at 6 times over
// 6 h and 30 min.
const (
	pageFactor   = 14
	ticketFactor = 6
	burnMinimum  = 90000 // burn-rate rules cover objectives of 90% or more
)

// slo is one M1 service level objective (09 req 71; OBS "SLO definitions").
// Exactly one of good and bad is set: good builds the ratio of events that
// meet the objective over a PromQL range, bad the ratio that miss it. An
// objective below burnMinimum names the threshold rule that alerts on it
// instead of the burn-rate rules.
type slo struct {
	id            string
	title         string
	objective     int
	good          func(rng string) string
	bad           func(rng string) string
	thresholdRule string
}

// errorRatio is the PromQL ratio of events that miss the objective.
func (s slo) errorRatio(rng string) string {
	if s.bad != nil {
		return s.bad(rng)
	}
	return "1 - " + s.good(rng)
}

// budget is the error budget as a decimal string ("0.0005").
func (s slo) budget() string { return scaled(objectiveScale - s.objective) }

// scaled formats n / objectiveScale as the shortest exact decimal.
func scaled(n int) string {
	return strconv.FormatFloat(float64(n)/objectiveScale, 'f', -1, 64)
}

// withinBound builds the good-event ratio of a latency SLO: the share of a
// histogram's observations at or below bound, with extra matchers.
func withinBound(family, matchers string, bound float64) func(string) string {
	le := fmt.Sprintf(`le="%s"`, openMetricsFloat(bound))
	bucket, count := le, ""
	if matchers != "" {
		bucket, count = matchers+","+le, "{"+matchers+"}"
	}
	return func(rng string) string {
		return fmt.Sprintf("sum(rate(%[1]s_bucket{%[2]s}[%[4]s])) / sum(rate(%[1]s_count%[3]s[%[4]s]))",
			family, bucket, count, rng)
	}
}

// sloGW3 is SLO-GW-3, whose 50% objective is below the burn-rate minimum;
// the RuralzGatewayP50 threshold rule alerts on it instead.
func sloGW3() slo {
	return slo{
		id: "SLO-GW-3", title: "Gateway-added time within 150 µs", objective: 50000,
		good:          withinBound(catalog.HTTPGatewayDurationSeconds, "", 0.00015),
		thresholdRule: "RuralzGatewayP50",
	}
}

// slos returns the M1 SLOs of 09 req 71 in ID order.
func slos() []slo {
	return []slo{
		{
			id: "SLO-GW-1", title: "Node-generated 5xx responses", objective: 99950,
			bad: func(rng string) string {
				return fmt.Sprintf(`sum(rate(%[1]s{status_class="5xx",origin="node"}[%[2]s])) / sum(rate(%[1]s[%[2]s]))`,
					catalog.HTTPListenerRequestsTotal, rng)
			},
		},
		{
			id: "SLO-GW-2", title: "Gateway-added time within 1 ms", objective: 99000,
			good: withinBound(catalog.HTTPGatewayDurationSeconds, "", 0.001),
		},
		sloGW3(),
		{
			// script_multi round trips are excluded by selecting op="gcra".
			id: "SLO-GW-5", title: "GCRA State Store round trips within 1 ms", objective: 99000,
			good: withinBound(catalog.StateCallDurationSeconds, `op="gcra"`, 0.001),
		},
		{
			id: "SLO-GW-6", title: "Compile of up to 10,000 Routes within 2 s", objective: 99000,
			good: withinBound(catalog.ConfigActivationDurationSeconds, `stage="compile",size_class!="gt10000"`, 2),
		},
		{
			id: "SLO-GW-7", title: "Spans and log records exported", objective: 99900,
			bad: func(rng string) string {
				return fmt.Sprintf("(sum(rate(%[1]s[%[5]s])) + sum(rate(%[2]s[%[5]s]))) / (sum(rate(%[3]s[%[5]s])) + sum(rate(%[4]s[%[5]s])))",
					catalog.TelemetrySpansDroppedTotal, catalog.TelemetryLogsDroppedTotal,
					catalog.TelemetrySpansTotal, catalog.TelemetryLogsTotal, rng)
			},
		},
	}
}

// ruleLabel is one label of a generated rule.
type ruleLabel struct {
	name, value string
}

// alertRule is one generated Prometheus alerting rule (spec 09 section 3
// AlertRule: name, expression, for, severity, dashboard).
type alertRule struct {
	name      string
	expr      string
	forDur    string // empty for none
	severity  string
	extra     []ruleLabel // labels after severity
	dashboard string      // title of the dashboard that charts the alert
	summary   string      // annotation; may use $labels templates
}

// labels returns the rule labels in output order.
func (r alertRule) labels() []ruleLabel {
	return append([]ruleLabel{{"severity", r.severity}}, r.extra...)
}

// ruleGroup is one Prometheus rule group.
type ruleGroup struct {
	name  string
	rules []alertRule
}

// reasonAlternation joins degraded reasons into a PromQL regex alternation.
func reasonAlternation(rs ...catalog.Reason) string {
	names := make([]string, len(rs))
	for i, r := range rs {
		names[i] = r.String()
	}
	return strings.Join(names, "|")
}

// m1Rules returns the M1 rules of OBS "Alert rules" with their exact
// expressions, for durations and severities (09 req 72). Control and AI
// rules are M2 and M3.
func m1Rules() []alertRule {
	page := []catalog.Reason{catalog.ReasonStateStoreBreakerOpen, catalog.ReasonUpstreamPanic, catalog.ReasonSnapshotEndingOverdue}
	notTicket := slices.Concat(page, []catalog.Reason{catalog.ReasonCleartextHop})
	hops := strings.Join([]string{catalog.HopStateStore.String(), catalog.HopTelemetry.String(), catalog.HopAdmin.String()}, "|")
	gw3 := sloGW3()
	return []alertRule{
		{
			name: "RuralzNodeDegradedPage", severity: severityPage, forDur: "5m", dashboard: dashOverview,
			expr:    fmt.Sprintf(`%s{reason=~"%s"} == 1`, catalog.NodeDegradedInfo, reasonAlternation(page...)),
			summary: "Node {{ $labels.instance }} is degraded: {{ $labels.reason }}",
		},
		{
			name: "RuralzNodeDegraded", severity: severityTicket, forDur: "5m", dashboard: dashOverview,
			expr:    fmt.Sprintf(`%s{reason!~"%s"} == 1`, catalog.NodeDegradedInfo, reasonAlternation(notTicket...)),
			summary: "Node {{ $labels.instance }} is degraded: {{ $labels.reason }}",
		},
		{
			// client and upstream hops chart on the Node dashboard only
			// (OQ-observability-21 (a)).
			name: "RuralzCleartextHop", severity: severityTicket, forDur: "15m", dashboard: dashNode,
			expr:    fmt.Sprintf(`%s{hop=~"%s"} > 0`, catalog.SecurityCleartextHops, hops),
			summary: "Node {{ $labels.instance }} has a cleartext {{ $labels.hop }} hop",
		},
		{
			name: gw3.thresholdRule, severity: severityTicket, forDur: "30m", dashboard: dashSLO,
			expr:    fmt.Sprintf("%s < %s", gw3.good("1h"), scaled(gw3.objective)),
			summary: "Fewer than half of the requests spend at most 150 µs in the gateway (SLO-GW-3)",
		},
		{
			name: "RuralzStateWritesDropped", severity: severityTicket, forDur: "10m", dashboard: dashState,
			expr:    fmt.Sprintf("rate(%s[5m]) > 0", catalog.StateWritesDroppedTotal),
			summary: "Node {{ $labels.instance }} drops {{ $labels.kind }} State Store writes",
		},
		{
			// New folding only, as OBS writes it: large Bundles fold by design
			// at their first admission, so this rule deliberately omits the
			// rare-event clause "or (x > 0 unless x offset 15m)", which would
			// ticket every new Node (every Pod of a rollout) whose first
			// admission folds. It is not a rare-event rule (architecture
			// 2.16), so neither that clause of spec 09 req 72 nor the
			// event-before-the-first-scrape case of req 72 and test 43
			// applies to it.
			name: "RuralzSeriesFolded", severity: severityTicket, dashboard: dashTelemetry,
			expr:    fmt.Sprintf("delta(%s[15m]) > 0", catalog.TelemetryFoldedLabelSets),
			summary: "Node {{ $labels.instance }} folded new {{ $labels.instrument }} label sets into _overflow",
		},
	}
}

// burnRateRules returns a page and a ticket rule for every SLO objective of
// 90% or more (09 req 72).
func burnRateRules() []alertRule {
	var out []alertRule
	for _, s := range slos() {
		if s.objective < burnMinimum {
			continue
		}
		budget := objectiveScale - s.objective
		for _, b := range []struct {
			name, severity, long, short string
			factor                      int
		}{
			{"RuralzErrorBudgetBurnPage", severityPage, "1h", "5m", pageFactor},
			{"RuralzErrorBudgetBurnTicket", severityTicket, "6h", "30m", ticketFactor},
		} {
			threshold := scaled(budget * b.factor)
			out = append(out, alertRule{
				name:     b.name,
				severity: b.severity,
				extra:    []ruleLabel{{"slo", s.id}},
				expr: fmt.Sprintf("(%s) > %s and (%s) > %s",
					s.errorRatio(b.long), threshold, s.errorRatio(b.short), threshold),
				dashboard: dashSLO,
				summary: fmt.Sprintf("%s (%s) burns its 30-day error budget %d times too fast over %s and %s",
					s.id, s.title, b.factor, b.long, b.short),
			})
		}
	}
	return out
}

// ruleGroups returns the generated rule groups.
func ruleGroups() []ruleGroup {
	return []ruleGroup{
		{name: "ruralz", rules: m1Rules()},
		{name: "ruralz-slo-burn-rate", rules: burnRateRules()},
	}
}

// checkRules returns the problems of the rule groups: every expression
// passes checkExpr, names, severities and durations are well formed, each
// rule names an M1 dashboard, and no two rules share a name and label set.
func checkRules(groups []ruleGroup, dashboards []string) []string {
	alertNamePattern := regexp.MustCompile(`^Ruralz[A-Z][A-Za-z0-9]*$`)
	var out []string
	seen := map[string]bool{}
	for _, g := range groups {
		for _, r := range g.rules {
			where := g.name + "/" + r.name
			for _, p := range checkExpr(r.expr) {
				out = append(out, where+": "+p)
			}
			if !alertNamePattern.MatchString(r.name) {
				out = append(out, where+": alert names match "+alertNamePattern.String())
			}
			if r.severity != severityPage && r.severity != severityTicket {
				out = append(out, fmt.Sprintf("%s: severity %q is neither page nor ticket", where, r.severity))
			}
			if r.forDur != "" && !isDuration(r.forDur) {
				out = append(out, fmt.Sprintf("%s: for %q is not a Prometheus duration", where, r.forDur))
			}
			if !slices.Contains(dashboards, r.dashboard) {
				out = append(out, fmt.Sprintf("%s: dashboard %q is not in the dashboard pack", where, r.dashboard))
			}
			if r.summary == "" {
				out = append(out, where+": no summary")
			}
			key := r.name
			for _, l := range r.labels() {
				key += "," + l.name + "=" + l.value
			}
			if seen[key] {
				out = append(out, where+": duplicate rule "+key)
			}
			seen[key] = true
		}
	}
	return out
}

// isDuration reports whether s is a single-unit Prometheus duration such
// as "5m" or "30m".
func isDuration(s string) bool {
	digits := strings.TrimLeft(s, "0123456789")
	if len(digits) == len(s) || s[0] == '0' {
		return false
	}
	switch digits {
	case "ms", "s", "m", "h", "d", "w", "y":
		return true
	}
	return false
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// rulesHeader starts the generated rule file.
const rulesHeader = `# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Code generated by telemetrygen from internal/telemetry/catalog. DO NOT EDIT.
# Regenerate with "go generate ./internal/tool/telemetrygen".
#
# Prometheus alert rules of Ruralz Gateway (M1): the rules of
# docs/architecture/10-observability.md "Alert rules" and a page and a ticket
# burn-rate rule for every SLO objective of 90% or more ("SLO definitions").
# Histogram bounds are matched in the form Prometheus 3 stores them (le="2.0").
`

// renderRules writes the rule groups as a Prometheus rule file.
func renderRules(groups []ruleGroup) []byte {
	var b bytes.Buffer
	b.WriteString(rulesHeader)
	b.WriteString("groups:\n")
	for _, g := range groups {
		fmt.Fprintf(&b, "  - name: %s\n    rules:\n", yamlString(g.name))
		for _, r := range g.rules {
			fmt.Fprintf(&b, "      - alert: %s\n", yamlString(r.name))
			fmt.Fprintf(&b, "        expr: %s\n", yamlString(r.expr))
			if r.forDur != "" {
				fmt.Fprintf(&b, "        for: %s\n", yamlString(r.forDur))
			}
			b.WriteString("        labels:\n")
			for _, l := range r.labels() {
				fmt.Fprintf(&b, "          %s: %s\n", l.name, yamlString(l.value))
			}
			b.WriteString("        annotations:\n")
			fmt.Fprintf(&b, "          dashboard: %s\n", yamlString(r.dashboard))
			fmt.Fprintf(&b, "          summary: %s\n", yamlString(r.summary))
		}
	}
	return b.Bytes()
}

// yamlString renders s as a YAML scalar that parses back to the same
// string: plain when that is unambiguous, single-quoted otherwise, and
// double-quoted (JSON escapes are valid YAML) when s has control characters.
func yamlString(s string) string {
	if yamlPlainSafe(s) {
		return s
	}
	if strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0 {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return strconv.Quote(s)
}

// yamlPlainSafe reports whether s can be a plain block scalar: it starts
// with a letter (so it is never a number, an indicator or a tag), is not a
// YAML 1.1 boolean or null, and has no ": " or " #" sequence, no trailing
// space or colon and no control or non-ASCII characters.
func yamlPlainSafe(s string) bool {
	if isDuration(s) {
		return true // "5m" is a string in every YAML schema
	}
	if s == "" || !isLetter(s[0]) {
		return false
	}
	switch strings.ToLower(s) {
	case "y", "n", "yes", "no", "true", "false", "on", "off", "null":
		return false
	}
	if strings.Contains(s, ": ") || strings.Contains(s, " #") || strings.HasSuffix(s, " ") || strings.HasSuffix(s, ":") {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r > 0x7e }) < 0
}
