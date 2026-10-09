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

// TestUnresolvedDispatch pins the merge of a Policy whose spec.type holds
// a ${VAR} expression: overlays merge before substitution (01 req 27), so
// no config branch is selected and config merges as free content (keyed
// lists replaced whole, $patch inside RZ-CFG-005). Spec 01 leaves the case
// open; it is reported as a contract change request.
func TestUnresolvedDispatch(t *testing.T) {
	base := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"${PT}","config":{"issuers":[{"issuer":"a","x":1}]}}}`
	over := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"config":{"issuers":[{"issuer":"a","y":2}]}}}`
	out, ds, _ := run(t, []file{{"ruralz.yaml", base}}, []file{{"o.yaml", over}})
	if len(ds) != 0 {
		t.Fatalf("diagnostics: %q", texts(ds))
	}
	if g, w := dump(at(t, out[0].Root, "spec.config.issuers")), `[{"issuer":"a","y":2}]`; g != w {
		t.Errorf("issuers = %s, want the overlay list %s", g, w)
	}
	del := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"config":{"issuers":[{"issuer":"a","$patch":"delete"}]}}}`
	_, ds, _ = run(t, []file{{"ruralz.yaml", base}}, []file{{"o.yaml", del}})
	if g := codes(ds); !slices.Equal(g, []string{CodeSchema}) {
		t.Errorf("codes = %v, want [%s]: %q", g, CodeSchema, texts(ds))
	}
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
// order whatever the file order, and nil documents are ignored.
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
	if g := dump(at(t, out[3].Root, "spec")); g != "{}" {
		t.Errorf("new identity spec = %s, want {} (null dropped)", g)
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
