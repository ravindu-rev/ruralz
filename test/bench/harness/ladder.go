// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// LadderSpec is one rate ladder: open-loop steps of 5% of the expected
// saturation from 50% to 110%, 60 s each, after one 60 s warm-up (PBB
// "Warm-up, duration and percentiles").
type LadderSpec struct {
	Scenario Scenario
	Build    Build
	Revision Revision
	// Ladder overrides PBB's ladder (smoke and functional runs); its
	// Expected defaults to the scenario's saturation.
	Ladder *loadgen.Ladder
}

// Ladder climbs the ladder on one Node, writes each step's record and the
// ladder's summary with its saturation, and checks the saturation budgets.
func (s *Session) Ladder(ctx context.Context, ls LadderSpec) (*benchrecord.Record, error) {
	lad := loadgen.BenchmarkLadder(ls.Scenario.Saturation)
	if ls.Ladder != nil {
		lad = *ls.Ladder
		if lad.Expected == 0 {
			lad.Expected = ls.Scenario.Saturation
		}
	}
	rates, err := lad.Rates()
	if err != nil {
		return nil, err
	}
	id := ls.Scenario.ID + "-ladder"
	sc := ls.Scenario
	sc.ID = id
	first := RunSpec{Scenario: sc, Build: ls.Build, Revision: ls.Revision, Name: "step-1"}
	st, err := s.prepare(ctx, first)
	if err != nil {
		return nil, err
	}
	defer st.close()
	var steps []loadgen.StepSummary
	var recSteps []benchrecord.LadderStep
	var runs []string
	for i, rate := range rates {
		spec := first
		spec.Scenario.Workload.Rate = rate
		spec.Name = "step-" + strconv.Itoa(i+1)
		spec.Duration, spec.NoSettle, spec.Warmup = lad.StepDuration, true, -1
		if i == 0 {
			spec.Warmup = lad.Warmup
		}
		out, err := s.measure(ctx, st, spec)
		if err != nil {
			return nil, err
		}
		runs = append(runs, out.Path)
		step := loadgen.StepSummary{
			Offered: rate, RateError: out.Record.Validity.RateError, P99: out.Record.Latency.External.P99, ErrorRate: out.ErrorRate,
		}
		steps = append(steps, step)
		recSteps = append(recSteps, ladderStep(step, out.Record.Load.AchievedRate, loadgen.BenchmarkSaturation()))
		if lad.Stop != nil && st.node.Exited() {
			break
		}
	}
	sat, _ := loadgen.Saturation(steps, loadgen.BenchmarkSaturation())
	rec := s.baseRecord(sc, RunSpec{Build: ls.Build, Revision: ls.Revision}, time.Now().UTC().Truncate(time.Millisecond), benchrecord.KindSummary)
	var budgets []Budget
	for _, b := range ls.Scenario.Budgets {
		if b.Kind == MinSaturation {
			budgets = append(budgets, b)
		}
	}
	rec.Budgets = EvaluateAll(budgets, &Measures{Saturation: sat})
	rec.Load = benchrecord.Load{Tool: string(sc.Workload.Tool), Version: s.ToolVersions[sc.Workload.Tool], CommandLine: s.commandLine(sc.Workload), OfferedRate: rates[len(rates)-1], Connections: sc.Workload.Connections}
	rec.Latency = benchrecord.Latency{HistogramFiles: []string{}}
	rec.Validity = benchrecord.Validity{Verdict: benchrecord.VerdictValid}
	rec.Summary = &benchrecord.Summary{
		Of: benchrecord.OfLadder, Runs: runs, Verdict: benchrecord.ABNone,
		Ladder: benchrecord.Ladder{Steps: recSteps, Saturation: sat},
	}
	if _, err := s.Rec.Write("summary-ladder", rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// ladderStep converts a step for the record, marking whether it meets the
// saturation rule.
func ladderStep(s loadgen.StepSummary, achieved float64, c loadgen.SaturationCriteria) benchrecord.LadderStep {
	return benchrecord.LadderStep{
		OfferedRate: s.Offered, AchievedRate: achieved, RateError: s.RateError, ErrorRate: math.Min(math.Max(s.ErrorRate, 0), 1), P99: s.P99,
		Saturated: s.RateError <= c.MaxRateError && s.P99 <= c.MaxP99 && s.ErrorRate <= c.MaxErrorRate,
	}
}

// Soak rules (PBB "Suite layers"): S2 at half saturation for 2 hours,
// sampled every 10 s; the RSS drift compares the median of the last 10
// minutes with that of minutes 10 to 20, at most 2%; goroutines are flat
// when those medians differ by at most 1% or 50, whichever is larger
// (targets).
const (
	SoakDuration      = 2 * time.Hour
	SoakSample        = 10 * time.Second
	MaxSoakRSSDrift   = 0.02
	soakGoroutineFrac = 0.01
	soakGoroutineAbs  = 50
)

// SoakPoint is one soak sample: its offset from the measured start, the
// Node's RSS and its goroutines.
type SoakPoint struct {
	At         time.Duration
	RSS        int64
	Goroutines int64
}

// SoakDrift applies the soak rule to points over a run of d: early is
// minutes 10 to 20, late the last 10 minutes (scaled to d/12 for shorter
// runs, so a functional soak keeps the same shape).
func SoakDrift(points []SoakPoint, d time.Duration) benchrecord.Soak {
	unit := 10 * time.Minute
	if d < SoakDuration {
		unit = d / 12
	}
	var rssE, rssL, grE, grL []float64
	for _, p := range points {
		switch {
		case p.At >= unit && p.At < 2*unit:
			rssE, grE = append(rssE, float64(p.RSS)), append(grE, float64(p.Goroutines))
		case p.At >= d-unit && p.At <= d:
			rssL, grL = append(rssL, float64(p.RSS)), append(grL, float64(p.Goroutines))
		}
	}
	out := benchrecord.Soak{Samples: len(points)}
	out.RSSEarlyBytes, out.RSSLateBytes = int64(Median(rssE)), int64(Median(rssL))
	out.GoroutinesEarly, out.GoroutinesLate = int64(Median(grE)), int64(Median(grL))
	if out.RSSEarlyBytes > 0 {
		out.RSSDrift = float64(out.RSSLateBytes-out.RSSEarlyBytes) / float64(out.RSSEarlyBytes)
	}
	allowed := math.Max(soakGoroutineFrac*float64(out.GoroutinesEarly), soakGoroutineAbs)
	out.GoroutinesFlat = len(grE) > 0 && len(grL) > 0 && math.Abs(float64(out.GoroutinesLate-out.GoroutinesEarly)) <= allowed
	return out
}

// soakBudgets returns the soak rule as budget results.
func soakBudgets(so benchrecord.Soak) []benchrecord.BudgetResult {
	rss := benchrecord.BudgetResult{ID: "soak-rss-drift", Value: "<= 2% RSS drift (target)", Result: formatSig(so.RSSDrift*100, 3) + "%", Verdict: benchrecord.BudgetPass}
	if so.RSSEarlyBytes == 0 || so.RSSLateBytes == 0 {
		rss.Result, rss.Verdict = "no samples in a compared window", benchrecord.BudgetFail
	} else if so.RSSDrift > MaxSoakRSSDrift {
		rss.Verdict = benchrecord.BudgetFail
	}
	gr := benchrecord.BudgetResult{
		ID: "soak-goroutines", Value: "flat within 1% or 50 (target)",
		Result: fmt.Sprintf("%d to %d", so.GoroutinesEarly, so.GoroutinesLate), Verdict: benchrecord.BudgetPass,
	}
	if !so.GoroutinesFlat {
		gr.Verdict = benchrecord.BudgetFail
	}
	return []benchrecord.BudgetResult{rss, gr}
}

// SoakSpec is the release soak.
type SoakSpec struct {
	Scenario Scenario
	Build    Build
	Revision Revision
	// Duration overrides the 2 hours (functional runs).
	Duration time.Duration
}

// Soak runs the soak: one run of S2 at half saturation sampled every
// 10 s, meeting S2's validity rules with zero telemetry drops and PB-2
// and PB-3 over the whole run, then the soak's summary.
func (s *Session) Soak(ctx context.Context, ss SoakSpec) (*benchrecord.Record, error) {
	d := ss.Duration
	if d == 0 {
		d = SoakDuration
	}
	sc := ss.Scenario
	sc.ID += "-soak"
	out, err := s.Run(ctx, RunSpec{Scenario: sc, Build: ss.Build, Revision: ss.Revision, Name: "soak", Duration: d, Sample: min(SoakSample, max(d/120, time.Second))})
	if err != nil {
		return nil, err
	}
	so := SoakDrift(soakPoints(out.samples, out.MeasureStart, d), d)
	rec := s.baseRecord(sc, RunSpec{Build: ss.Build, Revision: ss.Revision}, time.Now().UTC().Truncate(time.Millisecond), benchrecord.KindSummary)
	rec.Load = out.Record.Load
	rec.Latency = benchrecord.Latency{External: out.Record.Latency.External, Internal: out.Record.Latency.Internal, HistogramFiles: []string{}}
	rec.Resources = out.Record.Resources
	rec.Resources.CPUProfileFiles = []string{}
	rec.Budgets = append(append([]benchrecord.BudgetResult{}, out.Record.Budgets...), soakBudgets(so)...)
	rec.Validity = out.Record.Validity
	if rec.Validity.Verdict == benchrecord.VerdictInvalid {
		rec.Validity.Verdict = benchrecord.VerdictInconclusive
	}
	rec.Summary = &benchrecord.Summary{
		Of: benchrecord.OfSoak, Runs: []string{out.Path}, HeadRuns: btoi(out.Valid), Verdict: benchrecord.ABNone, Soak: so,
		Ladder: benchrecord.Ladder{Steps: []benchrecord.LadderStep{}},
	}
	if _, err := s.Rec.Write("summary-soak", rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// soakPoints converts the samples of the measured window [start,
// start+d] to soak points.
func soakPoints(samples []nodeSample, start time.Time, d time.Duration) []SoakPoint {
	var points []SoakPoint
	for _, smp := range samples {
		at := smp.at.Sub(start)
		if smp.err != nil || at < 0 || at > d {
			continue
		}
		points = append(points, SoakPoint{At: at, RSS: smp.rss, Goroutines: int64(smp.metrics.Sum("ruralz_runtime_goroutines", promtext.Labels(nil)))})
	}
	return points
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
