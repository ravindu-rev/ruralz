// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// Failure catalog rows the Chaos job asserts on the runs of this
// package's experiments (docs/operations/04-high-availability-and-disaster-recovery.md
// "Failure catalog"; docs/engineering/03-testing-and-quality-strategy.md
// "Drills and failure rows"): FC-10 on CE-4's run, FC-12 on CE-5's and
// FC-13 on CE-6's. Their criteria carry the row in their names.
const (
	FC10 = "FC-10"
	FC12 = "FC-12"
	FC13 = "FC-13"
)

// BreakerHalfOpenMin is the shortest delay after which an open State
// client breaker half-opens and lets one probe call through (target): a
// probe that meets a black-holed State Store waits its deadline, so a
// Node's requests wait at most one deadline per this delay once its
// breaker opened (docs/architecture/11-scalability-and-distributed-state.md
// "State client and RZ-STS error codes").
const BreakerHalfOpenMin = time.Second

// LimitRoutes are the Routes whose Policies call the State Store on every
// request of a LimitPlan's streams, by their route label.
func LimitRoutes() []string { return []string{"limited", "limited-closed", "undeclared"} }

// SumIncrease returns how much a counter grew over (from, to] on every
// Node together.
func (r *Run) SumIncrease(name string, match promtext.Labels, from, to time.Time) float64 {
	total := 0.0
	for _, n := range r.Sampler.Nodes() {
		total += Increase(r.Sampler.Trace(n, name, match), from, to)
	}
	return total
}

// RequestsOver returns how many requests of the Routes named routes the
// Nodes timed above d over (from, to], by their request duration
// histogram on the Nodes' clock: d is one of its bucket bounds.
func (r *Run) RequestsOver(from, to time.Time, d time.Duration, routes ...string) float64 {
	le := strconv.FormatFloat(d.Seconds(), 'g', -1, 64)
	total := 0.0
	for _, route := range routes {
		for _, n := range r.Sampler.Nodes() {
			all := Increase(r.Sampler.Trace(n, catalog.HTTPRequestDurationSeconds+"_count", promtext.L("route", route)), from, to)
			fast := Increase(r.Sampler.Trace(n, catalog.HTTPRequestDurationSeconds+"_bucket", promtext.L("route", route, "le", le)), from, to)
			total += all - fast
		}
	}
	return total
}

// HalfOpenProbes bounds the requests that wait a State Store deadline over
// d of Node time while every one of nodes Nodes' breakers stays open on a
// black-holed State Store: one half-open probe per Node and
// BreakerHalfOpenMin, the first counted at once.
func HalfOpenProbes(nodes int, d time.Duration) float64 {
	if nodes <= 0 || d <= 0 {
		return 0
	}
	return float64(nodes) * math.Ceil(float64(d)/float64(BreakerHalfOpenMin))
}

// sampleEvery returns the run's sampling period.
func (r *Run) sampleEvery() time.Duration {
	if r.Spec.SampleEvery > 0 {
		return r.Spec.SampleEvery
	}
	return DefaultSampleEvery
}

// AddFC10 adds FC-10's criteria to a run of a LimitPlan's streams l whose
// every Node's State Store path was black-holed from start to end, after
// the baseline base and before the recovery rec (CE-4). Its Behavior: a
// request waits at most one State Store deadline, and none once the
// breaker opens beyond the half-open probes; the fail-open rules, which
// CE-4's admission criteria assert. Its Detection: the breaker reason,
// which CE-4 asserts on every Node; ruralz_state_ops_total, whose rate
// falls on every Node once its breaker opened, since an open breaker skips
// its calls without a round trip; RZ-STS codes only under closed.
func (r *Run) AddFC10(l *Load, base, rec Phase, start, end time.Time) {
	r.T.Helper()
	nodes := r.NodeNames()
	r.Add(Check(FC10+": limit-Route requests waiting a second State Store deadline", Functional,
		r.RequestsOver(start, end, 2*StateStoreTimeout, LimitRoutes()...), EQ, 0))
	raised := r.Sampler.ReasonRaised(nodes, BreakerReason, start, end)
	if len(raised) < len(nodes) {
		r.Add(Holds(FC10+": every Node's breaker opened", Functional, false))
	} else {
		// From a sample after the last Node raised the reason, so no request
		// counted began before its Node's breaker opened.
		open := start
		for _, at := range raised {
			if at.After(open) {
				open = at
			}
		}
		open = open.Add(r.sampleEvery())
		r.Add(Check(FC10+": limit-Route requests waiting a deadline once every breaker opened", Functional,
			r.RequestsOver(open, end, StateStoreTimeout, LimitRoutes()...), LE, HalfOpenProbes(len(nodes), r.NodeTime(end).Sub(r.NodeTime(open)))))
		fewer := 0
		for _, n := range nodes {
			tr := r.Sampler.Trace(n, catalog.StateOpsTotal, nil)
			baseRate := Increase(tr, base.Start, base.End) / base.End.Sub(base.Start).Seconds()
			faultRate := Increase(tr, open, end) / end.Sub(open).Seconds()
			r.T.Logf("chaos: %s: %s: %.1f State Store operations per second in the baseline, %.1f once its breaker opened", FC10, n, baseRate, faultRate)
			if faultRate < baseRate {
				fewer++
			}
		}
		r.Add(Check(FC10+": Nodes whose State Store operations fell once every breaker opened", Functional, float64(fewer), EQ, float64(len(nodes))))
	}
	var sts int64
	for _, st := range l.Streams {
		if st.Spec.Path != PathLimitedClosed {
			sts += CodesIn(st, base.Start, rec.End, STSCodes()...)
		}
	}
	r.Add(Check(FC10+": RZ-STS answers on Routes whose Policies fail open", Functional, float64(sts), EQ, 0))
}

// ScriptStats are the counts of a State Store server that show its
// clients' script loads: SCRIPT LOAD calls (INFO commandstats) and
// NOSCRIPT error replies (INFO errorstats).
type ScriptStats struct {
	Loads, NoScript int64
}

// ScriptLoadCalls returns the SCRIPT LOAD calls among a server's command
// statistics: the script|load subcommand, or the script command of a
// server that does not count subcommands apart.
func ScriptLoadCalls(stats map[string]CommandStat) int64 {
	if st, ok := stats["script|load"]; ok {
		return st.Calls
	}
	return stats["script"].Calls
}

// ErrorReplies returns the error replies of one prefix, such as
// "NOSCRIPT", in a server's INFO errorstats fields.
func ErrorReplies(info map[string]string, prefix string) int64 {
	for kv := range strings.SplitSeq(info["errorstat_"+prefix], ",") {
		if v, ok := strings.CutPrefix(kv, "count="); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err == nil {
				return n
			}
		}
	}
	return 0
}

// ReplicaOf returns the replica a failover of shard promotes: a cluster
// shard's replica, else the deployment's one replica; nil without one.
func ReplicaOf(d *redisserver.Deployment, shard int) *redisserver.Server {
	rs := d.Replicas()
	switch {
	case d.Topology() == statestore.TopologyCluster && shard >= 0 && shard < len(rs):
		return rs[shard]
	case len(rs) > 0:
		return rs[0]
	default:
		return nil
	}
}

// ReadScriptStats reads a server's ScriptStats.
func ReadScriptStats(ctx context.Context, s *redisserver.Server) (ScriptStats, error) {
	if s == nil {
		return ScriptStats{}, errors.New("chaos: no server to read script statistics from")
	}
	cmds, err := CommandStats(ctx, s)
	if err != nil {
		return ScriptStats{}, err
	}
	errs, err := s.Info(ctx, "errorstats")
	if err != nil {
		return ScriptStats{}, err
	}
	return ScriptStats{Loads: ScriptLoadCalls(cmds), NoScript: ErrorReplies(errs, "NOSCRIPT")}, nil
}

// FC12Criteria returns FC-12's Detection criterion on CE-5's run: from the
// server a failover promoted, read before the kill (before) and after the
// fault (after), every one of nodes Nodes loaded its scripts there, since a
// Node runs SCRIPT LOAD at every connect, reconnect and failover before its
// first EVALSHA, so a NOSCRIPT reply only ever precedes a load
// (docs/architecture/11-scalability-and-distributed-state.md "State Store
// availability"). CE-5's criteria assert FC-12's Behavior: failureMode for
// at most 15 s and at most the lag's extra admissions per key.
func FC12Criteria(nodes int, before, after ScriptStats) []Criterion {
	return []Criterion{
		Check(FC12+": SCRIPT LOAD calls on the promoted primary after the failover", Functional, float64(after.Loads-before.Loads), GE, float64(nodes)),
	}
}

// FC13Criteria returns FC-13's criteria on a run of CE-6 under policy on
// the main connection or, with onCache, on the stateStore.cache one, that
// evicted keys and saw raising Nodes raise state_store_eviction_policy.
// FC-13's Behavior: stores skip above 70% of maxmemory and, once a
// noeviction deployment is full, limits open and closed Policies answer
// RZ-STS, which CE-6's main-noeviction criteria assert; eviction resets
// counters: an evicting main connection evicts keys once full. Its
// Detection: the reason, raised by every Node under allkeys-lru, which
// CE-6 asserts, and by none under noeviction, which every deployment
// holding limit keys MUST set.
func FC13Criteria(policy string, onCache bool, evicted int64, raising int) []Criterion {
	switch {
	case policy == "noeviction":
		return []Criterion{Check(FC13+": Nodes raising state_store_eviction_policy under noeviction", Functional, float64(raising), EQ, 0)}
	case policy == "allkeys-lru" && !onCache:
		return []Criterion{Check(FC13+": keys the full main connection evicted, resetting their counters", Functional, float64(evicted), GT, 0)}
	default:
		return nil
	}
}
