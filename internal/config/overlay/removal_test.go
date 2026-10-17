// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

const (
	discoveryUpstream = `{"apiVersion":"ruralz/v1alpha1","kind":"Upstream","metadata":{"name":"u"},"spec":{"protocol":"http","discovery":{"type":"dns","service":"s.example","port":80}}}`
	plainUpstream     = `{"apiVersion":"ruralz/v1alpha1","kind":"Upstream","metadata":{"name":"u"},"spec":{"protocol":"http","endpoints":[{"address":"a:1"}]}}`
	pluginPolicy      = `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"plugin","plugin":"geo","config":{"k":1}}}`
)

// TestRemovalOnlyIsNoop covers 01 req 25 "absent key: no-op" and 01 risk
// 18 (review finding): an overlay value that holds nothing but removals
// (null members, $patch: delete entries, at any depth) adds no member the
// base lacks, and leaves a base value of another kind in place; a
// written {} or [] and an applied {$patch: replace} are values and stay.
// Each row is one member of the class; path names the spec member to
// read, want its merged value or "absent".
func TestRemovalOnlyIsNoop(t *testing.T) {
	over := func(kind, name, spec string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"` + kind + `","metadata":{"name":"` + name + `"},"spec":` + spec + `}`
	}
	cases := []struct {
		name, base, over, member, want string
	}{
		{
			"req25 delete-only map list on an absent list", discoveryUpstream,
			over("Upstream", "u", `{"endpoints":[{"address":"a:1","$patch":"delete"}]}`), "endpoints", "absent",
		},
		{
			"req25 several delete entries on an absent map list", discoveryUpstream,
			over("Upstream", "u", `{"endpoints":[{"address":"a:1","$patch":"delete"},{"address":"b:2","$patch":"delete"}]}`), "endpoints", "absent",
		},
		{
			"req25 delete-only orderedMap list on an absent list", route,
			over("Route", "r", `{"policies":[{"name":"x","$patch":"delete"}]}`), "policies", "absent",
		},
		{
			"req25 nested delete-only list inside a new member", route,
			over("Route", "r", `{"composition":{"steps":[{"name":"s","$patch":"delete"}]}}`), "composition", "absent",
		},
		{
			"req25 null-only object on an absent object", plainUpstream,
			over("Upstream", "u", `{"tls":{"sni":null}}`), "tls", "absent",
		},
		{
			"req25 nested null-only object on an absent object", plainUpstream,
			over("Upstream", "u", `{"tls":{"caCertificate":{"secretRef":null}}}`), "tls", "absent",
		},
		{
			"req25 nulls and nested removals together", plainUpstream,
			over("Upstream", "u", `{"tls":{"sni":null,"caCertificate":{"secretRef":null},"clientKey":{}}}`), "tls", `{"clientKey":{}}`,
		},
		{
			"req25 nested null-only free content", pluginPolicy,
			over("Policy", "p", `{"config":{"a":{"b":{"c":null}}}}`), "config", `{"k":1}`,
		},
		{
			"req25 a list in free content is a value whose elements stay", pluginPolicy,
			over("Policy", "p", `{"config":{"l":[{"x":null}]}}`), "config", `{"k":1,"l":[{}]}`,
		},
		{
			"req25 written empty list stays", route,
			over("Route", "r", `{"policies":[]}`), "policies", `[]`,
		},
		{
			"req25 written empty object stays", plainUpstream,
			over("Upstream", "u", `{"tls":{}}`), "tls", `{}`,
		},
		{
			"req25 replace on an absent list stays", route,
			over("Route", "r", `{"policies":[{"$patch":"replace"}]}`), "policies", `[]`,
		},
		{
			"req25 replace and a delete on an absent list stays", route,
			over("Route", "r", `{"policies":[{"$patch":"replace"},{"name":"x","$patch":"delete"}]}`), "policies", `[]`,
		},
		{
			"req25 a delete beside a new entry adds the entry", route,
			over("Route", "r", `{"policies":[{"name":"x","$patch":"delete"},{"name":"y"}]}`), "policies", `[{"name":"y"}]`,
		},
		{
			"req25 a null beside a value adds the value", plainUpstream,
			over("Upstream", "u", `{"tls":{"sni":null,"caCertificate":{"secretRef":{"provider":"file","name":"/ca"}}}}`), "tls",
			`{"caCertificate":{"secretRef":{"provider":"file","name":"/ca"}}}`,
		},
		{
			"req25 removals against a base scalar leave it", route,
			over("Route", "r", `{"timeout":{"a":null}}`), "timeout", `"5s"`,
		},
		{
			"req25 delete-only list against a base object leaves it",
			`{"apiVersion":"ruralz/v1alpha1","kind":"Upstream","metadata":{"name":"u"},"spec":{"protocol":"http","endpoints":{"a":1}}}`,
			over("Upstream", "u", `{"endpoints":[{"address":"a:1","$patch":"delete"}]}`), "endpoints", `{"a":1}`,
		},
		{
			"req25 a null-only object against a base list leaves it", plainUpstream,
			over("Upstream", "u", `{"endpoints":{"x":null}}`), "endpoints", `[{"address":"a:1"}]`,
		},
		{
			"req25 a value against a base scalar replaces it", route,
			over("Route", "r", `{"timeout":{"a":1,"b":null}}`), "timeout", `{"a":1}`,
		},
		{
			"req25 null-only object in a new keyed entry", gateway,
			over("Gateway", "edge", `{"listeners":[{"name":"x","protocol":"http","port":1,"tls":{"minVersion":null}}]}`), "listeners",
			`[{"name":"http","protocol":"http","port":8080},
			{"name":"https","protocol":"https","port":8443,"tls":{"certificates":[{"name":"main","certificate":{"secretRef":{"provider":"file","name":"/c"}},"privateKey":{"secretRef":{"provider":"file","name":"/k"}}}]}},
			{"name":"admin","protocol":"http","port":9000},{"name":"x","protocol":"http","port":1}]`,
		},
		{
			"req25 null-only object in a matched keyed entry", gateway,
			over("Gateway", "edge", `{"listeners":[{"name":"http","tls":{"minVersion":null}}]}`), "listeners.0", `{"name":"http","protocol":"http","port":8080}`,
		},
		{
			"req25 an element of a list replaced whole stays empty",
			`{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"h"},"spec":{"type":"headers","config":{"request":{"add":[{"name":"a","value":"1"}]}}}}`,
			over("Policy", "h", `{"config":{"request":{"add":[{"x":null},{"name":"b","value":"2"}]}}}`), "config.request.add", `[{},{"name":"b","value":"2"}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ds, _ := run(t, []file{{"ruralz.yaml", tc.base}}, []file{{"overlays/prod/a.yaml", tc.over}})
			if len(ds) != 0 || len(out) != 1 {
				t.Fatalf("%d resources, diagnostics %q", len(out), texts(ds))
			}
			spec := at(t, out[0].Root, "spec")
			got := "absent"
			if n, ok := spec.At(pathOf(tc.member)); ok {
				got = dump(n)
			}
			if want := tc.want; want == "absent" && got != want || want != "absent" && got != compact(t, want) {
				t.Errorf("spec.%s = %s, want %s (spec %s)", tc.member, got, tc.want, dump(spec))
			}
		})
	}
}

// pathOf turns a dotted member path with numeric list indexes into a
// diag.Path.
func pathOf(p string) (out diag.Path) {
	for _, seg := range strings.Split(p, ".") {
		if i, err := strconv.Atoi(seg); err == nil {
			out = append(out, diag.Index(i))
			continue
		}
		out = append(out, diag.Field(seg))
	}
	return out
}

// TestRemovalOnlyEndToEnd is the review finding's repro through stage F:
// deleting an endpoint of an Upstream that uses discovery must not add
// endpoints: [], which schema validation rejects (at most one of
// endpoints and discovery), and deleting a Policy of a Route without
// policies must leave the Route as its base wrote it, so its canonical
// form and Revision digest do not change.
func TestRemovalOnlyEndToEnd(t *testing.T) {
	views, err := schemaview.Embedded(schemas(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, base, over string }{
		{
			"req25 Upstream with discovery, delete an absent endpoint", discoveryUpstream,
			`{"apiVersion":"ruralz/v1alpha1","kind":"Upstream","metadata":{"name":"u"},"spec":{"endpoints":[{"address":"a:1","$patch":"delete"}]}}`,
		},
		{
			"req25 Route without policies, delete an absent policy",
			`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"path":{"prefix":"/a"}},"upstreams":[{"name":"u"}]}}`,
			`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"policies":[{"name":"x","$patch":"delete"}]}}`,
		},
		{
			"req25 Upstream without tls, null a tls member", plainUpstream,
			`{"apiVersion":"ruralz/v1alpha1","kind":"Upstream","metadata":{"name":"u"},"spec":{"tls":{"sni":null}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, ds, files := run(t, []file{{"ruralz.yaml", tc.base}}, []file{{"overlays/p/a.yaml", tc.over}})
			if len(ds) != 0 || len(out) != 1 {
				t.Fatalf("%d resources, overlay diagnostics %q", len(out), texts(ds))
			}
			if g, w := dump(out[0].Root), compact(t, tc.base); g != w {
				t.Errorf("merged %s, want the base unchanged %s", g, w)
			}
			vd, ok := views.Validate(out[0], files)
			if !ok || len(vd) != 0 {
				t.Errorf("schema validation: %q", texts(vd))
			}
		})
	}
}

// TestPatchReplaceKeepsSpec: ruralz.io/patch: replace replaces the base
// spec with the overlay's (01 req 25), an explicit replacement that keeps
// its spec even when nothing but removals remain in it, as {$patch:
// replace} keeps a list; without the annotation such a spec adds nothing.
func TestPatchReplaceKeepsSpec(t *testing.T) {
	doc := func(annotation, spec string) string {
		md := `{"name":"u"}`
		if annotation != "" {
			md = `{"name":"u","annotations":{"ruralz.io/patch":"` + annotation + `"}}`
		}
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Upstream","metadata":` + md + `,"spec":` + spec + `}`
	}
	for _, tc := range []struct {
		name, base, over, want string
	}{
		{"req25 replace with a null-only spec", plainUpstream, doc("replace", `{"tls":null}`), `{}`},
		{"req25 replace with a delete-only list", plainUpstream, doc("replace", `{"endpoints":[{"address":"a:1","$patch":"delete"}]}`), `{}`},
		{"req25 replace with a nested null-only object", plainUpstream, doc("replace", `{"protocol":"http","tls":{"sni":null}}`), `{"protocol":"http"}`},
		{"req25 replace on a new identity", "", doc("replace", `{"tls":null}`), `{}`},
		{"req25 merge with a null-only spec on a new identity", "", doc("", `{"tls":null}`), "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var base []file
			if tc.base != "" {
				base = []file{{"ruralz.yaml", tc.base}}
			}
			out, ds, _ := run(t, base, []file{{"overlays/p/a.yaml", tc.over}})
			if len(ds) != 0 || len(out) != 1 {
				t.Fatalf("%d resources, diagnostics %q", len(out), texts(ds))
			}
			got := "absent"
			if spec, ok := out[0].Root.Get("spec"); ok {
				got = dump(spec)
			}
			if got != tc.want {
				t.Errorf("spec = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestRemovalOnlyEveryKeyedList runs the delete-only overlay of 01 req 25
// on every map and orderedMap list field of every kind and dispatch
// branch, enumerated from the committed schema (R-62): against a base
// whose list is absent the overlay adds nothing, and the list stays
// absent.
func TestRemovalOnlyEveryKeyedList(t *testing.T) {
	set := schemas(t)
	x, _ := set.Index(v1)
	n := 0
	for f := range x.FieldsSeq() {
		if f.Path == "" || !f.Node.Types().Has(schemaidx.TypeArray) || !keyed(f.Node) {
			continue
		}
		_, key := f.Node.List()
		n++
		name := f.Kind + " " + f.Path
		base := skeleton(t, x, f, list(entry(key, "a")))
		if !dropLeaf(base, f.Path) {
			t.Fatalf("%s: the skeleton has no list to drop", name)
		}
		id := tree.ID{Kind: v1alpha1.Kind(f.Kind), Name: "x"}
		want := dump(base)
		b := &tree.Resource{ID: id, APIVersion: v1, Root: base}
		o := &tree.Resource{ID: id, APIVersion: v1, Root: skeleton(t, x, f, list(entry(key, "a", PatchKey, PatchDelete)))}
		out, ds := pipeline(t, []*tree.Resource{b}, []*tree.Resource{o}, Options{Schemas: set})
		if len(ds) != 0 || len(out) != 1 {
			t.Fatalf("%s: %d resources, diagnostics %q", name, len(out), texts(ds))
		}
		if got := dump(out[0].Root); got != want {
			t.Errorf("%s: delete-only overlay on an absent list gave\n%s\nwant the base\n%s", name, got, want)
		}
	}
	if n == 0 {
		t.Fatal("no keyed list field in the schema")
	}
}

// dropLeaf removes the last member of schema path p ("spec.a[].b") from
// root, following the first element of every list on the way.
func dropLeaf(root *tree.Node, p string) bool {
	segs := splitSchemaPath(p)
	parent := follow(root, strings.Join(segs[:len(segs)-1], "."))
	return parent != nil && parent.Delete(segs[len(segs)-1])
}

// TestDispatchTestsScalars pins the assumptions mergeMap's dispatch and
// settle rest on (review findings, 01 req 25): every if/then rule of the
// rendered schema tests scalar constants of members it requires, so a new
// composite member, present or dropped as a removal, and a base composite
// replaced by an overlay composite of the other kind never change which
// branch applies; and no rule has an else branch, so the dispatch is
// monotone: a composite standing in a member a rule tests only makes that
// rule fail, and a failing rule adds no schema, so settle's test that
// standing every overlay composite in at once leaves the dispatch as the
// base scalars give it also covers every mix of them. A base scalar is
// another matter, since it comes back when the overlay composite
// replacing it holds nothing but removals: settle merges those the
// dispatch reads before it is read (TestRemovalOnlyDispatchMember). A
// synthetic schema of each kind of rule the checks refuse shows they find
// it.
func TestDispatchTestsScalars(t *testing.T) {
	var doc any
	if err := json.Unmarshal(schema.RenderedV1alpha1(), &doc); err != nil {
		t.Fatal(err)
	}
	problems, rules := ruleProblems(doc)
	for _, p := range problems {
		t.Error(p)
	}
	if rules == 0 {
		t.Fatal("no if/then rule in the rendered schema")
	}
	for name, src := range map[string]string{
		"an else branch":                `{"allOf":[{"if":{"properties":{"mode":{"const":"b"}},"required":["mode"]},"then":{"properties":{"x":{}}},"else":{"properties":{"y":{}}}}]}`,
		"a member tested without const": `{"allOf":[{"if":{"properties":{"mode":{"type":"string"}},"required":["mode"]},"then":{}}]}`,
		"an unrequired member":          `{"allOf":[{"if":{"properties":{"mode":{"const":"b"}}},"then":{}}]}`,
		"a composite constant":          `{"allOf":[{"if":{"properties":{"mode":{"const":{"a":1}}},"required":["mode"]},"then":{}}]}`,
		"another keyword":               `{"allOf":[{"if":{"properties":{"mode":{"const":"b"}},"required":["mode"],"minProperties":1},"then":{}}]}`,
	} {
		var v any
		if err := json.Unmarshal([]byte(src), &v); err != nil {
			t.Fatal(err)
		}
		if problems, _ := ruleProblems(v); len(problems) == 0 {
			t.Errorf("%s: the checks find no problem in %s", name, src)
		}
	}
}

// ruleProblems returns what breaks the dispatch assumptions in the if/then
// rules of the schema document doc, and the number of rules.
func ruleProblems(doc any) (problems []string, rules int) {
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if cond, ok := v["if"].(map[string]any); ok {
				rules++
				if _, ok := v["else"]; ok {
					problems = append(problems, fmt.Sprintf("if rule %v has an else branch: the dispatch is not monotone (settle)", cond))
				}
				props, _ := cond["properties"].(map[string]any)
				required, _ := cond["required"].([]any)
				for k := range cond {
					if k != "properties" && k != "required" {
						problems = append(problems, fmt.Sprintf("if rule %v tests %q", cond, k))
					}
				}
				if len(required) == 0 {
					problems = append(problems, fmt.Sprintf("if rule %v requires no member", cond))
				}
				for _, r := range required {
					p, _ := props[r.(string)].(map[string]any)
					c, ok := p["const"]
					if !ok || len(p) != 1 {
						problems = append(problems, fmt.Sprintf("if rule %v: required member %v is not tested against a constant alone", cond, r))
					}
					switch c.(type) {
					case map[string]any, []any:
						problems = append(problems, fmt.Sprintf("if rule %v: constant %v is not a scalar", cond, c))
					}
				}
				for k := range props {
					if !slices.Contains(required, any(k)) {
						problems = append(problems, fmt.Sprintf("if rule %v tests %q without requiring it", cond, k))
					}
				}
			}
			for _, x := range v {
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(doc)
	return problems, rules
}

// TestDispatchMembersFixed pins the assumption settle rests on (review
// finding, 01 req 25): every member an if/then rule of the rendered
// schema tests has one schema whichever branch applies, so settle, which
// merges such a member under its schema in the dispatch the base scalars
// give, before the merged dispatch is known, merges it as the merged
// dispatch reads it. A synthetic schema whose branch changes the member
// it tests shows the check finds one.
func TestDispatchMembersFixed(t *testing.T) {
	x, _ := schemas(t).Index(v1)
	tested, unfixed := dispatchMembers(x)
	if !slices.Contains(tested, "Policy spec.type") {
		t.Errorf("tested members %q, want Policy spec.type among them", tested)
	}
	if len(unfixed) > 0 {
		t.Errorf("members a dispatch rule tests with more than one schema: %q", unfixed)
	}
	const changing = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {"apiVersion": {"const": "test/v1"}, "kind": {"enum": ["Thing"]}},
  "allOf": [{"if": {"properties": {"kind": {"const": "Thing"}}, "required": ["kind"]}, "then": {"$ref": "#/$defs/Thing"}}],
  "$defs": {
    "Thing": {"type": "object", "properties": {"kind": {"const": "Thing"}, "spec": {"$ref": "#/$defs/Spec"}}},
    "Spec": {"type": "object", "properties": {"mode": {"type": "string"}},
      "allOf": [{"if": {"properties": {"mode": {"const": "b"}}, "required": ["mode"]}, "then": {"properties": {"mode": {"minLength": 1}}}}]}
  }
}`
	sx, err := schemaidx.Load("test/v1", []byte(changing))
	if err != nil {
		t.Fatal(err)
	}
	if _, unfixed := dispatchMembers(sx); !slices.Equal(unfixed, []string{"Thing spec.mode"}) {
		t.Errorf("a branch changing the member it tests: unfixed %q, want [Thing spec.mode]", unfixed)
	}
}

// dispatchMembers returns the members the if/then rules of x test, as
// "<kind> <schema path>", and those of them that have more than one
// schema among the positions Fields lists (it lists a position again
// under each branch that changes it), both sorted.
func dispatchMembers(x *schemaidx.Index) (tested, unfixed []string) {
	is := map[string]bool{}
	schemaOf := map[string][]*schemaidx.Node{}
	for f := range x.FieldsSeq() {
		id := f.Kind + " " + f.Path
		if !slices.Contains(schemaOf[id], f.Node) {
			schemaOf[id] = append(schemaOf[id], f.Node)
		}
		for _, d := range f.Where {
			for _, c := range d.Conditions {
				p := c.Member
				if d.Path != "" {
					p = d.Path + "." + c.Member
				}
				is[f.Kind+" "+p] = true
			}
		}
	}
	for id := range is {
		tested = append(tested, id)
		if len(schemaOf[id]) != 1 {
			unfixed = append(unfixed, id)
		}
	}
	slices.Sort(tested)
	slices.Sort(unfixed)
	return tested, unfixed
}

// fixedDispatch is a rendered schema whose dispatch members are not
// plain scalars: spec.mode, a string or an object whose doc member is a
// JSONSchemaDocument in every branch, makes spec.x a document when it is
// b; spec.other y adds spec.z. It shows settle reads a member's own schema
// (TestRemovalOnlyDispatchMember) and that the related location is the
// first changed member in overlay order (TestRedispatchRelated).
const fixedDispatch = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {"apiVersion": {"const": "test/v1"}, "kind": {"enum": ["Thing"]}},
  "allOf": [{"if": {"properties": {"kind": {"const": "Thing"}}, "required": ["kind"]}, "then": {"$ref": "#/$defs/Thing"}}],
  "$defs": {
    "JSONSchemaDocument": {"type": ["object", "boolean"]},
    "Thing": {"type": "object", "properties": {
      "apiVersion": {"const": "test/v1"}, "kind": {"const": "Thing"},
      "metadata": {"type": "object"}, "spec": {"$ref": "#/$defs/Spec"}}},
    "Spec": {"type": "object",
      "properties": {"mode": {"type": ["string", "object"], "properties": {"doc": {"$ref": "#/$defs/JSONSchemaDocument"}}},
        "other": {"type": "string"}, "x": {"type": "object"}},
      "allOf": [
        {"if": {"properties": {"mode": {"const": "b"}}, "required": ["mode"]}, "then": {"properties": {"x": {"$ref": "#/$defs/JSONSchemaDocument"}}}},
        {"if": {"properties": {"other": {"const": "y"}}, "required": ["other"]}, "then": {"properties": {"z": {"type": "object"}}}}]}
  }
}`

// syntheticSet indexes a synthetic rendered schema of apiVersion test/v1.
func syntheticSet(t *testing.T, src string) *schemaidx.Set {
	t.Helper()
	x, err := schemaidx.Load("test/v1", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	set, err := schemaidx.NewSet(x)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// TestRemovalOnlyDispatchMember covers 01 req 25 ("inside
// Policy.spec.config the type dispatch uses the merged spec.type"; "absent
// key: no-op") for a member a dispatch rule tests (review finding): an
// overlay composite that replaces the base scalar and holds nothing but
// removals leaves the base scalar, so the object merges under the base
// dispatch, as it does without that member; a composite of which
// something remains replaces the scalar and the object merges under the
// dispatch that value selects. Each row is one class member; want is the
// merged spec and diags the expected diagnostic codes.
func TestRemovalOnlyDispatchMember(t *testing.T) {
	synthetic, fixed := syntheticSet(t, nestedDispatch), syntheticSet(t, fixedDispatch)
	policy := func(spec string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"v"},"spec":` + spec + `}`
	}
	thing := func(spec string) string {
		return `{"apiVersion":"test/v1","kind":"Thing","metadata":{"name":"t"},"spec":` + spec + `}`
	}
	const (
		docPatch = `{"type":"validation.json-schema","config":{"schema":{"$patch":1}}}`
		issuers  = `{"type":"auth.jwt","config":{"issuers":[{"issuer":"a","jwksUri":"https://a/j"},{"issuer":"b","jwksUri":"https://b/j"}]}}`
	)
	for _, tc := range []struct {
		name, base, over, want string
		diags                  []string
		set                    *schemaidx.Set
	}{
		{
			name: "(a) a base $patch inside the document stays data", base: policy(docPatch),
			over: policy(`{"type":{"a":null}}`), want: docPatch,
		},
		{
			name: "(a) a nested value holding nothing but removals", base: policy(docPatch),
			over: policy(`{"type":{"x":{"y":null}}}`), want: docPatch,
		},
		{
			name: "(b) a map list still merges by key", base: policy(issuers),
			over: policy(`{"type":{"x":null},"config":{"issuers":[{"issuer":"a","jwksUri":"https://a/k"}]}}`),
			want: `{"type":"auth.jwt","config":{"issuers":[{"issuer":"a","jwksUri":"https://a/k"},{"issuer":"b","jwksUri":"https://b/j"}]}}`,
		},
		{
			name: "(c) a delete entry still deletes", base: policy(issuers),
			over: policy(`{"type":{"x":{"y":null}},"config":{"issuers":[{"issuer":"b","$patch":"delete"}]}}`),
			want: `{"type":"auth.jwt","config":{"issuers":[{"issuer":"a","jwksUri":"https://a/j"}]}}`,
		},
		{
			name: "(d) the document is still replaced whole",
			base: policy(`{"type":"validation.json-schema","config":{"schema":{"type":"object","required":["a"]}}}`),
			over: policy(`{"type":{"x":null},"config":{"schema":{"type":"string"}}}`),
			want: `{"type":"validation.json-schema","config":{"schema":{"type":"string"}}}`,
		},
		{
			name: "a second base scalar replaced beside the dispatch member merges under the base type",
			base: policy(`{"type":"validation.json-schema","config":"x"}`),
			over: policy(`{"type":{"a":null},"config":{"schema":{"$patch":1}}}`),
			want: docPatch,
		},
		{
			name: "(e) a boolean dispatch member", set: synthetic,
			base: thing(`{"flag":true,"doc":{"$patch":1}}`), over: thing(`{"flag":{"a":null}}`),
			want: `{"flag":true,"doc":{"$patch":1}}`,
		},
		{
			name: "(e) a string dispatch member of the synthetic schema", set: synthetic,
			base: thing(`{"mode":"b","inner":{"type":"doc","config":{"$patch":1}}}`), over: thing(`{"mode":{"a":null}}`),
			want: `{"mode":"b","inner":{"type":"doc","config":{"$patch":1}}}`,
		},
		{
			name: "(e) a dispatch member of a nested object", set: synthetic,
			base: thing(`{"mode":"b","inner":{"type":"doc","config":{"$patch":1}}}`), over: thing(`{"inner":{"type":{"z":null}}}`),
			want: `{"mode":"b","inner":{"type":"doc","config":{"$patch":1}}}`,
		},
		{
			name: "a dispatch member with structure, holding nothing but removals", set: fixed,
			base: thing(`{"mode":"b","x":{"$patch":1}}`), over: thing(`{"mode":{"k":null}}`),
			want: `{"mode":"b","x":{"$patch":1}}`,
		},
		{
			name: "negative: a dispatch member with structure keeps a null inside its document", set: fixed,
			base: thing(`{"mode":"b","x":{"$patch":1}}`), over: thing(`{"mode":{"doc":{"k":null}}}`),
			want: `{"mode":{"doc":{"k":null}},"x":{}}`, diags: []string{CodeBasePatch},
		},
		{
			name: "negative: a value replaces the type and clears the dispatch", base: policy(docPatch),
			over: policy(`{"type":{"a":1}}`), want: `{"type":{"a":1},"config":{"schema":{}}}`, diags: []string{CodeBasePatch},
		},
		{
			name: "negative: under a cleared dispatch a map list is replaced whole", base: policy(issuers),
			over: policy(`{"type":{"a":1},"config":{"issuers":[{"issuer":"a","jwksUri":"https://a/k"}]}}`),
			want: `{"type":{"a":1},"config":{"issuers":[{"issuer":"a","jwksUri":"https://a/k"}]}}`,
		},
		{
			name: "negative: a misplaced $patch in a type value is reported once", base: policy(docPatch),
			over: policy(`{"type":{"$patch":1,"a":1}}`), want: `{"type":{"a":1},"config":{"schema":{}}}`, diags: []string{CodeBasePatch, CodeSchema},
		},
		{
			name: "negative: a misplaced $patch in a list replacing the type is reported once", base: policy(docPatch),
			over: policy(`{"type":[{"$patch":1,"a":1}]}`), want: `{"type":[{"a":1}],"config":{"schema":{}}}`, diags: []string{CodeBasePatch, CodeSchema},
		},
		{
			name: "negative: a written list replaces the type", base: policy(docPatch),
			over: policy(`{"type":[]}`), want: `{"type":[],"config":{"schema":{}}}`, diags: []string{CodeBasePatch},
		},
		{
			name: "negative: a second scalar replaced beside a type value merges as free content, its $patch misplaced",
			base: policy(`{"type":"validation.json-schema","config":"x"}`),
			over: policy(`{"type":{"a":1},"config":{"schema":{"$patch":1}}}`),
			want: `{"type":{"a":1},"config":"x"}`, diags: []string{CodeSchema},
		},
		{
			name: "negative: removals over a base composite leave it",
			base: policy(`{"type":["x"],"config":{"issuers":[{"issuer":"a"}]}}`),
			over: policy(`{"type":{"a":null},"config":{"issuers":[{"issuer":"b"}]}}`),
			want: `{"type":["x"],"config":{"issuers":[{"issuer":"b"}]}}`,
		},
		{
			name: "negative: a boolean dispatch member given a value", set: synthetic,
			base: thing(`{"flag":true,"doc":{"$patch":1}}`), over: thing(`{"flag":{"a":1}}`),
			want: `{"flag":{"a":1},"doc":{}}`, diags: []string{CodeBasePatch},
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
			out, ds := pipeline(t, b, o, Options{Schemas: set, Files: files})
			if g := codes(ds); !slices.Equal(g, tc.diags) {
				t.Errorf("codes = %v, want %v: %q", g, tc.diags, texts(ds))
			}
			if len(out) != 1 {
				t.Fatalf("%d resources", len(out))
			}
			if g, w := dump(at(t, out[0].Root, "spec")), compact(t, tc.want); g != w {
				t.Errorf("spec = %s, want %s", g, w)
			}
		})
	}
}
