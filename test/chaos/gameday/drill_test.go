// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package gameday

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/test/chaos"
)

func drillAt(ms int) time.Time {
	return time.Unix(2_000_000, 0).Add(time.Duration(ms) * time.Millisecond)
}

// failedReq is a failed request on connection local of node, sent at sent
// and done at done.
func failedReq(sent, done int, local, node string) chaos.Request {
	return chaos.Request{Intended: drillAt(sent), Sent: drillAt(sent), Done: drillAt(done), Outcome: loadgen.Reset, Local: local, Node: node}
}

func isFailed(q chaos.Request) bool { return q.Outcome != loadgen.Success }

func byNodeField(q chaos.Request) string { return q.Node }

// TestInFlight checks CE-1's error rule per kill window: failures on the
// victim's connections sent before the connection's first failure pass,
// one on another Node or sent after its connection failed does not, and a
// client address an earlier window's connection used counts again only in
// its own window.
func TestInFlight(t *testing.T) {
	reqs := []chaos.Request{
		failedReq(100, 500, "c1", "node-2"),
		failedReq(200, 500, "c1", "node-2"),
		failedReq(300, 600, "c2", "node-2"),
		failedReq(700, 800, "c1", "node-2"), // sent after c1 failed
		failedReq(350, 600, "c3", "node-1"), // a survivor's failure
		failedReq(5100, 5200, "c1", "node-2"),
	}
	if e, l := InFlight(reqs, isFailed, byNodeField, "node-2", drillAt(0), drillAt(5000)); e != 1 || l != 1 {
		t.Errorf("InFlight over the first kill = %d elsewhere, %d late; want 1, 1", e, l)
	}
	if e, l := InFlight(reqs, isFailed, byNodeField, "node-2", drillAt(5000), drillAt(9000)); e != 0 || l != 0 {
		t.Errorf("InFlight over the second kill = %d elsewhere, %d late; want 0, 0: c1's earlier failure is another window's", e, l)
	}
}

// TestAlsoFor checks that a pass criterion shows under its own name and
// the failure catalog row's, with one verdict and kind.
func TestAlsoFor(t *testing.T) {
	c := chaos.Check("failed requests on the surviving Nodes", chaos.Functional, 2, chaos.EQ, 0)
	cs := AlsoFor(FC1, c)
	if len(cs) != 2 || cs[0] != c || cs[1].Name != "FC-1: failed requests on the surviving Nodes" ||
		cs[1].Verdict != chaos.VerdictFail || cs[1].Kind() != chaos.Functional || cs[1].Value != 2 {
		t.Errorf("AlsoFor = %+v", cs)
	}
}

// TestFirstGCRA checks the first GCRA answer per Node after an instant:
// admitted or RZ-RL-002 answers count, State Store codes and local
// denials do not, nor requests sent earlier or on no Node.
func TestFirstGCRA(t *testing.T) {
	reqs := []chaos.Request{
		{Sent: drillAt(50), Done: drillAt(60), Status: 200, Node: "node-1"},
		{Sent: drillAt(100), Done: drillAt(150), Status: 503, Code: chaos.CodeSTSOpen, Node: "node-1"},
		{Sent: drillAt(110), Done: drillAt(400), Status: 429, Code: chaos.CodeGCRADenied, Node: "node-1"},
		{Sent: drillAt(120), Done: drillAt(300), Status: 200, Node: "node-1"},
		{Sent: drillAt(130), Done: drillAt(200), Status: 429, Code: chaos.CodeLocalDenied, Node: "node-2"},
		{Sent: drillAt(140), Done: drillAt(210), Status: 200, Node: ""},
	}
	got := FirstGCRA(reqs, byNodeField, drillAt(100))
	if len(got) != 1 || !got["node-1"].Equal(drillAt(300)) {
		t.Errorf("FirstGCRA = %v, want node-1 at 300 ms only", got)
	}
}

// TestModeSpan checks a Node's failureMode period from its decisions'
// Node times, in any order.
func TestModeSpan(t *testing.T) {
	if got := ModeSpan(nil); got != 0 {
		t.Errorf("ModeSpan(nil) = %v", got)
	}
	pts := []chaos.Point{{Node: drillAt(3000)}, {Node: drillAt(1000)}, {Node: drillAt(12000)}}
	if got := ModeSpan(pts); got != 11*time.Second {
		t.Errorf("ModeSpan = %v, want 11s", got)
	}
}

// TestExtraAdmissions checks FC-1's count: admissions per key over the
// aligned windows against (W + 1) × requests.
func TestExtraAdmissions(t *testing.T) {
	ident := func(x time.Time) time.Time { return x }
	var reqs []chaos.Request
	add := func(key string, n, ms int) {
		for range n {
			reqs = append(reqs, chaos.Request{Key: key, Sent: drillAt(ms), Status: 200})
		}
	}
	add("a", 25, 500)  // window 0
	add("a", 20, 1500) // window 1
	add("b", 10, 500)
	reqs = append(reqs, chaos.Request{Key: "a", Sent: drillAt(1600), Status: 429, Code: chaos.CodeGCRADenied})
	// Two windows, requests 10: the healthy bound is 30.
	worst, key := ExtraAdmissions(reqs, ident, drillAt(0), drillAt(1999), 10)
	if worst != 15 || key != "a" {
		t.Errorf("ExtraAdmissions = %d on %q, want 15 on a", worst, key)
	}
	if worst, _ := ExtraAdmissions(nil, ident, drillAt(0), drillAt(1999), 10); worst != -30 {
		t.Errorf("ExtraAdmissions of nothing = %d, want -30", worst)
	}
}

// TestByNodeAndCut checks the split of requests by a Node set and GD-4's
// cut half.
func TestByNodeAndCut(t *testing.T) {
	reqs := []chaos.Request{{Node: "node-1"}, {Node: "node-2"}, {Node: ""}, {Node: "node-3"}}
	in, out := ByNode(reqs, byNodeField, []string{"node-1", "node-3"})
	if len(in) != 2 || len(out) != 2 {
		t.Errorf("ByNode = %d in, %d out; want 2, 2", len(in), len(out))
	}
	for _, tc := range []struct {
		nodes []string
		want  []string
	}{
		{nil, nil},
		{[]string{"node-1"}, []string{"node-1"}},
		{[]string{"node-1", "node-2", "node-3"}, []string{"node-1"}},
		{[]string{"n1", "n2", "n3", "n4", "n5", "n6", "n7", "n8", "n9", "n10"}, []string{"n1", "n2", "n3", "n4", "n5"}},
	} {
		if got := Cut(tc.nodes); !slices.Equal(got, tc.want) {
			t.Errorf("Cut(%v) = %v, want %v", tc.nodes, got, tc.want)
		}
	}
}

// TestCounters checks the quota checks: an under-charge is an admitted
// request no counter holds, and a replacement's counters start empty when
// they hold no more than was charged since it accepted connections.
func TestCounters(t *testing.T) {
	admitted := map[string]int64{"chaos-1": 10, "chaos-2": 5}
	if got := UnderCharge(admitted, map[string]int64{"chaos-1": 12, "chaos-2": 3}); got != 2 {
		t.Errorf("UnderCharge = %d, want 2", got)
	}
	if got := StartsEmpty(map[string]int64{"chaos-1": 9}, map[string]int64{"chaos-1": 10}); got != 0 {
		t.Errorf("StartsEmpty of a fresh replacement = %d, want 0", got)
	}
	if got := StartsEmpty(map[string]int64{"chaos-1": 5000, "chaos-3": 2}, map[string]int64{"chaos-1": 10}); got != 4992 {
		t.Errorf("StartsEmpty of restored counters = %d, want 4992", got)
	}
}

// TestCPU checks GD-2's CPU arithmetic: the calibrated rate for the
// three-zone threshold, and GOMAXPROCS read from a process environment.
func TestCPU(t *testing.T) {
	if got := RateForShare(1000, 0.25, ZoneScaleOut); math.Abs(got-2120) > 1e-9 {
		t.Errorf("RateForShare = %v, want 2120", got)
	}
	if got := RateForShare(1000, 0, ZoneScaleOut); got != 1000 {
		t.Errorf("RateForShare without a share = %v, want 1000", got)
	}
	env := []byte("PATH=/bin\x00GOMAXPROCS=3\x00RURALZ_ZONE=a\x00")
	if n, ok := GOMAXPROCSOf(env); !ok || n != 3 {
		t.Errorf("GOMAXPROCSOf = %d, %v; want 3", n, ok)
	}
	for _, bad := range []string{"PATH=/bin\x00", "GOMAXPROCS=x\x00", "GOMAXPROCS=0"} {
		if n, ok := GOMAXPROCSOf([]byte(bad)); ok {
			t.Errorf("GOMAXPROCSOf(%q) = %d, want none", bad, n)
		}
	}
}

// TestWaitNodeTime checks that the wait ends by the Nodes' clock, not by
// real time: with Node time at 90% of real time, a condition that holds
// 55 s of Node time in, past 60 s of real time, is met under a 60 s bound,
// a condition past the bound is not, and a Node clock that stopped ends
// the wait at the real-time guard.
func TestWaitNodeTime(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	const poll = 100 * time.Millisecond
	tests := []struct {
		name  string
		speed float64 // Node time per real time
		met   time.Duration
		want  bool
	}{
		{"Node clock at 90%", 0.9, 55 * time.Second, true},
		{"condition past the bound", 1, 61 * time.Second, false},
		{"stopped Node clock", 0, time.Second, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wall := clocktest.New(start)
			node := func() time.Time {
				return start.Add(time.Duration(float64(wall.Now().Sub(start)) * tc.speed))
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var got bool
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				got = WaitNodeTime(t.Context(), wall, node, poll, time.Minute, func() bool { return !node().Before(start.Add(tc.met)) })
				cancel()
			}()
			for wall.BlockUntil(ctx, 1) == nil {
				wall.Advance(poll)
			}
			<-finished
			if got != tc.want {
				t.Errorf("WaitNodeTime = %v at %v of real time, want %v", got, wall.Now().Sub(start), tc.want)
			}
			if tc.speed == 0 && wall.Now().Sub(start) > 90*time.Second+poll {
				t.Errorf("a stopped Node clock waited %v of real time, past the 90 s guard", wall.Now().Sub(start))
			}
		})
	}
}
