// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// locator converts tree positions to diagnostic locations.
type locator struct {
	files *tree.FileTable
}

// position converts pos, a position in r's tree, to a location: through
// the FileTable when one is set, else with r's own file name when pos lies
// in the file of r's root node; otherwise, and for an unknown position,
// r's start.
func (l locator) position(r *hub.Resource, pos tree.Pos) diag.Location {
	if !pos.Known() {
		return r.Source.Start
	}
	if l.files != nil {
		if loc := l.files.Location(pos); loc.File != "" {
			return loc
		}
	}
	if r.Tree != nil && pos.File == r.Tree.Pos.File && r.Source.File != "" {
		return diag.Location{File: r.Source.File, Line: int(pos.Line), Column: int(pos.Column)}
	}
	return r.Source.Start
}

// indexAbove is the member count from which a map gets a member index.
const indexAbove = 8

// finder locates key-aware paths in one resource's tree. Each list it
// steps into by key or by item, and each large map, is indexed once, so
// locating every entry of a list costs time linear in the list. It
// belongs to one goroutine.
type finder struct {
	l      locator
	r      *hub.Resource
	w      *work
	keyed  map[keyedList]map[string]int
	items  map[*tree.Node]map[string]int
	fields map[*tree.Node]map[string]int
}

// keyedList names the index of a list by one key member.
type keyedList struct {
	list  *tree.Node
	field string
}

// newFinder returns the finder of r on w.
func (l locator) newFinder(r *hub.Resource, w *work) *finder {
	return &finder{l: l, r: r, w: w}
}

// path returns the location of the node at p: its own position, or for a
// missing node or one materialized from a default (tree.StyleDefaulted),
// the position of its nearest authored ancestor; without one, the
// resource start.
func (f *finder) path(p diag.Path) diag.Location {
	_, best := f.find(p)
	return f.l.position(f.r, best)
}

// node returns the node at p, nil when the tree has none.
func (f *finder) node(p diag.Path) *tree.Node {
	n, _ := f.find(p)
	return n
}

// find returns the node at p (nil when missing) and the nearest known
// position on the way that no default produced.
func (f *finder) find(p diag.Path) (*tree.Node, tree.Pos) {
	var best tree.Pos
	cur := f.r.Tree
	if cur == nil {
		return nil, best
	}
	if cur.Pos.Known() && cur.Style != tree.StyleDefaulted {
		best = cur.Pos
	}
	defaulted := cur.Style == tree.StyleDefaulted
	for _, e := range p {
		cur = f.step(cur, e)
		if cur == nil {
			return nil, best
		}
		if cur.Style == tree.StyleDefaulted {
			defaulted = true
		}
		if !defaulted && cur.Pos.Known() {
			best = cur.Pos
		}
	}
	return cur, best
}

// step returns the child of n that e names, or nil.
func (f *finder) step(n *tree.Node, e diag.PathElem) *tree.Node {
	if n == nil {
		return nil
	}
	switch e.Kind {
	case diag.ElemField:
		return f.member(n, e.Name)
	case diag.ElemIndex:
		if n.Kind != tree.KindList || e.Index < 0 || e.Index >= len(n.Items) {
			return nil
		}
		f.w.charge(1)
		return n.Items[e.Index]
	case diag.ElemKeyed:
		if n.Kind != tree.KindList {
			return nil
		}
		i, ok := f.keyedIndex(n, e.KeyField)[e.Key]
		if !ok {
			return nil
		}
		return n.Items[i]
	default:
		if n.Kind != tree.KindList {
			return nil
		}
		if s, ok := e.Item.(string); ok {
			i, found := f.itemIndex(n)[s]
			if !found {
				return nil
			}
			return n.Items[i]
		}
		f.w.charge(len(n.Items))
		next, _ := n.At(diag.Path{e})
		return next
	}
}

// member returns the value of member name of the map n, or nil; a map of
// more than indexAbove members is indexed on first use.
func (f *finder) member(n *tree.Node, name string) *tree.Node {
	if n.Kind != tree.KindMap {
		return nil
	}
	if len(n.Members) <= indexAbove {
		f.w.charge(len(n.Members))
		for i := range n.Members {
			if n.Members[i].Key == name {
				return n.Members[i].Value
			}
		}
		return nil
	}
	if f.fields == nil {
		f.fields = map[*tree.Node]map[string]int{}
	}
	ix, ok := f.fields[n]
	if !ok {
		f.w.charge(len(n.Members))
		ix = make(map[string]int, len(n.Members))
		for i := range n.Members {
			if _, dup := ix[n.Members[i].Key]; !dup {
				ix[n.Members[i].Key] = i
			}
		}
		f.fields[n] = ix
	}
	if i, ok := ix[name]; ok {
		return n.Members[i].Value
	}
	return nil
}

// keyedIndex returns the index of the list n by its entries' string member
// field: the first entry per key, as a keyed path names it.
func (f *finder) keyedIndex(n *tree.Node, field string) map[string]int {
	k := keyedList{list: n, field: field}
	if f.keyed == nil {
		f.keyed = map[keyedList]map[string]int{}
	}
	if ix, ok := f.keyed[k]; ok {
		return ix
	}
	ix := make(map[string]int, len(n.Items))
	for i, it := range n.Items {
		f.w.charge(1)
		if it == nil || it.Kind != tree.KindMap {
			continue
		}
		for j := range it.Members {
			m := &it.Members[j]
			if m.Key != field {
				continue
			}
			if m.Value != nil && m.Value.Kind == tree.KindString {
				if _, dup := ix[m.Value.Text]; !dup {
					ix[m.Value.Text] = i
				}
			}
			break
		}
	}
	f.keyed[k] = ix
	return ix
}

// itemIndex returns the index of the string elements of the list n: the
// first element per text.
func (f *finder) itemIndex(n *tree.Node) map[string]int {
	if f.items == nil {
		f.items = map[*tree.Node]map[string]int{}
	}
	if ix, ok := f.items[n]; ok {
		return ix
	}
	ix := make(map[string]int, len(n.Items))
	for i, it := range n.Items {
		f.w.charge(1)
		if it != nil && it.Kind == tree.KindString {
			if _, dup := ix[it.Text]; !dup {
				ix[it.Text] = i
			}
		}
	}
	f.items[n] = ix
	return ix
}
