// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// locator maps paths inside a resource to source locations through the
// resource's positioned tree (hub.Resource.Tree).
type locator struct {
	files *tree.FileTable
}

// path returns the location of the node at p in r: the value's position,
// or the member key's when key is set and p ends in a field (a "declared
// in" location, such as spec.overridable). A missing node, or a value
// materialized from a default (tree.StyleDefaulted), takes the location of
// its nearest known ancestor; without one, the resource start. Every list
// item it reads, one per index step and one per item a keyed step
// compares, is a unit of w, so a caller that searched a list per entry
// shows in w's count (TestListLocLinear).
func (l locator) path(w *worker, r *hub.Resource, p diag.Path, key bool) diag.Location {
	if r == nil {
		return diag.Location{}
	}
	if r.Tree == nil {
		return r.Source.Start
	}
	var best tree.Pos
	if r.Tree.Pos.Known() {
		best = r.Tree.Pos
	}
	cur := r.Tree
	for i, e := range p {
		var at tree.Pos
		if e.Kind == diag.ElemField {
			m := member(cur, e.Name)
			if m == nil || m.Value == nil {
				break
			}
			cur, at = m.Value, m.Value.Pos
			if key && i == len(p)-1 {
				at = m.KeyPos
			}
		} else {
			next := step(w, cur, e)
			if next == nil {
				break
			}
			cur, at = next, next.Pos
		}
		if cur.Style == tree.StyleDefaulted {
			break
		}
		if at.Known() {
			best = at
		}
	}
	return l.position(r, best)
}

// step returns the item of the list n that the index, keyed or item
// element e names, or nil, charging w one unit per item it reads. A keyed
// step skips an item whose key member has no value. An item element,
// which no path stage I locates holds, is matched by tree.Node.At, with
// one unit per item charged before that scan.
func step(w *worker, n *tree.Node, e diag.PathElem) *tree.Node {
	if n == nil || n.Kind != tree.KindList {
		return nil
	}
	switch e.Kind {
	case diag.ElemIndex:
		if e.Index < 0 || e.Index >= len(n.Items) {
			return nil
		}
		w.charge(siteLocate, 1)
		return n.Items[e.Index]
	case diag.ElemKeyed:
		for _, it := range n.Items {
			w.charge(siteLocate, 1)
			if k := member(it, e.KeyField); k != nil && k.Value != nil && k.Value.Text == e.Key {
				return it
			}
		}
		return nil
	default:
		w.charge(siteLocate, len(n.Items))
		next, _ := n.At(diag.Path{e})
		return next
	}
}

// listLoc locates the entries of one list spec.<field> of a resource. It
// belongs to the goroutine of its worker. Locating every entry of a list
// costs time linear in the list's length: an entry is read by index, and
// an entry the index does not locate is found through a name index built
// once for the list, never by a keyed search per entry. Every item read
// is a unit of the worker, which the linearity tests count.
type listLoc struct {
	l     locator
	w     *worker
	r     *hub.Resource
	field string
	// list is the tree's list node, or nil when the tree holds no list at
	// spec.<field>.
	list *tree.Node
	// byName maps each name to the index of the first item with that
	// name member, as a keyed path finds it; built on the first fallback.
	// A name longer than maxRefBytes is left out unread: entry only looks
	// up names that resolved, so it never asks for one.
	byName map[string]int
}

// list returns the locator of the list spec.<field> of r on worker w.
func (l locator) list(w *worker, r *hub.Resource, field string) *listLoc {
	ll := &listLoc{l: l, w: w, r: r, field: field}
	if r != nil && r.Tree != nil {
		if spec := member(r.Tree, "spec"); spec != nil {
			if m := member(spec.Value, field); m != nil && m.Value != nil && m.Value.Kind == tree.KindList {
				ll.list = m.Value
			}
		}
	}
	return ll
}

// entry returns the location of element i of the list, whose name member
// is name: the element's own position, read by index. When the element
// has no position, or the tree and the typed list disagree (another name
// at i, or no element i), it returns what the keyed path
// spec.<field>[name=<name>] locates: the first element named name, or its
// nearest known ancestor when that element has no position; the list's
// own location when no element has the name; and without such a list,
// the nearest known ancestor of spec.<field>.
func (ll *listLoc) entry(i int, name string) diag.Location {
	if ll.list == nil {
		return ll.l.path(ll.w, ll.r, entryPath(ll.field, name), false)
	}
	items := ll.list.Items
	if i >= 0 && i < len(items) {
		ll.w.charge(siteLocate, 1)
		it := items[i]
		if k := member(it, "name"); k != nil && k.Value != nil && k.Value.Text == name && it.Pos.Known() {
			return ll.l.position(ll.r, it.Pos)
		}
	}
	if ll.byName == nil {
		ll.byName = make(map[string]int, len(items))
		for k, it := range items {
			ll.w.charge(siteLocate, 1)
			if n := member(it, "name"); n != nil && n.Value != nil && ll.w.resolvable(siteLongLocate, n.Value.Text) {
				if _, seen := ll.byName[n.Value.Text]; !seen {
					ll.byName[n.Value.Text] = k
				}
			}
		}
	}
	p := diag.Path{diag.Field("spec"), diag.Field(ll.field)}
	if k, ok := ll.byName[name]; ok {
		p = append(p, diag.Index(k))
	}
	return ll.l.path(ll.w, ll.r, p, false)
}

// position converts pos to a location: through the FileTable when one is
// set, else with the resource's own file name when pos lies in the file of
// the resource's root node; otherwise the resource start.
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

// member returns the member key of a map node, or nil.
func member(n *tree.Node, key string) *tree.Member {
	if n == nil || n.Kind != tree.KindMap {
		return nil
	}
	for i := range n.Members {
		if n.Members[i].Key == key {
			return &n.Members[i]
		}
	}
	return nil
}
