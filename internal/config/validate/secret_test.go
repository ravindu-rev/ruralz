// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestEverySecretFieldClassified: every x-ruralz-secret field of the hub
// schema has a classify row with the kind and destination
// "Secret-to-destination binding" gives it, so a new secret field fails
// here until it is bound to a destination.
func TestEverySecretFieldClassified(t *testing.T) {
	want := map[string]secretClass{
		"Gateway||spec.listeners[].tls.certificates[].certificate": {kind: secret.KindPEMCertificate},
		"Gateway||spec.listeners[].tls.certificates[].privateKey":  {kind: secret.KindPEMPrivateKey},
		"Gateway||spec.stateStore.url":                             {kind: secret.KindStateStoreURL, dest: destStateStore},
		"Gateway||spec.stateStore.cache.url":                       {kind: secret.KindStateStoreURL, dest: destStateStore},
		"Gateway||spec.stateStore.tls.caCertificate":               {kind: secret.KindPEMCertPool},
		"Gateway||spec.stateStore.tls.clientCertificate":           {kind: secret.KindPEMCertificate},
		"Gateway||spec.stateStore.tls.clientKey":                   {kind: secret.KindPEMPrivateKey},
		"Gateway||spec.stateStore.cache.tls.caCertificate":         {kind: secret.KindPEMCertPool},
		"Gateway||spec.stateStore.cache.tls.clientCertificate":     {kind: secret.KindPEMCertificate},
		"Gateway||spec.stateStore.cache.tls.clientKey":             {kind: secret.KindPEMPrivateKey},
		"Gateway||spec.telemetry.otlp.tls.caCertificate":           {kind: secret.KindPEMCertPool},
		"Gateway||spec.telemetry.otlp.tls.clientCertificate":       {kind: secret.KindPEMCertificate},
		"Gateway||spec.telemetry.otlp.tls.clientKey":               {kind: secret.KindPEMPrivateKey},
		"Upstream||spec.tls.caCertificate":                         {kind: secret.KindPEMCertPool},
		"Upstream||spec.tls.clientCertificate":                     {kind: secret.KindPEMCertificate},
		"Upstream||spec.tls.clientKey":                             {kind: secret.KindPEMPrivateKey},
		"Policy|auth.mtls|spec.config.caCertificate":               {kind: secret.KindPEMCertPool},
		"Policy|auth.mtls|spec.config.crl":                         {kind: secret.KindPEMCRL},
		"Policy|auth.upstream-oauth2|spec.config.clientSecret":     {kind: secret.KindOpaque, dest: destTokenURL},
		"Consumer||spec.credentials.apiKeys[].secretRef":           {kind: secret.KindAPIKey},
		"AIProvider||spec.credentials.apiKey":                      {kind: secret.KindOpaque},
	}
	idx, ok := pipeline(t).idx.Index(convert.HubAPIVersion)
	if !ok {
		t.Fatal("no hub schema")
	}
	var found []string
	for _, f := range idx.Fields() {
		if !f.Node.Keywords().Secret || !inBundle(v1alpha1.Kind(f.Kind)) {
			continue
		}
		policyType := ""
		for _, d := range f.Where {
			for _, c := range d.Conditions {
				if d.Path == "spec" && c.Member == "type" {
					policyType, _ = c.Value.(string)
				}
			}
		}
		key := f.Kind + "|" + policyType + "|" + f.Path
		found = append(found, key)
		got, ok := classify(key)
		if !ok {
			t.Errorf("secret field %s has no classify row", key)
			continue
		}
		if w, listed := want[key]; !listed || got != w {
			t.Errorf("classify(%s) = %+v, want %+v (listed %v)", key, got, w, listed)
		}
	}
	for key := range want {
		if !slices.Contains(found, key) {
			t.Errorf("classify row %s names no secret field of the schema", key)
		}
	}
	if _, ok := classify("Route||spec.timeout"); ok {
		t.Error("classify accepted a field that is no secret")
	}
}

// TestSecretKey: the classify key of an instance path writes list
// elements as "[]" and the Policy type only for Policies.
func TestSecretKey(t *testing.T) {
	p := diag.Path{
		diag.Field("spec"), diag.Field("listeners"), diag.Keyed("name", "https"), diag.Field("tls"),
		diag.Field("certificates"), diag.Index(0), diag.Field("certificate"),
	}
	if got, want := secretKey(v1alpha1.KindGateway, "ignored", p), "Gateway||spec.listeners[].tls.certificates[].certificate"; got != want {
		t.Errorf("secretKey = %q, want %q", got, want)
	}
	q := diag.Path{diag.Field("spec"), diag.Field("config"), diag.Field("clientSecret")}
	if got, want := secretKey(v1alpha1.KindPolicy, "auth.upstream-oauth2", q), "Policy|auth.upstream-oauth2|spec.config.clientSecret"; got != want {
		t.Errorf("secretKey = %q, want %q", got, want)
	}
}

// TestSourceOf: two references read one secret exactly when the Node's
// resolver reads them from one source: env ignores the key, a file is
// named by its cleaned absolute path and its key, and a file read whole
// overlaps its keys; kubernetes and vault match by name and key.
func TestSourceOf(t *testing.T) {
	tests := []struct {
		ref      v1alpha1.SecretRef
		src, key string
		overlaps bool
	}{
		{v1alpha1.SecretRef{Provider: "env", Name: "RURALZ_SECRET_A", Key: "x"}, "env\x00RURALZ_SECRET_A", "", false},
		{v1alpha1.SecretRef{Provider: "file", Name: "/etc/ruralz/a/../b//c"}, "file\x00/etc/ruralz/b/c", "", true},
		{v1alpha1.SecretRef{Provider: "file", Name: "/etc/ruralz/b/c", Key: "k"}, "file\x00/etc/ruralz/b/c", "k", true},
		{v1alpha1.SecretRef{Provider: "file", Name: "relative/../x"}, "file\x00relative/../x", "", true},
		{v1alpha1.SecretRef{Provider: "kubernetes", Name: "llm-keys", Key: "a"}, "kubernetes\x00llm-keys", "a", false},
		{v1alpha1.SecretRef{Provider: "vault", Name: "kv/data/x"}, "vault\x00kv/data/x", "", false},
	}
	for _, tt := range tests {
		src, key, overlaps := sourceOf(tt.ref)
		if src != tt.src || key != tt.key || overlaps != tt.overlaps {
			t.Errorf("sourceOf(%+v) = %q, %q, %v; want %q, %q, %v", tt.ref, src, key, overlaps, tt.src, tt.key, tt.overlaps)
		}
	}
}

// sameSecret is the oracle of checkSecrets: two uses read one secret when
// sourceOf gives one source and the same key, or one reads a whole file
// the other reads a key of.
func sameSecret(a, b v1alpha1.SecretRef) bool {
	sa, ka, oa := sourceOf(a)
	sb, kb, _ := sourceOf(b)
	if sa != sb {
		return false
	}
	return ka == kb || (oa && (ka == "" || kb == ""))
}

// bruteSecrets reports, for each use, the earliest earlier use of the same
// secret with another destination, or -1: the quadratic definition.
func bruteSecrets(uses []hub.SecretUse) []int {
	out := make([]int, len(uses))
	for i := range uses {
		out[i] = -1
		for j := range i {
			if sameSecret(uses[i].Ref, uses[j].Ref) && uses[i].Destination != uses[j].Destination {
				out[i] = j
				break
			}
		}
	}
	return out
}

// TestCheckSecretsMatchesOracle: on generated use lists, checkSecrets
// reports exactly the uses the quadratic definition reports, each against
// the same earliest conflicting use, in one linear pass.
func TestCheckSecretsMatchesOracle(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: a fixed seed makes the generated lists the same on every run.
	providers := []v1alpha1.SecretProvider{"env", "file", "kubernetes"}
	names := []string{"/etc/ruralz/a", "/etc/ruralz/./a", "/etc/ruralz/b", "RURALZ_SECRET_A"}
	keys := []string{"", "", "k1", "k2"}
	dests := []string{hub.DestinationLocal, hub.DestinationStateStore, "https://a.example:443", "https://b.example:443"}
	reported := 0
	for round := range 300 {
		n := 1 + rng.IntN(12)
		uses := make([]hub.SecretUse, n)
		for i := range uses {
			uses[i] = hub.SecretUse{
				Ref: v1alpha1.SecretRef{
					Provider: providers[rng.IntN(len(providers))],
					Name:     names[rng.IntN(len(names))],
					Key:      keys[rng.IntN(len(keys))],
				},
				Resource:    diag.ResourceID{Kind: "Policy", Name: fmt.Sprintf("p%d", i)},
				Path:        diag.Path{diag.Field("spec")},
				Loc:         diag.Location{File: "f.yaml", Line: i + 1, Column: 1},
				Destination: dests[rng.IntN(len(dests))],
			}
		}
		k := newKeeper(1 << 20)
		checkSecrets(uses, &work{}, &sink{k: k})
		got := make([]int, n)
		for i := range got {
			got[i] = -1
		}
		for _, d := range k.list() {
			if d.Code != CodeDestination {
				t.Fatalf("round %d: code %s", round, d.Code)
			}
			i := d.Line - 1
			j := d.Related[0].Line - 1
			if got[i] != -1 {
				t.Fatalf("round %d: use %d reported twice", round, i)
			}
			got[i] = j
			reported++
		}
		if want := bruteSecrets(uses); !slices.Equal(got, want) {
			t.Fatalf("round %d: checkSecrets reports %v, the oracle %v, for %+v", round, got, want, uses)
		}
	}
	if reported == 0 {
		t.Fatal("no generated list had a conflict; the generator tests nothing")
	}
}

// TestTokenOrigin: a tokenUrl binds its secret to "scheme://host:port"
// with the default port explicit and case folded (egress.Origin), and a
// tokenUrl without a host to its own text.
func TestTokenOrigin(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://auth.shop.example/oauth2/token", "https://auth.shop.example:443"},
		{"HTTPS://Auth.Shop.Example:443/x", "https://auth.shop.example:443"},
		{"https://auth.shop.example:8443/x", "https://auth.shop.example:8443"},
		{"http://auth.shop.example/x", "http://auth.shop.example:80"},
		{"https://[2001:db8::1]/token", "https://[2001:db8::1]:443"},
		{"not a url", "not a url"},
		{"%zz", "%zz"},
	}
	for _, tt := range tests {
		if got := tokenOrigin(tt.in); got != tt.want {
			t.Errorf("tokenOrigin(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
