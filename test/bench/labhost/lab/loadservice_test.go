// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
)

// loadTarget starts an in-process mock Upstream and returns its URL.
func loadTarget(t *testing.T) string {
	t.Helper()
	m, err := StartMock(t.Context(), MockConfig{BodySize: 16, Name: "backend-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return "http://" + m.Ready().Addr + "/plain"
}

// waitEnded polls the load's progress until it ended.
func waitEnded(t *testing.T, c *Client, name string) *LoadState {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second) // a hang guard only
	defer cancel()
	for {
		r, err := c.Do(Request{Op: OpProgress, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if !r.LoadState.Running {
			return r.LoadState
		}
		select {
		case <-ctx.Done():
			t.Fatalf("load %s still runs: %+v", name, r.LoadState)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// fetchRecords returns every record of a stream, block by block.
func fetchRecords(t *testing.T, c *Client, name string, stream int) []Record {
	t.Helper()
	var d RecordDecoder
	var out []Record
	for b := 0; ; b++ {
		r, err := c.Do(Request{Op: OpRecords, Name: name, Stream: stream, Block: b})
		if err != nil {
			if b == 0 {
				t.Fatal(err)
			}
			t.Fatalf("block %d: %v", b, err)
		}
		if err := d.Decode(r.Records.Data, func(rec Record) { out = append(out, rec) }); err != nil {
			t.Fatal(err)
		}
		if b+1 >= r.Records.Blocks {
			return out
		}
	}
}

// TestLoadServiceKeepsAndReturnsRecords: the load service runs a load and
// answers its progress with its clock in every answer; it keeps every
// request's record, refuses to return any while another load of the
// session runs, which a measured window would be, and once none runs
// returns exactly the requests the result counts, each with its client
// address and whether it carried its key; the result decodes with its
// histograms, and an ended load is removed with its records.
func TestLoadServiceKeepsAndReturnsRecords(t *testing.T) {
	url := loadTarget(t)
	c, _ := session(t, NewLoadService())
	if c.Ready.Service != "load" {
		t.Fatalf("ready = %+v", c.Ready)
	}
	start, err := c.Do(Request{Op: OpStart, Name: "a", Load: &LoadStart{
		Streams: []LoadStream{{
			Name: "plain", URL: url, Rate: 200, Connections: 2, RoundRobin: true,
			KeyHeader: "x-chaos-key", Keys: &KeySpec{Kind: KeysList, Values: []string{"k1", "k2"}},
		}},
		Warmup: 50 * time.Millisecond, Duration: 200 * time.Millisecond,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if start.Now.IsZero() || start.LoadState == nil || start.LoadState.Name != "a" || len(start.LoadState.Streams) != 1 {
		t.Fatalf("start = %+v, want the load's state and the service's clock", start)
	}
	ended := waitEnded(t, c, "a")
	st := ended.Streams[0]
	if !st.Done || st.MeasureStart.IsZero() || st.Start.IsZero() || st.Records == 0 {
		t.Fatalf("ended stream = %+v", st)
	}
	if _, err := c.Do(Request{Op: OpStart, Name: "b", Load: &LoadStart{
		Streams: []LoadStream{{Name: "slow", URL: url, Rate: 5}}, Duration: time.Hour,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(Request{Op: OpRecords, Name: "a"}); err == nil || !strings.Contains(err.Error(), "load b runs") {
		t.Fatalf("records while load b runs = %v, want a refusal naming it", err)
	}
	if _, err := c.Do(Request{Op: OpResult, Name: "b"}); err == nil || !strings.Contains(err.Error(), "runs") {
		t.Errorf("result of a running load = %v, want a refusal", err)
	}
	if _, err := c.Do(Request{Op: OpStop, Name: "b"}); err != nil {
		t.Fatal(err)
	}
	waitEnded(t, c, "b")
	res, err := c.Do(Request{Op: OpResult, Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	doc := res.LoadState.Streams[0].Result
	if doc == nil {
		t.Fatal("no result")
	}
	// The result travels as JSON, histograms included.
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var back ResultDoc
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	result, err := back.Result(func(at time.Time) time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	offered := result.Warmup.Offered + result.Measured.Offered
	if result.Measured.Offered == 0 || result.Measured.Latency.Count() == 0 || len(result.Windows) == 0 {
		t.Fatalf("result = measured %d offered, %d latencies, %d windows", result.Measured.Offered, result.Measured.Latency.Count(), len(result.Windows))
	}
	recs := fetchRecords(t, c, "a", 0)
	if int64(len(recs)) != offered || int64(len(recs)) != st.Records {
		t.Fatalf("%d records, the result offered %d and the progress counted %d", len(recs), offered, st.Records)
	}
	seen := map[uint64]bool{}
	for _, r := range recs {
		if seen[r.Seq] || r.Status != http.StatusOK || !r.Keyed || r.Local == "" || r.Sent.Before(r.Intended) || r.Done.Before(r.Sent) {
			t.Fatalf("record %+v: want each request once, answered 200 with its key over a connection", r)
		}
		seen[r.Seq] = true
	}
	if _, err := c.Do(Request{Op: OpRecords, Name: "a", Stream: 1}); err == nil {
		t.Error("records of a stream the load lacks answered")
	}
	if _, err := c.Do(Request{Op: OpRemove, Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(Request{Op: OpProgress, Name: "a"}); err == nil {
		t.Error("progress of a removed load answered")
	}
}

// TestLoadServiceNoRecords: a load that keeps no records, as a saturation
// step, answers its result and refuses records.
func TestLoadServiceNoRecords(t *testing.T) {
	url := loadTarget(t)
	c, _ := session(t, NewLoadService())
	if _, err := c.Do(Request{Op: OpStart, Name: "step", Load: &LoadStart{
		Streams: []LoadStream{{Name: "plain", URL: url, Rate: 100, RoundRobin: true}}, Duration: 100 * time.Millisecond, NoRecords: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if st := waitEnded(t, c, "step").Streams[0]; st.Records != 0 {
		t.Errorf("a load without records kept %d", st.Records)
	}
	if r, err := c.Do(Request{Op: OpResult, Name: "step"}); err != nil || r.LoadState.Streams[0].Result.Measured.Offered == 0 {
		t.Errorf("result = %+v, %v", r.LoadState, err)
	}
	if _, err := c.Do(Request{Op: OpRecords, Name: "step"}); err == nil || !strings.Contains(err.Error(), "keeps no records") {
		t.Errorf("records = %v, want a refusal", err)
	}
}

// TestLoadServiceRefuses: a load needs a fresh name, streams, a duration,
// positive rates, a known protocol, a CA that holds a certificate and a
// key header together with its keys; operations name a known load.
func TestLoadServiceRefuses(t *testing.T) {
	c, _ := session(t, NewLoadService())
	ok := LoadStream{Name: "s", URL: "http://127.0.0.1:1/", Rate: 1}
	start := func(name string, ls *LoadStart) error {
		_, err := c.Do(Request{Op: OpStart, Name: name, Load: ls})
		return err
	}
	with := func(mod func(*LoadStream)) *LoadStart {
		s := ok
		mod(&s)
		return &LoadStart{Streams: []LoadStream{s}, Duration: time.Second}
	}
	for _, tc := range []struct {
		name, load string
		ls         *LoadStart
		want       string
	}{
		{"no name", "", &LoadStart{Streams: []LoadStream{ok}, Duration: time.Second}, "needs a name"},
		{"no stream", "x", &LoadStart{Duration: time.Second}, "no stream"},
		{"no duration", "x", &LoadStart{Streams: []LoadStream{ok}}, "duration"},
		{"zero rate", "x", with(func(s *LoadStream) { s.Rate = 0 }), "rate"},
		{"unknown protocol", "x", with(func(s *LoadStream) { s.Protocol = "h3" }), "unknown protocol"},
		{"empty CA", "x", with(func(s *LoadStream) { s.CA = "not pem" }), "no certificate"},
		{"keys without a header", "x", with(func(s *LoadStream) { s.Keys = &KeySpec{Kind: KeysSingle, Prefix: "k"} }), "go together"},
		{"unknown keys", "x", with(func(s *LoadStream) { s.KeyHeader, s.Keys = "x-k", &KeySpec{Kind: "bogus"} }), "unknown key distribution"},
	} {
		if err := start(tc.load, tc.ls); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: start = %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
	if err := start("dup", &LoadStart{Streams: []LoadStream{ok}, Duration: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := start("dup", &LoadStart{Streams: []LoadStream{ok}, Duration: time.Hour}); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("a second load named dup = %v, want a refusal", err)
	}
	if _, err := c.Do(Request{Op: OpRemove, Name: "dup"}); err == nil || !strings.Contains(err.Error(), "runs") {
		t.Errorf("remove of a running load = %v, want a refusal", err)
	}
	for _, op := range []string{OpProgress, OpStop, OpResult, OpRecords, OpRemove} {
		if _, err := c.Do(Request{Op: op, Name: "missing"}); err == nil || !strings.Contains(err.Error(), `no load "missing"`) {
			t.Errorf("%s of a missing load = %v", op, err)
		}
	}
}

// TestLoadServiceEmptyStream: a stream that ended without a record
// answers one empty block, so the harness reads every stream alike.
func TestLoadServiceEmptyStream(t *testing.T) {
	gen, err := loadgen.Start(t.Context(), loadgen.TargetFunc(func(context.Context, loadgen.Request) loadgen.Response {
		return loadgen.Response{Status: http.StatusOK}
	}), loadgen.Config{Rate: 1000, Duration: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gen.Wait(); err != nil {
		t.Fatal(err)
	}
	tgt, err := loadgen.NewHTTP(loadgen.HTTPConfig{URL: "http://127.0.0.1:1/"})
	if err != nil {
		t.Fatal(err)
	}
	s := NewLoadService()
	s.loads["empty"] = &serviceLoad{streams: []*serviceStream{{spec: LoadStream{Name: "plain"}, gen: gen, tgt: tgt, rec: NewRecordEncoder()}}}
	r := s.Handle(t.Context(), Request{Op: OpRecords, Name: "empty"})
	if !r.OK || r.Records == nil || r.Records.Blocks != 0 || len(r.Records.Data) != 0 {
		t.Errorf("records of an empty stream = %+v (%+v), want one empty block", r, r.Records)
	}
	if r := s.Handle(t.Context(), Request{Op: OpRecords, Name: "empty", Block: 1}); r.OK {
		t.Errorf("a second block of an empty stream answered %+v", r)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestResultDocShift: a result's times move by the harness's translation,
// its windows' included.
func TestResultDocShift(t *testing.T) {
	res, err := loadgen.Run(t.Context(), loadgen.TargetFunc(func(context.Context, loadgen.Request) loadgen.Response {
		return loadgen.Response{Status: http.StatusOK}
	}), loadgen.Config{Rate: 1000, Duration: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	shift := func(at time.Time) time.Time { return at.Add(-time.Hour) }
	got, err := ResultDocOf(res).Result(shift)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Start.Equal(res.Start.Add(-time.Hour)) || !got.MeasureStart.Equal(res.MeasureStart.Add(-time.Hour)) || !got.MeasureEnd.Equal(res.MeasureEnd.Add(-time.Hour)) {
		t.Errorf("shifted times = %v %v %v", got.Start, got.MeasureStart, got.MeasureEnd)
	}
	if len(got.Windows) != len(res.Windows) || !got.Windows[0].Start.Equal(res.Windows[0].Start.Add(-time.Hour)) {
		t.Errorf("windows = %d, first at %v", len(got.Windows), got.Windows[0].Start)
	}
	if got.Measured.Offered != res.Measured.Offered || got.Measured.Outcomes != res.Measured.Outcomes || got.Measured.Latency.Count() != res.Measured.Latency.Count() || got.Windows[0].Service != nil {
		t.Errorf("measured = %+v, want %+v", got.Measured, res.Measured)
	}
}

// TestMockName: the mock names itself in every response, as the chaos
// runs' mock Upstreams do.
func TestMockName(t *testing.T) {
	var out bytes.Buffer
	m, err := StartMock(t.Context(), MockConfig{Name: "backend-2"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+m.Ready().Addr+"/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(&out, resp.Body)
	_ = resp.Body.Close()
	if got := resp.Header.Get(mockup.HeaderServer); got != "backend-2" {
		t.Errorf("%s = %q, want backend-2", mockup.HeaderServer, got)
	}
}

// TestPortCommand: labhost port answers a port free on the -listen host.
func TestPortCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main(t.Context(), []string{"port", "-listen", "127.0.0.1:0"}, strings.NewReader(""), &out, &errb); code != ExitOK {
		t.Fatalf("port = %d: %s", code, errb.String())
	}
	var doc struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc.Port < 1 || doc.Port > 65535 {
		t.Errorf("port answered %q, %v", out.String(), err)
	}
	if code := Main(t.Context(), []string{"port", "-listen", "256.0.0.1:0"}, strings.NewReader(""), &out, &errb); code != ExitError {
		t.Errorf("port on a bad address = %d, want %d", code, ExitError)
	}
}
