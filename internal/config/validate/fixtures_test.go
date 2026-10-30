// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestFixtures runs stage H on the base Bundle edited by each fixture
// under testdata/fixtures/<name> (a file there replaces the base file of
// the same path) and compares the diagnostics, text form, with
// want.txt. Each case states the rule it proves; a fixture whose rule is
// removed from stage H loses its finding and fails. Accepted fixtures
// (no codes) hold the edge cases a rule must not refuse.
func TestFixtures(t *testing.T) {
	tests := []struct {
		name  string
		req   string
		codes []string // nil for an accepted fixture
	}{
		{"009-route-upstream", "a Route upstreams entry naming no Upstream, with the nearest name as hint", []string{CodeReference}},
		{"009-step-upstream", "a composition step upstream naming no Upstream (CM Diagnostics example)", []string{CodeReference}},
		{"009-gateway-policy", "a Gateway policies entry naming no Policy", []string{CodeReference}},
		{"009-exclude-policy", "a Route excludePolicies entry naming no Policy, without a hint", []string{CodeReference}},
		{"009-wrong-kind", "a Route upstreams entry naming a Policy: a wrong-kind target", []string{CodeReference}},
		{"009-upstream-policy", "an Upstream policies entry naming no Policy", []string{CodeReference}},
		{"009-route-listener", "a Route listener that is no Gateway listener", []string{CodeReference}},
		{"009-quota", "a quota consumerQuota that no Consumer defines", []string{CodeReference}},
		{"009-quota-unit", "a quota consumerQuota defined only with unit tokens", []string{CodeReference}},
		{"009-token-budget", "an ai.token-budget consumerQuota defined only with unit requests", []string{CodeReference}},
		{"009-plugin", "a plugin Policy naming no Plugin", []string{CodeReference}},
		{"009-ai-references", "Upstream ai.models and AIModel candidates naming no target", []string{CodeReference}},
		{"005-listener-port", "two listeners on one port", []string{CodeSchema}},
		{"005-admin-port", "a listener on the authored admin port", []string{CodeSchema}},
		{"005-admin-port-default", "a listener on the default admin port 9901", []string{CodeSchema}},
		{"005-admin-port-materialized", "admin: {} gets the default port from stage G, still the default", []string{CodeSchema}},
		{"005-https-tls", "an https listener without tls or without certificates", []string{CodeSchema}},
		{"005-http-listener-tls", "tls on an http listener, which ignores it", []string{CodeSchema}},
		{"005-listener-hostname", "listener hostnames outside the match.hosts forms", []string{CodeSchema}},
		{"005-trusted-proxies", "trustedProxies entries that are no CIDR or address", []string{CodeSchema}},
		{"005-otlp", "an OTLP endpoint with an empty port and a client certificate without key", []string{CodeSchema}},
		{"005-otlp-http-tls", "telemetry.otlp.tls beside an http endpoint, which ignores it", []string{CodeSchema}},
		{"005-match-host", "a match.hosts entry with a * that is not one leading label", []string{CodeSchema}},
		{"005-match-path", "path criteria the Router cannot compile, empty ones included", []string{CodeSchema}},
		{"005-match-header", "header criteria the Router cannot compile", []string{CodeSchema}},
		{"005-match-grpc", "an empty grpc service", []string{CodeSchema}},
		{"005-upstream-endpoints", "an http Upstream without endpoints or discovery; protocol ai needs none", []string{CodeSchema}},
		{"005-endpoint-address", "Endpoint addresses that are not host:port", []string{CodeSchema}},
		{"005-dns-discovery", "dns discovery without a valid service or port", []string{CodeSchema}},
		{"005-upstream-tls", "an Upstream clientKey without clientCertificate", []string{CodeSchema}},
		{"005-filter-class", "a cors Policy choosing another Filter class", []string{CodeSchema}},
		{"005-oauth-clients", "oauthClients without a jwt issuer (internal/identity)", []string{CodeSchema}},
		{"005-aggregate-when", "when on an aggregate step, never evaluated; on a sequential step accepted", []string{CodeSchema}},
		{"005-step-path", "literal step paths without the hardening of the client path or with a control byte in the query; escapes and three dots accepted", []string{CodeSchema}},
		{"005-step-target", "step targets that are no dot path; star and escapes accepted", []string{CodeSchema}},
		{"005-step-group", "collection: true with an empty group, which is no group; a named group accepted", []string{CodeSchema}},
		{"005-quota-window", "Consumer quota windows below 1s or not whole seconds; 1s and calendar: month accepted", []string{CodeSchema}},
		{"021-image-digest", "Plugin images without a full lower-case @sha256 digest; pinned images accepted", []string{CodePlugin}},
		{"031-steps", "more steps than an authored maxCompositionSteps", []string{CodeSteps}},
		{"031-steps-default", "more steps than the default maxCompositionSteps 16", []string{CodeSteps}},
		{"031-steps-materialized", "limits without maxCompositionSteps: the materialized default, no related location", []string{CodeSteps}},
		{"032-exceed", "explicit maxBodyBytes values above maxResponseBodyBytes", []string{CodeStepBodies}},
		{"032-no-share", "explicit maxBodyBytes values that leave no default share", []string{CodeStepBodies}},
		{"032-accepted", "explicit values that leave each other step a share", nil},
		{"023-identical", "two Routes with identical criteria on every listener", []string{CodeDuplicateMatch}},
		{"023-normalized", "criteria equal after host and path normalization", []string{CodeDuplicateMatch}},
		{"023-other-listener", "identical criteria on disjoint listeners are accepted", nil},
		{"023-shared-listener", "identical criteria sharing one listener", []string{CodeDuplicateMatch}},
		{"041-api-key-to-token-url", "a Consumer API key secret also sent to a tokenUrl", []string{CodeDestination}},
		{"041-env-key", "an env secret whose ignored key differs", []string{CodeDestination}},
		{"041-file-member", "a whole file and one of its keys with two destinations", []string{CodeDestination}},
		{"041-file-path", "one file spelled with a different path", []string{CodeDestination}},
		{"041-two-token-urls", "one clientSecret sent to two token origins", []string{CodeDestination}},
		{"041-accepted", "one origin spelled twice, the State Store URL twice, distinct keys of one file", nil},
		{"035-basic-username", "a basic username declared by two Consumers (internal/identity)", []string{"RZ-CFG-035"}},
		{"036-iterations", "basic iterations below 600,000 (internal/identity)", []string{"RZ-CFG-036"}},
	}
	v := newValidator(t, nil)
	seen := map[string]bool{}
	for _, tc := range tests {
		seen[tc.name] = true
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join("testdata", "fixtures", tc.name)
			b, files := loadDirs(t, baseDir, dir)
			_, ds := stageH(t, v, b, files, 2)
			if tc.codes == nil && len(ds) > 0 {
				t.Errorf("%s: want no diagnostics, got:\n%s", tc.req, diagText(t, ds))
			}
			if tc.codes != nil && len(ds) == 0 {
				t.Errorf("%s: want %v, got no diagnostics", tc.req, tc.codes)
			}
			for _, d := range ds {
				if !slices.Contains(tc.codes, d.Code) {
					t.Errorf("%s: code %s, want only %v", tc.req, d.Code, tc.codes)
				}
			}
			golden(t, filepath.Join(dir, "want.txt"), diagText(t, ds))
		})
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "fixtures"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !seen[e.Name()] {
			t.Errorf("fixture %s has no test case", e.Name())
		}
	}
}
