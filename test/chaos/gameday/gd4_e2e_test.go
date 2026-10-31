// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package gameday

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
	"github.com/ravindu-rev/ruralz/test/chaos"
)

// GD-4's partition: the cut Nodes' State Store paths black-holed, then
// refused, each for 60 s (target).
const (
	gd4Blackholed = "blackholed"
	gd4Refused    = "refused"
	gd4Span       = 60 * time.Second
)

// TestGD4StateStorePartition is GD-4, State Store partition, and the
// failure catalog row FC-11 on its run
// (docs/operations/04-high-availability-and-disaster-recovery.md "Game
// days" and "Failure catalog"): half the Nodes cut from the State Store
// at 50% load, each key offered twice its bound, their paths black-holed
// for 60 s, then refused for 60 s. Per key, the admissions the L4 balancer
// attributes to cut-off Nodes stay within the declared-ceiling and
// file-mode fail-open rows of the cut-off Nodes alone, and the connected
// Nodes' within the healthy GCRA row, 2 × requests in one window; for
// FC-11 the cut-off Nodes' closed Policies answer RZ-STS and admit
// nothing after it while the connected Nodes' answer none, and the
// breaker state differs across Nodes: every cut-off Node raises
// state_store_breaker_open in each part of the partition, no connected
// Node does.
func TestGD4StateStorePartition(t *testing.T) {
	s := saturation(t)
	plan := chaos.PlanLimits(suite.Profile.LoadShare*s, suite.Profile.Nodes)
	r := suite.Start(t, chaos.Spec{Name: GD4, Bundle: plan.Apply, SampleEvery: 250 * time.Millisecond})
	l := r.StartLoad(chaos.LoadSpec{Duration: chaos.Baseline + 2*gd4Span + chaos.RecoveryTime, Streams: plan.Streams()})
	r.Begin(l)
	base := r.Hold(l, chaos.PhaseBaseline, chaos.Baseline)
	nodes := r.NodeNames()
	cut := Cut(nodes)
	connected := slices.DeleteFunc(slices.Clone(nodes), func(n string) bool { return slices.Contains(cut, n) })
	set := func(m faultproxy.Mode) func() error { return func() error { return gd4Set(r, cut, m) } }
	start := r.Inject("blackhole", "state-store-of-cut-nodes", set(faultproxy.Blackhole{}))
	bh := r.Hold(l, gd4Blackholed, gd4Span)
	r.Inject("refuse", "state-store-of-cut-nodes", set(faultproxy.Refuse{}))
	rf := r.Hold(l, gd4Refused, gd4Span)
	end := r.Inject("pass", "state-store-of-cut-nodes", set(faultproxy.Pass{}))
	rec := r.Hold(l, chaos.PhaseRecovery, chaos.RecoveryTime)
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	logStreams(r, l)
	r.Baseline(l, base)
	node := nodeOf(r)
	r.Breakdown("cut_nodes", float64(len(cut)))
	nCut := int64(len(cut))
	admissions := func(name string, reqs []chaos.Request, bound int64) []chaos.Criterion {
		worst, _ := chaos.AdmissionBound(reqs, chaos.Admitted, r.NodeSpan, time.Second, start, rf.End, bound)
		if worst.Key != "" {
			t.Logf("gameday: %s: %s: fullest window %s at %s: %d admitted, bound %d", GD4, name, worst.Key, worst.Window.Format(time.RFC3339Nano), worst.Count, bound)
		}
		return AlsoFor(FC11, chaos.Check(name, chaos.Functional, float64(worst.Count), chaos.LE, float64(bound)))
	}
	cutLimited, connLimited := ByNode(l.StreamNamed("limited").Requests(), node, cut)
	cutUndeclared, connUndeclared := ByNode(l.StreamNamed("undeclared").Requests(), node, cut)
	r.Add(admissions("cut-off Nodes' admissions per key and window, declared ceiling", cutLimited, nCut*int64(plan.Ceiling))...)
	r.Add(admissions("cut-off Nodes' admissions per key and window, file mode without a ceiling", cutUndeclared, nCut*int64(plan.UndeclaredRequests))...)
	r.Add(admissions("connected Nodes' admissions per key and window, healthy GCRA", connLimited, 2*int64(plan.Requests))...)
	r.Add(admissions("connected Nodes' admissions per key and window without a ceiling, healthy GCRA", connUndeclared, 2*int64(plan.UndeclaredRequests))...)

	// The closed Policies: RZ-STS on cut-off Nodes only, and no admission
	// on a cut-off Node after its first RZ-STS.
	cutClosed, connClosed := ByNode(l.StreamNamed("closed").Requests(), node, cut)
	sts, admittedAfter := gd4ClosedAfterSTS(cutClosed, node, start, rf.End)
	var connSTS int64
	for _, q := range connClosed {
		if !q.Intended.Before(start) && q.Intended.Before(rf.End) && slices.Contains(chaos.STSCodes(), q.Code) {
			connSTS++
		}
	}
	r.Add(
		chaos.Check(FC11+": cut-off Nodes answering RZ-STS on closed Policies", chaos.Functional, float64(sts), chaos.EQ, float64(len(cut))),
		chaos.Check(FC11+": cut-off Nodes' closed-Policy admissions after their first RZ-STS", chaos.Functional, float64(admittedAfter), chaos.EQ, 0),
		chaos.Check(FC11+": connected Nodes' RZ-STS answers", chaos.Functional, float64(connSTS), chaos.EQ, 0),
	)
	// Detection: the breaker state differs across Nodes in each part.
	for _, p := range []chaos.Phase{bh, rf} {
		from := p.Start
		if p.Name == gd4Blackholed {
			from = start
		}
		r.Add(
			chaos.Check(fmt.Sprintf("%s: %s: cut-off Nodes raising state_store_breaker_open", FC11, p.Name), chaos.Functional, float64(len(r.Sampler.ReasonRaised(cut, chaos.BreakerReason, from, p.End))), chaos.EQ, float64(len(cut))),
			chaos.Check(fmt.Sprintf("%s: %s: connected Nodes raising state_store_breaker_open", FC11, p.Name), chaos.Functional, float64(len(r.Sampler.ReasonRaised(connected, chaos.BreakerReason, from, p.End))), chaos.EQ, 0),
		)
	}
	r.RecoveryCriteria(l, base, rec, end)
	r.Finish()
}

// gd4Set puts the State Store proxies of the named Nodes in mode m.
func gd4Set(r *chaos.Run, names []string, m faultproxy.Mode) error {
	for _, n := range r.Top.Nodes() {
		if !slices.Contains(names, n.Name()) {
			continue
		}
		ps, ok := n.StateStoreProxy("main")
		if !ok {
			return fmt.Errorf("gameday: %s has no State Store proxy", n.Name())
		}
		for _, p := range ps {
			if err := p.SetMode(m); err != nil {
				return fmt.Errorf("gameday: %s: %w", n.Name(), err)
			}
		}
	}
	return nil
}

// gd4ClosedAfterSTS returns, over closed-Policy requests due in [from, to),
// how many Nodes answered RZ-STS, and how many requests a Node admitted
// after its first RZ-STS answer completed.
func gd4ClosedAfterSTS(reqs []chaos.Request, node func(chaos.Request) string, from, to time.Time) (nodes int, admittedAfter int64) {
	first := map[string]time.Time{}
	in := func(q chaos.Request) bool { return !q.Intended.Before(from) && q.Intended.Before(to) }
	for _, q := range reqs {
		if !in(q) || !slices.Contains(chaos.STSCodes(), q.Code) {
			continue
		}
		if n := node(q); n != "" {
			if t, ok := first[n]; !ok || q.Done.Before(t) {
				first[n] = q.Done
			}
		}
	}
	for _, q := range reqs {
		if t, ok := first[node(q)]; ok && in(q) && chaos.Admitted(q) && q.Sent.After(t) {
			admittedAfter++
		}
	}
	return len(first), admittedAfter
}
