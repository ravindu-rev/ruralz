// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminapi

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// Tests for the /debug/upstreams body (docs/architecture/03-data-plane.md
// "Admin endpoints"): its golden with the member order the document
// lists, null for unset times and errors, [] for empty lists, the
// constants and the "readers ignore unknown members" rule.

// sampleUpstreams is the value of testdata/upstreams.json: a dns
// Upstream with an open breaker, a stale source, a ring and two Endpoints,
// and a static one half-open without Endpoints.
func sampleUpstreams() Upstreams {
	return Upstreams{Upstreams: []UpstreamInfo{
		{
			Name: "inventory",
			Source: SourceInfo{
				Type: SourceDNS, LastRefresh: ptr("2026-09-26T11:59:30Z"), Stale: true, Failures: 2,
				Error: ptr("lookup inventory.internal: no such host"),
			},
			Breaker: BreakerInfo{
				State: BreakerOpen, OpenedAt: ptr("2026-09-26T11:59:58.25Z"), OpenUntil: ptr("2026-09-26T12:00:26Z"),
				WindowLegs: 24, WindowFailures: 13,
			},
			InFlight: 3, Pending: 1,
			RetryBudget: RetryBudgetInfo{Originals: 3, Retries: 1},
			Balancers: []BalancerInfo{{
				Algorithm: "ring-hash", Effective: "ring-hash", VirtualNodes: 1024, Bytes: 32768, Endpoints: 2,
				Built: ptr("2026-09-26T11:59:30Z"),
			}},
			Endpoints: []EndpointInfo{
				{
					Identity: "inventory-0.internal:8080", Weight: 3, Healthy: true, Ejected: true,
					EjectedUntil: ptr("2026-09-26T12:00:30Z"), Ejections: 1, Failures1s: 2,
					Addresses: []string{"10.0.0.7:8080", "[fd00::7]:8080"},
				},
				{
					Identity: "inventory-1.internal:8080", Weight: 1, ConsecutiveErrors: 1, Suspect: true, Stalled: 1,
					InFlight: 3, Addresses: []string{"10.0.0.8:8080"},
				},
			},
		},
		{
			Name:    "orders",
			Source:  SourceInfo{Type: SourceStatic},
			Breaker: BreakerInfo{State: BreakerHalfOpen, OpenedAt: ptr("2026-09-26T11:58:00Z")},
			Balancers: []BalancerInfo{{
				Algorithm: "least-request", Effective: "least-request", Built: ptr("2026-09-26T11:00:00Z"),
			}},
			Endpoints: []EndpointInfo{},
		},
	}}
}

func TestUpstreamsGoldens(t *testing.T) {
	tests := []struct {
		file string
		v    Upstreams
	}{
		{"upstreams.json", sampleUpstreams()},
		{"upstreams-empty.json", Upstreams{Upstreams: []UpstreamInfo{}}},
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
			var back Upstreams
			if err := json.Unmarshal(want, &back); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back, tt.v) {
				t.Fatalf("round trip = %+v, want %+v", back, tt.v)
			}
		})
	}
}

// keys returns the member names of a JSON object in document order.
func keys(t *testing.T, obj json.RawMessage) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(obj))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("%s is not an object: %v", obj, err)
	}
	var out []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// member returns the value of the member called name of a JSON object.
func member(t *testing.T, obj json.RawMessage, name string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(obj, &m); err != nil {
		t.Fatal(err)
	}
	v, ok := m[name]
	if !ok {
		t.Fatalf("%s has no member %q", obj, name)
	}
	return v
}

// first returns the first element of a JSON array.
func first(t *testing.T, arr json.RawMessage) json.RawMessage {
	t.Helper()
	var items []json.RawMessage
	if err := json.Unmarshal(arr, &items); err != nil || len(items) == 0 {
		t.Fatalf("%s is not a non-empty array: %v", arr, err)
	}
	return items[0]
}

// TestUpstreamsMemberOrder holds every object of the body to the members
// and order the document lists, each always present.
func TestUpstreamsMemberOrder(t *testing.T) {
	doc := json.RawMessage(golden(t, "upstreams.json"))
	up := first(t, member(t, doc, "upstreams"))
	tests := []struct {
		name string
		obj  json.RawMessage
		want []string
	}{
		{"body", doc, []string{"upstreams"}},
		{"upstream", up, []string{"name", "source", "breaker", "inFlight", "pending", "retryBudget", "panic", "balancers", "endpoints"}},
		{"source", member(t, up, "source"), []string{"type", "lastRefresh", "stale", "failures", "error"}},
		{"breaker", member(t, up, "breaker"), []string{"state", "openedAt", "openUntil", "windowLegs", "windowFailures", "consecutiveFailures"}},
		{"retryBudget", member(t, up, "retryBudget"), []string{"originals", "retries"}},
		{"balancer", first(t, member(t, up, "balancers")), []string{"algorithm", "effective", "virtualNodes", "fallback", "bytes", "endpoints", "built", "stale"}},
		{"endpoint", first(t, member(t, up, "endpoints")), []string{
			"identity", "weight", "healthy", "ejected", "ejectedUntil", "ejections", "consecutiveErrors",
			"suspect", "stalled", "failures1s", "inFlight", "addresses",
		}},
	}
	for _, tt := range tests {
		if got := keys(t, tt.obj); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s members = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestUpstreamsNullsAndEmptyLists checks that unset times and a missing
// refresh error are null, never omitted, and that empty lists are [].
func TestUpstreamsNullsAndEmptyLists(t *testing.T) {
	b, err := json.Marshal(UpstreamInfo{
		Name:      "orders",
		Source:    SourceInfo{Type: SourceStatic},
		Breaker:   BreakerInfo{State: BreakerClosed},
		Balancers: []BalancerInfo{{Algorithm: "random", Effective: "random"}},
		Endpoints: []EndpointInfo{{Identity: "10.0.0.1:80", Addresses: []string{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	up := json.RawMessage(b)
	ep := first(t, member(t, up, "endpoints"))
	for _, tc := range []struct {
		name string
		got  json.RawMessage
		want string
	}{
		{"source.lastRefresh", member(t, member(t, up, "source"), "lastRefresh"), "null"},
		{"source.error", member(t, member(t, up, "source"), "error"), "null"},
		{"breaker.openedAt", member(t, member(t, up, "breaker"), "openedAt"), "null"},
		{"breaker.openUntil", member(t, member(t, up, "breaker"), "openUntil"), "null"},
		{"balancers[0].built", member(t, first(t, member(t, up, "balancers")), "built"), "null"},
		{"endpoints[0].ejectedUntil", member(t, ep, "ejectedUntil"), "null"},
		{"endpoints[0].addresses", member(t, ep, "addresses"), "[]"},
		{"endpoints[0].inFlight", member(t, ep, "inFlight"), "0"},
	} {
		if string(tc.got) != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
	var back UpstreamInfo
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Source.LastRefresh != nil || back.Source.Error != nil || back.Breaker.OpenedAt != nil || back.Endpoints[0].EjectedUntil != nil {
		t.Errorf("decoded nulls = %+v, want nil pointers", back)
	}
}

func TestUpstreamsReadersIgnoreUnknownMembers(t *testing.T) {
	// Shapes are versioned surfaces: members are only added.
	var u Upstreams
	in := `{"upstreams":[{"name":"orders","zone":"a","source":{"type":"dns","ttl":30},"endpoints":[{"identity":"10.0.0.1:80","locality":"a"}]}],"node":"n1"}`
	if err := json.Unmarshal([]byte(in), &u); err != nil {
		t.Fatal(err)
	}
	if len(u.Upstreams) != 1 || u.Upstreams[0].Name != "orders" || u.Upstreams[0].Source.Type != SourceDNS || u.Upstreams[0].Endpoints[0].Identity != "10.0.0.1:80" {
		t.Fatalf("Upstreams = %+v", u)
	}
}

func TestUpstreamsConstants(t *testing.T) {
	// Endpoint source types and breaker states.
	got := []string{SourceStatic, SourceDNS, BreakerClosed, BreakerOpen, BreakerHalfOpen}
	want := []string{"static", "dns", "closed", "open", "half_open"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("constants %v, want %v", got, want)
	}
}
