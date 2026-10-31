// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package gameday

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/test/chaos"
)

// FC-20's run, the code that rejects a Revision of an apiVersion the
// Nodes do not serve (docs/architecture/02-configuration-model.md), its
// phases, how long each state holds under load once every Node reached
// it, and how long the run waits for every Node to act on a published
// source, as the Verification runbook's gate steps wait.
const (
	fc20Name           = "FC-20"
	fc20Invalid        = "RZ-CFG-007"
	fc20PhaseInvalid   = "invalid-revision"
	fc20PhaseFailing   = "failing-revision"
	fc20Hold           = 15 * time.Second
	fc20Wait           = 30 * time.Second
	fc20Route          = "plain"
	fc20ServedVersion  = "apiVersion: ruralz/v1alpha1"
	fc20UnknownVersion = "apiVersion: ruralz/v1alpha9"
	// fc20Upstream is the chaos Bundle's Upstream, whose breaker the
	// failing Revision opens.
	fc20Upstream = "chaos-backend"
)

// fc20BreakerClose bounds, in Node time, how long after the revert every
// Node's breaker closes: it survives the Hot Reload open
// (docs/architecture/09-traffic-management-and-resilience.md), half-opens
// after openDuration, jittered +20% at most, and its half-open probes,
// which succeed at once against the Endpoints the revert restored, close
// it within a second, the sampling included.
func fc20BreakerClose() time.Duration {
	return resilience.DefaultOpenDuration*(100+resilience.OpenJitterPercent)/100 + time.Second
}

// TestFC20BadRevision is the file-mode half of FC-20, "Bad Revision", at
// 50% load (docs/operations/04-high-availability-and-disaster-recovery.md
// "Failure catalog"; docs/engineering/03-testing-and-quality-strategy.md
// "Drills and failure rows"). Every Node's watched source first takes an
// invalid Revision: each Node rejects it with its code, counted in
// ruralz_config_activations_total{result="rejected"}, while the previous
// Revision keeps serving without a failed request. Then a valid Revision
// whose Endpoints refuse every connection: every Node activates it,
// requests on its Routes fail and the 5xx counters rise on every Node.
// Republishing the previous source reverts it: every Node recovers in one
// Hot Reload. The Upstream's breaker, which the failing Revision opened,
// survives the Hot Reload, so until each Node's breaker closes, within
// openDuration after the revert, requests fail with RZ-UP-005 only;
// after that no request fails and the 5xx counters stay still. The
// watchers' 1 s polls on the Nodes' clock load each source; no SIGHUP is
// sent.
func TestFC20BadRevision(t *testing.T) {
	s := saturation(t)
	r := suite.Start(t, chaos.Spec{Name: fc20Name, SampleEvery: 250 * time.Millisecond})
	ctx := r.Context()
	rate := suite.Profile.LoadShare * s
	l := r.StartLoad(chaos.LoadSpec{Duration: chaos.Baseline + 3*time.Minute + chaos.RecoveryTime, Streams: []chaos.StreamSpec{
		{Name: "plain", Path: chaos.PathPlain, Rate: rate / 2},
		{Name: "limited", Path: chaos.PathLimited, Rate: rate / 2, APIKeys: true, Counted: chaos.Decisions()},
	}})
	r.Begin(l)
	base := r.Hold(l, chaos.PhaseBaseline, chaos.Baseline)
	nodes := r.NodeNames()
	before := revisions(ctx, t, r)

	// An invalid Revision in every Node's watched source.
	invalidAt := r.Inject("invalid-revision", "every-node", func() error {
		for _, n := range r.Top.Nodes() {
			b, err := fc20Replace(n.Render().Bytes, fc20ServedVersion, fc20UnknownVersion)
			if err != nil {
				return err
			}
			if err := writeConfig(n, b); err != nil {
				return err
			}
		}
		return nil
	})
	rejected := func(n string) bool {
		return FC20Rose(r.Sampler.Trace(n, catalog.ConfigActivationsTotal, promtext.L("result", "rejected", "code", fc20Invalid)), invalidAt)
	}
	waitUntil(ctx, fc20Wait, func() bool {
		r.Sampler.Round(ctx)
		for _, n := range nodes {
			if !rejected(n) {
				return false
			}
		}
		return true
	})
	if err := l.WaitUntil(ctx, time.Now().Add(fc20Hold)); err != nil {
		t.Fatal(err)
	}
	invalid := r.Mark(fc20PhaseInvalid, invalidAt)
	rejectedBy := 0
	for _, n := range nodes {
		if rejected(n) {
			rejectedBy++
		}
	}
	r.Add(
		chaos.Check("FC-20: Nodes rejecting the invalid Revision with RZ-CFG-007", chaos.Functional, float64(rejectedBy), chaos.EQ, float64(len(nodes))),
		chaos.Check("FC-20: Nodes whose active Revision changed on the invalid one", chaos.Functional, float64(fc20Changed(before, revisions(ctx, t, r))), chaos.EQ, 0),
		chaos.Check("FC-20: failed requests while the invalid Revision was rejected", chaos.Functional, float64(chaos.LoadStats(l, invalidAt, invalid.End).Failed), chaos.EQ, 0),
	)

	// A valid Revision whose Endpoints refuse every connection.
	params := r.Params
	params.Backends = fc20Refusing(ctx, t)
	failing, err := chaos.RenderBundle(ctx, suite.Pipeline(), params)
	if err != nil {
		t.Fatal(err)
	}
	failingAt := r.Inject("failing-revision", "every-node", func() error {
		for _, n := range r.Top.Nodes() {
			if _, err := n.Reload(ctx, failing); err != nil {
				return err
			}
		}
		return nil
	})
	activated := fc20WaitRevisions(ctx, t, r)
	activeAt := time.Now()
	if err := l.WaitUntil(ctx, activeAt.Add(fc20Hold)); err != nil {
		t.Fatal(err)
	}
	bad := r.Mark(fc20PhaseFailing, failingAt)
	fiveXX := promtext.L("route", fc20Route, "status_class", "5xx")
	rose := 0
	for _, n := range nodes {
		if FC20Rose(r.Sampler.Trace(n, catalog.HTTPRequestsTotal, fiveXX), failingAt) {
			rose++
		}
	}
	r.Add(
		chaos.Check("FC-20: Nodes activating the failing Revision", chaos.Functional, float64(activated), chaos.EQ, float64(len(nodes))),
		chaos.Check("FC-20: /plain requests answered 5xx under the failing Revision", chaos.Functional, float64(fc20ServerErrors(l.StreamNamed("plain"), activeAt, bad.End)), chaos.GT, 0),
		chaos.Check("FC-20: Nodes whose /plain 5xx counter rose under the failing Revision", chaos.Functional, float64(rose), chaos.EQ, float64(len(nodes))),
	)
	r.Breakdown("fc20_failed_requests_failing_revision", float64(chaos.LoadStats(l, failingAt, bad.End).Failed))

	// Republishing the previous source reverts it in one Hot Reload.
	revertAt := r.Inject("republish-previous", "every-node", func() error {
		for _, n := range r.Top.Nodes() {
			if _, err := n.Reload(ctx, r.Bundle); err != nil {
				return err
			}
		}
		return nil
	})
	reverted := fc20WaitRevisions(ctx, t, r)
	recoveredAt := time.Now()
	rec := r.Hold(l, chaos.PhaseRecovery, chaos.RecoveryTime)
	l.Stop()
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	logStreams(r, l)
	r.Baseline(l, base)
	r.Breakdown("fc20_recovered_after_revert_s", recoveredAt.Sub(revertAt).Seconds())
	reloads := map[string]float64{}
	for _, n := range nodes {
		reloads[n] = nodeIncrease(r, n, catalog.ConfigActivationsTotal, promtext.L("result", "activated"), revertAt, rec.End)
	}
	off := FC20OffByOne(reloads, 1)
	if len(off) > 0 {
		t.Logf("gameday: activations per Node after the revert: %v", reloads)
	}
	// The breaker survives the revert: until every Node's closed, requests
	// fail only with RZ-UP-005; once all closed, none fails.
	closedAt, closed := fc20BreakersClosed(r, nodes, revertAt)
	closing := float64(math.MaxInt64)
	if closed {
		closing = r.NodeTime(closedAt).Sub(r.NodeTime(revertAt)).Seconds()
	} else {
		closedAt = rec.End
	}
	var breakerOpen int64
	for _, st := range l.Streams {
		breakerOpen += chaos.CodesIn(st, recoveredAt, closedAt, resilience.CodeBreakerOpen)
	}
	r.Breakdown("fc20_breakers_closed_after_revert_s", closing)
	r.Add(
		chaos.Check("FC-20: Nodes back on the previous Revision", chaos.Functional, float64(reverted), chaos.EQ, float64(len(nodes))),
		chaos.Check("FC-20: Nodes whose active Revision differs from the previous one after the revert", chaos.Functional, float64(fc20Changed(before, revisions(ctx, t, r))), chaos.EQ, 0),
		chaos.Check("FC-20: Nodes not recovering in exactly one Hot Reload", chaos.Functional, float64(len(off)), chaos.EQ, 0),
		chaos.Check("FC-20: seconds from the revert until every Node's breaker closed", chaos.Functional, closing, chaos.LE, fc20BreakerClose().Seconds()),
		chaos.Check("FC-20: failed requests other than RZ-UP-005 until then", chaos.Functional, float64(chaos.LoadStats(l, recoveredAt, closedAt).Failed-breakerOpen), chaos.EQ, 0),
		chaos.Check("FC-20: failed requests once every breaker closed", chaos.Functional, float64(chaos.LoadStats(l, closedAt, rec.End).Failed), chaos.EQ, 0),
		chaos.Check("FC-20: /plain 5xx counted once every breaker closed", chaos.Functional, r.SumIncrease(catalog.HTTPRequestsTotal, fiveXX, closedAt, rec.End), chaos.EQ, 0),
	)
	r.StaleReasons(nodes, base.End, rec.End)
	r.Finish()
}

// fc20WaitRevisions waits until every Node serves its current test
// render's Revision and returns how many do.
func fc20WaitRevisions(ctx context.Context, t *testing.T, r *chaos.Run) int {
	t.Helper()
	n := 0
	for _, node := range r.Top.Nodes() {
		if err := node.WaitRevision(ctx, node.TestDigest()); err != nil {
			t.Logf("gameday: %v", err)
			continue
		}
		n++
	}
	return n
}

// fc20Refusing returns three loopback addresses no socket listens on:
// three ports bound together, so they differ, then closed, so a dial to
// each is refused at once.
func fc20Refusing(ctx context.Context, t *testing.T) [3]string {
	t.Helper()
	var lns [3]net.Listener
	var out [3]string
	for i := range lns {
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		lns[i], out[i] = ln, ln.Addr().String()
	}
	for _, ln := range lns {
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// fc20BreakersClosed returns the real instant by which every Node's
// breaker of fc20Upstream read closed again after since, from the first
// sample of each Node that does, and whether every Node's did.
func fc20BreakersClosed(r *chaos.Run, nodes []string, since time.Time) (time.Time, bool) {
	closed := promtext.L("upstream", fc20Upstream, "state", "closed")
	var last time.Time
	for _, n := range nodes {
		p, ok := chaos.FirstAt(r.Sampler.Trace(n, catalog.UpstreamBreakerStateInfo, closed), since, func(v float64) bool { return v > 0 })
		if !ok {
			return time.Time{}, false
		}
		last = latest(last, p.Real)
	}
	return last, true
}

// fc20Replace replaces the first from in b by to.
func fc20Replace(b []byte, from, to string) ([]byte, error) {
	i := bytes.Index(b, []byte(from))
	if i < 0 {
		return nil, fmt.Errorf("gameday: %q not in the test render", from)
	}
	return append(append(append([]byte(nil), b[:i]...), to...), b[i+len(from):]...), nil
}

// fc20Changed counts the Nodes whose Revision differs between two
// readings.
func fc20Changed(a, b map[string]string) int {
	n := 0
	for k, v := range a {
		if b[k] != v {
			n++
		}
	}
	return n
}

// fc20ServerErrors counts the responses of st due in [from, to) with a
// 5xx status.
func fc20ServerErrors(st *chaos.Stream, from, to time.Time) int64 {
	var n int64
	for _, q := range st.Requests() {
		if !q.Intended.Before(from) && q.Intended.Before(to) && q.Status >= 500 && q.Status < 600 {
			n++
		}
	}
	return n
}
