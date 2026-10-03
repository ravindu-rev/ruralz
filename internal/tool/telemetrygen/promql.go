// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// matcher is one label matcher of a PromQL vector selector.
type matcher struct {
	label, op, value string
}

// selector is one PromQL vector selector: a metric name and its matchers.
type selector struct {
	name     string
	matchers []matcher
}

// targetLabels are the labels Prometheus attaches to every scraped series;
// a rule or panel may match them on any family.
func targetLabels() []string { return []string{"instance", "job"} }

// histogramSuffixes are the series suffixes of a classic histogram
// (09 req 73: dashboard checks strip them before the catalog lookup).
func histogramSuffixes() []string { return []string{"_bucket", "_sum", "_count"} }

// promqlKeywords are identifiers that are never metric names: binary and
// set operators, modifiers, aggregation operators (which may be followed by
// "by" or "without" instead of a parenthesis) and the number literals.
func promqlKeywords() []string {
	return []string{
		"and", "or", "unless", "bool", "offset", "on", "ignoring", "group_left", "group_right",
		"by", "without", "atan2", "inf", "nan", "start", "end",
		"sum", "min", "max", "avg", "group", "stddev", "stdvar", "count", "count_values",
		"bottomk", "topk", "quantile", "limitk", "limit_ratio",
	}
}

// labelListKeywords are the keywords followed by a parenthesized list of
// label names rather than an expression.
func labelListKeywords() []string {
	return []string{"by", "without", "on", "ignoring", "group_left", "group_right"}
}

// openMetricsFloat formats a histogram bound as Prometheus 3 stores the le
// label: the shortest 'g' form with ".0" added to integers (le="2.0",
// le="0.00015", le="1e-05"). Rules and panels match that form (09 section 9
// risk 6).
func openMetricsFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if strings.ContainsAny(s, ".e") {
		return s
	}
	return s + ".0"
}

// exprScanner walks a PromQL expression (or a Grafana variable query) and
// collects its vector selectors. It is a small tokenizer, not a full
// parser: it recognizes identifiers, strings, numbers, durations, selectors,
// grouping label lists, # comments and Grafana $variables, which is enough
// to find every metric name an expression reads.
type exprScanner struct {
	src       string
	pos       int
	selectors []selector
	problems  []string
}

func (s *exprScanner) errorf(format string, args ...any) {
	s.problems = append(s.problems, fmt.Sprintf(format, args...))
}

func (s *exprScanner) peek() byte {
	if s.pos < len(s.src) {
		return s.src[s.pos]
	}
	return 0
}

// skipComment skips a # comment up to the end of its line.
func (s *exprScanner) skipComment() {
	if i := strings.IndexByte(s.src[s.pos:], '\n'); i >= 0 {
		s.pos += i
		return
	}
	s.pos = len(s.src)
}

func (s *exprScanner) skipSpace() {
	for s.pos < len(s.src) && strings.IndexByte(" \t\r\n", s.src[s.pos]) >= 0 {
		s.pos++
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentChar(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

func (s *exprScanner) ident() string {
	start := s.pos
	for s.pos < len(s.src) && isIdentChar(s.src[s.pos]) {
		s.pos++
	}
	return s.src[start:s.pos]
}

// str reads a quoted string at s.pos and returns its value.
func (s *exprScanner) str() (string, bool) {
	q := s.src[s.pos]
	i := s.pos + 1
	for i < len(s.src) && s.src[i] != q {
		if s.src[i] == '\\' && q != '`' {
			i++
		}
		i++
	}
	if i >= len(s.src) {
		s.errorf("unterminated string at offset %d", s.pos)
		s.pos = len(s.src)
		return "", false
	}
	raw := s.src[s.pos : i+1]
	s.pos = i + 1
	switch q {
	case '"', '`':
		v, err := strconv.Unquote(raw)
		if err != nil {
			s.errorf("malformed string %s", raw)
			return "", false
		}
		return v, true
	default: // single quotes: PromQL escapes as in double quotes
		inner := strings.ReplaceAll(raw[1:len(raw)-1], `\'`, `'`)
		v, err := strconv.Unquote(`"` + strings.ReplaceAll(inner, `"`, `\"`) + `"`)
		if err != nil {
			s.errorf("malformed string %s", raw)
			return "", false
		}
		return v, true
	}
}

// skipBalanced skips from an opening bracket to its matching close.
func (s *exprScanner) skipBalanced(open, closing byte) {
	depth := 0
	for s.pos < len(s.src) {
		switch s.src[s.pos] {
		case '"', '\'', '`':
			s.str()
			continue
		case '#':
			s.skipComment()
			continue
		case open:
			depth++
		case closing:
			depth--
			if depth == 0 {
				s.pos++
				return
			}
		}
		s.pos++
	}
	s.errorf("unbalanced %q", string(open))
}

// matchers reads a {...} matcher list at s.pos. On a malformed list it
// reports the problem and ends the scan.
func (s *exprScanner) matchers() ([]matcher, bool) {
	out, ok := s.matcherList()
	if !ok {
		s.pos = len(s.src)
	}
	return out, ok
}

func (s *exprScanner) matcherList() ([]matcher, bool) {
	s.pos++ // '{'
	var out []matcher
	for {
		s.skipSpace()
		if s.peek() == '}' {
			s.pos++
			return out, true
		}
		if !isIdentStart(s.peek()) {
			s.errorf("malformed label matcher at offset %d", s.pos)
			return out, false
		}
		m := matcher{label: s.ident()}
		s.skipSpace()
		for _, op := range []string{"=~", "!~", "!=", "="} {
			if strings.HasPrefix(s.src[s.pos:], op) {
				m.op = op
				s.pos += len(op)
				break
			}
		}
		if m.op == "" {
			s.errorf("label %q has no matcher operator", m.label)
			return out, false
		}
		s.skipSpace()
		if c := s.peek(); c != '"' && c != '\'' && c != '`' {
			s.errorf("label %q has no quoted value", m.label)
			return out, false
		}
		v, ok := s.str()
		if !ok {
			return out, false
		}
		m.value = v
		out = append(out, m)
		s.skipSpace()
		if s.peek() == ',' {
			s.pos++
		}
	}
}

// scan walks the whole expression.
func (s *exprScanner) scan() {
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch {
		case c == '"' || c == '\'' || c == '`':
			s.str()
		case c == '#':
			s.skipComment()
		case c == '$':
			s.pos++
			if s.peek() == '{' {
				s.skipBalanced('{', '}')
			} else {
				s.ident()
			}
		case c == '[':
			s.skipBalanced('[', ']') // range, subquery or $__rate_interval
		case c == '{':
			ms, ok := s.matchers()
			if !ok {
				return
			}
			s.selectors = append(s.selectors, selector{matchers: ms})
		case (c >= '0' && c <= '9') || (c == '.' && s.pos+1 < len(s.src) && s.src[s.pos+1] >= '0' && s.src[s.pos+1] <= '9'):
			s.number()
		case isIdentStart(c):
			s.identifier()
		default:
			s.pos++
		}
	}
}

// number skips a number or a duration (5m, 1e-05, 0x1f).
func (s *exprScanner) number() {
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		if isIdentChar(c) || c == '.' {
			s.pos++
			continue
		}
		if (c == '+' || c == '-') && (s.src[s.pos-1] == 'e' || s.src[s.pos-1] == 'E') {
			s.pos++
			continue
		}
		return
	}
}

// identifier classifies the identifier at s.pos: a function call, a
// keyword, a grouping label list, Grafana's label_values or a metric name.
func (s *exprScanner) identifier() {
	name := s.ident()
	save := s.pos
	s.skipSpace()
	next := s.peek()
	lower := strings.ToLower(name)
	switch {
	case slices.Contains(labelListKeywords(), lower) && next == '(':
		s.skipBalanced('(', ')')
	case name == "label_values" && next == '(':
		s.labelValues()
	case next == '(' && !slices.Contains(promqlKeywords(), lower):
		// A function call; its arguments are scanned as the loop goes on.
	case slices.Contains(promqlKeywords(), lower):
		s.pos = save
	default:
		sel := selector{name: name}
		if next == '{' {
			ms, ok := s.matchers()
			if !ok {
				return
			}
			sel.matchers = ms
		} else {
			s.pos = save
		}
		s.selectors = append(s.selectors, sel)
	}
}

// labelValues handles Grafana's label_values(selector, label) and
// label_values(label) variable queries.
func (s *exprScanner) labelValues() {
	start := s.pos
	s.skipBalanced('(', ')')
	inner := s.src[start+1 : max(start+1, s.pos-1)]
	sel, label, found := cutLast(inner, ',')
	if !found {
		return // label_values(label): no metric
	}
	sub := &exprScanner{src: sel}
	sub.scan()
	if len(sub.selectors) == 1 {
		// Record the label as a presence matcher so it is checked against
		// the family's labels.
		m := matcher{label: strings.TrimSpace(label), op: "=~", value: ".+"}
		sub.selectors[0].matchers = append(sub.selectors[0].matchers, m)
	}
	s.problems = append(s.problems, sub.problems...)
	s.selectors = append(s.selectors, sub.selectors...)
}

// cutLast splits s at the last sep outside quotes and brackets.
func cutLast(s string, sep byte) (before, after string, found bool) {
	depth, last := 0, -1
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			switch c {
			case '\\':
				i++
			case quote:
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '(' || c == '{' || c == '[':
			depth++
		case c == ')' || c == '}' || c == ']':
			depth--
		case c == sep && depth == 0:
			last = i
		}
	}
	if last < 0 {
		return s, "", false
	}
	return s[:last], s[last+1:], true
}

// topLevelAlternatives splits a regular expression at the | operators
// outside groups and character classes; v must compile.
func topLevelAlternatives(v string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\\':
			i++
		case '[':
			// Skip the class; a ] right after [ or [^ is a literal.
			i++
			if i < len(v) && v[i] == '^' {
				i++
			}
			if i < len(v) && v[i] == ']' {
				i++
			}
			for i < len(v) && v[i] != ']' {
				if v[i] == '\\' {
					i++
				}
				i++
			}
		case '(':
			depth++
		case ')':
			depth--
		case '|':
			if depth == 0 {
				out = append(out, v[start:i])
				start = i + 1
			}
		}
	}
	return append(out, v[start:])
}

// anchored compiles a label matcher's regular expression the way Prometheus
// does: fully anchored, with . matching newlines. v must compile on its own
// first, so "a)|(b" is not accepted through the wrapping group.
func anchored(v string) (*regexp.Regexp, error) {
	if _, err := regexp.Compile(v); err != nil {
		return nil, err
	}
	return regexp.Compile("^(?s:" + v + ")$")
}

// resolveFamily finds the catalog family a series name belongs to and the
// histogram suffix it carries.
func resolveFamily(name string) (catalog.Family, string, bool) {
	if f, ok := catalog.Lookup(name); ok {
		return f, "", true
	}
	for _, suffix := range histogramSuffixes() {
		base, ok := strings.CutSuffix(name, suffix)
		if !ok {
			continue
		}
		if f, ok := catalog.Lookup(base); ok && f.Kind == catalog.Histogram {
			return f, suffix, true
		}
	}
	return catalog.Family{}, "", false
}

// checkExpr returns the problems of one PromQL expression: every metric it
// reads is a catalog family (09 req 73, spec 09 test 20), every matched
// label belongs to that family, and every matched enumeration value and
// le bound is one the family exports.
func checkExpr(expr string) []string {
	s := &exprScanner{src: expr}
	s.scan()
	out := s.problems
	for _, sel := range s.selectors {
		out = append(out, checkSelector(sel)...)
	}
	return out
}

func checkSelector(sel selector) []string {
	name := sel.name
	if name == "" {
		for _, m := range sel.matchers {
			if m.label == "__name__" && m.op == "=" {
				name = m.value
			}
		}
		if name == "" {
			return []string{"a selector has no metric name"}
		}
	}
	fam, suffix, ok := resolveFamily(name)
	if !ok {
		return []string{fmt.Sprintf("%s is not a metric of the telemetry catalog", name)}
	}
	var out []string
	if fam.Kind == catalog.Histogram && suffix == "" {
		// Classic histograms export no series under the family name.
		out = append(out, fmt.Sprintf("%[1]s is a histogram with no series of that name: select %[1]s_bucket, %[1]s_sum or %[1]s_count", name))
	}
	for _, m := range sel.matchers {
		if m.label == "__name__" || slices.Contains(targetLabels(), m.label) {
			continue
		}
		if m.label == "le" && suffix == "_bucket" {
			out = append(out, checkValues(name, m, boundValues(fam))...)
			continue
		}
		i := slices.IndexFunc(fam.Labels, func(l catalog.Label) bool { return l.Name == m.label })
		if i < 0 {
			out = append(out, fmt.Sprintf("%s has no label %q", name, m.label))
			continue
		}
		if l := fam.Labels[i]; l.Values != nil && !l.Code {
			out = append(out, checkValues(name, m, l.Values)...)
		}
	}
	return out
}

// boundValues returns the le values of a histogram family.
func boundValues(f catalog.Family) []string {
	out := []string{"+Inf"}
	for _, b := range f.Bounds.Seconds() {
		out = append(out, openMetricsFloat(b))
	}
	return out
}

// checkValues checks a matcher value against the values a label can take.
// An equality matcher must name one of them. A regular expression must
// compile, and each of its top-level alternatives must match at least one
// value, so a misspelled value or a bound such as le=~"0.001|9" is caught
// while state_.* passes. Empty values and alternatives select series
// without the label and are not checked, nor are values holding a Grafana
// variable.
func checkValues(name string, m matcher, allowed []string) []string {
	if strings.Contains(m.value, "$") {
		return nil
	}
	where := fmt.Sprintf("%s{%s%s%q}", name, m.label, m.op, m.value)
	want := strings.Join(allowed, ", ")
	switch m.op {
	case "=", "!=":
		if m.value == "" || slices.Contains(allowed, m.value) {
			return nil
		}
		return []string{fmt.Sprintf("%s: %q is not a value of %s (want one of %s)", where, m.value, m.label, want)}
	}
	if _, err := anchored(m.value); err != nil {
		return []string{fmt.Sprintf("%s: invalid regular expression: %v", where, err)}
	}
	var out []string
	for _, alt := range topLevelAlternatives(m.value) {
		re, err := anchored(alt)
		if alt == "" || err != nil || slices.ContainsFunc(allowed, re.MatchString) {
			continue
		}
		verb := "matches no value of"
		if regexp.QuoteMeta(alt) == alt {
			verb = "is not a value of"
		}
		out = append(out, fmt.Sprintf("%s: %q %s %s (want one of %s)", where, alt, verb, m.label, want))
	}
	return out
}
