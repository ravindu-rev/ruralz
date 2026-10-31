// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// CE-5's bounds from the SIGKILL, in Node time (target).
const (
	ReconnectBound   = 10 * time.Second
	FailureModeBound = 15 * time.Second
)

// TestCE5PrimaryKilled is CE-5 and V-7: the State Store primary SIGKILLed
// after the baseline at 50% load, on the profile's CE-5 Node count, under
// each topology. Under standalone the test promotes the replica and the
// Nodes' endpoint follows it; under sentinel the sentinels fail over on
// their own; under cluster the killed primary's replica takes over. From
// the SIGKILL every Node makes its first successful State Store call on
// the killed shard within 10 s and decides by failureMode for at most
// 15 s, from its first to its last fail-open or RZ-STS decision; a key
// admits at most L × its admitted rate above the healthy bound, L being
// the replica's lag sampled every 100 ms before the kill
// (docs/architecture/11-scalability-and-distributed-state.md "Chaos
// experiments"). These are the Behavior of the failure catalog row FC-12,
// and each run adds its Detection (FC12Criteria).
func TestCE5PrimaryKilled(t *testing.T) {
	t.Run("standalone", func(t *testing.T) {
		ce5(t, "standalone", topology.StateStoreOptions{Replica: true}, func(ctx context.Context, r *Run, _ int) error {
			return r.Store.Promote(ctx)
		})
	})
	t.Run("sentinel", func(t *testing.T) {
		ce5(t, "sentinel", topology.StateStoreOptions{Kind: redisserver.ProcessSentinel}, func(ctx context.Context, r *Run, _ int) error {
			return r.Store.Deployment().AwaitFailover(ctx)
		})
	})
	t.Run("cluster", func(t *testing.T) {
		ce5(t, "cluster", topology.StateStoreOptions{Kind: redisserver.ProcessCluster}, func(ctx context.Context, r *Run, shard int) error {
			return r.Store.Deployment().ClusterFailover(ctx, shard)
		})
	})
}

// ce5 runs one CE-5 variant; failover follows the kill of shard's
// primary.
func ce5(t *testing.T, variant string, store topology.StateStoreOptions, failover func(context.Context, *Run, int) error) {
	s := saturation(t)
	plan := PlanLimits(suite.Profile.LoadShare*s, suite.Profile.CE5Nodes)
	r := suite.Start(t, Spec{Name: CE5 + "/" + variant, Nodes: suite.Profile.CE5Nodes, StateStore: store, Bundle: plan.Apply, SampleEvery: 100 * time.Millisecond})
	ctx := r.Context()
	dep := r.Store.Deployment()
	// The killed shard holds the most Consumers' rate limit keys.
	onShard := map[int][]string{}
	for _, c := range Consumers() {
		i, err := ShardOf(ctx, dep, LimitedKeySlot(c))
		if err != nil {
			t.Fatal(err)
		}
		onShard[i] = append(onShard[i], c)
	}
	shard := 0
	for i, cs := range onShard {
		if len(cs) > len(onShard[shard]) || (len(cs) == len(onShard[shard]) && i < shard) {
			shard = i
		}
	}
	victims := onShard[shard]
	primary := dep.Nodes()[shard]
	l := r.StartLoad(LoadSpec{Duration: Baseline + FaultPhase + RecoveryTime, Streams: plan.Streams()})
	r.Begin(l)
	lag := TrackLag(ctx, primary)
	base := r.Hold(l, PhaseBaseline, Baseline)
	samples := lag.Stop()
	L, lagOK := ReplicaLag(samples)
	r.Breakdown("replica_lag_ms", ms(L))
	scripts, err := ReadScriptStats(ctx, ReplicaOf(dep, shard))
	if err != nil {
		t.Fatal(err)
	}
	kill := r.Inject("sigkill", primary.Name(), func() error { return primary.Kill(ctx) })
	r.Inject("failover", primary.Name(), func() error { return failover(ctx, r, shard) })
	fault := r.Hold(l, PhaseFault, FaultPhase)
	rec := r.Hold(l, PhaseRecovery, RecoveryTime)
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, st := range l.Streams {
		r.IntervalLog(st.Spec.Name, st)
	}
	r.Baseline(l, base)
	r.Add(Holds("replica lag sampled before the kill", Functional, lagOK))
	killNode := r.NodeTime(kill)
	nodes := r.NodeNames()

	// Reconnection: each Node's first closed-Policy request on the killed
	// shard's keys sent after the SIGKILL that took a GCRA answer from
	// the shard, admitted or denied by GCRA (RZ-RL-002), its first
	// successful State Store call there; a local denial makes no call,
	// and a closed Policy never admits without an answer.
	closed := l.StreamNamed("closed")
	first := map[string]time.Time{}
	for _, q := range closed.Requests() {
		if q.Sent.Before(kill) || !slices.Contains(victims, q.Key) || (!Admitted(q) && q.Code != CodeGCRADenied) {
			continue
		}
		n := r.attribute(false, q.Local)
		if t0, ok := first[n]; n != "" && (!ok || q.Done.Before(t0)) {
			first[n] = q.Done
		}
	}
	worstReconnect := time.Duration(0)
	lastReconnect := kill
	for _, n := range nodes {
		at, ok := first[n]
		if !ok {
			t.Logf("chaos: %s made no successful call on the killed shard after the SIGKILL", n)
			worstReconnect = time.Duration(math.MaxInt64)
			continue
		}
		worstReconnect = max(worstReconnect, r.NodeTime(at).Sub(killNode))
		if at.After(lastReconnect) {
			lastReconnect = at
		}
	}
	r.Add(Check("slowest Node's reconnection after the SIGKILL, seconds", Functional, worstReconnect.Seconds(), LE, ReconnectBound.Seconds()))
	nodeOf := func(q Request) string { return r.attribute(false, q.Local) }
	t.Logf("chaos: closed-Policy outcomes on the killed shard's keys per Node and second after the SIGKILL:%s",
		FormatTimeline(OutcomeTimeline(closed, nodeOf, victims, kill, kill.Add(FailureModeBound), time.Second), time.Second))

	// failureMode: each Node's first to last fail-open or fail-closed
	// decision after the SIGKILL.
	worstMode := time.Duration(0)
	for _, n := range nodes {
		var rises []Point
		for _, fam := range []string{"ruralz_ratelimit_decisions_total", "ruralz_quota_decisions_total"} {
			for _, res := range []string{"fail_open", "fail_closed"} {
				rises = append(rises, Rises(Between(r.Sampler.Trace(n, fam, promtext.L("result", res)), kill, rec.End))...)
			}
		}
		if len(rises) == 0 {
			continue
		}
		lo, hi := rises[0].Node, rises[0].Node
		for _, p := range rises {
			if p.Node.Before(lo) {
				lo = p.Node
			}
			if p.Node.After(hi) {
				hi = p.Node
			}
		}
		worstMode = max(worstMode, hi.Sub(lo))
	}
	r.Add(Check("longest failureMode period of a Node, seconds", Functional, worstMode.Seconds(), LE, FailureModeBound.Seconds()))

	// Extra admissions: over the windows from the SIGKILL to a second
	// after the last reconnection, each victim key admits at most the
	// healthy bound, (W + 1) × requests, plus L × its admitted rate.
	limited := l.StreamNamed("limited")
	from, to := killNode.Truncate(time.Second), r.NodeTime(lastReconnect).Add(2*time.Second).Truncate(time.Second)
	windows := int64(to.Sub(from) / time.Second)
	admitted := map[string]int64{}
	for _, q := range limited.Requests() {
		if !Admitted(q) || !slices.Contains(victims, q.Key) {
			continue
		}
		if at := r.NodeTime(q.Sent); !at.Before(from) && at.Before(to) {
			admitted[q.Key]++
		}
	}
	worstExtra := math.Inf(-1)
	allowed := float64((windows + 1) * int64(plan.Requests))
	lagExtra := L.Seconds() * float64(plan.Requests)
	for _, c := range victims {
		worstExtra = max(worstExtra, float64(admitted[c])-allowed-lagExtra)
		t.Logf("chaos: %s admitted %d over %d windows; healthy bound %.0f, lag extra %.1f", c, admitted[c], windows, allowed, lagExtra)
	}
	r.Add(Check("admissions per victim key above the healthy bound plus L × its admitted rate", Functional, max(worstExtra, 0), EQ, 0))
	promoted, err := ReadScriptStats(ctx, r.Primaries()[shard])
	if err != nil {
		t.Fatal(err)
	}
	r.Breakdown("noscript_replies_on_the_promoted_primary", float64(promoted.NoScript-scripts.NoScript))
	r.Add(FC12Criteria(len(nodes), scripts, promoted)...)
	r.RecoveryCriteria(l, base, rec, fault.End)
	r.Finish()
}
