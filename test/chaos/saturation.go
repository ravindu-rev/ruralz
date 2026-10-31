// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// Saturation is the record name of the measurement of the topology's
// saturation S, and SaturationBreakdown the breakdown holding S in
// requests per second.
const (
	Saturation          = "saturation"
	SaturationBreakdown = "saturation_rps"
)

// Pre-search of the expected saturation the rate ladder climbs around:
// steps doubling from PreSearchStart, each a short open-loop run.
const (
	PreSearchStart    = 250.0
	PreSearchWarmup   = 10 * time.Second
	PreSearchDuration = 20 * time.Second
	// PreSearchMax ends the pre-search past any loopback topology's rate.
	PreSearchMax = 1_000_000.0
)

// stepFunc runs one open-loop step of the saturation measurement at rate,
// warmup then duration, and returns its result.
type stepFunc func(rate float64, warmup, duration time.Duration) (*loadgen.Result, error)

// MeasureSaturation measures the topology's saturation S once per job
// (docs/engineering/03-testing-and-quality-strategy.md "Scale profiles"):
// the profile's Nodes behind the L4 balancer offered /plain over HTTP/1.1
// by the load generator, on the harness's host or at the target profile on
// the load host, first in short steps doubling from PreSearchStart until
// a step misses the saturation rule, that step's rate being the expected
// saturation, then the load generator's rate ladder of 5% steps from 50%
// to 110% of it, ending after the first step that misses the rule; S is
// the highest offered rate meeting it (loadgen.Saturation,
// loadgen.BenchmarkSaturation). The run writes a "saturation" record, one
// phase per step, which a later package of the same job reads instead of
// measuring again (ReadSaturation).
func (h *Harness) MeasureSaturation(t testing.TB) float64 {
	t.Helper()
	if s, err := h.ReadSaturation(); err == nil {
		t.Logf("chaos: saturation S = %.0f rps from this job's record", s)
		h.SetSaturation(s)
		return s
	}
	r := h.Start(t, Spec{Name: Saturation})
	crit := loadgen.BenchmarkSaturation()
	run, err := r.saturationSteps()
	if err != nil {
		t.Fatal(err)
	}
	meets := func(res *loadgen.Result) bool {
		sum := res.Summary()
		return sum.RateError <= crit.MaxRateError && sum.P99 <= crit.MaxP99 && sum.ErrorRate <= crit.MaxErrorRate
	}
	step := func(name string, res *loadgen.Result) {
		r.mu.Lock()
		r.rec.Phases = append(r.rec.Phases, Phase{Name: name, Start: res.MeasureStart, End: res.MeasureEnd})
		r.mu.Unlock()
		sum := res.Summary()
		r.Breakdown(name+"_p99_us", float64(sum.P99.Microseconds()))
		r.Breakdown(name+"_error_rate", sum.ErrorRate)
		r.Breakdown(name+"_rate_error", sum.RateError)
	}
	expected, lastPass := 0.0, 0.0
	for rate := PreSearchStart; rate <= PreSearchMax; rate *= 2 {
		res, err := run(rate, PreSearchWarmup, PreSearchDuration)
		if err != nil {
			t.Fatal(err)
		}
		step("presearch-"+strconv.Itoa(int(rate)), res)
		if !meets(res) {
			expected = rate
			break
		}
		lastPass = rate
	}
	if expected == 0 {
		t.Fatalf("chaos: saturation: no pre-search step up to %.0f rps missed the saturation rule", PreSearchMax)
	}
	ladder := loadgen.BenchmarkLadder(expected)
	ladder.Stop = func(res *loadgen.Result) bool { return !meets(res) }
	results, err := runLadder(run, ladder)
	if err != nil {
		t.Fatal(err)
	}
	var sums []loadgen.StepSummary
	for _, res := range results {
		step("ladder-"+strconv.Itoa(int(res.Rate)), res)
		sums = append(sums, res.Summary())
	}
	s, ok := loadgen.Saturation(sums, crit)
	if !ok {
		// Not even 50% of the expected rate met the rule: the pre-search's
		// last passing step is the best known.
		s = lastPass
	}
	if s <= 0 {
		t.Fatal("chaos: saturation: no step met the saturation rule")
	}
	r.Breakdown(SaturationBreakdown, s)
	r.Breakdown("expected_rps", expected)
	r.Finish()
	h.SetSaturation(s)
	t.Logf("chaos: saturation S = %.0f rps (expected %.0f)", s, expected)
	return s
}

// saturationSteps returns how the run offers the saturation steps: /plain
// over HTTP/1.1 connections taken in turn, which spread the steps over
// every Node as the runs' loads are spread, from the harness's host, or
// at the target profile from the load host, which keeps no record of a
// step, its result being all the rule reads, as on the harness's host.
func (r *Run) saturationSteps() (stepFunc, error) {
	if r.H.place != nil {
		s, err := r.loadSession()
		if err != nil {
			return nil, err
		}
		addr := r.HTTP.Addr()
		return func(rate float64, warmup, duration time.Duration) (*loadgen.Result, error) {
			return s.runStep(saturationStream(addr, rate), warmup, duration)
		}, nil
	}
	target, err := loadgen.NewHTTP(loadgen.HTTPConfig{URL: "http://" + r.HTTP.Addr() + PathPlain, RoundRobin: true})
	if err != nil {
		return nil, err
	}
	r.T.Cleanup(func() { _ = target.Close() })
	return func(rate float64, warmup, duration time.Duration) (*loadgen.Result, error) {
		return loadgen.Run(r.Context(), target, loadgen.Config{Rate: rate, Warmup: warmup, Duration: duration})
	}, nil
}

// saturationStream returns a saturation step's stream on the load host:
// /plain at addr over HTTP/1.1 connections taken in turn, at rate.
func saturationStream(addr string, rate float64) lab.LoadStream {
	return lab.LoadStream{Name: "plain", URL: "http://" + addr + PathPlain, Protocol: "http1", RoundRobin: true, Rate: rate}
}

// runLadder runs every step of l with run, as loadgen.RunLadder does:
// each step lasts StepDuration, only the first has the warm-up, and the
// climb ends after a step Stop reports. It returns the step results so far
// and the first error.
func runLadder(run stepFunc, l loadgen.Ladder) ([]*loadgen.Result, error) {
	rates, err := l.Rates()
	if err != nil {
		return nil, err
	}
	if l.StepDuration <= 0 {
		return nil, errors.New("chaos: saturation: the ladder's step duration must be positive")
	}
	var out []*loadgen.Result
	for k, rate := range rates {
		warmup := time.Duration(0)
		if k == 0 {
			warmup = l.Warmup
		}
		res, err := run(rate, warmup, l.StepDuration)
		if res != nil {
			out = append(out, res)
		}
		if err != nil {
			return out, err
		}
		if l.Stop != nil && l.Stop(res) {
			break
		}
	}
	return out, nil
}

// ReadSaturation returns S from this job's saturation record in the
// records directory: one of the same profile, commit and Node count.
func (h *Harness) ReadSaturation() (float64, error) {
	if h.Records == "" {
		return 0, ErrNoSaturation
	}
	b, err := os.ReadFile(filepath.Join(h.Records, (&Record{Experiment: Saturation}).FileName()))
	if err != nil {
		return 0, fmt.Errorf("chaos: saturation record: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return 0, fmt.Errorf("chaos: saturation record: %w", err)
	}
	s, ok := rec.Breakdowns[SaturationBreakdown]
	switch {
	case rec.Format != Format || rec.Profile != h.Profile.Name || rec.Commit != h.Commit || rec.Nodes != h.Profile.Nodes:
		return 0, errors.New("chaos: the saturation record is another job's")
	case !ok || s <= 0:
		return 0, errors.New("chaos: the saturation record holds no saturation")
	}
	return s, nil
}
