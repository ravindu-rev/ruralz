// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// BudgetKind is what a budget measures.
type BudgetKind int

// Budget kinds. Latency budgets read the view of what they measure (PBB
// "Measurement points"): gateway costs the internal view, what clients see
// the external view.
const (
	// InternalP99 and InternalP50 bound the access log's gateway_duration
	// percentile (PB-1, PB-2, PB-3, PB-15, PB-16).
	InternalP99 BudgetKind = iota
	InternalP50
	// ExternalP99 bounds the end-to-end p99 (C2's existing traffic).
	ExternalP99
	// MetricP99 bounds the p99 of a Node histogram over the measured
	// window (PB-9 and PB-14 on ruralz_state_call_duration_seconds).
	MetricP99
	// MinSaturation is a rate ladder's lower bound on saturation (PB-4).
	MinSaturation
	// MaxCPUPer10k bounds the Node's vCPU per 10,000 rps (Resources per
	// core).
	MaxCPUPer10k
	// MaxGCShare bounds the GC's share of Node CPU (Resources per core).
	MaxGCShare
	// MaxLoadedRSS bounds the Node's RSS under load (Resources per core).
	MaxLoadedRSS
	// MaxMeanCPU bounds the mean CPU per call of a component benchmark
	// (the RZ-RT-005 rejection's 16 µs).
	MaxMeanCPU
)

// Budget is one budget a scenario checks, as the budget catalog and the
// resources per core table state it.
type Budget struct {
	// ID names the budget in records, such as PB-2.
	ID string
	// Value is the budget as stated, such as "<= 1ms (target)".
	Value string
	// SLO names the linked SLO, such as SLO-GW-2; empty when none.
	SLO  string
	Kind BudgetKind
	// Limit is the bound in the kind's unit: a duration in nanoseconds, a
	// rate in requests per second, a share, vCPU or bytes.
	Limit float64
	// Metric and Labels select MetricP99's histogram family.
	Metric string
	Labels map[string]string
}

// Measures are what budgets read: one run's, or a comparison's pooled
// head runs.
type Measures struct {
	External, Internal *loadgen.Histogram
	// CPUPerRequest is Node CPU over achieved requests.
	CPUPerRequest time.Duration
	GCShare       float64
	RSSLoad       int64
	// Saturation is a rate ladder's saturation; 0 when none.
	Saturation float64
	// Metrics holds the histogram deltas over the measured window, keyed
	// by MetricKey.
	Metrics map[string]promtext.Histogram
}

// MetricKey keys a histogram family and label set in Measures.Metrics.
func MetricKey(name string, labels map[string]string) string {
	return name + promtext.Labels(labels).String()
}

// Evaluate checks b against m. A budget whose measure is missing (an
// empty view, no ladder, no samples) is skipped and says why.
func (b Budget) Evaluate(m *Measures) benchrecord.BudgetResult {
	r := benchrecord.BudgetResult{ID: b.ID, Value: b.Value, SLO: b.SLO, Verdict: benchrecord.BudgetSkipped}
	verdict := func(pass bool) string {
		if pass {
			return benchrecord.BudgetPass
		}
		return benchrecord.BudgetFail
	}
	switch b.Kind {
	case InternalP99, InternalP50, ExternalP99:
		h, pct := m.Internal, 99.0
		if b.Kind == ExternalP99 {
			h = m.External
		}
		if b.Kind == InternalP50 {
			pct = 50
		}
		if h == nil || h.Count() == 0 {
			return r
		}
		v := h.ValueAtPercentile(pct)
		r.Result, r.Verdict = durationString(v), verdict(float64(v) <= b.Limit)
	case MetricP99:
		h, ok := m.Metrics[MetricKey(b.Metric, b.Labels)]
		if !ok || h.Count == 0 {
			return r
		}
		q := h.Quantile(0.99)
		if math.IsNaN(q) {
			return r
		}
		v := time.Duration(q * float64(time.Second))
		r.Result, r.Verdict = durationString(v), verdict(float64(v) <= b.Limit)
	case MinSaturation:
		if m.Saturation <= 0 {
			r.Result, r.Verdict = "no step met the saturation rule", benchrecord.BudgetFail
			return r
		}
		r.Result, r.Verdict = strconv.FormatFloat(m.Saturation, 'f', 0, 64)+" rps", verdict(m.Saturation >= b.Limit)
	case MaxCPUPer10k:
		if m.CPUPerRequest <= 0 {
			return r
		}
		v := m.CPUPerRequest.Seconds() * 10000
		r.Result, r.Verdict = formatSig(v, 3)+" vCPU", verdict(v <= b.Limit)
	case MaxGCShare:
		r.Result, r.Verdict = formatSig(m.GCShare*100, 3)+"%", verdict(m.GCShare <= b.Limit)
	case MaxMeanCPU:
		if m.CPUPerRequest <= 0 {
			return r
		}
		r.Result, r.Verdict = durationString(m.CPUPerRequest), verdict(float64(m.CPUPerRequest) <= b.Limit)
	case MaxLoadedRSS:
		if m.RSSLoad <= 0 {
			return r
		}
		r.Result, r.Verdict = formatSig(float64(m.RSSLoad)/(1<<20), 3)+" MiB", verdict(float64(m.RSSLoad) <= b.Limit)
	default:
		r.Result, r.Verdict = fmt.Sprintf("unknown budget kind %d", b.Kind), benchrecord.BudgetFail
	}
	return r
}

// EvaluateAll checks every budget against m.
func EvaluateAll(bs []Budget, m *Measures) []benchrecord.BudgetResult {
	out := make([]benchrecord.BudgetResult, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Evaluate(m))
	}
	return out
}

// AnyFailed reports a failed budget.
func AnyFailed(rs []benchrecord.BudgetResult) bool {
	for _, r := range rs {
		if r.Verdict == benchrecord.BudgetFail {
			return true
		}
	}
	return false
}

// histogramDelta returns after minus before, bucket by bucket; buckets the
// before scrape lacks count from zero (a series born in the window).
func histogramDelta(before, after promtext.Histogram) promtext.Histogram {
	prev := map[float64]float64{}
	for _, b := range before.Buckets {
		prev[b.UpperBound] = b.Count
	}
	out := promtext.Histogram{Count: after.Count - before.Count, Sum: after.Sum - before.Sum}
	for _, b := range after.Buckets {
		out.Buckets = append(out.Buckets, promtext.Bucket{UpperBound: b.UpperBound, Count: max(b.Count-prev[b.UpperBound], 0)})
	}
	return out
}

// mergeHistograms adds histograms with the same bucket layout.
func mergeHistograms(hs ...promtext.Histogram) promtext.Histogram {
	per := map[float64]float64{}
	var out promtext.Histogram
	for _, h := range hs {
		out.Count += h.Count
		out.Sum += h.Sum
		for _, b := range h.Buckets {
			per[b.UpperBound] += b.Count
		}
	}
	for ub, c := range per {
		out.Buckets = append(out.Buckets, promtext.Bucket{UpperBound: ub, Count: c})
	}
	sortBuckets(out.Buckets)
	return out
}

// sortBuckets sorts buckets by upper bound.
func sortBuckets(b []promtext.Bucket) {
	for i := 1; i < len(b); i++ {
		for j := i; j > 0 && b[j].UpperBound < b[j-1].UpperBound; j-- {
			b[j], b[j-1] = b[j-1], b[j]
		}
	}
}
