// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// common is the base of most fixtures: a library of unattached Policies
// and Plugins with a Gateway, an Upstream and a Route.
const common = "testdata/fixtures/common"

// shopFixtures are the fixtures over the example Bundle: each changes one
// line of a base file of its golden corpus entry, given as the file, the
// line and its replacement, so the package holds no edited copy of an
// example Bundle file.
func shopFixtures() map[string][3]string {
	return map[string][3]string{
		"019-exclude": {
			"routes/cart-grpc.yaml",
			"    - name: cors-default                # not a browser API\n",
			"    - name: ratelimit-global            # a platform guardrail\n",
		},
		"019-relist": {
			"routes/orders-summary.yaml",
			"    - name: ratelimit-orders            # own slot: adds to ratelimit-global\n",
			"    - name: ratelimit-global            # re-listed: a replacement in its own slot\n",
		},
	}
}

// fixture loads the fixture name over base: over common, the YAML files of
// testdata/fixtures/<name> replace the files of the same path; over the
// example Bundle (shopEntry), the fixture is its edit in shopFixtures.
func fixture(t testing.TB, base, name string) (*hub.Bundle, *tree.FileTable) {
	t.Helper()
	if base != shopEntry {
		return loadBundle(t, base, filepath.Join("testdata", "fixtures", name))
	}
	e, ok := shopFixtures()[name]
	if !ok {
		t.Fatalf("fixture %s has no edit of the example Bundle", name)
	}
	return loadLayers(t, edited(t, shopLayer(t), e[0], e[1], e[2]))
}

// TestFixtures runs stage I over one negative fixture per case and
// compares the diagnostics, in their text form with file, line and
// column, with the fixture's want.txt (TQ "Conformance suites"). A
// fixture's base is common, whose files its YAML files replace, or the
// example Bundle of the golden corpus, one line of which it edits.
func TestFixtures(t *testing.T) {
	tests := []struct {
		name string
		base string
		req  string
		code string // "" for an accepted fixture
	}{
		{"018-route-auth", common, "two Route auth.* Policies", CodeSlot},
		{"018-gateway-slot", common, "two Gateway ratelimit with an explicit equal slot", CodeSlot},
		{"018-upstream-oauth2", common, "two auth.upstream-oauth2 on one Upstream", CodeSlot},
		{"019-exclude", shopEntry, "exclude ratelimit-global, CM Diagnostics example", CodeOverridable},
		{"019-replace-cors", common, "Route cors replaces an overridable: false Gateway cors", CodeOverridable},
		{"019-relist", shopEntry, "Route re-lists ratelimit-global", CodeOverridable},
		{"020-validation-gateway", common, "validation.json-schema at Gateway", CodeScope},
		{"020-cache-gateway", common, "cache at Gateway", CodeScope},
		{"020-oauth2-route", common, "auth.upstream-oauth2 at Route", CodeScope},
		{"020-jwt-upstream", common, "auth.jwt at Upstream", CodeScope},
		{"020-plugin-upstream", common, "plugin with onResponse at Upstream", CodeScope},
		{"020-plugin-route", common, "plugin with onUpstreamRequest at Route", CodeScope},
		{"029-jwt", common, "auth.jwt open", CodeFailureMode},
		{"029-authz-cel", common, "authz.cel open", CodeFailureMode},
		{"029-upstream-oauth2", common, "auth.upstream-oauth2 open", CodeFailureMode},
		{"029-plugin-authz", common, "plugin filterClass authz open", CodeFailureMode},
		{"029-accepted", common, "ratelimit open and plugin custom open are accepted", ""},
		{"038-validation", common, "cache with validation.json-schema", CodeCacheGuardrail},
		{"038-authz-cel-body", common, "cache with an authz.cel rule reading request.body", CodeCacheGuardrail},
		{"038-authz-cel-headers", common, "cache with an authz.cel rule without the body is accepted", ""},
		{"038-plugin-authz", common, "cache with a plugin authz Policy in onRequestBody", CodeCacheGuardrail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, files := fixture(t, tc.base, tc.name)
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
			golden(t, filepath.Join("testdata", "fixtures", tc.name, "want.txt"), diagText(t, ds))
		})
	}
}

// TestDiagnosticsExample reproduces the RZ-CFG-019 line of the
// configuration model's "Diagnostics and source map" example byte for
// byte.
func TestDiagnosticsExample(t *testing.T) {
	b, files := fixture(t, shopEntry, "019-exclude")
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
