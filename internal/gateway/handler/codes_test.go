// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/executor"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// TestStatusOf checks the status of a Node-generated response: the
// registered one, 503 for the end codes, 500 for a fault, 502 for an
// Upstream code without a single status.
func TestStatusOf(t *testing.T) {
	h := newKit(t).h
	for code, want := range map[string]int{
		codeNoRoute: 404, codeHeaderTooLarge: 431, codeBodyTooLarge: 413, codeFull: 503, codeWhen: 500,
		codeGrace: 503, codeDrain: 503, codeRejected: 400, codeWebSocket: 400, codeFault: 500,
		codeUpstreamTimeout: 504, codeUpstreamReset: 502, codeAfterCommit: 502, "RZ-UP-010": 502,
		"RZ-AUTH-001": 401, "RZ-RL-001": 429,
	} {
		if got := h.statusOf(code); got != want {
			t.Errorf("statusOf(%s) = %d, want %d", code, got, want)
		}
	}
}

// TestStatusTable checks that the table holds every registered code with a
// single status, and only those.
func TestStatusTable(t *testing.T) {
	m := statusTable()
	for _, c := range errcode.All() {
		if got, ok := m[c.ID]; ok != (c.Status != 0) || got != c.Status {
			t.Errorf("statusTable()[%s] = %d (present %t), want %d", c.ID, got, ok, c.Status)
		}
	}
}

// TestKindOf checks which Node-generated responses are Upstream failures
// the Response Cache may replace.
func TestKindOf(t *testing.T) {
	if kindOf("RZ-UP-005") != executor.ResponseUpstreamError || kindOf("RZ-RT-007") != executor.ResponseGenerated {
		t.Error("kindOf: an RZ-UP code is an Upstream failure, any other generated")
	}
}

// TestViaEntryAndProtocol checks the client hop's Via entry and protocol
// labels.
func TestViaEntryAndProtocol(t *testing.T) {
	tests := []struct {
		major, minor int
		via, label   string
		index        int
	}{
		{1, 0, "1.0 ruralz", protoHTTP1, emit.ProtoHTTP1},
		{1, 1, "1.1 ruralz", protoHTTP1, emit.ProtoHTTP1},
		{2, 0, "2 ruralz", protoHTTP2, emit.ProtoHTTP2},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ProtoMajor, r.ProtoMinor = tt.major, tt.minor
		if got := viaEntry(r); got != tt.via {
			t.Errorf("viaEntry(HTTP/%d.%d) = %q, want %q", tt.major, tt.minor, got, tt.via)
		}
		if protoLabel(r) != tt.label || protoIndex(r) != tt.index {
			t.Errorf("protocol of HTTP/%d.%d = %s %d, want %s %d", tt.major, tt.minor, protoLabel(r), protoIndex(r), tt.label, tt.index)
		}
	}
}

// TestUpgradesToWebSocket checks the upgrade test: the websocket token in
// any case, in a list, with a version.
func TestUpgradesToWebSocket(t *testing.T) {
	for v, want := range map[string]bool{
		"websocket": true, "WebSocket": true, "h2c, websocket": true, "websocket/13": true,
		"h2c": false, "": false, "websockets": false,
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if v != "" {
			r.Header.Set("Upgrade", v)
		}
		if got := upgradesToWebSocket(r); got != want {
			t.Errorf("upgradesToWebSocket(%q) = %t, want %t", v, got, want)
		}
	}
}

// TestBodyAllowed checks the responses that carry content.
func TestBodyAllowed(t *testing.T) {
	tests := []struct {
		method string
		status int
		want   bool
	}{
		{http.MethodGet, 200, true}, {http.MethodHead, 200, false}, {http.MethodGet, 103, false},
		{http.MethodGet, 204, false}, {http.MethodGet, 304, false}, {http.MethodPost, 500, true},
	}
	for _, tt := range tests {
		if got := bodyAllowed(tt.method, tt.status); got != tt.want {
			t.Errorf("bodyAllowed(%s, %d) = %t, want %t", tt.method, tt.status, got, tt.want)
		}
	}
}

// TestFlushes checks the flush rule's input: no Content-Length, or an
// event stream in any case and with parameters.
func TestFlushes(t *testing.T) {
	for name, tt := range map[string]struct {
		h    http.Header
		want bool
	}{
		"no length":    {http.Header{}, true},
		"length":       {http.Header{"Content-Length": {"3"}}, false},
		"event stream": {http.Header{"Content-Length": {"3"}, "Content-Type": {" text/EVENT-stream ;charset=utf-8"}}, true},
		"json":         {http.Header{"Content-Length": {"3"}, "Content-Type": {"application/json"}}, false},
	} {
		if got := flushes(tt.h); got != tt.want {
			t.Errorf("flushes(%s) = %t, want %t", name, got, tt.want)
		}
	}
}

// TestRecordHelpers checks the access record's client_address and
// tls_version forms.
func TestRecordHelpers(t *testing.T) {
	peer := filter.Source{IP: netip.MustParseAddr("192.0.2.10")}
	header := filter.Source{IP: netip.MustParseAddr("2001:db8::1"), FromHeader: true}
	for _, tt := range []struct {
		src    filter.Source
		remote string
		want   string
	}{
		{peer, "192.0.2.10:40000", "192.0.2.10"},
		{header, "192.0.2.10:40000", "2001:db8::1"},
		{filter.Source{}, "192.0.2.10:40000", ""},
		{peer, "garbage", "192.0.2.10"},
	} {
		if got := addrText(tt.src, tt.remote); got != tt.want {
			t.Errorf("addrText(%v, %q) = %q, want %q", tt.src, tt.remote, got, tt.want)
		}
	}
	for v, want := range map[uint16]string{tls.VersionTLS13: "1.3", tls.VersionTLS12: "1.2", tls.VersionTLS11: ""} {
		if got := tlsVersion(&tls.ConnectionState{Version: v}); got != want {
			t.Errorf("tlsVersion(%x) = %q, want %q", v, got, want)
		}
	}
	if tlsVersion(nil) != "" {
		t.Error("tlsVersion(nil) is not empty")
	}
}

// TestSameAuthority checks the HTTP/2 Host comparison: lowercased, the
// scheme's default port dropped.
func TestSameAuthority(t *testing.T) {
	for _, tt := range []struct {
		a, b, scheme string
		want         bool
	}{
		{"API.example:443", "api.example", "https", true},
		{"api.example:80", "api.example", "https", false},
		{"api.example:80", "api.example", "http", true},
		{"other.example", "api.example", "http", false},
	} {
		if got := sameAuthority(tt.a, tt.b, tt.scheme); got != tt.want {
			t.Errorf("sameAuthority(%q, %q, %s) = %t, want %t", tt.a, tt.b, tt.scheme, got, tt.want)
		}
	}
}

// TestDateCache checks the Date value: HTTP date format in UTC, one value
// per second.
func TestDateCache(t *testing.T) {
	var d dateCache
	at := time.Date(2026, 10, 1, 14, 0, 0, 100, time.FixedZone("x", 2*3600))
	v := d.get(at)
	if v[0] != "Thu, 01 Oct 2026 12:00:00 GMT" {
		t.Errorf("Date = %q", v[0])
	}
	if w := d.get(at.Add(500 * time.Millisecond)); &w[0] != &v[0] {
		t.Error("a second Date value within the same second")
	}
	if w := d.get(at.Add(time.Second)); w[0] != "Thu, 01 Oct 2026 12:00:01 GMT" {
		t.Errorf("next second Date = %q", w[0])
	}
}
