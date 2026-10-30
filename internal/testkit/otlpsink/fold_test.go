// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package otlpsink

import (
	"errors"
	"strings"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// Tests for folding the access log's gateway_duration into windowed
// histograms (the benchmark harness's internal view).

func accessFold() *Fold { return &Fold{Event: "ruralz.access", Attribute: "gateway_duration"} }

func dbl(k string, v float64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v}}}
}

func i64(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

// accessAt is an access record at time at with the given attributes.
func accessAt(at time.Time, attrs ...*commonpb.KeyValue) *logspb.LogRecord {
	r := logRecord("access", "ruralz.access", attrs...)
	r.TimeUnixNano = uint64(at.UnixNano()) //nolint:gosec // G115: test times are after 1970
	return r
}

// TestFoldWindows checks that access records fold into every window whose
// [start, end) holds their timestamp, as seconds converted exactly, that
// records outside every window and records without a usable attribute are
// counted, that folded records are never recorded while other records are,
// and that a request holding only folded records still counts as answered.
func TestFoldWindows(t *testing.T) {
	s := start(t, Config{Fold: accessFold()})
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if err := s.AddWindow("warmup", base, base.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWindow("measured", base.Add(10*time.Second), base.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWindow("all", base, base.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	cc := dial(t, s, nil)
	ctx := ctxTimeout(t, 10*time.Second)
	client := collogs.NewLogsServiceClient(cc)
	// One request mixes a process record with folded access records.
	if _, err := client.Export(ctx, logReq("gw",
		logRecord("process line", ""),
		accessAt(base.Add(time.Second), dbl("gateway_duration", 0.000123)),
		accessAt(base.Add(12*time.Second), dbl("gateway_duration", 0.000845)),
		accessAt(base.Add(13*time.Second), i64("gateway_duration", 1)),
	)); err != nil {
		t.Fatal(err)
	}
	// One request holds only access records: never recorded, still counted.
	if _, err := client.Export(ctx, logReq("gw",
		accessAt(base.Add(10*time.Second)),                                              // missing attribute, in measured
		accessAt(base.Add(20*time.Second), dbl("gateway_duration", 0.0002)),             // end is exclusive: outside
		accessAt(base.Add(14*time.Second), &commonpb.KeyValue{Key: "gateway_duration"}), // empty value
	)); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitFolded(ctx, base.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	m, err := s.Window("measured")
	if err != nil {
		t.Fatal(err)
	}
	if m.Count != 2 || m.Missing != 2 || m.Histogram.Count() != 2 {
		t.Errorf("measured = %d folded, %d missing, %d in the histogram; want 2, 2, 2", m.Count, m.Missing, m.Histogram.Count())
	}
	if got := m.Histogram.Max(); got < time.Second || got > time.Second+time.Second/1000 {
		t.Errorf("measured max = %v, want the integer 1 s", got)
	}
	if got := m.Histogram.ValueAtPercentile(50); got < 844*time.Microsecond || got > 846*time.Microsecond {
		t.Errorf("measured p50 = %v, want about 845µs", got)
	}
	w, _ := s.Window("warmup")
	all, _ := s.Window("all")
	if w.Count != 1 || all.Count != 3 {
		t.Errorf("warmup folded %d, all %d; want 1 and 3", w.Count, all.Count)
	}
	if !w.Start.Equal(base) || !w.End.Equal(base.Add(10*time.Second)) {
		t.Errorf("warmup bounds = %v..%v", w.Start, w.End)
	}
	st := s.FoldStats()
	if st.Records != 6 || st.Missing != 2 || st.Outside != 1 || !st.Latest.Equal(base.Add(20*time.Second)) {
		t.Errorf("FoldStats = %+v, want 6 records, 2 missing, 1 outside, latest at the end", st)
	}
	logs := s.Logs()
	if len(logs) != 1 || logs[0].Body() != "process line" {
		t.Errorf("recorded logs = %d (%v), want only the process record", len(logs), logs)
	}
	if got := s.Stats().LogRequests; got != 2 {
		t.Errorf("LogRequests = %d, want 2 (the folded-only request counts)", got)
	}
	if got := len(s.Requests()); got != 1 {
		t.Errorf("recorded requests = %d, want 1", got)
	}
	// The returned histogram is a copy.
	m.Histogram.Record(time.Hour)
	if again, _ := s.Window("measured"); again.Histogram.Count() != 2 {
		t.Error("Window exposes the live histogram")
	}
	s.RemoveWindow("measured")
	if _, err := s.Window("measured"); err == nil {
		t.Error("removed window still answers")
	}
}

// TestFoldWindowRules checks AddWindow's refusals and the errors of a sink
// without Fold.
func TestFoldWindowRules(t *testing.T) {
	s := start(t, Config{Fold: accessFold()})
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if err := s.AddWindow("w", base, base); err == nil {
		t.Error("empty window accepted")
	}
	if err := s.AddWindow("w", base, base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWindow("w", base, base.Add(time.Second)); err == nil {
		t.Error("duplicate window accepted")
	}
	for i := range MaxFoldWindows - 1 {
		if err := s.AddWindow(strings.Repeat("x", i+1), base, base.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddWindow("one-too-many", base, base.Add(time.Second)); err == nil {
		t.Errorf("window %d accepted", MaxFoldWindows+1)
	}
	if _, err := s.Window("nope"); err == nil {
		t.Error("unknown window answered")
	}

	plain := start(t, Config{})
	if err := plain.AddWindow("w", base, base.Add(time.Second)); !errors.Is(err, errNoFold) {
		t.Errorf("AddWindow without Fold = %v", err)
	}
	if _, err := plain.Window("w"); !errors.Is(err, errNoFold) {
		t.Errorf("Window without Fold = %v", err)
	}
	if err := plain.WaitFolded(t.Context(), base); !errors.Is(err, errNoFold) {
		t.Errorf("WaitFolded without Fold = %v", err)
	}
	if st := plain.FoldStats(); st != (FoldStats{}) {
		t.Errorf("FoldStats without Fold = %+v", st)
	}
	plain.RemoveWindow("w") // no-op
	if _, err := Start(t.Context(), Config{Fold: &Fold{Event: "ruralz.access"}}); err == nil {
		t.Error("Fold without an attribute accepted")
	}
}

// TestWaitFoldedEnds checks that WaitFolded returns when its context ends
// or the sink closes before a late enough record arrives.
func TestWaitFoldedEnds(t *testing.T) {
	s := start(t, Config{Fold: accessFold()})
	ctx := ctxTimeout(t, 50*time.Millisecond)
	if err := s.WaitFolded(ctx, time.Now().Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "no folded record") {
		t.Errorf("WaitFolded past its deadline = %v", err)
	}
	_ = s.Close()
	if err := s.WaitFolded(t.Context(), time.Now().Add(time.Hour)); !errors.Is(err, ErrClosed) {
		t.Errorf("WaitFolded after Close = %v", err)
	}
}
