// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package serviceaccount

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// Tests for service-account key parsing: the fields a jwt-bearer
// assertion uses, both PEM encodings Google and older tools write, each
// refusal with its fixed reason, and a Key that never prints its private
// key.

const testEmail = "orders@shop.iam.gserviceaccount.com"

// The test keys, generated once per test binary.
var (
	keysOnce        sync.Once
	rsaKey, rsaWeak *rsa.PrivateKey
	ecKey           *ecdsa.PrivateKey
	keysErr         error
)

func keys(t *testing.T) (strong, weak *rsa.PrivateKey, ec *ecdsa.PrivateKey) {
	t.Helper()
	keysOnce.Do(func() {
		if rsaKey, keysErr = rsa.GenerateKey(rand.Reader, MinRSABits); keysErr != nil {
			return
		}
		if rsaWeak, keysErr = rsa.GenerateKey(rand.Reader, MinRSABits-512); keysErr != nil {
			return
		}
		ecKey, keysErr = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	})
	if keysErr != nil {
		t.Fatal(keysErr)
	}
	return rsaKey, rsaWeak, ecKey
}

// pkcs8 returns key as a PEM PRIVATE KEY block.
func pkcs8(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// keyJSON returns a service-account JSON key with members m over the
// defaults of a Google-issued key.
func keyJSON(t *testing.T, privateKey string, m map[string]any) []byte {
	t.Helper()
	v := map[string]any{ //nolint:gosec // G101: a test key built from a generated private key, not a credential
		"type": "service_account", "project_id": "shop", "private_key_id": "0123abcd",
		"private_key": privateKey, "client_email": testEmail, "client_id": "1234567890",
		"auth_uri": "https://accounts.google.com/o/oauth2/auth", "token_uri": "https://oauth2.googleapis.com/token",
		"universe_domain": "googleapis.com",
	}
	for k, x := range m {
		if x == nil {
			delete(v, k)
			continue
		}
		v[k] = x
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestParse checks a Google-issued key, PKCS #8, and a PKCS #1 one: the
// account, the key ID and the private key are read, other members are
// ignored, and a key without private_key_id has an empty KeyID.
func TestParse(t *testing.T) {
	strong, _, _ := keys(t)
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(strong)}))
	for _, tc := range []struct {
		name  string
		in    []byte
		keyID string
	}{
		{"PKCS #8", keyJSON(t, pkcs8(t, strong), nil), "0123abcd"},
		{"PKCS #1", keyJSON(t, pkcs1, nil), "0123abcd"},
		{"no key ID", keyJSON(t, pkcs8(t, strong), map[string]any{"private_key_id": nil}), ""},
		{"unknown member", keyJSON(t, pkcs8(t, strong), map[string]any{"x-extra": []int{1}}), "0123abcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, err := Parse(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if k.Email != testEmail || k.KeyID != tc.keyID || !k.Private.Equal(strong) {
				t.Errorf("Parse = %s, private key equal %v; want %s key %q", k, k.Private.Equal(strong), testEmail, tc.keyID)
			}
		})
	}
}

// TestParseRefusals checks every refusal and its fixed reason.
func TestParseRefusals(t *testing.T) {
	strong, weak, ec := keys(t)
	encrypted := string(pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("x")}))
	garbled := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("garbage")}))
	for _, tc := range []struct {
		name string
		in   []byte
		want error
	}{
		{"empty", nil, ErrNotJSON},
		{"not JSON", []byte(pkcs8(t, strong)), ErrNotJSON},
		{"JSON array", []byte(`[]`), ErrNotJSON},
		{"no type", keyJSON(t, pkcs8(t, strong), map[string]any{"type": nil}), ErrType},
		{"user credentials", keyJSON(t, pkcs8(t, strong), map[string]any{"type": "authorized_user"}), ErrType},
		{"external account", keyJSON(t, pkcs8(t, strong), map[string]any{"type": "external_account"}), ErrType},
		{"no email", keyJSON(t, pkcs8(t, strong), map[string]any{"client_email": nil}), ErrNoEmail},
		{"no private key", keyJSON(t, "", nil), ErrNoKey},
		{"private key not PEM", keyJSON(t, "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC", nil), ErrNoKey},
		{"encrypted private key", keyJSON(t, encrypted, nil), ErrNoKey},
		{"certificate instead of a key", keyJSON(t, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")})), nil), ErrNoKey},
		{"garbled PKCS #8", keyJSON(t, garbled, nil), ErrKeyEncoding},
		{"garbled PKCS #1", keyJSON(t, strings.Replace(garbled, "PRIVATE KEY", "RSA PRIVATE KEY", 2), nil), ErrKeyEncoding},
		{"EC key", keyJSON(t, pkcs8(t, ec), nil), ErrNotRSA},
		{"short RSA key", keyJSON(t, pkcs8(t, weak), nil), ErrKeySize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, err := Parse(tc.in)
			if !errors.Is(err, tc.want) || k != nil {
				t.Fatalf("Parse = %v, %v; want %v", k, err, tc.want)
			}
		})
	}
}

// TestKeyNeverPrintsItsKey checks that every way of printing a Key, and of
// logging one, names the account and the key ID but no key material.
func TestKeyNeverPrintsItsKey(t *testing.T) {
	strong, _, _ := keys(t)
	k, err := Parse(keyJSON(t, pkcs8(t, strong), nil))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("loaded", "key", *k, "ptr", k)
	d := strings.TrimLeft(fmt.Sprint(strong.D), "-")[:12]
	for _, out := range []string{k.String(), fmt.Sprintf("%v %+v %#v %s", *k, *k, *k, k), logs.String()} {
		if !strings.Contains(out, testEmail) || !strings.Contains(out, "0123abcd") {
			t.Errorf("output %q does not name the account and key ID", out)
		}
		if strings.Contains(out, d) || strings.Contains(out, "PRIVATE") {
			t.Errorf("output %q holds key material", out)
		}
	}
}
