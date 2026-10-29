// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
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
// schema admits, is RZ-CFG-005 offline, as Build refuses it at
// activation, so validation reports what a Node would.
func TestCheckEmptyIssuer(t *testing.T) {
	got := checkText(&v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{
		{Issuer: "", JWKSURL: "https://idp.example/jwks", Audiences: []string{audA}},
	}})
	want := []string{`policies/jwt.yaml:1:1 error RZ-CFG-005 Policy/jwt-default spec.config.issuers[issuer=""].issuer: config.issuers has an empty issuer`}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("Check = %q, want %q", got, want)
	}
}

// TestCheckReportsEveryBuildRule: Check reports every rule Build refuses,
// all of them in one run: no issuers, an issuer named twice, an issuer
// without audiences and a jwksUrl that is not https.
func TestCheckReportsEveryBuildRule(t *testing.T) {
	if got := checkText(&v1alpha1.AuthJWTConfig{}); len(got) != 1 ||
		got[0] != `policies/jwt.yaml:1:1 error RZ-CFG-005 Policy/jwt-default spec.config.issuers: config.issuers is empty` {
		t.Errorf("no issuers: Check = %q", got)
	}
	got := checkText(&v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{
		{Issuer: issA, JWKSURL: "https://idp.example/jwks"},
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

// TestBuildRefusesFirstFinding: Build's error is the first finding Check
// reports, with its code, for every config, so offline validation and
// activation never disagree.
func TestBuildRefusesFirstFinding(t *testing.T) {
	const ok = "https://idp.example/jwks"
	for _, cfg := range []*v1alpha1.AuthJWTConfig{
		{},
		{Issuers: []v1alpha1.JWTIssuer{{Issuer: "", JWKSURL: ok, Audiences: []string{audA}}}},
		{Issuers: []v1alpha1.JWTIssuer{{Issuer: issA, JWKSURL: ok, Audiences: []string{audA}}, {Issuer: issA, JWKSURL: ok, Audiences: []string{audA}}}},
		{Issuers: []v1alpha1.JWTIssuer{{Issuer: issA, JWKSURL: ok}}},
		{Issuers: []v1alpha1.JWTIssuer{{Issuer: issA, JWKSURL: "ftp://x", Audiences: []string{audA}}}},
		{Issuers: []v1alpha1.JWTIssuer{{Issuer: issA, JWKSURL: ok, Audiences: []string{audA}}}},
	} {
		err := validate(cfg)
		fs := findings(cfg)
		switch {
		case len(fs) == 0 && err != nil:
			t.Errorf("%+v: Build refuses %v, Check reports nothing", cfg, err)
		case len(fs) > 0 && err == nil:
			t.Errorf("%+v: Check reports %v, Build accepts", cfg, fs)
		case len(fs) > 0:
			if code, _ := errcode.CodeOf(err); code != fs[0].code || !strings.HasSuffix(err.Error(), fs[0].msg) {
				t.Errorf("%+v: Build refuses %v, Check's first finding is %s %s", cfg, err, fs[0].code, fs[0].msg)
			}
		}
	}
}
