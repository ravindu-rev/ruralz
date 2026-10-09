// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
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

// skeleton builds a resource tree holding the field f with value leaf:
// every dispatch condition of f.Where is set, and every element of a keyed
// list on the way carries its key field, so an overlay built the same way
// matches it.
func skeleton(t *testing.T, x *schemaidx.Index, f schemaidx.Field, leaf *tree.Node) *tree.Node {
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
			elems = append(elems, elem{list: append(diag.Path(nil), path...), node: next})
			path = append(path, diag.Index(0))
			sp += "[]"
		case "{}":
			cur.Set("k", tree.Pos{}, next)
			path = append(path, diag.Field("k"))
			sp += "{}"
		default:
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
		obj := objects[d.Path]
		for _, c := range d.Conditions {
			s, ok := c.Value.(string)
			if !ok {
				t.Fatalf("%s %s: non-string dispatch const %v", f.Kind, f.Path, c.Value)
			}
			obj.Set(c.Member, tree.Pos{}, str(s))
		}
	}
	for _, e := range elems {
		info, ok := x.Lookup(f.Kind, root, e.list)
		if !ok {
			t.Fatalf("%s %s: list %s does not resolve", f.Kind, f.Path, e.list)
		}
		if lt, key := info.Node.List(); lt.Keyed() && e.node.Kind == tree.KindMap {
			e.node.Set(key, tree.Pos{}, str("k0"))
		}
	}
	return root
}

func str(s string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: s} }

// entry returns an object of the given key and value pairs.
func entry(kv ...string) *tree.Node {
	n := &tree.Node{Kind: tree.KindMap}
	for i := 0; i+1 < len(kv); i += 2 {
		n.Set(kv[i], tree.Pos{}, str(kv[i+1]))
	}
	return n
}

func list(items ...*tree.Node) *tree.Node { return &tree.Node{Kind: tree.KindList, Items: items} }

// follow returns the node at schema path p of an instance built by
// skeleton.
func follow(n *tree.Node, p string) *tree.Node {
	cur := n
	for _, seg := range splitSchemaPath(p) {
		switch {
		case cur == nil:
			return nil
		case seg == "[]":
			if cur.Kind != tree.KindList || len(cur.Items) == 0 {
				return nil
			}
			cur = cur.Items[0]
		case seg == "{}":
			cur, _ = cur.Get("k")
		default:
			cur, _ = cur.Get(seg)
		}
	}
	return cur
}

// TestListTypesFromSchema merges every array field of every kind and
// dispatch branch, enumerated from the committed schema rather than a
// hard-coded list (R-62), by its x-ruralz-list type (01 req 25): map and
// orderedMap lists merge by key with $patch: delete, orderedMap lists also
// reorder with {$patch: replace} (a map list rejects it), and set, atomic
// and unmarked lists are replaced.
func TestListTypesFromSchema(t *testing.T) {
	set := schemas(t)
	x, _ := set.Index(v1)
	counts := map[schemaidx.ListType]int{}
	for f := range x.FieldsSeq() {
		if f.Path == "" || !f.Node.Types().Has(schemaidx.TypeArray) {
			continue
		}
		lt, key := f.Node.List()
		counts[lt]++
		name := f.Kind + " " + f.Path
		apply := func(base, over *tree.Node) (*tree.Node, diag.List) {
			id := tree.ID{Kind: v1alpha1.Kind(f.Kind), Name: "x"}
			b := &tree.Resource{ID: id, APIVersion: v1, Root: skeleton(t, x, f, base)}
			o := &tree.Resource{ID: id, APIVersion: v1, Root: skeleton(t, x, f, over)}
			out, ds := pipeline(t, []*tree.Resource{b}, []*tree.Resource{o}, Options{Schemas: set})
			if len(out) != 1 {
				t.Fatalf("%s: %d resources", name, len(out))
			}
			return follow(out[0].Root, f.Path), ds
		}
		if !lt.Keyed() {
			got, ds := apply(list(str("a"), str("b")), list(str("c")))
			if len(ds) != 0 || dump(got) != `["c"]` {
				t.Errorf("%s (%v list): merged %s with %q, want the overlay [\"c\"]", name, lt, dump(got), texts(ds))
			}
			continue
		}
		got, ds := apply(
			list(entry(key, "a", "x", "1"), entry(key, "b")),
			list(entry(key, "b", "y", "2"), entry(key, "c"), entry(key, "a", PatchKey, PatchDelete)),
		)
		want := `[{"` + key + `":"b","y":"2"},{"` + key + `":"c"}]`
		if len(ds) != 0 || dump(got) != want {
			t.Errorf("%s (%v by %s): merged %s with %q, want %s", name, lt, key, dump(got), texts(ds), want)
		}
		replace := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: PatchKey, Value: str(PatchReplace)}}}
		got, ds = apply(
			list(entry(key, "a", "x", "1"), entry(key, "b")),
			list(replace, entry(key, "b"), entry(key, "a")),
		)
		if lt == schemaidx.ListOrderedMap {
			want = `[{"` + key + `":"b"},{"` + key + `":"a"}]`
			if len(ds) != 0 || dump(got) != want {
				t.Errorf("%s: reordered %s with %q, want %s", name, dump(got), texts(ds), want)
			}
		} else if c := codes(ds); len(c) != 1 || c[0] != CodeSchema {
			t.Errorf("%s: {$patch: replace} on a map list gave %q, want one %s", name, texts(ds), CodeSchema)
		}
	}
	t.Logf("array fields by list type: %v", counts)
	for _, lt := range []schemaidx.ListType{schemaidx.ListMap, schemaidx.ListOrderedMap, schemaidx.ListSet, schemaidx.ListAtomic} {
		if counts[lt] == 0 {
			t.Errorf("no %v list field in the schema", lt)
		}
	}
}
