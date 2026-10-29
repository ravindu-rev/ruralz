// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Canaries of the secret leak test: values no Bundle holds, so finding
// one in an output means a resolved secret leaked.
const (
	envCanary  = "redis://state-store:6379/0?canary=env-3c8e1f57"
	certCanary = "canary-file-cert-9a41d2c6"
	keyCanary  = "canary-file-key-0f7be3a9"
)

// TestSecretValuesNeverRendered is render's part of the secret leak test
// ("secretRef shown, values never"): with a canary in
// RURALZ_STATE_STORE_URL and in file secrets, a Gateway whose
// stateStore.url and listener TLS use secretRefs renders, in YAML and
// JSON, with the references (provider, name, key) and without any canary
// in any encoding.
func TestSecretValuesNeverRendered(t *testing.T) {
	t.Setenv("RURALZ_STATE_STORE_URL", envCanary)
	secrets := t.TempDir()
	certFile := filepath.ToSlash(filepath.Join(secrets, "tls.crt"))
	keyFile := filepath.ToSlash(filepath.Join(secrets, "tls.json"))
	if err := os.WriteFile(certFile, []byte(certCanary), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte(`{"pem":"`+keyCanary+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	gateway := `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: https
      protocol: https
      port: 8443
      tls:
        certificates:
          - name: edge
            certificate: {secretRef: {provider: file, name: "` + certFile + `"}}
            privateKey: {secretRef: {provider: file, name: "` + keyFile + `", key: pem}}
  stateStore:
    driver: redis
    url: {secretRef: {provider: env, name: RURALZ_STATE_STORE_URL}}
`
	r := mustPipeline(t, []source{{"ruralz.yaml", []byte(gateway)}}, nil, subst.Environ(os.Environ()))
	yamlOut, jsonOut := renderYAML(t, r), renderAs(t, r, FormatJSON)
	// A temporary path is written plain or quoted, as its characters
	// require on each OS.
	for _, ref := range []string{"provider: env\n", "name: RURALZ_STATE_STORE_URL\n", certFile, keyFile, "key: pem\n"} {
		if !bytes.Contains(yamlOut, []byte(ref)) {
			t.Errorf("YAML lacks the reference %q:\n%s", ref, yamlOut)
		}
	}
	if !bytes.Contains(jsonOut, []byte(`"name": "RURALZ_STATE_STORE_URL"`)) {
		t.Errorf("JSON lacks the reference:\n%s", jsonOut)
	}
	for _, out := range [][]byte{yamlOut, jsonOut} {
		for _, canary := range []string{envCanary, certCanary, keyCanary, "canary="} {
			if bytes.Contains(out, []byte(canary)) {
				t.Errorf("rendered output holds the canary %q", canary)
			}
		}
	}
}

// TestNonReferenceSecretRefused checks that a value at an x-ruralz-secret
// position that is not a well-formed reference refuses the whole render
// with one RZ-CFG-012 at the secret field: a literal in a SecretValue
// field, a secretRef with an undeclared member, a provider outside its
// enum, a number for a name, and a literal at a SecretRef field (a
// Consumer API key). The finding is located and names neither the value
// nor any member below the field, and nothing is written.
func TestNonReferenceSecretRefused(t *testing.T) {
	const secret = "hunter2-literal"
	cases := []struct {
		name, yaml, path string
	}{
		{"literal SecretValue", "  stateStore:\n    url: " + secret + "\n", "spec.stateStore.url"},
		{"undeclared member", "  stateStore:\n    url: {secretRef: {provider: env, name: RURALZ_STATE_STORE_URL, value: " + secret + "}}\n", "spec.stateStore.url"},
		{"undeclared member name", "  stateStore:\n    url: {secretRef: {provider: env, name: X, " + secret + ": 1}}\n", "spec.stateStore.url"},
		{"provider outside enum", "  stateStore:\n    url: {secretRef: {provider: " + secret + ", name: X}}\n", "spec.stateStore.url"},
		{"number name", "  stateStore:\n    url: {secretRef: {provider: env, name: 12345}}\n", "spec.stateStore.url"},
	}
	s := mustStages(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := &tree.FileTable{}
			r := resource(t, files, "ruralz.yaml", minimalGateway+tc.yaml)
			var buf bytes.Buffer
			ds, err := WriteBundle(t.Context(), &buf, []*tree.Resource{r}, FormatYAML, Options{Schemas: s.idx, Files: files})
			if err != nil {
				t.Fatal(err)
			}
			if len(ds) != 1 || ds[0].Code != CodeLiteral || ds[0].Severity != diag.SeverityError {
				t.Fatalf("diagnostics = %+v, want one RZ-CFG-012 error", ds)
			}
			d := ds[0]
			if d.Path.String() != tc.path || d.File != "ruralz.yaml" || d.Line == 0 || d.Resource == nil || d.Resource.Name != "edge" {
				t.Errorf("finding at %s:%d %s (%v), want ruralz.yaml %s of Gateway/edge", d.File, d.Line, d.Path, d.Resource, tc.path)
			}
			line := string(d.AppendText(nil))
			if strings.Contains(line, secret) || strings.Contains(line, "12345") {
				t.Errorf("the finding shows the value: %s", line)
			}
			if !strings.Contains(d.Hint, "{secretRef: {name, provider}}") {
				t.Errorf("hint = %q, want the reference shape", d.Hint)
			}
			if buf.Len() > 0 {
				t.Errorf("a refused render wrote:\n%s", buf.Bytes())
			}
		})
	}
	t.Run("SecretRef field", func(t *testing.T) {
		files := &tree.FileTable{}
		c := resource(t, files, "consumers.yaml", `apiVersion: ruralz/v1alpha1
kind: Consumer
metadata: {name: acme}
spec:
  credentials:
    apiKeys:
      - name: primary
        secretRef: `+secret+`
`)
		_, ds, err := Documents(t.Context(), []*tree.Resource{c}, Options{Schemas: s.idx, Files: files})
		if err != nil || len(ds) != 1 || ds[0].Code != CodeLiteral {
			t.Fatalf("Documents = %+v, %v; want one RZ-CFG-012", ds, err)
		}
		if got := ds[0].Path.String(); got != "spec.credentials.apiKeys[name=primary].secretRef" {
			t.Errorf("path = %s", got)
		}
		if !strings.Contains(ds[0].Hint, "{name, provider}") {
			t.Errorf("hint = %q, want the SecretRef shape", ds[0].Hint)
		}
	})
}

// TestSecretFindingsPerField checks that each refused secret field gives
// one finding, in source order, and that well-formed sibling references
// give none.
func TestSecretFindingsPerField(t *testing.T) {
	files := &tree.FileTable{}
	r := resource(t, files, "ruralz.yaml", `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata: {name: edge}
spec:
  listeners:
    - name: https
      protocol: https
      port: 8443
      tls:
        certificates:
          - name: a
            certificate: {secretRef: {provider: file, name: /a.crt}}
            privateKey: literal-one
          - name: b
            certificate: literal-two
            privateKey: {secretRef: {provider: file, name: /b.key, extra: x, other: y}}
`)
	_, ds, err := Documents(t.Context(), []*tree.Resource{r}, Options{Schemas: mustStages(t).idx, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, d := range ds {
		paths = append(paths, d.Path.String())
	}
	want := []string{
		"spec.listeners[name=https].tls.certificates[name=a].privateKey",
		"spec.listeners[name=https].tls.certificates[name=b].certificate",
		"spec.listeners[name=https].tls.certificates[name=b].privateKey",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("finding paths = %q\nwant %q", paths, want)
	}
}

// TestSecretFindingsCapped checks the diagnostic cap: past
// Options.MaxDiagnostics findings the check stops with the RZ-CFG-001
// "too many diagnostics" error.
func TestSecretFindingsCapped(t *testing.T) {
	files := &tree.FileTable{}
	var b strings.Builder
	b.WriteString("apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata: {name: edge}\nspec:\n  listeners:\n")
	b.WriteString("    - name: https\n      protocol: https\n      port: 8443\n      tls:\n        certificates:\n")
	for _, n := range []string{"a", "b", "c", "d"} {
		b.WriteString("          - name: " + n + "\n            certificate: x\n            privateKey: y\n")
	}
	r := resource(t, files, "ruralz.yaml", b.String())
	_, ds, err := Documents(t.Context(), []*tree.Resource{r}, Options{Schemas: mustStages(t).idx, Files: files, MaxDiagnostics: 3})
	if err != nil {
		t.Fatal(err)
	}
	got := diagCodes(ds)
	slices.Sort(got)
	if want := []string{"RZ-CFG-001", CodeLiteral, CodeLiteral, CodeLiteral}; !slices.Equal(got, want) {
		t.Errorf("codes = %q, want three RZ-CFG-012 and one RZ-CFG-001", got)
	}
}
