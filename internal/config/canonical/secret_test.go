// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// Canaries of the secret leak test: values no Bundle holds, so finding
// one in an output means a resolved secret leaked.
const (
	envCanary  = "redis://state-store:6379/0?canary=env-7f3a9c41"
	certCanary = "canary-file-cert-1d6e0b93"
	keyCanary  = "canary-file-key-5be2d08f"
)

// TestSecretValuesNeverEncoded is the stage L part of the secret leak test
// (02 test plan 26; 02 req 34; 04 req 74, "secretRef shown, values
// never"): with a canary in RURALZ_STATE_STORE_URL and in file secrets, a
// Gateway whose stateStore.url and listener TLS use secretRefs encodes,
// dumps, decodes and reads back with the references (provider, name, key)
// and without any canary. Every output of the package is checked: the
// canonical bytes, the /config/dump document, the decoded trees and the
// dump ReadDump returns.
func TestSecretValuesNeverEncoded(t *testing.T) {
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
	bundleDir := t.TempDir()
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
	if err := os.WriteFile(filepath.Join(bundleDir, "ruralz.yaml"), []byte(gateway), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rev, err := NewEncoder(hubIndex(t)).Encode(ctx, loadBundle(t, bundleDir), Options{})
	if err != nil {
		t.Fatal(err)
	}
	refs := [][]byte{
		[]byte(`"url":{"secretRef":{"name":"RURALZ_STATE_STORE_URL","provider":"env"}}`),
		append(append([]byte(`"certificate":{"secretRef":{"name":`), quoted(t, certFile)...), `,"provider":"file"}}`...),
		append(append([]byte(`"privateKey":{"secretRef":{"key":"pem","name":`), quoted(t, keyFile)...), `,"provider":"file"}}`...),
	}
	for _, ref := range refs {
		if !bytes.Contains(rev.Content, ref) {
			t.Errorf("canonical content lacks the reference %s:\n%s", ref, rev.Content)
		}
	}
	noCanary(t, "canonical content", rev.Content)
	body := dumpOf(t, nil, rev, rev.Digest)
	noCanary(t, "/config/dump document", body)
	for _, ref := range refs {
		if !bytes.Contains(body, ref) {
			t.Errorf("/config/dump document lacks the reference %s", ref)
		}
	}
	dec := NewDecoder(hubIndex(t), DecoderOptions{})
	rs, err := dec.Verify(ctx, rev.Content, rev.Digest, Source{Name: "lkg.json"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		noCanaryTree(t, r.Root)
	}
	d, rs, err := dec.ReadDump(ctx, bytes.NewReader(body), 0, Source{Name: "dump.json"})
	if err != nil {
		t.Fatal(err)
	}
	noCanary(t, "dump read back", d.Body)
	for _, r := range rs {
		noCanaryTree(t, r.Root)
	}
}

// quoted returns the JSON string of s.
func quoted(t *testing.T, s string) []byte {
	t.Helper()
	q, err := jsonval.AppendString(nil, s)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// noCanary fails when b holds a canary.
func noCanary(t *testing.T, what string, b []byte) {
	t.Helper()
	for _, c := range []string{envCanary, certCanary, keyCanary} {
		if bytes.Contains(b, []byte(c)) {
			t.Errorf("%s holds the secret value %q", what, c)
		}
	}
}

// noCanaryTree fails when a key or scalar of n holds a canary.
func noCanaryTree(t *testing.T, n *tree.Node) {
	t.Helper()
	if n == nil {
		return
	}
	for _, c := range []string{envCanary, certCanary, keyCanary} {
		if strings.Contains(n.Text, c) {
			t.Errorf("decoded tree holds the secret value %q", c)
		}
	}
	for _, m := range n.Members {
		noCanaryTree(t, m.Value)
	}
	for _, it := range n.Items {
		noCanaryTree(t, it)
	}
}
