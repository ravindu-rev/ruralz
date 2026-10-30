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
// scope"): every Node follows it, a later process joins it, a stopped Node
// catches up, and a real-time topology refuses it.

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
			resp, _ := get(t, client, n.URL("http")+"/")
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

// A process starting while the topology's clock is ahead of real time is
// brought forward to it at once.
func TestClockBringsALaterProcessForward(t *testing.T) {
	topo := standinTopology(t)
	addNodes(t, topo, 1, nodeOptions(t, topo))
	if err := topo.Advance(t.Context(), time.Hour); err != nil {
		t.Fatal(err)
	}
	second := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	resp, _ := get(t, topo.HTTPClient(), second.URL("http")+"/")
	if got := clockReading(t, resp); !got.Equal(topo.Now()) {
		t.Errorf("the new Node reads %s, the topology %s", format(got), format(topo.Now()))
	}
}

// A process starting later than the topology's time moves every other
// Node forward to its start, since its clock cannot move back.
func TestClockMovesNodesToALaterStart(t *testing.T) {
	topo := standinTopology(t)
	first := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	before := topo.Now()
	addNodes(t, topo, 1, nodeOptions(t, topo))
	after := topo.Now()
	if !after.After(before) {
		t.Fatalf("the clock read %s before the second start and %s after, want it moved to the later start", format(before), format(after))
	}
	resp, _ := get(t, topo.HTTPClient(), first.URL("http")+"/")
	if got := clockReading(t, resp); !got.Equal(after) {
		t.Errorf("the first Node reads %s, want the second's start %s", format(got), format(after))
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
