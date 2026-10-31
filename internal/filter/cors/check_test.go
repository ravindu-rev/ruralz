// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cors

import (
	"context"
	"errors"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestSerializedOrigin checks which allowOrigins entries are serialized
// origins: "null" and scheme://host[:port] as browsers serialize it, with
// a lower-case scheme and host, no default port, no leading zero in the
// port, a canonical IPv4 or IPv6 address, and nothing after the port.
func TestSerializedOrigin(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"https://shop.example", true},
		{"http://localhost:8080", true},
		{"https://shop.example:8443", true},
		{"http://shop.example:0", true},
		{"https://xn--bcher-kva.example", true},
		{"https://under_score.example", true},
		{"https://10.0.0.1", true},
		{"https://[::1]", true},
		{"https://[2001:db8::1]:8443", true},
		{"https://[::ffff:102:304]", true},
		{"chrome-extension://abcdefghijklmnop", true},
		{"null", true},
		{"https://Shop.example", false},
		{"HTTPS://shop.example", false},
		{"https://shop.example/", false},
		{"https://shop.example/path", false},
		{"https://shop.example?q", false},
		{"https://shop.example#f", false},
		{"https://user@shop.example", false},
		{"https://shop.example:443", false},
		{"http://shop.example:80", false},
		{"wss://shop.example:443", false},
		{"https://shop.example:08443", false},
		{"https://shop.example:65536", false},
		{"https://shop.example:", false},
		{"https://*.shop.example", false},
		{"https://a!b.example", false},
		{"https://shop..example", false},
		{"https://.shop.example", false},
		{"https://shop.example.", false},
		{"https://", false},
		{"shop.example", false},
		{"//shop.example", false},
		{"1https://shop.example", false},
		{"https://bücher.example", false},
		{"https://shop example", false},
		{"https://1.2.3", false},
		{"https://010.0.0.1", false},
		{"https://shop.123", false},
		{"https://a.0x1f", false},
		{"https://[2001:DB8::1]", false},
		{"https://[2001:db8:0:0:0:0:0:1]", false},
		{"https://[::ffff:1.2.3.4]", false},
		{"https://[fe80::1%25eth0]", false},
		{"https://[::1", false},
		{"https://[::1]x", false},
		{"NULL", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := serializedOrigin(tt.s); got != tt.want {
			t.Errorf("serializedOrigin(%q) = %v, want %v", tt.s, got, tt.want)
		}
	}
}

// TestBuildRejectsInvalidConfig checks that Build refuses with RZ-CFG-005
// a config that is not a cors config and every finding of Check: an
// allowOrigins entry that is neither "*" nor a serialized origin; an
// allowMethods, allowHeaders or exposeHeaders entry that is not a token;
// and allowCredentials true with "*" or "null" in allowOrigins, or with
// "*" in allowHeaders or exposeHeaders, while the same lists without
// credentials build. It also stops on an ended context with the context's
// error, which carries no code.
func TestBuildRejectsInvalidConfig(t *testing.T) {
	yes := true
	tests := []struct {
		name string
		cfg  any
	}{
		{"nil", nil},
		{"not a cors config", &v1alpha1.AuthzIPConfig{}},
		{"nil cors config", (*v1alpha1.CORSConfig)(nil)},
		{"origin with a path", &v1alpha1.CORSConfig{AllowOrigins: []string{"https://shop.example/"}}},
		{"origin in upper case", &v1alpha1.CORSConfig{AllowOrigins: []string{"https://Shop.example"}}},
		{"credentials with any origin", &v1alpha1.CORSConfig{AllowOrigins: []string{"*"}, AllowCredentials: &yes}},
		{"credentials with the null origin", &v1alpha1.CORSConfig{AllowOrigins: []string{"https://shop.example", "null"}, AllowCredentials: &yes}},
		{"credentials with any header", &v1alpha1.CORSConfig{AllowOrigins: []string{"https://shop.example"}, AllowHeaders: []string{"*"}, AllowCredentials: &yes}},
		{"credentials exposing every header", &v1alpha1.CORSConfig{AllowOrigins: []string{"https://shop.example"}, ExposeHeaders: []string{"*"}, AllowCredentials: &yes}},
		{"method not a token", &v1alpha1.CORSConfig{AllowMethods: []string{"GET POST"}}},
		{"header not a token", &v1alpha1.CORSConfig{AllowHeaders: []string{"x api"}}},
		{"exposed header not a token", &v1alpha1.CORSConfig{ExposeHeaders: []string{"a:b"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Factory{}.Build(t.Context(), filter.BuildEnv{Config: tt.cfg})
			if code, ok := errcode.CodeOf(err); f != nil || !ok || code != CodeConfig {
				t.Errorf("Build = %v, %v; want an %s error", f, err, CodeConfig)
			}
		})
	}
	for _, cfg := range []*v1alpha1.CORSConfig{
		{AllowOrigins: []string{"*"}, AllowHeaders: []string{"*"}, ExposeHeaders: []string{"*"}},
		{AllowOrigins: []string{"https://shop.example", "null"}},
		{AllowOrigins: []string{"https://shop.example"}, AllowHeaders: []string{"Authorization"}, ExposeHeaders: []string{"RateLimit"}, AllowCredentials: &yes},
	} {
		if _, err := (Factory{}).Build(t.Context(), filter.BuildEnv{Config: cfg}); err != nil {
			t.Errorf("Build(%+v) = %v, want a Filter", cfg, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Factory{}.Build(ctx, filter.BuildEnv{Config: &v1alpha1.CORSConfig{}})
	if _, coded := errcode.CodeOf(err); !errors.Is(err, context.Canceled) || coded {
		t.Errorf("Build with a canceled context = %v, want context.Canceled without a code", err)
	}
}

// TestCheckReportsFindings checks that the offline check reports every
// finding as RZ-CFG-005 at its path under spec.config, located through
// loc, and nothing for a valid config, another config type, a nil
// resource or a nil add.
func TestCheckReportsFindings(t *testing.T) {
	yes := true
	cfg := &v1alpha1.CORSConfig{
		AllowOrigins:     []string{"https://shop.example", "*", "null", "https://shop.example:443"},
		AllowMethods:     []string{"GET", "GET POST"},
		AllowHeaders:     []string{"x-api-key", "x api", "*"},
		ExposeHeaders:    []string{"a:b", "*"},
		AllowCredentials: &yes,
	}
	res := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: "cors-default"}, Config: cfg}
	var got []diag.Diagnostic
	loc := func(p diag.Path) diag.Location { return diag.Location{File: "policies.yaml", Line: len(p)} }
	Check(res, loc, func(d diag.Diagnostic) { got = append(got, d) })
	want := []struct{ path, message string }{
		{"spec.config.allowOrigins[item=https://shop.example:443]", `neither "*" nor a serialized origin such as https://shop.example`},
		{"spec.config.allowMethods[item=\"GET POST\"]", "not a method"},
		{"spec.config.allowHeaders[item=\"x api\"]", "not a field name"},
		{"spec.config.exposeHeaders[item=a:b]", "not a field name"},
		{"spec.config.allowCredentials", `true with "*" in allowOrigins`},
		{"spec.config.allowCredentials", `true with "null" in allowOrigins`},
		{"spec.config.allowCredentials", `true with "*" in allowHeaders`},
		{"spec.config.allowCredentials", `true with "*" in exposeHeaders`},
	}
	if len(got) != len(want) {
		t.Fatalf("Check reported %d diagnostics, want %d: %+v", len(got), len(want), got)
	}
	for i, d := range got {
		if d.Code != CodeConfig || d.Severity != diag.SeverityError || d.Path.String() != want[i].path || d.Message != want[i].message {
			t.Errorf("diagnostic %d = %s %v at %s %q, want %s at %s %q", i, d.Code, d.Severity, d.Path, d.Message, CodeConfig, want[i].path, want[i].message)
		}
		if d.Resource == nil || d.Resource.Name != "cors-default" || d.Line != len(d.Path) || d.File != "policies.yaml" {
			t.Errorf("diagnostic %d resource %+v, location %+v; want the Policy and loc's location", i, d.Resource, d.Location)
		}
	}
	var n int
	count := func(diag.Diagnostic) { n++ }
	Check(&hub.Resource{Config: &v1alpha1.CORSConfig{AllowOrigins: []string{"https://shop.example"}, AllowMethods: []string{"GET"}}}, nil, count)
	Check(&hub.Resource{Config: &v1alpha1.AuthzIPConfig{Allow: []string{"x"}}}, nil, count)
	Check(nil, nil, count)
	Check(res, nil, nil)
	if n != 0 {
		t.Errorf("Check reported %d diagnostics for valid or foreign input, want 0", n)
	}
}
