// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
)

// typedMapIndex returns the ruralz/v1alpha1 rendered view with Environment
// spec.variables a typed map of objects (the definition TypedMapValue),
// with the key base declared too: a Duration member timeout, a member
// mode with the default "fast", an object limits whose member burst has
// the default 5, and a set hosts.
// The committed schema has no typed map whose values are not strings, so
// this is the only way to reach a typed-map value schema; 02 req 10 says
// one needs no code change.
func typedMapIndex(t *testing.T) *schemaidx.Index {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(schema.RenderedV1alpha1()))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	defs, _ := doc["$defs"].(map[string]any)
	defs["TypedMapValue"] = map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"timeout": map[string]any{"$ref": "#/$defs/Duration"},
			"mode":    map[string]any{"type": "string", "default": "fast"},
			"limits": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{"burst": map[string]any{"type": "integer", "default": json.Number("5")}},
			},
			"hosts": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "x-ruralz-list": map[string]any{"type": "set"}},
		},
	}
	env, _ := defs["EnvironmentSpec"].(map[string]any)
	props, _ := env["properties"].(map[string]any)
	vars, _ := props["variables"].(map[string]any)
	vars["additionalProperties"] = map[string]any{"$ref": "#/$defs/TypedMapValue"}
	// One key declared as well: still a typed map, whose keys a decode
	// matches exactly (no case folding).
	vars["properties"] = map[string]any{"base": map[string]any{"$ref": "#/$defs/TypedMapValue"}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := schemaidx.Load(v1, data)
	if err != nil {
		t.Fatalf("schemaidx.Load: %v", err)
	}
	return idx
}

// environment returns an Environment whose spec is spec.
func environment(spec string) string {
	return `{"apiVersion": "ruralz/v1alpha1", "kind": "Environment", "metadata": {"name": "e"}, "spec": ` + spec + `}`
}

// TestTypedMapValues covers typed-map values in every step that reads a
// member's schema through memberSchema (02 req 9 to 11 and 15 to 17;
// WP-37 review round 5: dropping memberSchema's typed-map branch passed
// every test, since every typed map of the committed schema has string
// values): Materialize writes the defaults inside each present value and
// its present objects, and no absent value or object is created;
// Normalize rewrites a Duration and sorts a set inside a value, reports a
// duration out of range and a member name that folds onto a declared one
// at <map>.<key>.<member>, but not a key of the map that folds onto its
// declared key (a Go map matches keys exactly); and the declared-only
// filter of a config decode passes a value's declared members only.
func TestTypedMapValues(t *testing.T) {
	idx := typedMapIndex(t)
	t.Run("materialize and normalize", func(t *testing.T) {
		f := newFixture()
		res := f.resource(t, "e.json", environment(`{"variables": {"a": {"timeout": "90s"}, "b": {"mode": "slow"}, "c": {"limits": {}, "hosts": ["b", "a"]}}}`))
		if got := Materialize(idx, nil, res); got != 3 {
			t.Errorf("Materialize added %d members, want 3 (a.mode, c.mode, c.limits.burst)", got)
		}
		if ds := Normalize(idx, res, f.files); len(ds) != 0 {
			t.Fatalf("Normalize: %q", texts(ds))
		}
		want := `{"a":{"mode":"fast","timeout":"1m30s"},"b":{"mode":"slow"},"c":{"hosts":["a","b"],"limits":{"burst":5},"mode":"fast"}}`
		if got := dump(at(t, res.Root, "spec.variables")); got != want {
			t.Errorf("spec.variables = %s\nwant %s", got, want)
		}
		empty := f.resource(t, "e2.json", environment(`{}`))
		if got := Materialize(idx, nil, empty); got != 0 || dump(empty.Root) != dump(f.resource(t, "e3.json", environment(`{}`)).Root) {
			t.Errorf("Materialize of an Environment without variables added %d: %s", got, dump(empty.Root))
		}
	})
	t.Run("normalize errors", func(t *testing.T) {
		f := newFixture()
		res := f.resource(t, "e.json", environment(`{"variables": {"x": {"timeout": "9999999999h"}, "y": {"Mode": "m"}, "Base": {}, "base": {}}}`))
		var got []string
		for _, d := range Normalize(idx, res, f.files) {
			got = append(got, d.Path.String())
		}
		if want := []string{"spec.variables.x.timeout", "spec.variables.y.Mode"}; !slices.Equal(got, want) {
			t.Errorf("Normalize paths = %q, want %q", got, want)
		}
	})
	t.Run("declared-only filter", func(t *testing.T) {
		spec, _ := idx.Spec("Environment")
		vars, _ := spec.Property("variables")
		n := mustParse(t, `{"a": {"mode": "m", "extra": 1, "limits": {"burst": 2, "more": 3}}}`)
		got, err := appendJSON(nil, n, &declaredOnly{vars}, nil)
		if want := `{"a":{"mode":"m","limits":{"burst":2}}}`; err != nil || string(got) != want {
			t.Errorf("appendJSON = %s, %v; want %s", got, err, want)
		}
		var m map[string]map[string]any
		if err := (&chunker{w: newWork(), limit: 1, bytes: decodeBytes}).decode(n, reflect.ValueOf(&m).Elem(), &declaredOnly{vars}); err != nil || len(m["a"]) != 2 || m["a"]["extra"] != nil {
			t.Errorf("chunked decode = %v, %v; want the declared members of the value only", m, err)
		}
	})
}
