// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// splitSchemaPath splits a schemaidx schema path ("spec.listeners[].tls")
// into member names, "[]" and "{}".
func splitSchemaPath(p string) []string {
	var out []string
	for _, part := range strings.Split(p, ".") {
		for part != "" {
			i := strings.IndexAny(part, "[{")
			switch {
			case i < 0:
				out = append(out, part)
				part = ""
			case i > 0:
				out = append(out, part[:i])
				part = part[i:]
			default:
				out = append(out, part[:2])
				part = part[2:]
			}
		}
	}
	return out
}

// skeleton builds a resource tree holding field f with value leaf, every
// dispatch condition of f.Where set and a key field on every keyed-list
// element on the way (unless the leaf is that key). It returns the tree
// and the leaf's instance path.
func skeleton(t *testing.T, x *schemaidx.Index, f schemaidx.Field, leaf *tree.Node) (*tree.Node, diag.Path) {
	t.Helper()
	root := &tree.Node{Kind: tree.KindMap}
	objects := map[string]*tree.Node{"": root}
	type elem struct {
		list diag.Path
		node *tree.Node
	}
	var elems []elem
	var path diag.Path
	cur, sp := root, ""
	segs := splitSchemaPath(f.Path)
	for i, seg := range segs {
		next := &tree.Node{Kind: tree.KindMap}
		if i == len(segs)-1 {
			next = leaf
		}
		switch seg {
		case "[]":
			cur.Kind, cur.Items = tree.KindList, []*tree.Node{next}
			elems = append(elems, elem{list: slices.Clone(path), node: next})
			path = append(path, diag.Index(0))
			sp += "[]"
		case "{}":
			cur.Kind = tree.KindMap
			cur.Set("k", tree.Pos{}, next)
			path = append(path, diag.Field("k"))
			sp += "{}"
		default:
			cur.Kind = tree.KindMap
			cur.Set(seg, tree.Pos{}, next)
			path = append(path, diag.Field(seg))
			if sp != "" {
				sp += "."
			}
			sp += seg
		}
		objects[sp] = next
		cur = next
	}
	for _, d := range f.Where {
		for _, c := range d.Conditions {
			s, _ := c.Value.(string)
			objects[d.Path].Set(c.Member, tree.Pos{}, &tree.Node{Kind: tree.KindString, Text: s})
		}
	}
	for _, e := range elems {
		info, ok := x.Lookup(f.Kind, root, e.list)
		if !ok {
			t.Fatalf("%s %s: list %s does not resolve", f.Kind, f.Path, e.list)
		}
		if lt, key := info.Node.List(); lt.Keyed() && e.node.Kind == tree.KindMap {
			if _, has := e.node.Get(key); !has {
				e.node.Set(key, tree.Pos{}, &tree.Node{Kind: tree.KindString, Text: "k0"})
			}
		}
	}
	return root, path
}

// conditionMember reports a field that is itself a dispatch member of
// f.Where (spec.type), which the skeleton sets to the dispatch const.
func conditionMember(f schemaidx.Field) bool {
	for _, d := range f.Where {
		for _, c := range d.Conditions {
			p := c.Member
			if d.Path != "" {
				p = d.Path + "." + c.Member
			}
			if p == f.Path {
				return true
			}
		}
	}
	return false
}

// TestPositionsFromSchema checks every string field of every kind and
// dispatch branch, enumerated from the committed schema (R-62), against
// schemaidx.Info, which owns the forbidden-position rules (architecture
// R-3): ${X} is RZ-CFG-011 exactly where Info.NoSubstitution holds and is
// substituted elsewhere (01 req 28), and a credential-shaped literal warns
// exactly outside Info.InSecret (01 J row 013).
func TestPositionsFromSchema(t *testing.T) {
	set := schemas(t)
	x, _ := set.Index(v1)
	s := New(Options{Schemas: set, Variables: Map{"X": "v"}})
	forbidden, allowed, secret := 0, 0, 0
	for f := range x.FieldsSeq() {
		if f.Path == "" || !f.Node.Types().Has(schemaidx.TypeString) || conditionMember(f) {
			continue
		}
		name := f.Kind + " " + f.Path
		id := tree.ID{Kind: v1alpha1.Kind(f.Kind), Name: "x"}

		leaf := &tree.Node{Kind: tree.KindString, Text: "${X}"}
		root, path := skeleton(t, x, f, leaf)
		info, ok := x.Lookup(f.Kind, root, path)
		if !ok {
			t.Fatalf("%s: does not resolve", name)
		}
		ds := s.Apply(&tree.Resource{ID: id, APIVersion: v1, Root: root})
		if info.NoSubstitution {
			forbidden++
			if g := codes(ds); !slices.Equal(g, []string{CodeForbidden}) || leaf.Text != "${X}" {
				t.Errorf("%s: forbidden position gave %q and text %q, want one %s", name, texts(ds), leaf.Text, CodeForbidden)
			}
		} else {
			allowed++
			if len(ds) != 0 || leaf.Text != "v" || !slices.Equal(leaf.Vars, []string{"X"}) {
				t.Errorf("%s: substituted %q (Vars %v) with %q, want \"v\"", name, leaf.Text, leaf.Vars, texts(ds))
			}
		}

		leaf = &tree.Node{Kind: tree.KindString, Text: "AKIAABCDEFGHIJKLMNOP"}
		root, _ = skeleton(t, x, f, leaf)
		ds = s.Apply(&tree.Resource{ID: id, APIVersion: v1, Root: root})
		want := []string{CodeWarning}
		if info.InSecret {
			secret++
			want = nil
		}
		if g := codes(ds); !slices.Equal(g, want) {
			t.Errorf("%s (InSecret %v): credential literal gave %q, want %v", name, info.InSecret, texts(ds), want)
		}
	}
	t.Logf("string fields: %d forbidden, %d allowed, %d in a secret subtree", forbidden, allowed, secret)
	if forbidden == 0 || allowed == 0 || secret == 0 {
		t.Errorf("the schema should hold forbidden (%d), allowed (%d) and secret (%d) string fields", forbidden, allowed, secret)
	}
}
