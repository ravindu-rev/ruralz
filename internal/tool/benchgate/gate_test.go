// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
)

// runs returns n samples of the given allocs/op.
func runs(n int, allocs float64) []sample {
	out := make([]sample, n)
	for i := range out {
		out[i] = sample{allocs: allocs, bytes: allocs * 16}
	}
	return out
}

func series(allocs ...float64) []sample {
	out := make([]sample, len(allocs))
	for i, a := range allocs {
		out[i] = sample{allocs: a}
	}
	return out
}

func ptr(v int64) *int64 { return &v }

// TestEvaluate covers the decision rules of 11 req 64 and 65 (test plan
// item 1): 30 to 31 fails (3.3%), 100 to 103 passes (exactly 3%), 100 to 104
// fails, a cap exceeded fails without a regression, a new benchmark passes
// and is reported, a removed benchmark fails, and the four-of-five pair rule.
func TestEvaluate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		base, head []sample
		maxAllocs  *int64
		threshold  float64
		haveBase   bool
		want       verdict
	}{
		{"PB-8 one extra allocation at 30 fails", runs(10, 30), runs(10, 31), nil, 0.03, true, verdictRegression},
		{"exactly 3 percent passes", runs(10, 100), runs(10, 103), nil, 0.03, true, verdictPass},
		{"above 3 percent fails", runs(10, 100), runs(10, 104), nil, 0.03, true, verdictRegression},
		{"fewer allocations pass", runs(10, 30), runs(10, 20), nil, 0.03, true, verdictPass},
		{"equal passes", runs(10, 30), runs(10, 30), nil, 0.03, true, verdictPass},
		{"zero to one fails", runs(10, 0), runs(10, 1), nil, 0.03, true, verdictRegression},
		{"zero to zero passes", runs(10, 0), runs(10, 0), nil, 0.03, true, verdictPass},
		{"cap exceeded without regression fails", runs(10, 31), runs(10, 31), ptr(30), 0.03, true, verdictCap},
		{"at the cap passes", runs(10, 30), runs(10, 30), ptr(30), 0.03, true, verdictPass},
		{"regression and cap", runs(10, 30), runs(10, 40), ptr(30), 0.03, true, verdictRegressionCap},
		{"new benchmark passes", nil, runs(10, 12), nil, 0.03, true, verdictNew},
		{"new benchmark above its cap fails", nil, runs(10, 31), ptr(30), 0.03, true, verdictCap},
		{"removed benchmark fails", runs(10, 12), nil, nil, 0.03, true, verdictRemoved},
		{"no base checks the cap only", nil, runs(10, 500), nil, 0.03, false, verdictHeadOnly},
		{"no base above the cap fails", nil, runs(10, 31), ptr(30), 0.03, false, verdictCap},
		{"four of five pairs slower fails", series(100, 100, 100, 100, 100), series(104, 104, 104, 104, 100), nil, 0.03, true, verdictRegression},
		{"three of five pairs slower passes", series(100, 100, 100, 100, 100), series(104, 104, 104, 100, 100), nil, 0.03, true, verdictPass},
		{"eight of ten pairs slower fails", runs(10, 100), series(104, 104, 104, 104, 104, 104, 104, 104, 100, 100), nil, 0.03, true, verdictRegression},
		{"seven of ten pairs slower passes", runs(10, 100), series(104, 104, 104, 104, 104, 104, 104, 100, 100, 100), nil, 0.03, true, verdictPass},
		{"zero threshold fails any growth", runs(10, 100), runs(10, 101), nil, 0, true, verdictRegression},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, head := map[string][]sample{}, map[string][]sample{}
			if tc.base != nil {
				base["BenchmarkX"] = tc.base
			}
			if tc.head != nil {
				head["BenchmarkX"] = tc.head
			}
			rs := evaluate("./p", base, head, tc.maxAllocs, tc.threshold, tc.haveBase)
			if len(rs) != 1 {
				t.Fatalf("got %d results, want 1", len(rs))
			}
			if rs[0].verdict != tc.want {
				t.Errorf("verdict = %v, want %v (base %v, head %v, pairs %d/%d)",
					rs[0].verdict, tc.want, rs[0].baseAllocs, rs[0].headAllocs, rs[0].slower, rs[0].pairs)
			}
			if rs[0].verdict.failed() != (tc.want == verdictRegression || tc.want == verdictCap || tc.want == verdictRegressionCap || tc.want == verdictRemoved) {
				t.Errorf("failed() = %v for %v", rs[0].verdict.failed(), rs[0].verdict)
			}
		})
	}
}

// TestEvaluateOrderAndTable checks the sorted union of names and the
// report columns (11 req 64 "a table per benchmark").
func TestEvaluateOrderAndTable(t *testing.T) {
	base := map[string][]sample{"BenchmarkB": runs(2, 30), "BenchmarkGone": runs(2, 1)}
	head := map[string][]sample{"BenchmarkB": runs(2, 31), "BenchmarkA": runs(2, 5)}
	rs := evaluate("./internal/gateway/router", base, head, ptr(30), 0.03, true)
	var names []string
	for _, r := range rs {
		names = append(names, r.name)
	}
	if got := strings.Join(names, ","); got != "BenchmarkA,BenchmarkB,BenchmarkGone" {
		t.Fatalf("names = %s", got)
	}
	var b bytes.Buffer
	if err := writeTable(&b, rs); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"package", "slower pairs",
		"BenchmarkA", "new (no base)",
		"BenchmarkB", "+3.3%", "2/2", "FAIL: regression, above cap",
		"BenchmarkGone", "FAIL: removed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	b.Reset()
	if err := writeMarkdown(&b, "head vs base", rs, "footer text"); err != nil {
		t.Fatal(err)
	}
	md := b.String()
	for _, want := range []string{"### alloc/op gate", "head vs base", "| `./internal/gateway/router` | `BenchmarkB` | 30 | 31 | +3.3% |", "footer text"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}

func TestFormatting(t *testing.T) {
	if got := fmtDelta(result{baseAllocs: 0, headAllocs: 2}); got != "+inf" {
		t.Errorf("fmtDelta(0 to 2) = %s", got)
	}
	if got := fmtDelta(result{baseAllocs: 0, headAllocs: 0}); got != "+0.0%" {
		t.Errorf("fmtDelta(0 to 0) = %s", got)
	}
	if got := fmtDelta(result{baseAllocs: 10, headAllocs: 5}); got != "-50.0%" {
		t.Errorf("fmtDelta(10 to 5) = %s", got)
	}
	if got := fmtCap(nil); got != "-" {
		t.Errorf("fmtCap(nil) = %s", got)
	}
	if got := verdict(99).String(); got != "verdict(99)" {
		t.Errorf("unknown verdict = %s", got)
	}
	for v := verdictPass; v <= verdictRemoved; v++ {
		if strings.HasPrefix(v.String(), "verdict(") {
			t.Errorf("verdict %d has no name", int(v))
		}
	}
}
