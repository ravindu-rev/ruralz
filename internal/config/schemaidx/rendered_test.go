// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaidx

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Tests over the committed rendered view (WP-03 "Done when": every schema
// path resolvable; keyed-list keys and defaults found for every marker the
// generator emitted). Markers are read from the schema at run time (R-62).

// isMarker reports the keywords the index must surface at their position.
func isMarker(k string) bool { return k == "default" || strings.HasPrefix(k, "x-ruralz-") }

func TestEmbeddedSet(t *testing.T) {
	// 02 req 4: resources decode under their own apiVersion's rendered
	// schema; an unserved apiVersion has no index (RZ-CFG-007 upstream).
	s, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Served(); !slices.Contains(got, "ruralz/v1alpha1") {
		t.Fatalf("Served() = %v, want ruralz/v1alpha1", got)
	}
	x, ok := s.Index("ruralz/v1alpha1")
	if !ok || x.APIVersion() != "ruralz/v1alpha1" {
		t.Fatalf("Index(ruralz/v1alpha1) = %v, %v", x, ok)
	}
	if _, ok := s.Index("ruralz/v9"); ok {
		t.Error("Index(ruralz/v9) found")
	}
}

func TestKindsFollowRootDispatch(t *testing.T) {
	doc := rawDoc(t)
	var want []string
	for _, m := range doc["allOf"].([]any) {
		c := m.(map[string]any)["if"].(map[string]any)["properties"].(map[string]any)["kind"].(map[string]any)["const"].(string)
		want = append(want, c)
	}
	x := v1(t)
	if got := x.Kinds(); !slices.Equal(got, want) {
		t.Fatalf("Kinds() = %v, want %v", got, want)
	}
	defs := doc["$defs"].(map[string]any)
	for _, k := range want {
		r, ok := x.Resource(k)
		if !ok || r.Def() != k {
			t.Errorf("Resource(%s).Def() = %q, %v", k, r.Def(), ok)
		}
		if !r.Closed() || !slices.Equal(r.PropertyNames(), []string{"apiVersion", "kind", "metadata", "spec"}) {
			t.Errorf("Resource(%s): closed %v, properties %v", k, r.Closed(), r.PropertyNames())
		}
		if got := r.Required(); !slices.Equal(got, []string{"apiVersion", "kind", "metadata", "spec"}) {
			t.Errorf("Resource(%s).Required() = %v", k, got)
		}
		kn, _ := r.Property("kind")
		if !slices.Equal(kn.Enum(), []string{k}) {
			t.Errorf("Resource(%s) kind enum = %v", k, kn.Enum())
		}
		spec, ok := x.Spec(k)
		wantSpec := strings.TrimPrefix(defs[k].(map[string]any)["properties"].(map[string]any)["spec"].(map[string]any)["$ref"].(string), "#/$defs/")
		if !ok || spec.Def() != wantSpec {
			t.Errorf("Spec(%s).Def() = %q, want %q", k, spec.Def(), wantSpec)
		}
	}
	if _, ok := x.Resource("Nothing"); ok {
		t.Error("Resource(Nothing) found")
	}
	if _, ok := x.Spec("Nothing"); ok {
		t.Error("Spec(Nothing) found")
	}
}

func TestEverySchemaPathResolvable(t *testing.T) {
	// Done when: every property, items and additionalProperties schema of
	// the committed schema is reached from some resource root, and every
	// enumerated field resolves by Lookup and by Walk to the same node.
	x := v1(t)
	fields := x.Fields()
	reached := map[string]bool{}
	for _, f := range fields {
		for _, p := range partPtrs(f.Node) {
			reached[p] = true
		}
	}
	rawSchemas(rawDoc(t), func(ptr string, _ map[string]any) {
		if strings.Contains(ptr, "/properties/") || strings.HasSuffix(ptr, "/items") || strings.HasSuffix(ptr, "/additionalProperties") {
			if !reached[ptr] {
				t.Errorf("schema position %s is not reachable from any resource root", ptr)
			}
		}
	})
	for _, f := range fields {
		res, path := instanceFor(f)
		info, ok := x.Lookup(f.Kind, res, path)
		if !ok || info.Node != f.Node {
			t.Errorf("Lookup(%s %s %v) = %v %v, want the enumerated node", f.Kind, f.Path, f.Where, info.Node.Def(), ok)
			continue
		}
		// 02 req 76: every position's schema path, the lifecycle-table key,
		// is the path Fields enumerates.
		if sp, ok := x.SchemaPath(f.Kind, res, path); !ok || sp != f.Path {
			t.Errorf("SchemaPath(%s %v) = %q, %v, want %q", f.Kind, path, sp, ok, f.Path)
		}
		found := false
		err := x.Walk(f.Kind, res, func(c *Cursor) error {
			if c.Depth() == len(path) && samePosition(c.Path(), path) {
				found = c.Info.Node == f.Node
			}
			return nil
		})
		if err != nil || !found {
			t.Errorf("Walk(%s) did not reach %s with the enumerated node (err %v)", f.Kind, f.Path, err)
		}
	}
}

// samePosition compares paths by member names, treating every list
// element form (index, keyed, item) alike.
func samePosition(a, b diag.Path) bool {
	return slices.EqualFunc(a, b, func(x, y diag.PathElem) bool {
		if x.Kind == diag.ElemField || y.Kind == diag.ElemField {
			return x.Kind == y.Kind && x.Name == y.Name
		}
		return true
	})
}

// rawKeywords parses the markers of one raw schema object independently of
// the compiler.
func rawKeywords(t *testing.T, ptr string, m map[string]any) (Keywords, any, bool) {
	t.Helper()
	var k Keywords
	if l, ok := m["x-ruralz-list"].(map[string]any); ok {
		k.List, _ = parseListType(l["type"].(string))
		k.ListKey, _ = l["key"].(string)
	}
	k.Ref, _ = m["x-ruralz-ref"].(string)
	k.Secret, _ = m["x-ruralz-secret"].(bool)
	if c, ok := m["x-ruralz-cel"].(map[string]any); ok {
		spec := &CELSpec{Result: c["result"].(string)}
		for _, v := range c["variables"].([]any) {
			spec.Variables = append(spec.Variables, v.(string))
		}
		k.CEL = spec
	}
	if l, ok := m["x-ruralz-impact"].([]any); ok {
		for _, v := range l {
			i, ok := ParseImpact(v.(string))
			if !ok {
				t.Errorf("%s: impact %q", ptr, v)
			}
			k.Impact |= i
		}
	}
	if n, ok := m["x-ruralz-since"].(json.Number); ok {
		i, _ := n.Int64()
		k.Since = int(i)
	}
	if l, ok := m["x-ruralz-validations"].([]any); ok {
		for _, v := range l {
			vm := v.(map[string]any)
			msg, _ := vm["message"].(string)
			k.Validations = append(k.Validations, Validation{Rule: vm["rule"].(string), Message: msg})
		}
	}
	d, hasDefault := m["default"]
	return k, d, hasDefault
}

func TestEveryMarkerFound(t *testing.T) {
	// Done when: keyed-list keys and defaults found for every marker the
	// generator emitted (R-62); 02 req 9-10 (defaults table-free), 02 req
	// 17 (list keywords), 02 req 23 and 75 (since), 02 req 64 (impact), 01
	// req 28 (ref, secret, cel positions), 01 req 36 (defaults).
	x := v1(t)
	byFirstPart := map[string][]Field{}
	for _, f := range x.Fields() {
		p := f.Node.parts[0].ptr
		byFirstPart[p] = append(byFirstPart[p], f)
	}
	markers := 0
	rawSchemas(rawDoc(t), func(ptr string, m map[string]any) {
		if !slices.ContainsFunc(slices.Collect(maps.Keys(m)), isMarker) {
			return
		}
		markers++
		want, wantDefault, hasDefault := rawKeywords(t, ptr, m)
		fields := byFirstPart[ptr]
		if len(fields) == 0 {
			t.Errorf("marker at %s: no field resolves to it", ptr)
			return
		}
		for _, f := range fields {
			got := f.Node.Keywords()
			if got.List != want.List || got.ListKey != want.ListKey || got.Ref != want.Ref || got.Since != want.Since ||
				!reflect.DeepEqual(got.CEL, want.CEL) || !reflect.DeepEqual(got.Validations, want.Validations) ||
				(want.Secret && !got.Secret) || got.Impact&want.Impact != want.Impact {
				t.Errorf("%s %s: keywords %+v, want %+v (from %s)", f.Kind, f.Path, got, want, ptr)
			}
			d, ok := f.Node.Default()
			if ok != hasDefault || !jsonEqual(d, wantDefault) {
				t.Errorf("%s %s: default %v %v, want %v %v", f.Kind, f.Path, d, ok, wantDefault, hasDefault)
			}
			if got.List.Keyed() {
				items, ok := f.Node.Items()
				if _, hasKey := items.Property(got.ListKey); !ok || !hasKey {
					t.Errorf("%s %s: %s list key %q is not a member of its items", f.Kind, f.Path, got.List, got.ListKey)
				}
				if !slices.Contains(items.Required(), got.ListKey) {
					t.Logf("%s %s: key %q is not required in its items", f.Kind, f.Path, got.ListKey)
				}
			}
			if got.List != ListNone && !f.Node.Types().Has(TypeArray) {
				t.Errorf("%s %s: x-ruralz-list on a non-array", f.Kind, f.Path)
			}
		}
	})
	if markers == 0 {
		t.Fatal("no markers found in the rendered view")
	}
}

func TestDefaultsTyped(t *testing.T) {
	// 01 req 36 and 37: every default materializes as a defaulted tree node
	// of its position's type; integers at integer positions are integer
	// text. 02 req 9: defaults come only from the schema's default keywords.
	x := v1(t)
	n := 0
	for _, f := range x.Fields() {
		d, ok := f.Node.Default()
		if !ok {
			if f.Node.DefaultNode() != nil {
				t.Errorf("%s %s: DefaultNode without a default", f.Kind, f.Path)
			}
			continue
		}
		n++
		dn := f.Node.DefaultNode()
		if dn == nil || dn.Style != tree.StyleDefaulted || dn.Pos.Known() {
			t.Fatalf("%s %s: DefaultNode = %+v", f.Kind, f.Path, dn)
		}
		if !jsonEqual(dn.JSONValue(), d) {
			t.Errorf("%s %s: DefaultNode %v != Default %v", f.Kind, f.Path, dn.JSONValue(), d)
		}
		types := f.Node.Types()
		var kindType TypeSet
		switch dn.Kind {
		case tree.KindString:
			kindType = TypeString
		case tree.KindInt:
			kindType = TypeInteger
		case tree.KindFloat:
			kindType = TypeNumber
		case tree.KindBool:
			kindType = TypeBoolean
		case tree.KindMap:
			kindType = TypeObject
		case tree.KindList:
			kindType = TypeArray
		default:
			kindType = TypeNull
		}
		if !types.Has(kindType) {
			t.Errorf("%s %s: default %v (%v) does not fit types %s", f.Kind, f.Path, d, dn.Kind, types)
		}
	}
	if n == 0 {
		t.Fatal("no defaults in the rendered view")
	}
}

func TestDefaultsIterator(t *testing.T) {
	// 01 req 36: for every object, each property with a default is listed
	// by Defaults, and no other.
	x := v1(t)
	for _, f := range x.Fields() {
		var want []string
		for name, p := range f.Node.Properties() {
			if _, ok := p.Default(); ok {
				want = append(want, name)
			}
		}
		var got []string
		for name := range f.Node.Defaults() {
			got = append(got, name)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s %s: Defaults() = %v, want %v", f.Kind, f.Path, got, want)
		}
	}
}

func TestLevelIsMaxSince(t *testing.T) {
	// 02 req 75: the served level is the maximum x-ruralz-since (0 in M1).
	want := 0
	rawSchemas(rawDoc(t), func(_ string, m map[string]any) {
		if n, ok := m["x-ruralz-since"].(json.Number); ok {
			i, _ := n.Int64()
			want = max(want, int(i))
		}
	})
	if got := v1(t).Level(); got != want {
		t.Fatalf("Level() = %d, want %d", got, want)
	}
}

func TestPolicyConfigDispatch(t *testing.T) {
	// 02 req 11 and 01 req 25/36: Policy config is resolved through the
	// PolicySpec allOf/if/then dispatch on spec.type, for every type.
	doc := rawDoc(t)
	ps := doc["$defs"].(map[string]any)["PolicySpec"].(map[string]any)
	want := map[string]string{}
	var order []string
	for _, m := range ps["allOf"].([]any) {
		rule := m.(map[string]any)
		ifc, ok := rule["if"].(map[string]any)
		if !ok {
			continue
		}
		typ := ifc["properties"].(map[string]any)["type"].(map[string]any)["const"].(string)
		ref := rule["then"].(map[string]any)["properties"].(map[string]any)["config"].(map[string]any)["$ref"].(string)
		want[typ] = strings.TrimPrefix(ref, "#/$defs/")
		order = append(order, typ)
	}
	spec, _ := v1(t).Spec("Policy")
	if got := spec.DispatchValues("type"); !slices.Equal(got, order) {
		t.Fatalf("DispatchValues(type) = %v, want %v", got, order)
	}
	for typ, def := range want {
		cfg, ok := spec.PolicyConfig(typ)
		if !ok || cfg.Def() != def {
			t.Errorf("PolicyConfig(%s) = %q %v, want %q", typ, cfg.Def(), ok, def)
		}
		if !cfg.Types().Has(TypeObject) {
			t.Errorf("PolicyConfig(%s) types %s", typ, cfg.Types())
		}
		sel := spec.Select(parseTree(t, `{"type":"`+typ+`"}`))
		if c, _ := sel.Property("config"); c != cfg {
			t.Errorf("Select(type=%s).config differs from PolicyConfig", typ)
		}
	}
	if _, ok := spec.PolicyConfig("no.such-type"); ok {
		t.Error("PolicyConfig(no.such-type) found")
	}
	// Without a type the static config schema is open and untyped.
	base, _ := spec.Property("config")
	if !base.Open() || base.Def() != "" {
		t.Errorf("static config: open %v def %q", base.Open(), base.Def())
	}
}

func TestRequirementExamples(t *testing.T) {
	// Normative examples of the specs, resolved by the index from the
	// committed schema.
	x := v1(t)
	weight := mustInfo(t, x, "Route", `{"spec":{"upstreams":[{"name":"a"}]}}`, "spec", "upstreams", 0, "weight")
	if d, ok := weight.Node.Default(); !ok || !jsonEqual(d, json.Number("1")) {
		// 02 req 9: each Route.spec.upstreams[].weight defaults to 1.
		t.Errorf("upstreams[].weight default = %v %v", d, ok)
	}
	if dn := weight.Node.DefaultNode(); dn.Kind != tree.KindInt || dn.Text != "1" {
		t.Errorf("upstreams[].weight DefaultNode = %+v", dn)
	}
	key := mustInfo(t, x, "Policy", `{"spec":{"type":"quota","config":{}}}`, "spec", "config", "key")
	if d, _ := key.Node.Default(); d != "consumer.name" || key.Node.Keywords().CEL == nil {
		// 02 req 11, 01 req 36 and 41: quota config.key "consumer.name" via
		// the type dispatch, and a CEL place.
		t.Errorf("quota config.key = %v %+v", d, key.Node.Keywords())
	}
	if !key.NoSubstitution {
		t.Error("quota config.key allows substitution (01 req 28: CEL)")
	}
	timeout := mustInfo(t, x, "Route", `{}`, "spec", "timeout")
	if timeout.Node.Scalar() != ScalarDuration {
		// 02 req 15: normalization by definition name.
		t.Errorf("Route spec.timeout scalar = %s", timeout.Node.Scalar())
	}
	limit := mustInfo(t, x, "Gateway", `{}`, "spec", "limits", "maxResponseBodyBytes")
	if limit.Node.Scalar() != ScalarByteSize || !limit.Node.Types().Has(TypeInteger|TypeString) {
		// 02 req 15, 01 req 29: ByteSize re-types to an integer or stays a string.
		t.Errorf("maxResponseBodyBytes scalar %s types %s", limit.Node.Scalar(), limit.Node.Types())
	}
	pol := mustInfo(t, x, "Gateway", `{}`, "spec", "policies")
	if lt, key := pol.Node.List(); lt != ListOrderedMap || key != "name" {
		// 02 req 17: spec.policies is an orderedMap keyed by name.
		t.Errorf("Gateway spec.policies list = %s %s", lt, key)
	}
	ref := mustInfo(t, x, "Route", `{"spec":{"policies":[{"name":"p"}]}}`, "spec", "policies", 0, "name")
	if ref.Node.Keywords().Ref != "Policy" || !ref.NoSubstitution {
		// 01 req 28: x-ruralz-ref positions forbid substitution.
		t.Errorf("PolicyRef name: %+v noSubst %v", ref.Node.Keywords(), ref.NoSubstitution)
	}
	secret := mustInfo(t, x, "Gateway", `{}`, "spec", "stateStore", "url", "secretRef", "name")
	if !secret.InSecret || !secret.NoSubstitution {
		// 01 req 28: the whole x-ruralz-secret subtree; 02 req 64 gives
		// every op under it the security impact.
		t.Errorf("stateStore.url.secretRef.name: %+v", secret)
	}
	name := mustInfo(t, x, "Upstream", `{}`, "metadata", "name")
	if !name.NoSubstitution {
		t.Error("metadata.name allows substitution (01 req 28)")
	}
	label := mustInfo(t, x, "Upstream", `{}`, "metadata", "labels", "team")
	if label.NoSubstitution || !label.Node.Types().Has(TypeString) {
		t.Errorf("metadata.labels.team: %+v", label)
	}
	// 02 req 76: typed-map values share one schema path.
	if sp, _ := x.SchemaPath("Upstream", nil, pathOf("metadata", "labels", "team")); sp != "metadata.labels{}" {
		t.Errorf("SchemaPath(labels.team) = %q", sp)
	}
}

func TestUnknownFieldsAndOpenConfigs(t *testing.T) {
	// 02 req 33: a field unknown to the schema is detectable (RZ-CFG-024 in
	// canonical decode, RZ-CFG-006 upstream); 02 req 18: undeclared members
	// of +ruralz:open configs are free, not unknown.
	x := v1(t)
	if _, ok := x.Lookup("Gateway", nil, pathOf("spec", "nosuchfield")); ok {
		t.Error("Gateway spec.nosuchfield resolved")
	}
	if _, ok := x.Lookup("Gateway", nil, pathOf("spec", "listeners", "x")); ok {
		t.Error("a member path into an array resolved")
	}
	if _, ok := x.Lookup("Gateway", nil, pathOf("spec", "admin", 0)); ok {
		t.Error("an index path into an object resolved")
	}
	if _, ok := x.Lookup("Nothing", nil, nil); ok {
		t.Error("unknown kind resolved")
	}
	res := parseTree(t, `{"spec":{"type":"ratelimit","config":{"futureField":{"a":[1]}}}}`)
	info, ok := x.Lookup("Policy", res, pathOf("spec", "config", "futureField", "a", 0))
	if !ok || info.Known() || !info.Free {
		t.Errorf("open config member: %+v %v", info, ok)
	}
	cfg := mustInfo(t, x, "Policy", `{"spec":{"type":"ratelimit"}}`, "spec", "config")
	if !cfg.Node.Open() || cfg.Node.Closed() {
		t.Errorf("ratelimit config open %v closed %v", cfg.Node.Open(), cfg.Node.Closed())
	}
}

func mustInfo(t *testing.T, x *Index, kind, res string, elems ...any) Info {
	t.Helper()
	info, ok := x.Lookup(kind, parseTree(t, res), pathOf(elems...))
	if !ok || !info.Known() {
		t.Fatalf("Lookup(%s %v) = %+v %v", kind, elems, info, ok)
	}
	return info
}

func TestConcurrentUse(t *testing.T) {
	// The index is immutable and safe for concurrent use (run under -race),
	// including dispatch combinations built at run time.
	x := v1(t)
	spec, _ := x.Spec("Policy")
	done := make(chan error)
	for g := range 8 {
		go func() {
			var err error
			for i := range 50 {
				typ := spec.DispatchValues("type")[(g+i)%len(spec.DispatchValues("type"))]
				res := &tree.Node{Kind: tree.KindMap}
				res.Set("spec", tree.Pos{}, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{
					{Key: "type", Value: &tree.Node{Kind: tree.KindString, Text: typ}},
				}})
				if _, ok := x.Lookup("Policy", res, pathOf("spec", "config")); !ok {
					err = errors.New("lookup failed for " + typ)
				}
				_ = x.Walk("Policy", res, func(*Cursor) error { return nil })
			}
			done <- err
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}
