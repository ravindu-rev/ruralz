// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package configschema reads the secret and key fields of a Plugin
// configSchema (docs/architecture/05-wasm-plugin-system.md "Host Function
// rules", Cryptography, keys and secrets): a property marked
// x-ruralz-secret: true holds a SecretValue whose Node-resolved value
// rz_configure receives in its place; a property marked x-ruralz-key:
// {algorithm, operations} holds a SecretValue whose key stays in the host,
// rz_configure receiving the key's handle in its place. Stage H of the
// configuration pipeline lists their secret uses and refuses a literal in
// one; the Plugin host resolves them. It imports only the configuration
// types, so every binary links it.
package configschema

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Schema keywords of the markers.
const (
	KeywordSecret = "x-ruralz-secret"
	KeywordKey    = "x-ruralz-key"
)

// Operations of x-ruralz-key.operations.
const (
	OpSign   = "sign"
	OpVerify = "verify"
	OpHMAC   = "hmac"
)

// Algorithms returns the JWA algorithms a key field may declare: the HMAC
// algorithms, for hmac, and the signature algorithms, for sign and verify.
func Algorithms() []string {
	return []string{
		"HS256", "HS384", "HS512", "RS256", "RS384", "RS512", "PS256", "PS384", "PS512",
		"ES256", "ES384", "ES512", "EdDSA",
	}
}

// Supports reports whether alg keys support operation op.
func Supports(alg, op string) bool {
	if !slices.Contains(Algorithms(), alg) {
		return false
	}
	if strings.HasPrefix(alg, "HS") {
		return op == OpHMAC
	}
	return op == OpSign || op == OpVerify
}

// Field is one secret or key field a configSchema marks.
type Field struct {
	// Path is the field's config path: member names, and "[]" for every
	// array element or "{}" for every member of additionalProperties.
	Path []string
	// Key marks an x-ruralz-key field; Algorithm and Operations are its
	// declaration.
	Key        bool
	Algorithm  string
	Operations []string
}

// ErrMarker reports a marker the configSchema states wrongly.
var ErrMarker = errors.New("malformed marker")

// Fields returns the fields a configSchema marks, in path order: markers on
// properties, array items and additionalProperties, at any depth. A
// malformed marker fails: an x-ruralz-secret other than true, which would
// leave a mistyped secret field unprotected, or an x-ruralz-key whose
// algorithm is outside Algorithms or whose operations its keys do not
// support.
func Fields(schema map[string]any) ([]Field, error) {
	var out []Field
	if err := walk(schema, nil, &out); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.Join(out[i].Path, ".") < strings.Join(out[j].Path, ".") })
	return out, nil
}

func walk(schema map[string]any, path []string, out *[]Field) error {
	if schema == nil {
		return nil
	}
	if v, ok := schema[KeywordSecret]; ok {
		if b, _ := v.(bool); !b {
			return fmt.Errorf("%s at %s: %w: the value is not true", KeywordSecret, PathText(path), ErrMarker)
		}
		*out = append(*out, Field{Path: slices.Clone(path)})
		return nil
	}
	if v, ok := schema[KeywordKey]; ok {
		f, err := keyField(v)
		if err != nil {
			return fmt.Errorf("%s at %s: %w", KeywordKey, PathText(path), err)
		}
		f.Path = slices.Clone(path)
		*out = append(*out, f)
		return nil
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		names := make([]string, 0, len(props))
		for n := range props {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			sub, _ := props[n].(map[string]any)
			if err := walk(sub, append(path, n), out); err != nil {
				return err
			}
		}
	}
	if sub, ok := schema["items"].(map[string]any); ok {
		if err := walk(sub, append(path, "[]"), out); err != nil {
			return err
		}
	}
	if sub, ok := schema["additionalProperties"].(map[string]any); ok {
		if err := walk(sub, append(path, "{}"), out); err != nil {
			return err
		}
	}
	return nil
}

// keyField reads an x-ruralz-key declaration.
func keyField(v any) (Field, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return Field{}, ErrMarker
	}
	alg, _ := m["algorithm"].(string)
	if !slices.Contains(Algorithms(), alg) {
		return Field{}, fmt.Errorf("%w: algorithm %q is not one of %s", ErrMarker, clip(alg), strings.Join(Algorithms(), ", "))
	}
	raw, _ := m["operations"].([]any)
	if len(raw) == 0 {
		return Field{}, fmt.Errorf("%w: no operations", ErrMarker)
	}
	f := Field{Key: true, Algorithm: alg}
	for _, o := range raw {
		s, _ := o.(string)
		if s != OpSign && s != OpVerify && s != OpHMAC {
			return Field{}, fmt.Errorf("%w: operation %q is not sign, verify or hmac", ErrMarker, clip(s))
		}
		if !Supports(alg, s) {
			return Field{}, fmt.Errorf("%w: %s keys do not support %s", ErrMarker, alg, s)
		}
		f.Operations = append(f.Operations, s)
	}
	return f, nil
}

// clip quotes at most 64 code points of an authored value.
func clip(s string) string {
	r := []rune(s)
	if len(r) > 64 {
		return string(r[:64]) + "…"
	}
	return s
}

// PathText writes a config path for messages: config, then its members.
func PathText(p []string) string {
	if len(p) == 0 {
		return "config"
	}
	return "config." + strings.Join(p, ".")
}

// FieldRef is the value one marked field holds at one config path: its
// secretRef, or Literal when the value is not a SecretValue.
type FieldRef struct {
	Field Field
	// At is the instance path: member names and array indexes.
	At      []string
	Ref     v1alpha1.SecretRef
	Literal bool
}

// Refs returns every value the marked fields of cfg hold, in path order.
func Refs(fields []Field, cfg map[string]any) []FieldRef {
	var out []FieldRef
	for _, f := range fields {
		visit(cfg, f.Path, nil, func(v any, at []string) {
			ref, ok := secretRefOf(v)
			out = append(out, FieldRef{Field: f, At: slices.Clone(at), Ref: ref, Literal: !ok})
		})
	}
	return out
}

// visit calls fn for every value v holds at path.
func visit(v any, path, at []string, fn func(v any, at []string)) {
	if len(path) == 0 {
		fn(v, at)
		return
	}
	switch path[0] {
	case "[]":
		arr, _ := v.([]any)
		for i, e := range arr {
			visit(e, path[1:], append(at, strconv.Itoa(i)), fn)
		}
	case "{}":
		m, _ := v.(map[string]any)
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			visit(m[n], path[1:], append(at, n), fn)
		}
	default:
		m, _ := v.(map[string]any)
		if e, ok := m[path[0]]; ok {
			visit(e, path[1:], append(at, path[0]), fn)
		}
	}
}

// secretRefOf reads a {secretRef: {provider, name, key}} value.
func secretRefOf(v any) (v1alpha1.SecretRef, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return v1alpha1.SecretRef{}, false
	}
	r, ok := m["secretRef"].(map[string]any)
	if !ok {
		return v1alpha1.SecretRef{}, false
	}
	p, _ := r["provider"].(string)
	n, _ := r["name"].(string)
	k, _ := r["key"].(string)
	if p == "" || n == "" {
		return v1alpha1.SecretRef{}, false
	}
	for member := range r {
		if member != "provider" && member != "name" && member != "key" {
			return v1alpha1.SecretRef{}, false
		}
	}
	return v1alpha1.SecretRef{Provider: v1alpha1.SecretProvider(p), Name: n, Key: k}, true
}
