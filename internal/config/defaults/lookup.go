// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// The lookups of stage G whose cost grows with the value they look at go
// through the methods of this file, which count what each scans as work
// units: tree.Node.Get scans an object's members, schemaidx.Node.Select
// evaluates dispatch rules that look members up, schemaidx.ItemElem looks
// up a keyed entry's key member or encodes a set element,
// schemaidx.AppendItemJSON encodes a value, and the finder indexes the
// containers a path steps into (tree.Node.At). A stage that called one of
// them once per member of a large object would be quadratic in its size;
// counted here, that shows in work.total, which TestScaleLinear and
// TestScaleLinearErrors compare across sizes, and TestLookupsCounted
// keeps every use of the package (calls, method and function values, by
// the type selected from) on these methods. Each method takes a nil w, as
// the test helpers pass.

// get returns member key of object n, as tree.Node.Get does, counting n's
// members and, for each member whose name is as long as key, the bytes of
// key, which a string comparison of equal lengths may read.
func (w *work) get(n *tree.Node, key string) (*tree.Node, bool) {
	if n == nil || n.Kind != tree.KindMap {
		return nil, false
	}
	units := len(n.Members)
	for i := range n.Members {
		if len(n.Members[i].Key) == len(key) {
			units += len(key)
		}
	}
	w.charge(units)
	return n.Get(key)
}

// selectSchema returns s with the dispatch rules for instance n applied,
// as schemaidx.Node.Select does, counting n's members, which the rules
// look up. A nil member value is a null to the rules (nullMembers).
func (w *work) selectSchema(s *schemaidx.Node, n *tree.Node) *schemaidx.Node {
	if n != nil {
		w.charge(len(n.Members))
	}
	return s.Select(nullMembers(n))
}

// itemElem returns the key-aware path element of element i of a list
// whose schema is list, as schemaidx.ItemElem does, counting the item's
// members (a keyed entry's key lookup) and the bytes of a set element's
// canonical JSON. A nil key member is a null, which names no entry
// (nullMembers).
func (w *work) itemElem(list *schemaidx.Node, item *tree.Node, i int) diag.PathElem {
	e := schemaidx.ItemElem(list, nullMembers(item), i)
	if item != nil {
		w.charge(len(item.Members))
	}
	if raw, ok := e.Item.(json.RawMessage); ok {
		w.charge(len(raw))
	}
	return e
}

// nullMembers returns n, or, when a member value of object n is nil, a
// shallow copy of n in which each such value is a null node: the
// dispatch rules of schemaidx.Node.Select and the key lookup of
// schemaidx.ItemElem read member values and take a nil one for a node.
// Its scan of the members is part of what the callers count.
func nullMembers(n *tree.Node) *tree.Node {
	if n == nil || n.Kind != tree.KindMap {
		return n
	}
	var out *tree.Node
	for i, m := range n.Members {
		if m.Value != nil {
			continue
		}
		if out == nil {
			c := *n
			c.Members = slices.Clone(n.Members)
			out = &c
		}
		out.Members[i].Value = &tree.Node{Kind: tree.KindNull, Pos: m.KeyPos}
	}
	if out == nil {
		return n
	}
	return out
}

// itemJSON returns the RFC 8785 encoding of n, as
// schemaidx.AppendItemJSON(nil, n) does, counting the bytes written,
// which bound the nodes encoded.
func (w *work) itemJSON(n *tree.Node) []byte {
	b := schemaidx.AppendItemJSON(nil, n)
	w.charge(len(b))
	return b
}

// finder resolves the paths of many diagnostics below one root, as
// tree.Node.At does for one (own: the diagnostics a converter returned),
// at a cost linear in the tree and the paths together. A step into an
// object of more than smallObject members, a keyed list (per key member)
// or a set looks the element up in an index of that container, built on
// first use and counted then; a later step into the same container costs
// one map lookup, however many diagnostics name its entries. Resolving
// each path from the root instead would scan the container again per
// diagnostic: quadratic for one diagnostic per entry of a large list.
//
// A step finds what tree.Node.At finds: the first member of the name,
// the first entry whose key member's text is the key, the element at an
// index, or the first element equal to an item. The object and list
// items of a set are matched by their RFC 8785 text (schemaidx.
// AppendItemJSON), which is exact for every tree whose objects have
// unique member names, as the profile parses them (TestFinderMatchesAt,
// FuzzFinder).
type finder struct {
	w      *work
	fields map[*tree.Node]map[string]*tree.Node
	keyed  map[keyedList]map[string]*tree.Node
	items  map[*tree.Node]*itemIndex
}

// keyedList names the index of a keyed list by one key member.
type keyedList struct {
	list     *tree.Node
	keyField string
}

// itemIndex finds the first element of a list equal to a diag.Item value.
type itemIndex struct {
	// scalars reports that the maps below were built.
	scalars bool
	// strs maps a string element's text to its first index; numText a
	// number element's text, numVal the value of one whose text parses.
	strs    map[string]int
	numText map[string]int
	numVal  map[float64]int
	// bools holds one plus the index of the first false and true
	// element; null one plus the index of the first null; 0 for none.
	bools [2]int
	null  int
	// values maps the RFC 8785 text of each object and list element to
	// the index of the first; nil until an object or list item is looked
	// up.
	values map[string]int
}

// newFinder returns a finder counting on w.
func (w *work) newFinder() *finder { return &finder{w: w} }

// at returns the position of the node path names below n or, when that
// node has no position, of the nearest positioned node on the way; ok is
// false for an empty path, a path that names no node, and one with no
// positioned node. A nil node on the way (a null member value or
// element) has no position, and nothing below it.
func (f *finder) at(n *tree.Node, path diag.Path) (tree.Pos, bool) {
	var pos tree.Pos
	cur := n
	for _, e := range path {
		next, ok := f.step(cur, e)
		if !ok {
			return tree.Pos{}, false
		}
		cur = next
		if cur != nil && cur.Pos.Known() {
			pos = cur.Pos
		}
	}
	return pos, pos.Known()
}

// step resolves one path element below n as tree.Node.At does.
func (f *finder) step(n *tree.Node, e diag.PathElem) (*tree.Node, bool) {
	f.w.tick()
	switch e.Kind {
	case diag.ElemField:
		return f.field(n, e.Name)
	case diag.ElemKeyed:
		if n == nil || n.Kind != tree.KindList {
			return nil, false
		}
		k := keyedList{list: n, keyField: e.KeyField}
		idx, ok := f.keyed[k]
		if !ok {
			idx = make(map[string]*tree.Node, len(n.Items))
			for _, it := range n.Items {
				if key, ok := f.w.get(it, e.KeyField); ok && key != nil {
					f.w.charge(len(key.Text))
					if _, dup := idx[key.Text]; !dup {
						idx[key.Text] = it
					}
				}
			}
			if f.keyed == nil {
				f.keyed = map[keyedList]map[string]*tree.Node{}
			}
			f.keyed[k] = idx
		}
		f.w.charge(len(e.Key))
		it, ok := idx[e.Key]
		return it, ok
	case diag.ElemItem:
		if n == nil || n.Kind != tree.KindList {
			return nil, false
		}
		i, ok := f.item(n, e.Item)
		if !ok {
			return nil, false
		}
		return n.Items[i], true
	default:
		next, ok := n.At(diag.Path{e})
		return next, ok
	}
}

// field returns member name of n, through an index of n's members when
// it has more than smallObject; building the index counts the members and
// the bytes of their names, and each lookup the bytes of name, which the
// map hashes.
func (f *finder) field(n *tree.Node, name string) (*tree.Node, bool) {
	if n == nil || n.Kind != tree.KindMap || len(n.Members) <= smallObject {
		return f.w.get(n, name)
	}
	idx, ok := f.fields[n]
	if !ok {
		f.w.charge(len(n.Members))
		idx = make(map[string]*tree.Node, len(n.Members))
		for _, m := range n.Members {
			f.w.charge(len(m.Key))
			if _, dup := idx[m.Key]; !dup {
				idx[m.Key] = m.Value
			}
		}
		if f.fields == nil {
			f.fields = map[*tree.Node]map[string]*tree.Node{}
		}
		f.fields[n] = idx
	}
	f.w.charge(len(name))
	v, ok := idx[name]
	return v, ok
}

// item returns the index of the first element of list n equal to v, a
// diag.Item value, as tree.Node.At compares them.
func (f *finder) item(n *tree.Node, v any) (int, bool) {
	x := f.items[n]
	if x == nil {
		x = &itemIndex{}
		if f.items == nil {
			f.items = map[*tree.Node]*itemIndex{}
		}
		f.items[n] = x
	}
	if raw, ok := v.(json.RawMessage); ok {
		return f.value(n, x, raw)
	}
	f.scalars(n, x)
	f.w.charge(scalarBytes(v))
	return x.scalar(v)
}

// scalarBytes returns the bytes of the text of a diag.Item scalar, which
// a lookup of it in the scalar maps hashes and parses.
func scalarBytes(v any) int {
	switch t := v.(type) {
	case string:
		return len(t)
	case json.Number:
		return len(t)
	default:
		return 0
	}
}

// scalars builds the scalar maps of x, the index of list n, counting the
// elements and the bytes of their texts, which the maps hash and the
// number parse reads.
func (f *finder) scalars(n *tree.Node, x *itemIndex) {
	if x.scalars {
		return
	}
	x.scalars = true
	f.w.charge(len(n.Items))
	x.strs, x.numText, x.numVal = map[string]int{}, map[string]int{}, map[float64]int{}
	for i, it := range n.Items {
		if it == nil {
			continue
		}
		f.w.charge(len(it.Text))
		switch it.Kind {
		case tree.KindString:
			if _, dup := x.strs[it.Text]; !dup {
				x.strs[it.Text] = i
			}
		case tree.KindInt, tree.KindFloat:
			if _, dup := x.numText[it.Text]; !dup {
				x.numText[it.Text] = i
			}
			if fv, err := strconv.ParseFloat(it.Text, 64); err == nil {
				if _, dup := x.numVal[fv]; !dup {
					x.numVal[fv] = i
				}
			}
		case tree.KindBool:
			b := 0
			if it.Bool {
				b = 1
			}
			if x.bools[b] == 0 {
				x.bools[b] = i + 1
			}
		case tree.KindNull:
			if x.null == 0 {
				x.null = i + 1
			}
		default:
		}
	}
}

// scalar returns the index of the first element equal to v, a string,
// json.Number or bool, as tree.Node.At compares them: a number by its
// text or, when both texts parse, by its float64 value.
func (x *itemIndex) scalar(v any) (int, bool) {
	switch t := v.(type) {
	case string:
		i, ok := x.strs[t]
		return i, ok
	case bool:
		b := 0
		if t {
			b = 1
		}
		return x.bools[b] - 1, x.bools[b] > 0
	case json.Number:
		i, ok := x.numText[string(t)]
		if fv, err := strconv.ParseFloat(string(t), 64); err == nil {
			if j, found := x.numVal[fv]; found && (!ok || j < i) {
				i, ok = j, true
			}
		}
		return i, ok
	default:
		return 0, false
	}
}

// value returns the index of the first element of list n equal to the
// JSON value raw, which tree.Node.At decodes with encoding/json
// (UseNumber) and compares by value: a scalar through the scalar maps, a
// null by the first null, and an object or a list by its RFC 8785 text,
// which two values share exactly when that comparison finds them equal
// (strings byte for byte, numbers by their text or double, objects by
// their member names and values), given unique member names. w counts
// raw's bytes and, on the first such lookup in n, the bytes of every
// element's text.
func (f *finder) value(n *tree.Node, x *itemIndex, raw json.RawMessage) (int, bool) {
	f.w.charge(len(raw))
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return 0, false
	}
	switch v.(type) {
	case nil:
		f.scalars(n, x)
		return x.null - 1, x.null > 0
	case string, bool, json.Number:
		f.scalars(n, x)
		return x.scalar(v)
	default:
	}
	if x.values == nil {
		x.values = map[string]int{}
		for i, it := range n.Items {
			if it != nil && (it.Kind == tree.KindMap || it.Kind == tree.KindList) {
				if k := string(f.w.itemJSON(it)); !hasKey(x.values, k) {
					x.values[k] = i
				}
			}
		}
	}
	i, ok := x.values[string(f.w.itemJSON(valueNode(v)))]
	return i, ok
}

// hasKey reports whether m holds k.
func hasKey(m map[string]int, k string) bool {
	_, ok := m[k]
	return ok
}

// valueNode returns the tree of v, a value encoding/json decoded with
// UseNumber, for its RFC 8785 text.
func valueNode(v any) *tree.Node {
	switch t := v.(type) {
	case string:
		return &tree.Node{Kind: tree.KindString, Text: t}
	case bool:
		return &tree.Node{Kind: tree.KindBool, Bool: t}
	case json.Number:
		return &tree.Node{Kind: tree.KindFloat, Text: string(t)}
	case []any:
		n := &tree.Node{Kind: tree.KindList, Items: make([]*tree.Node, len(t))}
		for i, e := range t {
			n.Items[i] = valueNode(e)
		}
		return n
	case map[string]any:
		n := &tree.Node{Kind: tree.KindMap, Members: make([]tree.Member, 0, len(t))}
		for _, k := range slices.Sorted(maps.Keys(t)) {
			n.Members = append(n.Members, tree.Member{Key: k, Value: valueNode(t[k])})
		}
		return n
	default:
		return &tree.Node{Kind: tree.KindNull}
	}
}
