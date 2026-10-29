// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/phase"
)

// TestClaimsValue checks auth.claims as an expr.Value: an object that is
// never null, written as JSON with sorted members, whose Native form is a
// copy a caller may edit without changing the identity.
func TestClaimsValue(t *testing.T) {
	fx := newFixture(t, options{})
	x := bearer(mint(t, fx.keys.es, nil, map[string]any{"scope": "orders:read", "nested": map[string]any{"b": 1, "a": []any{true}}}))
	id := wantAllow(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x)
	if id.Claims.IsNull() {
		t.Fatal("claims are null")
	}
	body, err := id.Claims.AppendBody(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"aud":"shop-api","exp":` + itoa(t0.Add(time.Hour).Unix()) + `,"iat":` + itoa(t0.Unix()) +
		`,"iss":"https://idp.example","nested":{"a":[true],"b":1},"scope":"orders:read","sub":"user-1"}`
	if string(body) != want {
		t.Fatalf("AppendBody = %s, want %s", body, want)
	}
	n, err := id.Claims.Native()
	if err != nil {
		t.Fatal(err)
	}
	m, ok := n.(map[string]any)
	if !ok || m["scope"] != "orders:read" || m["exp"] != json.Number(itoa(t0.Add(time.Hour).Unix())) {
		t.Fatalf("Native = %#v", n)
	}
	m["scope"] = "admin"
	m["nested"].(map[string]any)["b"] = json.Number("2")
	again, _ := id.Claims.AppendBody(nil)
	if string(again) != want {
		t.Fatal("editing the Native form changed the identity's claims")
	}
}

// TestClaimsLookups checks the identity.Claims view Consumer binding
// reads: string claims only for StringClaim, any member for HasClaim.
func TestClaimsLookups(t *testing.T) {
	c := claims{"sub": "u", "n": json.Number("1"), "null": nil}
	if s, ok := c.StringClaim("sub"); !ok || s != "u" {
		t.Fatalf("StringClaim(sub) = %q, %v", s, ok)
	}
	for _, name := range []string{"n", "null", "absent"} {
		if _, ok := c.StringClaim(name); ok {
			t.Errorf("StringClaim(%s) found a string", name)
		}
	}
	if !c.HasClaim("null") || !c.HasClaim("n") || c.HasClaim("absent") {
		t.Fatal("HasClaim")
	}
}

// TestIssuedAt checks the iat passed to the Revoker: a NumericDate with
// its fraction, else the zero time ("iat is not checked").
func TestIssuedAt(t *testing.T) {
	tests := []struct {
		name string
		iat  any
		want time.Time
	}{
		{"integer", json.Number("1790000000"), time.Unix(1790000000, 0)},
		{"fraction", json.Number("1790000000.25"), time.Unix(1790000000, 250000000)},
		{"absent", nil, time.Time{}},
		{"string", "1790000000", time.Time{}},
		{"out of range", json.Number("1e300"), time.Time{}},
		{"overflow", json.Number("1e999"), time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := claims{}
			if tt.iat != nil {
				c["iat"] = tt.iat
			}
			if got := c.issuedAt(); !got.Equal(tt.want) {
				t.Errorf("issuedAt(%v) = %v, want %v", tt.iat, got, tt.want)
			}
		})
	}
}

// TestBearerToken checks the Authorization parse: the Bearer scheme in any
// case, then one or more spaces and the token; any other scheme is no
// bearer credential.
func TestBearerToken(t *testing.T) {
	tests := []struct {
		value, tok string
		bearer     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"Bearer   abc  ", "abc", true},
		{"\tBearer abc\t", "abc", true},
		{"Bearer", "", true},
		{"Bearer ", "", true},
		{"Bearer a b", "a b", true},
		{"Bearer\tabc", "", false},
		{"Basic abc", "", false},
		{"Bearerabc", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		tok, bearer := bearerToken(tt.value)
		if tok != tt.tok || bearer != tt.bearer {
			t.Errorf("bearerToken(%q) = %q, %v, want %q, %v", tt.value, tok, bearer, tt.tok, tt.bearer)
		}
	}
}
