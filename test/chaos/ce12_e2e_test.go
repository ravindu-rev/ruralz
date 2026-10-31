// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// Phases of CE-12.
const (
	PhaseHotDeclared = "hot-declared"
	PhaseHotLocal    = "hot-local"
)

// MaxShardCPU is CE-12's bound on the hot shard's main-thread CPU share
// per second (target).
const MaxShardCPU = 0.70

// hotLocalFactor is how far above the declared phase's requests CE-12's
// localOnly phase sets its limit.
const hotLocalFactor = 2

// TestCE12HotKey is CE-12: the hot key offered the profile's hot rate,
// S at nightly (1,000,000 calls per second at target), first on /hot,
// with requests at the rate / 40 and a declared perNodeCeiling of
// requests / N_serving, then on /hot-local with config.localOnly and a
// limit above it, beside a low-rate co-tenant key on the same shard. The
// GCRA calls on the hot key stay within 2 × N_serving × ceiling per
// window, none with localOnly, and no breaker opens for the co-tenant;
// the functional run on the controllable-clock build counts them through
// the counting RESP proxies, and a measurement run on the release build
// records the shard's script CPU, under 70% (target)
// (docs/architecture/11-scalability-and-distributed-state.md "Chaos
// experiments").
func TestCE12HotKey(t *testing.T) {
	t.Run("functional", func(t *testing.T) { ce12(t, false) })
	t.Run("cpu", func(t *testing.T) { ce12(t, true) })
}

func ce12(t *testing.T, measurement bool) {
	s := saturation(t)
	hot := suite.Profile.HotRate(s)
	requests := max(1, int(hot/suite.Profile.HotRatio))
	name := CE12
	if measurement {
		name += "/cpu"
	}
	nodes := suite.Profile.Nodes
	cotenantRate := max(10, hot/100)
	r := suite.Start(t, Spec{
		Name: name, Measurement: measurement, MeasureKind: CPU, StateStore: topology.StateStoreOptions{RESPProxy: true},
		Bundle: func(b *BundleParams) {
			b.Hot = Limit{Requests: requests, Ceiling: max(1, requests/nodes)}
			b.HotLocal = Limit{Requests: hotLocalFactor * requests, Ceiling: max(1, hotLocalFactor*requests/nodes)}
			b.Cotenant = int(4 * cotenantRate)
		},
	})
	ctx := r.Context()
	base := r.StartLoad(LoadSpec{Duration: Baseline + 2*FaultPhase + RecoveryTime, Streams: []StreamSpec{
		{Name: "cotenant", Path: PathCotenant, Rate: cotenantRate},
	}})
	var calls *CallLog
	if !measurement {
		calls = r.StartCallLog(ctx, "main", PolicyHot, PolicyHotLocal, PolicyCotenant)
		defer calls.Stop()
	}
	r.Begin(base)
	bl := r.Hold(base, PhaseBaseline, Baseline)
	shard := r.Primaries()[0]
	statsBefore, statsErr := CommandStats(ctx, shard)
	cpu := trackCPU(ctx, shard)
	hotLoad := r.StartLoad(LoadSpec{Warmup: time.Millisecond, Duration: FaultPhase, Streams: []StreamSpec{
		{Name: "hot", Path: PathHot, Rate: hot, Counted: Decisions()},
	}})
	declared := r.Hold(base, PhaseHotDeclared, FaultPhase)
	if err := hotLoad.Wait(); err != nil {
		t.Fatal(err)
	}
	cpuDeclared := cpu.stop()
	statsAfter, err := CommandStats(ctx, shard)
	if err == nil && statsErr == nil {
		used := statsAfter["evalsha"].Usec - statsBefore["evalsha"].Usec
		r.Breakdown("cmdstat_evalsha_usec_per_s", float64(used)/declared.End.Sub(declared.Start).Seconds())
	}
	localLoad := r.StartLoad(LoadSpec{Warmup: time.Millisecond, Duration: FaultPhase, Streams: []StreamSpec{
		{Name: "hot-local", Path: PathHotLocal, Rate: hot, Counted: Decisions()},
	}})
	local := r.Hold(base, PhaseHotLocal, FaultPhase)
	if err := localLoad.Wait(); err != nil {
		t.Fatal(err)
	}
	rec := r.Hold(base, PhaseRecovery, RecoveryTime)
	if err := base.Wait(); err != nil {
		t.Fatal(err)
	}
	r.IntervalLog("cotenant", base.Streams[0])
	r.IntervalLog("hot", hotLoad.Streams[0])
	r.IntervalLog("hot-local", localLoad.Streams[0])
	r.Baseline(base, bl)
	if measurement {
		// The shard's script CPU while the hot key runs GCRA, a
		// measurement on the release build, gated only at target.
		worst := 0.0
		for _, f := range cpuDeclared {
			worst = max(worst, f)
		}
		r.Add(Check("hot shard main-thread CPU share, worst second", CPU, worst, LT, MaxShardCPU))
		r.Finish()
		return
	}

	// No breaker opens for the co-tenant, whose requests all succeed.
	all := r.NodeNames()
	r.Add(
		Check("Nodes raising state_store_breaker_open", Functional, float64(len(r.Sampler.ReasonRaised(all, BreakerReason, bl.Start, rec.End))), EQ, 0),
		Check("co-tenant failed requests", Functional, float64(LoadStats(base, bl.Start, rec.End).Failed), EQ, 0),
	)
	// A localOnly key admits N_serving × ceiling per window beyond the
	// full buckets the Nodes start it with: the summed bucket law.
	perWindow := float64(r.Nodes * max(1, hotLocalFactor*requests/nodes))
	excess, slack := BucketLaw(localLoad.Streams[0].Requests(), Admitted, r.NodeSpan, perWindow, perWindow, local.Start, local.End)
	r.Breakdown("hot_local_decision_interval_ms", ms(slack))
	r.Add(Check("localOnly admissions above N_serving × ceiling per window beyond the full buckets", Functional, excess, EQ, 0))
	r.Breakdown("hot_local_allow_local", r.SumIncrease("ruralz_ratelimit_decisions_total", promtext.L("policy", PolicyHotLocal, "result", "allow_local"), local.Start, local.End))
	got, missed := calls.Stop()
	ceiling := int64(max(1, requests/nodes))
	worstHot, totalHot := CallsPerWindow(got, KeyTag(HotKey), r.NodeSpan, time.Second, declared.Start, declared.End)
	_, totalLocal := CallsPerWindow(got, KeyTag(HotLocalKey), r.NodeSpan, time.Second, local.Start, local.End)
	r.Breakdown("hot_gcra_calls", float64(totalHot))
	r.Add(
		Check("commands the RESP proxies' logs dropped", Functional, float64(missed), EQ, 0),
		Check("GCRA calls on the hot key, fullest window", Functional, float64(worstHot), LE, float64(2*int64(r.Nodes)*ceiling)),
		Check("GCRA calls on the localOnly key", Functional, float64(totalLocal), EQ, 0),
	)
	r.Finish()
}

// cpuTrack samples a State Store server's main-thread CPU every second.
type cpuTrack struct {
	mu     sync.Mutex
	shares []float64
	cancel context.CancelFunc
	done   chan struct{}
}

// trackCPU starts sampling srv's main-thread CPU share per second.
func trackCPU(ctx context.Context, srv *redisserver.Server) *cpuTrack {
	ctx, cancel := context.WithCancel(ctx)
	ct := &cpuTrack{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(ct.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var last time.Duration
		var lastAt time.Time
		for {
			if used, err := MainThreadCPU(ctx, srv); err == nil {
				now := time.Now()
				if !lastAt.IsZero() {
					ct.mu.Lock()
					ct.shares = append(ct.shares, float64(used-last)/float64(now.Sub(lastAt)))
					ct.mu.Unlock()
				}
				last, lastAt = used, now
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return ct
}

// stop ends sampling and returns the per-second shares.
func (ct *cpuTrack) stop() []float64 {
	ct.cancel()
	<-ct.done
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return append([]float64(nil), ct.shares...)
}
