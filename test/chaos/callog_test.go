// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// TestCallLogNodeCallsCoverTheBacklog checks that the GCRA calls the Nodes
// counted are read over the span the call log covered, the span of the
// calls it saw: calls the Nodes make while they work off a flood's backlog
// after the fault ended count on both sides. In CE-15's run whose flood
// requests were served up to 8 s after the fault, a window ending 5 s
// after it counted 41,240 of the 43,781 GCRA calls the Nodes and the
// proxies both saw.
func TestCallLogNodeCallsCoverTheBacklog(t *testing.T) {
	s := &Sampler{keep: DefaultKeep(), ids: map[string]int{}, samples: map[string][]Sample{}}
	expo := func(ok, timeout int) promtext.Set {
		text := "# TYPE ruralz_state_calls_total counter\n" +
			`ruralz_state_calls_total{op="gcra",result="ok"} ` + strconv.Itoa(ok) + "\n" +
			`ruralz_state_calls_total{op="gcra",result="timeout"} ` + strconv.Itoa(timeout) + "\n" +
			`ruralz_state_calls_total{op="cache_get",result="ok"} ` + strconv.Itoa(7*ok) + "\n"
		set, err := promtext.ParseBytes([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return set
	}
	// The log covers 0 to 130 s; the flood's fault runs 10 s to 70 s and
	// each Node makes its last 30 calls 8 s after it, past 75 s.
	for _, n := range []string{"node-1", "node-2"} {
		s.record(n, at(0), at(0), at(1), expo(0, 0), nil)
		s.record(n, at(9_000), at(9_000), at(9_001), expo(0, 0), nil)
		s.record(n, at(40_000), at(40_000), at(40_001), expo(59, 1), nil)
		s.record(n, at(70_000), at(70_000), at(70_001), expo(99, 1), nil)
		s.record(n, at(75_000), at(75_000), at(75_001), expo(110, 1), nil)
		s.record(n, at(78_000), at(78_000), at(78_001), expo(129, 1), nil)
		s.record(n, at(125_000), at(125_000), at(125_001), expo(129, 1), nil)
	}
	cl := &CallLog{from: at(0), to: at(130_000)}
	if got := cl.NodeCalls(s, "gcra"); got != 260 {
		t.Errorf("NodeCalls(gcra) = %v, want every call of both Nodes, 260, the late ones included", got)
	}
	if got := cl.NodeCalls(s, "quota"); got != 0 {
		t.Errorf("NodeCalls(quota) = %v, want 0: no Node made one", got)
	}
}
