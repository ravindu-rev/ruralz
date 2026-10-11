// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestFixtures runs stage I over one negative fixture per case of 02 test
// 16 and compares the diagnostics, in their text form with file, line and
// column, with the fixture's want.txt (TQ "Conformance suites"). A
// fixture's YAML files replace the files of the same path in its base:
// testdata/fixtures/common, a library of unattached Policies and Plugins
// with a Gateway, an Upstream and a Route, or the example Bundle.
func TestFixtures(t *testing.T) {
	const common = "testdata/fixtures/common"
	tests := []struct {
		name string
		base string
		req  string
		code string // "" for an accepted fixture
	}{
		{"018-route-auth", common, "02 req 35.3: two Route auth.* Policies", CodeSlot},
		{"018-gateway-slot", common, "02 req 35.3: two Gateway ratelimit with an explicit equal slot", CodeSlot},
		{"018-upstream-oauth2", common, "02 req 35.3, R-15: two auth.upstream-oauth2 on one Upstream", CodeSlot},
		{"019-exclude", shopBundle, "02 req 35.1: exclude ratelimit-global, CM Diagnostics example", CodeOverridable},
		{"019-replace-cors", common, "02 req 35.2: Route cors replaces an overridable: false Gateway cors", CodeOverridable},
		{"019-relist", shopBundle, "02 req 35.2, R-16: Route re-lists ratelimit-global", CodeOverridable},
		{"020-validation-gateway", common, "02 req 35.4: validation.json-schema at Gateway", CodeScope},
		{"020-cache-gateway", common, "02 req 35.4: cache at Gateway", CodeScope},
		{"020-oauth2-route", common, "02 req 35.4: auth.upstream-oauth2 at Route", CodeScope},
		{"020-jwt-upstream", common, "02 req 35.4: auth.jwt at Upstream", CodeScope},
		{"020-plugin-upstream", common, "02 req 14: plugin with onResponse at Upstream", CodeScope},
		{"020-plugin-route", common, "02 req 14: plugin with onUpstreamRequest at Route", CodeScope},
		{"029-jwt", common, "02 req 37: auth.jwt open", CodeFailureMode},
		{"029-authz-cel", common, "02 req 37: authz.cel open", CodeFailureMode},
		{"029-upstream-oauth2", common, "02 req 37: auth.upstream-oauth2 open", CodeFailureMode},
		{"029-plugin-authz", common, "02 req 37: plugin filterClass authz open", CodeFailureMode},
		{"029-accepted", common, "02 req 37: ratelimit open and plugin custom open are accepted", ""},
		{"038-validation", common, "02 req 39: cache with validation.json-schema", CodeCacheGuardrail},
		{"038-authz-cel-body", common, "02 req 39: cache with an authz.cel rule reading request.body", CodeCacheGuardrail},
		{"038-authz-cel-headers", common, "02 req 39: cache with an authz.cel rule without the body is accepted", ""},
		{"038-plugin-authz", common, "02 req 39: cache with a plugin authz Policy in onRequestBody", CodeCacheGuardrail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join("testdata", "fixtures", tc.name)
			b, files := loadBundle(t, tc.base, dir)
			_, ds := stageI(t, b, files, 2)
			if tc.code == "" && len(ds) > 0 {
				t.Errorf("%s: want no diagnostics, got:\n%s", tc.req, diagText(t, ds))
			}
			for _, d := range ds {
				if d.Code != tc.code {
					t.Errorf("%s: code %s, want only %s", tc.req, d.Code, tc.code)
				}
				if d.File == "" || d.Line == 0 || d.Column == 0 {
					t.Errorf("%s: diagnostic without file, line and column: %+v", tc.req, d)
				}
			}
			if tc.code != "" && len(ds) != 1 {
				t.Errorf("%s: %d diagnostics, want 1:\n%s", tc.req, len(ds), diagText(t, ds))
			}
			golden(t, filepath.Join(dir, "want.txt"), diagText(t, ds))
		})
	}
}

// TestDiagnosticsExample reproduces the RZ-CFG-019 line of the
// configuration model's "Diagnostics and source map" example byte for
// byte (02 req 35.1).
func TestDiagnosticsExample(t *testing.T) {
	b, files := loadBundle(t, shopBundle, "testdata/fixtures/019-exclude")
	_, ds := stageI(t, b, files, 1)
	const want = "routes/cart-grpc.yaml:11:7 error RZ-CFG-019 Route/cart-grpc spec.excludePolicies[name=ratelimit-global]: " +
		"cannot exclude a Policy with overridable: false (declared in policies/common.yaml:46:3)\n"
	if got := diagText(t, ds); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if !strings.Contains(diagText(t, ds), "RZ-CFG-019") {
		t.Fatal("no RZ-CFG-019")
	}
}
