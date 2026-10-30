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

// MeasureSaturation measures the topology's saturation S once per job
// (docs/engineering/03-testing-and-quality-strategy.md "Scale profiles"):
// the profile's Nodes behind the L4 balancer offered /plain over HTTP/1.1
// by the load generator, first in short steps doubling from
// PreSearchStart until a step misses the saturation rule, that step's
// rate being the expected saturation, then the load generator's rate
// ladder of 5% steps from 50% to 110% of it, ending after the first step
// that misses the rule; S is the highest offered rate meeting it
// (loadgen.Saturation, loadgen.BenchmarkSaturation). The run writes a
// "saturation" record, one phase per step, which a later package of the
// same job reads instead of measuring again (ReadSaturation).
func (h *Harness) MeasureSaturation(t testing.TB) float64 {
	t.Helper()
	if s, err := h.ReadSaturation(); err == nil {
		t.Logf("chaos: saturation S = %.0f rps from this job's record", s)
		h.SetSaturation(s)
		return s
	}
	r := h.Start(t, Spec{Name: Saturation})
	crit := loadgen.BenchmarkSaturation()
	// Round-robin connections spread the steps over every Node, as the
	// runs' loads are spread.
	target, err := loadgen.NewHTTP(loadgen.HTTPConfig{URL: "http://" + r.HTTP.Addr() + PathPlain, RoundRobin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()
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
		res, err := loadgen.Run(r.Context(), target, loadgen.Config{Rate: rate, Warmup: PreSearchWarmup, Duration: PreSearchDuration})
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
	results, err := loadgen.RunLadder(r.Context(), target, loadgen.Config{}, ladder)
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

// ReadSaturation returns S from this job's saturation record in the
// records directory: one of the same profile, commit and Node count.
func (h *Harness) ReadSaturation() (float64, error) {
	if h.Records == "" {
		return 0, ErrNoSaturation
	}
	b, err := os.ReadFile(filepath.Join(h.Records, (&Record{Experiment: Saturation}).FileName())) //nolint:gosec // G304: the records directory the job names
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
