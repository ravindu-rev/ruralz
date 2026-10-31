// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package gameday

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/test/chaos"
)

// fc16T0 anchors the synthetic timelines of these tests.
func fc16T0() time.Time { return time.Unix(2_000_000, 0) }

func fc16At(ms int) time.Time { return fc16T0().Add(time.Duration(ms) * time.Millisecond) }

// fc16Identity is the Node span of a run without a controllable clock.
func fc16Identity(t time.Time) (lo, hi time.Time) { return t, t }

// TestFC16Fetched checks the fetch log filter: only fetches answered with
// the status asked for and recorded in the window, oldest first.
func TestFC16Fetched(t *testing.T) {
	fetches := []mockidp.Fetch{
		{Time: fc16At(300), Status: http.StatusOK},
		{Time: fc16At(100), Status: http.StatusOK},
		{Time: fc16At(200), Status: http.StatusServiceUnavailable},
		{Time: fc16At(900), Status: http.StatusOK},
	}
	got := FC16Fetched(fetches, http.StatusOK, fc16At(0), fc16At(900))
	if want := []time.Time{fc16At(100), fc16At(300)}; !slices.EqualFunc(got, want, time.Time.Equal) {
		t.Errorf("FC16Fetched(200, [0, 900)) = %v, want %v", got, want)
	}
	if got := FC16Fetched(fetches, http.StatusServiceUnavailable, fc16At(0), fc16At(1000)); len(got) != 1 {
		t.Errorf("FC16Fetched(503) = %v, want one refused fetch", got)
	}
}

// TestFC16Expiry checks the expiry window of the cached key sets: from
// the first fetch's cause span, which reaches one advance back, and to the
// last fetch's Node span a fetch bound later, each plus the lifetime.
func TestFC16Expiry(t *testing.T) {
	// Node time runs 1 h ahead of real time; a cause span reaches 10 ms
	// back and a Node span 10 ms ahead.
	cause := func(x time.Time) (time.Time, time.Time) {
		return x.Add(time.Hour - 10*time.Millisecond), x.Add(time.Hour)
	}
	span := func(x time.Time) (time.Time, time.Time) {
		return x.Add(time.Hour), x.Add(time.Hour + 10*time.Millisecond)
	}
	earliest, latest, ok := FC16Expiry([]time.Time{fc16At(2000), fc16At(1000), fc16At(1500)}, cause, span, FC16Lifetime)
	if !ok {
		t.Fatal("FC16Expiry of three fetches reported none")
	}
	if want := fc16At(1000).Add(time.Hour - 10*time.Millisecond + FC16Lifetime); !earliest.Equal(want) {
		t.Errorf("earliest expiry = %v, want %v", earliest.Sub(fc16T0()), want.Sub(fc16T0()))
	}
	if want := fc16At(2000).Add(FC16FetchBound + time.Hour + 10*time.Millisecond + FC16Lifetime); !latest.Equal(want) {
		t.Errorf("latest expiry = %v, want %v", latest.Sub(fc16T0()), want.Sub(fc16T0()))
	}
	if _, _, ok := FC16Expiry(nil, cause, span, FC16Lifetime); ok {
		t.Error("FC16Expiry without a fetch reported a window")
	}
	if FC16HalfLife != 150*time.Second {
		t.Errorf("FC16HalfLife = %v, want the 150 s half-life of max-age=300", FC16HalfLife)
	}
}

// TestFC16Decided checks which requests count as decided within a Node
// time window, and how they sort by outcome.
func TestFC16Decided(t *testing.T) {
	req := func(sent, done, status int, code string, o loadgen.Outcome) chaos.Request {
		return chaos.Request{Intended: fc16At(sent), Sent: fc16At(sent), Done: fc16At(done), Status: status, Code: code, Outcome: o}
	}
	reqs := []chaos.Request{
		req(100, 110, 200, "", loadgen.Success),
		req(200, 210, 401, "RZ-AUTH-006", loadgen.ClientError),
		req(300, 310, 502, "RZ-UP-001", loadgen.ServerError),
		req(400, 410, 0, "", loadgen.Reset),
		req(50, 120, 200, "", loadgen.Success),   // sent before the window
		req(950, 1010, 200, "", loadgen.Success), // completed after it
		{Intended: fc16At(500), Outcome: loadgen.Missed},
	}
	got := FC16Decided(reqs, fc16Identity, fc16At(100), fc16At(1000), "RZ-AUTH-006")
	if want := (FC16Counts{Admitted: 1, Code: 1, Other: 2}); got != want {
		t.Errorf("FC16Decided = %+v, want %+v", got, want)
	}
	if got := FC16Decided(reqs, fc16Identity, fc16At(100), fc16At(1000), ""); got.Code != 0 || got.Other != 3 {
		t.Errorf("FC16Decided without a code = %+v, want every failure under Other", got)
	}
}

// TestFC16Switch checks the per-Node switch to a failing code: the first
// failure per Node and the admissions a Node made after it.
func TestFC16Switch(t *testing.T) {
	req := func(local string, sent, done, status int, code string) chaos.Request {
		return chaos.Request{Local: local, Intended: fc16At(sent), Sent: fc16At(sent), Done: fc16At(done), Status: status, Code: code}
	}
	nodes := map[string]string{"c1": "node-1", "c2": "node-2"}
	node := func(q chaos.Request) string { return nodes[q.Local] }
	reqs := []chaos.Request{
		req("c1", 100, 110, 200, ""),
		req("c1", 200, 210, 401, "RZ-AUTH-005"),
		req("c1", 300, 310, 401, "RZ-AUTH-005"),
		req("c2", 150, 160, 200, ""),
		req("c2", 250, 260, 200, ""),
		req("c3", 270, 280, 401, "RZ-AUTH-005"), // no Node
	}
	first, after := FC16Switch(reqs, node, "RZ-AUTH-005")
	if len(first) != 1 || !first["node-1"].Equal(fc16At(210)) || after != 0 {
		t.Errorf("FC16Switch = %v, %d; want node-1 at 210 ms and no admission after it", first, after)
	}
	reqs = append(reqs, req("c1", 400, 410, 200, ""))
	if _, after := FC16Switch(reqs, node, "RZ-AUTH-005"); after != 1 {
		t.Errorf("FC16Switch admissions after the switch = %d, want 1", after)
	}
}

// TestFC20Rose checks a counter's rise after an instant, a series that
// first appears after it included.
func TestFC20Rose(t *testing.T) {
	for _, tc := range []struct {
		name  string
		trace []chaos.Point
		want  bool
	}{
		{"grew", []chaos.Point{{Real: fc16At(0), Value: 2}, {Real: fc16At(200), Value: 3}}, true},
		{"flat", []chaos.Point{{Real: fc16At(0), Value: 2}, {Real: fc16At(200), Value: 2}}, false},
		{"appeared after", []chaos.Point{{Real: fc16At(150), Value: 1}}, true},
		{"appeared at zero", []chaos.Point{{Real: fc16At(150), Value: 0}}, false},
		{"no series", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FC20Rose(tc.trace, fc16At(100)); got != tc.want {
				t.Errorf("FC20Rose(%v) = %v, want %v", tc.trace, got, tc.want)
			}
		})
	}
}

// TestFC20OffByOne checks FC-20's one Hot Reload per Node.
func TestFC20OffByOne(t *testing.T) {
	got := FC20OffByOne(map[string]float64{"node-1": 1, "node-2": 2, "node-3": 0, "node-4": 1}, 1)
	if want := []string{"node-2", "node-3"}; !slices.Equal(got, want) {
		t.Errorf("FC20OffByOne = %v, want %v", got, want)
	}
	if got := FC20OffByOne(map[string]float64{"node-1": 1}, 1); got != nil {
		t.Errorf("FC20OffByOne of one reload = %v, want none", got)
	}
}
