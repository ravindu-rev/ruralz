// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package gameday

import (
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
	"github.com/ravindu-rev/ruralz/test/chaos"
)

// TestGD3StateStoreOutage is GD-3, State Store outage
// (docs/operations/04-high-availability-and-disaster-recovery.md "Game
// days"): CE-4's fault, every Node's path to the State Store black-holed
// for 60 s at 50% load, each key offered twice its bound. Admission stays
// within the fail-open bounds: per key and window N × the declared
// ceiling on /limited and N × requests on /undeclared, the file-mode bound
// without a ceiling; the closed ratelimit and quota of /limited-closed
// answer RZ-STS-001, then RZ-STS-003 once the breakers open, admitting
// nothing, as closed Token Budgets do from M3; afterwards the breakers
// close and errors return to zero.
func TestGD3StateStoreOutage(t *testing.T) {
	s := saturation(t)
	plan := chaos.PlanLimits(suite.Profile.LoadShare*s, suite.Profile.Nodes)
	r := suite.Start(t, chaos.Spec{Name: GD3, Bundle: plan.Apply})
	l := r.StartLoad(chaos.LoadSpec{Duration: chaos.Baseline + chaos.FaultPhase + chaos.RecoveryTime, Streams: plan.Streams()})
	r.Begin(l)
	base := r.Hold(l, chaos.PhaseBaseline, chaos.Baseline)
	start := r.Inject("blackhole", "state-store", func() error { return r.SetStoreMode("main", faultproxy.Blackhole{}) })
	fault := r.Hold(l, chaos.PhaseFault, chaos.FaultPhase)
	end := r.Inject("pass", "state-store", func() error { return r.SetStoreMode("main", faultproxy.Pass{}) })
	rec := r.Hold(l, chaos.PhaseRecovery, chaos.RecoveryTime)
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	logStreams(r, l)
	r.Baseline(l, base)
	limited, closed, undeclared := l.StreamNamed("limited"), l.StreamNamed("closed"), l.StreamNamed("undeclared")
	n := int64(r.Nodes)
	r.Add(
		chaos.Check("baseline RZ-STS responses", chaos.Functional, float64(chaos.CodesIn(closed, base.Start, base.End, chaos.STSCodes()...)), chaos.EQ, 0),
		r.AdmissionCheck("declared-ceiling fail-open admissions per key and window", limited, time.Second, start, fault.End, n*int64(plan.Ceiling)),
		r.AdmissionCheck("file-mode fail-open admissions per key and window", undeclared, time.Second, start, fault.End, n*int64(plan.UndeclaredRequests)),
	)
	order := chaos.CodeOrder(closed, start, fault.End, chaos.STSCodes()...)
	t.Logf("gameday: %s: closed-Policy codes in the order they first appeared: %v", GD3, order)
	r.Add(
		chaos.Holds("closed Policies answer RZ-STS-001 first", chaos.Functional, len(order) > 0 && order[0] == chaos.CodeSTSTimeout),
		chaos.Holds("closed Policies answer RZ-STS-003 once the breakers open", chaos.Functional, slices.Contains(order, chaos.CodeSTSOpen)),
		chaos.Check("closed-Policy admissions after the first RZ-STS", chaos.Functional, float64(chaos.AdmittedAfter(closed, start, fault.End, chaos.STSCodes()...)), chaos.EQ, 0),
	)
	nodes := r.NodeNames()
	raised := r.Sampler.ReasonRaised(nodes, chaos.BreakerReason, start, end)
	r.Add(chaos.Check("Nodes raising state_store_breaker_open in the fault", chaos.Functional, float64(len(raised)), chaos.EQ, float64(len(nodes))))
	r.RecoveryCriteria(l, base, rec, end)
	r.Finish()
}
