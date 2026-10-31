// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
)

// TestCE4StateStoreBlackholed is CE-4: every Node's path to the State
// Store black-holed for 60 s at 50% load, each key offered twice its
// bound. While it lasts, a key's admissions per window stay within N ×
// its declared ceiling on /limited and within N × requests on
// /undeclared, the file-mode bound without a ceiling, and the closed
// Policies of /limited-closed answer RZ-STS-001, then RZ-STS-003 once the
// breakers open, admitting nothing; afterwards the breakers close and
// errors return to zero
// (docs/architecture/11-scalability-and-distributed-state.md "Chaos
// experiments"). Derived ceilings join in M2 and Token Budgets in M3. The
// run also asserts the failure catalog row FC-10 (AddFC10).
func TestCE4StateStoreBlackholed(t *testing.T) {
	s := saturation(t)
	plan := PlanLimits(suite.Profile.LoadShare*s, suite.Profile.Nodes)
	r := suite.Start(t, Spec{Name: CE4, Bundle: plan.Apply})
	l := r.StartLoad(LoadSpec{Duration: Baseline + FaultPhase + RecoveryTime, Streams: plan.Streams()})
	r.Begin(l)
	base := r.Hold(l, PhaseBaseline, Baseline)
	start := r.Inject("blackhole", "state-store", func() error { return r.SetStoreMode("main", faultproxy.Blackhole{}) })
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
	limited, closed, undeclared := l.StreamNamed("limited"), l.StreamNamed("closed"), l.StreamNamed("undeclared")
	n := int64(r.Nodes)
	r.Add(
		Check("baseline RZ-STS responses", Functional, float64(CodesIn(closed, base.Start, base.End, STSCodes()...)), EQ, 0),
		r.AdmissionCheck("declared-ceiling fail-open admissions per key and window", limited, time.Second, start, fault.End, n*int64(plan.Ceiling)),
		r.AdmissionCheck("file-mode fail-open admissions per key and window", undeclared, time.Second, start, fault.End, n*int64(plan.UndeclaredRequests)),
	)
	order := CodeOrder(closed, start, fault.End, STSCodes()...)
	t.Logf("chaos: closed-Policy codes in the order they first appeared: %v", order)
	r.Add(
		Holds("closed Policies answer RZ-STS-001 first", Functional, len(order) > 0 && order[0] == CodeSTSTimeout),
		Holds("closed Policies answer RZ-STS-003 once the breakers open", Functional, slices.Contains(order, CodeSTSOpen)),
		Check("closed-Policy admissions after the first RZ-STS", Functional, float64(AdmittedAfter(closed, start, fault.End, STSCodes()...)), EQ, 0),
	)
	nodes := r.NodeNames()
	raised := r.Sampler.ReasonRaised(nodes, BreakerReason, start, end)
	r.Add(Check("Nodes raising state_store_breaker_open in the fault", Functional, float64(len(raised)), EQ, float64(len(nodes))))
	r.AddFC10(l, base, rec, start, end)
	r.RecoveryCriteria(l, base, rec, end)
	r.Finish()
}
