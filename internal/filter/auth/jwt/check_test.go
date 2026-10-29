// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestCheckJWKSURL checks the offline rule every binary runs: an
// issuers[].jwksUrl that is not an https URL with a host is RZ-CFG-037 at
// spec.config.issuers[issuer=<issuer>].jwksUrl, located through loc, and
// an https one yields nothing.
func TestCheckJWKSURL(t *testing.T) {
	cfg := &v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{
		{Issuer: "https://good.example", JWKSURL: "https://good.example/jwks", Audiences: []string{audA}},
		{Issuer: "https://plain.example", JWKSURL: "http://plain.example/jwks", Audiences: []string{audA}},
		{Issuer: "tenant one", JWKSURL: "/jwks", Audiences: []string{audA}},
	}}
	r := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: policy}, Config: cfg}
	var got diag.List
	var asked []string
	loc := func(p diag.Path) diag.Location {
		asked = append(asked, p.String())
		return diag.Location{File: "policies/jwt.yaml", Line: 9, Column: 7}
	}
	Check(r, loc, func(d diag.Diagnostic) { got = append(got, d) })
	want := []string{
		`policies/jwt.yaml:9:7 error RZ-CFG-037 Policy/jwt-default spec.config.issuers[issuer=https://plain.example].jwksUrl: jwksUrl is not an https URL`,
		`policies/jwt.yaml:9:7 error RZ-CFG-037 Policy/jwt-default spec.config.issuers[issuer="tenant one"].jwksUrl: jwksUrl is not an https URL`,
	}
	if len(got) != len(want) || len(asked) != len(want) {
		t.Fatalf("Check produced %d diagnostics (%d locations), want %d", len(got), len(asked), len(want))
	}
	for i, d := range got {
		if s := string(d.AppendText(nil)); s != want[i] {
			t.Errorf("diagnostic %d = %s, want %s", i, s, want[i])
		}
	}
}

// TestCheckIgnoresOtherResources checks that Check yields nothing for a
// resource without a decoded auth.jwt config, and accepts a nil location
// mapper.
func TestCheckIgnoresOtherResources(t *testing.T) {
	add := func(d diag.Diagnostic) { t.Errorf("unexpected diagnostic %v", d) }
	Check(nil, nil, add)
	Check(&hub.Resource{Config: &v1alpha1.AuthAPIKeyConfig{}}, nil, add)
	Check(&hub.Resource{Config: (*v1alpha1.AuthJWTConfig)(nil)}, nil, add)
	Check(&hub.Resource{}, nil, nil)
	n := 0
	bad := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: policy}, Config: &v1alpha1.AuthJWTConfig{
		Issuers: []v1alpha1.JWTIssuer{{Issuer: issA, JWKSURL: "ftp://x", Audiences: []string{audA}}},
	}}
	Check(bad, nil, func(d diag.Diagnostic) {
		n++
		if d.Code != CodeHTTPS || d.Location != (diag.Location{}) {
			t.Errorf("diagnostic %+v", d)
		}
	})
	if n != 1 {
		t.Fatalf("diagnostics = %d, want 1", n)
	}
}
