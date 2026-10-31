// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// refAt is the reference of finder.at: tree.Node.At one element at a
// time, keeping the nearest known position.
func refAt(n *tree.Node, path diag.Path) (tree.Pos, bool) {
	var pos tree.Pos
	cur := n
	for _, e := range path {
		next, ok := cur.At(diag.Path{e})
		if !ok {
			return tree.Pos{}, false
		}
		cur = next
		if cur.Pos.Known() {
			pos = cur.Pos
		}
	}
	return pos, pos.Known()
}

// pathsOf returns paths to every node below n, each list element named
// every way tree.Node.At accepts (index, item value in canonical and in
// authored member order, and key member when it is an object), plus
// variants that name nothing.
func pathsOf(n *tree.Node, prefix diag.Path, out []diag.Path) []diag.Path {
	out = append(out, prefix)
	switch n.Kind {
	case tree.KindMap:
		for _, m := range n.Members {
			out = pathsOf(m.Value, prefix.Append(diag.Field(m.Key)), out)
		}
		out = append(out, prefix.Append(diag.Field("\x00absent")))
	case tree.KindList:
		for i, it := range n.Items {
			out = pathsOf(it, prefix.Append(diag.Index(i)), out)
			for _, e := range itemNames(it) {
				out = append(out, prefix.Append(e))
			}
		}
		out = append(out, prefix.Append(diag.Index(len(n.Items))), prefix.Append(diag.Item("\x00absent")),
			prefix.Append(diag.Keyed("name", "\x00absent")), prefix.Append(diag.Item(json.RawMessage(`{"\u0000":1}`))))
	default:
	}
	return out
}

// itemNames returns the path elements that name list element it by value
// or by key.
func itemNames(it *tree.Node) []diag.PathElem {
	var out []diag.PathElem
	switch it.Kind {
	case tree.KindString:
		out = append(out, diag.Item(it.Text))
	case tree.KindInt, tree.KindFloat:
		out = append(out, diag.Item(json.Number(it.Text)))
		if f, err := strconv.ParseFloat(it.Text, 64); err == nil {
			out = append(out, diag.Item(json.Number(strconv.FormatFloat(f, 'e', -1, 64))))
		}
	case tree.KindBool:
		out = append(out, diag.Item(it.Bool))
	case tree.KindNull:
		out = append(out, diag.Item(json.RawMessage(`null`)))
	case tree.KindMap, tree.KindList:
		out = append(out, diag.Item(json.RawMessage(schemaidx.AppendItemJSON(nil, it))), diag.Item(json.RawMessage(dump(it))))
		for _, m := range it.Members {
			out = append(out, diag.Keyed(m.Key, m.Value.Text))
		}
	default:
	}
	if it.Kind != tree.KindMap && it.Kind != tree.KindList {
		out = append(out, diag.Item(json.RawMessage(dump(it))))
	}
	return out
}

// TestFinderMatchesAt checks finder.at, with which own resolves the paths
// of all converter diagnostics, against tree.Node.At on every path of a
// corpus of documents: the first member, keyed entry or equal element
// wins, numbers compare by value (1, 1.0 and 1e0), raw items by value
// whatever their member order, a raw scalar or null like the scalar, one
// finder serving every path of a document, and paths that name nothing
// fail alike.
func TestFinderMatchesAt(t *testing.T) {
	docs := []string{
		`{"a": 1, "b": {"c": [1, 1.0, "1", true, false, null, {"k": 1}, {"k": 1.0}, [1, 2], [2, 1]]}}`,
		`{"l": [{"name": "x", "v": 1}, {"name": "x", "v": 2}, {"name": 5}, {"other": "y"}, {"name": {"n": 1}}, {"name": ""}]}`,
		`{"s": [-0, 0, 0.0, 1e2, 100, 9007199254740993, 9007199254740992, 1e400, "a", "a", "", {"b": 1, "a": [true]}, {"a": [true], "b": 1}]}`,
		`{"o": {"m00": 0, "m01": 1, "m02": 2, "m03": 3, "m04": 4, "m05": 5, "m06": 6, "m07": 7, "m08": 8, "m09": 9, "m10": 10, "m11": 11, "m12": 12, "m13": 13, "m14": 14, "m15": 15, "m16": 16, "m17": 17}}`,
		`{"n": [[{"x": [{"y": null}]}], [], {}, [[]], [{}]]}`,
		`[]`, `{}`, `"scalar"`,
	}
	for _, src := range docs {
		root := mustParse(t, src)
		f := newWork().newFinder()
		for _, p := range pathsOf(root, nil, nil) {
			wantPos, wantOK := refAt(root, p)
			gotPos, gotOK := f.at(root, p)
			if gotPos != wantPos || gotOK != wantOK {
				t.Errorf("%s: finder.at(%s) = %v %v, want %v %v", src, p, gotPos, gotOK, wantPos, wantOK)
			}
		}
	}
}

// FuzzFinder checks finder.at against tree.Node.At on every path of
// fuzzed JSON documents.
func FuzzFinder(f *testing.F) {
	for _, s := range []string{
		`{"a": [1, 1.0, {"k": 1}, {"k": 1.0}, "x", true, null]}`,
		`{"l": [{"name": "a"}, {"name": "a", "x": 1}, {"name": 1}]}`,
		`[[1, [2]], [1, [2.0]], {"a": {"b": []}}, {"b": {"a": []}}]`,
		`{"s": [-0, 0, 1e400, 1E400, "é", "é"]}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		root, err := parse(src, 0)
		if err != nil || strings.Count(src, "[")+strings.Count(src, "{") > 200 {
			t.Skip()
		}
		finder := newWork().newFinder()
		for _, p := range pathsOf(root, nil, nil) {
			wantPos, wantOK := refAt(root, p)
			if gotPos, gotOK := finder.at(root, p); gotPos != wantPos || gotOK != wantOK {
				t.Fatalf("finder.at(%s) = %v %v, want %v %v", p, gotPos, gotOK, wantPos, wantOK)
			}
		}
	})
}

// ownCase is a resource with n converter diagnostics and, for each, the
// node its path names.
type ownCase struct {
	src   string
	ds    diag.List
	named func(root *tree.Node, i int) *tree.Node
}

// ownScaleCases build a resource with one large object or list of n
// members and n converter diagnostics, one naming each member: the shapes
// where resolving each path from the root would cost a scan of the
// container per diagnostic.
func ownScaleCases() map[string]func(n int) ownCase {
	each := func(n int, f func(i int) (string, diag.Path)) (string, diag.List) {
		parts := make([]string, n)
		ds := make(diag.List, n)
		for i := range n {
			var p diag.Path
			parts[i], p = f(i)
			ds[i] = diag.Diagnostic{Code: CodeUnserved, Severity: diag.SeverityError, Path: p, Message: "converted"}
		}
		return strings.Join(parts, ", "), ds
	}
	thing := func(list string) string {
		return `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r"}, "spec": {"s": [` + list + `]}}`
	}
	item := func(root *tree.Node, i int) *tree.Node {
		spec, _ := root.Get("spec")
		l, _ := spec.Get("s")
		return l.Items[i]
	}
	member := func(name string, of func(*tree.Node, int) *tree.Node) func(*tree.Node, int) *tree.Node {
		return func(root *tree.Node, i int) *tree.Node {
			v, _ := of(root, i).Get(name)
			return v
		}
	}
	return map[string]func(n int) ownCase{
		"keyed entries": func(n int) ownCase {
			l, ds := each(n, func(i int) (string, diag.Path) {
				a := "h" + strconv.Itoa(i) + ":80"
				return `{"address": "` + a + `", "weight": 1}`, diag.Path{diag.Field("spec"), diag.Field("s"), diag.Keyed("address", a), diag.Field("weight")}
			})
			return ownCase{thing(l), ds, member("weight", item)}
		},
		"object members": func(n int) ownCase {
			l, ds := each(n, func(i int) (string, diag.Path) {
				k := "k" + strconv.Itoa(i)
				return `"` + k + `": "v"`, diag.Path{diag.Field("metadata"), diag.Field("labels"), diag.Field(k)}
			})
			src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r", "labels": {` + l + `}}, "spec": {}}`
			return ownCase{src, ds, func(root *tree.Node, i int) *tree.Node {
				meta, _ := root.Get("metadata")
				labels, _ := meta.Get("labels")
				return labels.Members[i].Value
			}}
		},
		"string set": func(n int) ownCase {
			l, ds := each(n, func(i int) (string, diag.Path) {
				v := "T" + strconv.Itoa(i)
				return `"` + v + `"`, diag.Path{diag.Field("spec"), diag.Field("s"), diag.Item(v)}
			})
			return ownCase{thing(l), ds, item}
		},
		"number set": func(n int) ownCase {
			l, ds := each(n, func(i int) (string, diag.Path) {
				v := strconv.Itoa(i)
				return v + ".0", diag.Path{diag.Field("spec"), diag.Field("s"), diag.Item(json.Number(v))}
			})
			return ownCase{thing(l), ds, item}
		},
		"object set": func(n int) ownCase {
			l, ds := each(n, func(i int) (string, diag.Path) {
				v := `{"k":` + strconv.Itoa(i) + `}`
				return v, diag.Path{diag.Field("spec"), diag.Field("s"), diag.Item(json.RawMessage(v)), diag.Field("k")}
			})
			return ownCase{thing(l), ds, member("k", item)}
		},
	}
}

// TestOwnScaleLinear checks that own, completing n converter diagnostics
// that each name another entry of one large container, costs about eight
// times the work units for eight times the entries, and that every
// diagnostic gets the position of the node its path names.
func TestOwnScaleLinear(t *testing.T) {
	s := newStage(t)
	const small, factor = 1000, 8
	for name, build := range ownScaleCases() {
		t.Run(name, func(t *testing.T) {
			cost := func(n int) int {
				c := build(n)
				res := envelope(mustParse(t, c.src))
				w := newWork()
				got := s.own(res, nil, c.ds, w)
				for i, d := range got {
					if want := location(nil, c.named(res.Root, i).Pos); d.Location != want {
						t.Fatalf("diagnostic %d at %s located %+v, want %+v", i, d.Path, d.Location, want)
					}
				}
				return w.total
			}
			a, b := cost(small), cost(small*factor)
			ratio := float64(b) / float64(a)
			t.Logf("work units: %d entries %d, %d entries %d: ratio %.2f", small, a, small*factor, b, ratio)
			if ratio < factor/2 || ratio > 2*factor {
				t.Errorf("work units: %d entries %d, %d entries %d: ratio %.2f, want about %d", small, a, small*factor, b, ratio, factor)
			}
		})
	}
}
