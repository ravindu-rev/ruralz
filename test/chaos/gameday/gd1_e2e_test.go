// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package gameday

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
	"github.com/ravindu-rev/ruralz/test/chaos"
)

// GD-1's restarts of the killed Node, in order: with its data directory,
// with a wiped one, and onto an unreadable Bundle, which boots
// Last-Known-Good.
const (
	gd1Kept       = "data-dir"
	gd1Wiped      = "wiped-data-dir"
	gd1Unreadable = "unreadable-bundle"
)

// gd1Garbage is the unreadable Bundle: bytes no loader reads as YAML or
// JSON.
const gd1Garbage = "\xff\xfe\x00 not a Bundle\n"

// gd1Restart is one restart cycle as it happened.
type gd1Restart struct {
	mode                 string
	kill, start, ready   time.Time
	probeFailures        int64
	lkgServed, lkgRaised bool
	lkgCleared           bool
}

// TestGD1NodeLoss is GD-1, Node loss, and the failure catalog row FC-1 on
// its run (docs/operations/04-high-availability-and-disaster-recovery.md
// "Game days" and "Failure catalog"): CE-1's fault, one Node SIGKILLed at
// 50% load, then restarted at once, three times: with its data
// directory, with a wiped one, and onto an unreadable Bundle, so it boots
// Last-Known-Good. Errors come only from the killed Node's in-flight
// requests, no Quota is under-charged, each restart is ready within 30 s
// of its start, and, for FC-1, the survivors serve, each restart admits at
// most one extra ceiling per key, and the balancer's /readyz probes detect
// each loss. The /limited keys are offered twice their limit, so the
// limit binds while the Node restarts.
func TestGD1NodeLoss(t *testing.T) {
	s := saturation(t)
	rate := suite.Profile.LoadShare * s
	nodes := suite.Profile.Nodes
	keys := float64(len(chaos.Consumers()))
	ceiling := max(1, int(rate/2/keys/float64(2*nodes)))
	requests := ceiling * nodes
	r := suite.Start(t, chaos.Spec{Name: GD1, Bundle: func(b *chaos.BundleParams) {
		b.Limited = chaos.Limit{Requests: requests, Ceiling: ceiling}
	}})
	ctx := r.Context()
	// A cycle's worst case: the restart, the lkg_boot wait and its clearing
	// wait, each bounded by RestartReady, then the serving span. The run
	// stops the load after its recovery.
	cycle := 3*RestartReady + LocalOnlyEntry
	l := r.StartLoad(chaos.LoadSpec{Duration: chaos.Baseline + 3*cycle + chaos.RecoveryTime, Streams: []chaos.StreamSpec{
		{Name: "plain", Path: chaos.PathPlain, Rate: rate / 2},
		{Name: "limited", Path: chaos.PathLimited, Rate: rate / 2, APIKeys: true, Counted: chaos.Decisions()},
	}})
	r.Begin(l)
	base := r.Hold(l, chaos.PhaseBaseline, chaos.Baseline)
	victim := r.Top.Nodes()[len(r.Top.Nodes())/2]
	var restarts []gd1Restart
	for _, mode := range []string{gd1Kept, gd1Wiped, gd1Unreadable} {
		restarts = append(restarts, gd1Cycle(ctx, t, r, l, victim, mode))
	}
	rec := r.Hold(l, chaos.PhaseRecovery, chaos.RecoveryTime)
	l.Stop()
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	logStreams(r, l)
	r.Baseline(l, base)
	node := nodeOf(r)
	first := restarts[0].kill
	r.Attribute(chaos.Attribution(l, first, rec.End))

	// CE-1's error rule, per kill: each kill's window runs to the next.
	var elsewhere, late int64
	for i, rs := range restarts {
		to := rec.End
		if i+1 < len(restarts) {
			to = restarts[i+1].kill
		}
		for _, st := range l.Streams {
			e, lt := InFlight(st.Requests(), st.Failed, node, victim.Name(), rs.kill, to)
			elsewhere += e
			late += lt
		}
	}
	under := gd1UnderCharge(ctx, t, r, l.StreamNamed("limited"))
	// Errors only on the killed Node's in-flight requests: FC-1's
	// survivors serve.
	r.Add(AlsoFor(FC1, chaos.Check("failed requests on the surviving Nodes", chaos.Functional, float64(elsewhere), chaos.EQ, 0))...)
	r.Add(
		chaos.Check("failed requests sent after their connection failed", chaos.Functional, float64(late), chaos.EQ, 0),
		chaos.Check("quota under-charge (admitted requests above the counter)", chaos.Functional, float64(under), chaos.EQ, 0),
	)
	limited := l.StreamNamed("limited").Requests()
	for _, rs := range restarts {
		readyIn := r.NodeTime(rs.ready).Sub(r.NodeTime(rs.start))
		worst, key := ExtraAdmissions(limited, r.NodeTime, r.NodeTime(rs.kill), r.NodeTime(rs.ready).Add(LocalOnlyEntry), int64(requests))
		t.Logf("gameday: %s: restart %s ready after %v; key %s admitted %d above the healthy bound (ceiling %d)", GD1, rs.mode, readyIn, key, worst, ceiling)
		r.Breakdown("restart_ready_s_"+rs.mode, readyIn.Seconds())
		r.Add(chaos.Check("restart "+rs.mode+": ready after its start, seconds", chaos.Functional, readyIn.Seconds(), chaos.LE, RestartReady.Seconds()))
		r.Add(AlsoFor(FC1, chaos.Check("restart "+rs.mode+": admissions per key above the healthy bound", chaos.Functional, float64(max(worst, 0)), chaos.LE, float64(ceiling)))...)
		r.Add(chaos.Check(FC1+": restart "+rs.mode+": readiness probes the killed Node failed", chaos.Functional, float64(rs.probeFailures), chaos.GE, 1))
		if rs.mode == gd1Unreadable {
			r.Add(
				chaos.Holds("restart "+rs.mode+": serves its Last-Known-Good Revision", chaos.Functional, rs.lkgServed),
				chaos.Holds("restart "+rs.mode+": raises lkg_boot", chaos.Functional, rs.lkgRaised),
				chaos.Holds("restart "+rs.mode+": clears lkg_boot once its Bundle is restored", chaos.Functional, rs.lkgCleared),
			)
		}
	}
	r.Add(chaos.Check("recovery errors", chaos.Functional, float64(chaos.LoadStats(l, rec.Start, rec.End).Failed), chaos.EQ, 0))
	r.StaleReasons(r.NodeNames(), base.End, rec.End)
	r.Finish()
}

// gd1Cycle kills victim, prepares its restart by mode and restarts it at
// once, waiting until it is ready; then it lets the Node serve for the
// local-only entry, the span FC-1 counts its extra admissions over.
func gd1Cycle(ctx context.Context, t *testing.T, r *chaos.Run, l *chaos.Load, victim *topology.Node, mode string) gd1Restart {
	t.Helper()
	rs := gd1Restart{mode: mode}
	before, _, err := victim.Readyz(ctx)
	if err != nil {
		t.Fatal(err)
	}
	failures := gd1ProbeFailures(r, victim.Name())
	rs.kill = r.Inject("sigkill", victim.Name(), func() error { return victim.Kill(ctx) })
	switch mode {
	case gd1Wiped:
		if err := gd1Wipe(victim.DataDir()); err != nil {
			t.Fatal(err)
		}
	case gd1Unreadable:
		if err := writeConfig(victim, []byte(gd1Garbage)); err != nil {
			t.Fatal(err)
		}
	}
	// FC-1's Detection: /readyz on the admin port fails while the Node is
	// down. A restart can finish within one of the balancer's probe
	// intervals, so the harness also probes it once itself.
	var down int64
	if _, _, err := victim.Readyz(ctx); err != nil {
		down = 1
	}
	rs.start = r.Inject("restart-"+mode, victim.Name(), func() error { return victim.Start(ctx) })
	rs.ready = time.Now()
	rs.probeFailures = gd1ProbeFailures(r, victim.Name()) - failures + down
	if mode == gd1Unreadable {
		rz, _, err := victim.Readyz(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rs.lkgServed = rz.Revision != "" && rz.Revision == before.Revision
		lkg := catalog.ReasonLKGBoot.String()
		rs.lkgRaised = waitUntil(ctx, RestartReady, func() bool {
			r.Sampler.Round(ctx)
			return len(r.Sampler.ReasonRaised([]string{victim.Name()}, lkg, rs.start, time.Now().Add(time.Second))) == 1
		})
		// The source renders the Revision the Node serves again: its load
		// clears lkg_boot (docs/architecture/03-data-plane.md "Last-Known-Good
		// files").
		r.Inject("restore-bundle", victim.Name(), func() error {
			if err := writeConfig(victim, victim.Render().Bytes); err != nil {
				return err
			}
			return victim.Signal(ctx, syscall.SIGHUP)
		})
		restored := time.Now()
		rs.lkgCleared = waitUntil(ctx, RestartReady, func() bool {
			r.Sampler.Round(ctx)
			tr := r.Sampler.ReasonTrace(victim.Name(), lkg)
			return len(tr) > 0 && tr[len(tr)-1].Real.After(restored) && tr[len(tr)-1].Value == 0
		})
	}
	r.Mark("restart-"+mode, rs.kill)         //nolint:contextcheck // The run samples metrics on its own context, which outlives the cycle.
	r.Hold(l, "serve-"+mode, LocalOnlyEntry) //nolint:contextcheck // The run samples metrics on its own context, which outlives the cycle.
	return rs
}

// gd1ProbeFailures returns the http balancer's failed readiness probes of
// a Node.
func gd1ProbeFailures(r *chaos.Run, node string) int64 {
	b, ok := r.HTTP.Backend(node)
	if !ok {
		return 0
	}
	return b.ProbeFailures
}

// gd1Wipe removes everything under a Node's data directory, which the
// restarted Node recreates.
func gd1Wipe(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("gameday: wipe %s: %w", dir, err)
		}
	}
	return nil
}

// gd1UnderCharge is CE-1's quota rule over the whole run: each Consumer's
// counter in the State Store holds at least the requests admitted for it.
func gd1UnderCharge(ctx context.Context, t *testing.T, r *chaos.Run, st *chaos.Stream) int64 {
	t.Helper()
	admitted := map[string]int64{}
	for _, q := range st.Requests() {
		if chaos.Admitted(q) {
			admitted[q.Key]++
		}
	}
	counters, err := chaos.QuotaCounters(ctx, r.Primaries())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chaos.Consumers() {
		t.Logf("gameday: %s: %s: %d admitted, quota counter %d", GD1, c, admitted[c], counters[c])
	}
	return UnderCharge(admitted, counters)
}
