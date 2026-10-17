// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

type file struct{ path, src string }

// run parses the base and overlay files and applies the overlay.
func run(t *testing.T, base, over []file, extraVersions ...string) ([]*tree.Resource, diag.List, *tree.FileTable) {
	t.Helper()
	files := &tree.FileTable{}
	var b, o []*tree.Resource
	for _, f := range base {
		b = append(b, docs(t, files, tree.RoleBase, f.path, f.src)...)
	}
	for _, f := range over {
		o = append(o, docs(t, files, tree.RoleOverlay, f.path, f.src)...)
	}
	out, ds := pipeline(t, b, o, Options{Schemas: schemaSet(t, extraVersions...), Files: files})
	return out, ds, files
}

const gateway = `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge","labels":{"team":"core"}},
"spec":{"listeners":[{"name":"http","protocol":"http","port":8080},
 {"name":"https","protocol":"https","port":8443,"tls":{"certificates":[{"name":"main","certificate":{"secretRef":{"provider":"file","name":"/c"}},"privateKey":{"secretRef":{"provider":"file","name":"/k"}}}]}},
 {"name":"admin","protocol":"http","port":9000}],
"policies":[{"name":"cors"},{"name":"auth"},{"name":"rl"}],
"trustedProxies":["10.0.0.0/8","192.168.0.0/16"]}}`

const route = `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},
"spec":{"timeout":"5s","match":{"path":{"prefix":"/a"},"when":"true"},"upstreams":[{"name":"u","weight":1}]}}`

// TestMergeRules covers the merge table of 01 req 25 (CM "Overlays"):
// objects merge recursively, an overlay scalar or non-keyed list wins, a
// null removes the member, map lists merge by key and append new entries,
// orderedMap lists merge in place and reorder with {$patch: replace},
// set and atomic lists are replaced, and $patch: delete removes an entry.
func TestMergeRules(t *testing.T) {
	cases := []struct {
		name, base, over, path, want string
	}{
		{
			name: "req25 objects merge, scalar wins, null removes, new members appended",
			base: route,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","labels":{"env":"prod"}},
				"spec":{"timeout":"10s","match":{"when":null,"methods":["GET"]}}}`,
			path: "",
			want: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","labels":{"env":"prod"}},
				"spec":{"timeout":"10s","match":{"path":{"prefix":"/a"},"methods":["GET"]},"upstreams":[{"name":"u","weight":1}]}}`,
		},
		{
			name: "req25 map list merges by key, appends new entries, deletes entries",
			base: gateway,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"listeners":[
				{"name":"https","port":9443},{"name":"grpc","protocol":"http","port":50051},
				{"name":"admin","$patch":"delete"},{"name":"absent","$patch":"delete"}]}}`,
			path: "spec.listeners",
			want: `[{"name":"http","protocol":"http","port":8080},
				{"name":"https","protocol":"https","port":9443,"tls":{"certificates":[{"name":"main","certificate":{"secretRef":{"provider":"file","name":"/c"}},"privateKey":{"secretRef":{"provider":"file","name":"/k"}}}]}},
				{"name":"grpc","protocol":"http","port":50051}]`,
		},
		{
			name: "req25 nested map list inside a map list entry",
			base: gateway,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"listeners":[
				{"name":"https","tls":{"minVersion":"1.2","certificates":[{"name":"main","privateKey":{"secretRef":{"name":"/k2"}}},{"name":"alt"}]}}]}}`,
			path: "spec.listeners.1.tls",
			want: `{"certificates":[{"name":"main","certificate":{"secretRef":{"provider":"file","name":"/c"}},"privateKey":{"secretRef":{"provider":"file","name":"/k2"}}},{"name":"alt"}],"minVersion":"1.2"}`,
		},
		{
			name: "req25 orderedMap merges in place and appends new entries at the end",
			base: gateway,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"policies":[{"name":"rl"},{"name":"extra"},{"name":"cors"}]}}`,
			path: "spec.policies",
			want: `[{"name":"cors"},{"name":"auth"},{"name":"rl"},{"name":"extra"}]`,
		},
		{
			name: "req25 orderedMap reorder with a first {$patch: replace}",
			base: gateway,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"policies":[{"$patch":"replace"},{"name":"rl"},{"name":"cors"}]}}`,
			path: "spec.policies",
			want: `[{"name":"rl"},{"name":"cors"}]`,
		},
		{
			name: "req25 orderedMap delete entry",
			base: gateway,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"policies":[{"name":"auth","$patch":"delete"}]}}`,
			path: "spec.policies",
			want: `[{"name":"cors"},{"name":"rl"}]`,
		},
		{
			name: "req25 set list replaced",
			base: gateway,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"trustedProxies":["172.16.0.0/12"]}}`,
			path: "spec.trustedProxies",
			want: `["172.16.0.0/12"]`,
		},
		{
			name: "req25 atomic list replaced",
			base: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"h"},"spec":{"type":"headers","config":{"request":{"add":[{"name":"a","value":"1"},{"name":"b","value":"2"}]}}}}`,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"h"},"spec":{"config":{"request":{"add":[{"name":"b","value":"3"}]}}}}`,
			path: "spec.config.request.add",
			want: `[{"name":"b","value":"3"}]`,
		},
		{
			name: "req25 empty keyed overlay list changes nothing",
			base: gateway,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"policies":[]}}`,
			path: "spec.policies",
			want: `[{"name":"cors"},{"name":"auth"},{"name":"rl"}]`,
		},
		{
			name: "req25 keyed list added to a base without it consumes the directives",
			base: route,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"policies":[{"$patch":"replace"},{"name":"a","x":null},{"name":"gone","$patch":"delete"}]}}`,
			path: "spec.policies",
			want: `[{"name":"a"}]`,
		},
		{
			name: "req25 a composite replaces a scalar and nulls inside it are dropped",
			base: route,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"timeout":{"a":1,"b":null}}}`,
			path: "spec.timeout",
			want: `{"a":1}`,
		},
		{
			name: "req25 a null for an absent member is a no-op",
			base: route,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"composition":null}}`,
			path: "spec",
			want: `{"timeout":"5s","match":{"path":{"prefix":"/a"},"when":"true"},"upstreams":[{"name":"u","weight":1}]}`,
		},
		{
			name: "req25 config dispatch uses the merged spec.type: issuers becomes a map list",
			base: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"authz.ip","config":{"issuers":[{"issuer":"a","x":1}]}}}`,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"auth.jwt","config":{"issuers":[{"issuer":"a","y":2}]}}}`,
			path: "spec",
			want: `{"type":"auth.jwt","config":{"issuers":[{"issuer":"a","x":1,"y":2}]}}`,
		},
		{
			name: "req25 config dispatch uses the merged spec.type: issuers becomes atomic",
			base: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"auth.jwt","config":{"issuers":[{"issuer":"a","x":1}]}}}`,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"authz.ip","config":{"issuers":[{"issuer":"a","y":2}]}}}`,
			path: "spec",
			want: `{"type":"authz.ip","config":{"issuers":[{"issuer":"a","y":2}]}}`,
		},
		{
			name: "req25 free content of an open config merges objects and replaces lists",
			base: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"plugin","plugin":"geo","config":{"a":{"x":1,"y":[1,2]},"b":1}}}`,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"config":{"a":{"y":[3],"z":null},"c":2}}}`,
			path: "spec.config",
			want: `{"a":{"x":1,"y":[3]},"b":1,"c":2}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ds, _ := run(t, []file{{"ruralz.yaml", tc.base}}, []file{{"overlays/prod/a.yaml", tc.over}})
			if len(ds) != 0 {
				t.Fatalf("diagnostics: %q", texts(ds))
			}
			if len(out) != 1 {
				t.Fatalf("got %d resources, want 1", len(out))
			}
			got := out[0].Root
			if tc.path != "" {
				got = at(t, got, tc.path)
			}
			if g, w := dump(got), compact(t, tc.want); g != w {
				t.Errorf("merged %s =\n%s\nwant\n%s", tc.path, g, w)
			}
		})
	}
}

// TestSchemaDocumentReplacedWhole covers CR 144/161 and 07 req 74: the
// value of the JSONSchemaDocument definition (validation.json-schema
// config.schema) is one atomic value: an overlay sharing keys with the
// base replaces it whole, nothing inside merges, and $patch and null
// inside it are data.
func TestSchemaDocumentReplacedWhole(t *testing.T) {
	base := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"v"},"spec":{"type":"validation.json-schema",
		"config":{"schema":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a","b"],"enum":[1,2]}}}}`
	cases := []struct{ name, schema string }{
		{"one keyword changed replaces the whole document", `{"required":["a"]}`},
		{"shared keys are not merged", `{"type":"object","properties":{"a":{"type":"integer"}},"enum":[3]}`},
		{"null and $patch inside are data", `{"properties":{"a":null},"$patch":"delete","enum":[null,{"$patch":"replace"}]}`},
		{"boolean document", `true`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			over := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"v"},"spec":{"config":{"schema":` + tc.schema + `}}}`
			out, ds, _ := run(t, []file{{"ruralz.yaml", base}}, []file{{"overlays/prod/v.yaml", over}})
			if len(ds) != 0 {
				t.Fatalf("diagnostics: %q", texts(ds))
			}
			if g, w := dump(at(t, out[0].Root, "spec.config.schema")), compact(t, tc.schema); g != w {
				t.Errorf("schema = %s, want the overlay's %s", g, w)
			}
		})
	}
	t.Run("new identity keeps the document verbatim", func(t *testing.T) {
		doc := `{"properties":{"$patch":{"const":null}},"$patch":"replace"}`
		over := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"w"},"spec":{"type":"validation.json-schema","config":{"schema":` + doc + `}}}`
		out, ds, _ := run(t, nil, []file{{"overlays/prod/w.yaml", over}})
		if len(ds) != 0 {
			t.Fatalf("diagnostics: %q", texts(ds))
		}
		if g, w := dump(at(t, out[0].Root, "spec.config.schema")), compact(t, doc); g != w {
			t.Errorf("schema = %s, want %s", g, w)
		}
	})
	t.Run("base $patch inside the document is data", func(t *testing.T) {
		b := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"v"},"spec":{"type":"validation.json-schema","config":{"schema":{"$patch":1}}}}`
		if _, ds, _ := run(t, []file{{"ruralz.yaml", b}}, nil); len(ds) != 0 {
			t.Errorf("diagnostics: %q", texts(ds))
		}
	})
}

// TestRedispatchedBasePatch covers 01 req 25 where an overlay changes how
// base content is read: a base $patch is data only inside the value of
// the JSONSchemaDocument definition (CheckBase exempts it there), so when
// the merged dispatch reads that value as anything else, the $patch is a
// $patch in a base document like any other (RZ-CFG-006 at the base
// member, as 01 req 25 and its 006 test-plan row map it, related the
// overlay member that changed the dispatch) and is removed. Each case is
// one way the value leaves the document position, or one way it does not.
func TestRedispatchedBasePatch(t *testing.T) {
	base := func(schema string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"v"},"spec":{"type":"validation.json-schema","config":{"schema":` + schema + `}}}`
	}
	over := func(rest string) string {
		if strings.HasPrefix(rest, `"metadata"`) {
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy",` + strings.Replace(rest, `"metadata":{`, `"metadata":{"name":"v",`, 1) + `}`
		}
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"v"},` + rest + `}`
	}
	stray := func(col, path, related string) string {
		return `ruralz.yaml:1:` + col + ` error RZ-CFG-006 Policy/v ` + path + `: $patch is valid only in overlays: this base $patch is no longer inside a JSON Schema document after the overlay merge ` +
			`(the overlay changes how this value is read, as a new Policy spec.type does; remove $patch from the base value or keep the base type) ` +
			`(the overlay changes its schema in o.yaml:1:` + related + `)`
	}
	cases := []struct {
		name, schema, over string
		want               []string
		// result is the merged spec.config.schema; "" when it is absent.
		result string
	}{
		{
			name: "type changed to an open config (the reported fuzz input)", schema: `{"$patch":1}`, over: `"spec":{"type":"cors"}`,
			want: []string{stray("133", "spec.config.schema.$patch", "88")}, result: `{}`,
		},
		{
			name: "type changed to plugin", schema: `{"$patch":1}`, over: `"spec":{"type":"plugin","plugin":"geo"}`,
			want: []string{stray("133", "spec.config.schema.$patch", "88")}, result: `{}`,
		},
		{
			name: "type changed to a closed config without schema", schema: `{"$patch":1}`, over: `"spec":{"type":"auth.basic"}`,
			want: []string{stray("133", "spec.config.schema.$patch", "88")}, result: `{}`,
		},
		{
			name: "type removed", schema: `{"$patch":1}`, over: `"spec":{"type":null}`,
			want: []string{stray("133", "spec.config.schema.$patch", "88")}, result: `{}`,
		},
		{
			name: "type changed to an unresolved expression", schema: `{"$patch":1}`, over: `"spec":{"type":"${PT}"}`,
			want: []string{stray("133", "spec.config.schema.$patch", "88")}, result: `{}`,
		},
		{
			name: "overlay merges into the former document", schema: `{"$patch":1,"type":"object"}`,
			over: `"spec":{"type":"cors","config":{"schema":{"type":"string","minLength":1}}}`,
			want: []string{stray("133", "spec.config.schema.$patch", "88")}, result: `{"type":"string","minLength":1}`,
		},
		{
			name: "dispatch member after the config in the overlay", schema: `{"$patch":1}`,
			over: `"spec":{"config":{"x":1},"type":"cors"}`,
			want: []string{stray("133", "spec.config.schema.$patch", "105")}, result: `{}`,
		},
		{
			name: "nested object", schema: `{"properties":{"$patch":{"type":"string"},"a":{"type":"string"}}}`, over: `"spec":{"type":"cors"}`,
			want: []string{stray("147", "spec.config.schema.properties.$patch", "88")}, result: `{"properties":{"a":{"type":"string"}}}`,
		},
		{
			name: "object in a list", schema: `{"allOf":[{"$patch":1},{"type":"object"}]}`, over: `"spec":{"type":"cors"}`,
			want: []string{stray("143", "spec.config.schema.allOf[0].$patch", "88")}, result: `{"allOf":[{},{"type":"object"}]}`,
		},
		{
			name: "two objects", schema: `{"$patch":1,"items":{"$patch":2}}`, over: `"spec":{"type":"cors"}`,
			want: []string{
				stray("133", "spec.config.schema.$patch", "88"),
				stray("153", "spec.config.schema.items.$patch", "88"),
			},
			result: `{"items":{}}`,
		},
		{
			name: "a $patch inside a $patch value goes with it", schema: `{"$patch":{"$patch":1}}`, over: `"spec":{"type":"cors"}`,
			want: []string{stray("133", "spec.config.schema.$patch", "88")}, result: `{}`,
		},
		{name: "type kept", schema: `{"$patch":1}`, over: `"spec":{"type":"validation.json-schema"}`, result: `{"$patch":1}`},
		{name: "metadata only", schema: `{"$patch":1}`, over: `"metadata":{"labels":{"a":"b"}}`, result: `{"$patch":1}`},
		{name: "former document removed", schema: `{"$patch":1}`, over: `"spec":{"type":"cors","config":{"schema":null}}`},
		{name: "former document replaced by a scalar", schema: `{"$patch":1}`, over: `"spec":{"type":"cors","config":{"schema":true}}`, result: `true`},
		{name: "former document replaced by a list", schema: `{"$patch":1}`, over: `"spec":{"type":"cors","config":{"schema":[{"a":1}]}}`, result: `[{"a":1}]`},
		{name: "spec replaced by the annotation", schema: `{"$patch":1}`, over: `"metadata":{"annotations":{"ruralz.io/patch":"replace"}},"spec":{"type":"cors"}`},
		{name: "a document without $patch", schema: `{"type":"object","enum":[{"a":1}]}`, over: `"spec":{"type":"cors"}`, result: `{"type":"object","enum":[{"a":1}]}`},
		{name: "boolean document", schema: `true`, over: `"spec":{"type":"cors"}`, result: `true`},
	}
	for _, tc := range cases {
		t.Run("req25 "+tc.name, func(t *testing.T) {
			out, ds, _ := run(t, []file{{"ruralz.yaml", base(tc.schema)}}, []file{{"o.yaml", over(tc.over)}})
			if g := texts(ds); !slices.Equal(g, tc.want) {
				t.Errorf("diagnostics =\n%q\nwant\n%q", g, tc.want)
			}
			if len(out) != 1 {
				t.Fatalf("got %d resources", len(out))
			}
			if p, ok := strayPatch(t, out[0]); ok {
				t.Errorf("a $patch member survived at %s: %s", p, dump(out[0].Root))
			}
			spec, _ := out[0].Root.Get("spec")
			config, _ := spec.Get("config")
			got, ok := config.Get("schema")
			switch {
			case tc.result == "" && ok:
				t.Errorf("spec.config.schema = %s, want none", dump(got))
			case tc.result != "" && (!ok || dump(got) != compact(t, tc.result)):
				t.Errorf("spec.config.schema = %s, want %s", dump(got), tc.result)
			}
		})
	}
	// A base $patch CheckBase already reported (RZ-CFG-006, not exempt in
	// the base) is not reported again, whichever way the dispatch moves.
	for _, tc := range []struct {
		name, base, over string
		want             []string
	}{
		{
			name: "beside an exempt one",
			base: base(`{"$patch":1},"x":{"$patch":2}`),
			over: over(`"spec":{"type":"cors"}`),
			want: []string{
				stray("133", "spec.config.schema.$patch", "88"),
				`ruralz.yaml:1:150 error RZ-CFG-006 Policy/v spec.config.x.$patch: $patch is valid only in overlays`,
			},
		},
		{
			name: "free content of an open config, type changed to another open config",
			base: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"v"},"spec":{"type":"cors","config":{"x":{"$patch":1}}}}`,
			over: over(`"spec":{"type":"plugin","plugin":"geo"}`),
			want: []string{`ruralz.yaml:1:110 error RZ-CFG-006 Policy/v spec.config.x.$patch: $patch is valid only in overlays`},
		},
		{
			name: "free content moved into a JSON Schema document",
			base: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"v"},"spec":{"type":"cors","config":{"schema":{"$patch":1}}}}`,
			over: over(`"spec":{"type":"validation.json-schema"}`),
			want: []string{`ruralz.yaml:1:115 error RZ-CFG-006 Policy/v spec.config.schema.$patch: $patch is valid only in overlays`},
		},
	} {
		t.Run("req25 reported once: "+tc.name, func(t *testing.T) {
			_, ds, _ := run(t, []file{{"ruralz.yaml", tc.base}}, []file{{"o.yaml", tc.over}})
			if g := texts(ds); !slices.Equal(g, tc.want) {
				t.Errorf("diagnostics =\n%q\nwant\n%q", g, tc.want)
			}
		})
	}
}

// TestBasePatchMessage covers the review finding on the RZ-CFG-006 text
// (01 req 25: "$patch in a base document: RZ-CFG-006 (\"valid only in
// overlays\")"): every check that finds a $patch in a base document says
// so with the text of the rule, whichever check found it, so one base
// $patch never reads differently by the path that found it. Each row is
// one producer: CheckBase in a described object and in free content, and
// Apply for a base $patch an overlay moves out of a JSON Schema document
// by a new spec.type, a removed one, an unresolved one, or a change at a
// nested level of a synthetic schema.
func TestBasePatchMessage(t *testing.T) {
	const rule = "$patch is valid only in overlays"
	policy := func(spec string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"v"},"spec":` + spec + `}`
	}
	thing := func(spec string) string {
		return `{"apiVersion":"test/v1","kind":"Thing","metadata":{"name":"t"},"spec":` + spec + `}`
	}
	document := policy(`{"type":"validation.json-schema","config":{"schema":{"$patch":1}}}`)
	for _, tc := range []struct {
		name, base, over string
		set              *schemaidx.Set
	}{
		{name: "CheckBase in a described object", base: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"$patch":1}}}`},
		{name: "CheckBase in free content", base: policy(`{"type":"plugin","config":{"a":{"$patch":1}}}`)},
		{name: "a new spec.type", base: document, over: policy(`{"type":"cors"}`)},
		{name: "a removed spec.type", base: document, over: policy(`{"type":null}`)},
		{name: "an unresolved spec.type", base: document, over: policy(`{"type":"${T}"}`)},
		{
			name: "a nested change", set: syntheticSet(t, nestedDispatch),
			base: thing(`{"mode":"b","inner":{"type":"doc","config":{"$patch":1}}}`), over: thing(`{"inner":{"type":"x"}}`),
		},
	} {
		t.Run("req25 "+tc.name, func(t *testing.T) {
			set := tc.set
			if set == nil {
				set = schemas(t)
			}
			files := &tree.FileTable{}
			b := docs(t, files, tree.RoleBase, "b.yaml", tc.base)
			var o []*tree.Resource
			if tc.over != "" {
				o = docs(t, files, tree.RoleOverlay, "o.yaml", tc.over)
			}
			_, ds := pipeline(t, b, o, Options{Schemas: set, Files: files})
			if len(ds) != 1 || ds[0].Code != CodeBasePatch {
				t.Fatalf("diagnostics %q, want one %s", texts(ds), CodeBasePatch)
			}
			if !strings.HasPrefix(ds[0].Message, rule) {
				t.Errorf("message %q, want it to start with %q", ds[0].Message, rule)
			}
		})
	}
}

// nestedDispatch is a rendered schema with two dispatch levels and a
// JSONSchemaDocument position below each, which the committed schema
// does not have yet: spec.mode b selects InnerB for spec.inner, whose
// type doc makes inner.config a document; spec.flag true makes spec.doc
// one (a boolean const, which DispatchValues does not list); spec.left
// and spec.right are two sibling InnerB objects.
const nestedDispatch = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["apiVersion", "kind", "metadata", "spec"],
  "properties": {"apiVersion": {"const": "test/v1"}, "kind": {"enum": ["Thing"]}},
  "allOf": [{"if": {"properties": {"kind": {"const": "Thing"}}, "required": ["kind"]}, "then": {"$ref": "#/$defs/Thing"}}],
  "$defs": {
    "JSONSchemaDocument": {"type": ["object", "boolean"]},
    "Thing": {"type": "object", "properties": {
      "apiVersion": {"const": "test/v1"}, "kind": {"const": "Thing"},
      "metadata": {"type": "object"}, "spec": {"$ref": "#/$defs/Spec"}}},
    "Spec": {"type": "object",
      "properties": {"mode": {"type": "string"}, "flag": {"type": "boolean"}, "inner": {"type": "object"},
        "left": {"$ref": "#/$defs/InnerB"}, "right": {"$ref": "#/$defs/InnerB"}},
      "allOf": [
        {"if": {"properties": {"mode": {"const": "b"}}, "required": ["mode"]}, "then": {"properties": {"inner": {"$ref": "#/$defs/InnerB"}}}},
        {"if": {"properties": {"flag": {"const": true}}, "required": ["flag"]}, "then": {"properties": {"doc": {"$ref": "#/$defs/JSONSchemaDocument"}}}}]},
    "InnerB": {"type": "object",
      "properties": {"type": {"type": "string"}, "config": {"type": "object"}},
      "allOf": [{"if": {"properties": {"type": {"const": "doc"}}, "required": ["type"]}, "then": {"properties": {"config": {"$ref": "#/$defs/JSONSchemaDocument"}}}}]}
  }
}`

// TestRedispatchNested covers the dispatch rules of 01 req 25 at more
// than one level, with a synthetic schema: the outermost object whose
// dispatch the overlay changes rechecks its whole subtree against the
// base view CheckBase used, so a nested object never compares against a
// view the base was not checked under (no second report of a base
// $patch CheckBase reported); a change below an unchanged object is
// found there, in each of two siblings; and a change by a member
// DispatchValues does not list (a boolean const) relates the overlay
// object.
func TestRedispatchNested(t *testing.T) {
	x, err := schemaidx.Load("test/v1", []byte(nestedDispatch))
	if err != nil {
		t.Fatal(err)
	}
	set, err := schemaidx.NewSet(x)
	if err != nil {
		t.Fatal(err)
	}
	doc := func(spec string) string {
		return `{"apiVersion":"test/v1","kind":"Thing","metadata":{"name":"t"},"spec":` + spec + `}`
	}
	for _, tc := range []struct {
		name, base, over string
		want             []string
	}{
		{
			name: "outer change: a nested view the base was not checked under is not compared",
			base: `{"mode":"a","inner":{"type":"doc","config":{"$patch":1}}}`, over: `{"mode":"b","inner":{"type":"other"}}`,
			want: []string{`b.yaml:1:115 error RZ-CFG-006 Thing/t spec.inner.config.$patch: $patch is valid only in overlays`},
		},
		{
			name: "outer change: an untouched nested document leaves its position",
			base: `{"mode":"b","inner":{"type":"doc","config":{"$patch":1}}}`, over: `{"mode":"a"}`,
			want: []string{`b.yaml:1:115 error RZ-CFG-006 Thing/t spec.inner.config.$patch: $patch is valid only in overlays: this base $patch is no longer inside a JSON Schema document after the overlay merge ` +
				`(the overlay changes how this value is read, as a new Policy spec.type does; remove $patch from the base value or keep the base type) (the overlay changes its schema in o.yaml:1:79)`},
		},
		{
			name: "nested change below an unchanged object",
			base: `{"mode":"b","inner":{"type":"doc","config":{"$patch":1}}}`, over: `{"inner":{"type":"x"}}`,
			want: []string{`b.yaml:1:115 error RZ-CFG-006 Thing/t spec.inner.config.$patch: $patch is valid only in overlays: this base $patch is no longer inside a JSON Schema document after the overlay merge ` +
				`(the overlay changes how this value is read, as a new Policy spec.type does; remove $patch from the base value or keep the base type) (the overlay changes its schema in o.yaml:1:88)`},
		},
		{
			name: "boolean dispatch relates the overlay object",
			base: `{"flag":true,"doc":{"$patch":1}}`, over: `{"flag":false}`,
			want: []string{`b.yaml:1:91 error RZ-CFG-006 Thing/t spec.doc.$patch: $patch is valid only in overlays: this base $patch is no longer inside a JSON Schema document after the overlay merge ` +
				`(the overlay changes how this value is read, as a new Policy spec.type does; remove $patch from the base value or keep the base type) (the overlay changes its schema in o.yaml:1:71)`},
		},
		{
			name: "two sibling changes",
			base: `{"left":{"type":"doc","config":{"$patch":1}},"right":{"type":"doc","config":{"$patch":2}}}`,
			over: `{"left":{"type":"x"},"right":{"type":"y"}}`,
			want: []string{
				`b.yaml:1:103 error RZ-CFG-006 Thing/t spec.left.config.$patch: $patch is valid only in overlays: this base $patch is no longer inside a JSON Schema document after the overlay merge ` +
					`(the overlay changes how this value is read, as a new Policy spec.type does; remove $patch from the base value or keep the base type) (the overlay changes its schema in o.yaml:1:87)`,
				`b.yaml:1:148 error RZ-CFG-006 Thing/t spec.right.config.$patch: $patch is valid only in overlays: this base $patch is no longer inside a JSON Schema document after the overlay merge ` +
					`(the overlay changes how this value is read, as a new Policy spec.type does; remove $patch from the base value or keep the base type) (the overlay changes its schema in o.yaml:1:108)`,
			},
		},
		{
			name: "both levels kept",
			base: `{"mode":"b","inner":{"type":"doc","config":{"$patch":1}}}`, over: `{"mode":"b","inner":{"type":"doc","config":{"a":1}}}`,
		},
	} {
		t.Run("req25 "+tc.name, func(t *testing.T) {
			files := &tree.FileTable{}
			b := docs(t, files, tree.RoleBase, "b.yaml", doc(tc.base))
			o := docs(t, files, tree.RoleOverlay, "o.yaml", doc(tc.over))
			out, ds := pipeline(t, b, o, Options{Schemas: set, Files: files})
			if g := texts(ds); !slices.Equal(g, tc.want) {
				t.Errorf("diagnostics =\n%q\nwant\n%q", g, tc.want)
			}
			if len(out) != 1 {
				t.Fatalf("got %d resources", len(out))
			}
		})
	}
}

// policySpec returns the undispatched PolicySpec schema of the committed
// view.
func policySpec(t *testing.T) *schemaidx.Node {
	t.Helper()
	x, _ := schemas(t).Index(v1)
	root, ok := x.Resource("Policy")
	if !ok {
		t.Fatal("no Policy schema")
	}
	spec, ok := root.Property("spec")
	if !ok {
		t.Fatal("no Policy spec schema")
	}
	return spec
}

// TestExemptHolders pins the base view the redispatch check of 01 req 25
// starts from: the $patch holders inside JSONSchemaDocument values of
// base content, nested objects and list elements included; a member the
// overlay added or replaced is not base content (its directives are
// checked as it merges), while a base member the overlay merges into is,
// and so is a base value an overlay value of another kind replaces,
// which comes back when that value holds nothing but removals.
func TestExemptHolders(t *testing.T) {
	orig := policySpec(t).SelectValue("type", "validation.json-schema")
	spec, err := parseJSON(`{"type":"validation.json-schema","config":{"schema":{"$patch":1,"items":[{"$patch":2},{"a":{"$patch":3}}],"b":{}}}}`, 0)
	if err != nil {
		t.Fatal(err)
	}
	config, _ := spec.Get("config")
	doc, _ := config.Get("schema")
	items, _ := doc.Get("items")
	a, _ := items.Items[1].Get("a")
	all := []*tree.Node{doc, items.Items[0], a}
	m := newApplier(Options{}).merger(tree.ID{})
	for _, tc := range []struct {
		name string
		todo []pendingMember
		want []*tree.Node
	}{
		{"untouched base member", nil, all},
		{"base member the overlay merges into", []pendingMember{{at: 1, key: "config", base: config, ov: &tree.Node{Kind: tree.KindMap}}}, all},
		{"member the overlay added", []pendingMember{{at: 1, key: "config", ov: config}}, nil},
		{"base value an overlay list replaces", []pendingMember{{at: 1, key: "config", base: config, ov: &tree.Node{Kind: tree.KindList}}}, all},
		{"base scalar an overlay object replaces", []pendingMember{{at: 1, key: "config", base: &tree.Node{Kind: tree.KindString}, ov: config}}, nil},
		{"another member the overlay added", []pendingMember{{at: 2, key: "other", ov: &tree.Node{Kind: tree.KindMap}}}, all},
	} {
		obj := spec
		if len(tc.todo) > 0 && tc.todo[0].at == 2 {
			obj = &tree.Node{Kind: tree.KindMap, Members: append(slices.Clone(spec.Members), tree.Member{Key: "other", Value: tc.todo[0].ov})}
		}
		held := m.exemptHolders(obj, orig, tc.todo)
		if len(held) != len(tc.want) {
			t.Errorf("%s: %d holders, want %d", tc.name, len(held), len(tc.want))
		}
		for _, n := range tc.want {
			if !held[n] {
				t.Errorf("%s: %s is not held", tc.name, dump(n))
			}
		}
	}
	if held := m.exemptHolders(spec, policySpec(t).SelectValue("type", "cors"), nil); held != nil {
		t.Errorf("free content under cors: %d holders, want none (CheckBase reports them)", len(held))
	}
}

// TestRedispatchRelated pins the related location of a redispatch
// diagnostic (01 req 25): the first overlay member a dispatch rule tests
// whose member the merge changes (a null removing it, a new or another
// scalar, a composite of which something remains replacing a scalar),
// else the overlay object. A member the merge leaves as the base wrote it
// changed nothing and is never named (review finding: a value holding
// nothing but removals was named although the base value stayed). at is
// a substring of the overlay whose first byte the location names; ""
// names the overlay spec object.
func TestRedispatchRelated(t *testing.T) {
	synthetic, fixed := syntheticSet(t, nestedDispatch), syntheticSet(t, fixedDispatch)
	policy := func(spec string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"v"},"spec":` + spec + `}`
	}
	thing := func(spec string) string {
		return `{"apiVersion":"test/v1","kind":"Thing","metadata":{"name":"t"},"spec":` + spec + `}`
	}
	jsonSchema := policy(`{"type":"validation.json-schema","config":{"schema":{"$patch":1}}}`)
	flagged := thing(`{"mode":"a","flag":true,"doc":{"$patch":1}}`)
	for _, tc := range []struct {
		name, base, over, at string
		set                  *schemaidx.Set
	}{
		{name: "type member", base: jsonSchema, over: policy(`{"type":"cors"}`), at: `"cors"`},
		{name: "type after another member", base: jsonSchema, over: policy(`{"config":{},"type":"cors"}`), at: `"cors"`},
		{name: "null type", base: jsonSchema, over: policy(`{"type":null}`), at: `null`},
		{name: "type replaced by an object holding a value", base: jsonSchema, over: policy(`{"type":{"a":1}}`), at: `{"a":1}`},
		{name: "type replaced by a list", base: jsonSchema, over: policy(`{"type":[]}`), at: `[]`},
		// flag is tested against a boolean, which DispatchValues does not
		// list (dispatchPos).
		{name: "a member tested against a boolean is not named", base: flagged, over: thing(`{"flag":false}`), set: synthetic},
		{name: "an equal scalar is not named", base: flagged, over: thing(`{"mode":"a","flag":false}`), set: synthetic},
		{name: "a null for an absent member is not named", base: thing(`{"flag":true,"doc":{"$patch":1}}`), over: thing(`{"mode":null,"flag":false}`), set: synthetic},
		{name: "a value holding nothing but removals is not named", base: flagged, over: thing(`{"mode":{"x":null},"flag":false}`), set: synthetic},
		{name: "a nested value holding nothing but removals is not named", base: flagged, over: thing(`{"mode":{"x":{"y":null}},"flag":false}`), set: synthetic},
		{name: "a composite replacing a base composite of another kind is not named", base: thing(`{"mode":["x"],"flag":true,"doc":{"$patch":1}}`), over: thing(`{"mode":{"k":1},"flag":false}`), set: synthetic},
		{name: "a composite merged into a base composite is not named", base: thing(`{"mode":{"k":1},"flag":true,"doc":{"$patch":1}}`), over: thing(`{"mode":{"k":2},"flag":false}`), set: synthetic},
		{name: "a composite replacing a scalar is named", base: flagged, over: thing(`{"flag":false,"mode":{"x":1}}`), at: `{"x":1}`, set: synthetic},
		{name: "a stood-in member before a changed scalar is named", base: flagged, over: thing(`{"mode":{"x":1},"flag":false}`), at: `{"x":1}`, set: synthetic},
		{name: "a removed member is named", base: flagged, over: thing(`{"flag":false,"mode":null}`), at: `null`, set: synthetic},
		{name: "a new scalar is named", base: thing(`{"flag":true,"doc":{"$patch":1}}`), over: thing(`{"flag":false,"mode":"c"}`), at: `"c"`, set: synthetic},
		{
			name: "a composite replacing a scalar before a changed scalar is named first", set: fixed,
			base: thing(`{"mode":"b","other":"y","x":{"$patch":1}}`), over: thing(`{"mode":{"a":1},"other":"z"}`), at: `{"a":1}`,
		},
		{
			name: "a changed scalar before a composite replacing a scalar is named first", set: fixed,
			base: thing(`{"mode":"b","other":"y","x":{"$patch":1}}`), over: thing(`{"other":"z","mode":{"a":1}}`), at: `"z"`,
		},
		// Review finding: a composite replacing a scalar no rule tests (q)
		// changed nothing a rule reads, so the tested member after it is
		// named.
		{
			name: "a composite replacing an untested scalar is not named", set: fixed,
			base: thing(`{"mode":"b","q":"s","x":{"$patch":1}}`), over: thing(`{"q":{"k":1},"mode":"c"}`), at: `"c"`,
		},
		// settle merges flag before the dispatch is read, as a member the
		// dispatch reads; a boolean test is still not named.
		{name: "a composite replacing a boolean-tested scalar is not named", base: flagged, over: thing(`{"flag":{"a":1}}`), set: synthetic},
		// Review finding: the first changed scalar a rule tests is named,
		// not the last.
		{
			name: "the first of two changed scalars is named", set: fixed,
			base: thing(`{"mode":"b","other":"y","x":{"$patch":1}}`), over: thing(`{"other":"z","mode":"c"}`), at: `"z"`,
		},
	} {
		t.Run("req25 "+tc.name, func(t *testing.T) {
			set := tc.set
			if set == nil {
				set = schemas(t)
			}
			files := &tree.FileTable{}
			b := docs(t, files, tree.RoleBase, "b.yaml", tc.base)
			o := docs(t, files, tree.RoleOverlay, "o.yaml", tc.over)
			_, ds := pipeline(t, b, o, Options{Schemas: set, Files: files})
			if len(ds) != 1 || ds[0].Code != CodeBasePatch || len(ds[0].Related) != 1 {
				t.Fatalf("diagnostics %q, want one redispatched base $patch", texts(ds))
			}
			col := strings.Index(tc.over, `"spec":`) + len(`"spec":`) + 1
			if tc.at != "" {
				col = strings.Index(tc.over[col-1:], tc.at) + col
			}
			if got, want := ds[0].Related[0].Location, (diag.Location{File: "o.yaml", Line: 1, Column: col}); got != want {
				t.Errorf("related %v, want %v", got, want)
			}
		})
	}
	t.Run("req25 a named member without a position names the overlay object", func(t *testing.T) {
		files := &tree.FileTable{}
		b := docs(t, files, tree.RoleBase, "b.yaml", jsonSchema)
		o := docs(t, files, tree.RoleOverlay, "o.yaml", policy(`{"type":"cors"}`))
		spec, _ := o[0].Root.Get("spec")
		typ, _ := spec.Get("type")
		typ.Pos = tree.Pos{}
		_, ds := pipeline(t, b, o, Options{Schemas: schemas(t), Files: files})
		if len(ds) != 1 || len(ds[0].Related) != 1 || ds[0].Related[0].Location != files.Location(spec.Pos) {
			t.Errorf("diagnostics %q, want one related to the overlay spec at %v", texts(ds), files.Location(spec.Pos))
		}
	})
}

// TestPatchAnnotation covers the ruralz.io/patch annotation of 01 req 25:
// delete removes the resource (absent base: no-op), replace replaces spec
// whole while metadata merges, and the annotation never survives.
func TestPatchAnnotation(t *testing.T) {
	other := `{"apiVersion":"ruralz/v1alpha1","kind":"Upstream","metadata":{"name":"u"},"spec":{"protocol":"http","endpoints":[{"address":"a:1"}]}}`
	t.Run("delete removes the base resource", func(t *testing.T) {
		over := `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{"ruralz.io/patch":"delete"}},"spec":{"timeout":"1s"}}`
		out, ds, _ := run(t, []file{{"ruralz.yaml", route + "\n---\n" + other}}, []file{{"overlays/prod/r.yaml", over}})
		if len(ds) != 0 {
			t.Fatalf("diagnostics: %q", texts(ds))
		}
		if len(out) != 1 || out[0].ID.Name != "u" {
			t.Fatalf("resources = %v, want only Upstream/u", ids(out))
		}
	})
	t.Run("delete of an absent resource is a no-op", func(t *testing.T) {
		over := `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"gone","annotations":{"ruralz.io/patch":"delete"}}}`
		out, ds, _ := run(t, []file{{"ruralz.yaml", other}}, []file{{"overlays/prod/r.yaml", over}})
		if len(ds) != 0 || len(out) != 1 {
			t.Fatalf("resources %v, diagnostics %q; want Upstream/u only", ids(out), texts(ds))
		}
	})
	t.Run("replace replaces spec whole and merges metadata", func(t *testing.T) {
		base := `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","labels":{"a":"1"},"annotations":{"keep":"x"}},
			"spec":{"timeout":"5s","match":{"path":{"prefix":"/a"}},"policies":[{"name":"p"}]}}`
		over := `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","labels":{"b":"2"},"annotations":{"ruralz.io/patch":"replace"}},
			"spec":{"match":{"methods":["GET"]},"policies":[{"$patch":"replace"},{"name":"q"}]}}`
		out, ds, _ := run(t, []file{{"ruralz.yaml", base}}, []file{{"overlays/prod/r.yaml", over}})
		if len(ds) != 0 {
			t.Fatalf("diagnostics: %q", texts(ds))
		}
		want := `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","labels":{"a":"1","b":"2"},"annotations":{"keep":"x"}},
			"spec":{"match":{"methods":["GET"]},"policies":[{"name":"q"}]}}`
		if g, w := dump(out[0].Root), compact(t, want); g != w {
			t.Errorf("replaced =\n%s\nwant\n%s", g, w)
		}
	})
	t.Run("replace without an overlay spec leaves no spec", func(t *testing.T) {
		over := `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{"ruralz.io/patch":"replace"}}}`
		out, ds, _ := run(t, []file{{"ruralz.yaml", route}}, []file{{"overlays/prod/r.yaml", over}})
		if len(ds) != 0 {
			t.Fatalf("diagnostics: %q", texts(ds))
		}
		if _, ok := out[0].Root.Get("spec"); ok {
			t.Errorf("spec survived a replace without spec: %s", dump(out[0].Root))
		}
	})
	t.Run("replace of an absent resource adds it without the annotation", func(t *testing.T) {
		over := `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"n","annotations":{"ruralz.io/patch":"replace","x":"y"}},"spec":{"timeout":"1s"}}`
		out, ds, _ := run(t, nil, []file{{"overlays/prod/n.yaml", over}})
		if len(ds) != 0 {
			t.Fatalf("diagnostics: %q", texts(ds))
		}
		want := `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"n","annotations":{"x":"y"}},"spec":{"timeout":"1s"}}`
		if g, w := dump(out[0].Root), compact(t, want); g != w {
			t.Errorf("added = %s, want %s", g, w)
		}
	})
	// Review finding: only the first ruralz.io/patch member was removed, so
	// a second one with the same key was merged into the result. The first
	// one gives the action, none survives, and an annotations object left
	// empty hides a later annotations member as the first key of an object
	// does (stage B leaves every key once, so only a direct caller meets
	// these).
	for _, tc := range []struct{ name, base, over, want string }{
		{
			name: "req25 a duplicated annotation key on an existing identity", base: route,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{"ruralz.io/patch":"replace","ruralz.io/patch":"delete"}},"spec":{"timeout":"1s"}}`,
			want: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"timeout":"1s"}}`,
		},
		{
			name: "req25 a duplicated annotation key on a new identity",
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"n","annotations":{"ruralz.io/patch":"replace","x":"y","ruralz.io/patch":"delete"}},"spec":{"timeout":"1s"}}`,
			want: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"n","annotations":{"x":"y"}},"spec":{"timeout":"1s"}}`,
		},
		// The FuzzOverlay finding of this round: the annotation is a
		// directive in an overlay only; a base document's is data, which
		// stage F reports for its reserved prefix (01 req 35).
		{
			name: "req25 a base document's annotation is data",
			base: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{"ruralz.io/patch":"delete"}},"spec":{"timeout":"5s"}}`,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"timeout":"1s"}}`,
			want: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{"ruralz.io/patch":"delete"}},"spec":{"timeout":"1s"}}`,
		},
		{
			name: "req25 an annotations object left empty hides a later one", base: route,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{"ruralz.io/patch":"replace"},"annotations":{"ruralz.io/patch":"delete","x":"y"}},"spec":{"timeout":"1s"}}`,
			want: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"timeout":"1s"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var base []file
			if tc.base != "" {
				base = []file{{"ruralz.yaml", tc.base}}
			}
			out, ds, _ := run(t, base, []file{{"overlays/prod/r.yaml", tc.over}})
			if len(ds) != 0 || len(out) != 1 {
				t.Fatalf("resources %v, diagnostics %q", ids(out), texts(ds))
			}
			if g, w := dump(out[0].Root), compact(t, tc.want); g != w {
				t.Errorf("result =\n%s\nwant\n%s", g, w)
			}
		})
	}
	t.Run("unsupported value is RZ-CFG-005 and the document is not applied", func(t *testing.T) {
		over := `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{"ruralz.io/patch":"patch"}},"spec":{"timeout":"1s"}}`
		out, ds, _ := run(t, []file{{"ruralz.yaml", route}}, []file{{"overlays/prod/r.yaml", over}})
		want := []string{`overlays/prod/r.yaml:1:104 error RZ-CFG-005 Route/r metadata.annotations["ruralz.io/patch"]: unsupported ruralz.io/patch value "patch" (use delete or replace)`}
		if g := texts(ds); !slices.Equal(g, want) {
			t.Errorf("diagnostics = %q, want %q", g, want)
		}
		if got := at(t, out[0].Root, "spec.timeout").Text; got != "5s" {
			t.Errorf("timeout = %q, want the base 5s", got)
		}
	})
}

func ids(rs []*tree.Resource) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID.String()
	}
	return out
}

// TestDirectiveErrors covers the malformed directives of 01 req 25
// (RZ-CFG-005 in overlays, RZ-CFG-006 for $patch in a base document),
// each at its position and key-aware path.
func TestDirectiveErrors(t *testing.T) {
	head := `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":`
	cases := []struct {
		name, spec string
		want       []string
	}{
		{
			name: "$patch: merge in a list entry",
			spec: `{"policies":[{"name":"auth","$patch":"merge"}]}`,
			want: []string{`o.yaml:1:121 error RZ-CFG-005 Gateway/edge spec.policies[name=auth].$patch: unsupported $patch value "merge" in a list entry (use $patch: delete, or {$patch: replace} as the first element of an orderedMap list)`},
		},
		{
			name: "{$patch: replace} second",
			spec: `{"policies":[{"name":"rl"},{"$patch":"replace"}]}`,
			want: []string{`o.yaml:1:111 error RZ-CFG-005 Gateway/edge spec.policies[1]: {$patch: replace} must be the first element of the list`},
		},
		{
			name: "{$patch: replace} on a map list",
			spec: `{"listeners":[{"$patch":"replace"}]}`,
			want: []string{`o.yaml:1:98 error RZ-CFG-005 Gateway/edge spec.listeners[0]: {$patch: replace} is valid only in an orderedMap list (a map list merges by "name"; remove entries with $patch: delete)`},
		},
		{
			name: "$patch in an object",
			spec: `{"$patch":"replace","trustedProxies":["1.2.3.4/32"]}`,
			want: []string{`o.yaml:1:85 error RZ-CFG-005 Gateway/edge spec.$patch: $patch is not valid in an object (delete a keyed-list entry with {<key>: <value>, $patch: delete}; remove or replace a resource with the ruralz.io/patch annotation)`},
		},
		{
			name: "keyed entry without its key field",
			spec: `{"listeners":[{"port":1}]}`,
			want: []string{`o.yaml:1:98 error RZ-CFG-005 Gateway/edge spec.listeners[0]: list entry has no key field "name"`},
		},
		{
			name: "delete entry without its key field",
			spec: `{"listeners":[{"$patch":"delete"}]}`,
			want: []string{`o.yaml:1:98 error RZ-CFG-005 Gateway/edge spec.listeners[0]: list entry has no key field "name"`},
		},
		{
			name: "keyed entry that is not an object",
			spec: `{"listeners":["http"]}`,
			want: []string{`o.yaml:1:98 error RZ-CFG-005 Gateway/edge spec.listeners[0]: list entry must be an object with its key field "name"`},
		},
		{
			name: "delete entry with other members",
			spec: `{"listeners":[{"name":"http","port":1,"$patch":"delete"}]}`,
			want: []string{`o.yaml:1:98 error RZ-CFG-005 Gateway/edge spec.listeners[name=http]: a $patch: delete entry holds only "name" and $patch`},
		},
		{
			name: "two entries with one key in one overlay list",
			spec: `{"listeners":[{"name":"http","port":1},{"name":"http","port":2}]}`,
			want: []string{`o.yaml:1:131 error RZ-CFG-005 Gateway/edge spec.listeners[name=http]: two entries with name "http" in one overlay list (first entry o.yaml:1:106)`},
		},
		{
			name: "$patch in a set list element object",
			spec: `{"trustedProxies":[{"$patch":"delete"}]}`,
			want: []string{`o.yaml:1:104 error RZ-CFG-005 Gateway/edge spec.trustedProxies[item={"$patch":"delete"}].$patch: $patch is valid only in an entry of a map or orderedMap list (a set list is replaced whole: write the complete list)`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ds, _ := run(t, []file{{"ruralz.yaml", gateway}}, []file{{"o.yaml", head + tc.spec + `}`}})
			if g := texts(ds); !slices.Equal(g, tc.want) {
				t.Errorf("diagnostics =\n%q\nwant\n%q", g, tc.want)
			}
			if len(out) != 1 {
				t.Errorf("got %d resources", len(out))
			}
		})
	}
	t.Run("req25 $patch in a base document is RZ-CFG-006", func(t *testing.T) {
		b := `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"policies":[{"name":"a","$patch":"delete"}],"match":{"$patch":"x"}}}`
		_, ds, _ := run(t, []file{{"routes/r.yaml", b}}, nil)
		want := []string{
			`routes/r.yaml:1:104 error RZ-CFG-006 Route/r spec.policies[name=a].$patch: $patch is valid only in overlays`,
			`routes/r.yaml:1:133 error RZ-CFG-006 Route/r spec.match.$patch: $patch is valid only in overlays`,
		}
		if g := texts(ds); !slices.Equal(g, want) {
			t.Errorf("diagnostics =\n%q\nwant\n%q", g, want)
		}
	})
	t.Run("req25 $patch inside free content of a base document is RZ-CFG-006", func(t *testing.T) {
		b := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"plugin","config":{"a":{"$patch":"delete"},"l":[{"k":"b","$patch":"delete"}]}}}`
		_, ds, _ := run(t, []file{{"ruralz.yaml", b}}, nil)
		want := []string{
			`ruralz.yaml:1:112 error RZ-CFG-006 Policy/p spec.config.a.$patch: $patch is valid only in overlays`,
			`ruralz.yaml:1:145 error RZ-CFG-006 Policy/p spec.config.l[0].$patch: $patch is valid only in overlays`,
		}
		if g := texts(ds); !slices.Equal(g, want) {
			t.Errorf("diagnostics =\n%q\nwant\n%q", g, want)
		}
	})
}

// TestFreeContentDirectives covers 01 req 25 in free content (undeclared
// members of open configs, lists without x-ruralz-list, plugin configs)
// and in set and atomic lists: a list there is atomic, so {$patch:
// replace} is valid only in an orderedMap list and any other $patch is
// misplaced (RZ-CFG-005); no directive survives into the result.
func TestFreeContentDirectives(t *testing.T) {
	plugin := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"plugin","plugin":"geo","config":{"l":[{"k":"a"}],"m":{"x":1}}}}`
	headers := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"h"},"spec":{"type":"headers","config":{"request":{"add":[{"name":"a","value":"1"}]}}}}`
	cases := []struct {
		name, base, over, path, result string
		want                           []string
	}{
		{
			name: "list without x-ruralz-list in a plugin config",
			base: plugin,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"config":{"l":[{"$patch":"replace"},{"k":"b","$patch":"delete"}]}}}`,
			path: "spec.config.l", result: `[{"k":"b"}]`,
			want: []string{
				`o.yaml:1:96 error RZ-CFG-005 Policy/p spec.config.l[0]: {$patch: replace} is valid only in an orderedMap list (a list without x-ruralz-list is replaced whole: write the complete list)`,
				`o.yaml:1:126 error RZ-CFG-005 Policy/p spec.config.l[1].$patch: $patch is valid only in an entry of a map or orderedMap list (a list without x-ruralz-list is replaced whole: write the complete list)`,
			},
		},
		{
			name: "undeclared member of an open config",
			base: plugin,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"config":{"m":{"$patch":"delete","y":2}}}}`,
			path: "spec.config.m", result: `{"x":1,"y":2}`,
			want: []string{
				`o.yaml:1:96 error RZ-CFG-005 Policy/p spec.config.m.$patch: $patch is not valid in an object (content without a schema merges as data, objects member by member and lists replaced whole; remove $patch)`,
			},
		},
		{
			name: "free content added by an overlay",
			base: plugin,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"config":{"n":[[{"$patch":"x"}]]}}}`,
			path: "spec.config.n", result: `[[]]`,
			want: []string{
				`o.yaml:1:98 error RZ-CFG-005 Policy/p spec.config.n[0][0].$patch: $patch is valid only in an entry of a map or orderedMap list (a list without x-ruralz-list is replaced whole: write the complete list)`,
			},
		},
		{
			name: "{$patch: replace} in an atomic list of objects",
			base: headers,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"h"},"spec":{"config":{"request":{"add":[{"$patch":"replace"},{"name":"b","value":"2"}]}}}}`,
			path: "spec.config.request.add", result: `[{"name":"b","value":"2"}]`,
			want: []string{
				`o.yaml:1:109 error RZ-CFG-005 Policy/h spec.config.request.add[0]: {$patch: replace} is valid only in an orderedMap list (an atomic list is replaced whole: write the complete list)`,
			},
		},
	}
	for _, tc := range cases {
		t.Run("req25 "+tc.name, func(t *testing.T) {
			out, ds, _ := run(t, []file{{"ruralz.yaml", tc.base}}, []file{{"o.yaml", tc.over}})
			if g := texts(ds); !slices.Equal(g, tc.want) {
				t.Errorf("diagnostics =\n%q\nwant\n%q", g, tc.want)
			}
			if g := dump(at(t, out[0].Root, tc.path)); g != compact(t, tc.result) {
				t.Errorf("%s = %s, want %s", tc.path, g, tc.result)
			}
			if p, ok := strayPatch(t, out[0]); ok {
				t.Errorf("a $patch member survived at %s", p)
			}
		})
	}
}

// TestUnresolvedDispatch pins the three consequences of a Policy whose
// spec.type holds a ${VAR} expression (review finding: the package doc
// named only the first): overlays merge before substitution (01 req 27),
// so no config branch is selected, in a base document as in a merged one.
// Spec 01 leaves the case open; a contract change request against 01 req
// 25 and 27 names all three, and each would be unnecessary once the
// variable names validation.json-schema.
func TestUnresolvedDispatch(t *testing.T) {
	policy := func(typ, config string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"` + typ + `","config":` + config + `}}`
	}
	t.Run("req25 composite members merge as free content", func(t *testing.T) {
		base := policy("${PT}", `{"issuers":[{"issuer":"a","x":1}]}`)
		over := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"config":{"issuers":[{"issuer":"a","y":2}]}}}`
		out, ds, _ := run(t, []file{{"ruralz.yaml", base}}, []file{{"o.yaml", over}})
		if len(ds) != 0 {
			t.Fatalf("diagnostics: %q", texts(ds))
		}
		if g, w := dump(at(t, out[0].Root, "spec.config.issuers")), `[{"issuer":"a","y":2}]`; g != w {
			t.Errorf("issuers = %s, want the overlay list %s", g, w)
		}
	})
	t.Run("req25 an overlay $patch inside them is RZ-CFG-005", func(t *testing.T) {
		base := policy("${PT}", `{"issuers":[{"issuer":"a","x":1}]}`)
		del := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"config":{"issuers":[{"issuer":"a","$patch":"delete"}]}}}`
		_, ds, _ := run(t, []file{{"ruralz.yaml", base}}, []file{{"o.yaml", del}})
		if g := codes(ds); !slices.Equal(g, []string{CodeSchema}) {
			t.Errorf("codes = %v, want [%s]: %q", g, CodeSchema, texts(ds))
		}
	})
	t.Run("req25 CheckBase reads a base config.schema as outside a document", func(t *testing.T) {
		_, ds, _ := run(t, []file{{"ruralz.yaml", policy("${T}", `{"schema":{"$patch":1}}`)}}, nil)
		want := []string{`ruralz.yaml:1:115 error RZ-CFG-006 Policy/p spec.config.schema.$patch: $patch is valid only in overlays`}
		if g := texts(ds); !slices.Equal(g, want) {
			t.Errorf("diagnostics =\n%q\nwant\n%q", g, want)
		}
		_, ds, _ = run(t, []file{{"ruralz.yaml", policy("validation.json-schema", `{"schema":{"$patch":1}}`)}}, nil)
		if len(ds) != 0 {
			t.Errorf("the same document under the resolved type: %q", texts(ds))
		}
	})
	t.Run("req25 an overlay spec.type expression changes the base dispatch", func(t *testing.T) {
		base := policy("validation.json-schema", `{"schema":{"$patch":1}}`)
		over := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"${T}"}}`
		out, ds, _ := run(t, []file{{"ruralz.yaml", base}}, []file{{"o.yaml", over}})
		want := []string{`ruralz.yaml:1:133 error RZ-CFG-006 Policy/p spec.config.schema.$patch: $patch is valid only in overlays: this base $patch is no longer inside a JSON Schema document after the overlay merge ` +
			`(the overlay changes how this value is read, as a new Policy spec.type does; remove $patch from the base value or keep the base type) (the overlay changes its schema in o.yaml:1:88)`}
		if g := texts(ds); !slices.Equal(g, want) {
			t.Errorf("diagnostics =\n%q\nwant\n%q", g, want)
		}
		if g := dump(at(t, out[0].Root, "spec.config.schema")); g != `{}` {
			t.Errorf("spec.config.schema = %s, want the base $patch removed", g)
		}
	})
}

// TestSetElementDirective: an object element of a set list is a
// described object, so $patch in it is a misplaced directive.
func TestSetElementDirective(t *testing.T) {
	over := `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"upstreams":[{"name":"u","weight":2,"$patch":"x"}]}}`
	_, ds, _ := run(t, []file{{"ruralz.yaml", route}}, []file{{"o.yaml", over}})
	if g := codes(ds); !slices.Equal(g, []string{CodeSchema}) {
		t.Errorf("codes = %v, want [%s]: %q", g, CodeSchema, texts(ds))
	}
}

// TestIdentityRules covers 01 req 24: one identity in two documents of one
// overlay (two files or one) is RZ-CFG-008 at the later one and none of
// them applies; a patch with another apiVersion is RZ-CFG-030 (tested with
// a second served view, 01 risk 17); an unserved apiVersion is RZ-CFG-007
// only.
func TestIdentityRules(t *testing.T) {
	p := func(timeout string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"timeout":"` + timeout + `"}}`
	}
	t.Run("req24 two files", func(t *testing.T) {
		out, ds, _ := run(t, []file{{"ruralz.yaml", route}}, []file{{"overlays/prod/b.yaml", p("2s")}, {"overlays/prod/a.yaml", p("1s")}})
		want := []string{`overlays/prod/b.yaml:1:67 error RZ-CFG-008 Route/r metadata.name: Route/r is declared twice in the overlay; results never depend on file order (first declared in overlays/prod/a.yaml:1:67)`}
		if g := texts(ds); !slices.Equal(g, want) {
			t.Errorf("diagnostics =\n%q\nwant\n%q", g, want)
		}
		if got := at(t, out[0].Root, "spec.timeout").Text; got != "5s" {
			t.Errorf("timeout = %q, want the base 5s (no duplicate applies)", got)
		}
	})
	t.Run("req24 one file", func(t *testing.T) {
		_, ds, _ := run(t, []file{{"ruralz.yaml", route}}, []file{{"overlays/prod/a.yaml", p("1s") + "\n---\n" + p("2s") + "\n---\n" + p("3s")}})
		want := []string{
			`overlays/prod/a.yaml:3:67 error RZ-CFG-008 Route/r metadata.name: Route/r is declared twice in the overlay; results never depend on file order (first declared in overlays/prod/a.yaml:1:67)`,
			`overlays/prod/a.yaml:5:67 error RZ-CFG-008 Route/r metadata.name: Route/r is declared twice in the overlay; results never depend on file order (first declared in overlays/prod/a.yaml:1:67)`,
		}
		if g := texts(ds); !slices.Equal(g, want) {
			t.Errorf("diagnostics =\n%q\nwant\n%q", g, want)
		}
	})
	t.Run("req24 apiVersion differs from the base: RZ-CFG-030", func(t *testing.T) {
		over := strings.Replace(p("1s"), "ruralz/v1alpha1", "ruralz/v1alpha2", 1)
		out, ds, _ := run(t, []file{{"ruralz.yaml", route}}, []file{{"overlays/prod/a.yaml", over}}, "ruralz/v1alpha2")
		want := []string{`overlays/prod/a.yaml:1:15 error RZ-CFG-030 Route/r apiVersion: overlay apiVersion "ruralz/v1alpha2" differs from the base resource's "ruralz/v1alpha1" (patch a resource with the apiVersion of its base document) (base resource declared in ruralz.yaml:1:15)`}
		if g := texts(ds); !slices.Equal(g, want) {
			t.Errorf("diagnostics =\n%q\nwant\n%q", g, want)
		}
		if got := at(t, out[0].Root, "spec.timeout").Text; got != "5s" {
			t.Errorf("timeout = %q, want the base 5s", got)
		}
	})
	t.Run("req24 a new identity under a second served version is added", func(t *testing.T) {
		over := `{"apiVersion":"ruralz/v1alpha2","kind":"Route","metadata":{"name":"n"},"spec":{"timeout":"1s"}}`
		out, ds, _ := run(t, []file{{"ruralz.yaml", route}}, []file{{"overlays/prod/a.yaml", over}}, "ruralz/v1alpha2")
		if len(ds) != 0 || len(out) != 2 || out[1].APIVersion != "ruralz/v1alpha2" {
			t.Errorf("resources %v, diagnostics %q", ids(out), texts(ds))
		}
	})
	t.Run("req24 unserved apiVersion is RZ-CFG-007 only", func(t *testing.T) {
		over := strings.Replace(p("1s"), "ruralz/v1alpha1", "ruralz/v1beta1", 1)
		_, ds, _ := run(t, []file{{"ruralz.yaml", route}}, []file{{"overlays/prod/a.yaml", over}})
		if g := codes(ds); !slices.Equal(g, []string{CodeAPIVersion}) {
			t.Errorf("codes = %v, want [%s]", g, CodeAPIVersion)
		}
	})
	t.Run("unserved base apiVersion is RZ-CFG-007 and dropped", func(t *testing.T) {
		b := strings.Replace(route, "ruralz/v1alpha1", "ruralz/v9", 1)
		out, ds, _ := run(t, []file{{"ruralz.yaml", b}}, nil)
		if g := codes(ds); !slices.Equal(g, []string{CodeAPIVersion}) || len(out) != 0 {
			t.Errorf("codes = %v, resources %v", g, ids(out))
		}
	})
	t.Run("unknown kind is RZ-CFG-007", func(t *testing.T) {
		over := `{"apiVersion":"ruralz/v1alpha1","kind":"Rout","metadata":{"name":"r"}}`
		_, ds, _ := run(t, nil, []file{{"overlays/prod/a.yaml", over}})
		if g := codes(ds); !slices.Equal(g, []string{CodeAPIVersion}) {
			t.Errorf("codes = %v", g)
		}
	})
	t.Run("nil schema set reports every document", func(t *testing.T) {
		files := &tree.FileTable{}
		b := docs(t, files, tree.RoleBase, "ruralz.yaml", route)
		out, ds := pipeline(t, b, nil, Options{Files: files})
		if len(out) != 0 || !slices.Equal(codes(ds), []string{CodeAPIVersion}) {
			t.Errorf("resources %v, codes %v", ids(out), codes(ds))
		}
	})
}

// TestNewIdentities covers 01 req 25 "New identity: added, merged against
// an empty base": new resources follow the base ones in (kind, name)
// order whatever the file order, nil documents are ignored, and a spec
// holding nothing but a removal is not added ("absent key: no-op").
func TestNewIdentities(t *testing.T) {
	mk := func(kind, name string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"` + kind + `","metadata":{"name":"` + name + `"},"spec":{"x":null}}`
	}
	files := &tree.FileTable{}
	b := docs(t, files, tree.RoleBase, "ruralz.yaml", route)
	var o []*tree.Resource
	o = append(o, docs(t, files, tree.RoleOverlay, "overlays/p/z.yaml", mk("Upstream", "b"))...)
	o = append(o, nil)
	o = append(o, docs(t, files, tree.RoleOverlay, "overlays/p/a.yaml", mk("Route", "z")+"\n---\n"+mk("Policy", "m"))...)
	out, ds := pipeline(t, b, o, Options{Schemas: schemas(t), Files: files})
	if len(ds) != 0 {
		t.Fatalf("diagnostics: %q", texts(ds))
	}
	if g, w := ids(out), []string{"Route/r", "Policy/m", "Route/z", "Upstream/b"}; !slices.Equal(g, w) {
		t.Errorf("order = %v, want %v", g, w)
	}
	if _, ok := out[3].Root.Get("spec"); ok {
		t.Errorf("new identity %s: spec {x: null} was added, want no spec (a removal of nothing)", dump(out[3].Root))
	}
}

// TestPositions covers 01 req 25 "Each result node keeps the position of
// the document that supplied it".
func TestPositions(t *testing.T) {
	over := `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},
"spec":{"listeners":[{"name":"http","protocol":"http","port":8081,"hostnames":["a"]}]}}`
	out, ds, files := run(t, []file{{"ruralz.yaml", gateway}}, []file{{"overlays/prod/gw.yaml", over}})
	if len(ds) != 0 {
		t.Fatalf("diagnostics: %q", texts(ds))
	}
	loc := func(path string) string {
		l := files.Location(at(t, out[0].Root, path).Pos)
		return l.File + ":" + strconv.Itoa(l.Line) + ":" + strconv.Itoa(l.Column)
	}
	for path, want := range map[string]string{
		"spec.listeners.0.port":      "overlays/prod/gw.yaml:2:62",
		"spec.listeners.0.hostnames": "overlays/prod/gw.yaml:2:79",
		"spec.listeners.0.protocol":  "ruralz.yaml:2:48",
		"spec.listeners.0.name":      "ruralz.yaml:2:30",
		"spec.listeners.0":           "ruralz.yaml:2:22",
		"kind":                       "ruralz.yaml:1:40",
		"spec.listeners.1.port":      "ruralz.yaml:3:44",
	} {
		if got := loc(path); got != want {
			t.Errorf("%s at %s, want %s", path, got, want)
		}
	}
	listener := at(t, out[0].Root, "spec.listeners.0")
	if m := listener.Members[len(listener.Members)-1]; m.Key != "hostnames" || files.Location(m.KeyPos).File != "overlays/prod/gw.yaml" {
		t.Errorf("new member %q key at %v, want hostnames from the overlay", m.Key, files.Location(m.KeyPos))
	}
}

// TestKeyKinds: keyed-list keys match by kind and text, so the number 1
// and the string "1" are different entries (01 req 25), and an overlay
// scalar equal to the base keeps the base node.
func TestKeyKinds(t *testing.T) {
	base := `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"listeners":[{"name":1,"port":1},{"name":"1","port":2},{"name":1.5,"port":3}],"admin":{"enabled":true,"x":null}}}`
	over := `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"listeners":[{"name":"1","port":20},{"name":1.5,"port":30},{"name":true}],"admin":{"enabled":true,"x":null}}}`
	out, ds, _ := run(t, []file{{"ruralz.yaml", base}}, []file{{"o.yaml", over}})
	if g := codes(ds); !slices.Equal(g, []string{CodeSchema}) {
		t.Fatalf("codes = %v, want one %s for the boolean key: %q", g, CodeSchema, texts(ds))
	}
	want := `[{"name":1,"port":1},{"name":"1","port":20},{"name":1.5,"port":30}]`
	if g := dump(at(t, out[0].Root, "spec.listeners")); g != want {
		t.Errorf("listeners = %s, want %s", g, want)
	}
	if g := dump(at(t, out[0].Root, "spec.admin")); g != `{"enabled":true}` {
		t.Errorf("admin = %s", g)
	}
}

// TestScalarHelpers pins the scalar comparison and message forms.
func TestScalarHelpers(t *testing.T) {
	n := func(k tree.Kind, text string, b bool) *tree.Node { return &tree.Node{Kind: k, Text: text, Bool: b} }
	for _, tc := range []struct {
		a, b *tree.Node
		want bool
	}{
		{n(tree.KindBool, "", true), n(tree.KindBool, "", true), true},
		{n(tree.KindBool, "", true), n(tree.KindBool, "", false), false},
		{n(tree.KindNull, "", false), n(tree.KindNull, "", false), true},
		{n(tree.KindInt, "1", false), n(tree.KindFloat, "1", false), false},
		{n(tree.KindString, "a", false), n(tree.KindString, "a", false), true},
		{n(tree.KindMap, "", false), n(tree.KindMap, "", false), false},
	} {
		if got := scalarEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("scalarEqual(%v, %v) = %v", dump(tc.a), dump(tc.b), got)
		}
	}
	for want, node := range map[string]*tree.Node{
		`"a"`: n(tree.KindString, "a", false), "12": n(tree.KindInt, "12", false), "true": n(tree.KindBool, "", true),
		"null": n(tree.KindNull, "", false), "(an object)": n(tree.KindMap, "", false), "(a list)": n(tree.KindList, "", false),
	} {
		if got := scalarText(node); got != want {
			t.Errorf("scalarText = %q, want %q", got, want)
		}
	}
}

// TestUnknownPositions: a document without positions reports at its
// start; duplicates are dropped before any other check.
func TestUnknownPositions(t *testing.T) {
	root := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{
		{Key: "metadata", Value: &tree.Node{Kind: tree.KindMap}},
	}}
	start := tree.Pos{File: 0, Line: 3, Column: 1}
	files := &tree.FileTable{}
	files.Add(tree.File{Path: "o.yaml"})
	r := func() *tree.Resource {
		return &tree.Resource{ID: tree.ID{Kind: "Route", Name: "r"}, APIVersion: "ruralz/v9", Root: root.Clone(), Start: start}
	}
	_, ds := pipeline(t, nil, []*tree.Resource{r(), r()}, Options{Schemas: schemas(t), Files: files})
	want := []string{
		"o.yaml:3:1 error RZ-CFG-008 Route/r metadata.name: Route/r is declared twice in the overlay; results never depend on file order (first declared in o.yaml:3:1)",
	}
	if g := texts(ds); !slices.Equal(g, want) {
		t.Errorf("diagnostics =\n%q\nwant\n%q", g, want)
	}
}

// TestCancellation covers 01 req 53: Apply checks ctx between resources
// and, once it is canceled, returns ctx.Err() and no result.
func TestCancellation(t *testing.T) {
	files := &tree.FileTable{}
	b := docs(t, files, tree.RoleBase, "ruralz.yaml", route)
	o := docs(t, files, tree.RoleOverlay, "o.yaml", `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"timeout":"1s"}}`)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	opts := Options{Schemas: schemas(t), Files: files}
	for name, base := range map[string][]*tree.Resource{"between base resources": b, "between overlay documents": nil} {
		out, ds, err := Apply(ctx, base, o, opts)
		if !errors.Is(err, context.Canceled) || out != nil || ds != nil {
			t.Errorf("%s: Apply = %v, %q, %v; want no result and context.Canceled", name, ids(out), texts(ds), err)
		}
	}
	if got := at(t, b[0].Root, "spec.timeout").Text; got != "5s" {
		t.Errorf("a canceled Apply changed the base: timeout %q", got)
	}
}

// TestCheckBase covers the per-resource base check of 01 req 25 and 53:
// it reads one resource, so the loader runs it in its worker pool, and
// skips what it cannot select a schema for (stage C and Apply report
// RZ-CFG-007).
func TestCheckBase(t *testing.T) {
	opts := Options{Schemas: schemas(t)}
	withPatch := func(apiVersion, kind string) *tree.Resource {
		files := &tree.FileTable{}
		src := `{"apiVersion":"` + apiVersion + `","kind":"` + kind + `","metadata":{"name":"r"},"spec":{"$patch":"delete"}}`
		return docs(t, files, tree.RoleBase, "r.yaml", src)[0]
	}
	if ds := CheckBase(withPatch(v1, "Route"), opts); !slices.Equal(codes(ds), []string{CodeBasePatch}) {
		t.Errorf("served kind: codes %v, want [%s]", codes(ds), CodeBasePatch)
	}
	for name, tc := range map[string]struct {
		res  *tree.Resource
		opts Options
	}{
		"nil resource":        {nil, opts},
		"list root":           {&tree.Resource{APIVersion: v1, Root: &tree.Node{Kind: tree.KindList}}, opts},
		"nil schema set":      {withPatch(v1, "Route"), Options{}},
		"unserved version":    {withPatch("ruralz/v9", "Route"), opts},
		"kind without schema": {withPatch(v1, "Rout"), opts},
	} {
		if ds := CheckBase(tc.res, tc.opts); ds != nil {
			t.Errorf("%s: diagnostics %q, want none", name, texts(ds))
		}
	}
}
