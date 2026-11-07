// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwtsign

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for Build and the signing key's lifecycle: the key resolves
// through secretRef only (RZ-CFG-026 otherwise), follows rotations by
// watch, keeps its last value when a rotation is refused, and survives a
// snapshot swap.

// TestBuildKeyRefusals: an unresolved reference, a table without secrets,
// a public key, a key of another family, a JWK naming another alg and a
// value that is no key are RZ-CFG-026 at Build, never echoing the value,
// and leave no watch running.
func TestBuildKeyRefusals(t *testing.T) {
	k := keys(t)
	cases := []struct {
		name  string
		alg   v1alpha1.JWTSignAlgorithm
		value []byte
		// unresolved leaves the reference out of the table.
		unresolved bool
		reason     string
	}{
		{name: "unresolved", alg: v1alpha1.JWTSignAlgorithmRS256, unresolved: true, reason: "not resolved"},
		{name: "public PEM key", alg: v1alpha1.JWTSignAlgorithmRS256, value: spki(t, k.rsa.Public()), reason: "public key"},
		{name: "public JWK", alg: v1alpha1.JWTSignAlgorithmEdDSA, value: jwk(t, okpMembers(k.ed), map[string]any{"d": nil}), reason: "public key"},
		{name: "RSA key for ES256", alg: v1alpha1.JWTSignAlgorithmES256, value: pkcs8(t, k.rsa), reason: "signs with a P-256 key"},
		{name: "P-256 key for RS256", alg: v1alpha1.JWTSignAlgorithmRS256, value: sec1(t, k.p256), reason: "signs with an RSA key"},
		{name: "P-256 key for EdDSA", alg: v1alpha1.JWTSignAlgorithmEdDSA, value: jwk(t, ecMembers(t, k.p256), nil), reason: "signs with an Ed25519 key"},
		{name: "Ed25519 key for PS256", alg: v1alpha1.JWTSignAlgorithmPS256, value: pkcs8(t, k.ed), reason: "signs with an RSA key"},
		{name: "JWK naming RS256 for PS256", alg: v1alpha1.JWTSignAlgorithmPS256, value: jwk(t, rsaMembers(k.rsa), map[string]any{"alg": "RS256"}), reason: "names alg"},
		{name: "P-384 key for ES256", alg: v1alpha1.JWTSignAlgorithmES256, value: pkcs8(t, k.p384), reason: "not an RSA, P-256 or Ed25519 key"},
		{name: "a password", alg: v1alpha1.JWTSignAlgorithmEdDSA, value: []byte("correct horse battery staple"), reason: "neither a JWK nor a PEM private key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newStore(c.value)
			if c.unresolved {
				st = newStore(nil)
			}
			_, err := Factory{}.Build(t.Context(), buildEnv(t, pathsConfig(c.alg, "", "t"), st))
			if code, _ := errcode.CodeOf(err); code != CodeKey {
				t.Fatalf("Build = %v, want %s", err, CodeKey)
			}
			if !strings.Contains(err.Error(), c.reason) {
				t.Errorf("Build = %v, want the reason %q", err, c.reason)
			}
			if len(c.value) > 12 && strings.Contains(err.Error(), string(c.value[len(c.value)-12:])) {
				t.Errorf("Build = %v repeats the value", err)
			}
			if st.live() != 0 {
				t.Errorf("a refused key left %d watches running", st.live())
			}
		})
	}
	env := buildEnv(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "t"), nil)
	env.Secrets = nil
	if _, err := (Factory{}).Build(t.Context(), env); err == nil {
		t.Fatal("Build without secrets succeeded")
	} else if code, _ := errcode.CodeOf(err); code != CodeKey {
		t.Errorf("Build without secrets = %v, want %s", err, CodeKey)
	}
}

// TestBuildRefusesOtherConfigs: a config of another type is RZ-CFG-005
// and an ended context its error, without a code.
func TestBuildRefusesOtherConfigs(t *testing.T) {
	env := buildEnv(t, nil, newStore(pkcs8(t, keys(t).ed)))
	env.Config = &v1alpha1.AuthMTLSConfig{}
	if _, err := (Factory{}).Build(t.Context(), env); err == nil {
		t.Fatal("Build accepted another config type")
	} else if code, _ := errcode.CodeOf(err); code != CodeConfig {
		t.Errorf("Build = %v, want %s", err, CodeConfig)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (Factory{}).Build(ctx, buildEnv(t, pathsConfig(v1alpha1.JWTSignAlgorithmEdDSA, "", "t"), newStore(pkcs8(t, keys(t).ed)))); !errors.Is(err, context.Canceled) {
		t.Errorf("Build with an ended context = %v", err)
	}
}

// sign runs f on a response holding claims at t and returns the token.
func sign(t *testing.T, f *Filter) string {
	t.Helper()
	x, m := onResponse(t, http.StatusOK, jsonHeader(), `{"t":`+claimsJSON()+`}`)
	wantNext(t, "sign", f.Handle(t.Context(), phase.OnResponse, x))
	return tokenAt(t, m.Raw, "t")
}

// TestRotation: a rotated value that is a fitting private key signs from
// the next request; a refused one, a public key, a key of another family
// or no key, keeps the last key and returns why to the resolver, which
// counts the failure; a later valid value is taken again.
func TestRotation(t *testing.T) {
	k := keys(t)
	st := newStore(pkcs8(t, k.rsa))
	f := build(t, pathsConfig(v1alpha1.JWTSignAlgorithmRS256, "k", "t"), st)
	verify(t, sign(t, f), v1alpha1.JWTSignAlgorithmRS256, k.rsa.Public())

	other := k.rsa2
	if err := st.rotate(jwk(t, rsaMembers(other), nil)); err != nil {
		t.Fatalf("a valid rotation was refused: %v", err)
	}
	verify(t, sign(t, f), v1alpha1.JWTSignAlgorithmRS256, other.Public())

	for name, bad := range map[string][]byte{
		"public key":     spki(t, k.rsa.Public()),
		"another family": pkcs8(t, k.p256),
		"no key":         []byte("rotated by mistake"),
	} {
		err := st.rotate(bad)
		if err == nil {
			t.Fatalf("%s: rotation accepted", name)
		}
		if !strings.Contains(err.Error(), string(Type)) {
			t.Errorf("%s: watch error %q does not name the Policy type", name, err)
		}
		verify(t, sign(t, f), v1alpha1.JWTSignAlgorithmRS256, other.Public())
	}
	if err := st.rotate(pkcs8(t, k.rsa)); err != nil {
		t.Fatalf("the valid value after failures was refused: %v", err)
	}
	verify(t, sign(t, f), v1alpha1.JWTSignAlgorithmRS256, k.rsa.Public())
}

// TestCarryOver: a Filter built with the previous snapshot's Filter of
// the same reference and algorithm shares its key holder, its watch and
// its last accepted key, and the watch stops only when the last Filter
// sharing it closes; another algorithm gets a holder of its own.
func TestCarryOver(t *testing.T) {
	k := keys(t)
	st := newStore(pkcs8(t, k.rsa))
	cfg := pathsConfig(v1alpha1.JWTSignAlgorithmRS256, "k", "t")
	old, err := Factory{}.Build(t.Context(), buildEnv(t, cfg, st))
	if err != nil {
		t.Fatal(err)
	}
	// A refused rotation leaves the old key in the holder while the table
	// holds the refused value.
	if err := st.rotate([]byte("bad")); err == nil {
		t.Fatal("bad rotation accepted")
	}
	env := buildEnv(t, pathsConfig(v1alpha1.JWTSignAlgorithmRS256, "k2", "t"), st)
	env.Previous = old
	next, err := Factory{}.Build(t.Context(), env)
	if err != nil {
		t.Fatalf("Build carrying the holder over: %v", err)
	}
	nf := next.(*Filter)
	if nf.keys != old.(*Filter).keys || st.live() != 1 {
		t.Fatalf("holder not shared (%d watches)", st.live())
	}
	verify(t, sign(t, nf), v1alpha1.JWTSignAlgorithmRS256, k.rsa.Public())
	// The same config without a previous Filter reads the refused value.
	if _, err := (Factory{}).Build(t.Context(), buildEnv(t, cfg, st)); err == nil {
		t.Error("a new holder accepted the refused value")
	}
	if err := old.(filter.Closer).Close(); err != nil {
		t.Fatal(err)
	}
	if st.live() != 1 {
		t.Fatal("closing the old Filter stopped the shared watch")
	}
	if err := st.rotate(pkcs8(t, k.rsa)); err != nil {
		t.Fatal(err)
	}
	verify(t, sign(t, nf), v1alpha1.JWTSignAlgorithmRS256, k.rsa.Public())
	_ = nf.Close()
	_ = nf.Close()
	if st.live() != 0 {
		t.Fatal("the last Close left the watch running")
	}
	// A closed holder is never carried over again.
	again, err := Factory{}.Build(t.Context(), func() filter.BuildEnv { e := buildEnv(t, cfg, st); e.Previous = nf; return e }())
	if err != nil {
		t.Fatal(err)
	}
	if again.(*Filter).keys == nf.keys {
		t.Error("a closed holder was carried over")
	}
	_ = again.(filter.Closer).Close()
	// Another algorithm, or another reference, never shares the holder.
	ps, err := Factory{}.Build(t.Context(), func() filter.BuildEnv {
		e := buildEnv(t, pathsConfig(v1alpha1.JWTSignAlgorithmPS256, "k", "t"), st)
		e.Previous = again
		return e
	}())
	if err != nil {
		t.Fatal(err)
	}
	if ps.(*Filter).keys == again.(*Filter).keys {
		t.Error("a holder of another algorithm was shared")
	}
	_ = ps.(filter.Closer).Close()
}
