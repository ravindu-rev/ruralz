// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
)

// PB10Share is PB-10's bound: telemetry CPU at defaults at most 5% of Node
// CPU (target).
const PB10Share = 0.05

// ComponentMetricRecording is the metric recording component benchmark
// whose per-request CPU PB-10 adds, since metric recording has no switch.
const ComponentMetricRecording = "metric-recording"

// summaryComparison is the record name of a comparison's summary.
const summaryComparison = "summary-comparison.json"

// RunPB10 runs PB-10's telemetry-off S2 (PBB "Scenarios"): S2's workload on
// the s2-telemetry-off Bundle, which sets no telemetry.otlp,
// traceSampling 0 and accessLog.when "false". On RH-1 it judges PB-10:
// telemetry CPU, the CPU per request S2 spends with telemetry on (tonight's
// S2 comparison) minus the telemetry-off run's plus the per-request CPU of
// metric recording (tonight's metric-recording component benchmark), at
// most 5% of the telemetry-on CPU per request.
func RunPB10(ctx context.Context, e Env, s Scale) (*Result, error) {
	res := &Result{}
	s2, err := catalog("S2")
	if err != nil {
		return nil, err
	}
	warm, dur, noSettle := e.lengths(s)
	w := s2.Workload
	w.Rate = s.PB10.Rate
	sc := scenario("S2-telemetry-off", DirS2TelemetryOff, w, s2.Saturation)
	var on, recording time.Duration
	var onErr, recErr error
	if !s.Functional {
		on, onErr = summaryCPU(e.S, "S2")
		recording, recErr = summaryCPU(e.S, "component-"+ComponentMetricRecording)
	}
	_, err = e.run(ctx, sc, harness.RunSpec{
		Warmup: warm, Duration: dur, NoSettle: noSettle, NoInternal: true,
		Criteria: func(o *harness.RunOutcome) []benchrecord.BudgetResult {
			return []benchrecord.BudgetResult{s.hardware(withSLO(pb10(on, o.Measures.CPUPerRequest, recording, onErr, recErr), "SLO-GW-7"))}
		},
	}, res)
	if err != nil {
		return nil, err
	}
	return res.finish(), nil
}

// summaryCPU reads the pooled head CPU per request of tonight's summary of
// scenario.
func summaryCPU(s *harness.Session, scenario string) (time.Duration, error) {
	rec, err := readRecord(s, scenario, summaryComparison)
	if err != nil {
		return 0, err
	}
	if rec.Resources.CPUPerRequest <= 0 {
		return 0, fmt.Errorf("scenarios: %s's summary holds no CPU per request", scenario)
	}
	return rec.Resources.CPUPerRequest, nil
}

// pb10 judges PB-10 from the telemetry-on and telemetry-off CPU per
// request and metric recording's per-request CPU.
func pb10(on, off, recording time.Duration, onErr, recErr error) benchrecord.BudgetResult {
	const id, value = "PB-10", "telemetry CPU at defaults <= 5% of Node CPU at half saturation (target)"
	var missing []string
	for _, m := range []struct {
		what string
		err  error
		v    time.Duration
	}{{"S2 with telemetry on", onErr, on}, {"metric recording", recErr, recording}, {"S2 with telemetry off", nil, off}} {
		switch {
		case m.err != nil:
			missing = append(missing, m.what+": "+m.err.Error())
		case m.v <= 0:
			missing = append(missing, m.what+": no CPU per request")
		}
	}
	if len(missing) > 0 {
		return judge(id, value, false, strings.Join(missing, "; "))
	}
	tel := on - off + recording
	share := float64(tel) / float64(on)
	return judge(id, value, share <= PB10Share,
		fmt.Sprintf("%.2f%%: %s per request on, %s off, %s metric recording", share*100, on, off, recording))
}
