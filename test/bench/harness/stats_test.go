// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"math"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

func TestMedianAndCV(t *testing.T) {
	tests := []struct {
		name   string
		xs     []float64
		median float64
		cv     float64
	}{
		{"empty", nil, 0, 0},
		{"one", []float64{5}, 5, 0},
		{"odd", []float64{3, 1, 2}, 2, math.Sqrt(2.0/3) / 2},
		{"even", []float64{4, 1, 3, 2}, 2.5, math.Sqrt(1.25) / 2.5},
		{"zero mean", []float64{0, 0}, 0, 0},
	}
	for _, tt := range tests {
		if got := Median(tt.xs); got != tt.median {
			t.Errorf("Median(%v) = %v, want %v", tt.xs, got, tt.median)
		}
		if got := CV(tt.xs); math.Abs(got-tt.cv) > 1e-12 {
			t.Errorf("CV(%v) = %v, want %v", tt.xs, got, tt.cv)
		}
	}
}

// TestCompareView covers the macro p99 gate: a median-of-runs p99 more
// than 5% above the baseline's with four of five pairs slower fails;
// noise above a 2% coefficient of variation is inconclusive.
func TestCompareView(t *testing.T) {
	base := []float64{100, 101, 100, 99, 100}
	tests := []struct {
		name      string
		head      []float64
		base      []float64
		regressed bool
		noisy     bool
		slower    int
	}{
		{"equal", []float64{100, 101, 100, 99, 100}, base, false, false, 0},
		{"6% slower, all pairs", []float64{106, 107, 106, 105, 106}, base, true, false, 5},
		{"6% slower, three pairs", []float64{106, 107, 106, 98, 98}, base, false, false, 3},
		{"5% slower exactly", []float64{105, 105, 105, 105, 105}, base, false, false, 5},
		{"faster", []float64{90, 90, 90, 90, 90}, base, false, false, 0},
		{"noisy baseline", []float64{100, 100, 100, 100, 100}, []float64{90, 110, 95, 105, 100}, false, true, 2},
	}
	for _, tt := range tests {
		v := CompareView(tt.head, tt.base)
		if v.Regressed != tt.regressed || v.Noisy != tt.noisy || v.Slower != tt.slower {
			t.Errorf("%s: CompareView = %+v, want regressed %v noisy %v slower %d", tt.name, v, tt.regressed, tt.noisy, tt.slower)
		}
	}
}

func TestABVerdict(t *testing.T) {
	ok := ViewComparison{HeadMedian: 100, BaseMedian: 100}
	bad := ViewComparison{HeadMedian: 110, BaseMedian: 100, Regressed: true}
	noisy := ViewComparison{HeadMedian: 100, BaseMedian: 100, Noisy: true}
	unmeasured := ViewComparison{Noisy: true}
	tests := []struct {
		name       string
		head, base int
		views      []ViewComparison
		want       string
	}{
		{"pass", 5, 5, []ViewComparison{ok, ok}, benchrecord.ABPass},
		{"external regressed", 5, 5, []ViewComparison{bad, ok}, benchrecord.ABFail},
		{"internal regressed", 5, 5, []ViewComparison{ok, bad}, benchrecord.ABFail},
		{"noise wins over regression", 5, 5, []ViewComparison{bad, noisy}, benchrecord.ABInconclusive},
		{"four valid head runs", 4, 5, []ViewComparison{ok, ok}, benchrecord.ABInconclusive},
		{"four valid baseline runs", 5, 4, []ViewComparison{ok, ok}, benchrecord.ABInconclusive},
		{"an unmeasured view never decides", 5, 5, []ViewComparison{ok, unmeasured}, benchrecord.ABPass},
	}
	for _, tt := range tests {
		if got := ABVerdict(tt.head, tt.base, tt.views...); got != tt.want {
			t.Errorf("%s: ABVerdict = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func hist(t *testing.T, ds ...time.Duration) *loadgen.Histogram {
	t.Helper()
	h, err := Pool()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		h.Record(d)
	}
	return h
}

func TestPoolAndPercentiles(t *testing.T) {
	a, b := hist(t, time.Millisecond, 2*time.Millisecond), hist(t, 3*time.Millisecond)
	p, err := Pool(a, nil, b)
	if err != nil {
		t.Fatal(err)
	}
	if p.Count() != 3 {
		t.Errorf("Pool count = %d, want 3", p.Count())
	}
	if got := Percentiles(nil); got != (benchrecord.Percentiles{}) {
		t.Errorf("Percentiles(nil) = %+v, want zero", got)
	}
	if got := Percentiles(p); got.Max < 3*time.Millisecond || got.P50 < time.Millisecond {
		t.Errorf("Percentiles = %+v", got)
	}
}

func TestDurationString(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{1500 * time.Millisecond, "1.50s"},
		{840 * time.Microsecond, "840µs"},
		{1234 * time.Microsecond, "1.23ms"},
		{17 * time.Microsecond, "17.0µs"},
		{500, "500ns"},
		{0, "0ns"},
	}
	for _, tt := range tests {
		if got := durationString(tt.d); got != tt.want {
			t.Errorf("durationString(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// TestBudgetEvaluate covers every budget kind, its view and its skip.
func TestBudgetEvaluate(t *testing.T) {
	ms := func(n float64) float64 { return n * float64(time.Millisecond) }
	m := &Measures{
		External: hist(t, 2*time.Millisecond), Internal: hist(t, 500*time.Microsecond),
		CPUPerRequest: 90 * time.Microsecond, GCShare: 0.05, RSSLoad: 300 << 20, Saturation: 52000,
		Metrics: map[string]promtext.Histogram{
			MetricKey("ruralz_state_call_duration_seconds", map[string]string{"op": "gcra", "zone": "same"}): {
				Count: 100, Buckets: []promtext.Bucket{{UpperBound: 0.0005, Count: 90}, {UpperBound: 0.001, Count: 100}, {UpperBound: math.Inf(1), Count: 100}},
			},
		},
	}
	tests := []struct {
		b    Budget
		want string
	}{
		{Budget{Kind: InternalP99, Limit: ms(1)}, "pass"},
		{Budget{Kind: InternalP50, Limit: float64(150 * time.Microsecond)}, "fail"},
		{Budget{Kind: ExternalP99, Limit: ms(1)}, "fail"},
		{Budget{Kind: MetricP99, Limit: ms(1), Metric: "ruralz_state_call_duration_seconds", Labels: map[string]string{"op": "gcra", "zone": "same"}}, "pass"},
		{Budget{Kind: MetricP99, Limit: ms(1), Metric: "ruralz_state_call_duration_seconds", Labels: map[string]string{"op": "gcra", "zone": "cross"}}, "skipped"},
		{Budget{Kind: MinSaturation, Limit: 50000}, "pass"},
		{Budget{Kind: MaxCPUPer10k, Limit: 0.8}, "fail"},
		{Budget{Kind: MaxGCShare, Limit: 0.10}, "pass"},
		{Budget{Kind: MaxLoadedRSS, Limit: 256 << 20}, "fail"},
		{Budget{Kind: BudgetKind(99)}, "fail"},
	}
	for i, tt := range tests {
		tt.b.ID, tt.b.Value = "b", "v"
		if got := tt.b.Evaluate(m); got.Verdict != tt.want || (got.Verdict != "skipped" && got.Result == "") {
			t.Errorf("case %d kind %d: Evaluate = %+v, want %s with a result", i, tt.b.Kind, got, tt.want)
		}
	}
	empty := &Measures{}
	if got := (Budget{ID: "PB-2", Value: "v", Kind: InternalP99, Limit: 1}).Evaluate(empty); got.Verdict != "skipped" {
		t.Errorf("an unmeasured view = %+v, want skipped", got)
	}
	if got := (Budget{ID: "PB-4", Value: "v", Kind: MinSaturation, Limit: 1}).Evaluate(empty); got.Verdict != "fail" {
		t.Errorf("a ladder without saturation = %+v, want fail", got)
	}
	if !AnyFailed([]benchrecord.BudgetResult{{Verdict: "pass"}, {Verdict: "fail"}}) || AnyFailed(nil) {
		t.Error("AnyFailed is wrong")
	}
}

func TestHistogramDeltaAndMerge(t *testing.T) {
	before := promtext.Histogram{Count: 10, Sum: 1, Buckets: []promtext.Bucket{{UpperBound: 1, Count: 10}, {UpperBound: math.Inf(1), Count: 10}}}
	after := promtext.Histogram{Count: 25, Sum: 3, Buckets: []promtext.Bucket{{UpperBound: 0.5, Count: 4}, {UpperBound: 1, Count: 20}, {UpperBound: math.Inf(1), Count: 25}}}
	d := histogramDelta(before, after)
	if d.Count != 15 || d.Buckets[0].Count != 4 || d.Buckets[1].Count != 10 || d.Buckets[2].Count != 15 {
		t.Errorf("histogramDelta = %+v", d)
	}
	m := mergeHistograms(d, d)
	if m.Count != 30 || len(m.Buckets) != 3 || m.Buckets[0].UpperBound != 0.5 || m.Buckets[2].Count != 30 {
		t.Errorf("mergeHistograms = %+v", m)
	}
}
