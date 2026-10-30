// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// V3Handovers is V-3's count of handovers, and of refused handovers
// (target).
const V3Handovers = 1000

// v3RoundBudget bounds the real time one round of handovers takes: the
// successors' start and the walks through the gate, linger and Drain, a
// WalkStepWait per WalkStep of Node time. The load, which the run stops
// after its recovery, lasts at least the rounds' budgets.
const v3RoundBudget = 15 * time.Second

// allExited reports whether every process of ps has exited.
func allExited(ps []*proc.Process) bool {
	for _, p := range ps {
		if p != nil && p.State() == nil {
			return false
		}
	}
	return true
}

// TestV3Handovers is V-3: V3Handovers handovers spread over the Nodes at
// 50% load, behind the balancer's keep-alive health checker on 9901,
// each run in its own network namespace with net.ipv4.tcp_migrate_req at
// 1 and at 0, the first run also starting a second successor beside
// every accepted one, which the holder refuses as busy. No request
// fails, every probe answers 200, no process dies by a signal, the State
// Store refuses no connection, and resets per handover, the balancer's
// failed dials, are none at 1 and recorded at 0
// (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "Verification runbook"). It needs root for the namespaces and skips
// with that reason otherwise; the derived-ceiling count joins in M2.
func TestV3Handovers(t *testing.T) {
	for _, migrate := range []string{"1", "0"} {
		t.Run("tcp_migrate_req="+migrate, func(t *testing.T) {
			if proc.NetNSChild(t) {
				v3(t, migrate == "1")
				return
			}
			if os.Geteuid() != 0 {
				t.Skip("chaos: V-3 sets net.ipv4.tcp_migrate_req in a network namespace of its own, which needs root")
			}
			saturation(t)
			_, err := proc.RunInNetNS(t.Context(), proc.NetNSOptions{
				TestName: t.Name(),
				Sysctls:  map[string]string{"net.ipv4.tcp_migrate_req": migrate},
				Env:      childEnv(),
				Output:   testWriter{t},
			})
			if errors.Is(err, proc.ErrChildSkipped) {
				t.Skip(err)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// testWriter logs a child's output.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Logf("%s", b)
	return len(b), nil
}

// v3 runs one V-3 run inside its namespace; busy also starts a second
// successor beside each accepted one.
func v3(t *testing.T, busy bool) {
	s := saturation(t)
	name := V3 + "/tcp_migrate_req=0"
	if busy {
		name = V3 + "/tcp_migrate_req=1"
	}
	r := suite.Start(t, Spec{Name: name})
	ctx := r.Context()
	rounds := (V3Handovers + r.Nodes - 1) / r.Nodes
	// A round walks the clock through the gate, linger and Drain and
	// starts the successors; the load, stopped after the recovery, runs
	// past the slowest rounds' end.
	budget := time.Duration(rounds) * v3RoundBudget
	l := r.StartLoad(LoadSpec{Duration: Baseline + budget + RecoveryTime, Streams: []StreamSpec{
		{Name: "plain", Path: PathPlain, Rate: suite.Profile.LoadShare * s},
	}})
	r.Begin(l)
	base := r.Hold(l, PhaseBaseline, Baseline)
	store := r.Primaries()[0]
	rejectedBefore, _ := rejectedConns(ctx, store)
	probesBefore := probeFailures(r)
	dialsBefore := dialFailures(r)
	start := time.Now()
	var resets []int64
	handovers, refused, signaled := 0, 0, 0
	for round := 0; round < rounds && handovers < V3Handovers; round++ {
		nodes := r.Top.Nodes()
		olds := make([]*proc.Process, len(nodes))
		news := make([]*proc.Process, len(nodes))
		extra := make([]*proc.Process, len(nodes))
		for i, n := range nodes {
			olds[i] = n.Process()
			p, err := n.Handover(ctx, topology.HandoverOptions{})
			if err != nil {
				t.Fatal(err)
			}
			news[i] = p
			if busy {
				if extra[i], err = n.Handover(ctx, topology.HandoverOptions{}); err != nil {
					t.Fatal(err)
				}
			}
		}
		// The holders accept, linger and Drain on the Nodes' clock: walk
		// it until every successor holds its Node's lock, then until every
		// old process has drained and exited.
		var wg sync.WaitGroup
		var pending atomic.Int64
		pending.Store(int64(len(nodes)))
		won := make([]*proc.Process, len(nodes))
		for i, n := range nodes {
			wg.Go(func() {
				defer pending.Add(-1)
				won[i] = holderOf(ctx, n, news[i], extra[i])
			})
		}
		r.WalkUntil(HandoverGate+HandoverLinger, func() bool { return pending.Load() == 0 })
		wg.Wait()
		r.WalkUntil(HandoverLinger+HandoverExitBound, func() bool { return allExited(olds) })
		for i := range nodes {
			if won[i] == nil {
				t.Fatalf("chaos: %s: no successor took the lock in round %d", nodes[i].Name(), round)
			}
			handovers++
			if code, err := waitExit(ctx, olds[i], 2*HandoverExitBound); err != nil || code != 0 {
				signaled++
				t.Logf("chaos: %s: the old process exited %d (%v)", nodes[i].Name(), code, err)
			}
			if extra[i] != nil {
				loser := extra[i]
				if won[i] == extra[i] {
					loser = news[i]
				}
				if code, err := waitExit(ctx, loser, 2*HandoverGate); err == nil && code != 0 {
					refused++
				}
			}
		}
		d := dialFailures(r)
		resets = append(resets, d-dialsBefore-sum(resets))
	}
	r.Mark("handovers", start)
	rec := r.Hold(l, PhaseRecovery, RecoveryTime)
	l.Stop()
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	r.IntervalLog("plain", l.Streams[0])
	r.Baseline(l, base)
	rejectedAfter, _ := rejectedConns(ctx, store)
	worst := int64(0)
	for _, x := range resets {
		worst = max(worst, x)
	}
	r.Breakdown("handovers", float64(handovers))
	r.Breakdown("resets_total", float64(sum(resets)))
	r.Breakdown("resets_per_round_max", float64(worst))
	r.Add(
		Check("handovers", Functional, float64(handovers), GE, V3Handovers),
		Check("failed requests", Functional, float64(LoadStats(l, base.End, rec.End).Failed), EQ, 0),
		Check("failed probes", Functional, float64(probeFailures(r)-probesBefore), EQ, 0),
		Check("old processes not exiting 0", Functional, float64(signaled), EQ, 0),
		Check("State Store connections refused", Functional, float64(rejectedAfter-rejectedBefore), EQ, 0),
	)
	if busy {
		r.Add(
			Check("resets per handover at tcp_migrate_req=1", Functional, float64(sum(resets)), EQ, 0),
			Check("busy successors refused", Functional, float64(refused), GE, V3Handovers),
		)
	}
	r.Finish()
}

// holderOf waits until one of the successors holds the Node's lock and
// returns it; nil when neither does within the gate.
func holderOf(ctx context.Context, n *topology.Node, a, b *proc.Process) *proc.Process {
	if b == nil {
		if n.WaitHolder(ctx, a) == nil {
			return a
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, HandoverGate)
	defer cancel()
	for ctx.Err() == nil {
		h, err := n.Holder()
		if err == nil {
			for _, p := range []*proc.Process{a, b} {
				if h.PID == p.PID() {
					if n.WaitHolder(ctx, p) == nil {
						return p
					}
				}
			}
		}
		time.Sleep(topology.PollInterval)
	}
	return nil
}

// probeFailures sums the balancers' failed readiness probes.
func probeFailures(r *Run) int64 {
	var n int64
	for _, b := range r.HTTP.Backends() {
		n += b.ProbeFailures
	}
	return n
}

// dialFailures sums the balancers' failed backend dials: the resets and
// refusals a handover caused, which the balancer redispatched.
func dialFailures(r *Run) int64 {
	var n int64
	for _, b := range r.HTTP.Backends() {
		n += b.DialFailures
	}
	return n
}

// rejectedConns returns a State Store server's rejected_connections.
func rejectedConns(ctx context.Context, s *redisserver.Server) (int64, error) {
	info, err := s.Info(ctx, "stats")
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(info["rejected_connections"], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("chaos: INFO stats rejected_connections = %q", info["rejected_connections"])
	}
	return n, nil
}

func sum(xs []int64) int64 {
	var t int64
	for _, x := range xs {
		t += x
	}
	return t
}
