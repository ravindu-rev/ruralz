// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Tests for spec 06 requirement 22: every identity exposes a non-secret
// principal key: the Consumer name when bound, else "jwt:" + iss + "#" +
// sub, else "mtls:" + subject; an unbound identity without a subject has
// none ("": a shared cache neither stores nor serves for it).

func TestReq22Principal(t *testing.T) {
	acme := &expr.Consumer{Name: "acme"}
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"bound consumer", ConsumerPrincipal(acme), "acme"},
		{"no consumer", ConsumerPrincipal(nil), ""},
		{"jwt bound", JWTPrincipal(acme, "https://idp", "u1"), "acme"},
		{"jwt unbound", JWTPrincipal(nil, "https://idp", "u1"), "jwt:https://idp#u1"},
		{"jwt without sub", JWTPrincipal(nil, "https://idp", ""), ""},
		{"jwt bound without sub", JWTPrincipal(acme, "https://idp", ""), "acme"},
		{"jwt issuer with #", JWTPrincipal(nil, "a#b", "c"), "jwt:a%23b#c"},
		{"jwt issuer with %", JWTPrincipal(nil, "a%23b", "c"), "jwt:a%2523b#c"},
		{"mtls bound", MTLSPrincipal(acme, "CN=x"), "acme"},
		{"mtls unbound", MTLSPrincipal(nil, "CN=x,O=Acme"), "mtls:CN=x,O=Acme"},
		{"mtls unbound without subject", MTLSPrincipal(nil, ""), ""},
		{"mtls bound without subject", MTLSPrincipal(acme, ""), "acme"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, tt.got, tt.want)
		}
	}
	// Distinct (iss, sub) pairs never share a principal.
	if JWTPrincipal(nil, "a#b", "c") == JWTPrincipal(nil, "a", "b#c") {
		t.Error("principal encoding is not injective")
	}
}
