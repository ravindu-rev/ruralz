// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/respproxy"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// CE-16's load: cacheable 64 KiB responses on fresh URIs, so every
// request stores an entry, beside invalidating POSTs and quota-charged
// requests to /refunded, each refunded at onLog; and the cache State
// Store's script replies held 1 s while it lasts, so its stores drain
// slower than they arrive. The cache Policy's stateStoreTimeout outlasts
// the hold, so a held write is late, never failed. The cache State Store
// holds ce16MaxMemory under volatile-ttl, a policy the cache connection
// allows, which evicts the entries nearest expiry: the run's stores, GBs
// over its phases, then never outgrow the host or the leak scan's
// keyspace read.
const (
	ce16Body        = 64 << 10
	ce16Hold        = time.Second
	ce16Timeout     = 5 * time.Second
	ce16Invalidates = 50.0
	ce16Refunds     = 50.0
	ce16MaxMemory   = "128mb"
)

// TestCE16QueueSaturated is CE-16: cache stores saturate the post-commit
// queue while invalidations and Quota refunds run, on a dedicated
// stateStore.cache connection, where the Response Cache's memory rules 2
// and 3 are off. Stores drop for their full class, while no
// cache_invalidate and no refund write drops, and refunds keep being
// written (docs/architecture/11-scalability-and-distributed-state.md
// "Chaos experiments" and "Post-commit write queue"). Plugin writes join
// in M2.
func TestCE16QueueSaturated(t *testing.T) {
	s := saturation(t)
	rate := suite.Profile.LoadShare * s
	r := suite.Start(t, Spec{
		Name: CE16, SampleEvery: 250 * time.Millisecond,
		CacheStore: &topology.StateStoreOptions{RESPProxy: true, Config: []string{"maxmemory " + ce16MaxMemory, "maxmemory-policy volatile-ttl"}},
		Bundle:     func(b *BundleParams) { b.CacheTimeout = ce16Timeout },
		Mock: func(_ int, c *mockup.Config) {
			c.Behavior = mockup.Behavior{BodySize: ce16Body, Header: http.Header{"Cache-Control": {"max-age=600"}}}
		},
	})
	fresh := func(q *http.Request, seq uint64) { q.URL.Path = PathCached + "/fresh-" + strconv.FormatUint(seq, 10) }
	invalidate := func(q *http.Request, seq uint64) { q.URL.Path = PathCached + "/hot-" + strconv.FormatUint(seq%64, 10) }
	l := r.StartLoad(LoadSpec{Duration: Baseline + FaultPhase + RecoveryTime, Streams: []StreamSpec{
		{Name: "stores", Path: PathCached, Protocol: loadgen.H2C, Rate: max(1, rate-ce16Invalidates-ce16Refunds), Prepare: fresh},
		{Name: "invalidations", Path: PathCached, Method: http.MethodPost, Protocol: loadgen.H2C, Rate: ce16Invalidates, Prepare: invalidate},
		{
			Name: "refunds", Path: PathRefunded, Method: http.MethodPost, Body: []byte(`{}`), Protocol: loadgen.H2C, Rate: ce16Refunds,
			APIKeys: true, Counted: []string{CodeBodyRejected},
		},
	}})
	hold := func(d time.Duration) func() error {
		return func() error {
			for _, n := range r.Top.Nodes() {
				ps, _ := n.RESPProxy("cache")
				for _, p := range ps {
					var delays []respproxy.ReplyDelay
					if d > 0 {
						delays = append(delays, respproxy.ReplyDelay{Command: "EVALSHA", Delay: d})
					}
					if err := p.SetReplyDelays(delays...); err != nil {
						return err
					}
				}
			}
			return nil
		}
	}
	r.Begin(l)
	base := r.Hold(l, PhaseBaseline, Baseline)
	start := r.Inject("hold-script-replies", "cache-state-store", hold(ce16Hold))
	fault := r.Hold(l, PhaseFault, FaultPhase)
	r.Inject("release-script-replies", "cache-state-store", hold(0))
	rec := r.Hold(l, PhaseRecovery, RecoveryTime)
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, st := range l.Streams {
		r.IntervalLog(st.Spec.Name, st)
	}
	r.Baseline(l, base)
	dropped := func(kind string) float64 {
		return r.SumIncrease("ruralz_state_writes_dropped_total", promtext.L("kind", kind), start, rec.End)
	}
	r.Breakdown("cache_invalidate_writes", r.SumIncrease("ruralz_state_ops_total", promtext.L("kind", "cache_invalidate"), start, fault.End))
	r.Add(
		Check("cache_set drops while the queue saturates", Functional, dropped("cache_set"), GT, 0),
		Check("cache_invalidate drops", Functional, dropped("cache_invalidate"), EQ, 0),
		Check("refunds written while the queue saturates", Functional, r.SumIncrease("ruralz_state_ops_total", promtext.L("kind", "refund"), start, fault.End), GT, 0),
		Check("refund drops", Functional, dropped("refund"), EQ, 0),
		Check("Nodes raising state_store_breaker_open", Functional, float64(len(r.Sampler.ReasonRaised(r.NodeNames(), BreakerReason, base.Start, rec.End))), EQ, 0),
		Check("failed requests", Functional, float64(LoadStats(l, base.Start, rec.End).Failed), EQ, 0),
	)
	r.StaleReasons(r.NodeNames(), base.End, rec.End)
	r.Finish()
}
