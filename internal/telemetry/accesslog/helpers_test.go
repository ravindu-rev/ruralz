// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package accesslog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

const testNode = "01J9Z8Q4W6X3V5T7R2N0M1K8H4"

// testZone is created once: time.FixedZone allocates.
var testZone = time.FixedZone("x", 3600)

// fullRecord fills every M1 field of r (09 req 68).
func fullRecord(r *emit.AccessRecord) {
	r.Start = time.Date(2026, 9, 26, 7, 46, 12, 345678901, testZone)
	r.TraceID = [16]byte{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
	r.SpanID = [8]byte{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7}
	r.Sampled = true
	r.Revision = "rev-4f2a9c01b7de"
	r.Listener = "public"
	r.Protocol = "http2"
	r.Route = "orders-summary"
	r.Method = "GET"
	r.Host = "api.example.com"
	r.Path = "/v1/orders/42/summary"
	r.Status = 429
	r.Code = "RZ-RL-005"
	r.Duration = 12345678 * time.Nanosecond
	r.RequestBytes = 0
	r.ResponseBytes = 187
	r.GatewayDuration = 143 * time.Microsecond
	r.UpstreamDuration = 11 * time.Millisecond
	r.StateStoreDuration = 812 * time.Microsecond
	r.ClientAddress = "203.0.113.7"
	r.UserAgent = "curl/8.9.1"
	r.TLSVersion = "1.3"
	r.Consumer = "acme-mobile"
	r.Tier = "gold"
	r.AuthMethod = "jwt"
	r.Upstream = "orders"
	r.Endpoint = "10.0.4.17:8080"
	r.Attempts = 2
	r.ShortCircuitPolicy = "ratelimit-per-consumer"
	r.ShortCircuitPhase = phase.OnRequestHeaders
	r.FailureModes = append(r.FailureModes, emit.FailureModeEntry{Policy: "quota-monthly", Phase: phase.OnRequestHeaders, Mode: "open"})
	r.Cache = "miss"
}

// encode returns the access line of r.
func encode(r *emit.AccessRecord) string { return string(AppendJSON(nil, r, testNode)) }

// keys returns the top-level member names of a JSON object, in order.
func keys(t *testing.T, line string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %q", line)
	}
	var out []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("%v in %q", err, line)
		}
		out = append(out, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("%v in %q", err, line)
		}
	}
	return out
}

// object decodes a JSON line.
func object(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("invalid JSON %q: %v", line, err)
	}
	return m
}
