// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/phase"
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
// including ones Validate only meets in unusual trees (07 req 81, the
// keyword location of a refusal).
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

// TestValidateNumberBounds is the regression test of the minor review
// finding that the exported Validate passed any number to the library,
// which panics on an exponent past about 10^6 (proposed number bounds,
// package documentation): a number the library would parse outside the
// bounds is not ok at the root, without panicking, while one it never
// parses validates.
//
// Requirements: 07 req 80.
func TestValidateNumberBounds(t *testing.T) {
	s := mustCompile(t, `{"maximum":10,"properties":{"id":{"type":"string"}}}`)
	if loc, ok := s.Validate(json.Number("1e9999999999")); ok || loc != "" {
		t.Errorf("Validate(1e9999999999) = %q, %v, want not ok at the root", loc, ok)
	}
	if _, ok := s.Validate(map[string]any{"note": json.Number("1e9999999999")}); !ok {
		t.Error("a number no keyword parses was refused")
	}
}

// TestValidateMapModel checks that the decoder's data model is the one the
// library expects: json.Number, map[string]any, []any (07 req 80).
func TestValidateMapModel(t *testing.T) {
	s := mustCompile(t, `{"type":"object","properties":{"n":{"type":"number"},"l":{"type":"array"}}}`)
	if _, ok := s.Validate(map[string]any{"n": json.Number("1"), "l": []any{}}); !ok {
		t.Error("the decoded data model does not validate")
	}
}

// walkCases are failing bodies whose error trees the location walk
// reads: a dense array failing at a short location, at one long location
// reached through an anchor (review finding: a 250,000-byte $defs name),
// and at two long locations that differ only at their end, which the walk
// compares in full.
func walkCases(nameLen, items int) []struct{ name, schema, body string } {
	long := strings.Repeat("n", nameLen)
	return []struct{ name, schema, body string }{
		{"short", `{"items":{"type":"integer"}}`, denseArray(items, `"x"`, 0)},
		{"one long", `{"type":"array","items":{"$ref":"#a"},"$defs":{"` + long + `":{"$anchor":"a","type":"integer"}}}`, denseArray(items, `"x"`, 0)},
		{
			"two long", `{"type":"array","items":{"anyOf":[{"$ref":"#a"},{"$ref":"#b"}]},"$defs":{"` + long + `a":{"$anchor":"a","type":"integer"},"` +
				long + `b":{"$anchor":"b","type":"integer"}}}`, denseArray(items, `"x"`, 0),
		},
	}
}

// BenchmarkLocationWalk measures the location walk alone against what the
// scan charges it (walkWork, 07 section 9 item 21): ns/unit must stay
// below 1 on the reference machine.
func BenchmarkLocationWalk(b *testing.B) {
	for _, nameLen := range []int{10, 20000} {
		for _, tc := range walkCases(nameLen, 5000) {
			b.Run(fmt.Sprintf("%s/%d", tc.name, nameLen), func(b *testing.B) {
				s := mustCompile(b, tc.schema)
				v, scan := scanBody(b, s, tc.body)
				var verr *sjs.ValidationError
				if !errors.As(s.root.Validate(v), &verr) {
					b.Fatal("the body is valid")
				}
				for b.Loop() {
					_, _, _ = locationOf(verr, nil)
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(scan.walk), "ns/unit")
			})
		}
	}
}

// TestHandleLocationWalk is the regression test of the review finding
// that the location pass's walk over the error tree reads every leaf's
// location, so its work grows with the leaves times the length of the
// location, which the work model did not charge and nothing could stop: a
// 160 KB body failing under a 250,000-byte $defs name held Handle for 23 s
// (07 section 9 item 21). The scan charges the walk walkWork per error
// unit; when the decision pass, the location pass and the walk do not fit
// the work limit together, the body is refused with DetailMismatch alone,
// as when the request ends during the walk. Through Factory.Build and
// Handle at the 10 MiB default.
func TestHandleLocationWalk(t *testing.T) {
	cases := walkCases(120000, 40000)
	f := buildFilter(t, cases[2].schema, 0)
	_, scan := scanBody(t, f.schema, cases[2].body)
	if leaves := int64(2 * 40000); scan.walk < leaves*120000 {
		t.Errorf("the walk is charged %d, less than %d leaves of 120,000 bytes", scan.walk, leaves)
	}
	scan.workLimit = f.limits.work
	if got := detailOf(t, handle(t, f, cases[2].body)); got != DetailMismatch || !scan.locateOver() || scan.workOver() {
		t.Errorf("Handle(%d failing items under long locations) = %q, want DetailMismatch alone", 40000, got)
	}
	// A few items fit: the location of the first failing leaf, cut back to
	// the longest ancestor within maxDetailLocation.
	small := denseArray(300, `"x"`, 0)
	if got := detailOf(t, handle(t, f, small)); got != DetailMismatch+" at /$defs" {
		t.Errorf("Handle(300 failing items) = %.60q, want the location", got)
	}
	// The request ends during the walk, after Handle's and the location
	// pass's checks: the decision alone.
	x, _ := exchange("application/json", []byte(small))
	if got := detailOf(t, f.Handle(&endingContext{Context: t.Context(), live: 2}, phase.OnRequestBody, x)); got != DetailMismatch {
		t.Errorf("context ended during the walk: %.60q, want DetailMismatch alone", got)
	}
}
