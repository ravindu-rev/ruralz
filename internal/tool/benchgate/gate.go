// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"text/tabwriter"
)

// verdict is the outcome for one benchmark.
type verdict int

const (
	// verdictPass: within the threshold and the cap.
	verdictPass verdict = iota
	// verdictNew: no base counterpart; reported and passing (11 req 65).
	verdictNew
	// verdictHeadOnly: no base configured; only the cap was checked.
	verdictHeadOnly
	// verdictRegression: median allocs/op above the base by more than the
	// threshold, with at least four of five interleaved pairs slower.
	verdictRegression
	// verdictCap: median allocs/op above the configured absolute cap.
	verdictCap
	// verdictRegressionCap: both of the above.
	verdictRegressionCap
	// verdictRemoved: the base has the benchmark and the head does not; a
	// removal passes only by changing allocgate.json so that no entry
	// matches the benchmark any more (11 req 65).
	verdictRemoved
)

// failed reports whether v fails the gate.
func (v verdict) failed() bool {
	return v == verdictRegression || v == verdictCap || v == verdictRegressionCap || v == verdictRemoved
}

func (v verdict) String() string {
	switch v {
	case verdictPass:
		return "pass"
	case verdictNew:
		return "new (no base)"
	case verdictHeadOnly:
		return "pass (no base)"
	case verdictRegression:
		return "FAIL: regression"
	case verdictCap:
		return "FAIL: above cap"
	case verdictRegressionCap:
		return "FAIL: regression, above cap"
	case verdictRemoved:
		return "FAIL: removed"
	default:
		return fmt.Sprintf("verdict(%d)", int(v))
	}
}

// result is the comparison of one benchmark.
type result struct {
	pkg, name              string
	baseAllocs, headAllocs float64 // medians; NaN when that side has no samples
	baseBytes, headBytes   float64 // medians; NaN when that side has no samples
	slower, pairs          int     // pairs whose head allocs/op exceeds the base
	maxAllocs              *int64
	verdict                verdict
}

// exceeds reports whether head is above base by more than threshold,
// relatively; the small absolute slack keeps exactly-at-threshold values
// (100 to 103 at 3%) passing despite floating-point rounding.
func exceeds(head, base, threshold float64) bool {
	return head > base+base*threshold+1e-9
}

// fourOfFive reports whether at least four of five pairs, rounded up, are
// slower (docs/architecture/12-performance-budgets-and-benchmarking.md,
// "Statistics and runners").
func fourOfFive(slower, pairs int) bool {
	return pairs > 0 && slower*5 >= pairs*4
}

func allocsOf(s []sample) []float64 {
	out := make([]float64, len(s))
	for i, x := range s {
		out[i] = x.allocs
	}
	return out
}

func bytesOf(s []sample) []float64 {
	out := make([]float64, len(s))
	for i, x := range s {
		out[i] = x.bytes
	}
	return out
}

func medianOrNaN(xs []float64) float64 {
	if m, ok := median(xs); ok {
		return m
	}
	return math.NaN()
}

// evaluate compares the samples of one gated set. base and head hold the
// samples of each benchmark in run order, so base[i] and head[i] form the
// i-th interleaved pair. haveBase is false when no base was configured;
// then only the caps apply.
func evaluate(pkg string, base, head map[string][]sample, maxAllocs *int64, threshold float64, haveBase bool) []result {
	names := make([]string, 0, len(base)+len(head))
	for n := range base {
		names = append(names, n)
	}
	for n := range head {
		if _, ok := base[n]; !ok {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	out := make([]result, 0, len(names))
	for _, n := range names {
		b, h := base[n], head[n]
		r := result{
			pkg: pkg, name: n, maxAllocs: maxAllocs,
			baseAllocs: medianOrNaN(allocsOf(b)), headAllocs: medianOrNaN(allocsOf(h)),
			baseBytes: medianOrNaN(bytesOf(b)), headBytes: medianOrNaN(bytesOf(h)),
		}
		r.pairs = min(len(b), len(h))
		for i := range r.pairs {
			if h[i].allocs > b[i].allocs {
				r.slower++
			}
		}
		capped := len(h) > 0 && maxAllocs != nil && r.headAllocs > float64(*maxAllocs)
		switch {
		case len(h) == 0:
			r.verdict = verdictRemoved
		case !haveBase:
			r.verdict = verdictHeadOnly
			if capped {
				r.verdict = verdictCap
			}
		case len(b) == 0:
			r.verdict = verdictNew
			if capped {
				r.verdict = verdictCap
			}
		default:
			regressed := exceeds(r.headAllocs, r.baseAllocs, threshold) && fourOfFive(r.slower, r.pairs)
			switch {
			case regressed && capped:
				r.verdict = verdictRegressionCap
			case regressed:
				r.verdict = verdictRegression
			case capped:
				r.verdict = verdictCap
			default:
				r.verdict = verdictPass
			}
		}
		out = append(out, r)
	}
	return out
}

func fmtNum(x float64) string {
	if math.IsNaN(x) {
		return "-"
	}
	return fmt.Sprintf("%g", x)
}

func fmtDelta(r result) string {
	switch {
	case math.IsNaN(r.baseAllocs) || math.IsNaN(r.headAllocs):
		return "-"
	case r.baseAllocs == 0 && r.headAllocs == 0:
		return "+0.0%"
	case r.baseAllocs == 0:
		return "+inf"
	default:
		return fmt.Sprintf("%+.1f%%", (r.headAllocs-r.baseAllocs)/r.baseAllocs*100)
	}
}

func fmtCap(c *int64) string {
	if c == nil {
		return "-"
	}
	return fmt.Sprint(*c)
}

func fmtPairs(r result) string {
	if r.pairs == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", r.slower, r.pairs)
}

// writeTable prints the per-benchmark table (11 req 64).
func writeTable(w io.Writer, rs []result) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "package\tbenchmark\tbase allocs/op\thead allocs/op\tdelta\tbase B/op\thead B/op\tslower pairs\tcap\tresult")
	for _, r := range rs {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.pkg, r.name, fmtNum(r.baseAllocs), fmtNum(r.headAllocs), fmtDelta(r),
			fmtNum(r.baseBytes), fmtNum(r.headBytes), fmtPairs(r), fmtCap(r.maxAllocs), r.verdict)
	}
	return tw.Flush()
}

// writeMarkdown writes the job summary: a heading, the run description,
// the table and an optional footer.
func writeMarkdown(w io.Writer, header string, rs []result, footer string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "### alloc/op gate\n\n%s\n\n", header)
	b.WriteString("| Package | Benchmark | Base allocs/op | Head allocs/op | Delta | Base B/op | Head B/op | Slower pairs | Cap | Result |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rs {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			r.pkg, r.name, fmtNum(r.baseAllocs), fmtNum(r.headAllocs), fmtDelta(r),
			fmtNum(r.baseBytes), fmtNum(r.headBytes), fmtPairs(r), fmtCap(r.maxAllocs), r.verdict)
	}
	if footer != "" {
		fmt.Fprintf(&b, "\n%s\n", footer)
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}
