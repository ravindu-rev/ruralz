// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// testAPIKeys are API keys of the chaos Consumers.
func testAPIKeys() map[string]string {
	keys := map[string]string{}
	for _, c := range Consumers() {
		keys[c] = "key-" + c
	}
	return keys
}

// TestPlanRemoteLoad: a load on the load host offers the streams a load
// on the harness's host offers: the Consumers' API keys in turn, a
// stream's own keys, HTTP/1.1 round robin and HTTP/2 spread, the https
// balancer verified against the harness's CA by the name its certificate
// carries, the warm-up's default, and a settling load's extra duration;
// Prepare, which runs on the harness's host, is refused.
func TestPlanRemoteLoad(t *testing.T) {
	keys, byKey, err := apiKeys(testAPIKeys())
	if err != nil {
		t.Fatal(err)
	}
	target := loadTarget{http: "10.0.0.1:40000", https: "10.0.0.1:40001", ca: []byte("ca-pem")}
	spec := LoadSpec{Duration: 3 * time.Minute, Settle: loadgen.BenchmarkSettle(), Streams: []StreamSpec{
		{Name: "limited", Path: PathLimited, Rate: 10, APIKeys: true, Counted: Decisions()},
		{Name: "flood", Path: PathFlood, Rate: 20, KeyHeader: KeyHeader, Keys: &lab.KeySpec{Kind: lab.KeysRotate, Prefix: "flood-", PerKey: 1}},
		{Name: "h2", Path: PathPlain, Rate: 5, HTTPS: true, Protocol: loadgen.H2, Connections: 4},
	}}
	ls, streams, err := planRemoteLoad(spec, target, keys, byKey)
	if err != nil {
		t.Fatal(err)
	}
	if ls.Warmup != DefaultWarmup || ls.Duration != 3*time.Minute+loadgen.BenchmarkSettle().Max-DefaultWarmup || ls.Settle == nil || ls.NoRecords {
		t.Errorf("load = warm-up %v, duration %v, settle %+v, no records %v", ls.Warmup, ls.Duration, ls.Settle, ls.NoRecords)
	}
	limited, flood, h2 := ls.Streams[0], ls.Streams[1], ls.Streams[2]
	if limited.URL != "http://10.0.0.1:40000/limited" || limited.Protocol != "http1" || !limited.RoundRobin || limited.KeyHeader != APIKeyField ||
		limited.Keys.Kind != lab.KeysList || !slices.Equal(limited.Keys.Values, keys) {
		t.Errorf("limited = %+v (keys %+v), want the Consumers' API keys in turn over round-robin HTTP/1.1", limited, limited.Keys)
	}
	if flood.KeyHeader != KeyHeader || flood.Keys.Kind != lab.KeysRotate || flood.Keys.Prefix != "flood-" {
		t.Errorf("flood = %+v", flood)
	}
	if h2.URL != "https://10.0.0.1:40001/plain" || h2.Protocol != "h2" || h2.RoundRobin || h2.CA != "ca-pem" || h2.ServerName != "localhost" || h2.Connections != 4 {
		t.Errorf("h2 = %+v, want HTTP/2 to the https balancer verified as localhost", h2)
	}
	if len(streams) != 3 || streams[0].keys.Key(5) != keys[1] || streams[1].keys.Key(7) != "flood-7" || streams[2].keys != nil {
		t.Errorf("the streams' key sources derive keys %v", streams)
	}
	for _, tc := range []struct {
		name string
		mod  func(*LoadSpec)
		want string
	}{
		{"prepare", func(s *LoadSpec) { s.Streams[0].Prepare = func(*http.Request, uint64) {} }, "prepares its requests"},
		{"no https balancer", func(*LoadSpec) {}, "needs the https balancer"},
		{"zero rate", func(s *LoadSpec) { s.Streams[0].Rate = 0 }, "has rate"},
		{"no duration", func(s *LoadSpec) { s.Duration = 0 }, "no duration"},
		{"keys and API keys", func(s *LoadSpec) { s.Streams[0].KeyHeader = KeyHeader }, "sets both"},
	} {
		s := spec
		s.Streams = slices.Clone(spec.Streams)
		tc.mod(&s)
		tg := target
		if tc.name == "no https balancer" {
			tg.https = ""
		}
		if _, _, err := planRemoteLoad(s, tg, keys, byKey); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: planRemoteLoad = %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
}

// TestEstimateOffset: the load host's clock offset comes from the
// exchange with the shortest round trip, the remote reading minus that
// round trip's midpoint, within half of it; an exchange that fails or
// answers without a clock fails the estimate.
func TestEstimateOffset(t *testing.T) {
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	// Three exchanges: round trips of 10 ms, 2 ms and 6 ms; the remote
	// clock runs 5 s ahead and answers at each midpoint.
	local := []time.Time{base, base.Add(10 * time.Millisecond), base.Add(time.Second), base.Add(time.Second + 2*time.Millisecond), base.Add(2 * time.Second), base.Add(2*time.Second + 6*time.Millisecond)}
	remote := []time.Time{base.Add(5*time.Second + 5*time.Millisecond), base.Add(6*time.Second + time.Millisecond + 400*time.Microsecond), base.Add(7*time.Second + 3*time.Millisecond)}
	i, j := 0, 0
	now := func() time.Time { i++; return local[i-1] }
	read := func() (time.Time, error) { j++; return remote[j-1], nil }
	offset, bound, err := estimateOffset(read, now, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := 5*time.Second + 400*time.Microsecond; offset != want || bound != time.Millisecond {
		t.Errorf("offset %v within %v, want %v within 1ms: the second exchange's", offset, bound, want)
	}
	failing := func() (time.Time, error) { return time.Time{}, errors.New("broken pipe") }
	if _, _, err := estimateOffset(failing, time.Now, 3); err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Errorf("estimateOffset with a failing exchange = %v", err)
	}
	blank := func() (time.Time, error) { return time.Time{}, nil }
	if _, _, err := estimateOffset(blank, time.Now, 3); err == nil || !strings.Contains(err.Error(), "without its clock") {
		t.Errorf("estimateOffset without a clock = %v", err)
	}
}

// serviceClient runs svc behind lab.Serve on pipes, as labhost runs it on
// the load host, and returns its client; the session ends with t.
func serviceClient(t *testing.T, svc lab.Service) *lab.Client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = lab.Serve(t.Context(), svc, inR, outW)
		_ = outW.Close()
	})
	c, err := lab.NewClient(inW, outR)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = inW.Close()
		_, _ = io.Copy(io.Discard, outR)
		wg.Wait()
	})
	return c
}

// testLoadSession returns a load session of an in-process load service
// and the address of an in-process mock Upstream its loads target.
func testLoadSession(t *testing.T, tb testing.TB) (*loadSession, string) {
	t.Helper()
	m, err := lab.StartMock(t.Context(), lab.MockConfig{Name: "backend-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	s, err := newLoadSession(tb, serviceClient(t, lab.NewLoadService()))
	if err != nil {
		t.Fatal(err)
	}
	return s, m.Ready().Addr
}

// TestRecordReturn is the record-return protocol of a run's loads on the
// load host: a load that ended keeps its records there while another
// load of the run runs, a measured window of the run, and they all return
// once the last load ended and was waited for: every request once, its
// key derived from its sequence number, an API key named by its Consumer,
// its times moved onto the harness's clock by the load host's offset, as
// its result's are; then the load host drops them.
func TestRecordReturn(t *testing.T) {
	s, addr := testLoadSession(t, t)
	s.offset = time.Hour // the load host's clock runs an hour ahead
	target := loadTarget{http: addr}
	a, err := s.startLoad(LoadSpec{Warmup: 50 * time.Millisecond, Duration: 250 * time.Millisecond, Streams: []StreamSpec{
		{Name: "limited", Path: PathLimited, Rate: 100, APIKeys: true},
		{Name: "undeclared", Path: PathUndeclared, Rate: 60, KeyHeader: KeyHeader, Keys: &lab.KeySpec{Kind: lab.KeysList, Values: []string{"u-1", "u-2"}}},
	}}, target, testAPIKeys(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.startLoad(LoadSpec{Warmup: time.Millisecond, Duration: time.Hour, Streams: []StreamSpec{
		{Name: "plain", Path: PathPlain, Rate: 5},
	}}, target, testAPIKeys(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Wait(); err != nil {
		t.Fatal(err)
	}
	if a.remote.recordsReturned() {
		t.Fatal("load a's records returned while load b runs")
	}
	if a.Streams[0].Result() == nil {
		t.Fatal("load a has no result once it ended")
	}
	b.Stop()
	if err := b.Wait(); err != nil {
		t.Fatal(err)
	}
	if !a.remote.recordsReturned() || !b.remote.recordsReturned() {
		t.Fatal("the records did not return once every load ended")
	}
	for _, st := range a.Streams {
		res := st.Result()
		reqs := st.Requests()
		if offered := res.Warmup.Offered + res.Measured.Offered; int64(len(reqs)) != offered || offered == 0 {
			t.Fatalf("%s: %d records, the result offered %d", st.Spec.Name, len(reqs), offered)
		}
		seen := map[uint64]bool{}
		for _, q := range reqs {
			if seen[q.Seq] || q.Status != http.StatusOK || q.Local == "" {
				t.Fatalf("%s: request %+v: want each request once, answered over a connection", st.Spec.Name, q)
			}
			seen[q.Seq] = true
			if q.Intended.Before(res.Start) || q.Intended.After(time.Now().Add(-50*time.Minute)) {
				t.Fatalf("%s: intended %v, want an hour back, on the harness's clock as the result's start %v", st.Spec.Name, q.Intended, res.Start)
			}
		}
		for _, q := range reqs[:5] {
			switch st.Spec.Name {
			case "limited":
				if want := Consumers()[q.Seq%4]; q.Key != want {
					t.Errorf("limited request %d keyed %q, want its Consumer %s", q.Seq, q.Key, want)
				}
			default:
				if want := []string{"u-1", "u-2"}[q.Seq%2]; q.Key != want {
					t.Errorf("undeclared request %d keyed %q, want %s", q.Seq, q.Key, want)
				}
			}
		}
	}
	if _, err := s.svc.Do(lab.Request{Op: lab.OpProgress, Name: a.remote.name}); err == nil {
		t.Error("the load host still holds load a after its records returned")
	}
}

// fatalTB records a Fatalf and ends the goroutine that called it.
type fatalTB struct {
	testing.TB
	mu    sync.Mutex
	fatal string
}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.mu.Lock()
	f.fatal = fmt.Sprintf(format, args...)
	f.mu.Unlock()
	runtime.Goexit()
}

// TestRecordsBeforeReturnFail: reading a load host stream's requests
// before they returned, during the run's load, fails the run instead of
// answering an empty or partial set.
func TestRecordsBeforeReturnFail(t *testing.T) {
	ft := &fatalTB{TB: t}
	s, addr := testLoadSession(t, ft)
	l, err := s.startLoad(LoadSpec{Warmup: time.Millisecond, Duration: time.Hour, Streams: []StreamSpec{
		{Name: "plain", Path: PathPlain, Rate: 5},
	}}, loadTarget{http: addr}, testAPIKeys(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() { _ = l.Streams[0].Requests() })
	wg.Wait()
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if !strings.Contains(ft.fatal, "stay on the load host until the run's load has ended") {
		t.Errorf("Requests before the return failed with %q", ft.fatal)
	}
	l.Stop()
	if err := l.wait(false); err != nil {
		t.Fatal(err)
	}
	if l.remote.recordsReturned() {
		t.Error("a wait without records returned them")
	}
}

// TestRunStep: a saturation step on the load host keeps no record, and
// its result, on the harness's clock, is all it returns before the load
// host drops it.
func TestRunStep(t *testing.T) {
	s, addr := testLoadSession(t, t)
	res, err := s.runStep(saturationStream(addr, 100), 20*time.Millisecond, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if res.Measured.Offered == 0 || res.Summary().ErrorRate != 0 {
		t.Errorf("step result = %+v", res.Summary())
	}
	if _, err := s.svc.Do(lab.Request{Op: lab.OpProgress, Name: "step-1"}); err == nil {
		t.Error("the load host still holds the step")
	}
}

// TestRunLadder: the ladder's steps run at their rates, the first alone
// after the warm-up, and the climb ends after the step Stop reports.
func TestRunLadder(t *testing.T) {
	var got []string
	run := func(rate float64, warmup, duration time.Duration) (*loadgen.Result, error) {
		got = append(got, fmt.Sprintf("%.0f/%v/%v", rate, warmup, duration))
		return &loadgen.Result{Rate: rate}, nil
	}
	l := loadgen.Ladder{
		Expected: 1000, From: 0.5, To: 1.1, Step: 0.25, StepDuration: 20 * time.Second, Warmup: 10 * time.Second,
		Stop: func(r *loadgen.Result) bool { return r.Rate >= 1000 },
	}
	results, err := runLadder(run, l)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"500/10s/20s", "750/0s/20s", "1000/0s/20s"}; !slices.Equal(got, want) || len(results) != 3 {
		t.Errorf("steps %q, want %q", got, want)
	}
	if _, err := runLadder(run, loadgen.Ladder{Expected: 1000, From: 0.5, To: 1, Step: 0.5}); err == nil {
		t.Error("a ladder without a step duration ran")
	}
}
