// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package accesslog

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a fixed testdata path.
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("output differs from %s:\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

// Test plan 6.4 item 35, WP-11 done-when: access log encoder golden bytes
// for a full record, a minimal one, a truncated one and an unmatched one.
func TestAccessGolden_Req68(t *testing.T) {
	var buf bytes.Buffer
	full := &emit.AccessRecord{}
	fullRecord(full)
	buf.WriteString(encode(full) + "\n")

	minimal := &emit.AccessRecord{
		Start: time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), Revision: "rev-4f2a9c01b7de",
		Listener: "public", Protocol: "http1", Route: "health", Method: "GET", Host: "gw.local",
		Path: "/healthz", Status: 200, Duration: 250 * time.Microsecond, ClientAddress: "192.0.2.10",
	}
	buf.WriteString(encode(minimal) + "\n")

	cut := &emit.AccessRecord{}
	fullRecord(cut)
	cut.Host = strings.Repeat("h", 300)
	cut.Path = "/" + strings.Repeat("p", 1100) + "?q=never"
	cut.UserAgent = strings.Repeat("u", 257)
	cut.GatewayDurationSkipped = true
	cut.TLSVersion = ""
	buf.WriteString(encode(cut) + "\n")

	unmatched := &emit.AccessRecord{
		Start: time.Date(2026, 9, 26, 0, 0, 0, 1000, time.UTC), Revision: "rev-4f2a9c01b7de",
		Listener: "public", Protocol: "http1", Route: "_unmatched", Method: "POST", Host: "gw.local",
		Path: "/nope?token=secret", Status: 404, Code: "RZ-RT-001", ClientAddress: "2001:db8::1",
		UserAgent: "ua \"quoted\"\n\xff",
	}
	buf.WriteString(encode(unmatched) + "\n")
	golden(t, "access.golden", buf.Bytes())
}

// 09 req 68: the members and their order for a full record.
func TestAccessFieldOrder_Req68(t *testing.T) {
	r := &emit.AccessRecord{}
	fullRecord(r)
	line := encode(r)
	want := []string{
		"time", "level", "msg", "trace_id", "span_id", "sampled", "node_id", "revision",
		"listener", "protocol", "route", "method", "host", "path", "status", "code",
		"duration", "request_bytes", "response_bytes", "gateway_duration", "upstream_duration",
		"state_store_duration", "client_address", "user_agent", "tls_version", "consumer", "tier",
		"auth_method", "upstream", "endpoint", "attempts", "short_circuit", "failure_modes", "cache",
	}
	if got := keys(t, line); !slices.Equal(got, want) {
		t.Fatalf("keys = %v\nwant   %v", got, want)
	}
	m := object(t, line)
	checks := map[string]any{
		"time": "2026-09-26T06:46:12.345678Z", "level": "INFO", "msg": "access",
		"trace_id": "4bf92f3577b34da6a3ce929d0e0e4736", "span_id": "00f067aa0ba902b7", "sampled": true,
		"node_id": testNode, "status": float64(429), "duration": 0.012345, "gateway_duration": 0.000143,
		"attempts": float64(2), "tls_version": "1.3",
	}
	for k, v := range checks {
		if m[k] != v {
			t.Errorf("%s = %#v, want %#v", k, m[k], v)
		}
	}
	if got := fmt.Sprint(m["short_circuit"]); got != "map[phase:onRequestHeaders policy:ratelimit-per-consumer]" {
		t.Errorf("short_circuit = %s", got)
	}
	if got := fmt.Sprint(m["failure_modes"]); got != "[map[mode:open phase:onRequestHeaders policy:quota-monthly]]" {
		t.Errorf("failure_modes = %s", got)
	}
	if !strings.Contains(line, `"duration":0.012345,`) {
		t.Errorf("duration not written with six decimals: %s", line)
	}
}

// 09 req 68: optional members are omitted when empty; the always-present
// ones are written even when empty.
func TestAccessOmissions_Req68(t *testing.T) {
	r := &emit.AccessRecord{Status: 499, GatewayDurationSkipped: true, Endpoint: "ignored without upstream"}
	line := encode(r)
	want := []string{
		"time", "level", "msg", "trace_id", "span_id", "sampled", "node_id", "revision",
		"listener", "protocol", "route", "method", "host", "path", "status",
		"duration", "request_bytes", "response_bytes", "upstream_duration",
		"state_store_duration", "client_address", "user_agent",
	}
	if got := keys(t, line); !slices.Equal(got, want) {
		t.Fatalf("keys = %v\nwant   %v", got, want)
	}
	r2 := &emit.AccessRecord{Upstream: "orders"}
	if got := keys(t, encode(r2)); !slices.Contains(got, "attempts") || slices.Contains(got, "endpoint") {
		t.Fatalf("upstream without endpoint: %v", got)
	}
}

// 09 req 67, 68, test plan 6.1 item 17: host, path and user_agent caps,
// truncated naming the cut fields, no query string.
func TestAccessTruncation_Req67(t *testing.T) {
	r := &emit.AccessRecord{
		Host:      strings.Repeat("h", MaxHostBytes+1),
		Path:      "/" + strings.Repeat("p", MaxPathBytes) + "?secret=1",
		UserAgent: strings.Repeat("u", MaxUserAgentBytes+10),
	}
	m := object(t, encode(r))
	if got := len(m["host"].(string)); got != MaxHostBytes {
		t.Errorf("host length %d", got)
	}
	if got := len(m["path"].(string)); got != MaxPathBytes {
		t.Errorf("path length %d", got)
	}
	if got := len(m["user_agent"].(string)); got != MaxUserAgentBytes {
		t.Errorf("user_agent length %d", got)
	}
	if got := fmt.Sprint(m["truncated"]); got != "[host path user_agent]" {
		t.Errorf("truncated = %s", got)
	}

	exact := &emit.AccessRecord{Host: strings.Repeat("h", MaxHostBytes), Path: "/a?b=c&d=e"}
	m = object(t, encode(exact))
	if _, ok := m["truncated"]; ok {
		t.Errorf("fields at their cap or without a query are not truncated: %v", m["truncated"])
	}
	if m["path"] != "/a" {
		t.Errorf("path = %v, the query string must never be logged", m["path"])
	}
}

// 09 req 67: a multi-byte character is never split by a cap.
func TestAccessTruncationUTF8_Req67(t *testing.T) {
	r := &emit.AccessRecord{Host: strings.Repeat("h", MaxHostBytes-1) + "é"}
	m := object(t, encode(r))
	if got := m["host"].(string); got != strings.Repeat("h", MaxHostBytes-1) {
		t.Errorf("host = %q", got)
	}
}

// 09 req 67, test plan 6.1 item 17: string values take at most 4 KiB once
// escaped; later fields are cut and named, and still present.
func TestAccessRecordCap_Req67(t *testing.T) {
	long := strings.Repeat("r", 1500)
	r := &emit.AccessRecord{
		Listener: long, Route: long, Method: long, Consumer: "c", Upstream: "u",
		ShortCircuitPolicy: "sc", ShortCircuitPhase: phase.OnRoute,
	}
	e := &entry{}
	e.fill(r)
	if used := MaxRecordBytes - e.budget; used != MaxRecordBytes {
		t.Fatalf("used %d escaped bytes", used)
	}
	m := object(t, encode(r))
	if got := len(m["method"].(string)); got != MaxRecordBytes-3000 {
		t.Errorf("method length %d", got)
	}
	if m["consumer"] != "" || m["upstream"] != "" {
		t.Errorf("fields past the cap must be present and empty: %v %v", m["consumer"], m["upstream"])
	}
	if got := fmt.Sprint(m["truncated"]); got != "[method consumer upstream short_circuit]" {
		t.Errorf("truncated = %s", got)
	}

	// Escaping counts: 700 control bytes escape to 4,200 bytes.
	esc := &emit.AccessRecord{Route: strings.Repeat("\x01", 700)}
	m = object(t, encode(esc))
	if got := len(m["route"].(string)); got != MaxRecordBytes/6 {
		t.Errorf("escaped route kept %d bytes, want %d", got, MaxRecordBytes/6)
	}
}

// 09 req 68, test plan 6.1 item 17: JSON escaping, invalid UTF-8 as
// U+FFFD, control characters escaped.
func TestAccessEscaping_Req68(t *testing.T) {
	r := &emit.AccessRecord{UserAgent: "a\"b\\c\nd\re\tf\x00g\x1f\xff\u2028<&>é"}
	line := encode(r)
	want := `"user_agent":"a\"b\\c\nd\re\tf\u0000g\u001f\ufffd\u2028<&>é"`
	if !strings.Contains(line, want) {
		t.Fatalf("line %s\nlacks %s", line, want)
	}
	if strings.ContainsAny(line, "\n\r") {
		t.Fatal("raw newline in a line")
	}
}

// 09 req 68, test plan 6.1 item 17: failure_modes holds at most 8 entries.
func TestAccessFailureModesCap_Req68(t *testing.T) {
	r := &emit.AccessRecord{}
	for i := range 11 {
		r.FailureModes = append(r.FailureModes, emit.FailureModeEntry{Policy: fmt.Sprintf("p%d", i), Phase: phase.OnRoute, Mode: "closed"})
	}
	m := object(t, encode(r))
	fms := m["failure_modes"].([]any)
	if len(fms) != MaxFailureModes {
		t.Fatalf("failure_modes has %d entries", len(fms))
	}
	if got := fmt.Sprint(fms[7]); got != "map[mode:closed phase:onRoute policy:p7]" {
		t.Errorf("entry 8 = %s", got)
	}
	if got := fmt.Sprint(m["truncated"]); got != "[failure_modes]" {
		t.Errorf("truncated = %s", got)
	}
}

// 09 req 68: durations are seconds with microsecond precision.
func TestAppendSeconds_Req68(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0.000000"},
		{999 * time.Nanosecond, "0.000000"},
		{time.Microsecond, "0.000001"},
		{150 * time.Microsecond, "0.000150"},
		{1500 * time.Millisecond, "1.500000"},
		{3723*time.Second + 45*time.Microsecond, "3723.000045"},
		{-2500 * time.Microsecond, "-0.002500"},
	}
	for _, tt := range tests {
		if got := string(appendSeconds(nil, tt.d)); got != tt.want {
			t.Errorf("appendSeconds(%v) = %s, want %s", tt.d, got, tt.want)
		}
	}
}

// fit agrees with the encoder on escaped sizes and never splits a rune.
func TestFit(t *testing.T) {
	tests := []struct {
		s              string
		maxRaw, maxEnc int
		n, enc         int
	}{
		{"abc", 10, 10, 3, 3},
		{"abc", 2, 10, 2, 2},
		{"abc", 10, 2, 2, 2},
		{"a\"b", 10, 10, 3, 4},
		{"\x01", 10, 5, 0, 0},
		{"\x01", 10, 6, 1, 6},
		{"éa", 1, 10, 0, 0},
		{"éa", 2, 10, 2, 2},
		{"\xffa", 10, 7, 2, 7},
		{"\u2029", 10, 6, 3, 6},
	}
	for _, tt := range tests {
		n, enc := fit(tt.s, tt.maxRaw, tt.maxEnc)
		if n != tt.n || enc != tt.enc {
			t.Errorf("fit(%q, %d, %d) = %d, %d; want %d, %d", tt.s, tt.maxRaw, tt.maxEnc, n, enc, tt.n, tt.enc)
		}
		if got := len(appendEscaped(nil, []byte(tt.s[:n]))); got != enc {
			t.Errorf("escaped %q is %d bytes, fit said %d", tt.s[:n], got, enc)
		}
	}
}
