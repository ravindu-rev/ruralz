// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// CompareSpec is one scenario's A/B comparison.
type CompareSpec struct {
	Scenario Scenario
	Head     Build
	// Baseline is nil for a comparison judged on absolute budgets only:
	// a scenario's first run, or the first release.
	Baseline *Build
	Revision Revision
	// Warmup and Duration override the benchmark lengths; Pairs the
	// five pairs (smoke and functional runs).
	Warmup, Duration time.Duration
	NoSettle         bool
	Pairs            int
	// Rerun lets a single invalid run re-run once, from the nightly
	// reserve.
	Rerun func() bool
}

// Pair is one interleaved head and baseline run; a missing or invalid run
// is nil or has Valid false.
type Pair struct {
	Head, Baseline *RunOutcome
}

// CompareResult is a comparison's outcome.
type CompareResult struct {
	Summary *benchrecord.Record
	Path    string
	// Verdict is the A/B verdict; Requeue reports an inconclusive one.
	Verdict string
	Requeue bool
	// Pass reports a comparison whose A/B verdict passed (or had no
	// baseline) and whose budgets all held: the baseline may advance.
	Pass bool
}

// Compare runs the comparison: head and baseline alternately, A, B, A, B,
// each on a fresh Node loading the head's Revision, with a CPU profile
// after the last head run, then the summary record.
func (s *Session) Compare(ctx context.Context, cs CompareSpec) (*CompareResult, error) {
	n := cs.Pairs
	if n == 0 {
		n = Pairs
	}
	var pairs []Pair
	var reasons []string
	baseCommit := ""
	if cs.Baseline != nil {
		baseCommit = cs.Baseline.Commit
	}
	run := func(b Build, name string, profile bool) (*RunOutcome, error) {
		spec := RunSpec{
			Scenario: cs.Scenario, Build: b, Revision: cs.Revision, Baseline: baseCommit, Name: name,
			Warmup: cs.Warmup, Duration: cs.Duration, NoSettle: cs.NoSettle, Profile: profile,
		}
		out, err := s.Run(ctx, spec)
		if err == nil && !out.Valid && cs.Rerun != nil && cs.Rerun() {
			spec.Name += "-rerun"
			return s.Run(ctx, spec)
		}
		return out, err
	}
	refused := false
	for i := 1; i <= n; i++ {
		var p Pair
		h, err := run(cs.Head, "run-"+strconv.Itoa(i)+"-head", i == n)
		if err != nil {
			return nil, err
		}
		p.Head = h
		if cs.Baseline != nil && !refused {
			b, err := run(*cs.Baseline, "run-"+strconv.Itoa(i)+"-baseline", false)
			switch {
			case errors.Is(err, ErrRefused):
				refused = true
				reasons = append(reasons, "the baseline refuses the head's Revision: "+err.Error())
			case err != nil:
				return nil, err
			default:
				p.Baseline = b
			}
		}
		pairs = append(pairs, p)
	}
	sum, err := Summarize(cs.Scenario, pairs, cs.Baseline != nil, reasons)
	if err != nil {
		return nil, err
	}
	rec := s.baseRecord(cs.Scenario, RunSpec{Build: cs.Head, Revision: cs.Revision, Baseline: baseCommit}, time.Now().UTC().Truncate(time.Millisecond), benchrecord.KindSummary)
	sum.fill(rec)
	path, err := s.Rec.Write("summary-comparison", rec)
	if err != nil {
		return nil, err
	}
	return &CompareResult{
		Summary: rec, Path: path, Verdict: rec.Summary.Verdict,
		Requeue: rec.Summary.Verdict == benchrecord.ABInconclusive,
		Pass:    (rec.Summary.Verdict == benchrecord.ABPass || rec.Summary.Verdict == benchrecord.ABNone) && !AnyFailed(rec.Budgets) && rec.Validity.Verdict == benchrecord.VerdictValid,
	}, nil
}

// Summary is a comparison's computed summary.
type Summary struct {
	Runs                     []string
	HeadValid, BaseValid     int
	HeadViews, BaseViews     benchrecord.Views
	External, Internal       ViewComparison
	Verdict                  string
	Budgets                  []benchrecord.BudgetResult
	Reasons                  []string
	Load                     benchrecord.Load
	HeadResources            benchrecord.Resources
	hasBaseline, internalSet bool
}

// Summarize applies the comparison rules to the pairs. Budgets read the
// pooled histograms of the valid head runs; the views compare the per-run
// p99 values of pairs whose runs are both valid.
func Summarize(sc Scenario, pairs []Pair, hasBaseline bool, reasons []string) (*Summary, error) {
	sum := &Summary{hasBaseline: hasBaseline, Reasons: append([]string(nil), reasons...)}
	var headExt, headInt, baseExt, baseInt []*loadgen.Histogram
	var pe, pi, be, bi []float64
	var cpu, gc []float64
	var rss int64
	var metrics []map[string]promtext.Histogram
	for _, p := range pairs {
		for _, r := range []*RunOutcome{p.Head, p.Baseline} {
			if r != nil {
				sum.Runs = append(sum.Runs, r.Path)
			}
		}
		if p.Head != nil && p.Head.Valid {
			sum.HeadValid++
			headExt, headInt = append(headExt, p.Head.Measures.External), append(headInt, p.Head.Measures.Internal)
			cpu = append(cpu, float64(p.Head.Measures.CPUPerRequest))
			gc = append(gc, p.Head.Measures.GCShare)
			rss = max(rss, p.Head.Measures.RSSLoad)
			metrics = append(metrics, p.Head.Metrics)
			if sum.Load.Tool == "" {
				sum.Load = p.Head.Record.Load
			}
		}
		if p.Baseline != nil && p.Baseline.Valid {
			sum.BaseValid++
			baseExt, baseInt = append(baseExt, p.Baseline.Measures.External), append(baseInt, p.Baseline.Measures.Internal)
		}
		if p.Head != nil && p.Head.Valid && p.Baseline != nil && p.Baseline.Valid {
			pe, be = append(pe, p99(p.Head.Measures.External)), append(be, p99(p.Baseline.Measures.External))
			pi, bi = append(pi, p99(p.Head.Measures.Internal)), append(bi, p99(p.Baseline.Measures.Internal))
		}
	}
	if len(sum.Runs) == 0 {
		return nil, errors.New("harness: a comparison without runs")
	}
	he, err1 := Pool(headExt...)
	hi, err2 := Pool(headInt...)
	bpe, err3 := Pool(baseExt...)
	bpi, err4 := Pool(baseInt...)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return nil, err
	}
	sum.HeadViews = benchrecord.Views{External: Percentiles(he), Internal: Percentiles(hi)}
	sum.BaseViews = benchrecord.Views{External: Percentiles(bpe), Internal: Percentiles(bpi)}
	headP99s := func(hs []*loadgen.Histogram) []float64 {
		var out []float64
		for _, h := range hs {
			out = append(out, p99(h))
		}
		return out
	}
	if hasBaseline {
		sum.External, sum.Internal = CompareView(pe, be), CompareView(pi, bi)
		sum.Verdict = ABVerdict(sum.HeadValid, sum.BaseValid, sum.External, sum.Internal)
		if len(sum.Reasons) > 0 {
			sum.Verdict = benchrecord.ABInconclusive
		}
	} else {
		sum.External = ViewComparison{HeadMedian: Median(headP99s(headExt))}
		sum.Internal = ViewComparison{HeadMedian: Median(headP99s(headInt))}
		sum.Verdict = benchrecord.ABNone
	}
	pooled := &Measures{External: he, Internal: hi, CPUPerRequest: time.Duration(Median(cpu)), GCShare: Median(gc), RSSLoad: rss, Metrics: map[string]promtext.Histogram{}}
	for _, b := range sc.Budgets {
		if b.Kind != MetricP99 {
			continue
		}
		var hs []promtext.Histogram
		for _, m := range metrics {
			if h, ok := m[MetricKey(b.Metric, b.Labels)]; ok {
				hs = append(hs, h)
			}
		}
		if len(hs) > 0 {
			pooled.Metrics[MetricKey(b.Metric, b.Labels)] = mergeHistograms(hs...)
		}
	}
	sum.Budgets = EvaluateAll(sc.Budgets, pooled)
	sum.HeadResources = benchrecord.Resources{CPUPerRequest: pooled.CPUPerRequest, GCCPUShare: pooled.GCShare, RSSLoadBytes: rss}
	switch {
	case sum.HeadValid < len(pairs):
		sum.Reasons = append(sum.Reasons, fmt.Sprintf("%d of %d head runs valid", sum.HeadValid, len(pairs)))
	case hasBaseline && sum.BaseValid < len(pairs):
		sum.Reasons = append(sum.Reasons, fmt.Sprintf("%d of %d baseline runs valid", sum.BaseValid, len(pairs)))
	}
	if hasBaseline && (sum.External.Noisy || sum.Internal.Noisy) {
		sum.Reasons = append(sum.Reasons, fmt.Sprintf("baseline p99 noise external %.4f, internal %.4f, above %.2f", sum.External.BaseCV, sum.Internal.BaseCV, MaxBaselineCV))
	}
	return sum, nil
}

// fill writes the summary into a summary record.
func (sum *Summary) fill(rec *benchrecord.Record) {
	rec.Load = sum.Load
	rec.Latency = benchrecord.Latency{External: sum.HeadViews.External, Internal: sum.HeadViews.Internal, HistogramFiles: []string{}}
	rec.Resources = sum.HeadResources
	rec.Budgets = sum.Budgets
	verdict := benchrecord.VerdictValid
	if sum.Verdict == benchrecord.ABInconclusive || len(sum.Reasons) > 0 {
		verdict = benchrecord.VerdictInconclusive
	}
	reasons := dedupe(sum.Reasons)
	if verdict == benchrecord.VerdictInconclusive && len(reasons) == 0 {
		reasons = []string{"the A/B comparison is inconclusive"}
	}
	rec.Validity = benchrecord.Validity{NoiseCV: sum.External.BaseCV, Verdict: verdict, Reasons: reasons}
	rec.Summary = &benchrecord.Summary{
		Of: benchrecord.OfComparison, Runs: sum.Runs, HeadRuns: sum.HeadValid, BaselineRuns: sum.BaseValid,
		Head: sum.HeadViews, Baseline: sum.BaseViews,
		HeadMedianP99:     benchrecord.PerView{External: sum.External.HeadMedian, Internal: sum.Internal.HeadMedian},
		BaselineMedianP99: benchrecord.PerView{External: sum.External.BaseMedian, Internal: sum.Internal.BaseMedian},
		SlowerPairs:       benchrecord.PerView{External: float64(sum.External.Slower), Internal: float64(sum.Internal.Slower)},
		BaselineNoise:     benchrecord.PerView{External: sum.External.BaseCV, Internal: sum.Internal.BaseCV},
		Verdict:           sum.Verdict,
		Ladder:            benchrecord.Ladder{Steps: []benchrecord.LadderStep{}},
	}
}
