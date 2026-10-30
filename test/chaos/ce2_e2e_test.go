// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
)

// ce2ConnsPerNode is the HTTP/2 connections each of CE-2's two HTTP/2
// streams opens per Node, so the drained Node holds more than
// MinHeldConns at stop-accepting with its HTTP/1.1 connections.
const ce2ConnsPerNode = 60

// ce2SlowDelay is the mock Upstream's delay on CE-2's HTTP/1.1 stream:
// its requests are in flight when a shard shuts down, and their
// connections busy, and a close it delays stays more than 1 s before
// the shard after next.
const ce2SlowDelay = 100 * time.Millisecond

// TestCE2NodeDrained is CE-2 and V-4: the Node holding the most
// connections, HTTP/1.1 through the http listener and HTTP/2 through
// both, is sent SIGTERM after the baseline at 50% load. No new request
// fails, every request in flight completes, the Node's connections close
// in the K = 8 waves of its server shards as the L4 balancer sees them,
// none is left at the Drain deadline, and the Node exits 0 within 30 s
// (docs/architecture/11-scalability-and-distributed-state.md "Chaos
// experiments"). WebSockets and SSE join in M3.
func TestCE2NodeDrained(t *testing.T) {
	s := saturation(t)
	r := suite.Start(t, Spec{Name: CE2, HTTPS: true, Mock: func(_ int, c *mockup.Config) { c.Overrides = true }})
	rate := suite.Profile.LoadShare * s
	conns := ce2ConnsPerNode * r.Nodes
	slow := func(q *http.Request, _ uint64) { q.Header.Set(mockup.HeaderDelay, ce2SlowDelay.String()) }
	l := r.StartLoad(LoadSpec{Duration: Baseline + FaultPhase + RecoveryTime, Streams: []StreamSpec{
		{Name: "http1", Path: PathPlain, Rate: rate * 0.4, Prepare: slow},
		{Name: "h2c", Path: PathPlain, Protocol: loadgen.H2C, Connections: conns, Rate: rate * 0.3},
		{Name: "h2", Path: PathPlain, HTTPS: true, Protocol: loadgen.H2, Connections: conns, Rate: rate * 0.3},
	}})
	watch := StartConnWatch(r.Context(), 0, r.HTTP, r.HTTPS)
	defer watch.Stop()
	r.Begin(l)
	base := r.Hold(l, PhaseBaseline, Baseline)
	open := watch.Open()
	victim := r.Top.Nodes()[0]
	for _, n := range r.Top.Nodes() {
		if open[n.Name()] > open[victim.Name()] {
			victim = n
		}
	}
	p := victim.Process()
	sig := r.Inject("sigterm", victim.Name(), func() error { return victim.Signal(r.Context(), syscall.SIGTERM) })
	var exited time.Time
	select {
	case <-p.Done():
		exited = time.Now()
	case <-time.After(2 * ExitBound):
	case <-r.Context().Done():
		t.Fatal(r.Context().Err())
	}
	r.Hold(l, PhaseFault, FaultPhase)
	rec := r.Hold(l, PhaseRecovery, RecoveryTime)
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	watch.Stop()
	for _, st := range l.Streams {
		r.IntervalLog(st.Spec.Name, st)
	}
	r.Baseline(l, base)
	r.Attribute(Attribution(l, sig, rec.End))

	// The Drain's timeline in Node time, from the Node's reading of the
	// SIGTERM.
	sigLo, sigHi := r.NodeSpan(sig)
	stopLo, stopHi := sigLo.Add(AcceptWindow), sigHi.Add(AcceptWindow)
	stopReal := sig.Add(AcceptWindow)
	if r.Pump != nil {
		if at, ok := r.Pump.RealTime(stopHi); ok {
			stopReal = at
		}
	}
	held := HeldAt(watch.Conns(), victim.Name(), stopReal)
	proto := streamOfConn(l)
	byStream := map[string]int{}
	closes := make([]Span, 0, len(held))
	for _, c := range held {
		byStream[proto(c)]++
		realLo, realHi, closed := c.CloseSpan()
		if !closed {
			t.Logf("chaos: held connection %d from %s (%s) still open at the end: opened %v, %d bytes up, %d down, %d redispatches",
				c.ID, c.Client, proto(c), c.Opened.Sub(sig), c.BytesUp, c.BytesDown, c.Redispatches)
			closes = append(closes, Span{Lo: NeverClosed, Hi: NeverClosed})
			continue
		}
		// A shard's shutdown fires within an advance and its closes follow
		// the answer, so a close may stem from any Node time of the
		// advance before it.
		lo, _ := r.CauseSpan(realLo)
		_, hi := r.CauseSpan(realHi)
		closes = append(closes, Span{Lo: lo.Sub(stopHi), Hi: hi.Sub(stopLo)})
	}
	waves := AnalyzeWaves(closes)
	logCloses(t, closes, held, proto)
	for name, n := range byStream {
		r.Breakdown("held_connections_"+name, float64(n))
	}
	// Shard i of every listener shuts down together, so a wave holds up
	// to the sum over listeners of ⌈M_l / 8⌉.
	perListener := map[string]int{}
	for _, c := range held {
		perListener[c.Listener]++
	}
	waveMax := 0
	for _, m := range perListener {
		waveMax += (m + WaveCount - 1) / WaveCount
	}
	r.Breakdown("two_waves_by_listener", float64(2*waveMax))
	for i, n := range waves.Eighths {
		r.Breakdown("closes_eighth_"+string(rune('1'+i)), float64(n))
	}
	exit := float64(2 * ExitBound / time.Second)
	code := -1
	if !exited.IsZero() {
		exitLo, _ := r.NodeSpan(exited)
		exit = exitLo.Sub(sigLo).Seconds()
		code = p.State().ExitCode()
	}
	var newFailed, inflightFailed int64
	for _, st := range l.Streams {
		for _, q := range st.Requests() {
			if !st.Failed(q) {
				continue
			}
			switch {
			case !q.Sent.IsZero() && q.Sent.Before(sig) && !q.Done.Before(sig):
				inflightFailed++
			case !q.Intended.Before(sig) && q.Intended.Before(rec.Start):
				newFailed++
			}
		}
	}
	r.Add(
		Check("connections held at stop-accepting", Functional, float64(waves.Held), GE, MinHeldConns),
		Check("jitter-window eighths closing no wave", Functional, float64(waves.Empty), EQ, 0),
		Check("most held connections closed in a 1 s slice of the jitter window", Functional, float64(waves.MaxSlice), LE, float64(waves.SliceBound)),
		Check("held connections left at the Drain deadline", Functional, float64(waves.LeftAtDeadline), EQ, 0),
		Check("exit after the SIGTERM, seconds", Functional, exit, LE, ExitBound.Seconds()),
		Check("exit code", Functional, float64(code), EQ, 0),
		Check("failed new requests", Functional, float64(newFailed), EQ, 0),
		Check("failed in-flight requests", Functional, float64(inflightFailed), EQ, 0),
		Check("recovery errors", Functional, float64(LoadStats(l, rec.Start, rec.End).Failed), EQ, 0),
	)
	var survivors []string
	for _, n := range r.Top.Nodes() {
		if n != victim {
			survivors = append(survivors, n.Name())
		}
	}
	r.StaleReasons(survivors, base.End, rec.End)
	r.Finish()
}

// logCloses logs each held connection's close offset from stop-accepting
// in Node time, in close order, with its stream: the failure evidence of
// the wave criteria.
func logCloses(t *testing.T, closes []Span, held []Conn, proto func(Conn) string) {
	t.Helper()
	type entry struct {
		at     Span
		stream string
	}
	es := make([]entry, len(closes))
	for i := range closes {
		es[i] = entry{closes[i], proto(held[i])}
	}
	slices.SortFunc(es, func(a, b entry) int { return cmp.Compare(a.at.Lo, b.at.Lo) })
	var b strings.Builder
	for _, e := range es {
		if e.at.Hi == NeverClosed {
			fmt.Fprintf(&b, " open/%s", e.stream)
			continue
		}
		fmt.Fprintf(&b, " %d..%dms/%s", e.at.Lo.Milliseconds(), e.at.Hi.Milliseconds(), e.stream)
	}
	t.Logf("chaos: held connections' closes after stop-accepting:%s", b.String())
}

// streamOfConn returns a function naming the stream whose requests
// travelled a balancer connection, by the client address both report.
func streamOfConn(l *Load) func(Conn) string {
	by := map[string]string{}
	for _, st := range l.Streams {
		for _, q := range st.Requests() {
			if q.Local != "" {
				by[q.Local] = st.Spec.Name
			}
		}
	}
	return func(c Conn) string {
		if n, ok := by[c.Client]; ok {
			return n
		}
		return "unknown"
	}
}
