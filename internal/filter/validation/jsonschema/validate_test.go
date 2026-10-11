// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/json"
	"testing"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// decodeInstance decodes body as the Filter does.
func decodeInstance(t testing.TB, body string) any {
	t.Helper()
	v, _, err := jsonval.Decode([]byte(body), jsonval.Options{MapObjects: true})
	if err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return v
}

// TestValidateKeywordLocation covers 07 req 81: the detail names the
// absolute keyword location (references resolved) of the first failing
// leaf in pointer order, never instance data.
func TestValidateKeywordLocation(t *testing.T) {
	for _, tc := range []struct {
		name, schema, body, want string
	}{
		{"type", `{"type":"object"}`, `[]`, "/type"},
		{"false root", `false`, `1`, ""},
		{"false property", `{"properties":{"secret":false}}`, `{"secret":1}`, "/properties/secret"},
		{"first of several properties", `{"properties":{"b":{"type":"string"},"a":{"type":"string"},"c":{"minimum":3}}}`, `{"c":1,"b":1,"a":1}`, "/properties/a/type"},
		{"required before a sibling", `{"required":["x"],"properties":{"y":{"type":"string"}}}`, `{"y":1}`, "/properties/y/type"},
		{"required", `{"required":["x"]}`, `{}`, "/required"},
		{"additionalProperties", `{"additionalProperties":false}`, `{"leaked-name":1}`, "/additionalProperties"},
		{"reference resolved", `{"$defs":{"pos":{"type":"integer","minimum":0}},"properties":{"n":{"$ref":"#/$defs/pos"}}}`, `{"n":-1}`, "/$defs/pos/minimum"},
		{"not", `{"not":{"type":"string"}}`, `"x"`, "/not"},
		{"oneOf matched twice", `{"oneOf":[{"type":"integer"},{"minimum":0}]}`, `1`, "/oneOf"},
		{"anyOf leaves", `{"anyOf":[{"type":"string"},{"type":"boolean"}]}`, `1`, "/anyOf/0/type"},
		{"items by numeric order", `{"prefixItems":[true,true,true,true,true,true,true,true,true,true,true,{"type":"string"}],"items":{"type":"string"}}`, `[0,1,2,3,4,5,6,7,8,9,10,11,12]`, "/items/type"},
		{"allOf numeric order", `{"allOf":[true,true,{"maximum":1},true,true,true,true,true,true,true,{"minimum":5}]}`, `3`, "/allOf/2/maximum"},
		{"propertyNames", `{"propertyNames":{"maxLength":3}}`, `{"toolong":1}`, "/propertyNames/maxLength"},
		{"pointer escapes", `{"properties":{"a/b":{"properties":{"~":{"type":"null"}}}}}`, `{"a/b":{"~":1}}`, "/properties/a~1b/properties/~0/type"},
		{"embedded resource", `{"$id":"https://example.com/r","$defs":{"x":{"$id":"x.json","maxLength":1}},"$ref":"x.json"}`, `"ab"`, "/$defs/x/maxLength"},
		{"uniqueItems", `{"uniqueItems":true}`, `[1,1.0]`, "/uniqueItems"},
		{"const", `{"const":{"a":[1]}}`, `{"a":[2]}`, "/const"},
		// The library keeps the legacy dependencies keyword under draft
		// 2020-12 and names it "dependency" in its errors.
		{"dependencies array", `{"dependencies":{"a":["b"]}}`, `{"a":1}`, "/dependencies/a"},
		{"dependencies schema", `{"dependencies":{"a":{"required":["b"]}}}`, `{"a":1}`, "/dependencies/a/required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mustCompile(t, tc.schema)
			v := decodeInstance(t, tc.body)
			for range 20 {
				got, ok := s.Validate(v)
				if ok || got != tc.want {
					t.Fatalf("Validate(%s) = %q, %v, want %q, false", tc.body, got, ok, tc.want)
				}
			}
		})
	}
}

// TestKeywordPointer covers the error shapes keywordPointer maps,
// including ones Validate only meets in unusual trees.
func TestKeywordPointer(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *sjs.ValidationError
		want string
		ok   bool
	}{
		{"nil kind", &sjs.ValidationError{SchemaURL: baseURL + "#"}, "", false},
		{"reference", &sjs.ValidationError{SchemaURL: baseURL + "#/a", ErrorKind: &kind.Reference{Keyword: "$ref", URL: baseURL + "#/$defs/x"}}, "/$defs/x", true},
		{"not", &sjs.ValidationError{SchemaURL: baseURL + "#/p", ErrorKind: &kind.Not{}}, "/p/not", true},
		{"outside", &sjs.ValidationError{SchemaURL: "https://json-schema.org/draft/2020-12/schema#", ErrorKind: &kind.Type{}}, "", false},
		{"keyword", &sjs.ValidationError{SchemaURL: baseURL + "#/properties/a%20b", ErrorKind: &kind.MinLength{}}, "/properties/a b/minLength", true},
		{"dependencies", &sjs.ValidationError{SchemaURL: baseURL + "#", ErrorKind: &kind.Dependency{Prop: "a/b"}}, "/dependencies/a~1b", true},
	} {
		got, ok := keywordPointer(tc.err)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: keywordPointer = %q, %v, want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// TestValidateExactNumbers covers 07 req 80 and T13: numbers stay
// literal, so values beyond a double and large integers validate exactly.
func TestValidateExactNumbers(t *testing.T) {
	for _, tc := range []struct {
		schema, body string
		ok           bool
	}{
		{`{"maximum":1e400}`, `1e400`, true},
		{`{"maximum":1e400}`, `1.0000000000000001e400`, false},
		{`{"exclusiveMaximum":1e308}`, `1e400`, false},
		{`{"multipleOf":10}`, `12345678901234567890`, true},
		{`{"multipleOf":10}`, `12345678901234567891`, false},
		{`{"type":"integer"}`, `12345678901234567890`, true},
		{`{"type":"integer"}`, `1.5e1`, true},
		{`{"type":"integer"}`, `1.25e1`, false},
		{`{"maximum":9007199254740992}`, `9007199254740993`, false},
		{`{"multipleOf":0.01}`, `19.99`, true},
	} {
		s := mustCompile(t, tc.schema)
		if _, ok := s.Validate(decodeInstance(t, tc.body)); ok != tc.ok {
			t.Errorf("schema %s, body %s: ok = %v, want %v", tc.schema, tc.body, ok, tc.ok)
		}
	}
}

// TestValidateMapModel checks that the decoder's data model is the one the
// library expects: json.Number, map[string]any, []any.
func TestValidateMapModel(t *testing.T) {
	s := mustCompile(t, `{"type":"object","properties":{"n":{"type":"number"},"l":{"type":"array"}}}`)
	if _, ok := s.Validate(map[string]any{"n": json.Number("1"), "l": []any{}}); !ok {
		t.Error("the decoded data model does not validate")
	}
}
