// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// epoch is the fake clock's start.
func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// counter is a test emit.Counter.
type counter struct{ n atomic.Uint64 }

func (c *counter) Add(_ emit.Stripe, n uint64) { c.n.Add(n) }

// gauge is a test emit.Gauge.
type gauge struct{ v atomic.Int64 }

func (g *gauge) Add(_ emit.Stripe, d int64) { g.v.Add(d) }
func (g *gauge) Set(v int64)                { g.v.Store(v) }

// metrics are inspectable Upstream handles.
type metrics struct {
	m        *emit.UpstreamMetrics
	passive  *counter
	active   *counter
	skipped  *counter
	healthyG *gauge
}

func newMetrics() *metrics {
	x := &metrics{passive: &counter{}, active: &counter{}, skipped: &counter{}, healthyG: &gauge{}}
	x.m = &emit.UpstreamMetrics{HealthyEndpoints: x.healthyG, ProbesSkipped: x.skipped}
	x.m.Ejections[emit.EjectPassive] = x.passive
	x.m.Ejections[emit.EjectActive] = x.active
	return x
}

// fakeStatus records SetDegraded calls.
type fakeStatus struct {
	mu     sync.Mutex
	events []string
	on     map[string]bool
}

func (s *fakeStatus) SetDegraded(r catalog.Reason, source string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.on == nil {
		s.on = map[string]bool{}
	}
	s.events = append(s.events, fmt.Sprintf("%s/%s=%v", r, source, on))
	s.on[r.String()+"/"+source] = on
}

func (*fakeStatus) SetCleartextHops(catalog.Hop, int) {}

func (s *fakeStatus) held(r catalog.Reason, source string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on[r.String()+"/"+source]
}

func (s *fakeStatus) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// lockedSource is a concurrency-safe deterministic rand.Source.
type lockedSource struct {
	mu  sync.Mutex
	src *rand.PCG
}

func newRand(seed uint64) *lockedSource {
	return &lockedSource{src: rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)}
}

func (l *lockedSource) Uint64() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.src.Uint64()
}

// fixedSource always returns v.
type fixedSource uint64

func (f fixedSource) Uint64() uint64 { return uint64(f) }

// ids returns n sorted identities ep-000 ... ep-(n-1).
func ids(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("ep-%03d", i)
		if n > 1000 {
			out[i] = fmt.Sprintf("ep-%05d", i)
		}
	}
	return out
}

// healthyLog records OnHealthy calls.
type healthyLog struct {
	mu    sync.Mutex
	calls [][2]int
}

func (l *healthyLog) record(h, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, [2]int{h, n})
}

func (l *healthyLog) last() [2]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) == 0 {
		return [2]int{-1, -1}
	}
	return l.calls[len(l.calls)-1]
}

// newTracker returns a Tracker on a fake clock with n Endpoints.
func newTracker(t *testing.T, n int, mod func(*Config)) (*Tracker, *clocktest.Fake, *metrics, *healthyLog) {
	t.Helper()
	clk := clocktest.New(epoch())
	m := newMetrics()
	log := &healthyLog{}
	cfg := Config{Upstream: "orders", Clock: clk, Metrics: m.m, OnHealthy: log.record}
	if mod != nil {
		mod(&cfg)
	}
	tr := NewTracker(cfg)
	if err := tr.SetEndpoints(ids(n)); err != nil {
		t.Fatal(err)
	}
	return tr, clk, m, log
}

// fail ends one attempt to Endpoint i at the clock's now with a failure of
// the given cause; it reports whether the Endpoint was ejected.
func fail(tr *Tracker, clk *clocktest.Fake, i int, c Cause) bool {
	var a Attempt
	now := clk.Now()
	tr.View(now).Begin(&a, i, now)
	return a.End(now, Outcome{Failed: true, Cause: c})
}

// succeed ends one attempt to Endpoint i with response headers and no
// failure.
func succeed(tr *Tracker, clk *clocktest.Fake, i int) {
	var a Attempt
	now := clk.Now()
	tr.View(now).Begin(&a, i, now)
	a.Headers(now)
	a.End(now, Outcome{})
}

// ejectedCount counts passively ejected Endpoints at the clock's now.
func ejectedCount(tr *Tracker, now time.Time) int {
	n := 0
	for _, s := range tr.Status(now) {
		if s.Ejected {
			n++
		}
	}
	return n
}

// waitFor polls cond with a real-time bound; it only waits for goroutines
// the test started, never for fake time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}
