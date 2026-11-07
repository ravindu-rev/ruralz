// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	ht "github.com/ravindu-rev/ruralz/internal/pluginhost/pluginhosttest"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// secretTable is an in-memory secret.Store.
type secretTable map[secret.Ref][]byte

func (s secretTable) Get(r secret.Ref) (secret.Value, bool) {
	v, ok := s[r]
	return secret.NewValue(v), ok
}

func (secretTable) Watch(secret.Ref, func(secret.Value) error) func() { return func() {} }

// keySchema marks config.signing as an ES256 key and config.token as a
// secret.
func keySchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"signing": map[string]any{"x-ruralz-key": map[string]any{"algorithm": "ES256", "operations": []any{"sign", "verify"}}},
			"mac":     map[string]any{"x-ruralz-key": map[string]any{"algorithm": "HS256", "operations": []any{"hmac"}}},
			"token":   map[string]any{"x-ruralz-secret": true},
		},
	}
}

// ref returns a {secretRef} config value.
func ref(name string) map[string]any {
	return map[string]any{"secretRef": map[string]any{"provider": "file", "name": name}}
}

// TestHostKeys: rz_configure receives key handles and the secret's value;
// the guest signs and verifies with the ES256 key and computes an HMAC
// with the HS256 key, while no key byte enters guest memory.
func TestHostKeys(t *testing.T) {
	h := newHarness(t)
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	macKey := []byte("0123456789abcdef0123456789abcdef")
	secrets := secretTable{
		{Provider: "file", Name: "/etc/ruralz/es256.pem"}: keyPEM,
		{Provider: "file", Name: "/etc/ruralz/mac"}:       macKey,
		{Provider: "file", Name: "/etc/ruralz/token"}:     []byte("upstream-token-value"),
	}
	mac := hmac.New(sha256.New, macKey)
	mac.Write([]byte("msg"))
	wantMAC := mac.Sum(nil)
	guestKey := []byte("a key the guest holds itself, 0123456789")
	gmac := hmac.New(sha256.New, guestKey)
	gmac.Write([]byte("msg"))
	wantGuestMAC := gmac.Sum(nil)

	g := ht.NewGuest("crypto_key_sign", "crypto_key_verify", "crypto_key_hmac", "crypto_digest", "crypto_hmac")
	sig := uint32(2048)
	g.Phase("rz_on_request_headers", nil,
		// Key handles are numbered in path order: mac 1, signing 2.
		ht.Check(g.Call("crypto_key_sign", ht.I32Const(2), g.Str("msg"), ht.Addr(sig), ht.I32Const(64)), 64),
		ht.Check(g.Call("crypto_key_verify", ht.I32Const(2), g.Str("msg"), ht.Addr(sig), ht.I32Const(64)), 0),
		ht.Check(g.Call("crypto_key_verify", ht.I32Const(2), g.Str("other"), ht.Addr(sig), ht.I32Const(64)), errInvalid),
		ht.Check(g.Call("crypto_key_hmac", ht.I32Const(1), g.Str("msg"), ht.I32Const(int32(ht.Scratch)), ht.I32Const(32)), 32),
		ht.CheckMem(ht.Scratch, wantMAC),
		ht.Check(g.Call("crypto_key_hmac", ht.I32Const(9), g.Str("msg"), ht.I32Const(int32(ht.Scratch)), ht.I32Const(32)), errInvalid),
		ht.Check(g.Call("crypto_hmac", ht.I32Const(algSHA256), ht.Addr(g.Bytes(guestKey)), ht.I32Const(ht.Size(len(guestKey))), g.Str("msg"), ht.I32Const(int32(ht.Scratch)), ht.I32Const(32)), 32),
		ht.CheckMem(ht.Scratch, wantGuestMAC),
		ht.Check(g.Call("crypto_digest", ht.I32Const(9), g.Str("x"), ht.I32Const(int32(ht.Scratch)), ht.I32Const(64)), errInvalid),
		ht.I32Const(0))
	p := headersPlugin(g.Encode(), v1alpha1.CapabilityCryptoUse)
	p.schema = keySchema()
	p.config = map[string]any{"signing": ref("/etc/ruralz/es256.pem"), "mac": ref("/etc/ruralz/mac"), "token": ref("/etc/ruralz/token")}
	f := h.one(p, buildOpts{secrets: secrets})
	if r := handle(f, phase.OnRequestHeaders, exchange(nil)); r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue", r)
	}
	if got := string(f.pool.config); got != `{"mac":1,"signing":2,"token":"upstream-token-value"}` {
		t.Errorf("rz_configure input = %s, want handles and the secret's value", got)
	}
	waitIdle(t, f.pool)
	inst := <-f.pool.idle
	defer func() { f.pool.idle <- inst }()
	memory, _ := inst.mod.Memory().Read(0, inst.mod.Memory().Size())
	block, _ := pem.Decode(keyPEM)
	for _, secretBytes := range [][]byte{privBytes(t, priv), block.Bytes, macKey[:16]} {
		if bytes.Contains(memory, secretBytes) {
			t.Fatal("key bytes reached guest memory")
		}
	}
}

// TestKeyOperationDenied: an operation the key's declaration lacks is the
// sticky ERR_DENIED.
func TestKeyOperationDenied(t *testing.T) {
	h := newHarness(t)
	g := ht.NewGuest("crypto_key_sign")
	g.Phase("rz_on_request_headers", nil,
		ht.Check(g.Call("crypto_key_sign", ht.I32Const(1), g.Str("m"), ht.I32Const(int32(ht.Scratch)), ht.I32Const(64)), errDenied), ht.I32Const(0))
	p := headersPlugin(g.Encode(), v1alpha1.CapabilityCryptoUse)
	p.schema = map[string]any{"properties": map[string]any{"mac": map[string]any{"x-ruralz-key": map[string]any{"algorithm": "HS256", "operations": []any{"hmac"}}}}}
	p.config = map[string]any{"mac": ref("/k")}
	f := h.one(p, buildOpts{secrets: secretTable{{Provider: "file", Name: "/k"}: []byte("0123456789abcdef")}})
	if r := handle(f, phase.OnRequestHeaders, exchange(nil)); codeOf(r) != CodeMisuse {
		t.Fatalf("Handle = %+v, want %s", r, CodeMisuse)
	}
}

// TestSecretFields covers the configure input's errors: a literal in a
// secret field, an unresolved reference and key material that does not
// fit its algorithm.
func TestSecretFields(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name    string
		config  map[string]any
		secrets secretTable
		msg     string
	}{
		{name: "a literal", config: map[string]any{"token": "plaintext"}, msg: "not a {secretRef"},
		{name: "unresolved", config: map[string]any{"token": ref("/missing")}, secrets: secretTable{}, msg: "not resolved"},
		{name: "a key of another type", config: map[string]any{"signing": ref("/k")}, secrets: secretTable{{Provider: "file", Name: "/k"}: []byte("not a key")}, msg: "key material"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := headersPlugin(continueGuest(), v1alpha1.CapabilityCryptoUse)
			p.name, p.policy, p.schema, p.config = "p", "pol", keySchema(), tc.config
			_, err := h.build(h.bundleOf(p), "pol", buildOpts{secrets: tc.secrets})
			wantCode(t, err, codeSecret)
			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("Build = %q, want it to mention %q", err, tc.msg)
			}
		})
	}
}

// privBytes returns an ECDSA key's scalar.
func privBytes(t *testing.T, k *ecdsa.PrivateKey) []byte {
	t.Helper()
	b, err := k.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
