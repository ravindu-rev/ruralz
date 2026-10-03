// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// catalogDir holds the only Go files allowed to spell metric and span names.
const catalogDir = "internal/telemetry/catalog/"

// spanNS is the span name namespace. Span names and prefixes are assembled
// from it here, because repocheck's own literals must not start with a span
// prefix (check (b) below).
const spanNS = "ruralz"

// catalogView is the part of the telemetry catalog the checks read: the
// linked catalog in production, a fixture in tests.
type catalogView struct {
	families []catalog.Family
	reasons  []string
	hops     []string
	// Span names: the route-match span, the Filter and Upstream prefixes, and
	// the names FilterSpanName and UpstreamSpanName build for a sample name.
	spanRouteMatch, spanFilterPrefix, spanUpstreamPrefix string
	filterSpanSample, upstreamSpanSample                 string
}

// spanSample is the Policy and Upstream name used to check the span name
// builders.
const spanSample = "sample"

// linkedCatalog is the catalog compiled into repocheck.
func linkedCatalog() catalogView {
	return catalogView{
		families:           catalog.Families(),
		reasons:            catalog.ReasonNames(),
		hops:               catalog.HopNames(),
		spanRouteMatch:     catalog.SpanRouteMatch,
		spanFilterPrefix:   catalog.SpanFilterPrefix,
		spanUpstreamPrefix: catalog.SpanUpstreamPrefix,
		filterSpanSample:   catalog.FilterSpanName(spanSample),
		upstreamSpanSample: catalog.UpstreamSpanName(spanSample),
	}
}

// validateCatalog is check (d) of 09 req 77, catalog.Validate() in spec 09
// section 3: the foundation pack grammar and suffix rules for every family,
// unique names, well-formed labels, units and bounds, degraded reasons and
// hops, and the three span names.
func validateCatalog(c catalogView) []string {
	grammar := regexp.MustCompile(`^ruralz_[a-z][a-z0-9]*(_[a-z0-9]+)+$`)
	snake := regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	unit := regexp.MustCompile(`^(s|By|1|\{[a-z][a-z0-9]*(_[a-z0-9]+)*\})$`)
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	seen := map[string]bool{}
	for _, f := range c.families {
		n := f.Name
		if !grammar.MatchString(n) {
			add("%s does not match the pack grammar %s", n, grammar)
		}
		if seen[n] {
			add("%s is defined twice", n)
		}
		seen[n] = true
		switch f.Kind {
		case catalog.Counter:
			if !strings.HasSuffix(n, "_total") {
				add("counter %s must end in _total", n)
			}
		case catalog.Gauge:
			if strings.HasSuffix(n, "_total") {
				add("gauge %s must not end in _total", n)
			}
		case catalog.Histogram:
			if !hasAnySuffix(n, "_seconds", "_bytes", "_ratio") {
				add("histogram %s must end in _seconds, _bytes or _ratio", n)
			}
		default:
			add("%s has no instrument type", n)
		}
		if (f.Kind == catalog.Histogram) != (f.Bounds != catalog.BoundsNone) {
			add("%s: only histograms, and every histogram, name a bound set", n)
		}
		if !unit.MatchString(f.Unit) {
			add("%s: unit %q is not s, By, 1 or a {counted_noun} annotation", n, f.Unit)
		}
		if f.Unit == "1" && !hasAnySuffix(n, "_info", "_ratio") {
			add("%s has unit 1 and must end in _info or _ratio", n)
		}
		if strings.HasSuffix(n, "_seconds") && f.Unit != "s" {
			add("%s must have unit s", n)
		}
		if strings.HasSuffix(n, "_bytes") && f.Unit != "By" {
			add("%s must have unit By", n)
		}
		if strings.HasSuffix(n, "_info") && (f.Kind != catalog.Gauge || f.Unit != "1") {
			add("%s must be a gauge with unit 1", n)
		}
		labels := map[string]bool{}
		for _, l := range f.Labels {
			if !snake.MatchString(l.Name) || labels[l.Name] {
				add("%s: label %q is not snake_case or is repeated", n, l.Name)
			}
			labels[l.Name] = true
		}
	}
	for what, names := range map[string][]string{"degraded reason": c.reasons, "hop": c.hops} {
		dup := map[string]bool{}
		for _, r := range names {
			if !snake.MatchString(r) || dup[r] {
				add("%s %q is not snake_case or is repeated", what, r)
			}
			dup[r] = true
		}
	}
	filter, upstream := spanNS+".filter.", spanNS+".upstream."
	if c.spanRouteMatch != spanNS+".route.match" {
		add("route match span is %q, want %s.route.match", c.spanRouteMatch, spanNS)
	}
	if c.spanFilterPrefix != filter || c.filterSpanSample != filter+spanSample {
		add("Filter spans are %q and %q, want the prefix %s", c.spanFilterPrefix, c.filterSpanSample, filter)
	}
	if c.spanUpstreamPrefix != upstream || c.upstreamSpanSample != upstream+spanSample {
		add("Upstream spans are %q and %q, want the prefix %s", c.spanUpstreamPrefix, c.upstreamSpanSample, upstream)
	}
	slices.Sort(out)
	return out
}

// checkCatalog reports the validateCatalog problems as findings.
func checkCatalog(cv catalogView) []finding {
	var out []finding
	for _, p := range validateCatalog(cv) {
		out = append(out, finding{path: catalogDir + "catalog.go", msg: p})
	}
	return out
}

func hasAnySuffix(s string, suffixes ...string) bool {
	return slices.ContainsFunc(suffixes, func(x string) bool { return strings.HasSuffix(s, x) })
}

// checkTelemetryNames is checks (a) to (c) of 09 req 77 for one non-test Go
// file outside the catalog: (a) a string literal that is exactly a metric
// name, (b) a literal starting with a span name prefix, and (c) a call
// X.Start(ctx, "<literal>", ...) naming a span with a literal.
func checkTelemetryNames(fset *token.FileSet, rel string, f *ast.File) []finding {
	if strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, catalogDir) {
		return nil
	}
	metric := regexp.MustCompile(`^ruralz_[a-z0-9_]+$`)
	prefixes := []string{spanNS + ".route.", spanNS + ".filter.", spanNS + ".upstream."}
	var out []finding
	at := func(n ast.Node, msg string) {
		out = append(out, finding{path: rel, line: fset.Position(n.Pos()).Line, msg: msg})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(n.Value)
			if err != nil {
				return true
			}
			if metric.MatchString(v) {
				at(n, fmt.Sprintf("metric name %q: use the internal/telemetry/catalog constant", v))
			}
			for _, p := range prefixes {
				if strings.HasPrefix(v, p) {
					at(n, fmt.Sprintf("span name %q: build span names with internal/telemetry/catalog", v))
				}
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Start" || len(n.Args) < 2 {
				return true
			}
			if lit, ok := n.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				at(lit, fmt.Sprintf("span started with the literal name %s: span names come from internal/telemetry/catalog", lit.Value))
			}
		}
		return true
	})
	return out
}
