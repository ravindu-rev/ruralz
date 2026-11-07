// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	metrics "github.com/hashicorp/go-metrics"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// TestRaftLogsReachSlog: hashicorp/raft's hclog records reach slog with
// their component, snake_case keys and mapped levels, keys the record
// itself writes are prefixed, and below-level records are dropped.
func TestRaftLogsReachSlog(t *testing.T) {
	var buf bytes.Buffer
	l := newRaftLogger(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	l.Trace("trace record", "a", 1)
	l.Info("entering follower state", "last-index", 7, "error", errors.New("boom"))
	l.Named("snapshot").Warn("odd", "orphan")
	l.Info("reserved keys", "time", "t", "Level", "l", "msg", "m", "source", "s", "component", "c")
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(line, `"msg":`); n != 1 {
			t.Errorf("record %s has %d msg keys", line, n)
		}
		recs = append(recs, m)
	}
	if len(recs) != 4 {
		t.Fatalf("records = %v", recs)
	}
	if r := recs[3]; r["msg"] != "reserved keys" || r["level"] != "INFO" || r["component"] != componentRaft ||
		r["raft_time"] != "t" || r["raft_level"] != "l" || r["raft_msg"] != "m" || r["raft_source"] != "s" || r["raft_component"] != "c" {
		t.Errorf("reserved keys record = %v", r)
	}
	if recs[0]["level"] != "DEBUG" || recs[1]["level"] != "INFO" || recs[2]["level"] != "WARN" {
		t.Errorf("levels = %v %v %v", recs[0]["level"], recs[1]["level"], recs[2]["level"])
	}
	if recs[1]["last_index"] != float64(7) || recs[1]["error"] != "boom" || recs[1]["component"] != componentRaft {
		t.Errorf("record = %v", recs[1])
	}
	if recs[2]["component"] != componentRaft+".snapshot" || recs[2]["extra"] != "orphan" {
		t.Errorf("named record = %v", recs[2])
	}
	buf.Reset()
	quiet := newRaftLogger(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	quiet.Info("dropped")
	if buf.Len() != 0 {
		t.Errorf("a below-level record was written: %s", buf.String())
	}
}

// fakeHistogram records values.
type fakeHistogram struct {
	mu sync.Mutex
	v  []uint64
}

func (h *fakeHistogram) Record(_ emit.Stripe, v uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.v = append(h.v, v)
}

func (h *fakeHistogram) RecordExemplar(s emit.Stripe, v uint64, _ emit.Exemplar) { h.Record(s, v) }

func (h *fakeHistogram) values() []uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.v)
}

// TestMetricSinkForwardsCommitTime: only raft's commit time reaches
// ruralz_control_raft_commit_duration_seconds, in nanoseconds.
func TestMetricSinkForwardsCommitTime(t *testing.T) {
	h := &fakeHistogram{}
	s := NewMetricSink(h)
	s.AddSample([]string{"raft", "commitTime"}, 2.5)
	s.AddSampleWithLabels([]string{"raft", "commitTime"}, 1, nil)
	s.AddSample([]string{"raft", "fsm", "apply"}, 9)
	s.AddSample([]string{"raft", "commitTime"}, -1)
	s.SetGauge([]string{"raft", "peers"}, 3)
	s.IncrCounter([]string{"raft", "state", "leader"}, 1)
	s.EmitKey([]string{"x"}, 1)
	s.SetGaugeWithLabels(nil, 0, nil)
	s.IncrCounterWithLabels(nil, 0, nil)
	if got, want := h.values(), []uint64{2_500_000, 1_000_000}; !slices.Equal(got, want) {
		t.Errorf("recorded %v, want %v", got, want)
	}
	NewMetricSink(nil).AddSample([]string{"raft", "commitTime"}, 1)
}

// TestInstallMetricSink routes raft's process-wide go-metrics calls to the
// sink, then restores the discarding default.
func TestInstallMetricSink(t *testing.T) {
	h := &fakeHistogram{}
	if err := InstallMetricSink(NewMetricSink(h)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = metrics.NewGlobal(metrics.DefaultConfig(""), &metrics.BlackholeSink{}) })
	metrics.AddSample([]string{"raft", "commitTime"}, 123.5)
	if !slices.Contains(h.values(), 123_500_000) {
		t.Errorf("recorded %v, want 123.5 ms", h.values())
	}
}

// TestFrames round-trips headers and refuses malformed ones.
func TestFrames(t *testing.T) {
	var b bytes.Buffer
	if err := writeHeader(&b, statusResponse{Accepted: true, Leader: "a:1"}); err != nil {
		t.Fatal(err)
	}
	var got statusResponse
	if err := readHeader(&b, &got); err != nil || !got.Accepted || got.Leader != "a:1" {
		t.Errorf("readHeader = %+v, %v", got, err)
	}
	if err := writeHeader(&b, strings.Repeat("x", maxHeader)); err == nil {
		t.Error("an oversized header was written")
	}
	for name, raw := range map[string][]byte{
		"zero length":   {0, 0, 0, 0},
		"too long":      binary.BigEndian.AppendUint32(nil, maxHeader+1),
		"unknown field": append(binary.BigEndian.AppendUint32(nil, 11), []byte(`{"bogus":1}`)...),
		"truncated":     append(binary.BigEndian.AppendUint32(nil, 50), []byte(`{}`)...),
	} {
		if err := readHeader(bytes.NewReader(raw), &got); err == nil {
			t.Errorf("%s: readHeader accepted", name)
		}
	}
}
