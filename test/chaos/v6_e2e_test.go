// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// Handover timeline bounds (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "In-place handover"): the gate a new process waits to be accepted, the
// holder's linger after steering, and the handover Drain's exit bound.
const (
	HandoverGate      = 60 * time.Second
	HandoverLinger    = 3 * time.Second
	HandoverExitBound = 25 * time.Second
)

// TestV6RollbackPastTheLimit is V-6: Nodes on N serve a Revision using a
// field the N-1 variant lacks, past the binary rollback limit. A
// handover to N-1 is refused, the N-1 process reporting RZ-CFG-024 and
// exiting without the lock, and N keeps serving; then the rollback-limit
// row's three steps run in file mode: the Revision without the field
// replaces RURALZ_CONFIG, every Node's digest matches it, and a reverse
// handover moves every Node to N-1 with none unready and no request
// failing (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "Verification runbook" and "Rollback").
func TestV6RollbackPastTheLimit(t *testing.T) {
	s := saturation(t)
	n1, err := suite.NMinusOne(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r := suite.Start(t, Spec{Name: V6, Bundle: func(b *BundleParams) { b.Overlay = OverlayRouteTimeout }})
	ctx := r.Context()
	rate := suite.Profile.LoadShare * s
	row := 4 * time.Minute
	l := r.StartLoad(LoadSpec{Duration: Baseline + row + RecoveryTime, Streams: []StreamSpec{
		{Name: "plain", Path: PathPlain, Rate: rate / 2},
		{Name: "limited", Path: PathLimited, Rate: rate / 2, APIKeys: true, Counted: Decisions()},
	}})
	r.Begin(l)
	base := r.Hold(l, PhaseBaseline, Baseline)
	before := revisions(ctx, t, r)
	nodes := r.Top.Nodes()

	// The refused handover: every N-1 successor exits without the lock
	// once its gate expires, and N keeps the lock and its Revision.
	holders := map[string]int{}
	for _, n := range nodes {
		holders[n.Name()] = n.Process().PID()
	}
	succs := make([]*proc.Process, len(nodes))
	r.Inject("handover-to-n-1", "every-node", func() error {
		for i, n := range nodes {
			p, err := n.Handover(ctx, topology.HandoverOptions{Binary: n1.Ruralzd})
			if err != nil {
				return err
			}
			succs[i] = p
		}
		return nil
	})
	// Each N-1 successor gives up within its gate, on the Nodes' clock.
	r.WalkUntil(HandoverGate+time.Second, func() bool { return allExited(succs) })
	refused, reported := 0, 0
	for _, p := range succs {
		code, err := waitExit(ctx, p, 2*HandoverGate)
		if err == nil && code != 0 {
			refused++
		}
		out, errOut := p.Output()
		if strings.Contains(string(out)+string(errOut), "RZ-CFG-024") {
			reported++
		}
	}
	kept := 0
	for _, n := range nodes {
		if h, err := n.Holder(); err == nil && h.PID == holders[n.Name()] {
			kept++
		}
	}
	r.Add(
		Check("N-1 successors exiting non-zero", Functional, float64(refused), EQ, float64(len(nodes))),
		Check("N-1 successors reporting RZ-CFG-024", Functional, float64(reported), EQ, float64(len(nodes))),
		Check("Nodes whose N process kept the lock", Functional, float64(kept), EQ, float64(len(nodes))),
		Check("Nodes whose Revision changed", Functional, float64(changed(before, revisions(ctx, t, r))), EQ, 0),
	)

	// Step 1: the Revision without the field replaces RURALZ_CONFIG.
	params := r.Params
	params.Overlay = ""
	plain, err := RenderBundle(ctx, suite.Pipeline(), params)
	if err != nil {
		t.Fatal(err)
	}
	rowAt := r.Inject("row-step-1-revision-without-the-field", "every-node", func() error {
		for _, n := range nodes {
			if _, err := n.Reload(ctx, plain); err != nil {
				return err
			}
		}
		return loadAll(ctx, r, nil)
	})
	// Step 2: every Node's digest matches it.
	matched := 0
	for _, n := range nodes {
		if err := n.WaitRevision(ctx, n.TestDigest()); err == nil {
			matched++
		}
	}
	r.Add(Check("row step 2: Nodes whose digest matches the Revision without the field", Functional, float64(matched), EQ, float64(len(nodes))))
	// Step 3: the reverse handover to N-1.
	olds := make([]*proc.Process, len(nodes))
	news := make([]*proc.Process, len(nodes))
	r.Inject("row-step-3-reverse-handover", "every-node", func() error {
		for i, n := range nodes {
			olds[i] = n.Process()
			p, err := n.Handover(ctx, topology.HandoverOptions{Binary: n1.Ruralzd})
			if err != nil {
				return err
			}
			news[i] = p
		}
		return nil
	})
	// Walk the clock until every N-1 successor holds its Node's lock, after
	// the holder's linger, then until every N process has drained and
	// exited.
	var wg sync.WaitGroup
	var pending atomic.Int64
	pending.Store(int64(len(nodes)))
	held := make([]bool, len(nodes))
	for i, n := range nodes {
		wg.Go(func() {
			defer pending.Add(-1)
			held[i] = n.WaitHolder(ctx, news[i]) == nil
		})
	}
	r.WalkUntil(HandoverGate+HandoverLinger, func() bool { return pending.Load() == 0 })
	wg.Wait()
	r.WalkUntil(HandoverLinger+HandoverExitBound, func() bool { return allExited(olds) })
	onN1, cleanExit := 0, 0
	for i := range nodes {
		if held[i] {
			onN1++
		}
		if code, err := waitExit(ctx, olds[i], 2*HandoverExitBound); err == nil && code == 0 {
			cleanExit++
		}
	}
	r.Add(
		Check("row step 3: Nodes on N-1 after the reverse handover", Functional, float64(onN1), EQ, float64(len(nodes))),
		Check("row step 3: N processes exiting 0 after their handover Drain", Functional, float64(cleanExit), EQ, float64(len(nodes))),
	)
	r.Hold(l, "row", row)
	rec := r.Hold(l, PhaseRecovery, RecoveryTime)
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, st := range l.Streams {
		r.IntervalLog(st.Spec.Name, st)
	}
	r.Baseline(l, base)
	r.Add(
		Check("failed requests", Functional, float64(LoadStats(l, base.End, rec.End).Failed), EQ, 0),
		Check("Nodes taken out of rotation as unready", Functional, float64(r.UnreadyTransitions(base.Start, rec.End)), EQ, 0),
	)
	r.Breakdown("row_seconds", time.Since(rowAt).Seconds())
	r.Finish()
}

// waitExit waits up to d of real time for p to exit and returns its exit
// code.
func waitExit(ctx context.Context, p *proc.Process, d time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return p.Wait(ctx)
}
