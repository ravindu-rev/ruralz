// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
)

// Tests for spec 06 requirements 6 (identity: auth.method, auth.claims an
// empty map for non-JWT methods, consumer), 8 (challenge values), 9
// (credential stripping), 14, 24 and 41 (single field line reads), 15
// (auth.api-key default header) and 22 (principal).

func TestReq8Challenges(t *testing.T) {
	tests := []struct{ header, want string }{
		{"x-api-key", `APIKey header="x-api-key"`},
		{"X-Partner-Key", `APIKey header="X-Partner-Key"`},
		{`we"ird\name`, `APIKey header="we\"ird\\name"`},
	}
	for _, tt := range tests {
		if got := APIKeyChallenge(tt.header); got != tt.want {
			t.Errorf("APIKeyChallenge(%q) = %s, want %s", tt.header, got, tt.want)
		}
	}
	if ChallengeJWT != `Bearer realm="ruralz"` || ChallengeBasic != `Basic realm="ruralz", charset="UTF-8"` {
		t.Error("challenge constants changed")
	}
}

func TestReq15APIKeyHeader(t *testing.T) {
	empty, custom := "", "X-Partner-Key"
	tests := []struct {
		in   *string
		want string
	}{
		{nil, "x-api-key"},
		{&empty, "x-api-key"},
		{&custom, "X-Partner-Key"},
	}
	for _, tt := range tests {
		if got := APIKeyHeader(tt.in); got != tt.want {
			t.Errorf("APIKeyHeader = %q, want %q", got, tt.want)
		}
	}
}

func TestReq6Identities(t *testing.T) {
	acme := &expr.Consumer{Name: "acme"}
	claims := EmptyClaims()
	tests := []struct {
		name string
		id   *filter.Identity
		want filter.Identity
	}{
		{
			"api-key", NewIdentity(filter.MethodAPIKey, "keys", acme),
			filter.Identity{Method: "api-key", Policy: "keys", Consumer: acme, Principal: "acme"},
		},
		{
			"basic", NewIdentity(filter.MethodBasic, "basic", acme),
			filter.Identity{Method: "basic", Policy: "basic", Consumer: acme, Principal: "acme"},
		},
		{
			"jwt unbound", NewJWTIdentity("jwt", nil, claims, "https://idp", "u"),
			filter.Identity{Method: "jwt", Policy: "jwt", Principal: "jwt:https://idp#u"},
		},
		{
			"jwt bound", NewJWTIdentity("jwt", acme, claims, "https://idp", "u"),
			filter.Identity{Method: "jwt", Policy: "jwt", Consumer: acme, Principal: "acme"},
		},
		{
			"mtls unbound", NewMTLSIdentity("mtls", nil, "CN=x"),
			filter.Identity{Method: "mtls", Policy: "mtls", CertSubject: "CN=x", Principal: "mtls:CN=x"},
		},
		{
			"mtls bound", NewMTLSIdentity("mtls", acme, "CN=x"),
			filter.Identity{Method: "mtls", Policy: "mtls", Consumer: acme, CertSubject: "CN=x", Principal: "acme"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.id.Claims == nil || tt.id.Claims.IsNull() {
				t.Fatal("claims are nil or null")
			}
			got := *tt.id
			got.Claims = nil
			if got != tt.want {
				t.Fatalf("identity = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Requirement 22, subless tokens: two unbound tokens of one issuer without
// sub, differing only by client_id, never share a partition key. They get
// no principal at all (""), which a shared cache neither stores nor serves
// for; a mutual "jwt:<iss>#" key would hand one client's entries to the
// other. A subjectless certificate is treated the same way.
func TestReq22SublessIdentitiesHaveNoPrincipal(t *testing.T) {
	a := NewJWTIdentity("jwt", nil, payload{"iss": "https://idp", "client_id": "app-a"}, "https://idp", "")
	b := NewJWTIdentity("jwt", nil, payload{"iss": "https://idp", "client_id": "app-b"}, "https://idp", "")
	if a.Principal != "" || b.Principal != "" {
		t.Fatalf("subless principals = %q, %q; want none", a.Principal, b.Principal)
	}
	if NewJWTIdentity("jwt", nil, payload{"sub": "u1"}, "https://idp", "u1").Principal == "" {
		t.Fatal("a token with sub has no principal")
	}
	if p := NewMTLSIdentity("mtls", nil, "").Principal; p != "" {
		t.Fatalf("subjectless certificate principal = %q; want none", p)
	}
	acme := &expr.Consumer{Name: "acme"}
	if p := NewJWTIdentity("jwt", acme, nil, "https://idp", "").Principal; p != "acme" {
		t.Fatalf("bound subless token principal = %q; want the Consumer name", p)
	}
}

// payload is a decoded token payload stand-in.
type payload map[string]any

func (payload) IsNull() bool                          { return false }
func (payload) AppendBody(dst []byte) ([]byte, error) { return dst, nil }
func (p payload) Native() (any, error)                { return map[string]any(p), nil }

// valueClaims is a verified payload stand-in.
type valueClaims struct{ expr.Value }

func TestReq6JWTClaimsKept(t *testing.T) {
	v := valueClaims{EmptyClaims()}
	if id := NewJWTIdentity("jwt", nil, v, "i", "s"); id.Claims != v {
		t.Fatal("the verified payload was not kept")
	}
	if id := NewJWTIdentity("jwt", nil, nil, "i", "s"); id.Claims == nil {
		t.Fatal("nil claims were not replaced by the empty map")
	}
}

func TestReq6EmptyClaims(t *testing.T) {
	c := EmptyClaims()
	if c.IsNull() {
		t.Fatal("empty claims are null")
	}
	b, err := c.AppendBody([]byte("x="))
	if err != nil || string(b) != "x={}" {
		t.Fatalf("AppendBody = %q, %v", b, err)
	}
	n, err := c.Native()
	m, ok := n.(map[string]any)
	if err != nil || !ok || len(m) != 0 {
		t.Fatalf("Native = %#v, %v", n, err)
	}
	if enc, _ := json.Marshal(n); string(enc) != "{}" {
		t.Fatalf("Native encodes as %s", enc)
	}
	if a := testing.AllocsPerRun(100, func() { _ = EmptyClaims() }); a != 0 {
		t.Fatalf("EmptyClaims allocates %v times", a)
	}
}

func TestReq14Single(t *testing.T) {
	canonical := http.CanonicalHeaderKey("x-api-key")
	tests := []struct {
		name  string
		h     http.Header
		value string
		lines int
	}{
		{"absent", http.Header{"Other": {"v"}}, "", 0},
		{"one line", http.Header{canonical: {"k1"}}, "k1", 1},
		{"empty value", http.Header{canonical: {""}}, "", 1},
		{"two lines", http.Header{canonical: {"k1", "k2"}}, "k1", 2},
		{"one line with commas is one line", http.Header{canonical: {"k1, k2"}}, "k1, k2", 1},
		{"non-canonical key added by a Filter", http.Header{"x-api-key": {"k1"}}, "k1", 1},
		{"canonical and non-canonical keys", http.Header{canonical: {"k1"}, "x-API-key": {"k2"}}, "k1", 2},
		{"prefix of the name is not the name", http.Header{"X-Api-Keys": {"k1"}}, "", 0},
		{"nil header", nil, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, n := Single(tt.h, canonical)
			if v != tt.value || n != tt.lines {
				t.Fatalf("Single = %q, %d; want %q, %d", v, n, tt.value, tt.lines)
			}
		})
	}
	h := http.Header{canonical: {"k1"}, "Accept": {"*/*"}, "User-Agent": {"t"}}
	if a := testing.AllocsPerRun(100, func() { Single(h, canonical) }); a != 0 {
		t.Fatalf("Single allocates %v times", a)
	}
}

func TestReq9Strip(t *testing.T) {
	h := http.Header{
		"Authorization": {"Basic a", "Basic b"},
		"authorization": {"Basic c"},
		"X-Other":       {"kept"},
	}
	Strip(h, "Authorization")
	if len(h) != 1 || h.Get("X-Other") != "kept" {
		t.Fatalf("after Strip: %v", h)
	}
	Strip(h, "Authorization") // absent: no-op
	Strip(nil, "Authorization")
}
