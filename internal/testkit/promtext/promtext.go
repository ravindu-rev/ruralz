// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package promtext scrapes and parses the Prometheus text exposition format
// (version 0.0.4, plus the OpenMetrics 1.0 additions a scrape may meet:
// "# UNIT", "# EOF", exemplars, _created samples and quoted UTF-8 names)
// so integration, end-to-end and chaos tests can assert counters, gauges
// and histogram quantiles of a Node's /metrics (11 section 3, H 49).
//
// The package is standard library only: /metrics exposition belongs to
// internal/telemetry, and the parser is the independent reader tests use
// to check it.
package promtext

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Metric types as written after "# TYPE".
const (
	TypeCounter        = "counter"
	TypeGauge          = "gauge"
	TypeHistogram      = "histogram"
	TypeSummary        = "summary"
	TypeUntyped        = "untyped"
	TypeUnknown        = "unknown"
	TypeInfo           = "info"
	TypeStateSet       = "stateset"
	TypeGaugeHistogram = "gaugehistogram"
)

// MaxScrapeBytes bounds a scraped body.
const MaxScrapeBytes = 64 << 20

// Labels is a label set. Matching a sample against Labels requires every
// given name to have the given value; a value "" matches an absent label.
type Labels map[string]string

// L builds Labels from name, value pairs; an odd trailing name is ignored.
func L(pairs ...string) Labels {
	out := make(Labels, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i]] = pairs[i+1]
	}
	return out
}

// String formats the label set in exposition syntax with sorted names.
func (l Labels) String() string {
	names := make([]string, 0, len(l))
	for n := range l {
		names = append(names, n)
	}
	slices.Sort(names)
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(l[n]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// matches reports whether l has every pair of want.
func (l Labels) matches(want Labels) bool {
	for n, v := range want {
		if l[n] != v {
			return false
		}
	}
	return true
}

// Sample is one exposition line.
type Sample struct {
	// Name is the sample name, including any _bucket, _sum, _count,
	// _total or _created suffix.
	Name string
	// Labels holds the sample's labels (never nil).
	Labels Labels
	// Value is the sample value; +Inf, -Inf and NaN are allowed.
	Value float64
	// Timestamp is the optional timestamp in milliseconds; HasTimestamp
	// tells whether one was written.
	Timestamp    int64
	HasTimestamp bool
}

// Family is one metric family: the samples following a "# TYPE" (or, for
// untyped samples, sharing one name).
type Family struct {
	Name    string
	Type    string
	Help    string
	Unit    string
	Samples []Sample
}

// Set is a parsed exposition.
type Set struct {
	// Families holds every family by name.
	Families map[string]*Family
	// bySample indexes samples by sample name.
	bySample map[string][]Sample
}

// Parse reads a text exposition. Errors name the line.
func Parse(r io.Reader) (Set, error) {
	p := parser{set: Set{Families: map[string]*Family{}, bySample: map[string][]Sample{}}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		p.line++
		line := strings.TrimSuffix(sc.Text(), "\r")
		if p.eof {
			if strings.TrimSpace(line) != "" {
				return Set{}, p.errorf("content after # EOF")
			}
			continue
		}
		if err := p.parseLine(line); err != nil {
			return Set{}, err
		}
	}
	if err := sc.Err(); err != nil {
		return Set{}, fmt.Errorf("promtext: read: %w", err)
	}
	return p.set, nil
}

// ParseBytes parses an exposition held in memory.
func ParseBytes(b []byte) (Set, error) { return Parse(bytes.NewReader(b)) }

// Scrape fetches url with GET and parses the body. header is added to the
// request (for example Authorization for the admin metrics token); the
// request asks for text format 0.0.4. A nil client uses a client with a
// 10 s timeout.
func Scrape(ctx context.Context, client *http.Client, url string, header http.Header) (Set, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return Set{}, fmt.Errorf("promtext: scrape %s: %w", url, err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4;q=1,*/*;q=0.1")
	resp, err := client.Do(req)
	if err != nil {
		return Set{}, fmt.Errorf("promtext: scrape %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxScrapeBytes+1))
	if err != nil {
		return Set{}, fmt.Errorf("promtext: scrape %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return Set{}, fmt.Errorf("promtext: scrape %s: status %d: %s", url, resp.StatusCode, bytes.TrimSpace(body[:min(len(body), 512)]))
	}
	if len(body) > MaxScrapeBytes {
		return Set{}, fmt.Errorf("promtext: scrape %s: body exceeds %d bytes", url, MaxScrapeBytes)
	}
	set, err := ParseBytes(body)
	if err != nil {
		return Set{}, fmt.Errorf("promtext: scrape %s: %w", url, err)
	}
	return set, nil
}

// Names returns every family name, sorted.
func (s Set) Names() []string {
	out := make([]string, 0, len(s.Families))
	for n := range s.Families {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Family returns the family named name.
func (s Set) Family(name string) (*Family, bool) {
	f, ok := s.Families[name]
	return f, ok
}

// Samples returns the samples named name (a sample name, such as
// "x_bucket" or "x_total") whose labels match, in exposition order.
func (s Set) Samples(name string, match Labels) []Sample {
	var out []Sample
	for _, smp := range s.bySample[name] {
		if smp.Labels.matches(match) {
			out = append(out, smp)
		}
	}
	return out
}

// Has reports whether at least one sample named name matches.
func (s Set) Has(name string, match Labels) bool { return len(s.Samples(name, match)) > 0 }

// Value returns the value of the one sample named name whose labels match;
// no match or more than one is an error.
func (s Set) Value(name string, match Labels) (float64, error) {
	got := s.Samples(name, match)
	switch len(got) {
	case 0:
		return 0, fmt.Errorf("promtext: no sample %s%s", name, match)
	case 1:
		return got[0].Value, nil
	}
	return 0, fmt.Errorf("promtext: %d samples match %s%s", len(got), name, match)
}

// Sum adds the values of every matching sample named name; no match sums
// to 0 (series pre-created at 0 and absent series read alike).
func (s Set) Sum(name string, match Labels) float64 {
	var sum float64
	for _, smp := range s.Samples(name, match) {
		sum += smp.Value
	}
	return sum
}

// Delta is after.Sum(name, match) - before.Sum(name, match), the counter
// increase between two scrapes.
func Delta(before, after Set, name string, match Labels) float64 {
	return after.Sum(name, match) - before.Sum(name, match)
}

// Bucket is one cumulative histogram bucket.
type Bucket struct {
	UpperBound float64
	Count      float64
}

// Histogram is a classic histogram, aggregated over every matching series.
type Histogram struct {
	// Buckets are sorted by upper bound; the last one is +Inf.
	Buckets []Bucket
	Count   float64
	Sum     float64
}

// Histogram aggregates the classic histogram family name (without the
// _bucket suffix): the buckets of every series whose labels match are added
// per upper bound, as are _sum and _count.
func (s Set) Histogram(name string, match Labels) (Histogram, error) {
	per := map[float64]float64{}
	series := 0
	for _, smp := range s.Samples(name+"_bucket", match) {
		le, ok := smp.Labels["le"]
		if !ok {
			return Histogram{}, fmt.Errorf("promtext: %s_bucket%s has no le label", name, smp.Labels)
		}
		ub, err := parseFloat(le)
		if err != nil {
			return Histogram{}, fmt.Errorf("promtext: %s_bucket le=%q: %w", name, le, err)
		}
		per[ub] += smp.Value
		if math.IsInf(ub, 1) {
			series++
		}
	}
	if len(per) == 0 {
		return Histogram{}, fmt.Errorf("promtext: no histogram %s%s", name, match)
	}
	if series == 0 {
		return Histogram{}, fmt.Errorf("promtext: histogram %s%s has no +Inf bucket", name, match)
	}
	h := Histogram{Count: s.Sum(name+"_count", match), Sum: s.Sum(name+"_sum", match)}
	for ub, c := range per {
		h.Buckets = append(h.Buckets, Bucket{UpperBound: ub, Count: c})
	}
	sort.Slice(h.Buckets, func(i, j int) bool { return h.Buckets[i].UpperBound < h.Buckets[j].UpperBound })
	return h, nil
}

// Quantile estimates the q-quantile by linear interpolation inside the
// bucket holding the rank, like PromQL histogram_quantile: q below 0 is
// -Inf and above 1 is +Inf; a rank in the +Inf bucket returns the highest
// finite upper bound; the lowest bucket starts at 0 when its upper bound
// is positive. With no observations the result is NaN.
func (h Histogram) Quantile(q float64) float64 {
	switch {
	case math.IsNaN(q):
		return math.NaN()
	case q < 0:
		return math.Inf(-1)
	case q > 1:
		return math.Inf(1)
	}
	b := slices.Clone(h.Buckets)
	if len(b) < 2 || !math.IsInf(b[len(b)-1].UpperBound, 1) {
		return math.NaN()
	}
	// Force monotonic counts, as PromQL does for scrapes racing updates.
	for i := 1; i < len(b); i++ {
		b[i].Count = max(b[i].Count, b[i-1].Count)
	}
	total := b[len(b)-1].Count
	if total == 0 {
		return math.NaN()
	}
	rank := q * total
	i := sort.Search(len(b)-1, func(i int) bool { return b[i].Count >= rank })
	if i == len(b)-1 {
		return b[len(b)-2].UpperBound
	}
	if i == 0 && b[0].UpperBound <= 0 {
		return b[0].UpperBound
	}
	start, end, count := 0.0, b[i].UpperBound, b[i].Count
	if i > 0 {
		start = b[i-1].UpperBound
		count -= b[i-1].Count
		rank -= b[i-1].Count
	}
	if count == 0 {
		return end
	}
	return start + (end-start)*(rank/count)
}

// parser holds the state of one Parse call.
type parser struct {
	set  Set
	line int
	eof  bool
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("promtext: line %d: %s", p.line, fmt.Sprintf(format, args...))
}

func (p *parser) parseLine(line string) error {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" {
		return nil
	}
	if trimmed[0] == '#' {
		return p.parseComment(trimmed)
	}
	return p.parseSample(trimmed)
}

func (p *parser) parseComment(line string) error {
	fields := strings.Fields(line)
	if len(fields) == 2 && fields[1] == "EOF" {
		p.eof = true
		return nil
	}
	if len(fields) < 3 || (fields[1] != "HELP" && fields[1] != "TYPE" && fields[1] != "UNIT") {
		return nil // an ordinary comment
	}
	// Re-split so HELP keeps its inner spacing.
	rest := strings.TrimLeft(strings.TrimPrefix(strings.TrimLeft(line[1:], " \t"), fields[1]), " \t")
	name, text, err := p.metricName(rest)
	if err != nil {
		return err
	}
	text = strings.TrimLeft(text, " \t")
	f := p.family(name)
	switch fields[1] {
	case "HELP":
		f.Help = unescapeHelp(text)
	case "UNIT":
		f.Unit = strings.TrimSpace(text)
	default:
		typ := strings.TrimSpace(text)
		switch typ {
		case TypeCounter, TypeGauge, TypeHistogram, TypeSummary, TypeUntyped,
			TypeUnknown, TypeInfo, TypeStateSet, TypeGaugeHistogram:
		default:
			return p.errorf("unknown type %q for %s", typ, name)
		}
		if f.Type != "" {
			return p.errorf("second TYPE line for %s", name)
		}
		if len(f.Samples) > 0 {
			return p.errorf("TYPE line for %s after its samples", name)
		}
		f.Type = typ
	}
	return nil
}

// metricName reads a metric name (plain or double-quoted) at the start of
// s and returns it and the rest.
func (p *parser) metricName(s string) (name, rest string, err error) {
	if strings.HasPrefix(s, `"`) {
		name, n, err := readQuoted(s)
		if err != nil {
			return "", "", p.errorf("%v", err)
		}
		return name, s[n:], nil
	}
	i := 0
	for i < len(s) && isNameByte(s[i], i == 0) {
		i++
	}
	if i == 0 {
		return "", "", p.errorf("missing metric name")
	}
	return s[:i], s[i:], nil
}

func (p *parser) parseSample(line string) error {
	smp := Sample{Labels: Labels{}}
	rest := line
	if !strings.HasPrefix(rest, "{") {
		name, r, err := p.metricName(rest)
		if err != nil {
			return err
		}
		smp.Name, rest = name, r
	}
	if strings.HasPrefix(rest, "{") {
		r, err := p.parseLabels(rest[1:], &smp)
		if err != nil {
			return err
		}
		rest = r
	}
	if smp.Name == "" {
		return p.errorf("sample without a metric name")
	}
	// Drop an OpenMetrics exemplar.
	if i := strings.Index(rest, " # "); i >= 0 {
		rest = rest[:i]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 || len(fields) > 2 || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return p.errorf("want value [timestamp] after %s", smp.Name)
	}
	v, err := parseFloat(fields[0])
	if err != nil {
		return p.errorf("value %q: %v", fields[0], err)
	}
	smp.Value = v
	if len(fields) == 2 {
		ts, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			f, ferr := strconv.ParseFloat(fields[1], 64)
			if ferr != nil {
				return p.errorf("timestamp %q: %v", fields[1], err)
			}
			ts = int64(f * 1000) // OpenMetrics timestamps are seconds
		}
		smp.Timestamp, smp.HasTimestamp = ts, true
	}
	f := p.familyOf(smp.Name)
	f.Samples = append(f.Samples, smp)
	p.set.bySample[smp.Name] = append(p.set.bySample[smp.Name], smp)
	return nil
}

// parseLabels reads label pairs after '{' up to the closing '}'. A quoted
// name without "=" is the metric name (UTF-8 exposition).
func (p *parser) parseLabels(s string, smp *Sample) (string, error) {
	for {
		s = strings.TrimLeft(s, " \t")
		if strings.HasPrefix(s, "}") {
			return s[1:], nil
		}
		var name string
		if strings.HasPrefix(s, `"`) {
			q, n, err := readQuoted(s)
			if err != nil {
				return "", p.errorf("%v", err)
			}
			name, s = q, strings.TrimLeft(s[n:], " \t")
			if !strings.HasPrefix(s, "=") {
				if smp.Name != "" {
					return "", p.errorf("second metric name %q", q)
				}
				smp.Name = name
				s = strings.TrimPrefix(s, ",")
				continue
			}
		} else {
			i := 0
			for i < len(s) && isLabelByte(s[i], i == 0) {
				i++
			}
			if i == 0 {
				return "", p.errorf("bad label name at %q", truncate(s))
			}
			name, s = s[:i], strings.TrimLeft(s[i:], " \t")
		}
		if !strings.HasPrefix(s, "=") {
			return "", p.errorf("missing = after label %s", name)
		}
		s = strings.TrimLeft(s[1:], " \t")
		if !strings.HasPrefix(s, `"`) {
			return "", p.errorf("label %s: value not quoted", name)
		}
		val, n, err := readQuoted(s)
		if err != nil {
			return "", p.errorf("label %s: %v", name, err)
		}
		if _, dup := smp.Labels[name]; dup {
			return "", p.errorf("duplicate label %s", name)
		}
		smp.Labels[name] = val
		s = strings.TrimLeft(s[n:], " \t")
		switch {
		case strings.HasPrefix(s, ","):
			s = s[1:]
		case strings.HasPrefix(s, "}"):
		default:
			return "", p.errorf("want , or } after label %s", name)
		}
	}
}

// family returns the family named name, creating it.
func (p *parser) family(name string) *Family {
	f, ok := p.set.Families[name]
	if !ok {
		f = &Family{Name: name}
		p.set.Families[name] = f
	}
	return f
}

// suffixTypes lists the sample-name suffixes each family type accepts.
func suffixTypes(suffix string) []string {
	switch suffix {
	case "_bucket":
		return []string{TypeHistogram, TypeGaugeHistogram}
	case "_count", "_sum":
		return []string{TypeHistogram, TypeSummary}
	case "_gcount", "_gsum":
		return []string{TypeGaugeHistogram}
	case "_created":
		return []string{TypeHistogram, TypeSummary, TypeCounter}
	case "_total":
		return []string{TypeCounter}
	case "_info":
		return []string{TypeInfo}
	}
	return nil
}

// familyOf returns the family a sample belongs to: a declared family of the
// same name, else a declared family whose type accepts the name's suffix,
// else an untyped family of the sample's own name.
func (p *parser) familyOf(sample string) *Family {
	if f, ok := p.set.Families[sample]; ok {
		if f.Type == "" {
			f.Type = TypeUntyped
		}
		return f
	}
	for _, suffix := range []string{"_bucket", "_count", "_sum", "_gcount", "_gsum", "_created", "_total", "_info"} {
		base, ok := strings.CutSuffix(sample, suffix)
		if !ok {
			continue
		}
		if f, ok := p.set.Families[base]; ok && slices.Contains(suffixTypes(suffix), f.Type) {
			return f
		}
	}
	f := p.family(sample)
	if f.Type == "" {
		f.Type = TypeUntyped
	}
	return f
}

// readQuoted reads a double-quoted string with \\, \" and \n escapes at
// the start of s and returns it and the bytes consumed.
func readQuoted(s string) (string, int, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			return b.String(), i + 1, nil
		case '\\':
			if i+1 >= len(s) {
				return "", 0, errors.New("unterminated escape")
			}
			i++
			switch s[i] {
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			case 'n':
				b.WriteByte('\n')
			default:
				return "", 0, fmt.Errorf("unknown escape \\%c", s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, errors.New("unterminated quoted string")
}

// unescapeHelp undoes the \\ and \n escapes of HELP text.
func unescapeHelp(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseFloat parses a sample value, accepting the exposition spellings
// +Inf, -Inf and NaN.
func parseFloat(s string) (float64, error) {
	switch s {
	case "+Inf", "Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN":
		return math.NaN(), nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse float: %w", err)
	}
	return v, nil
}

func isNameByte(c byte, first bool) bool {
	return c == '_' || c == ':' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || (!first && '0' <= c && c <= '9')
}

func isLabelByte(c byte, first bool) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || (!first && '0' <= c && c <= '9')
}

func truncate(s string) string {
	if len(s) > 20 {
		return s[:20] + "..."
	}
	return s
}
