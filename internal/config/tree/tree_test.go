// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tree

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for architecture section 2.5 (WP-01): At over keyed, indexed and
// set-item paths of every diag.Item form (R-4, spec 01 req 48), Clone
// independence, JSONValue of every kind and FileTable.Location.

func str(s string) *Node         { return &Node{Kind: KindString, Text: s} }
func num(k Kind, s string) *Node { return &Node{Kind: k, Text: s} }
func boolean(b bool) *Node       { return &Node{Kind: KindBool, Bool: b} }
func list(items ...*Node) *Node  { return &Node{Kind: KindList, Items: items} }

func obj(kv ...any) *Node {
	n := &Node{Kind: KindMap}
	for i := 0; i+1 < len(kv); i += 2 {
		n.Members = append(n.Members, Member{Key: kv[i].(string), Value: kv[i+1].(*Node)})
	}
	return n
}

// sample is a Route-like envelope with map, set and atomic lists.
func sample() *Node {
	return obj(
		"kind", str("Route"),
		"spec", obj(
			"hosts", list(str("a.example"), str("b.example")),
			"methods", list(str("GET"), str("POST")),
			"ports", list(num(KindInt, "80"), num(KindFloat, "4.5e2"), num(KindInt, "8080")),
			"flags", list(boolean(false), boolean(true)),
			"steps", list(
				obj("name", str("stock"), "upstream", str("inventory")),
				obj("name", str("price"), "upstream", str("pricing")),
			),
			"matchers", list(
				obj("header", str("x-a"), "values", list(str("1"), num(KindInt, "2"))),
				obj("header", str("x-b"), "exact", boolean(true), "none", &Node{Kind: KindNull}),
			),
			"nested", list(list(str("a"), str("b")), list()),
		),
	)
}

func TestAt(t *testing.T) {
	root := sample()
	spec := diag.Path{diag.Field("spec")}
	tests := []struct {
		name string
		path diag.Path
		want string // Text of the node found; "" for a miss
		kind Kind
	}{
		{"root", nil, "", KindMap},
		{"field", diag.Path{diag.Field("kind")}, "Route", KindString},
		{"index", spec.Append(diag.Field("hosts"), diag.Index(1)), "b.example", KindString},
		{"keyed", spec.Append(diag.Field("steps"), diag.Keyed("name", "price"), diag.Field("upstream")), "pricing", KindString},
		{"item string", spec.Append(diag.Field("methods"), diag.Item("POST")), "POST", KindString},
		{"item number exact text", spec.Append(diag.Field("ports"), diag.Item(json.Number("8080"))), "8080", KindInt},
		{"item number by value", spec.Append(diag.Field("ports"), diag.Item(json.Number("80.0"))), "80", KindInt},
		{"item float by value", spec.Append(diag.Field("ports"), diag.Item(json.Number("450"))), "4.5e2", KindFloat},
		{"item bool", spec.Append(diag.Field("flags"), diag.Item(true)), "", KindBool},
		{"item object", spec.Append(diag.Field("matchers"), diag.Item(json.RawMessage(`{"values":["1",2],"header":"x-a"}`)), diag.Field("header")), "x-a", KindString},
		{"item object with null", spec.Append(diag.Field("matchers"), diag.Item(json.RawMessage(`{"none":null,"exact":true,"header":"x-b"}`)), diag.Field("header")), "x-b", KindString},
		{"item array", spec.Append(diag.Field("nested"), diag.Item(json.RawMessage(`["a","b"]`)), diag.Index(0)), "a", KindString},
		{"item empty array", spec.Append(diag.Field("nested"), diag.Item(json.RawMessage(`[]`))), "", KindList},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := root.At(tt.path)
			if !ok || got == nil {
				t.Fatalf("At(%s) missed", tt.path)
			}
			if got.Kind != tt.kind || got.Text != tt.want {
				t.Fatalf("At(%s) = kind %d text %q; want kind %d text %q", tt.path, got.Kind, got.Text, tt.kind, tt.want)
			}
		})
	}
	if n, _ := root.At(spec.Append(diag.Field("flags"), diag.Item(false))); n == nil || n.Bool {
		t.Fatal("Item(false) did not select the false element")
	}
}

func TestAtMisses(t *testing.T) {
	root := sample()
	spec := diag.Path{diag.Field("spec")}
	misses := []struct {
		name string
		path diag.Path
	}{
		{"unknown field", diag.Path{diag.Field("status")}},
		{"field of a scalar", diag.Path{diag.Field("kind"), diag.Field("x")}},
		{"field of a list", spec.Append(diag.Field("hosts"), diag.Field("x"))},
		{"index out of range", spec.Append(diag.Field("hosts"), diag.Index(2))},
		{"negative index", spec.Append(diag.Field("hosts"), diag.Index(-1))},
		{"index of a map", spec.Append(diag.Index(0))},
		{"keyed of a map", spec.Append(diag.Keyed("name", "stock"))},
		{"keyed unknown key", spec.Append(diag.Field("steps"), diag.Keyed("name", "cart"))},
		{"keyed wrong key field", spec.Append(diag.Field("steps"), diag.Keyed("id", "stock"))},
		{"keyed over scalars", spec.Append(diag.Field("hosts"), diag.Keyed("name", "a.example"))},
		{"item of a map", spec.Append(diag.Item("GET"))},
		{"item absent", spec.Append(diag.Field("methods"), diag.Item("PUT"))},
		{"item string is not a number", spec.Append(diag.Field("ports"), diag.Item("80"))},
		{"item number is not a string", spec.Append(diag.Field("matchers"), diag.Item(json.Number("1")))},
		{"item number mismatch", spec.Append(diag.Field("ports"), diag.Item(json.Number("81")))},
		{"item bad number", spec.Append(diag.Field("ports"), diag.Item(json.Number("x")))},
		{"item bool is not a string", spec.Append(diag.Field("methods"), diag.Item(true))},
		{"item object with a missing member", spec.Append(diag.Field("matchers"), diag.Item(json.RawMessage(`{"header":"x-a"}`)))},
		{"item object with an extra member", spec.Append(diag.Field("matchers"), diag.Item(json.RawMessage(`{"header":"x-b","exact":true,"none":null,"x":1}`)))},
		{"item object with a different value", spec.Append(diag.Field("matchers"), diag.Item(json.RawMessage(`{"header":"x-b","exact":false,"none":null}`)))},
		{"item object null mismatch", spec.Append(diag.Field("matchers"), diag.Item(json.RawMessage(`{"header":"x-b","exact":true,"none":1}`)))},
		{"item object against a scalar", spec.Append(diag.Field("methods"), diag.Item(json.RawMessage(`{"a":1}`)))},
		{"item array length", spec.Append(diag.Field("nested"), diag.Item(json.RawMessage(`["a"]`)))},
		{"item array element", spec.Append(diag.Field("nested"), diag.Item(json.RawMessage(`["a","c"]`)))},
		{"item array against a scalar", spec.Append(diag.Field("methods"), diag.Item(json.RawMessage(`["GET"]`)))},
		{"item invalid raw JSON", spec.Append(diag.Field("methods"), diag.Item(json.RawMessage(`{`)))},
		{"item unsupported Go type", spec.Append(diag.Field("ports"), diag.Item(80))},
		{"after a miss", spec.Append(diag.Field("nope"), diag.Index(0))},
	}
	for _, tt := range misses {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := root.At(tt.path); ok || got != nil {
				t.Fatalf("At(%s) = %+v, want a miss", tt.path, got)
			}
		})
	}
	var nilNode *Node
	if _, ok := nilNode.At(nil); ok {
		t.Fatal("nil.At(nil) succeeded")
	}
	for _, p := range []diag.Path{{diag.Field("a")}, {diag.Index(0)}, {diag.Keyed("k", "v")}, {diag.Item("x")}} {
		if _, ok := nilNode.At(p); ok {
			t.Fatalf("nil.At(%s) succeeded", p)
		}
	}
}

func TestGetSetDelete(t *testing.T) {
	n := obj("a", str("1"))
	if v, ok := n.Get("a"); !ok || v.Text != "1" {
		t.Fatal("Get(a) failed")
	}
	if _, ok := str("x").Get("a"); ok {
		t.Fatal("Get on a scalar succeeded")
	}
	var nilNode *Node
	if _, ok := nilNode.Get("a"); ok {
		t.Fatal("Get on nil succeeded")
	}
	n.Set("b", Pos{Line: 2, Column: 1}, str("2"))
	n.Set("a", Pos{Line: 9}, str("one"))
	if len(n.Members) != 2 || n.Members[0].Key != "a" || n.Members[0].Value.Text != "one" || n.Members[1].Key != "b" {
		t.Fatalf("Set did not keep authored order: %+v", n.Members)
	}
	if n.Members[0].KeyPos.Line != 0 || n.Members[1].KeyPos.Line != 2 {
		t.Fatal("Set replaced an existing key position or dropped a new one")
	}
	if !n.Delete("a") || n.Delete("a") || len(n.Members) != 1 {
		t.Fatalf("Delete: %+v", n.Members)
	}
}

func TestClone(t *testing.T) {
	orig := sample()
	orig.Vars = []string{"HOST"}
	c := orig.Clone()
	if !reflect.DeepEqual(c, orig) {
		t.Fatal("Clone differs from its source")
	}
	// Mutating the clone at every level leaves the source unchanged.
	c.Vars[0] = "CHANGED"
	c.Set("kind", Pos{}, str("Upstream"))
	spec, _ := c.Get("spec")
	spec.Members[0].Value.Items[0].Text = "changed.example"
	spec.Members[0].Value.Items = append(spec.Members[0].Value.Items, str("c.example"))
	steps, _ := spec.Get("steps")
	steps.Items[0].Set("upstream", Pos{}, str("changed"))
	spec.Delete("ports")

	want := sample()
	want.Vars = []string{"HOST"}
	if !reflect.DeepEqual(orig, want) {
		t.Fatal("mutating the clone changed the source")
	}
	var nilNode *Node
	if nilNode.Clone() != nil {
		t.Fatal("nil.Clone() != nil")
	}
	leaf := str("x")
	if lc := leaf.Clone(); lc.Members != nil || lc.Items != nil || lc == leaf {
		t.Fatal("Clone of a scalar invented children or aliased")
	}
}

func TestJSONValue(t *testing.T) {
	tests := []struct {
		name string
		n    *Node
		want any
	}{
		{"nil", nil, nil},
		{"null", &Node{Kind: KindNull}, nil},
		{"bool", boolean(true), true},
		{"int", num(KindInt, "-12"), json.Number("-12")},
		{"float keeps its text", num(KindFloat, "1.50e3"), json.Number("1.50e3")},
		{"string", str("x"), "x"},
		{"empty map", &Node{Kind: KindMap}, map[string]any{}},
		{"empty list", &Node{Kind: KindList}, []any{}},
		{
			"nested", obj("a", list(num(KindInt, "1"), &Node{Kind: KindNull}, obj("b", boolean(false)))),
			map[string]any{"a": []any{json.Number("1"), nil, map[string]any{"b": false}}},
		},
		{"unknown kind", &Node{Kind: Kind(99)}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.n.JSONValue(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("JSONValue = %#v, want %#v", got, tt.want)
			}
		})
	}
	// The value encodes like the tree, numbers byte-exact.
	b, err := json.Marshal(obj("n", num(KindFloat, "1.50e3")).JSONValue())
	if err != nil || string(b) != `{"n":1.50e3}` {
		t.Fatalf("encoded %s, %v", b, err)
	}
}

func TestFileTable(t *testing.T) {
	var ft FileTable
	a := ft.Add(File{Path: "ruralz.yaml", Role: RoleBase})
	b := ft.Add(File{Path: "overlays/prod/routes.yaml", Role: RoleOverlay})
	if a != 0 || b != 1 {
		t.Fatalf("IDs %d, %d; want 0, 1", a, b)
	}
	if f := ft.File(b); f.Path != "overlays/prod/routes.yaml" || f.Role != RoleOverlay {
		t.Fatalf("File(%d) = %+v", b, f)
	}
	if f := ft.File(7); f != (File{}) {
		t.Fatalf("File(unknown) = %+v", f)
	}
	tests := []struct {
		pos  Pos
		want diag.Location
	}{
		{Pos{File: b, Line: 3, Column: 7}, diag.Location{File: "overlays/prod/routes.yaml", Line: 3, Column: 7}},
		{Pos{File: a, Line: 1}, diag.Location{File: "ruralz.yaml", Line: 1}},
		{Pos{}, diag.Location{}},
		{Pos{File: b, Column: 4}, diag.Location{}}, // no line: unknown
		{Pos{File: 9, Line: 1, Column: 1}, diag.Location{Line: 1, Column: 1}},
	}
	for _, tt := range tests {
		if got := ft.Location(tt.pos); got != tt.want {
			t.Errorf("Location(%+v) = %+v, want %+v", tt.pos, got, tt.want)
		}
	}
	if (Pos{}).Known() || !(Pos{Line: 1}).Known() {
		t.Fatal("Pos.Known is wrong")
	}
}

func TestFileTableConcurrent(t *testing.T) {
	var ft FileTable
	var wg sync.WaitGroup
	ids := make([][]FileID, 8)
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				id := ft.Add(File{Path: fmt.Sprintf("w%d/%d.yaml", w, i), Role: RoleBase})
				ids[w] = append(ids[w], id)
				_ = ft.File(id)
			}
		}()
	}
	wg.Wait()
	seen := map[FileID]bool{}
	for w := range ids {
		for i, id := range ids[w] {
			if seen[id] {
				t.Fatalf("ID %d handed out twice", id)
			}
			seen[id] = true
			if got := ft.File(id).Path; got != fmt.Sprintf("w%d/%d.yaml", w, i) {
				t.Fatalf("File(%d) = %q", id, got)
			}
		}
	}
}

func TestID(t *testing.T) {
	id := ID{Kind: v1alpha1.KindRoute, Name: "checkout"}
	if id.String() != "Route/checkout" {
		t.Fatalf("String = %q", id.String())
	}
	if r := id.ResourceID(); *r != (diag.ResourceID{Kind: "Route", Name: "checkout"}) {
		t.Fatalf("ResourceID = %+v", r)
	}
}
