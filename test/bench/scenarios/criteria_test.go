// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/test/bench/gen"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// t0 is a fixed sample time.
var t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// metricsOf parses exposition text into a sample set.
func metricsOf(t *testing.T, text string) promtext.Set {
	t.Helper()
	s, err := promtext.ParseBytes([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// at returns a good Node sample at second sec holding text.
func at(t *testing.T, sec int, text string) harness.NodeSample {
	t.Helper()
	return harness.NodeSample{At: t0.Add(time.Duration(sec) * time.Second), Metrics: metricsOf(t, text)}
}

func verdict(t *testing.T, r benchrecord.BudgetResult, want string) {
	t.Helper()
	if r.Verdict != want {
		t.Errorf("%s = %s (%s), want %s", r.ID, r.Verdict, r.Result, want)
	}
}

// TestPlateau covers a gauge's plateau: reaching its ceiling within 1%
// passes, staying below or passing it fails, and no sample fails.
func TestPlateau(t *testing.T) {
	g := func(v string) string {
		return "# TYPE ruralz_state_inflight_calls gauge\nruralz_state_inflight_calls " + v + "\n"
	}
	for _, c := range []struct {
		name string
		vals []string
		want string
	}{
		{"at the ceiling", []string{"100", "8192", "8150"}, benchrecord.BudgetPass},
		{"within 1%", []string{"8120"}, benchrecord.BudgetPass},
		{"short of it", []string{"4000", "7000"}, benchrecord.BudgetFail},
		{"past it", []string{"8192", "8193"}, benchrecord.BudgetFail},
		{"no sample", nil, benchrecord.BudgetFail},
	} {
		var samples []harness.NodeSample
		for i, v := range c.vals {
			samples = append(samples, at(t, i, g(v)))
		}
		r := plateau("p", "v", samples, metricStateInflight, nil, StateInFlight)
		if r.Verdict != c.want {
			t.Errorf("%s: plateau = %s (%s), want %s", c.name, r.Verdict, r.Result, c.want)
		}
	}
	bad := harness.NodeSample{Err: errors.New("scrape failed")}
	verdict(t, plateau("p", "v", []harness.NodeSample{bad}, metricStateInflight, nil, StateInFlight), benchrecord.BudgetFail)
}

// TestAccounting covers O1's accounting: within 0.5% passes, beyond fails,
// and no offered request fails.
func TestAccounting(t *testing.T) {
	verdict(t, accounting("a", "v", &lab.LoadResult{Offered: 1000, Achieved: 996, RateError: 0.004}), benchrecord.BudgetPass)
	verdict(t, accounting("a", "v", &lab.LoadResult{Offered: 1000, Achieved: 990, RateError: 0.01}), benchrecord.BudgetFail)
	verdict(t, accounting("a", "v", nil), benchrecord.BudgetFail)
	verdict(t, accounting("a", "v", &lab.LoadResult{}), benchrecord.BudgetFail)
}

// TestNoCode covers a code the load must never see.
func TestNoCode(t *testing.T) {
	verdict(t, noCode("n", "v", &lab.LoadResult{Codes: map[string]int64{codeFull: 9}}, codeUpstream), benchrecord.BudgetPass)
	verdict(t, noCode("n", "v", &lab.LoadResult{Codes: map[string]int64{codeUpstream: 1}}, codeUpstream), benchrecord.BudgetFail)
}

// TestGoroutineBound covers O2's goroutine rule, judged on its tightest
// sample: one goroutine per open client connection and one more per
// request in flight, net/http's background read of an HTTP/1.1
// connection whose handler runs, which a functional O2 run's 8,192
// requests waiting on the State Store doubled past a bound of one per
// connection.
func TestGoroutineBound(t *testing.T) {
	text := func(g, client, active, pool string) string {
		return "# TYPE ruralz_runtime_goroutines gauge\nruralz_runtime_goroutines " + g +
			"\n# TYPE ruralz_listener_open_connections gauge\nruralz_listener_open_connections{listener=\"http\",protocol=\"http1\"} " + client +
			"\n# TYPE ruralz_http_active_requests gauge\nruralz_http_active_requests{listener=\"http\"} " + active +
			"\n# TYPE ruralz_upstream_pool_connections gauge\nruralz_upstream_pool_connections{upstream=\"mock\",state=\"idle\"} " + pool + "\n"
	}
	// Bound: 12,000 + 8,200 + 2 x 100 + 8,192 + 1,000 = 29,592.
	ok := []harness.NodeSample{at(t, 0, text("5000", "12000", "0", "100")), at(t, 1, text("29591", "12000", "8200", "100"))}
	verdict(t, goroutineBound("g", "v", ok), benchrecord.BudgetPass)
	over := append(ok, at(t, 2, text("29592", "12000", "8200", "100")))
	verdict(t, goroutineBound("g", "v", over), benchrecord.BudgetFail)
	// Without requests in flight the bound is the connections' alone.
	idle := []harness.NodeSample{at(t, 0, text("21392", "12000", "0", "100"))}
	verdict(t, goroutineBound("g", "v", idle), benchrecord.BudgetFail)
	verdict(t, goroutineBound("g", "v", nil), benchrecord.BudgetFail)
}

// TestInterpolate covers G1's interpolation between the step below a level
// and the first at or above it.
func TestInterpolate(t *testing.T) {
	rates := []float64{50000, 55000, 60000}
	if r, ok := interpolate(rates, []float64{0.5, 0.6, 0.8}, 0.7); !ok || math.Abs(r-57500) > 1e-6 {
		t.Errorf("interpolate = %v, %v, want 57500", r, ok)
	}
	if r, ok := interpolate(rates, []float64{0.5, 0.7, 0.8}, 0.7); !ok || r != 55000 {
		t.Errorf("interpolate on a step = %v, %v, want 55000", r, ok)
	}
	if _, ok := interpolate(rates, []float64{0.75, 0.8, 0.9}, 0.7); ok {
		t.Error("interpolate with the first step past the level reported a figure")
	}
	if _, ok := interpolate(rates, []float64{0.1, 0.2, 0.3}, 0.7); ok {
		t.Error("interpolate with no step reaching the level reported a figure")
	}
}

// TestQuantileBound covers a Node histogram's quantile over the samples.
func TestQuantileBound(t *testing.T) {
	h := func(le1, inf string) string {
		return "# TYPE ruralz_listener_tls_handshake_duration_seconds histogram\n" +
			"ruralz_listener_tls_handshake_duration_seconds_bucket{listener=\"https\",le=\"0.5\"} " + le1 + "\n" +
			"ruralz_listener_tls_handshake_duration_seconds_bucket{listener=\"https\",le=\"1\"} " + inf + "\n" +
			"ruralz_listener_tls_handshake_duration_seconds_bucket{listener=\"https\",le=\"+Inf\"} " + inf + "\n" +
			"ruralz_listener_tls_handshake_duration_seconds_sum{listener=\"https\"} 1\n" +
			"ruralz_listener_tls_handshake_duration_seconds_count{listener=\"https\"} " + inf + "\n"
	}
	in := []harness.NodeSample{at(t, 0, h("0", "0")), at(t, 1, h("100", "100"))}
	verdict(t, quantileBound("q", "v", in, metricHandshake, promtext.L("listener", "https"), 0.99, time.Second), benchrecord.BudgetPass)
	slow := []harness.NodeSample{at(t, 0, h("0", "0")), at(t, 1, h("0", "100"))}
	verdict(t, quantileBound("q", "v", slow, metricHandshake, promtext.L("listener", "https"), 0.99, 500*time.Millisecond), benchrecord.BudgetFail)
	verdict(t, quantileBound("q", "v", in[:1], metricHandshake, nil, 0.99, time.Second), benchrecord.BudgetFail)
}

// TestGoodput covers PB-11: O1b's 2xx per second against 90% of S3's
// saturation, every other answer RZ-RT-005.
func TestGoodput(t *testing.T) {
	ok := &lab.LoadResult{Statuses: map[int]int64{200: 26000 * 10, 503: 16000 * 10}, Codes: map[string]int64{codeFull: 16000 * 10},
		Outcomes: map[string]int64{loadgen.Success.String(): 260000, loadgen.ServerError.String(): 160000}}
	verdict(t, goodput(ok, 10*time.Second), benchrecord.BudgetPass)
	low := &lab.LoadResult{Statuses: map[int]int64{200: 20000 * 10}, Outcomes: map[string]int64{loadgen.Success.String(): 200000}}
	verdict(t, goodput(low, 10*time.Second), benchrecord.BudgetFail)
	other := &lab.LoadResult{Statuses: map[int]int64{200: 26000 * 10, 502: 5}, Outcomes: map[string]int64{loadgen.Success.String(): 260000}}
	verdict(t, goodput(other, 10*time.Second), benchrecord.BudgetFail)
	verdict(t, goodput(nil, time.Second), benchrecord.BudgetFail)
}

// TestHeld covers a conns service holding every connection.
func TestHeld(t *testing.T) {
	verdict(t, held("h", "v", &lab.ConnsStats{Opened: 10, Open: 10}, 10), benchrecord.BudgetPass)
	for name, st := range map[string]*lab.ConnsStats{
		"failed":    {Opened: 9, Open: 9, Failed: 1, Errors: []string{"refused"}},
		"closed":    {Opened: 10, Open: 9, ClosedByPeer: 1},
		"not yet":   {Opened: 5, Open: 5},
		"no counts": nil,
	} {
		if r := held("h", "v", st, 10); r.Verdict != benchrecord.BudgetFail {
			t.Errorf("%s: held = %s, want fail", name, r.Verdict)
		}
	}
	if r := held("h", "v", &lab.ConnsStats{Opened: 9, Open: 9, Failed: 1, Errors: []string{"refused"}}, 10); !strings.Contains(r.Result, "refused") {
		t.Errorf("held result %q names no failure", r.Result)
	}
}

// TestPerConnection covers the connection memory rows.
func TestPerConnection(t *testing.T) {
	verdict(t, perConnection("m", "v", 100<<20, 100<<20+20000*(20<<10), 20000, IdleCleartextBytes), benchrecord.BudgetPass)
	verdict(t, perConnection("m", "v", 100<<20, 100<<20+20000*(30<<10), 20000, IdleCleartextBytes), benchrecord.BudgetFail)
	verdict(t, perConnection("m", "v", 0, 1, 1, IdleCleartextBytes), benchrecord.BudgetFail)
}

// TestListenerHeld covers the Node's open connections on a listener.
func TestListenerHeld(t *testing.T) {
	s := at(t, 0, "# TYPE ruralz_listener_open_connections gauge\n"+
		"ruralz_listener_open_connections{listener=\"https\",protocol=\"h2\"} 1990\n"+
		"ruralz_listener_open_connections{listener=\"https\",protocol=\"http1\"} 10\n"+
		"ruralz_listener_open_connections{listener=\"http\",protocol=\"http1\"} 256\n")
	verdict(t, listenerHeld("l", "v", s, listenerHTTPS, 2000), benchrecord.BudgetPass)
	verdict(t, listenerHeld("l", "v", s, listenerHTTPS, 2001), benchrecord.BudgetFail)
	verdict(t, listenerHeld("l", "v", harness.NodeSample{Err: io.EOF}, listenerHTTPS, 1), benchrecord.BudgetFail)
}

// TestWindow covers an expiry window: every expiry between its value and
// its value plus 2 s.
func TestWindow(t *testing.T) {
	hist := func(ds ...time.Duration) string {
		h, err := loadgen.NewHistogram(loadgen.DefaultLowest, loadgen.DefaultHighest, loadgen.DefaultDigits)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range ds {
			h.Record(d)
		}
		return lab.EncodeHistogram(h)
	}
	verdict(t, window("w", "v", hist(5*time.Second, 6900*time.Millisecond), 2, PreRoutingDeadline), benchrecord.BudgetPass)
	verdict(t, window("w", "v", hist(4*time.Second), 1, PreRoutingDeadline), benchrecord.BudgetFail)
	verdict(t, window("w", "v", hist(7200*time.Millisecond), 1, PreRoutingDeadline), benchrecord.BudgetFail)
	verdict(t, window("w", "v", hist(), 0, PreRoutingDeadline), benchrecord.BudgetFail)
	verdict(t, window("w", "v", "not base64!", 1, PreRoutingDeadline), benchrecord.BudgetFail)
}

// TestPB10 covers PB-10: telemetry CPU, on minus off plus metric
// recording, at most 5% of the telemetry-on CPU per request.
func TestPB10(t *testing.T) {
	verdict(t, pb10(100*time.Microsecond, 97*time.Microsecond, time.Microsecond, nil, nil), benchrecord.BudgetPass)
	verdict(t, pb10(100*time.Microsecond, 94*time.Microsecond, time.Microsecond, nil, nil), benchrecord.BudgetFail)
	r := pb10(0, 97*time.Microsecond, time.Microsecond, errors.New("no S2 comparison tonight"), nil)
	if r.Verdict != benchrecord.BudgetFail || !strings.Contains(r.Result, "no S2 comparison") {
		t.Errorf("pb10 without S2 = %+v", r)
	}
	verdict(t, pb10(100*time.Microsecond, 0, time.Microsecond, nil, nil), benchrecord.BudgetFail)
}

// TestG1Rate covers G1's figure: interpolated at 70% script CPU, a fail
// when the first step is past it or no step reaches it.
func TestG1Rate(t *testing.T) {
	steps := []g1Step{{achieved: 50000, cpu: 0.5}, {achieved: 55000, cpu: 0.6}, {achieved: 60000, cpu: 0.8, p99: 900 * time.Microsecond}}
	r := g1Rate(steps)
	if r.Verdict != benchrecord.BudgetPass || !strings.Contains(r.Result, "57500") {
		t.Errorf("g1Rate = %+v, want 57500 calls/s", r)
	}
	verdict(t, g1Rate([]g1Step{{achieved: 50000, cpu: 0.75}}), benchrecord.BudgetFail)
	verdict(t, g1Rate([]g1Step{{achieved: 50000, cpu: 0.3, p99: 2 * time.Millisecond}}), benchrecord.BudgetFail)
}

// TestValkeyWindow covers the script CPU and EVALSHA rate between the INFO
// samples of a step and the connection count less the INFO connection.
func TestValkeyWindow(t *testing.T) {
	infos := []harness.ValkeyInfo{
		{At: t0, MainThreadCPU: 10, EvalshaMicros: 1e6, ConnectedClients: 3},
		{At: t0.Add(2 * time.Second), MainThreadCPU: 11.4, EvalshaMicros: 2e6, ConnectedClients: 11},
		{At: t0.Add(4 * time.Second), MainThreadCPU: 12.8, EvalshaMicros: 3e6, ConnectedClients: 11},
	}
	cpu, ev, clients := valkeyWindow(infos, t0, t0.Add(4*time.Second))
	if math.Abs(cpu-0.7) > 1e-9 || math.Abs(ev-5e5) > 1e-6 || clients != 10 {
		t.Errorf("valkeyWindow = %v, %v, %v, want 0.7, 500000, 10", cpu, ev, clients)
	}
	if cpu, ev, _ := valkeyWindow(infos[:1], t0, t0); cpu != 0 || ev != 0 {
		t.Errorf("one sample = %v, %v, want 0", cpu, ev)
	}
}

// TestShardConnections covers 2 plus 8 connections per emulated Node.
func TestShardConnections(t *testing.T) {
	verdict(t, shardConnections("c", "v", 10, 1), benchrecord.BudgetPass)
	verdict(t, shardConnections("c", "v", 11, 1), benchrecord.BudgetFail)
	verdict(t, shardConnections("c", "v", 10000, 1000), benchrecord.BudgetPass)
	verdict(t, shardConnections("c", "v", 0, 1), benchrecord.BudgetFail)
}

// TestRingFits covers the balancer budget arithmetic: 1,000 rings of 64
// Endpoints fit at 64 or more virtual nodes, 4,000 do not.
func TestRingFits(t *testing.T) {
	if !RingFits(1000, 64) {
		t.Error("RingFits(1000, 64) = false, want true")
	}
	if RingFits(4000, 64) {
		t.Error("RingFits(4000, 64) = true, want false")
	}
}

// TestRingCriteria covers a rung's criteria: within the budget with every
// ring built, or with fallbacks and balancer_budget past it.
func TestRingCriteria(t *testing.T) {
	check := func(rs []benchrecord.BudgetResult, want string) {
		t.Helper()
		for _, r := range rs {
			if r.Verdict != want {
				t.Errorf("%s = %s (%s), want %s", r.ID, r.Verdict, r.Result, want)
			}
		}
	}
	check(ringCriteria(1000, 64, ringState{bytes: 238 << 20, rings: 1000, minVNodes: 244, maxVNodes: 244}, false, nil), benchrecord.BudgetPass)
	check(ringCriteria(4000, 64, ringState{bytes: 250 << 20, rings: 4000, fallback: 100, minVNodes: 64, maxVNodes: 64}, true, nil), benchrecord.BudgetPass)
	check(ringCriteria(4000, 64, ringState{bytes: 250 << 20, rings: 4000, fallback: 100}, false, nil)[2:], benchrecord.BudgetFail)
	check(ringCriteria(1000, 64, ringState{bytes: 300 << 20, rings: 1000, minVNodes: 244, maxVNodes: 244}, false, nil)[:1], benchrecord.BudgetFail)
	check(ringCriteria(1000, 64, ringState{}, false, io.ErrUnexpectedEOF), benchrecord.BudgetFail)
}

// TestPausedBody covers the body flood's bodies: a JSON object of the
// given size whose read blocks at its pause point until released.
func TestPausedBody(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	b := &pausedBody{size: 64, pauseAt: 40, reached: func() { reached <- struct{}{} }, release: release}
	got := make([]byte, 0, 64)
	buf := make([]byte, 16)
	for len(got) < 40 {
		n, err := b.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, buf[:n]...)
	}
	if len(got) != 40 {
		t.Fatalf("read %d bytes before the pause, want 40", len(got))
	}
	select {
	case <-reached:
		t.Fatal("reached before the read that blocks")
	default:
	}
	done := make(chan []byte)
	go func() {
		rest, err := io.ReadAll(b)
		if err != nil {
			t.Error(err)
		}
		done <- rest
	}()
	<-reached
	close(release)
	got = append(got, <-done...)
	if want := `{"a":"` + strings.Repeat("a", 64-8) + `"}`; string(got) != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	b.reach() // a second reach is a no-op
}

// TestProblemCode covers reading a problem document's code.
func TestProblemCode(t *testing.T) {
	if got := problemCode([]byte(`{"type":"about:blank","code":"RZ-RT-004","status":503}`)); got != codeBufferFull {
		t.Errorf("problemCode = %q", got)
	}
	for _, b := range []string{"plain", `{"code":"RZ`, ""} {
		if got := problemCode([]byte(b)); got != "" {
			t.Errorf("problemCode(%q) = %q, want none", b, got)
		}
	}
}

// TestFailedRequests covers counting the measured phase's requests without
// a 2xx answer.
func TestFailedRequests(t *testing.T) {
	r := &lab.LoadResult{Offered: 10, Statuses: map[int]int64{200: 7, 204: 1, 503: 1}}
	if got := failedRequests(r); got != 2 {
		t.Errorf("failedRequests = %d, want 2 (one 503, one unanswered)", got)
	}
	if failedRequests(nil) != 0 || offered(nil) != 0 {
		t.Error("a nil load counts requests")
	}
}

// TestLeastSquares covers the RSS slope fit.
func TestLeastSquares(t *testing.T) {
	x := []float64{1000, 5000, 10000, 17000}
	y := make([]float64, len(x))
	for i := range x {
		y[i] = 95e6 + 1900*x[i]
	}
	if s := leastSquares(x, y); math.Abs(s-1900) > 1e-6 {
		t.Errorf("leastSquares = %v, want 1900", s)
	}
	verdict(t, r1Slope(x, y), benchrecord.BudgetPass)
	for i := range y {
		y[i] = 95e6 + 2100*x[i]
	}
	verdict(t, r1Slope(x, y), benchrecord.BudgetFail)
	verdict(t, r1Slope(x[:1], y[:1]), benchrecord.BudgetSkipped)
}

// TestR1Table checks the size-class table against the generator's ladder
// and PB-7, the 5,000-Route row's Hot Reload.
func TestR1Table(t *testing.T) {
	var sizes []int
	for _, r := range R1Table() {
		sizes = append(sizes, r.Routes)
	}
	want := gen.Sizes()
	if len(sizes) != len(want) {
		t.Fatalf("R1Table sizes %v, want %v", sizes, want)
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Errorf("R1Table size %d = %d, want %d", i, sizes[i], want[i])
		}
	}
	for _, r := range R1Table() {
		if r.Routes == R1StageRoutes && r.Reload != 500*time.Millisecond {
			t.Errorf("5,000 Routes reload %v, want PB-7's 500ms", r.Reload)
		}
		if r.Peak != int64(RetiredSnapshots+2)*r.Heap {
			t.Errorf("%d Routes: peak %d, want (K + 2) x snapshot %d", r.Routes, r.Peak, (RetiredSnapshots+2)*r.Heap)
		}
	}
}

// TestR1Criteria covers a size's criteria: the any-hardware ones judged at
// every scale, the timings and memory skipped at the functional scale and
// judged at the full one, PB-7 and the stages at 5,000 Routes only.
func TestR1Criteria(t *testing.T) {
	idle := r1Idle{
		cold: time.Second, lkg: 500 * time.Millisecond, lkgRev: "rev-1", lastActivated: "rev-1",
		heap: 2_000_000, reloads: 5, reloadMax: 400 * time.Millisecond, peakMax: 8_000_000, rssIdle: 105_000_000,
		stages: map[string]time.Duration{"verify": 5 * time.Millisecond, "compile": 300 * time.Millisecond, "swap": 5 * time.Millisecond},
	}
	o := &harness.RunOutcome{Load: &lab.LoadResult{Offered: 100, Statuses: map[int]int64{200: 100}}}
	ids := map[string]string{}
	for _, r := range r1Criteria(Full(), R1StageRoutes, idle, r1Loaded{reloads: 3, reloadMax: 600 * time.Millisecond}, o) {
		ids[r.ID] = r.Verdict
	}
	for _, id := range []string{"R1-activations", "R1-lkg-revision", "R1-loaded-failures", "R1-cold-start", "R1-lkg-start", "PB-7",
		"R1-snapshot-heap", "R1-peak-config", "PB-6", "R1-stage-verify", "R1-stage-compile", "R1-stage-plugin-compile", "R1-stage-swap", "R1-loaded-reload"} {
		if ids[id] != benchrecord.BudgetPass {
			t.Errorf("%s = %q, want pass", id, ids[id])
		}
	}
	if ids["R1-loaded-p99"] != benchrecord.BudgetFail {
		t.Errorf("R1-loaded-p99 without an internal view = %q, want fail", ids["R1-loaded-p99"])
	}
	for _, r := range r1Criteria(Functional(), 1000, idle, r1Loaded{}, o) {
		switch r.ID {
		case "R1-activations", "R1-lkg-revision", "R1-loaded-failures":
			verdict(t, r, benchrecord.BudgetPass)
		default:
			verdict(t, r, benchrecord.BudgetSkipped)
		}
	}
	bad := idle
	bad.lkgRev = "rev-0"
	for _, r := range r1Criteria(Functional(), 100, bad, r1Loaded{err: errors.New("no activation")}, o) {
		if (r.ID == "R1-lkg-revision" || r.ID == "R1-activations") && r.Verdict != benchrecord.BudgetFail {
			t.Errorf("%s = %s, want fail", r.ID, r.Verdict)
		}
	}
}

// TestScales checks the scales against the documents: the full scale's
// targets, and the reduced scales keeping the fixed ceilings while each
// reaches them.
func TestScales(t *testing.T) {
	f := Full()
	if f.O1.Connections*f.O1.Streams != 30000 || f.O1.ARate != 30000 || f.O1.ADelay != time.Second || f.O1.BRate != 1.5*S3Saturation {
		t.Errorf("full O1 = %+v", f.O1)
	}
	if f.O2.Connections != 12000 || f.O2.Rate != 24000 || f.O2.Delay != 500*time.Millisecond {
		t.Errorf("full O2 = %+v", f.O2)
	}
	if f.C1.Connections != ClientConnections || f.C2.Burst != 10000 || f.C2.Within != 2*time.Second || f.C2.Rate != S3Saturation/2 {
		t.Errorf("full C1 %+v, C2 %+v", f.C1, f.C2)
	}
	if f.G1.From != 50000 || f.G1.Step != 5000 || f.G1.StepDuration != time.Minute || f.G1.Nodes != 1000 {
		t.Errorf("full G1 = %+v", f.G1)
	}
	if f.R1.LoadedRate != 25000 || f.R1.IdleSettle != 120*time.Second || len(f.R1.Sizes) != len(gen.Sizes()) {
		t.Errorf("full R1 = %+v", f.R1)
	}
	if f.Mem.Container != 2560<<20 || f.Constants.ReloadRate != 10 || f.Constants.QuotaKeys != 100000 || f.Constants.FloodConnections != ClientConnections {
		t.Errorf("full Mem %+v, constants %+v", f.Mem, f.Constants)
	}
	for _, s := range []Scale{Full(), Functional()} {
		c := s.Constants
		if c.PreRoutingClients <= PreRoutingWrites {
			t.Errorf("%s: %d pre-routing clients do not pass the 2,000 writes", s.Name, c.PreRoutingClients)
		}
		if int64(c.Bodies)*c.BodyBytes <= BufferedBytes || c.BodyBytes > 10<<20 {
			t.Errorf("%s: %d bodies of %d bytes do not pass 512 MiB within maxRequestBodyBytes", s.Name, c.Bodies, c.BodyBytes)
		}
		if c.TraceRate/2 <= 1000 {
			t.Errorf("%s: trace rate %v leaves a half below the root cap", s.Name, c.TraceRate)
		}
		if float64(s.O1.Connections*s.O1.Streams) < InFlightUnits || s.O1.ARate*s.O1.ADelay.Seconds() <= InFlightUnits {
			t.Errorf("%s: O1 %+v cannot fill 20,000 in-flight units", s.Name, s.O1)
		}
		if s.O2.Rate*s.O2.Delay.Seconds() <= StateInFlight {
			t.Errorf("%s: O2 %+v cannot fill 8,192 calls in flight", s.Name, s.O2)
		}
		if !RingFits(c.RingSizes[0], c.RingEndpoints) || RingFits(c.RingSizes[len(c.RingSizes)-1], c.RingEndpoints) {
			t.Errorf("%s: ring ladder %v of %d Endpoints does not cross the balancer budget", s.Name, c.RingSizes, c.RingEndpoints)
		}
	}
	sm := Smoke()
	if sm.Duration != 5*time.Second || sm.O1.ARate != 100 || sm.R1.LoadedRate != 100 || sm.G1.From != 100 || !sm.Functional {
		t.Errorf("smoke = %+v", sm)
	}
}

// TestHeldUpstream covers the upstream connections O1a holds, which its
// open-file check needs: the in-flight ceiling at the full and functional
// scales, whose rate times delay passes it, and none in the smoke run,
// which delays nothing; a check that asked every scale for 20,000 failed
// the smoke run on a host allowing 20,000 open files.
func TestHeldUpstream(t *testing.T) {
	for _, c := range []struct {
		s    Scale
		want int
	}{{Full(), InFlightUnits}, {Functional(), InFlightUnits}, {Smoke(), 0}} {
		if got := heldUpstream(c.s.O1); got != c.want {
			t.Errorf("heldUpstream(%s O1) = %d, want %d", c.s.Name, got, c.want)
		}
	}
	if got := heldUpstream(O1Scale{ARate: 300, ADelay: 2 * time.Second}); got != 600 {
		t.Errorf("heldUpstream(300 rps held 2 s) = %d, want 600", got)
	}
}

// TestO2Keys checks the keys O2 cycles: S5's 100,000 at the full scale,
// and at the functional scale the first-seen budget's 200, which the run
// primes before its delay so every request calls GCRA; the catalog's S5
// keeps its own.
func TestO2Keys(t *testing.T) {
	for _, c := range []struct {
		s    Scale
		want int
	}{{Full(), harness.BenchKeyCardinality}, {Functional(), FirstSeenBurst}} {
		w, err := o2Workload(c.s.O2)
		if err != nil {
			t.Fatal(err)
		}
		if w.Keys == nil || w.Keys.N != c.want {
			t.Errorf("%s O2 keys = %+v, want %d", c.s.Name, w.Keys, c.want)
		}
	}
	s5, err := catalog("S5")
	if err != nil {
		t.Fatal(err)
	}
	if s5.Workload.Keys.N != harness.BenchKeyCardinality {
		t.Errorf("O2's key count leaked into the catalog's S5: %d keys", s5.Workload.Keys.N)
	}
}

// TestLabsMatchProfiles checks the generator's labs against the profiles:
// the loopback lab's addresses and CA under the loopback profile's fixed
// ports and secret root, the named lab's CA under the RH secret root.
func TestLabsMatchProfiles(t *testing.T) {
	l, sv := gen.LoopbackLab(), harness.LoopbackServices()
	if l.MockHost+":"+strconv.Itoa(l.MockPorts[0]) != sv.Mock || !strings.Contains(l.JWKSURL, sv.IdP) || !strings.Contains(l.OTLPEndpoint, sv.Sink) {
		t.Errorf("loopback lab %+v does not match the loopback services %+v", l, sv)
	}
	if !strings.HasPrefix(l.OTLPCA, harness.LoopbackTLSDir+"/") || !strings.HasPrefix(gen.NamedLab().OTLPCA, RHSecretRoot+"/") {
		t.Errorf("labs' CA files %s and %s outside the profiles' secret roots", l.OTLPCA, gen.NamedLab().OTLPCA)
	}
	if labOf(harness.Loopback(t.TempDir())).MockHost != l.MockHost || labOf(&harness.Profile{Name: harness.ProfileRH1}).MockHost != gen.NamedLab().MockHost {
		t.Error("labOf picks the wrong lab")
	}
}
