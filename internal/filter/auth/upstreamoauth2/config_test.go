// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamoauth2

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// validConfig returns a config that passes every check.
func validConfig(tokenURL string) *v1alpha1.AuthUpstreamOAuth2Config {
	return &v1alpha1.AuthUpstreamOAuth2Config{
		TokenURL:     tokenURL,
		ClientID:     testClientID,
		ClientSecret: v1alpha1.SecretValue{SecretRef: testRef()},
	}
}

// policyResource returns a validated Policy resource holding cfg.
func policyResource(cfg any) *hub.Resource {
	return &hub.Resource{ID: tree.ID{Kind: v1alpha1.KindPolicy, Name: testPolicy}, Config: cfg}
}

// fakeLocation maps a path to a location whose line is the path length.
func fakeLocation(p diag.Path) diag.Location {
	return diag.Location{File: "policies/token.yaml", Line: len(p), Column: 5}
}

// runCheck runs Check on cfg and returns its diagnostics.
func runCheck(cfg any) diag.List {
	var out diag.List
	Check(policyResource(cfg), fakeLocation, func(d diag.Diagnostic) { out = append(out, d) })
	return out
}

func duration(d time.Duration) *v1alpha1.Duration {
	v := v1alpha1.Duration(d)
	return &v
}

// TestCheckTokenURL checks that a tokenUrl that is not an absolute https
// URL is RZ-CFG-037 at spec.config.tokenUrl (Security and identity,
// "Transport security": tokenUrl MUST be https), reported offline, and
// that an https one passes.
func TestCheckTokenURL(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		bad       bool
	}{
		{"https", "https://auth.shop.example/oauth2/token", false},
		{"https with port and query", "https://auth.shop.example:8443/token?tenant=a", false},
		{"upper-case scheme", "HTTPS://auth.shop.example/token", false},
		{"http", "http://auth.shop.example/oauth2/token", true},
		{"ftp", "ftp://auth.shop.example/token", true},
		{"scheme-relative", "//auth.shop.example/token", true},
		{"relative path", "/oauth2/token", true},
		{"no host", "https:///oauth2/token", true},
		{"opaque", "https:auth.shop.example", true},
		{"empty", "", true},
		{"unparsable", "https://auth.shop.example/%zz", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runCheck(validConfig(tc.url))
			if !tc.bad {
				if len(got) != 0 {
					t.Fatalf("Check(%q) = %+v, want no finding", tc.url, got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("Check(%q) = %+v, want one finding", tc.url, got)
			}
			d := got[0]
			if d.Code != CodeNotHTTPS || d.Severity != diag.SeverityError || d.Path.String() != "spec.config.tokenUrl" {
				t.Errorf("Check(%q) = %s %v %s, want %s error spec.config.tokenUrl", tc.url, d.Code, d.Severity, d.Path, CodeNotHTTPS)
			}
			if d.Resource == nil || *d.Resource != (diag.ResourceID{Kind: "Policy", Name: testPolicy}) {
				t.Errorf("Check(%q) resource = %v", tc.url, d.Resource)
			}
			if d.Location != fakeLocation(d.Path) {
				t.Errorf("Check(%q) location = %+v, not the one loc returned", tc.url, d.Location)
			}
			// The Node refuses the same config with the same code.
			if _, err := settingsOf(validConfig(tc.url)); !hasCode(err, CodeNotHTTPS) {
				t.Errorf("settingsOf(%q) = %v, want %s", tc.url, err, CodeNotHTTPS)
			}
		})
	}
}

// TestCheckTimeout checks that a config.timeout that is not positive is
// RZ-CFG-005 at spec.config.timeout, since such a bound ends every fetch
// before it starts, and that an unset or positive one passes.
func TestCheckTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout *v1alpha1.Duration
		bad     bool
	}{
		{"unset", nil, false},
		{"one millisecond", duration(time.Millisecond), false},
		{"zero", duration(0), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig("https://auth.shop.example/token")
			cfg.Timeout = tc.timeout
			got := runCheck(cfg)
			_, err := settingsOf(cfg)
			if !tc.bad {
				if len(got) != 0 || err != nil {
					t.Fatalf("Check = %+v, settingsOf = %v; want neither to refuse", got, err)
				}
				return
			}
			if len(got) != 1 || got[0].Code != CodeConfig || got[0].Path.String() != "spec.config.timeout" {
				t.Fatalf("Check = %+v, want one %s at spec.config.timeout", got, CodeConfig)
			}
			if !hasCode(err, CodeConfig) {
				t.Errorf("settingsOf = %v, want %s", err, CodeConfig)
			}
		})
	}
}

// TestCheckIgnoresOtherResources checks that Check reports nothing for a
// resource that is not a decoded auth.upstream-oauth2 config, and
// tolerates a nil resource, location function or sink.
func TestCheckIgnoresOtherResources(t *testing.T) {
	if got := runCheck(&v1alpha1.CORSConfig{}); len(got) != 0 {
		t.Errorf("Check(cors config) = %+v, want nothing", got)
	}
	if got := runCheck((*v1alpha1.AuthUpstreamOAuth2Config)(nil)); len(got) != 0 {
		t.Errorf("Check(nil config) = %+v, want nothing", got)
	}
	Check(nil, fakeLocation, func(diag.Diagnostic) { t.Error("finding for a nil resource") })
	Check(policyResource(validConfig("http://x")), fakeLocation, nil)
	var got diag.List
	Check(policyResource(validConfig("http://x")), nil, func(d diag.Diagnostic) { got = append(got, d) })
	if len(got) != 1 || got[0].Location != (diag.Location{}) {
		t.Errorf("Check without loc = %+v, want one finding without a location", got)
	}
}

// TestSettings checks the derived fetch parameters: config.timeout
// defaults to 2 s (target), the clientSecret destination is the tokenUrl
// origin with its port explicit, and scopes become one scope parameter in
// order without duplicates or empty ones.
func TestSettings(t *testing.T) {
	cfg := validConfig("https://Auth.Shop.Example/oauth2/token")
	cfg.Scopes = []string{"orders.read", "", "orders.write", "orders.read"}
	s, err := settingsOf(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := settings{
		endpoint: "https://Auth.Shop.Example/oauth2/token",
		origin:   "https://auth.shop.example:443",
		clientID: testClientID,
		ref:      secret.RefOf(testRef()),
		scope:    "orders.read orders.write",
		timeout:  2 * time.Second,
	}
	if s != want {
		t.Errorf("settingsOf = %+v, want %+v", s, want)
	}
	for _, tc := range []struct {
		scopes []string
		want   string
	}{
		{nil, ""},
		{[]string{""}, ""},
		{[]string{"a"}, "a"},
		{[]string{"b", "a", "b"}, "b a"},
	} {
		if got := scopeOf(tc.scopes); got != tc.want {
			t.Errorf("scopeOf(%q) = %q, want %q", tc.scopes, got, tc.want)
		}
	}
}

// TestBuildRefuses checks what the Factory refuses: a config of the wrong
// type and a non-positive timeout (RZ-CFG-005), a tokenUrl that is not
// https (RZ-CFG-037), a clientSecret the Revision's secret table does not
// hold (RZ-CFG-026); and the failures of the compile itself, an ended
// context and a Factory without a Registry, which carry no code.
func TestBuildRefuses(t *testing.T) {
	reg := NewRegistry(Options{})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	ok := validConfig("https://auth.shop.example/token")
	zero := validConfig("https://auth.shop.example/token")
	zero.Timeout = duration(0)
	for _, tc := range []struct {
		name string
		ctx  context.Context
		fa   Factory
		env  filter.BuildEnv
		code string
		is   error
	}{
		{"wrong config type", t.Context(), Factory{reg}, filter.BuildEnv{Config: &v1alpha1.CORSConfig{}, Secrets: newSecrets("x")}, CodeConfig, nil},
		{"nil config", t.Context(), Factory{reg}, filter.BuildEnv{Config: (*v1alpha1.AuthUpstreamOAuth2Config)(nil), Secrets: newSecrets("x")}, CodeConfig, nil},
		{"http tokenUrl", t.Context(), Factory{reg}, filter.BuildEnv{Config: validConfig("http://auth.shop.example/token"), Secrets: newSecrets("x")}, CodeNotHTTPS, errTokenURL},
		{"zero timeout", t.Context(), Factory{reg}, filter.BuildEnv{Config: zero, Secrets: newSecrets("x")}, CodeConfig, errTimeout},
		{"no secret table", t.Context(), Factory{reg}, filter.BuildEnv{Config: ok}, CodeUnresolved, nil},
		{"secret not resolved", t.Context(), Factory{reg}, filter.BuildEnv{Config: ok, Secrets: &fakeSecrets{vals: map[secret.Ref]secret.Value{}}}, CodeUnresolved, nil},
		{"ended context", canceled, Factory{reg}, filter.BuildEnv{Config: ok, Secrets: newSecrets("x")}, "", context.Canceled},
		{"no registry", t.Context(), Factory{}, filter.BuildEnv{Config: ok, Secrets: newSecrets("x")}, "", errNoRegistry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := tc.fa.Build(tc.ctx, tc.env)
			if err == nil || f != nil {
				t.Fatalf("Build = %v, %v; want an error", f, err)
			}
			code, coded := errcode.CodeOf(err)
			if code != tc.code || coded != (tc.code != "") {
				t.Errorf("Build error %v has code %q, want %q", err, code, tc.code)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("Build error = %v, want %v", err, tc.is)
			}
		})
	}
	if n := len(reg.list()); n != 0 {
		t.Errorf("refused Builds left %d entries", n)
	}
}

// hasCode reports whether err carries code.
func hasCode(err error, code string) bool {
	c, ok := errcode.CodeOf(err)
	return ok && c == code
}
