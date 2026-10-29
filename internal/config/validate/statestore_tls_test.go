// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/secret"
)

// stateStoreGateway returns the base Bundle with lines inserted after
// the Gateway's stateStore.url line (indented four spaces).
func stateStoreGateway(t *testing.T, lines string) map[string]string {
	t.Helper()
	files := readDirs(t, baseDir)
	const anchor = "    url: {secretRef: {provider: env, name: RURALZ_STATE_STORE_URL}}\n"
	g := files["ruralz.yaml"]
	if !strings.Contains(g, anchor) {
		t.Fatal("the base Gateway lost its stateStore.url line")
	}
	files["ruralz.yaml"] = strings.Replace(g, anchor, anchor+lines, 1)
	return files
}

// TLS block lines of the tests, under stateStore (tls) and under
// stateStore.cache (cacheTLS).
const (
	tlsCA  = "      caCertificate: {secretRef: {provider: file, name: /etc/ruralz/redis/ca.crt}}\n"
	tlsCrt = "      clientCertificate: {secretRef: {provider: file, name: /etc/ruralz/redis/tls.crt}}\n"
	tlsKey = "      clientKey: {secretRef: {provider: file, name: /etc/ruralz/redis/tls.key}}\n"
	// cacheURL opens stateStore.cache; its tls lines follow, six spaces in.
	cacheURL = "    cache:\n      url: {secretRef: {provider: env, name: RURALZ_SECRET_CACHE_URL}}\n"
)

// cacheTLS returns lines for stateStore.cache.tls.
func cacheTLS(lines ...string) string {
	out := cacheURL + "      tls:\n"
	for _, l := range lines {
		out += "  " + l
	}
	return out
}

// TestStateStoreTLSClientPair: stateStore.tls and stateStore.cache.tls
// each take the client pair rule of telemetry.otlp.tls and Upstream tls,
// reported at the missing half (RZ-CFG-005); a full pair or a CA alone
// passes.
func TestStateStoreTLSClientPair(t *testing.T) {
	tests := []struct {
		name, lines string
		path        string // "" for no finding
	}{
		{"certificate without key", "    tls:\n" + tlsCrt, "spec.stateStore.tls.clientKey"},
		{"key without certificate", "    tls:\n" + tlsCA + tlsKey, "spec.stateStore.tls.clientCertificate"},
		{"full pair", "    tls:\n" + tlsCA + tlsCrt + tlsKey, ""},
		{"CA alone", "    tls:\n" + tlsCA, ""},
		{"cache certificate without key", cacheTLS(tlsCrt), "spec.stateStore.cache.tls.clientKey"},
		{"cache key without certificate", cacheTLS(tlsKey), "spec.stateStore.cache.tls.clientCertificate"},
		{"cache full pair", cacheTLS(tlsCA, tlsCrt, tlsKey), ""},
	}
	v := newValidator(t, nil)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, files := loadFiles(t, stateStoreGateway(t, tc.lines))
			_, ds := stageH(t, v, b, files, 2)
			if tc.path == "" {
				if len(ds) > 0 {
					t.Fatalf("want no diagnostics, got:\n%s", diagText(t, ds))
				}
				return
			}
			if len(ds) != 1 || ds[0].Code != CodeSchema || ds[0].Path.String() != tc.path {
				t.Fatalf("want one %s at %s, got:\n%s", CodeSchema, tc.path, diagText(t, ds))
			}
		})
	}
}

// TestStateStoreTLSDestination: the stateStore.tls and stateStore.cache.tls
// values are local, as Upstream tls and telemetry.otlp.tls values are, with
// the CA bundle, certificate and key kinds, so one CA file serves an
// Upstream and both State Store connections; the same file as a
// clientSecret (sent to a tokenUrl) or as a State Store URL (sent to the
// State Store) is two destinations (RZ-CFG-041).
func TestStateStoreTLSDestination(t *testing.T) {
	v := newValidator(t, nil)
	files := stateStoreGateway(t, "    tls:\n"+tlsCA+tlsCrt+tlsKey+cacheTLS(tlsCA, tlsCrt, tlsKey))
	files["shared-ca.yaml"] = "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: shared-ca\nspec:\n" +
		"  protocol: http\n  endpoints:\n    - address: 10.0.0.9:8443\n" +
		"  tls:\n    caCertificate: {secretRef: {provider: file, name: /etc/ruralz/redis/ca.crt}}\n"
	b, table := loadFiles(t, files)
	res, ds := stageH(t, v, b, table, 2)
	if len(ds) > 0 {
		t.Fatalf("a CA file shared by an Upstream and both State Store connections: want no diagnostics, got:\n%s", diagText(t, ds))
	}
	want := map[string]secret.Kind{
		"spec.stateStore.tls.caCertificate":           secret.KindPEMCertPool,
		"spec.stateStore.tls.clientCertificate":       secret.KindPEMCertificate,
		"spec.stateStore.tls.clientKey":               secret.KindPEMPrivateKey,
		"spec.stateStore.cache.tls.caCertificate":     secret.KindPEMCertPool,
		"spec.stateStore.cache.tls.clientCertificate": secret.KindPEMCertificate,
		"spec.stateStore.cache.tls.clientKey":         secret.KindPEMPrivateKey,
	}
	for _, u := range res.Secrets {
		k, ok := want[u.Path.String()]
		if !ok {
			continue
		}
		delete(want, u.Path.String())
		if u.Kind != k || u.Destination != hub.DestinationLocal {
			t.Errorf("%s: kind %s, destination %q; want %s, %q", u.Path, kindName(u.Kind), u.Destination, kindName(k), hub.DestinationLocal)
		}
	}
	for p := range want {
		t.Errorf("no secret use at %s", p)
	}

	for _, tc := range []struct {
		name, lines string
	}{
		// The base Bundle's auth.upstream-oauth2 clientSecret is this file.
		{"key file as a clientSecret", "    tls:\n      clientKey: {secretRef: {provider: file, name: /etc/ruralz/oauth/client-secret}}\n" +
			"      clientCertificate: {secretRef: {provider: file, name: /etc/ruralz/redis/tls.crt}}\n"},
		// The base Bundle's stateStore.url is this variable.
		{"CA as the State Store URL", "    tls:\n      caCertificate: {secretRef: {provider: env, name: RURALZ_STATE_STORE_URL}}\n"},
		{"cache CA as the cache URL", cacheTLS("      caCertificate: {secretRef: {provider: env, name: RURALZ_SECRET_CACHE_URL}}\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, table := loadFiles(t, stateStoreGateway(t, tc.lines))
			_, ds := stageH(t, v, b, table, 2)
			if len(ds) != 1 || ds[0].Code != CodeDestination {
				t.Fatalf("want one %s, got:\n%s", CodeDestination, diagText(t, ds))
			}
		})
	}
}
