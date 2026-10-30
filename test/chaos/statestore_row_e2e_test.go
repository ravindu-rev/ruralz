// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"context"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// Phases of the State Store row's runs.
const (
	PhaseSlow    = "slow"
	PhaseStopped = "stopped"
)

// slowStore is the State Store row's slowdown: each direction of every
// call held five times stateStoreTimeout, so no call answers within its
// deadline even while Node time lags real time.
var slowStore = faultproxy.Delay{Mean: 5 * StateStoreTimeout}

// TestStateStoreRow is the System overview row "State Store slow or
// down": at 50% load the State Store turns slower than
// stateStoreTimeout for 60 s, then stops for 60 s, SIGKILLed and
// restarted in one run, frozen by SIGSTOP with its sockets open and
// resumed in the other, then recovers for 60 s. A request waits at most
// one deadline, then its Policy's failureMode decides; every Node's
// breaker opens, on connect failures after the SIGKILL and on timeouts
// while frozen; a key's admissions stay within N × its per-Node ceiling;
// closed Policies reject with RZ-STS; after each stop the breakers close
// and admission returns to the healthy bound
// (docs/engineering/03-testing-and-quality-strategy.md "Chaos testing").
func TestStateStoreRow(t *testing.T) {
	t.Run("kill", func(t *testing.T) {
		stateStoreRow(t, "kill", func(ctx context.Context, r *Run) (func() error, string) {
			p := r.Store.Deployment().Primary()
			return func() error { return p.Kill(ctx) }, "sigkill"
		}, func(ctx context.Context, r *Run) func() error {
			p := r.Store.Deployment().Primary()
			return func() error { return p.Restart(ctx) }
		})
	})
	t.Run("freeze", func(t *testing.T) {
		stateStoreRow(t, "freeze", func(_ context.Context, r *Run) (func() error, string) {
			p := r.Store.Deployment().Primary()
			return func() error { return p.Signal(syscall.SIGSTOP) }, "sigstop"
		}, func(_ context.Context, r *Run) func() error {
			p := r.Store.Deployment().Primary()
			return func() error { return p.Signal(syscall.SIGCONT) }
		})
	})
}

// stateStoreRow runs one of the row's runs: stop stops the State Store
// and names the fault, resume brings it back.
func stateStoreRow(t *testing.T, variant string, stop func(context.Context, *Run) (func() error, string), resume func(context.Context, *Run) func() error) {
	s := saturation(t)
	plan := PlanLimits(suite.Profile.LoadShare*s, suite.Profile.Nodes)
	r := suite.Start(t, Spec{Name: RowStateStore + "/" + variant, Bundle: plan.Apply, SampleEvery: 250 * time.Millisecond})
	l := r.StartLoad(LoadSpec{Duration: Baseline + 2*FaultPhase + RecoveryTime, Streams: plan.Streams()})
	r.Begin(l)
	base := r.Hold(l, PhaseBaseline, Baseline)
	slowAt := r.Inject("slow", "state-store", func() error { return r.SetStoreMode("main", slowStore) })
	slow := r.Hold(l, PhaseSlow, FaultPhase)
	stopFn, kind := stop(r.Context(), r)
	stopAt := r.Inject(kind, "state-store", func() error {
		if err := r.SetStoreMode("main", faultproxy.Pass{}); err != nil {
			return err
		}
		return stopFn()
	})
	stopped := r.Hold(l, PhaseStopped, FaultPhase)
	end := r.Inject("resume", "state-store", resume(r.Context(), r))
	rec := r.Hold(l, PhaseRecovery, RecoveryTime)
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, st := range l.Streams {
		r.IntervalLog(st.Spec.Name, st)
	}
	r.Baseline(l, base)
	limited, closed := l.StreamNamed("limited"), l.StreamNamed("closed")
	nodes := r.NodeNames()
	r.Add(
		Check("baseline RZ-STS responses", Functional, float64(CodesIn(closed, base.Start, base.End, STSCodes()...)), EQ, 0),
		Check("requests waiting a second State Store deadline", Functional, r.slowRequests(slowAt, stopped.End, 2*StateStoreTimeout, "limited", "limited-closed"), EQ, 0),
		Check("Nodes whose breaker opened while the State Store was slow", Functional, float64(len(r.Sampler.ReasonRaised(nodes, BreakerReason, slowAt, slow.End))), EQ, float64(len(nodes))),
		Check("Nodes whose breaker was open while the State Store was stopped", Functional, float64(len(r.Sampler.ReasonRaised(nodes, BreakerReason, stopAt, stopped.End))), EQ, float64(len(nodes))),
		r.AdmissionCheck("fail-open admissions per key and window", limited, time.Second, slowAt, stopped.End, int64(r.Nodes)*int64(plan.Ceiling)),
		Check("closed-Policy RZ-STS responses during the fault", Functional, float64(CodesIn(closed, slowAt, stopped.End, STSCodes()...)), GT, 0),
		Check("closed-Policy admissions after the first RZ-STS", Functional, float64(AdmittedAfter(closed, slowAt, stopped.End, STSCodes()...)), EQ, 0),
	)
	for _, route := range []string{"limited", "limited-closed"} {
		r.Breakdown("over_two_deadlines_baseline_"+route, r.slowRequests(base.Start, base.End, 2*StateStoreTimeout, route))
		r.Breakdown("over_two_deadlines_slow_"+route, r.slowRequests(slowAt, slow.End, 2*StateStoreTimeout, route))
		r.Breakdown("over_two_deadlines_stopped_"+route, r.slowRequests(stopAt, stopped.End, 2*StateStoreTimeout, route))
	}
	// The breaker's cause while stopped: connection errors after the
	// SIGKILL, timeouts while frozen.
	errs, timeouts := r.CallsIncrease(stopAt, stopped.End, "error"), r.CallsIncrease(stopAt, stopped.End, "timeout")
	r.Breakdown("stopped_call_errors", errs)
	r.Breakdown("stopped_call_timeouts", timeouts)
	if variant == "kill" {
		r.Add(Check("State Store connection errors after the SIGKILL", Functional, errs, GT, 0))
	} else {
		r.Add(Check("State Store timeouts while frozen", Functional, timeouts, GT, 0))
	}
	r.RecoveryCriteria(l, base, rec, end)
	if closedAt, ok := r.Sampler.ReasonClearedAt(nodes, BreakerReason, end); ok && closedAt.Before(rec.End) {
		r.Add(r.AdmissionCheck("healthy admissions per key and window after the breakers closed", limited, time.Second, closedAt, rec.End, 2*int64(plan.Requests)))
	}
	r.Finish()
}

// slowRequests returns how many requests of the Routes named routes the
// Nodes timed above d over (from, to], by their request duration
// histogram on the Nodes' clock: d is one of its bucket bounds.
func (r *Run) slowRequests(from, to time.Time, d time.Duration, routes ...string) float64 {
	le := strconv.FormatFloat(d.Seconds(), 'g', -1, 64)
	total := 0.0
	for _, route := range routes {
		for _, n := range r.Sampler.Nodes() {
			all := Increase(r.Sampler.Trace(n, "ruralz_http_request_duration_seconds_count", promtext.L("route", route)), from, to)
			fast := Increase(r.Sampler.Trace(n, "ruralz_http_request_duration_seconds_bucket", promtext.L("route", route, "le", le)), from, to)
			total += all - fast
		}
	}
	return total
}
