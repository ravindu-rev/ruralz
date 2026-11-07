// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package configschema

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestFields reads the markers on properties, array items and
// additionalProperties at any depth, in path order, with each key field's
// declaration; an unmarked schema has none.
func TestFields(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"token":  map[string]any{"x-ruralz-secret": true},
			"signer": map[string]any{"x-ruralz-key": map[string]any{"algorithm": "ES256", "operations": []any{"sign", "verify"}}},
			"plain":  map[string]any{"type": "string"},
			"upstreams": map[string]any{
				"type": "array",
				"items": map[string]any{
					"properties": map[string]any{"password": map[string]any{"x-ruralz-secret": true}},
				},
			},
			"macs": map[string]any{
				"additionalProperties": map[string]any{"x-ruralz-key": map[string]any{"algorithm": "HS256", "operations": []any{"hmac"}}},
			},
		},
	}
	got, err := Fields(schema)
	if err != nil {
		t.Fatal(err)
	}
	want := []Field{
		{Path: []string{"macs", "{}"}, Key: true, Algorithm: "HS256", Operations: []string{"hmac"}},
		{Path: []string{"signer"}, Key: true, Algorithm: "ES256", Operations: []string{"sign", "verify"}},
		{Path: []string{"token"}},
		{Path: []string{"upstreams", "[]", "password"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Fields = %+v, want %+v", got, want)
	}
	if fs, err := Fields(map[string]any{"properties": map[string]any{"a": map[string]any{"type": "string"}}}); err != nil || len(fs) != 0 {
		t.Errorf("Fields(unmarked) = %v, %v, want none", fs, err)
	}
	if fs, err := Fields(nil); err != nil || len(fs) != 0 {
		t.Errorf("Fields(nil) = %v, %v, want none", fs, err)
	}
}

// TestFieldsRefusesMalformedMarkers: an x-ruralz-secret other than true,
// and an x-ruralz-key that is no object, names an algorithm outside the
// JWA list, has no operations, names an unknown operation, or an
// operation its keys do not support, all fail with ErrMarker, naming the
// field.
func TestFieldsRefusesMalformedMarkers(t *testing.T) {
	key := func(v any) map[string]any {
		return map[string]any{"properties": map[string]any{"f": map[string]any{"x-ruralz-key": v}}}
	}
	cases := []struct {
		name   string
		schema map[string]any
		want   string
	}{
		{"secret false", map[string]any{"properties": map[string]any{"f": map[string]any{"x-ruralz-secret": false}}}, "x-ruralz-secret at config.f"},
		{"secret string", map[string]any{"properties": map[string]any{"f": map[string]any{"x-ruralz-secret": "true"}}}, "the value is not true"},
		{"key not an object", key("ES256"), "x-ruralz-key at config.f"},
		{"unknown algorithm", key(map[string]any{"algorithm": "none", "operations": []any{"sign"}}), `algorithm "none"`},
		{"no algorithm", key(map[string]any{"operations": []any{"sign"}}), `algorithm ""`},
		{"no operations", key(map[string]any{"algorithm": "ES256"}), "no operations"},
		{"empty operations", key(map[string]any{"algorithm": "ES256", "operations": []any{}}), "no operations"},
		{"unknown operation", key(map[string]any{"algorithm": "ES256", "operations": []any{"encrypt"}}), `operation "encrypt"`},
		{"hmac on a signature key", key(map[string]any{"algorithm": "RS256", "operations": []any{"hmac"}}), "RS256 keys do not support hmac"},
		{"sign on an HMAC key", key(map[string]any{"algorithm": "HS512", "operations": []any{"sign"}}), "HS512 keys do not support sign"},
		{"nested in items", map[string]any{"items": map[string]any{"x-ruralz-secret": 1}}, "x-ruralz-secret at config.[]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Fields(tc.schema)
			if !errors.Is(err, ErrMarker) {
				t.Fatalf("Fields = %v, want ErrMarker", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Fields error %q does not name %q", err, tc.want)
			}
		})
	}
}

// TestSupports: HMAC algorithms support hmac alone, signature algorithms
// sign and verify alone, and an unknown algorithm nothing.
func TestSupports(t *testing.T) {
	for _, alg := range Algorithms() {
		hmacAlg := strings.HasPrefix(alg, "HS")
		if Supports(alg, OpHMAC) != hmacAlg || Supports(alg, OpSign) == hmacAlg || Supports(alg, OpVerify) == hmacAlg {
			t.Errorf("Supports(%s) = hmac %v, sign %v, verify %v", alg, Supports(alg, OpHMAC), Supports(alg, OpSign), Supports(alg, OpVerify))
		}
	}
	if Supports("none", OpSign) || Supports("HS1", OpHMAC) {
		t.Error("an algorithm outside the JWA list supports an operation")
	}
}

// TestRefs returns every value the marked fields hold, by instance path
// with array indexes and sorted member names: a {secretRef: ...} reads as
// its reference, anything else, an incomplete or extended reference
// included, as a literal; an absent field yields nothing.
func TestRefs(t *testing.T) {
	fields := []Field{
		{Path: []string{"macs", "{}"}, Key: true, Algorithm: "HS256", Operations: []string{"hmac"}},
		{Path: []string{"token"}},
		{Path: []string{"upstreams", "[]", "password"}},
		{Path: []string{"absent"}},
	}
	ref := func(provider, name, key string) map[string]any {
		r := map[string]any{"provider": provider, "name": name}
		if key != "" {
			r["key"] = key
		}
		return map[string]any{"secretRef": r}
	}
	cfg := map[string]any{
		"token": ref("env", "TOKEN", ""),
		"macs": map[string]any{
			"b": ref("file", "/etc/b", "k"),
			"a": "literal-mac-key",
		},
		"upstreams": []any{
			map[string]any{"password": ref("vault", "kv/u0", "pw")},
			map[string]any{"password": map[string]any{"secretRef": map[string]any{"provider": "env"}}},
			map[string]any{"password": map[string]any{"secretRef": map[string]any{"provider": "env", "name": "X", "extra": "y"}}},
			map[string]any{},
		},
	}
	got := Refs(fields, cfg)
	type row struct {
		at      string
		ref     v1alpha1.SecretRef
		literal bool
	}
	var rows []row
	for _, r := range got {
		rows = append(rows, row{PathText(r.At), r.Ref, r.Literal})
	}
	want := []row{
		{"config.macs.a", v1alpha1.SecretRef{}, true},
		{"config.macs.b", v1alpha1.SecretRef{Provider: "file", Name: "/etc/b", Key: "k"}, false},
		{"config.token", v1alpha1.SecretRef{Provider: "env", Name: "TOKEN"}, false},
		{"config.upstreams.0.password", v1alpha1.SecretRef{Provider: "vault", Name: "kv/u0", Key: "pw"}, false},
		{"config.upstreams.1.password", v1alpha1.SecretRef{}, true},
		{"config.upstreams.2.password", v1alpha1.SecretRef{}, true},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("Refs = %+v, want %+v", rows, want)
	}
	if got[0].Field.Algorithm != "HS256" {
		t.Errorf("Refs carries field %+v, want the key declaration", got[0].Field)
	}
}

// TestPathText writes config, then the members joined by dots.
func TestPathText(t *testing.T) {
	if got := PathText(nil); got != "config" {
		t.Errorf("PathText(nil) = %q", got)
	}
	if got := PathText([]string{"a", "[]", "b"}); got != "config.a.[].b" {
		t.Errorf("PathText = %q", got)
	}
}

// TestClip bounds an authored value in a message at 64 code points.
func TestClip(t *testing.T) {
	long := strings.Repeat("é", 100)
	if got := clip(long); got != strings.Repeat("é", 64)+"…" {
		t.Errorf("clip(100 code points) = %q", got)
	}
	if got := clip("HS1"); got != "HS1" {
		t.Errorf("clip(short) = %q", got)
	}
}
