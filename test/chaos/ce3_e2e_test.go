// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// ce3Added is CE-3's latency added to every State Store call, half in
// each direction (target).
const ce3Added = 200 * time.Millisecond

// TestCE3StateStoreSlowed is CE-3: 200 ms added to every State Store call
// for 60 s at 50% load, in two runs
// (docs/architecture/11-scalability-and-distributed-state.md "Chaos
// experiments"; docs/engineering/03-testing-and-quality-strategy.md
// "Hermetic rule scope"). The functional run, on the controllable-clock
// build, holds the timeline's rules: a valid baseline, every Node raising
// state_store_breaker_open during the fault, the event that ends the
// latency criterion's first part, and a recovery in which the breakers
// close, the degraded reasons clear and errors return to zero. The
// latency run is a measurement on the release build in real time, its
// warm-up extended until latency settles: each 1 s window's p99 of the
// requests calling the State Store stays at most the baseline's p99 plus
// stateStoreTimeout until the last Node raises state_store_breaker_open,
// then the rest of the fault phase's p99 within 10% of the baseline's
// (target). Its criteria, the timeline's included, are latency criteria,
// recorded at every profile and gating only at the target profile.
func TestCE3StateStoreSlowed(t *testing.T) {
	t.Run("functional", func(t *testing.T) { ce3(t, false) })
	t.Run("latency", func(t *testing.T) { ce3(t, true) })
}

// ce3 runs one of CE-3's runs: the latency measurement when measurement
// is set, else the functional run.
func ce3(t *testing.T, measurement bool) {
	s := saturation(t)
	rate := suite.Profile.LoadShare * s
	name := CE3
	spec := LoadSpec{Duration: Baseline + FaultPhase + RecoveryTime, Streams: []StreamSpec{
		{Name: "plain", Path: PathPlain, Rate: rate / 2},
		{Name: "limited", Path: PathLimited, Rate: rate / 2, APIKeys: true, Counted: Decisions()},
	}}
	if measurement {
		name += "/latency"
		spec.Settle = loadgen.BenchmarkSettle()
	}
	r := suite.Start(t, Spec{Name: name, Measurement: measurement, SampleEvery: 100 * time.Millisecond})
	l := r.StartLoad(spec)
	r.Begin(l)
	base := r.Hold(l, PhaseBaseline, Baseline)
	start := r.Inject("delay", "state-store", func() error {
		return r.SetStoreMode("main", faultproxy.Delay{Mean: ce3Added / 2})
	})
	fault := r.Hold(l, PhaseFault, FaultPhase)
	end := r.Inject("pass", "state-store", func() error { return r.SetStoreMode("main", faultproxy.Pass{}) })
	rec := r.Hold(l, PhaseRecovery, RecoveryTime)
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, st := range l.Streams {
		r.IntervalLog(st.Spec.Name, st)
	}
	r.Baseline(l, base)
	nodes := r.NodeNames()
	raised := r.Sampler.ReasonRaised(nodes, BreakerReason, start, fault.End)
	r.Add(Check("Nodes raising state_store_breaker_open in the fault", r.ValidityKind(), float64(len(raised)), EQ, float64(len(nodes))))
	last := start
	for _, at := range raised {
		if at.After(last) {
			last = at
		}
	}
	r.Breakdown("last_breaker_open_after_fault_s", last.Sub(start).Seconds())
	if measurement {
		limited := []*Stream{l.StreamNamed("limited")}
		baseP99 := Percentile(Latencies(limited, base.Start, base.End), 99)
		r.Breakdown("baseline_p99_ms", ms(baseP99))
		worst := time.Duration(0)
		for _, p := range WindowP99s(limited, start, last.Add(time.Second)) {
			worst = max(worst, p)
		}
		rest := Percentile(Latencies(limited, last, fault.End), 99)
		r.Add(
			Check("worst 1 s window p99 until the last breaker opened, ms", Latency, ms(worst), LE, ms(baseP99+StateStoreTimeout)),
			Check("fault-phase p99 after the last breaker opened, ms", Latency, ms(rest), LE, 1.1*ms(baseP99)),
		)
	}
	r.RecoveryCriteria(l, base, rec, end)
	r.Finish()
}

// ms returns d in milliseconds.
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
