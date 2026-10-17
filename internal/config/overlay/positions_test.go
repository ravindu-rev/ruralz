// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestSuppliedPositions covers 01 req 25 "Each result node keeps the
// position of the document that supplied it" for every way a value
// changes hands (review finding): a value the overlay supplies, whether a
// scalar, a list replaced whole, a JSON Schema document or an orderedMap
// list rebuilt by {$patch: replace}, takes the overlay's value and key
// positions; a value the overlay merges into, or whose removals against a
// value of another kind remove nothing, keeps the base's.
func TestSuppliedPositions(t *testing.T) {
	const basePath, overPath = "ruralz.yaml", "overlays/prod/o.yaml"
	policy := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"v"},"spec":{"type":"validation.json-schema","config":{"schema":{"type":"object"}}}}`
	cases := []struct {
		name, base, over, path string
		// from names the file supplying the value and the key; needle is
		// the text whose first occurrence in that file starts the value,
		// keyNeedle the key.
		from, needle, keyNeedle string
	}{
		{
			"req25 orderedMap list rebuilt by {$patch: replace}", gateway,
			`{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"policies":[{"$patch":"replace"},{"name":"auth"}]}}`,
			"spec.policies", overPath, `[{"$patch"`, `"policies"`,
		},
		{
			"req25 orderedMap list merged in place", gateway,
			`{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"policies":[{"name":"auth"}]}}`,
			"spec.policies", basePath, `[{"name":"cors"}`, `"policies"`,
		},
		{
			"req25 set list replaced whole", gateway,
			`{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"trustedProxies":["172.16.0.0/12"]}}`,
			"spec.trustedProxies", overPath, `["172.16.0.0/12"]`, `"trustedProxies"`,
		},
		{
			"req25 scalar replaced", route,
			`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"timeout":"9s"}}`,
			"spec.timeout", overPath, `"9s"`, `"timeout"`,
		},
		{
			"req25 object replacing a scalar", route,
			`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"timeout":{"a":1}}}`,
			"spec.timeout", overPath, `{"a":1}`, `"timeout"`,
		},
		{
			"req25 removals against a scalar keep the base", route,
			`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"timeout":{"a":null}}}`,
			"spec.timeout", basePath, `"5s"`, `"timeout"`,
		},
		{
			"req25 object merged into", route,
			`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"methods":["GET"]}}}`,
			"spec.match", basePath, `{"path"`, `"match"`,
		},
		{
			"req25 JSON Schema document replaced whole", policy,
			`{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"v"},"spec":{"config":{"schema":{"type":"string"}}}}`,
			"spec.config.schema", overPath, `{"type":"string"}`, `"schema"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ds, files := run(t, []file{{basePath, tc.base}}, []file{{overPath, tc.over}})
			if len(ds) != 0 || len(out) != 1 {
				t.Fatalf("%d resources, diagnostics %q", len(out), texts(ds))
			}
			src := map[string]string{basePath: tc.base, overPath: tc.over}[tc.from]
			parent := out[0].Root
			segs := strings.Split(tc.path, ".")
			for _, seg := range segs[:len(segs)-1] {
				parent = at(t, parent, seg)
			}
			key := segs[len(segs)-1]
			var mem *tree.Member
			for i := range parent.Members {
				if parent.Members[i].Key == key {
					mem = &parent.Members[i]
				}
			}
			if mem == nil {
				t.Fatalf("no member %s", tc.path)
			}
			loc := func(p tree.Pos) string {
				l := files.Location(p)
				return l.File + ":" + strconv.Itoa(l.Line) + ":" + strconv.Itoa(l.Column)
			}
			if got, want := loc(mem.Value.Pos), tc.from+":"+lineColumn(t, src, tc.needle); got != want {
				t.Errorf("%s value at %s, want %s", tc.path, got, want)
			}
			if got, want := loc(mem.KeyPos), tc.from+":"+lineColumn(t, src, tc.keyNeedle); got != want {
				t.Errorf("%s key at %s, want %s", tc.path, got, want)
			}
		})
	}
}

// lineColumn returns the 1-based "line:column" of the first needle in
// src (ASCII fixtures, so bytes are columns).
func lineColumn(t *testing.T, src, needle string) string {
	t.Helper()
	i := strings.Index(src, needle)
	if i < 0 {
		t.Fatalf("%q not in %s", needle, src)
	}
	line := strings.Count(src[:i], "\n") + 1
	return strconv.Itoa(line) + ":" + strconv.Itoa(i-strings.LastIndex(src[:i], "\n"))
}
