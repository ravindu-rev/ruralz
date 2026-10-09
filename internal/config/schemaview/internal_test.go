// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"encoding/json"
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestFieldSyntax checks the documented-format messages selected by
// schema location (01 req 33; CR 145), and the typed-map value path.
func TestFieldSyntax(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want finding
	}{
		{
			"api key hash", envelope("Consumer", `{"credentials": {"apiKeys": [{"name": "k", "hash": "md5:abc"}]}}`),
			finding{CodeSchema, "spec.credentials.apiKeys[name=k].hash", "must be sha256: followed by 64 lower-case hexadecimal digits", ""},
		},
		{
			"basic hash", envelope("Consumer", `{"credentials": {"basic": [{"username": "u", "hash": "plain"}]}}`),
			finding{CodeSchema, "spec.credentials.basic[username=u].hash", "must be pbkdf2-sha256:<salt>:<key> with a 22-character base64url salt and a 43-character base64url key", ""},
		},
		{
			"currency", envelope("AIProvider", `{"dialect": "openai", "pricing": {"currency": "euro", "version": "1"}}`),
			finding{CodeSchema, "spec.pricing.currency", "must be an upper-case ISO 4217 currency code such as EUR", ""},
		},
		{
			"country", policy("authz.geoip", `{"allow": ["de"]}`),
			finding{CodeSchema, "spec.config.allow[item=de]", "must be an upper-case ISO 3166-1 alpha-2 country code such as DE", ""},
		},
		{
			"decimal", envelope("AIProvider", `{"dialect": "openai", "pricing": {"currency": "EUR", "version": "1", "models": [{"model": "m", "inputPerMillionTokens": "1,50"}]}}`),
			finding{CodeSchema, "spec.pricing.models[model=m].inputPerMillionTokens", `must be a decimal number written as a string, such as "1.50"`, ""},
		},
		{
			"name too long", `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "` + strings.Repeat("a", 64) + `"}, "spec": {"type": "cors", "config": {}}}`,
			finding{CodeSchema, "metadata.name", "must be an RFC 1123 label of 1 to 63 characters: lower-case letters, digits and '-', starting and ending with a letter or digit", ""},
		},
		{
			"label value", `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p", "labels": {"tier": 1}}, "spec": {"type": "cors", "config": {}}}`,
			finding{CodeSchema, "metadata.labels.tier", "must be a string, not an integer", "quote the value to make it a string"},
		},
		{
			"otlp endpoint type", gateway(`, "telemetry": {"otlp": {"endpoint": 4317}}`),
			finding{CodeSchema, "spec.telemetry.otlp.endpoint", "must be a string, not an integer", "quote the value to make it a string"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := findings(validateJSON(t, tc.src))
			if !slices.Contains(got, tc.want) {
				t.Errorf("Validate() = %+v, want it to contain %+v", got, tc.want)
			}
		})
	}
}

// TestKeywordParse covers the vocabulary parser on shapes the asserted
// meta-schema rejects first (01 req 32).
func TestKeywordParse(t *testing.T) {
	ok := map[string]any{
		"x-ruralz-ref":         "Upstream",
		"x-ruralz-secret":      true,
		"x-ruralz-cel":         map[string]any{"variables": []any{"request"}, "result": "bool"},
		"x-ruralz-list":        map[string]any{"type": "map", "key": "name"},
		"x-ruralz-impact":      []any{"security"},
		"x-ruralz-since":       json.Number("1"),
		"x-ruralz-validations": []any{map[string]any{"rule": "true"}, map[string]any{"rule": "self.a > 0", "message": "a must be positive"}},
		"type":                 "object",
	}
	ext, err := compileKeywords(nil, ok)
	if err != nil {
		t.Fatalf("compileKeywords() error = %v", err)
	}
	k, _ := ext.(*keywords)
	want := keywords{
		ref: "Upstream", secret: true, celVariables: []string{"request"}, celResult: "bool",
		listType: "map", listKey: "name", impact: []string{"security"}, since: 1,
		validations: []validation{{rule: "true"}, {rule: "self.a > 0", message: "a must be positive"}},
	}
	if k == nil || k.ref != want.ref || !k.secret || k.celResult != "bool" || k.listKey != "name" || k.since != 1 ||
		!slices.Equal(k.validations, want.validations) ||
		!slices.Equal(k.impact, want.impact) || !slices.Equal(k.celVariables, want.celVariables) {
		t.Errorf("compileKeywords() = %+v, want %+v", k, want)
	}
	k.Validate(nil, nil)
	if ext, err := compileKeywords(nil, map[string]any{"type": "string"}); ext != nil || err != nil {
		t.Errorf("compileKeywords(no keywords) = %v, %v, want nil, nil", ext, err)
	}
	for _, bad := range []map[string]any{
		{"x-ruralz-ref": 1},
		{"x-ruralz-secret": "true"},
		{"x-ruralz-secret": false},
		{"x-ruralz-cel": "x"},
		{"x-ruralz-cel": map[string]any{"variables": []any{1}, "result": "bool"}},
		{"x-ruralz-list": "map"},
		{"x-ruralz-list": map[string]any{"type": 1}},
		{"x-ruralz-impact": []any{1}},
		{"x-ruralz-since": "1"},
		{"x-ruralz-since": json.Number("-1")},
		{"x-ruralz-since": json.Number("0")},
		{"x-ruralz-since": json.Number("1.5")},
		{"x-ruralz-validations": map[string]any{}},
		{"x-ruralz-validations": []any{"self.a > 0"}},
		{"x-ruralz-validations": []any{map[string]any{"message": "m"}}},
		{"x-ruralz-validations": []any{map[string]any{"rule": ""}}},
		{"x-ruralz-validations": []any{map[string]any{"rule": "true", "message": 1}}},
		{"x-ruralz-other": true},
	} {
		if _, err := compileKeywords(nil, bad); err == nil {
			t.Errorf("compileKeywords(%v) error = nil, want an error", bad)
		}
	}
}

// TestLocations covers the schema location parser and lookups.
func TestLocations(t *testing.T) {
	v := testView(t)
	for _, tc := range []struct {
		loc  string
		want location
		ok   bool
	}{
		{v.base, location{}, true},
		{v.base + "#", location{}, true},
		{v.base + "#/$defs/OTLP/properties/endpoint", location{"$defs", "OTLP", "properties", "endpoint"}, true},
		{v.base + "#/a~1b/c~0d/%7E", location{"a/b", "c~d", "~"}, true},
		{"urn:other#/$defs/OTLP", nil, false},
		{v.base + "x", nil, false},
		{v.base + "#anchor", nil, false},
		{v.base + "#/%zz", nil, false},
	} {
		got, ok := v.parseLocation(tc.loc)
		if ok != tc.ok || !slices.Equal(got, tc.want) {
			t.Errorf("parseLocation(%q) = %q, %v, want %q, %v", tc.loc, got, ok, tc.want, tc.ok)
		}
	}
	if v.at(v.base+"#/$defs/OTLP") == nil {
		t.Error("at(OTLP) = nil")
	}
	for _, loc := range []string{v.base + "#/$defs/ByteSize/anyOf/9", v.base + "#/$defs/ByteSize/anyOf/x", v.base + "#/title/x", "urn:other"} {
		if got := v.at(loc); got != nil {
			t.Errorf("at(%q) = %v, want nil", loc, got)
		}
	}
	if !(location{"$defs", "OTLP", "properties", "endpoint"}).property("OTLP", "endpoint") || (location{"x"}).property("OTLP", "endpoint") {
		t.Error("property() disagrees")
	}
	if got := typesOf(map[string]any{"type": 1}); got != nil {
		t.Errorf("typesOf(invalid) = %v", got)
	}
	if got := typesOf(map[string]any{"type": []any{"object", "boolean"}}); !slices.Equal(got, []string{"object", "boolean"}) {
		t.Errorf("typesOf(array) = %v", got)
	}
	if _, ok := requiredOnly([]any{map[string]any{"required": []any{}}}); ok {
		t.Error("requiredOnly(empty required) = ok")
	}
	if _, ok := requiredOnly([]any{"x"}); ok {
		t.Error("requiredOnly(non-object) = ok")
	}
}

// TestWordingHelpers covers fallbacks of the wording helpers.
func TestWordingHelpers(t *testing.T) {
	for _, tc := range []struct {
		r    *big.Rat
		want string
	}{
		{big.NewRat(3, 1), "3"},
		{big.NewRat(1, 4), "0.25"},
		{nil, "?"},
	} {
		if got := bound(map[string]any{}, "minimum", tc.r); got != tc.want {
			t.Errorf("bound(%v) = %q, want %q", tc.r, got, tc.want)
		}
	}
	if got := jsonText(func() {}); got != "?" {
		t.Errorf("jsonText(func) = %q", got)
	}
	for k, want := range map[tree.Kind]string{
		tree.KindNull: "null", tree.KindBool: "a boolean", tree.KindInt: "an integer", tree.KindFloat: "a number",
		tree.KindString: "a string", tree.KindMap: "an object", tree.KindList: "an array", tree.Kind(99): "",
	} {
		if got := typeName(&tree.Node{Kind: k}); got != want {
			t.Errorf("typeName(%d) = %q, want %q", k, got, want)
		}
	}
	if got := typeName(nil); got != "" {
		t.Errorf("typeName(nil) = %q", got)
	}
	if got := knownPos(nil, nil); got.Known() {
		t.Errorf("knownPos(nil, nil) = %v", got)
	}
	parent := &tree.Node{Pos: tree.Pos{Line: 2, Column: 3}}
	if got := knownPos(&tree.Node{}, parent); got != parent.Pos {
		t.Errorf("knownPos(unknown, parent) = %v", got)
	}
	m := newMapper(testView(t), &tree.Resource{}, nil, MaxDiagnostics)
	if got := m.location(tree.Pos{}); got.Line != 0 || got.File != "" {
		t.Errorf("location(unknown) = %+v", got)
	}
	if got := memberSchema(nil, "x"); got != nil {
		t.Errorf("memberSchema(nil) = %v", got)
	}
}

// TestTypeMismatch covers the branch classification of anyOf and oneOf.
func TestTypeMismatch(t *testing.T) {
	m := newMapper(testView(t), &tree.Resource{}, nil, MaxDiagnostics)
	typeErr := &jsonschema.ValidationError{InstanceLocation: []string{"a"}, ErrorKind: &kind.Type{Got: "boolean", Want: []string{"integer"}}}
	for _, tc := range []struct {
		name string
		e    *jsonschema.ValidationError
		want []string
		ok   bool
	}{
		{"type", typeErr, []string{"integer"}, true},
		{"wrapped", &jsonschema.ValidationError{ErrorKind: &kind.Reference{}, Causes: []*jsonschema.ValidationError{typeErr}}, []string{"integer"}, true},
		{"deeper", &jsonschema.ValidationError{InstanceLocation: []string{"a", "b"}, ErrorKind: &kind.Type{}}, nil, false},
		{"two causes", &jsonschema.ValidationError{ErrorKind: &kind.Group{}, Causes: []*jsonschema.ValidationError{typeErr, typeErr}}, nil, false},
		{"other", &jsonschema.ValidationError{ErrorKind: &kind.MinLength{}}, nil, false},
	} {
		got, ok := m.typeMismatch(tc.e, 1)
		if ok != tc.ok || !slices.Equal(got, tc.want) {
			t.Errorf("%s: typeMismatch() = %v, %v, want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// TestAtMostOneOf covers the recognition of the generator's atMostOneOf
// forms under not (01 req 33).
func TestAtMostOneOf(t *testing.T) {
	req := func(names ...any) map[string]any { return map[string]any{"required": names} }
	for _, tc := range []struct {
		name string
		sub  any
		want []string
	}{
		{"pair", req("a", "b"), []string{"a", "b"}},
		{"pairs", map[string]any{"anyOf": []any{req("a", "b"), req("a", "c"), req("b", "c")}}, []string{"a", "b", "c"}},
		{"three required", req("a", "b", "c"), nil},
		{"one required", req("a"), nil},
		{"branch of three", map[string]any{"anyOf": []any{req("a", "b"), req("a", "b", "c")}}, nil},
		{"anyOf with a sibling", map[string]any{"anyOf": []any{req("a", "b")}, "type": "object"}, nil},
		{"other branch", map[string]any{"anyOf": []any{req("a", "b"), map[string]any{"type": "string"}}}, nil},
		{"pair with a sibling", map[string]any{"required": []any{"a", "b"}, "type": "object"}, nil},
		{"not an object", true, nil},
	} {
		got, ok := atMostOneOf(tc.sub)
		if ok != (tc.want != nil) || !slices.Equal(got, tc.want) {
			t.Errorf("%s: atMostOneOf() = %v, %v, want %v", tc.name, got, ok, tc.want)
		}
	}
}
