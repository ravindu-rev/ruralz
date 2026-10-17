// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// notUTF8 is a string that is not valid UTF-8, as stage E may substitute
// from the process environment.
const notUTF8 = "caf\xe9"

// posText renders the position of a node as file:line:column of r.json.
func posAt(p tree.Pos) string { return fmt.Sprintf("r.json:%d:%d", p.Line, p.Column) }

// TestInvalidUTF8 covers the UTF-8 rule of Normalize (01 req 38, 02 req
// 15 and 19; WP-37 review round 3: only the typed decode rejected a
// string that is not valid UTF-8, at line 1 column 1 with an empty path
// and jsonval's error text, or at spec.config for a config member):
// every string and member name that is not valid UTF-8 is RZ-CFG-005 at
// its key-aware path and position, with a message that never shows the
// value, before any scalar normalization, for each kind of position, in
// authored and in canonical content alike (round 5: no row had the
// canonical role, so the rule could skip canonical content unnoticed).
func TestInvalidUTF8(t *testing.T) {
	gw := gateway
	tests := []struct {
		name string
		src  string
		// at is the dotted path of the string to edit, or of the object
		// whose member key names to edit.
		at, key string
		path    string
	}{
		{
			"typed string", `{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "u"}, "spec": {"protocol": "http", "endpoints": [{"address": "x"}]}}`,
			"spec.endpoints[0].address", "", `spec.endpoints[address="caf�"].address`,
		},
		{
			"map value", `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r", "labels": {"team": "x"}}, "spec": {"match": {}}}`,
			"metadata.labels.team", "", "metadata.labels.team",
		},
		{
			"keyed-list key", route(`{"match": {}, "upstreams": [{"name": "a"}, {"name": "x"}]}`),
			"spec.upstreams[1].name", "", `spec.upstreams[name="caf�"].name`,
		},
		{
			"set element", route(`{"match": {"hosts": ["a.example", "x"]}}`),
			"spec.match.hosts[1]", "", `spec.match.hosts[item="caf�"]`,
		},
		{"duration", route(`{"match": {}, "timeout": "1s"}`), "spec.timeout", "", "spec.timeout"},
		{"byte size", gw(`{"limits": {"maxBufferedBytes": "1Ki"}}`), "spec.limits.maxBufferedBytes", "", "spec.limits.maxBufferedBytes"},
		{
			"decimal", `{"apiVersion": "ruralz/v1alpha1", "kind": "AIProvider", "metadata": {"name": "p"}, "spec": {"dialect": "openai", "pricing": {"models": [{"model": "m", "inputPerMillionTokens": "3.50"}]}}}`,
			"spec.pricing.models[0].inputPerMillionTokens", "", "spec.pricing.models[model=m].inputPerMillionTokens",
		},
		{"plugin config string", pluginDoc(`{"x": ["y"]}`), "spec.config.x[0]", "", "spec.config.x[0]"},
		{"open config member", policyDoc("auth.api-key", `{"header": "x"}`), "spec.config.header", "", "spec.config.header"},
		{
			"json schema document", policyDoc("validation.json-schema", `{"schema": {"type": "object", "description": "x"}}`),
			"spec.config.schema.description", "", "spec.config.schema.description",
		},
		{
			"below a secret field", gw(`{"stateStore": {"driver": "redis", "url": {"secretRef": {"provider": "env", "name": "x"}}}}`),
			"spec.stateStore.url.secretRef.name", "", "spec.stateStore.url.secretRef.name",
		},
		{
			"member name", `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r", "labels": {"x": "v"}}, "spec": {"match": {}}}`,
			"metadata.labels", "x", `metadata.labels["caf�"]`,
		},
		{"plugin config member name", pluginDoc(`{"x": 1}`), "spec.config", "x", `spec.config["caf�"]`},
	}
	for _, tc := range tests {
		for _, role := range roles() {
			t.Run(tc.name+"/"+role.name, func(t *testing.T) {
				f := newFixture()
				res := f.resourceRole(t, "r.json", tc.src, role.role)
				n := at(t, res.Root, tc.at)
				pos, what := n.Pos, "string"
				if tc.key != "" {
					i := slices.IndexFunc(n.Members, func(m tree.Member) bool { return m.Key == tc.key })
					n.Members[i].Key, pos, what = notUTF8, n.Members[i].KeyPos, "member name"
				} else {
					n.Text = notUTF8
				}
				h, ds := newStage(t).Resource(res, f.files)
				kind, name := res.ID.Kind, res.ID.Name
				want := []string{fmt.Sprintf("%s error RZ-CFG-005 %s/%s %s: %s is not valid UTF-8: the canonical form holds only UTF-8 text", posAt(pos), kind, name, tc.path, what)}
				if got := texts(ds); h != nil || !slices.Equal(got, want) {
					t.Errorf("Resource = %v,\n%q\nwant\n%q", h != nil, got, want)
				}
				for _, d := range ds {
					if strings.Contains(d.Message, "caf") {
						t.Errorf("message shows the value: %q", d.Message)
					}
				}
			})
		}
	}
}

// roles lists the file roles a gate rule of Normalize must treat alike:
// authored content and content that re-enters as ruralz.canonical.v1.
func roles() []struct {
	name string
	role tree.Role
} {
	return []struct {
		name string
		role tree.Role
	}{{"authored", tree.RoleBase}, {"canonical", tree.RoleCanonical}}
}

// TestNullKeyNoCascade covers the keyed-list check after the null rule:
// a null entry, and an entry whose key member is null, is one diagnostic,
// not also an entry without a key member.
func TestNullKeyNoCascade(t *testing.T) {
	for _, upstreams := range []string{`[null, {"name": "b"}]`, `[{"name": null}, {"name": "b"}]`} {
		f := newFixture()
		res := f.resource(t, "r.json", route(`{"match": {}, "upstreams": `+upstreams+`}`))
		if _, ds := newStage(t).Resource(res, f.files); len(ds) != 1 || !strings.Contains(ds[0].Message, "null is not allowed") {
			t.Errorf("upstreams %s: %q, want the one null", upstreams, texts(ds))
		}
	}
}

// TestSubstitutedInvalidUTF8 runs the case of the review end to end:
// stage E substitutes an environment value that is not valid UTF-8 as it
// is, stage F accepts it, and stage G reports it at the field it lands
// in, for a label, an endpoint address and a config member.
func TestSubstitutedInvalidUTF8(t *testing.T) {
	views, err := schemaview.Embedded(schemas(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ src, path string }{
		{`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r", "labels": {"team": "${TEAM}"}}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`, "metadata.labels.team"},
		{`{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "u"}, "spec": {"protocol": "http", "endpoints": [{"address": "${TEAM}:80"}]}}`, `spec.endpoints[address="caf�:80"].address`},
		{`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "auth.api-key", "config": {"header": "x-${TEAM}"}}}`, "spec.config.header"},
	} {
		f := newFixture()
		res := f.resource(t, "r.json", tc.src)
		if ds := subst.New(subst.Options{Schemas: schemas(t), Files: f.files, Variables: subst.Map{"TEAM": notUTF8}}).Apply(res); ds.HasErrors() {
			t.Fatalf("stage E: %q", texts(ds))
		}
		if ds, _ := views.Validate(res, f.files); ds.HasErrors() {
			t.Fatalf("stage F: %q", texts(ds))
		}
		_, ds := newStage(t).Resource(res, f.files)
		if len(ds) != 1 || ds[0].Path.String() != tc.path || ds[0].Line != 1 || ds[0].Column <= 1 {
			t.Errorf("stage G = %q, want one RZ-CFG-005 at %s", texts(ds), tc.path)
		}
	}
}

// TestCanonicalNumberGrammar covers the number rule on canonical content
// (R-64 waives the range there, not the grammar): a number literal
// outside RFC 8259 is RZ-CFG-005 at its field, in a canonical resource
// and an authored one, never the decode's fallback at the resource.
func TestCanonicalNumberGrammar(t *testing.T) {
	for _, role := range []tree.Role{tree.RoleCanonical, tree.RoleBase} {
		f := newFixture()
		res := f.resourceRole(t, "r.json", route(`{"match": {}, "upstreams": [{"name": "a", "weight": 2}]}`), role)
		w := at(t, res.Root, "spec.upstreams[0].weight")
		w.Text = "2x"
		_, ds := newStage(t).Resource(res, f.files)
		want := []string{posAt(w.Pos) + ` error RZ-CFG-005 Route/r spec.upstreams[name=a].weight: invalid number "2x"`}
		if got := texts(ds); !slices.Equal(got, want) {
			t.Errorf("role %v: %q, want %q", role, got, want)
		}
	}
}

// TestNoJSONKind covers a node of no JSON kind, which only a tree built
// in code holds: RZ-CFG-005 at its path, never a decode failure, in
// authored and canonical content alike, and as a member, a set element
// and a keyed-list entry or key, each one diagnostic (round 5: a keyed
// entry or key of no JSON kind was reported again as an entry without a
// key member).
func TestNoJSONKind(t *testing.T) {
	tests := []struct {
		name, src, at, want string
	}{
		{"member", route(`{"match": {}, "timeout": "1s"}`), "spec.timeout", "spec.timeout"},
		{"set element", route(`{"match": {"hosts": ["a.example", "b.example"]}}`), "spec.match.hosts[1]", "spec.match.hosts[1]"},
		{"keyed entry", route(`{"match": {}, "upstreams": [{"name": "a"}, {"name": "b"}]}`), "spec.upstreams[1]", "spec.upstreams[1]"},
		{"keyed key", route(`{"match": {}, "upstreams": [{"name": "a"}, {"name": "b"}]}`), "spec.upstreams[1].name", "spec.upstreams[1].name"},
	}
	for _, tc := range tests {
		for _, role := range roles() {
			t.Run(tc.name+"/"+role.name, func(t *testing.T) {
				f := newFixture()
				res := f.resourceRole(t, "r.json", tc.src, role.role)
				n := at(t, res.Root, tc.at)
				n.Kind = tree.Kind(99)
				_, ds := newStage(t).Resource(res, f.files)
				want := []string{posAt(n.Pos) + " error RZ-CFG-005 Route/r " + tc.want + ": node kind 99 has no JSON value"}
				if got := texts(ds); !slices.Equal(got, want) {
					t.Errorf("%q, want %q", got, want)
				}
			})
		}
	}
}

// nullMessage is the text of a null diagnostic for a member or element.
func nullMessage(what string) string {
	return "null is not allowed: the canonical form has no null; remove the " + what
}

// TestNilNodes covers nil nodes (WP-37 review round 3: some steps took a
// nil member value or element for null and others dereferenced it, so a
// converter or a FromResources tree holding one crashed Run's worker):
// a nil node is JSON null to every step, which Normalize reports as
// RZ-CFG-005 at its path, through Resource and Run, in typed and free
// content, in a keyed entry's key, in the dispatch member spec.type, in
// spec itself, in hosts lists and in the lifecycle walk, for authored
// content (no file table) and canonical content (round 5: no row had the
// canonical role).
func TestNilNodes(t *testing.T) {
	lifecycle, err := convert.NewRegistry(convert.Lifecycle{
		APIVersions: map[string]convert.Notice{v1: {Removal: "0.9.0"}},
		Fields:      map[string]map[string]convert.Notice{v1: {"spec.upstreams[].weight": {Removal: "0.8.0"}}},
	}, convert.V1alpha1())
	if err != nil {
		t.Fatal(err)
	}
	stages := map[string]*Stage{"default": newStage(t), "lifecycle": stageWith(t, Options{Converters: lifecycle})}
	tests := []struct {
		name string
		src  string
		// nil sets a node to nil: a member value (key set) of the object
		// at path, or the element at index of the list at path.
		path, key string
		index     int
		want      string
	}{
		{"member value", route(`{"match": {}, "timeout": "1s"}`), "spec", "timeout", 0, "spec.timeout: " + nullMessage("member")},
		{"list element", route(`{"match": {}, "upstreams": [{"name": "a"}, {"name": "b"}]}`), "spec.upstreams", "", 1, "spec.upstreams[1]: " + nullMessage("element")},
		{"keyed-list key", route(`{"match": {}, "upstreams": [{"name": "a", "weight": 2}]}`), "spec.upstreams[0]", "name", 0, "spec.upstreams[0].name: " + nullMessage("member")},
		{"host element", route(`{"match": {"hosts": ["a.example", "b.example"]}}`), "spec.match.hosts", "", 0, "spec.match.hosts[0]: " + nullMessage("element")},
		{"hosts list", route(`{"match": {"hosts": ["a.example"]}}`), "spec.match", "hosts", 0, "spec.match.hosts: " + nullMessage("member")},
		{"spec", policyDoc("cors", `{}`), "", "spec", 0, "spec: " + nullMessage("member")},
		{"policy type", policyDoc("cors", `{}`), "spec", "type", 0, "spec.type: " + nullMessage("member")},
		{"metadata name", policyDoc("cors", `{}`), "metadata", "name", 0, "metadata.name: " + nullMessage("member")},
		{"free content", pluginDoc(`{"a": [1, 2]}`), "spec.config.a", "", 1, "spec.config.a[1]: " + nullMessage("element")},
		{"apiVersion", route(`{"match": {}}`), "", "apiVersion", 0, "apiVersion: " + nullMessage("member")},
		{
			"annotations", `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r", "annotations": {}}, "spec": {"match": {}}}`, "metadata", "annotations", 0,
			"metadata.annotations: " + nullMessage("member"),
		},
	}
	// The resources are parsed as file 0, which the canonical table holds.
	canonicalFiles := func() *tree.FileTable {
		files := &tree.FileTable{}
		files.Add(tree.File{Path: "lkg.json", Role: tree.RoleCanonical})
		return files
	}
	tables := map[string]func() *tree.FileTable{"authored": func() *tree.FileTable { return nil }, "canonical": canonicalFiles}
	for _, tc := range tests {
		for sname, s := range stages {
			for tname, table := range tables {
				t.Run(tc.name+"/"+sname+"/"+tname, func(t *testing.T) {
					build := func() *tree.Resource {
						res := envelope(mustParse(t, tc.src))
						res.APIVersion = v1
						n := res.Root
						if tc.path != "" {
							n = at(t, res.Root, tc.path)
						}
						if tc.key != "" {
							i := slices.IndexFunc(n.Members, func(m tree.Member) bool { return m.Key == tc.key })
							n.Members[i].Value = nil
						} else {
							n.Items[tc.index] = nil
						}
						return res
					}
					files := table()
					h, ds := s.Resource(build(), files)
					if h != nil || !slices.ContainsFunc(texts(ds), func(d string) bool { return strings.HasSuffix(d, tc.want) }) {
						t.Errorf("Resource = %v, %q; want a diagnostic ending %q", h != nil, texts(ds), tc.want)
					}
					if b, rds, err := s.Run(t.Context(), []*tree.Resource{build(), build()}, files, 2); b != nil || err != nil || len(rds) != 2*len(ds) {
						t.Errorf("Run = %v, %q, %v; want the diagnostics of both", b != nil, texts(rds), err)
					}
				})
			}
		}
	}
}

// TestNilNodesEverywhere sets every member value and list element of the
// example Bundle's resources to nil in turn: stage G never panics, with or
// without a lifecycle table, and reports the null, but for the
// conversion-data annotation, which the conversion strips (02 req 7).
func TestNilNodesEverywhere(t *testing.T) {
	lifecycle, err := convert.NewRegistry(convert.Lifecycle{
		APIVersions: map[string]convert.Notice{v1: {Removal: "0.9.0"}},
		Fields:      map[string]map[string]convert.Notice{v1: {"spec.upstreams[].weight": {Removal: "0.8.0"}, "spec.config.limits[].burst": {Removal: "0.8.0"}}},
	}, convert.V1alpha1())
	if err != nil {
		t.Fatal(err)
	}
	stages := []*Stage{newStage(t), stageWith(t, Options{Converters: lifecycle})}
	_, rs := loadBundle(t)
	positions := 0
	for _, r := range rs {
		var walk func(n *tree.Node, path []int)
		walk = func(n *tree.Node, path []int) {
			count := len(n.Members) + len(n.Items)
			for i := range count {
				child := append(slices.Clone(path), i)
				for _, s := range stages {
					res := &tree.Resource{ID: r.ID, APIVersion: r.APIVersion, Start: r.Start, Root: r.Root.Clone()}
					parent := res.Root
					for _, j := range path {
						parent = childAt(parent, j)
					}
					stripped := false
					if i < len(parent.Members) {
						stripped = parent.Members[i].Key == convert.ConversionDataAnnotation
						parent.Members[i].Value = nil
					} else {
						parent.Items[i-len(parent.Members)] = nil
					}
					func() {
						defer func() {
							if p := recover(); p != nil {
								t.Fatalf("%s/%s position %v: panic %v", r.ID.Kind, r.ID.Name, child, p)
							}
						}()
						h, ds := s.Resource(res, nil)
						null := slices.ContainsFunc(ds, func(d diag.Diagnostic) bool { return strings.HasPrefix(d.Message, "null is not allowed") })
						if stripped == (h == nil) || stripped == null {
							t.Errorf("%s/%s position %v: %v, %q; want the null reported", r.ID.Kind, r.ID.Name, child, h != nil, texts(ds))
						}
					}()
				}
				positions++
				if c := childAt(n, i); c != nil {
					walk(c, child)
				}
			}
		}
		walk(r.Root, nil)
	}
	if positions < 100 {
		t.Errorf("%d positions, want the whole Bundle", positions)
	}
}

// childAt returns member value i of n, or element i-len(Members).
func childAt(n *tree.Node, i int) *tree.Node {
	if i < len(n.Members) {
		return n.Members[i].Value
	}
	return n.Items[i-len(n.Members)]
}

// TestNilNodeLookups covers the lookups that a nil node reaches outside
// Normalize: Decode with a nil index decodes nothing (as Normalize), and
// the exported Decode of a tree with a nil member value decodes it as
// null, in the kind struct and in a config split into pieces; a
// converter's diagnostic is located through a nil member value and next
// to a nil apiVersion member.
func TestNilNodeLookups(t *testing.T) {
	res := envelope(mustParse(t, route(`{"match": {}, "timeout": "1s"}`)))
	if obj, cfg, ds := Decode(nil, registry.New(), res, nil); obj != nil || cfg != nil || ds != nil {
		t.Errorf("Decode(nil index) = %v %v %q", obj, cfg, texts(ds))
	}
	spec, _ := res.Root.Get("spec")
	spec.Members[1].Value = nil
	if obj, _, ds := Decode(hubIndex(t), registry.New(), res, nil); obj == nil || len(ds) != 0 {
		t.Errorf("Decode with a nil member = %v, %q; want the object", obj, texts(ds))
	}
	cfg := envelope(mustParse(t, pluginDoc(`{"a": [1, 2, 3, 4, 5, 6, 7, 8], "b": {"c": 1, "d": 2, "e": 3}}`)))
	at(t, cfg.Root, "spec.config.a").Items[3] = nil
	at(t, cfg.Root, "spec.config.b").Members[1].Value = nil
	w := newWork()
	w.chunk = 2
	if obj, c, ds, failed := decode(hubIndex(t), registry.New(), cfg, nil, w, nil); obj == nil || c == nil || failed || len(ds) != 0 {
		t.Errorf("decode in pieces with nil nodes = %v %v %q %v", obj, c, texts(ds), failed)
	}
	// own: a path through a nil member value, and the fallback beside a
	// nil apiVersion member.
	f := newFixture()
	conv := f.resource(t, "r.json", route(`{"match": {}, "timeout": "1s"}`))
	cs, _ := conv.Root.Get("spec")
	cs.Members[1].Value = nil
	conv.Root.Members[0].Value = nil
	got := newStage(t).own(conv, f.files, diag.List{
		{Path: diag.Path{diag.Field("spec"), diag.Field("timeout"), diag.Field("x")}},
		{Path: diag.Path{diag.Field("spec"), diag.Field("timeout")}},
	}, newWork())
	if got[0].Location != (diag.Location{File: "r.json", Line: int(conv.Start.Line), Column: int(conv.Start.Column)}) {
		t.Errorf("fallback beside a nil apiVersion = %+v, want the resource start", got[0].Location)
	}
	if want := (diag.Location{File: "r.json", Line: int(cs.Pos.Line), Column: int(cs.Pos.Column)}); got[1].Location != want {
		t.Errorf("path to a nil member = %+v, want its parent's %+v", got[1].Location, want)
	}
}
