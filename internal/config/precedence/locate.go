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
// its nearest known ancestor; without one, the resource start.
func (l locator) path(r *hub.Resource, p diag.Path, key bool) diag.Location {
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
			next, ok := cur.At(diag.Path{e})
			if !ok {
				break
			}
			cur, at = next, next.Pos
		}
		if cur == nil || cur.Style == tree.StyleDefaulted {
			break
		}
		if at.Known() {
			best = at
		}
	}
	return l.position(r, best)
}

// entry returns the location of element i of the list spec.<field> of r,
// whose name member is name. It reads the element by index, so locating
// every entry of a long list stays linear; it falls back to the keyed path
// (and its nearest known ancestor) when the tree and the typed list
// disagree or the element has no position.
func (l locator) entry(r *hub.Resource, field string, i int, name string) diag.Location {
	if r != nil && r.Tree != nil {
		if spec := member(r.Tree, "spec"); spec != nil {
			if m := member(spec.Value, field); m != nil && m.Value != nil && m.Value.Kind == tree.KindList && i < len(m.Value.Items) {
				it := m.Value.Items[i]
				if k := member(it, "name"); k != nil && k.Value != nil && k.Value.Text == name && it.Pos.Known() {
					return l.position(r, it.Pos)
				}
			}
		}
	}
	return l.path(r, entryPath(field, name), false)
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
