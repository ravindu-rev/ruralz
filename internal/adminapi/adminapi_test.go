// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Tests for architecture section 2.15 (WP-01): JSON goldens of the admin
// wire shapes of spec 04 requirements 61 (/readyz), 73 (/debug/snapshots)
// and 75 (/tap events, member order), plus the {"dropped":N} notice (R-24)
// and the "readers ignore unknown members" rule.

func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: the test reads its own golden files.
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(b, []byte("\n"))
}

func ptr(s string) *string { return &s }

func TestGoldens(t *testing.T) {
	tests := []struct {
		file string
		v    any
	}{
		{"readyz-ready.json", Readyz{Status: "ready", Revision: "rev-162af81f5de4"}},
		{"readyz-not-ready.json", Readyz{Status: "not_ready", Reasons: []ReadyReason{
			{Reason: ReasonNoRevision},
			{Reason: ReasonSecretsUnresolved, Code: "RZ-CFG-026", Detail: "file:/run/secrets/tls.pem is unreadable"},
			{Reason: ReasonListenersUnbound, Code: "RZ-CFG-039"},
			{Reason: ReasonDraining},
		}}},
		{"tap-event.json", TapEvent{
			Time: "2026-09-26T12:00:00.123Z", TraceID: "0af7651916cd43dd8448eb211c80319c", Listener: "public",
			Protocol: "http2", Route: "orders", Method: "GET", Host: "api.example", Path: "/orders/7",
			Status: 429, Code: "RZ-RL-002", DurationMs: 1.25, RequestBytes: 0, ResponseBytes: 121,
			Upstream: "inventory", Endpoint: "10.0.0.7:8080", Consumer: "acme",
			RequestHeaders:  map[string][]string{"authorization": {"[REDACTED]"}, "accept": {"application/json"}},
			ResponseHeaders: map[string][]string{"retry-after": {"1"}},
		}},
		{"tap-event-sparse.json", TapEvent{
			Time: "2026-09-26T12:00:01Z", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", Listener: "public",
			Protocol: "http1", Route: "_unmatched", Method: "POST", Host: "api.example", Path: "/nowhere",
			Status: 404, DurationMs: 0.05, RequestBytes: 12, ResponseBytes: 110,
		}},
		{"tap-dropped.json", TapDropped{Dropped: 3}},
		{"snapshots.json", Snapshots{
			Snapshots: []SnapshotInfo{
				{
					State: StateActive, Revision: "rev-68f782530463",
					Digest: "sha256:68f782530463d5c1f0e2b9a7c4d8e6f1a3b5c7d9e0f2a4b6c8d0e2f4a6b8c0d2",
					Pins:   12, ActivatedAt: "2026-09-26T12:00:00Z",
				},
				{
					State: StateRetired, Revision: "rev-162af81f5de4",
					Digest: "sha256:162af81f5de482d540e09cf25205ffe39b705fd2dcf5eca88eb42e92b098a85e",
					Pins:   1, ActivatedAt: "2026-09-26T11:00:00Z", RetiredAt: "2026-09-26T12:00:00Z", GraceEndsAt: "2026-09-26T12:00:30Z",
				},
			},
			Pending:       ptr("sha256:0e1f2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"),
			LastKnownGood: ptr("sha256:68f782530463d5c1f0e2b9a7c4d8e6f1a3b5c7d9e0f2a4b6c8d0e2f4a6b8c0d2"),
		}},
		{"snapshots-no-pending.json", Snapshots{Snapshots: []SnapshotInfo{}}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got, err := json.Marshal(tt.v)
			if err != nil {
				t.Fatal(err)
			}
			want := golden(t, tt.file)
			if !bytes.Equal(got, want) {
				t.Fatalf("json.Marshal =\n%s\nwant\n%s", got, want)
			}
			// Decoding the golden gives back the value.
			back := reflect.New(reflect.TypeOf(tt.v))
			if err := json.Unmarshal(want, back.Interface()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back.Elem().Interface(), tt.v) {
				t.Fatalf("round trip = %+v, want %+v", back.Elem().Interface(), tt.v)
			}
		})
	}
}

func TestTapEventMemberOrder(t *testing.T) {
	// 04 req 75 field order; optional members are omitted when empty.
	full := golden(t, "tap-event.json")
	dec := json.NewDecoder(bytes.NewReader(full))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"time", "traceId", "listener", "protocol", "route", "method", "host", "path", "status", "code",
		"durationMs", "requestBytes", "responseBytes", "upstream", "endpoint", "consumer", "requestHeaders", "responseHeaders",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("members %v, want %v", keys, want)
	}
}

func TestSnapshotsPendingNull(t *testing.T) {
	// pending and lastKnownGood are always present: a digest or null.
	b, err := json.Marshal(Snapshots{Snapshots: []SnapshotInfo{{State: StateEnding, Revision: "rev-a", Digest: "sha256:a", ActivatedAt: "t"}}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["pending"]) != "null" || string(m["lastKnownGood"]) != "null" {
		t.Fatalf("pending %s, lastKnownGood %s; want null", m["pending"], m["lastKnownGood"])
	}
	var s Snapshots
	if err := json.Unmarshal(b, &s); err != nil || s.Pending != nil || s.LastKnownGood != nil {
		t.Fatalf("decoded %+v, %v", s, err)
	}
	// A zero pins count is still written.
	var info map[string]json.RawMessage
	if err := json.Unmarshal(mustIndex(t, m["snapshots"]), &info); err != nil {
		t.Fatal(err)
	}
	if string(info["pins"]) != "0" {
		t.Fatalf("pins = %s, want 0", info["pins"])
	}
	if _, ok := info["retiredAt"]; ok {
		t.Fatal("empty retiredAt was written")
	}
}

func mustIndex(t *testing.T, arr json.RawMessage) []byte {
	t.Helper()
	var items []json.RawMessage
	if err := json.Unmarshal(arr, &items); err != nil || len(items) == 0 {
		t.Fatalf("snapshots = %s, %v", arr, err)
	}
	return items[0]
}

func TestReadersIgnoreUnknownMembers(t *testing.T) {
	// Shapes are versioned surfaces: members are only added.
	var r Readyz
	if err := json.Unmarshal([]byte(`{"status":"ready","revision":"rev-1","since":"2026-09-26T12:00:00Z","extra":{"a":1}}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Status != "ready" || r.Revision != "rev-1" {
		t.Fatalf("Readyz = %+v", r)
	}
	var e TapEvent
	if err := json.Unmarshal([]byte(`{"status":200,"route":"r","newMember":[1,2]}`), &e); err != nil || e.Status != 200 || e.Route != "r" {
		t.Fatalf("TapEvent = %+v, %v", e, err)
	}
}

func TestConstants(t *testing.T) {
	// Readiness reasons of 04 req 61 and snapshot states of 04 req 73.
	got := []string{
		ReasonNoRevision, ReasonSecretsUnresolved, ReasonListenersUnbound, ReasonDraining,
		StateActive, StateRetired, StateClosing, StateEnding,
	}
	want := []string{
		"no_revision", "secrets_unresolved", "listeners_unbound", "draining",
		"active", "retired", "closing", "ending",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("constants %v, want %v", got, want)
	}
}
