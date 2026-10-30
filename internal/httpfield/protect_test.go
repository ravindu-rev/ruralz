// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package httpfield

import (
	"net/textproto"
	"strings"
	"testing"
)

// Tests for the protected names, the fixed hop-by-hop list, the forwarding
// fields left unprotected, the trace context and framing fields protected,
// and every protected name in three casings.

var protectedNames = []struct {
	name string
	want Protection
}{
	{"host", HostField},
	{"connection", HopByHop},
	{"keep-alive", HopByHop},
	{"proxy-connection", HopByHop},
	{"te", HopByHop},
	{"trailer", HopByHop},
	{"transfer-encoding", HopByHop},
	{"upgrade", HopByHop},
	{"proxy-authenticate", HopByHop},
	{"proxy-authorization", HopByHop},
	{"content-length", Framing},
	{"expect", ExpectField},
	{"traceparent", TraceContext},
	{"tracestate", TraceContext},
}

func TestProtectThreeCasings(t *testing.T) {
	for _, tt := range protectedNames {
		for _, name := range []string{tt.name, strings.ToUpper(tt.name), textproto.CanonicalMIMEHeaderKey(tt.name)} {
			if got := Protect(name); got != tt.want {
				t.Errorf("Protect(%q) = %v, want %v", name, got, tt.want)
			}
		}
	}
}

func TestProtectPseudo(t *testing.T) {
	for _, name := range []string{":authority", ":method", ":path", ":scheme", ":status", ":protocol", ":"} {
		if got := Protect(name); got != Pseudo {
			t.Errorf("Protect(%q) = %v, want %v", name, got, Pseudo)
		}
	}
}

func TestProtectUnprotected(t *testing.T) {
	names := []string{
		// Forwarding fields are edited by U-scope Policies.
		"x-forwarded-for", "X-Forwarded-Proto", "x-forwarded-host", "Forwarded",
		"content-type", "set-cookie", "Set-Cookie", "authorization", "cookie",
		"content-encoding", "x-api-key", "baggage", "", "t", "tee", "hosts",
		"connections", "content-length2", "traceparent-x", "x-traceparent",
		// Kelvin sign: must not fold onto keep-alive.
		"Keep-alive",
		// Same length as protected names, different bytes.
		"ab", "abcd", "abcdefghij", "abcdefghijk", "proxy-connectiox",
	}
	for _, name := range names {
		if got := Protect(name); got != Unprotected {
			t.Errorf("Protect(%q) = %v, want %v", name, got, Unprotected)
		}
	}
}

func TestIsHopByHop(t *testing.T) {
	for _, tt := range protectedNames {
		if got, want := IsHopByHop(tt.name), tt.want == HopByHop; got != want {
			t.Errorf("IsHopByHop(%q) = %v, want %v", tt.name, got, want)
		}
	}
	// The fixed list, in canonical form.
	for _, name := range []string{
		"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer",
		"Transfer-Encoding", "Upgrade", "Proxy-Authenticate", "Proxy-Authorization",
	} {
		if !IsHopByHop(name) {
			t.Errorf("IsHopByHop(%q) = false", name)
		}
	}
	for _, name := range []string{"Expect", "Host", "Content-Length", "X-Foo", ":path"} {
		if IsHopByHop(name) {
			t.Errorf("IsHopByHop(%q) = true", name)
		}
	}
}

// TestIsForwardableTrailer checks the trailer rule of the data plane: every
// field crosses the Node but the hop-by-hop and framing fields, in any
// case.
func TestIsForwardableTrailer(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"Grpc-Status", true},
		{"X-Checksum", true},
		{"server-timing", true},
		{"Content-Digest", true},
		{"Content-Length", false},
		{"content-LENGTH", false},
		{"Transfer-Encoding", false},
		{"Trailer", false},
		{"TE", false},
		{"Connection", false},
		{"Keep-Alive", false},
		{"Upgrade", false},
		{"Proxy-Authorization", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsForwardableTrailer(tt.name); got != tt.want {
			t.Errorf("IsForwardableTrailer(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestProtectionStringAndMessage(t *testing.T) {
	tests := []struct {
		p    Protection
		str  string
		name string
		msg  string
	}{
		{Unprotected, "unprotected", "X-Foo", ""},
		{Pseudo, "pseudo-header", ":Path", "header :path is managed by the Node (pseudo-header)"},
		{HostField, "host", "Host", "header host is managed by the Node (host)"},
		{HopByHop, "hop-by-hop", "Transfer-Encoding", "header transfer-encoding is managed by the Node (hop-by-hop)"},
		{Framing, "framing", "content-length", "header content-length is managed by the Node (framing)"},
		{ExpectField, "expect", "Expect", "header expect is managed by the Node (expect)"},
		{TraceContext, "trace context", "traceparent", "header traceparent is managed by the Node (trace context)"},
		{Protection(99), "Protection(99)", "x", "header x is managed by the Node (Protection(99))"},
	}
	for _, tt := range tests {
		if got := tt.p.String(); got != tt.str {
			t.Errorf("%d.String() = %q, want %q", tt.p, got, tt.str)
		}
		if got := tt.p.Message(tt.name); got != tt.msg {
			t.Errorf("%v.Message(%q) = %q, want %q", tt.p, tt.name, got, tt.msg)
		}
	}
}

func TestProtectDoesNotAllocate(t *testing.T) {
	names := []string{"Traceparent", "X-Forwarded-For", "Proxy-Authorization", ":path", "Content-Type"}
	allocs := testing.AllocsPerRun(100, func() {
		for _, n := range names {
			_ = Protect(n)
			_ = IsHopByHop(n)
		}
	})
	if allocs != 0 {
		t.Errorf("allocations = %v, want 0", allocs)
	}
}
