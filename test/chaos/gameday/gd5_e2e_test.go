// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package gameday

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
	"github.com/ravindu-rev/ruralz/test/chaos"
)

// GD-5's runs, by how the Nodes reach the second replacement: a file
// secret's URL rotated in place, or an env secret's URL changed by a
// Drain and restart, zone by zone.
const (
	gd5File = "file"
	gd5Env  = "env"
)

// GD-5's timeline: the deleted deployment stays gone 60 s (target), and
// each replacement serves twice the GCRA bound before the next step, so
// every Node's first answer from it falls inside.
const (
	gd5Gone  = 60 * time.Second
	gd5Serve = 2 * ServingGCRA
	// gd5Tail is the end of the recovery over which the dropped writes
	// return to the baseline's rate.
	gd5Tail = 30 * time.Second
	// gd5Await bounds the real time the env run waits, after a zone's
	// restart, for each of its Nodes' first closed-Policy GCRA answer
	// before it Drains the next zone. Node time runs no faster than real
	// time meanwhile, so a Node still waiting then misses the bound.
	gd5Await = 2 * ServingGCRA
	// gd5AwaitPoll spaces that wait's reads of the stream's requests.
	gd5AwaitPoll = 100 * time.Millisecond
)

// gd5Zones are the env run's zones, Drained and restarted in order.
func gd5Zones() []string { return []string{"a", "b", "c"} }

// The env run's closed-Policy stream that opens a connection per request,
// at gd5FreshRate requests per second: the load's other streams keep
// their connections on the Nodes they moved to at each zone's Drain, so
// only it reaches the zone restarted last.
const (
	gd5Fresh     = "closed-fresh"
	gd5FreshRate = 20
)

// gd5Store is every deployment GD-5 runs: noeviction, as the runbook
// provisions a replacement, and in cleartext behind the counting RESP
// proxy, so a URL can name another deployment without its CA.
func gd5Store() topology.StateStoreOptions {
	return topology.StateStoreOptions{RESPProxy: true, Config: []string{"maxmemory-policy noeviction"}}
}

// TestGD5StateStoreRebuild is GD-5, State Store rebuild
// (docs/operations/04-high-availability-and-disaster-recovery.md "Game
// days" and "State Store rebuild"), in two runs at 50% load: the State
// Store deployment is deleted with its data and stays gone 60 s, then the
// runbook brings a noeviction replacement up at the same URL; that
// replacement is then lost too and the Nodes move to a second one, in the
// file run by rotating each Node's file secret holding the URL in place,
// in the env run, whose URL comes from the env secret RURALZ_STATE_STORE_URL,
// by a Drain and restart of each zone's Nodes with the new value. The
// traffic RTO holds: failureMode throughout the outage, every Node
// serving GCRA within 10 s of a replacement accepting connections or of
// reaching it; each replacement's counters start empty; the dropped
// writes return to the baseline's rate; and no Node raises
// state_store_eviction_policy afterward.
func TestGD5StateStoreRebuild(t *testing.T) {
	t.Run(gd5File, func(t *testing.T) { gd5(t, gd5File) })
	t.Run(gd5Env, func(t *testing.T) { gd5(t, gd5Env) })
}

// gd5 runs one of GD-5's runs.
func gd5(t *testing.T, variant string) {
	s := saturation(t)
	plan := chaos.PlanLimits(suite.Profile.LoadShare*s, suite.Profile.Nodes)
	spec := chaos.Spec{Name: GD5 + "/" + variant, StateStore: gd5Store(), Bundle: plan.Apply, SampleEvery: 250 * time.Millisecond}
	if variant == gd5Env {
		spec.Zones = gd5Zones()
		spec.Bundle = func(b *chaos.BundleParams) {
			plan.Apply(b)
			b.Overlay = chaos.OverlayEnvStateStore
		}
		spec.Node = func(r *chaos.Run, o *topology.NodeOptions) {
			o.AuthoredSecrets = append(o.AuthoredSecrets, chaos.RefStateStoreEnv)
			o.Env = append(o.Env, chaos.EnvStateStoreURL+"="+r.Store.URL())
		}
	}
	r := suite.Start(t, spec)
	ctx := r.Context()
	// The env run's worst case: each zone's Drains and restarts, each
	// bounded by the harness's kill time and readiness wait, and its wait
	// for the zone's GCRA answers. The run stops the load after its
	// recovery.
	moves := time.Duration(len(gd5Zones())) * (topology.DefaultKillAfter + topology.DefaultReadyTimeout + gd5Await)
	streams := plan.Streams()
	if variant == gd5Env {
		streams = append(streams, chaos.StreamSpec{
			Name: gd5Fresh, Path: chaos.PathLimitedClosed, Rate: gd5FreshRate, APIKeys: true,
			Counted: append(chaos.Decisions(), chaos.STSCodes()...), DisableKeepAlives: true,
		})
	}
	l := r.StartLoad(chaos.LoadSpec{Duration: chaos.Baseline + gd5Gone + 2*gd5Serve + moves + chaos.RecoveryTime, Streams: streams})
	r.Begin(l)
	base := r.Hold(l, chaos.PhaseBaseline, chaos.Baseline)
	dep := r.Store.Deployment()
	primary := dep.Primary()
	r.Inject("delete", primary.Name(), func() error { return primary.Kill(ctx) })
	deleted := time.Now()
	r.Hold(l, "gone", gd5Gone)
	r.Inject("replace-at-the-same-url", primary.Name(), func() error { return primary.Restart(ctx) })
	accepted := time.Now()
	r.Hold(l, "replaced", gd5Serve)

	// The replacement is lost too: the Nodes move to a second one.
	second := r.Top.StartStateStore("second", gd5Store())
	firstCounters, err := chaos.QuotaCounters(ctx, dep.Nodes())
	if err != nil {
		t.Fatal(err)
	}
	read := time.Now()
	reached := map[string]time.Time{} // when each Node could reach the second replacement
	moveStart := time.Now()
	switch variant {
	case gd5File:
		r.Inject("delete-and-rotate-the-file-secret", "every-node", func() error {
			if err := primary.Kill(ctx); err != nil {
				return err
			}
			return gd5Rotate(r, second.URL())
		})
		at := time.Now() // every file rotated, the first replacement gone
		for _, n := range r.NodeNames() {
			reached[n] = at
		}
	case gd5Env:
		r.Inject("delete", primary.Name(), func() error { return primary.Kill(ctx) })
		for _, zone := range gd5Zones() {
			for n, at := range gd5Restart(ctx, t, r, l, zone, second.URL()) {
				reached[n] = at
			}
		}
	}
	end := time.Now()
	r.Mark("moved", moveStart)
	r.Hold(l, "second-replacement", gd5Serve)
	rec := r.Hold(l, chaos.PhaseRecovery, chaos.RecoveryTime)
	l.Stop()
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	logStreams(r, l)
	r.Baseline(l, base)
	secondCounters, err := chaos.QuotaCounters(ctx, second.Deployment().Nodes())
	if err != nil {
		t.Fatal(err)
	}
	node := nodeOf(r)
	nodes := r.NodeNames()
	closed := gd5Closed(l)

	// failureMode throughout the outage: every Node's breaker opens, no
	// closed Policy gets a GCRA answer, and the Routes whose Policies fail
	// open fail no request.
	var gcra int64
	for _, q := range closed {
		if q.Sent.After(deleted) && q.Done.Before(accepted) && GCRAAnswered(q) {
			gcra++
		}
	}
	var openFailed int64
	for _, st := range l.Streams {
		if st.Spec.Path == chaos.PathLimitedClosed {
			continue
		}
		openFailed += chaos.PhaseStats(st, deleted, accepted).Failed
		openFailed += chaos.CodesIn(st, deleted, accepted, chaos.STSCodes()...)
	}
	r.Add(
		chaos.Check("Nodes whose breaker opened in the outage", chaos.Functional, float64(len(r.Sampler.ReasonRaised(nodes, chaos.BreakerReason, deleted, accepted))), chaos.EQ, float64(len(nodes))),
		chaos.Check("closed-Policy GCRA answers in the outage", chaos.Functional, float64(gcra), chaos.EQ, 0),
		chaos.Check("failed or RZ-STS requests on Routes failing open in the outage", chaos.Functional, float64(openFailed), chaos.EQ, 0),
	)

	// Every Node serving GCRA within the bound: from the same-URL
	// replacement accepting connections, then from reaching the second.
	same := map[string]time.Time{}
	for _, n := range nodes {
		same[n] = accepted
	}
	sameServing, _ := gd5Serving(t, r, "the same-URL replacement accepting connections", closed, node, same)
	secondServing, served := gd5Serving(t, r, gd5Reached(variant), closed, node, reached)
	r.Add(sameServing, secondServing)

	// Counters start empty in each replacement.
	r.Add(
		chaos.Check("same-URL replacement's quota units above what was charged since it accepted connections", chaos.Functional,
			float64(StartsEmpty(firstCounters, gd5Charged(l, accepted, read))), chaos.EQ, 0),
		chaos.Check("second replacement's quota units above what was charged since the Nodes reached it", chaos.Functional,
			float64(StartsEmpty(secondCounters, gd5Charged(l, gd5Earliest(reached), rec.End))), chaos.EQ, 0),
	)

	// The dropped writes return to the baseline's rate, and no Node raises
	// state_store_eviction_policy once a replacement serves.
	drops := func(from, to time.Time) float64 {
		return r.SumIncrease(catalog.StateWritesDroppedTotal, nil, from, to) / to.Sub(from).Seconds()
	}
	tail := rec.End.Add(-gd5Tail)
	r.Breakdown("writes_dropped_per_s_baseline", drops(base.Start, base.End))
	r.Add(
		chaos.Check("writes dropped per second at the end of the recovery above the baseline's", chaos.Functional, drops(tail, rec.End)-drops(base.Start, base.End), chaos.LE, 0),
		chaos.Check("Nodes raising state_store_eviction_policy once a replacement served", chaos.Functional,
			float64(len(r.Sampler.ReasonRaised(nodes, catalog.ReasonStateStoreEvictionPolicy.String(), accepted, rec.End))), chaos.EQ, 0),
	)
	// Until every Node serves GCRA from the second replacement, closed
	// Policies fail closed by design: the first replacement is gone the
	// moment the Nodes start moving. The recovery's errors count from then.
	r.RecoveryCriteria(l, base, rec, latest(end, served))
	r.Finish()
	// The first replacement stays lost through the run; it comes back,
	// empty, only so the topology's teardown can read every State Store's
	// keyspace for its leak scan.
	if err := primary.Restart(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// gd5Closed returns the requests of the closed-Policy streams: closed,
// and the env run's gd5Fresh.
func gd5Closed(l *chaos.Load) []chaos.Request {
	var out []chaos.Request
	for _, name := range []string{"closed", gd5Fresh} {
		if st := l.StreamNamed(name); st != nil {
			out = append(out, st.Requests()...)
		}
	}
	return out
}

// latest returns the later of two instants.
func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// gd5Reached names the instant the bound counts from once the Nodes move
// to the second replacement.
func gd5Reached(variant string) string {
	if variant == gd5Env {
		return "its restart with the new URL"
	}
	return "the rotation of its file secret"
}

// gd5Serving is the criterion that every Node served GCRA within
// ServingGCRA of Node time after its instant in from: its first
// closed-Policy request sent since that a GCRA answer decided. It also
// returns the latest real instant a Node first served.
func gd5Serving(t *testing.T, r *chaos.Run, after string, closed []chaos.Request, node func(chaos.Request) string, from map[string]time.Time) (chaos.Criterion, time.Time) {
	t.Helper()
	worst := time.Duration(0)
	var served time.Time
	for n, at := range from {
		first, ok := FirstGCRA(gd5OfNode(closed, node, n), node, at)[n]
		if !ok {
			t.Logf("gameday: %s: %s answered no closed-Policy request by GCRA after %s", GD5, n, after)
			worst = time.Duration(math.MaxInt64)
			continue
		}
		d := r.NodeTime(first).Sub(r.NodeTime(at))
		t.Logf("gameday: %s: %s first answered by GCRA %v of Node time (%v of real time) after %s", GD5, n, d, first.Sub(at), after)
		worst = max(worst, d)
		served = latest(served, first)
	}
	return chaos.Check("slowest Node's first GCRA answer after "+after+", seconds", chaos.Functional, worst.Seconds(), chaos.LE, ServingGCRA.Seconds()), served
}

// gd5OfNode returns the requests node attributes to the Node named n.
func gd5OfNode(reqs []chaos.Request, node func(chaos.Request) string, n string) []chaos.Request {
	var out []chaos.Request
	for _, q := range reqs {
		if node(q) == n {
			out = append(out, q)
		}
	}
	return out
}

// gd5Charged returns, per Consumer, the requests the /limited and
// /limited-closed quotas could have charged a deployment between two
// instants: the admitted ones sent then, the env run's gd5Fresh stream
// included. Both Policies charge the Consumer's one chaos-requests quota.
func gd5Charged(l *chaos.Load, from, to time.Time) map[string]int64 {
	out := map[string]int64{}
	for _, name := range []string{"limited", "closed", gd5Fresh} {
		st := l.StreamNamed(name)
		if st == nil {
			continue
		}
		for _, q := range st.Requests() {
			if !q.Sent.Before(from) && q.Sent.Before(to) && chaos.Admitted(q) {
				out[q.Key]++
			}
		}
	}
	return out
}

// gd5Earliest returns the earliest instant of at.
func gd5Earliest(at map[string]time.Time) time.Time {
	var out time.Time
	for _, t := range at {
		if out.IsZero() || t.Before(out) {
			out = t
		}
	}
	return out
}

// gd5Rotate rewrites every Node's file secret holding the main
// connection's URL in place, by an atomic rename, which each Node's
// resolver sees at its next poll and opens a client to at once
// (docs/architecture/11-scalability-and-distributed-state.md "State client
// and RZ-STS error codes").
func gd5Rotate(r *chaos.Run, url string) error {
	for _, n := range r.Top.Nodes() {
		path, ok := n.SecretPath(chaos.RefStateStoreURL)
		if !ok {
			return fmt.Errorf("gameday: %s has no State Store URL file", n.Name())
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(url), 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return nil
}

// gd5Restart Drains and restarts one zone's Nodes with the new URL in
// their environment, as an env secret's value changes, and returns when
// each was ready again, once the balancer has them all back and each gave
// its first closed-Policy GCRA answer since, or gd5Await passed. Stop
// walks the one clock through a Drain in 1 s steps, so the next zone's
// Drain inside the interval GD-5 bounds would count that walk, many times
// faster than real time, as the Node's recovery; a returning Node takes
// its first connections only as the balancer's slow start ramps it up.
// An operator rolls the next zone once this one serves, too.
func gd5Restart(ctx context.Context, t *testing.T, r *chaos.Run, l *chaos.Load, zone, url string) map[string]time.Time {
	t.Helper()
	var ns []*topology.Node
	for _, n := range r.Top.Nodes() {
		if r.Zone(n.Name()) == zone {
			if err := n.Setenv(chaos.EnvStateStoreURL, url); err != nil {
				t.Fatal(err)
			}
			ns = append(ns, n)
		}
	}
	ready := map[string]time.Time{}
	var names []string
	r.Inject("drain-and-restart", "zone-"+zone, func() error {
		for _, n := range ns {
			if code, err := n.Stop(ctx); err != nil {
				return err
			} else if code != 0 {
				t.Logf("gameday: %s: %s exited %d after its Drain", GD5, n.Name(), code)
			}
		}
		for _, n := range ns {
			if err := n.Start(ctx); err != nil {
				return err
			}
			ready[n.Name()] = time.Now()
			names = append(names, n.Name())
		}
		return nil
	})
	if err := r.HTTP.WaitHealthy(ctx, slices.Clone(names)...); err != nil {
		t.Fatal(err)
	}
	// The gd5Fresh stream is the one whose requests reach a restarted
	// Node; the criterion also counts the closed stream's.
	node := nodeOf(r)
	fresh := l.StreamNamed(gd5Fresh)
	serving := func() bool {
		reqs := fresh.Requests()
		for n, at := range ready {
			if _, ok := FirstGCRA(reqs, node, at)[n]; !ok {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(gd5Await)
	for !serving() && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(gd5AwaitPoll):
		}
	}
	return ready
}
