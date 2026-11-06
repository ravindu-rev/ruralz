// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// closedRequest is a closed-Policy request due at ms, answered with code
// (none for a 200).
func closedRequest(ms int, code string) Request {
	q := Request{Intended: at(ms), Sent: at(ms), Done: at(ms + 1), Status: 200, Outcome: loadgen.Success}
	if code != "" {
		q.Status, q.Code, q.Outcome = 503, code, loadgen.ServerError
	}
	return q
}

// newClosedStream returns the closed-Policy stream of a run, which counts
// the State Store codes as achieved, holding reqs.
func newClosedStream(reqs ...Request) *Stream {
	return &Stream{Spec: StreamSpec{Name: "closed", Counted: append(Decisions(), STSCodes()...)}, requests: reqs}
}

// TestErrorRepliesLeaveExpiriesToBreakdowns: a criterion on a connection
// the run's fault does not touch, such as a baseline's, counts the State
// Store's error replies, RZ-STS-002, and records the expiries, RZ-STS-001,
// and the RZ-STS-003 and RZ-STS-004 skips they leave as breakdowns, since
// an expiry there is the runner's real-time latency, not the fault's.
func TestErrorRepliesLeaveExpiriesToBreakdowns(t *testing.T) {
	closed := newClosedStream(
		closedRequest(100, CodeSTSTimeout),
		closedRequest(200, CodeSTSTimeout),
		closedRequest(300, CodeSTSSkipped),
		closedRequest(400, CodeSTSOpen),
		closedRequest(500, CodeSTSError),
		closedRequest(600, ""),
		closedRequest(5000, CodeSTSError), // after the window
	)
	if got := CodesIn(closed, at(0), at(1000), ErrorReplyCodes()...); got != 1 {
		t.Errorf("CodesIn(ErrorReplyCodes) = %d, want the one error reply in the window", got)
	}
	if got := CodesAmong(closed.Requests()[:3], at(0), at(1000), ExpiryCodes()...); got != 3 {
		t.Errorf("CodesAmong(ExpiryCodes) of the first three = %d, want 3", got)
	}
	r := &Run{rec: Record{Breakdowns: map[string]float64{}}}
	r.RecordExpiries("baseline", at(0), at(1000), closed.Requests())
	want := map[string]float64{
		"baseline_rz_sts_001_responses": 2,
		"baseline_rz_sts_003_responses": 1,
		"baseline_rz_sts_004_responses": 1,
	}
	got := r.Record().Breakdowns
	for k, v := range want {
		if got[k] != v {
			t.Errorf("breakdown %s = %v, want %v (breakdowns %v)", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("breakdowns = %v, want only %v", got, want)
	}
}

// TestRecoveryCountsErrorRepliesAfterTheBreakersClosed: once every Node's
// breaker closed after a fault, the recovery's criterion counts the
// failed requests and the State Store's error replies, while the expiries
// and the skips they leave, which a connection the fault no longer
// touches only shows on a slow runner, are recorded as breakdowns.
func TestRecoveryCountsErrorRepliesAfterTheBreakersClosed(t *testing.T) {
	s := &Sampler{keep: DefaultKeep(), ids: map[string]int{}, samples: map[string][]Sample{}}
	expo := func(v int) promtext.Set {
		text := "# TYPE " + DegradedMetric + " gauge\n" + DegradedMetric + `{reason="` + BreakerReason + `"} ` + strconv.Itoa(v) + "\n"
		set, err := promtext.ParseBytes([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return set
	}
	// node-1's breaker closes last: its sample answered at 2,004 ms is the
	// first to show it closed for good.
	s.record("node-1", at(0), at(0), at(2), expo(1), nil)
	s.record("node-1", at(1000), at(1000), at(1002), expo(1), nil)
	s.record("node-1", at(2000), at(2000), at(2004), expo(0), nil)
	s.record("node-2", at(0), at(0), at(3), expo(1), nil)
	s.record("node-2", at(1000), at(1000), at(1003), expo(0), nil)
	s.record("node-2", at(2000), at(2000), at(2003), expo(0), nil)

	closed := newClosedStream(
		closedRequest(1500, CodeSTSError), // before the breakers closed
		closedRequest(3000, CodeSTSTimeout),
		closedRequest(3100, CodeSTSSkipped),
		closedRequest(3200, CodeSTSError),
		closedRequest(3300, ""),
	)
	plain := &Stream{Spec: StreamSpec{Name: "plain"}, requests: []Request{
		{Intended: at(3400), Sent: at(3400), Done: at(3401), Outcome: loadgen.Reset},
	}}
	l := &Load{Streams: []*Stream{closed, plain}}
	r := &Run{T: t, Sampler: s, rec: Record{Breakdowns: map[string]float64{}}}
	base := Phase{Name: PhaseBaseline, Start: at(-60_000), End: at(0)}
	rec := Phase{Name: PhaseRecovery, Start: at(0), End: at(60_000)}
	r.recoveryCriteria([]string{"node-1", "node-2"}, l, base, rec, at(500))

	byName := map[string]Criterion{}
	for _, c := range r.Record().Criteria {
		byName[c.Name] = c
	}
	if c, ok := byName["breakers closed within the recovery"]; !ok || c.Value != 1 {
		t.Fatalf("breakers closed within the recovery = %+v, %v; want it held", c, ok)
	}
	if c := byName["recovery errors after the breakers closed"]; c.Value != 2 {
		t.Errorf("recovery errors after the breakers closed = %v, want the error reply and the reset, 2", c.Value)
	}
	if c := byName["degraded reasons left after the recovery"]; c.Value != 0 {
		t.Errorf("degraded reasons left after the recovery = %v, want 0", c.Value)
	}
	got := r.Record().Breakdowns
	for k, v := range map[string]float64{
		"recovery_rz_sts_001_responses": 1,
		"recovery_rz_sts_003_responses": 0,
		"recovery_rz_sts_004_responses": 1,
	} {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("breakdown %s = %v, %v; want %v", k, g, ok, v)
		}
	}
	if g := got["breakers_closed_after_fault_s"]; math.Abs(g-1.504) > 1e-9 {
		t.Errorf("breakers_closed_after_fault_s = %v, want 1.504", g)
	}
}

// TestConsumptiveCallsCountTheMainConnectionsErrorReplies: the main
// connection's error replies are those of the consumptive calls, gcra,
// quota and script_multi, on every Node; the cache connection's ops, such
// as cache_set and pipeline, and the expiries and skips of any op stay
// out of them.
func TestConsumptiveCallsCountTheMainConnectionsErrorReplies(t *testing.T) {
	s := &Sampler{keep: DefaultKeep(), ids: map[string]int{}, samples: map[string][]Sample{}}
	expo := func(series map[string]int) promtext.Set {
		var b strings.Builder
		b.WriteString("# TYPE ruralz_state_calls_total counter\n")
		for labels, v := range series {
			b.WriteString("ruralz_state_calls_total{" + labels + "} " + strconv.Itoa(v) + "\n")
		}
		set, err := promtext.ParseBytes([]byte(b.String()))
		if err != nil {
			t.Fatal(err)
		}
		return set
	}
	s.record("node-1", at(0), at(0), at(1), expo(map[string]int{
		`op="gcra",result="error"`: 1, `op="gcra",result="timeout"`: 0, `op="quota",result="error"`: 0,
		`op="script_multi",result="error"`: 0, `op="cache_set",result="error"`: 10, `op="pipeline",result="error"`: 3,
	}), nil)
	s.record("node-1", at(1000), at(1000), at(1001), expo(map[string]int{
		`op="gcra",result="error"`: 3, `op="gcra",result="timeout"`: 5, `op="quota",result="error"`: 1,
		`op="script_multi",result="error"`: 1, `op="cache_set",result="error"`: 110, `op="pipeline",result="error"`: 53,
	}), nil)
	s.record("node-2", at(0), at(0), at(1), expo(map[string]int{`op="gcra",result="timeout"`: 0, `op="quota",result="skipped"`: 0}), nil)
	s.record("node-2", at(1000), at(1000), at(1001), expo(map[string]int{`op="gcra",result="timeout"`: 3, `op="quota",result="skipped"`: 2}), nil)
	r := &Run{Sampler: s}
	for _, c := range []struct {
		result string
		want   float64
	}{
		{"error", 4},
		{"timeout", 8},
		{"skipped", 2},
	} {
		if got := r.ConsumptiveCalls(c.result, at(0), at(1000)); got != c.want {
			t.Errorf("ConsumptiveCalls(%q) = %v, want %v", c.result, got, c.want)
		}
	}
}
