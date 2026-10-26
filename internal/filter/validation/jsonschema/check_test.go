// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"bytes"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// policyResource returns a validated validation.json-schema Policy
// resource with schema as its config.schema.
func policyResource(name, schema string) *hub.Resource {
	return &hub.Resource{
		ID:     tree.ID{Kind: v1alpha1.KindPolicy, Name: name},
		Config: &v1alpha1.ValidationJSONSchemaConfig{Schema: v1alpha1.JSONSchemaDocument(schema)},
	}
}

// fakeLocations maps a path to a location whose line is the path length,
// so goldens show which path each diagnostic was located by.
func fakeLocations(p diag.Path) diag.Location {
	return diag.Location{File: "policies/body.yaml", Line: len(p), Column: 3}
}

// runCheck runs Check and returns its diagnostics.
func runCheck(r *hub.Resource) diag.List {
	var out diag.List
	Check(r, fakeLocations, func(d diag.Diagnostic) { out = append(out, d) })
	return out
}

// TestCheckPaths covers the finding paths: Check findings are RZ-CFG-005 at
// key-aware paths under spec.config.schema, an array element as an index,
// located through loc, about the resource.
func TestCheckPaths(t *testing.T) {
	for _, tc := range []struct {
		name, schema, path string
	}{
		{"pattern", `{"properties":{"a.b":{"pattern":"(?=x)"}}}`, `spec.config.schema.properties["a.b"].pattern`},
		{"allOf index", `{"allOf":[true,{"$schema":"http://json-schema.org/draft-07/schema#"}]}`, `spec.config.schema.allOf[1].$schema`},
		{"remote ref", `{"items":{"$ref":"https://example.com/x"}}`, `spec.config.schema.items.$ref`},
		{"fan-out", nestedAnyOf(11), `spec.config.schema.anyOf`},
		{"meta-schema", `{"prefixItems":[{"minItems":-1}]}`, `spec.config.schema.prefixItems[0].minItems`},
		{"size", `{"const":"` + string(bytes.Repeat([]byte("x"), MaxSchemaBytes)) + `"}`, `spec.config.schema`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runCheck(policyResource("body", tc.schema))
			if len(got) != 1 {
				t.Fatalf("Check = %+v, want one diagnostic", got)
			}
			d := got[0]
			if d.Code != CodeConfig || d.Severity != diag.SeverityError || d.Path.String() != tc.path {
				t.Errorf("diagnostic = %s %v %s, want %s error %s", d.Code, d.Severity, d.Path, CodeConfig, tc.path)
			}
			if d.Resource == nil || *d.Resource != (diag.ResourceID{Kind: "Policy", Name: "body"}) {
				t.Errorf("resource = %v", d.Resource)
			}
			if d.Location != fakeLocations(d.Path) {
				t.Errorf("location = %+v, not the one loc returned", d.Location)
			}
			// The Node's compile reports the same finding (all three binaries,
			// identical diagnostics).
			_, err := CompileSchema([]byte(tc.schema))
			var ce *CompileError
			if !errors.As(err, &ce) || len(ce.Issues) != 1 || ce.Issues[0].Message != d.Message {
				t.Errorf("CompileSchema = %v, want the message %q", err, d.Message)
			}
		})
	}
}

// TestCheckSkips covers the resources Check leaves to other stages
// (Check reports config.schema findings only).
func TestCheckSkips(t *testing.T) {
	for _, r := range []*hub.Resource{
		nil,
		{ID: tree.ID{Kind: v1alpha1.KindPolicy, Name: "x"}},
		{ID: tree.ID{Kind: v1alpha1.KindPolicy, Name: "x"}, Config: &v1alpha1.CORSConfig{}},
		{ID: tree.ID{Kind: v1alpha1.KindPolicy, Name: "x"}, Config: (*v1alpha1.ValidationJSONSchemaConfig)(nil)},
		policyResource("empty", ""),
		policyResource("valid", `{"type":"object"}`),
	} {
		if got := runCheck(r); len(got) != 0 {
			t.Errorf("Check(%v) = %+v, want nothing", r, got)
		}
	}
	// A nil add is tolerated, and a nil loc leaves locations unknown.
	Check(policyResource("bad", `{"pattern":"("}`), nil, nil)
	var got diag.List
	Check(policyResource("bad", `{"pattern":"("}`), nil, func(d diag.Diagnostic) { got = append(got, d) })
	if len(got) != 1 || got[0].Location != (diag.Location{}) {
		t.Errorf("Check without loc = %+v", got)
	}
}

// TestGoldenCheckDiagnostics compares the text and JSON diagnostic forms
// of Check findings with goldens, so ruralz, ruralzd and ruralz-control
// print them byte-identically.
func TestGoldenCheckDiagnostics(t *testing.T) {
	cases := []struct{ name, schema string }{
		{"multiple", `{"$schema":"http://json-schema.org/draft-07/schema#","properties":{"zip":{"pattern":"^(?!0)"},"n":{"minimum":1e-5000}},"items":{"$ref":"file:///etc/passwd"}}`},
		// A type entry that is a string the meta-schema refuses, so the
		// library reports it with the other meta-schema finding (an entry
		// that is not a string the walk refuses alone, with the same
		// message: TestNameListsMatchLibrary).
		{"meta", `{"type":["string","seven"],"properties":{"a":{"required":"id"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runCheck(policyResource("body-schema", tc.schema))
			got.Sort()
			var text, js bytes.Buffer
			if err := diag.WriteText(&text, got); err != nil {
				t.Fatal(err)
			}
			if err := diag.WriteJSON(&js, got); err != nil {
				t.Fatal(err)
			}
			golden(t, filepath.Join("testdata", "check", tc.name+".diag.txt"), text.Bytes())
			golden(t, filepath.Join("testdata", "check", tc.name+".diag.json"), js.Bytes())
			again := runCheck(policyResource("body-schema", tc.schema))
			again.Sort()
			if !slices.EqualFunc(got, again, func(a, b diag.Diagnostic) bool { return a.Path.String() == b.Path.String() && a.Message == b.Message }) {
				t.Error("Check is not deterministic")
			}
		})
	}
}
