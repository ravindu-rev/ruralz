// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package convert

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// fieldDoc builds a resource envelope of f.Kind in which the position
// f.Path holds a generated value, with every dispatch condition on the way
// satisfied. ok is false for the resource root.
func fieldDoc(f schemaidx.Field) (*tree.Node, bool) {
	if f.Path == "" {
		return nil, false
	}
	root := &tree.Node{Kind: tree.KindMap}
	root.Set("apiVersion", tree.Pos{}, str(HubAPIVersion))
	root.Set("kind", tree.Pos{}, str(f.Kind))
	root.Set("metadata", tree.Pos{}, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "name", Value: str("x")}}})
	for _, d := range f.Where {
		obj := ensure(root, d.Path)
		for _, c := range d.Conditions {
			if s, ok := c.Value.(string); ok && obj.Kind == tree.KindMap {
				obj.Set(c.Member, tree.Pos{}, str(s))
			}
		}
	}
	parent, last := splitLast(f.Path)
	holder := ensure(root, parent)
	leaf := generate(f.Node)
	switch last {
	case "[]":
		holder.Kind = tree.KindList
		holder.Members = nil
		holder.Items = append(holder.Items, leaf)
	case "{}":
		holder.Set("k", tree.Pos{}, leaf)
	default:
		if holder.Kind != tree.KindMap {
			return nil, false
		}
		holder.Set(last, tree.Pos{}, leaf)
	}
	return root, true
}

func str(s string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: s} }

// segments splits a schema path into members, "[]" and "{}".
func segments(path string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(path); i++ {
		switch {
		case path[i] == '.':
			flush()
		case strings.HasPrefix(path[i:], "[]"), strings.HasPrefix(path[i:], "{}"):
			flush()
			out = append(out, path[i:i+2])
			i++
		default:
			cur.WriteByte(path[i])
		}
	}
	flush()
	return out
}

func splitLast(path string) (string, string) {
	segs := segments(path)
	last := segs[len(segs)-1]
	prefix := strings.TrimSuffix(path, last)
	return strings.TrimSuffix(prefix, "."), last
}

// ensure returns the node at the schema path, creating maps (and lists
// for "[]") on the way.
func ensure(root *tree.Node, path string) *tree.Node {
	cur := root
	for _, s := range segments(path) {
		switch s {
		case "[]":
			if cur.Kind != tree.KindList {
				cur.Kind, cur.Members = tree.KindList, nil
			}
			if len(cur.Items) == 0 {
				cur.Items = append(cur.Items, &tree.Node{Kind: tree.KindMap})
			}
			cur = cur.Items[0]
		case "{}":
			next, ok := cur.Get("k")
			if !ok {
				next = &tree.Node{Kind: tree.KindMap}
				cur.Set("k", tree.Pos{}, next)
			}
			cur = next
		default:
			next, ok := cur.Get(s)
			if !ok {
				next = &tree.Node{Kind: tree.KindMap}
				cur.Set(s, tree.Pos{}, next)
			}
			cur = next
		}
	}
	return cur
}

// generate returns a value the schema node admits, or a plausible one.
func generate(n *schemaidx.Node) *tree.Node {
	if e := n.Enum(); len(e) > 0 {
		return str(e[0])
	}
	switch n.Scalar() {
	case schemaidx.ScalarDuration:
		return str("90s")
	case schemaidx.ScalarByteSize:
		return str("10Mi")
	case schemaidx.ScalarDecimal:
		return str("1.50")
	case schemaidx.ScalarIntOrString:
		return &tree.Node{Kind: tree.KindInt, Text: "8080"}
	default:
	}
	t := n.Types()
	switch {
	case t.Has(schemaidx.TypeString):
		return str("v")
	case t.Has(schemaidx.TypeNumber):
		return &tree.Node{Kind: tree.KindFloat, Text: "1.5"}
	case t.Has(schemaidx.TypeInteger):
		return &tree.Node{Kind: tree.KindInt, Text: "7"}
	case t.Has(schemaidx.TypeBoolean):
		return &tree.Node{Kind: tree.KindBool, Bool: true}
	case t.Has(schemaidx.TypeArray):
		return &tree.Node{Kind: tree.KindList, Items: []*tree.Node{str("e")}}
	default:
		return &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "m", Value: str("v")}}}
	}
}

// roundTrip converts doc from the hub to each served apiVersion and back
// and reports the first version whose round trip changed the tree.
func roundTrip(r *Registry, kind string, doc *tree.Node) error {
	for _, v := range r.Served() {
		down, ds := r.Convert(HubAPIVersion, v, kind, doc.Clone(), false)
		if ds.HasErrors() {
			return fmt.Errorf("to %s: %v", v, ds)
		}
		up, ds := r.Convert(v, HubAPIVersion, kind, down, false)
		if ds.HasErrors() {
			return fmt.Errorf("from %s: %v", v, ds)
		}
		if !equivalent(up, doc) {
			return fmt.Errorf("through %s: got %s, want %s", v, dump(up), dump(doc))
		}
	}
	return nil
}

// TestRoundTripEveryField is the 02 test plan item 21 round-trip property:
// for every property path enumerated from schemaidx, a generated value
// survives the conversion to each served apiVersion and back (02 req 8),
// here with the M1 registry and with a registry that adds a renaming spoke
// keeping a field in the conversion-data annotation (02 req 7).
func TestRoundTripEveryField(t *testing.T) {
	idx := hubIndex(t)
	beta, err := NewRegistry(Lifecycle{}, V1alpha1(), betaConverter{})
	if err != nil {
		t.Fatal(err)
	}
	registries := map[string]*Registry{"default": Default(), "with v1test": beta}
	count := 0
	for f := range idx.FieldsSeq() {
		doc, ok := fieldDoc(f)
		if !ok {
			continue
		}
		count++
		for name, r := range registries {
			if err := roundTrip(r, f.Kind, doc); err != nil {
				t.Errorf("%s %s (%s): %v", f.Kind, f.Path, name, err)
			}
		}
	}
	if count < 500 {
		t.Errorf("enumerated %d fields; the v1alpha1 schema has many more", count)
	}
}

// TestFaultyConverterDetected checks that the round-trip property catches
// a converter that loses a field.
func TestFaultyConverterDetected(t *testing.T) {
	r, err := NewRegistry(Lifecycle{}, V1alpha1(), lossyConverter{identity{version: vTest}})
	if err != nil {
		t.Fatal(err)
	}
	doc := mustParse(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "a"}, "spec": {"timeout": "5s"}}`)
	if err := roundTrip(r, "Route", doc); err == nil {
		t.Error("round trip through a lossy converter passed")
	}
}

// lossyConverter drops spec.timeout on the way down.
type lossyConverter struct{ identity }

func (lossyConverter) FromHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	if spec, ok := doc.Get("spec"); ok {
		spec.Delete("timeout")
	}
	return doc, nil
}

// FuzzConversionRoundTrip is 02 property 29 at the registry level: any
// JSON document, whole or partial, converts to every served apiVersion and
// back unchanged, and converting to the same apiVersion is the identity
// apart from the stripped conversion-data annotation.
func FuzzConversionRoundTrip(f *testing.F) {
	seeds := []string{
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "a"}, "spec": {"upstreams": [{"name": "u", "weight": 2}], "timeout": "5s"}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "edge"}, "spec": {"listeners": [{"name": "h", "$patch": "delete"}], "admin": {"port": "${P:-9901}"}}}`,
		`{"spec": {"config": {"schema": {"type": "object", "required": ["b", "a"], "enum": [3, 1, 2]}}}}`,
		`[1, 2, {"a": null}]`,
		`"scalar"`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	beta, err := NewRegistry(Lifecycle{}, V1alpha1(), betaConverter{})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, src string) {
		doc, err := parse(src, 0)
		if err != nil {
			t.Skip()
		}
		kind := "Route"
		if k, ok := doc.Get("kind"); ok && k.Kind == tree.KindString {
			kind = k.Text
		}
		StripConversionData(doc)
		if _, ok := doc.Get("apiVersion"); ok {
			// The registry rewrites apiVersion; start from the hub's.
			setAPIVersion(doc, HubAPIVersion)
		}
		// The renaming test spoke changes names a document may already
		// use; it round-trips only documents without them.
		registries := []*Registry{Default()}
		if !usesBetaNames(doc) {
			registries = append(registries, beta)
		}
		for _, r := range registries {
			if err := roundTrip(r, kind, doc); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// usesBetaNames reports a document the renaming test spoke cannot
// round-trip: a Route spec that already uses its renamed member, a
// timeout that is not a string, or metadata or annotations that are not
// objects.
func usesBetaNames(doc *tree.Node) bool {
	spec, ok := doc.Get("spec")
	if !ok || spec.Kind != tree.KindMap {
		return false
	}
	if _, ok := spec.Get("backends"); ok {
		return true
	}
	timeout, ok := spec.Get("timeout")
	if !ok {
		return false
	}
	if timeout.Kind != tree.KindString || strings.Contains(timeout.Text, ";") {
		return true
	}
	meta, ok := doc.Get("metadata")
	if !ok {
		return false
	}
	if meta.Kind != tree.KindMap {
		return true
	}
	ann, ok := meta.Get("annotations")
	return ok && ann.Kind != tree.KindMap
}
