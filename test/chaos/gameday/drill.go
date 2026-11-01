// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package gameday

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/test/chaos"
)

// Game day identifiers (docs/operations/04-high-availability-and-disaster-recovery.md
// "Game days") and the failure catalog rows asserted on their runs.
const (
	GD1 = "GD-1"
	GD2 = chaos.GD2
	GD3 = "GD-3"
	GD4 = "GD-4"
	GD5 = "GD-5"
	FC1 = "FC-1"
	FC3 = "FC-3"
	// FC11 is asserted on GD-4's run.
	FC11 = "FC-11"
)

// Bounds the drills assert (target).
const (
	// RestartReady bounds a restarted Node's readiness from its start
	// (GD-1).
	RestartReady = 30 * time.Second
	// LocalOnlyEntry is the local-only entry of a key a Node sees first:
	// a restarted Node sees every key first, so FC-1's extra ceiling is
	// counted over the windows up to this long after its readiness
	// (docs/architecture/11-scalability-and-distributed-state.md
	// "Distributed rate limits").
	LocalOnlyEntry = 10 * time.Second
	// ZoneScaleOut is the CPU share each Node runs at before GD-2's fault:
	// the scale-out threshold of three zones, 80% × (3 − 1) / 3.
	ZoneScaleOut = 0.53
	// SurvivorCPU bounds the surviving Nodes' CPU share after a zone loss
	// (GD-2, FC-3).
	SurvivorCPU = 0.80
	// FailureModeBound bounds a Node's State Store failureMode after a
	// zone loss or a failover (GD-2).
	FailureModeBound = 15 * time.Second
	// ServingGCRA bounds the time from a replacement State Store accepting
	// connections, or from a Node reaching it, to the Node's first GCRA
	// answer (GD-5).
	ServingGCRA = 10 * time.Second
)

// AlsoFor returns c, a drill's pass criterion, and a copy of it named for
// the failure catalog row whose Behavior or Detection it also asserts, so
// each verdict shows under both in the run's record.
func AlsoFor(row string, c chaos.Criterion) []chaos.Criterion {
	d := c
	d.Name = row + ": " + c.Name
	return []chaos.Criterion{c, d}
}

// InFlight is CE-1's error rule over the failed requests due in [from,
// to): each failure must have been on a connection the L4 balancer
// attributes to victim and sent before the client saw that connection's
// first failure. It returns the failures attributed to another Node or
// none, and those sent after their connection had failed. Windowing it
// per kill keeps a client address an earlier connection used from
// counting against a later one.
func InFlight(reqs []chaos.Request, failed func(chaos.Request) bool, node func(chaos.Request) string, victim string, from, to time.Time) (elsewhere, late int64) {
	in := func(q chaos.Request) bool { return !q.Intended.Before(from) && q.Intended.Before(to) && failed(q) }
	seen := map[string]time.Time{}
	for _, q := range reqs {
		if !in(q) || q.Local == "" {
			continue
		}
		if t, ok := seen[q.Local]; !ok || q.Done.Before(t) {
			seen[q.Local] = q.Done
		}
	}
	for _, q := range reqs {
		if !in(q) {
			continue
		}
		switch {
		case node(q) != victim:
			elsewhere++
		case q.Sent.After(seen[q.Local]):
			late++
		}
	}
	return elsewhere, late
}

// GCRAAnswered reports a request a ratelimit Policy decided with a GCRA
// answer: admitted, or denied by GCRA or its over-limit cache.
func GCRAAnswered(q chaos.Request) bool { return chaos.Admitted(q) || q.Code == chaos.CodeGCRADenied }

// FirstGCRA returns, per Node, when its first request sent at or after
// from that a ratelimit Policy decided by GCRA completed: on a closed
// Policy, which never admits without an answer, the Node's first
// successful State Store call there. node attributes a request to its
// Node; requests without one are skipped.
func FirstGCRA(reqs []chaos.Request, node func(chaos.Request) string, from time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	for _, q := range reqs {
		if q.Sent.Before(from) || !GCRAAnswered(q) {
			continue
		}
		n := node(q)
		if n == "" {
			continue
		}
		if t, ok := out[n]; !ok || q.Done.Before(t) {
			out[n] = q.Done
		}
	}
	return out
}

// ModeSpan returns the Node-time span from the earliest to the latest of
// points: a Node's failureMode period from its first to its last
// fail-open or fail-closed decision, as CE-5 measures it.
func ModeSpan(points []chaos.Point) time.Duration {
	if len(points) == 0 {
		return 0
	}
	lo, hi := points[0].Node, points[0].Node
	for _, p := range points[1:] {
		if p.Node.Before(lo) {
			lo = p.Node
		}
		if p.Node.After(hi) {
			hi = p.Node
		}
	}
	return hi.Sub(lo)
}

// ExtraAdmissions returns, over the aligned windows of Node time from from
// to to, how far the key admitted most exceeds the healthy GCRA bound of
// (W + 1) × requests for W windows, and that key; admissions count at the
// Node time a Node may first have read when the request was sent
// (nodeTime). A key under the bound gives a non-positive excess.
func ExtraAdmissions(reqs []chaos.Request, nodeTime func(time.Time) time.Time, from, to time.Time, requests int64) (worst int64, key string) {
	from, to = from.Truncate(time.Second), to.Truncate(time.Second).Add(time.Second)
	windows := int64(to.Sub(from) / time.Second)
	admitted := map[string]int64{}
	for _, q := range reqs {
		if !chaos.Admitted(q) || q.Key == "" {
			continue
		}
		if at := nodeTime(q.Sent); !at.Before(from) && at.Before(to) {
			admitted[q.Key]++
		}
	}
	worst = -(windows + 1) * requests
	for _, k := range slices.Sorted(maps.Keys(admitted)) {
		if extra := admitted[k] - (windows+1)*requests; extra > worst || key == "" {
			worst, key = extra, k
		}
	}
	return worst, key
}

// ByNode splits requests by whether node attributes them to a Node of
// set: in holds those, out the rest, unattributed ones included.
func ByNode(reqs []chaos.Request, node func(chaos.Request) string, set []string) (in, out []chaos.Request) {
	for _, q := range reqs {
		if slices.Contains(set, node(q)) {
			in = append(in, q)
		} else {
			out = append(out, q)
		}
	}
	return in, out
}

// Cut returns the Nodes GD-4 cuts from the State Store: the first half of
// nodes, at least one.
func Cut(nodes []string) []string {
	if len(nodes) == 0 {
		return nil
	}
	return slices.Clone(nodes[:max(1, len(nodes)/2)])
}

// UnderCharge returns how many admitted requests no quota counter holds,
// summed over the Consumers: each Consumer's admitted count above its
// counter. A State Store that started empty holds at most what the Nodes
// charged since, so StartsEmpty uses it the other way round.
func UnderCharge(admitted, counters map[string]int64) int64 {
	var under int64
	for c, n := range admitted {
		under += max(0, n-counters[c])
	}
	return under
}

// StartsEmpty returns how far the quota counters of a State Store that
// replaced a lost one exceed what the Nodes could have charged it since
// it accepted connections, summed over the Consumers: 0 when its counters
// started empty, as nothing restores them
// (docs/operations/04-high-availability-and-disaster-recovery.md "State
// Store rebuild").
func StartsEmpty(counters, chargedSince map[string]int64) int64 {
	return UnderCharge(counters, chargedSince)
}

// RateForShare returns the rate at which the Nodes run at target CPU
// share, from the share they ran at under rate, CPU taken as linear in
// the offered rate; rate itself when share is not positive.
func RateForShare(rate, share, target float64) float64 {
	if share <= 0 {
		return rate
	}
	return rate * target / share
}

// GOMAXPROCSOf returns the GOMAXPROCS setting of a process environment in
// the NUL-separated form of /proc/<pid>/environ.
func GOMAXPROCSOf(environ []byte) (int, bool) {
	for kv := range bytes.SplitSeq(environ, []byte{0}) {
		v, ok := bytes.CutPrefix(kv, []byte("GOMAXPROCS="))
		if !ok {
			continue
		}
		n, err := strconv.Atoi(string(v))
		return n, err == nil && n > 0
	}
	return 0, false
}

// WaitNodeTime polls cond every poll of real time on wall until it holds
// or node, the Nodes' clock, has moved d past its reading at the start,
// and reports whether it held. A bound that comes from a timer on the
// Nodes' clock, such as a failure backoff or a key set's half-life, holds
// in Node time, which runs behind real time while the clock pump advances
// late, so a real-time bound of d can end before the timer is due. A
// real-time guard of half again d ends a wait on a clock that stopped.
func WaitNodeTime(ctx context.Context, wall clock.Clock, node func() time.Time, poll, d time.Duration, cond func() bool) bool {
	until, guard := node().Add(d), wall.Now().Add(d+d/2)
	for !cond() {
		if !node().Before(until) || !wall.Now().Before(guard) {
			return false
		}
		t := wall.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C():
		}
	}
	return true
}
