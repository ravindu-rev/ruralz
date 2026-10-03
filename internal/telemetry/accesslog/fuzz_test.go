// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package accesslog

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// maxFraming bounds everything in a line besides the escaped string
// values: member names, punctuation, numbers, constant spellings, the
// truncated list and node_id (TestMaxFraming checks the bound).
const maxFraming = 1600

// worstFramingRecord has every member present with one-byte strings, the
// widest numbers and all eight failure modes.
func worstFramingRecord() *emit.AccessRecord {
	r := &emit.AccessRecord{
		Start: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), Sampled: true,
		Revision: "r", Listener: "l", Protocol: "p", Route: "r", Method: "m", Host: "h", Path: "/",
		Status: math.MinInt64, Code: "c", Duration: math.MinInt64, RequestBytes: math.MinInt64,
		ResponseBytes: math.MinInt64, GatewayDuration: math.MinInt64, UpstreamDuration: math.MinInt64,
		StateStoreDuration: math.MinInt64, ClientAddress: "a", UserAgent: "u", TLSVersion: "t",
		Consumer: "c", Tier: "t", AuthMethod: "a", Upstream: "u", Endpoint: "e", Attempts: math.MinInt64,
		ShortCircuitPolicy: "s", ShortCircuitPhase: phase.OnUpstreamResponseHeaders, Cache: "c",
	}
	for range MaxFailureModes + 1 {
		r.FailureModes = append(r.FailureModes, emit.FailureModeEntry{Policy: "p", Phase: phase.OnUpstreamResponseHeaders, Mode: "m"})
	}
	return r
}

func TestMaxFraming(t *testing.T) {
	r := worstFramingRecord()
	e := &entry{}
	e.fill(r)
	e.truncated = 1<<numFields - 1 // every name listed
	line := e.appendLine(nil, nodeMember(testNode))
	framing := len(line) - (MaxRecordBytes - e.budget)
	if framing > maxFraming {
		t.Fatalf("framing is %d bytes, above the %d bound", framing, maxFraming)
	}
}

// FuzzAccessLogRecord (test plan 6.3 item 29): arbitrary field bytes give
// exactly one line that encoding/json parses, without a raw newline, with
// at most 4 KiB of string values plus framing, and never a query string.
func FuzzAccessLogRecord(f *testing.F) {
	f.Add("GET", "example.com", "/a?b=c", "curl", "route", "policy", int64(200), 3)
	f.Add("", "", "", "", "", "", int64(0), 0)
	f.Add("\xff\xfe", "\x00\n", strings.Repeat("\u2028", 100), "\"\\", "\t", "é", int64(-1), 9)
	f.Add(strings.Repeat("M", 1500), strings.Repeat("h", 300), strings.Repeat("/p", 600), "ua", "r", "p", int64(599), 12)
	f.Fuzz(func(t *testing.T, method, host, path, ua, route, policy string, status int64, nfm int) {
		r := &emit.AccessRecord{
			Method: method, Host: host, Path: path, UserAgent: ua, Route: route, Listener: route,
			Consumer: policy, Upstream: host, Endpoint: path, Status: int(status),
			Duration: time.Duration(status), ShortCircuitPolicy: policy, Code: ua, Cache: method,
		}
		for i := range nfm % 16 {
			r.FailureModes = append(r.FailureModes, emit.FailureModeEntry{Policy: policy, Phase: phase.Phase(i), Mode: route})
		}
		e := &entry{}
		e.fill(r)
		values := MaxRecordBytes - e.budget
		line := e.appendLine(nil, nodeMember(route))
		if values > MaxRecordBytes {
			t.Fatalf("string values take %d bytes", values)
		}
		if len(line) > values+maxFraming+len(route)*6 {
			t.Fatalf("line of %d bytes exceeds %d of values plus framing", len(line), values)
		}
		if bytes.ContainsAny(line, "\n\r") {
			t.Fatalf("raw newline in %q", line)
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("invalid JSON %q: %v", line, err)
		}
		if p, _ := m["path"].(string); strings.Contains(p, "?") {
			t.Fatalf("query string logged: %q", p)
		}
		if got := AppendJSON(nil, r, route); !bytes.Equal(got, line) {
			t.Fatal("AppendJSON differs from the queued encoding")
		}
	})
}
