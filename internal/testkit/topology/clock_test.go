// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"errors"
	"testing"
	"time"
)

// Tests for the topology's one controllable clock
// (docs/engineering/03-testing-and-quality-strategy.md "Hermetic rule
// scope"): every Node follows it, a later process starts at its time, a
// stopped Node catches up, and a real-time topology refuses it.

// Advance moves every Node's clock and returns once each fired its due
// timers: the stand-ins' 10 s boot timer fires on both Nodes exactly when
// the clock passes it, and every Node reads the topology's time.
func TestClockAdvancesEveryNode(t *testing.T) {
	topo := standinTopology(t)
	nodes := addNodes(t, topo, 2, nodeOptions(t, topo))
	client := topo.HTTPClient()
	check := func(when, timer string) {
		t.Helper()
		for _, n := range nodes {
			resp := get(t, client, n.URL("http")+"/")
			if got := clockReading(t, resp); !got.Equal(topo.Now()) {
				t.Errorf("%s: %s clock reads %s, the topology %s", when, n.Name(), format(got), format(topo.Now()))
			}
			if got := resp.Header.Get("X-Standin-Timer"); got != timer {
				t.Errorf("%s: %s timer %s, want %s", when, n.Name(), got, timer)
			}
		}
	}
	start := topo.Now()
	if start.IsZero() {
		t.Fatal("the clock has no reading with Nodes running")
	}
	check("at start", "pending")
	if err := topo.Advance(t.Context(), standinTimer-time.Second); err != nil {
		t.Fatal(err)
	}
	check("one second before the timer", "pending")
	if err := topo.Advance(t.Context(), time.Second); err != nil {
		t.Fatal(err)
	}
	check("at the timer's deadline", "fired")
	if got := topo.Now().Sub(start); got != standinTimer {
		t.Errorf("the clock moved %v, want %v", got, standinTimer)
	}
	if err := topo.Advance(t.Context(), -time.Second); err == nil {
		t.Error("a negative advance succeeded")
	}
}

// startReading is the first time a stand-in's clock showed, which a
// ruralzd process takes as its start.
func startReading(t *testing.T, resp answer) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, resp.Header.Get("X-Standin-Start"))
	if err != nil {
		t.Fatalf("X-Standin-Start %q: %v", resp.Header.Get("X-Standin-Start"), err)
	}
	return at
}

// A process starting while the topology's clock leads real time, as after
// a walk, starts at the topology's time: its first reading, a ruralzd
// process's start, is that time, and its 10 s boot timer runs from it
// rather than firing at once, as when its clock started at real time and
// jumped forward after it read its start (a handover successor's 60 s
// gate expired that way).
func TestClockStartsALaterProcessAtItsTime(t *testing.T) {
	topo := standinTopology(t)
	addNodes(t, topo, 1, nodeOptions(t, topo))
	if err := topo.Advance(t.Context(), time.Hour); err != nil {
		t.Fatal(err)
	}
	at := topo.Now()
	second := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	resp := get(t, topo.HTTPClient(), second.URL("http")+"/")
	if got := startReading(t, resp); !got.Equal(at) {
		t.Errorf("the new Node started at %s, want the topology's %s", format(got), format(at))
	}
	if got := clockReading(t, resp); !got.Equal(topo.Now()) {
		t.Errorf("the new Node reads %s, the topology %s", format(got), format(topo.Now()))
	}
	if got := resp.Header.Get("X-Standin-Timer"); got != "pending" {
		t.Errorf("the new Node's boot timer is %s at its start, want pending", got)
	}
	if err := topo.Advance(t.Context(), standinTimer); err != nil {
		t.Fatal(err)
	}
	if resp := get(t, topo.HTTPClient(), second.URL("http")+"/"); resp.Header.Get("X-Standin-Timer") != "fired" {
		t.Errorf("the new Node's boot timer is %s %v after its start, want fired", resp.Header.Get("X-Standin-Timer"), standinTimer)
	}
}

// A process starting later in real time than the topology's time starts
// at that time too: no Node's clock moves.
func TestClockStartsALaterProcessWithoutMovingNodes(t *testing.T) {
	topo := standinTopology(t)
	first := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	before := topo.Now()
	second := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	if after := topo.Now(); !after.Equal(before) {
		t.Fatalf("the clock read %s before the second start and %s after, want it unmoved", format(before), format(after))
	}
	for _, n := range []*Node{first, second} {
		resp := get(t, topo.HTTPClient(), n.URL("http")+"/")
		if got := clockReading(t, resp); !got.Equal(before) {
			t.Errorf("%s reads %s, want the topology's %s", n.Name(), format(got), format(before))
		}
	}
	resp := get(t, topo.HTTPClient(), second.URL("http")+"/")
	if got := startReading(t, resp); !got.Equal(before) {
		t.Errorf("the second Node started at %s, want the topology's %s", format(got), format(before))
	}
}

// Nodes of a release build run on real time: the clock refuses to move.
func TestRealTimeTopologyRefusesTheClock(t *testing.T) {
	topo := standinTopology(t, func(c *Config) {
		c.TestClock = false
		c.Env = []string{standinEnv + "=1"}
	})
	n := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	if topo.TestClock() || topo.Clock().Enabled() {
		t.Error("TestClock reports the controllable clock")
	}
	if err := topo.Advance(t.Context(), time.Second); !errors.Is(err, ErrNoTestClock) {
		t.Errorf("Advance = %v, want ErrNoTestClock", err)
	}
	if !topo.Now().IsZero() {
		t.Errorf("Now = %v, want the zero time", topo.Now())
	}
	// A real-time Drain takes its 5 s accept window; end it at once.
	if err := n.Kill(t.Context()); err != nil {
		t.Fatal(err)
	}
}
