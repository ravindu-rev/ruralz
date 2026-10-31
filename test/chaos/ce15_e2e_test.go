// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// FirstSeenBudget is the first-seen GCRA calls a file-mode Node makes per
// second (target).
const FirstSeenBudget = 200

// floodCeiling is /flood's per-Node ceiling in CE-15.
const floodCeiling = 10

// TestCE15KeyRotationFlood is CE-15: /flood offered the profile's flood
// rate, min(100,000, S) new keys per second at nightly (100,000 at
// target), every request carrying a new key. Each key admits at most
// N_serving × ceiling per window; each Node's first-seen GCRA calls stay
// within the first-seen budget of 200 per second, a bucket that starts
// full, later keys being admitted on local-only entries; no key with a
// GCRA answer is evicted;
// and the GCRA calls the Nodes count match those the counting RESP
// proxies saw (docs/architecture/11-scalability-and-distributed-state.md
// "Chaos experiments").
func TestCE15KeyRotationFlood(t *testing.T) {
	s := saturation(t)
	flood := suite.Profile.FloodRate(s)
	r := suite.Start(t, Spec{
		Name: CE15, StateStore: topology.StateStoreOptions{RESPProxy: true}, SampleEvery: 250 * time.Millisecond,
		Bundle: func(b *BundleParams) {
			b.Flood = Limit{Requests: floodCeiling * suite.Profile.Nodes, Ceiling: floodCeiling}
		},
	})
	ctx := r.Context()
	base := r.StartLoad(LoadSpec{Duration: Baseline + FaultPhase + RecoveryTime, Streams: []StreamSpec{
		{Name: "plain", Path: PathPlain, Rate: max(10, s/100)},
	}})
	calls := r.StartCallLog(ctx, "main", PolicyFlood)
	r.Begin(base)
	bl := r.Hold(base, PhaseBaseline, Baseline)
	fl := r.StartLoad(LoadSpec{Warmup: time.Millisecond, Duration: FaultPhase, Streams: []StreamSpec{
		{
			Name: "flood", Path: PathFlood, Rate: flood, KeyHeader: KeyHeader,
			Keys: &lab.KeySpec{Kind: lab.KeysRotate, Prefix: "flood-", PerKey: 1}, Counted: Decisions(),
		},
	}})
	fault := r.Hold(base, PhaseFault, FaultPhase)
	if err := fl.Wait(); err != nil {
		t.Fatal(err)
	}
	rec := r.Hold(base, PhaseRecovery, RecoveryTime)
	if err := base.Wait(); err != nil {
		t.Fatal(err)
	}
	got, missed := calls.Stop()
	st := fl.Streams[0]
	r.IntervalLog("plain", base.Streams[0])
	r.IntervalLog("flood", st)
	r.Baseline(base, bl)
	r.Add(r.AdmissionCheck("admissions per key and window", st, time.Second, fault.Start, fault.End, int64(r.Nodes)*floodCeiling))

	// First-seen GCRA calls per Node: every flood key is new, so each of
	// its calls spent a unit of its Node's budget, between the Node
	// getting the request and the counting proxy reading the call. The
	// budget is a bucket of FirstSeenBudget units refilled at that rate
	// per second, starting full, so a Node's calls keep its law.
	sent := map[string]time.Time{}
	for _, q := range st.Requests() {
		sent[KeyTag(q.Key)] = q.Sent
	}
	perNode := map[string][]Interval{}
	worstWindow := int64(0)
	for _, c := range got {
		lo, _ := r.NodeSpan(c.At)
		if at, ok := sent[keys.HashTag(c.Key)]; ok {
			lo, _ = r.NodeSpan(at)
		}
		_, hi := r.NodeSpan(c.At)
		perNode[c.Node] = append(perNode[c.Node], Interval{Lo: lo, Hi: hi})
	}
	excess := 0.0
	for n, events := range perNode {
		e, slack := BucketLawOf(events, FirstSeenBudget, FirstSeenBudget)
		w := callsPerNodeWindow(got, n, r.NodeSpan, time.Second, fault.Start, fault.End.Add(time.Second))
		t.Logf("chaos: %s: %d GCRA commands on flood keys, fullest window %d, %.0f above the budget's law (decision interval %v)", n, len(events), w, e, slack)
		excess = max(excess, e)
		worstWindow = max(worstWindow, w)
	}
	r.Breakdown("first_seen_calls_fullest_window", float64(worstWindow))
	nodeCalls := 0.0
	for _, n := range r.Sampler.Nodes() {
		nodeCalls += Increase(r.Sampler.Trace(n, "ruralz_state_calls_total", promtext.L("op", "gcra")), fault.Start, rec.Start.Add(5*time.Second))
	}
	r.Breakdown("proxy_flood_gcra_commands", float64(len(got)))
	r.Breakdown("node_gcra_calls", nodeCalls)
	r.Add(
		Check("commands the RESP proxies' logs dropped", Functional, float64(missed), EQ, 0),
		Check("first-seen GCRA calls of a Node above its budget, 200 per second beyond the full budget", Functional, excess, EQ, 0),
		Check("allow_local decisions on flood keys", Functional, r.SumIncrease("ruralz_ratelimit_decisions_total", promtext.L("policy", PolicyFlood, "result", "allow_local"), fault.Start, rec.Start.Add(time.Second)), GT, 0),
		Check("main-segment evictions", Functional, r.SumIncrease("ruralz_ratelimit_bucket_evictions_total", promtext.L("segment", "main"), bl.Start, rec.End), EQ, 0),
		Check("GCRA calls the Nodes counted minus those the proxies saw", Functional, nodeCalls-float64(len(got)), EQ, 0),
	)
	r.Finish()
}

// callsPerNodeWindow returns the fullest window of the Nodes' clock among
// the calls of Node node read in [from, to).
func callsPerNodeWindow(calls []Call, node string, span func(time.Time) (lo, hi time.Time), window time.Duration, from, to time.Time) int64 {
	per := map[int64]int64{}
	worst := int64(0)
	for _, c := range calls {
		if c.Node != node || c.At.Before(from) || !c.At.Before(to) {
			continue
		}
		lo, _ := span(c.At)
		w := lo.UnixNano() / int64(window)
		per[w]++
		worst = max(worst, per[w])
	}
	return worst
}
