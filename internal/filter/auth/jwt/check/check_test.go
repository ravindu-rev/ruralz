// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"errors"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

const (
	issA   = "https://idp.example"
	audA   = "shop-api"
	policy = "jwt-default"
	okURL  = "https://idp.example/jwks"
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

// checkText runs Check on cfg and returns the text form of its findings,
// each located at line 1 of policies/jwt.yaml.
func checkText(cfg *v1alpha1.AuthJWTConfig) []string {
	r := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: policy}, Config: cfg}
	var out []string
	Check(r, func(diag.Path) diag.Location { return diag.Location{File: "policies/jwt.yaml", Line: 1, Column: 1} },
		func(d diag.Diagnostic) { out = append(out, string(d.AppendText(nil))) })
	return out
}

// TestCheckEmptyIssuer: an issuer that is the empty string, which the
// schema admits, is RZ-CFG-005 offline, as the Factory refuses it at
// activation, so validation reports what a Node would.
func TestCheckEmptyIssuer(t *testing.T) {
	got := checkText(&v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{
		{Issuer: "", JWKSURL: okURL, Audiences: []string{audA}},
	}})
	want := []string{`policies/jwt.yaml:1:1 error RZ-CFG-005 Policy/jwt-default spec.config.issuers[issuer=""].issuer: config.issuers has an empty issuer`}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("Check = %q, want %q", got, want)
	}
}

// TestCheckReportsEveryRule: Check reports every rule the Factory refuses,
// all of them in one run: no issuers, an issuer named twice, an issuer
// without audiences and a jwksUrl that is not https.
func TestCheckReportsEveryRule(t *testing.T) {
	if got := checkText(&v1alpha1.AuthJWTConfig{}); len(got) != 1 ||
		got[0] != `policies/jwt.yaml:1:1 error RZ-CFG-005 Policy/jwt-default spec.config.issuers: config.issuers is empty` {
		t.Errorf("no issuers: Check = %q", got)
	}
	got := checkText(&v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{
		{Issuer: issA, JWKSURL: okURL},
		{Issuer: issA, JWKSURL: "http://idp.example/jwks", Audiences: []string{audA}},
	}})
	want := []string{
		`policies/jwt.yaml:1:1 error RZ-CFG-005 Policy/jwt-default spec.config.issuers[issuer=https://idp.example].audiences: issuer "https://idp.example" has no audiences`,
		`policies/jwt.yaml:1:1 error RZ-CFG-005 Policy/jwt-default spec.config.issuers[issuer=https://idp.example]: issuer "https://idp.example" is listed twice`,
		`policies/jwt.yaml:1:1 error RZ-CFG-037 Policy/jwt-default spec.config.issuers[issuer=https://idp.example].jwksUrl: jwksUrl is not an https URL`,
	}
	if len(got) != len(want) {
		t.Fatalf("Check = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding %d = %s, want %s", i, got[i], want[i])
		}
	}
}

// TestFindingsJWKSURL covers the jwksUrl rule (TB-10) on every form it
// refuses, and the forms it accepts: an upper-case scheme reads as https.
func TestFindingsJWKSURL(t *testing.T) {
	tests := []struct {
		name, url, msg string
	}{
		{"https", okURL, ""},
		{"upper-case scheme", "HTTPS://idp.example/jwks", ""},
		{"port and query", "https://idp.example:8443/jwks?v=1", ""},
		{"http", "http://idp.example/jwks", "jwksUrl is not an https URL"},
		{"no scheme", "idp.example/jwks", "jwksUrl is not an https URL"},
		{"opaque", "https:idp.example", "jwksUrl is not an https URL"},
		{"no host", "https:///jwks", "jwksUrl has no host"},
		{"port only", "https://:443/jwks", "jwksUrl has no host"},
		{"not a URL", "https://[::1", "jwksUrl is not a URL"},
		{"empty", "", "jwksUrl is not an https URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := Findings(&v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{{Issuer: issA, JWKSURL: tt.url, Audiences: []string{audA}}}})
			switch {
			case tt.msg == "" && len(fs) != 0:
				t.Errorf("Findings(%q) = %+v, want none", tt.url, fs)
			case tt.msg != "" && (len(fs) != 1 || fs[0].Code != CodeHTTPS || fs[0].Message != tt.msg):
				t.Errorf("Findings(%q) = %+v, want one %s %q", tt.url, fs, CodeHTTPS, tt.msg)
			}
		})
	}
}

// TestFindingErr: the error the Factory refuses a config with carries the
// finding's code and names the rule; a jwksUrl error names the issuer and
// wraps the cause, so errors.Is reaches it.
func TestFindingErr(t *testing.T) {
	fs := Findings(&v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{{Issuer: issA, JWKSURL: "http://x", Audiences: []string{audA}}}})
	if len(fs) != 1 {
		t.Fatalf("Findings = %+v, want one", fs)
	}
	err := fs[0].Err()
	if code, _ := errcode.CodeOf(err); code != CodeHTTPS || !errors.Is(err, fs[0].Cause) ||
		!strings.Contains(err.Error(), `issuer "https://idp.example"`) {
		t.Errorf("Err() = %v (code %q)", err, code)
	}
	empty := Findings(&v1alpha1.AuthJWTConfig{})[0].Err()
	if code, _ := errcode.CodeOf(empty); code != CodeSchema || !strings.HasSuffix(empty.Error(), "auth.jwt: config.issuers is empty") {
		t.Errorf("Err() = %v (code %q)", empty, code)
	}
	if fs := Findings(nil); fs != nil {
		t.Errorf("Findings(nil) = %+v, want nil", fs)
	}
}
