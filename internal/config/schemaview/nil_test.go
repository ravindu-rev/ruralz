// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// nilCases are resources with member values or list elements that tests
// replace by a nil node, at paths of field names and decimal list
// indexes: every place the mapping, the uniqueness and reserved-prefix
// checks or schemaidx read a member value by its kind: a labels or
// annotations object, a keyed-list key, two of them, a key of a resource
// with no other finding, a dispatch member (a Policy's spec.type, the
// kind of a resource whose kind the view does not dispatch), a typed-map
// value, a set element's member, a list element, a secret field and
// metadata itself.
func nilCases() []struct {
	name, src string
	paths     [][]string
	// unknownKind clears the resource's kind, so the root dispatch reads
	// the nil kind member.
	unknownKind bool
} {
	route := func(meta string) string {
		return `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x"` + meta + `}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`
	}
	twoListeners := envelope("Gateway", `{"listeners": [{"name": "a", "port": 1, "protocol": "http"}, {"name": "b", "port": 2, "protocol": "http", "hostnames": ["h", "i"]}]}`)
	type nilCase = struct {
		name, src   string
		paths       [][]string
		unknownKind bool
	}
	return []nilCase{
		{"labels", route(`, "labels": {}`), [][]string{{"metadata", "labels"}}, false},
		{"annotations", route(`, "annotations": {"a": "b"}`), [][]string{{"metadata", "annotations"}}, false},
		{"metadata", route(""), [][]string{{"metadata"}}, false},
		{"label value", route(`, "labels": {"a": "b", "ruralz.io/c": "d"}`), [][]string{{"metadata", "labels", "a"}, {"metadata", "labels", "ruralz.io/c"}}, false},
		{"keyed-list key", twoListeners, [][]string{{"spec", "listeners", "0", "name"}}, false},
		{"two keyed-list keys", twoListeners, [][]string{{"spec", "listeners", "0", "name"}, {"spec", "listeners", "1", "name"}}, false},
		{"keyed-list key of a valid resource", envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}, {"name": "v"}]}`), [][]string{{"spec", "upstreams", "1", "name"}}, false},
		{"dispatch member", policy("cors", `{}`), [][]string{{"spec", "type"}}, false},
		{"kind of an undispatched kind", twoListeners, [][]string{{"kind"}}, true},
		{"set element member", envelope("Policy", `{"type": "auth.mtls", "config": {"caCertificate": {"secretRef": {"provider": "env", "name": "CA"}}, "subjects": [{"subject": "CN=a"}, {"subject": "CN=a"}]}}`), [][]string{{"spec", "config", "subjects", "0", "subject"}, {"spec", "config", "subjects", "1", "subject"}}, false},
		{"list element", twoListeners, [][]string{{"spec", "listeners", "1", "hostnames", "0"}}, false},
		{"list elements of a set", twoListeners, [][]string{{"spec", "listeners", "1", "hostnames", "0"}, {"spec", "listeners", "1", "hostnames", "1"}}, false},
		{"keyed-list entry", twoListeners, [][]string{{"spec", "listeners", "0"}}, false},
		{"secret field", envelope("Consumer", `{"credentials": {"apiKeys": [{"name": "k", "secretRef": {"provider": "env", "name": "K"}}]}}`), [][]string{{"spec", "credentials", "apiKeys", "0", "secretRef"}}, false},
		{"secret member", envelope("Consumer", `{"credentials": {"apiKeys": [{"name": "k", "secretRef": {"provider": "env", "name": "K"}}]}}`), [][]string{{"spec", "credentials", "apiKeys", "0", "secretRef", "name"}}, false},
	}
}

// setAt replaces the member value or list element at path p of root by v.
func setAt(t *testing.T, root *tree.Node, p []string, v *tree.Node) {
	t.Helper()
	n := root
	for i, tok := range p {
		last := i == len(p)-1
		if n.Kind == tree.KindList {
			j, err := strconv.Atoi(tok)
			if err != nil || j >= len(n.Items) {
				t.Fatalf("no element %s", tok)
			}
			if last {
				n.Items[j] = v
				return
			}
			n = n.Items[j]
			continue
		}
		j := -1
		for k := range n.Members {
			if n.Members[k].Key == tok {
				j = k
				break
			}
		}
		if j < 0 {
			t.Fatalf("no member %s", tok)
		}
		if last {
			n.Members[j].Value = v
			return
		}
		n = n.Members[j].Value
	}
}

// TestNilValues checks that a member value or list element that is a nil
// node is validated, mapped and checked exactly as a null node without a
// source position at the same place, which is how the validator reads it:
// no panic, the same findings, and the input tree left as it was, also
// where checkReserved, duplicateKeys and schemaidx read a nil labels
// value, keyed-list key or dispatch member.
func TestNilValues(t *testing.T) {
	v := testView(t)
	for _, tc := range nilCases() {
		t.Run(tc.name, func(t *testing.T) {
			parse := func(value func() *tree.Node) (*tree.Resource, *tree.FileTable) {
				files := &tree.FileTable{}
				res := mustResource(t, files, "r.json", tc.src)
				for _, p := range tc.paths {
					setAt(t, res.Root, p, value())
				}
				if tc.unknownKind {
					res.ID.Kind = ""
				}
				return res, files
			}
			nilRes, nilFiles := parse(func() *tree.Node { return nil })
			nullRes, nullFiles := parse(func() *tree.Node { return &tree.Node{Kind: tree.KindNull} })
			want := texts(v.Validate(nullRes, nullFiles))
			var got string
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("Validate() panicked: %v", r)
					}
				}()
				got = texts(v.Validate(nilRes, nilFiles))
			}()
			if got != want || want == "" {
				t.Errorf("Validate() with nil nodes =\n%s\nwant the findings of null nodes:\n%s", got, want)
			}
			for _, p := range tc.paths {
				var path diag.Path
				for _, tok := range p {
					if i, err := strconv.Atoi(tok); err == nil {
						path = append(path, diag.Index(i))
						continue
					}
					path = append(path, diag.Field(tok))
				}
				if n, ok := nilRes.Root.At(path); ok && n != nil {
					t.Errorf("Validate() replaced the nil node at %s in its input", path)
				}
			}
		})
	}
}

// TestNullFilled checks the copy runSchema validates when a tree holds
// nil nodes: each nil member value and list element becomes a null node
// without a position, only the objects and lists above them are copied,
// every other subtree is shared, the input is left as it was, a tree
// without nil nodes is returned itself, and each node visited is a work
// unit.
func TestNullFilled(t *testing.T) {
	str := func(s string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: s} }
	shared := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "k", Value: str("v")}}}
	inner := &tree.Node{Kind: tree.KindList, Items: []*tree.Node{str("a"), nil}}
	root := &tree.Node{Kind: tree.KindMap, Pos: tree.Pos{Line: 1, Column: 1}, Members: []tree.Member{
		{Key: "shared", Value: shared},
		{Key: "nil", Value: nil},
		{Key: "inner", Value: inner},
	}}
	w, _ := counter()
	got := nullFilled(root, w)
	if got == root || got.Pos != root.Pos || len(got.Members) != 3 {
		t.Fatalf("nullFilled() = %+v, want a copy of the root", got)
	}
	if got.Members[0].Value != shared {
		t.Error("nullFilled() copied a subtree without nil nodes")
	}
	if n := got.Members[1].Value; n == nil || n.Kind != tree.KindNull || n.Pos.Known() {
		t.Errorf("nullFilled() member = %+v, want a null node without a position", n)
	}
	if l := got.Members[2].Value; l == inner || l.Items[0] != inner.Items[0] || l.Items[1] == nil || l.Items[1].Kind != tree.KindNull {
		t.Errorf("nullFilled() list = %+v, want a copy with a null element", l)
	}
	if root.Members[1].Value != nil || inner.Items[1] != nil {
		t.Error("nullFilled() changed its input")
	}
	// The root, the shared object and its value, and the inner list and
	// its string are visited; the nil nodes are not.
	if want := 5; w.total != want {
		t.Errorf("nullFilled() charged %d units, want %d", w.total, want)
	}
	if again := nullFilled(shared, nil); again != shared {
		t.Error("nullFilled() copied a tree without nil nodes")
	}
}
