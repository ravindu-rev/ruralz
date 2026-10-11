// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Materialize writes the defaults res lacks into res.Root, the envelope of
// a resource authored under idx's apiVersion, and returns the number of
// members it added (01 req 36; 02 req 9 to 12):
//
//   - For every object present in the tree, each absent declared member
//     whose schema carries a default gets the rendered view's normalized
//     default (schemaidx.Node.DefaultNode), in list entries, typed-map
//     values and a Policy config by its spec.type dispatch too. An absent
//     object is never created, so a Gateway without stateStore stays
//     without one.
//   - A Policy's spec gets the registry defaults it lacks: slot (the
//     registry slot, or metadata.name for types whose slot is the name),
//     failureMode, and filterClass for every type (R-7; custom for
//     plugin). Their checks belong to later stages (CheckFilterClass at
//     stage H, CheckFailureMode at stage I).
//
// Free content gets nothing: undeclared members of open objects, a plugin
// config, Plugin configSchema and the JSONSchemaDocument value. Every
// materialized node is styled tree.StyleDefaulted and takes the position
// of the object it was added to, so a diagnostic on it points at the
// parent. Authored values are kept, so a second call adds nothing.
func Materialize(idx *schemaidx.Index, reg *registry.Registry, res *tree.Resource) int {
	return materialize(idx, reg, res, newWork(nil))
}

// materialize is Materialize on worker w, which counts the nodes visited
// toward its yield timer.
func materialize(idx *schemaidx.Index, reg *registry.Registry, res *tree.Resource, w *work) int {
	if idx == nil || res == nil || res.Root == nil {
		return 0
	}
	s, ok := idx.Resource(string(res.ID.Kind))
	if !ok {
		return 0
	}
	m := &materializer{w: w}
	if res.ID.Kind == v1alpha1.KindPolicy && reg != nil {
		m.registryDefaults(reg, res)
	}
	m.visit(res.Root, s)
	return m.added
}

type materializer struct {
	added int
	w     *work
}

// visit materializes the defaults of n, whose schema is s, and below it.
func (m *materializer) visit(n *tree.Node, s *schemaidx.Node) {
	if n == nil || s == nil {
		return
	}
	m.w.tick()
	switch n.Kind {
	case tree.KindMap:
		sel := s.Select(n)
		if sel.Def() == defJSONSchemaDocument {
			return
		}
		if m.defaults(n, sel) {
			// A default on a dispatch member would select other
			// branches; the generator emits none, but follow it anyway.
			sel = s.Select(n)
		}
		for i := range n.Members {
			m.visit(n.Members[i].Value, memberSchema(sel, n.Members[i].Key))
		}
	case tree.KindList:
		it := itemSchema(s)
		for _, item := range n.Items {
			m.visit(item, it)
		}
	default:
	}
}

// defaults adds the declared defaults n lacks and reports whether it added
// any. The members present are indexed once for a large object, so the
// cost stays linear in its size.
func (m *materializer) defaults(n *tree.Node, sel *schemaidx.Node) bool {
	var present map[string]struct{}
	has := func(name string) bool {
		if len(n.Members) <= smallObject {
			_, ok := n.Get(name)
			return ok
		}
		if present == nil {
			present = make(map[string]struct{}, len(n.Members))
			for _, mem := range n.Members {
				present[mem.Key] = struct{}{}
			}
		}
		_, ok := present[name]
		return ok
	}
	added := false
	for name, p := range sel.Defaults() {
		if has(name) {
			continue
		}
		d := p.DefaultNode()
		if d == nil {
			continue
		}
		place(d, n.Pos)
		n.Members = append(n.Members, tree.Member{Key: name, KeyPos: n.Pos, Value: d})
		m.added++
		added = true
	}
	return added
}

// smallObject is the member count up to which a linear scan beats an index.
const smallObject = 16

// registryDefaults writes the registry defaults into a Policy's spec (02 req
// 12). A spec that is not an object, or whose type is not a registered
// type string, is left to the schema's diagnostics.
func (m *materializer) registryDefaults(reg *registry.Registry, res *tree.Resource) {
	spec, ok := res.Root.Get("spec")
	if !ok || spec.Kind != tree.KindMap {
		return
	}
	typ, ok := spec.Get("type")
	if !ok || typ.Kind != tree.KindString {
		return
	}
	e, ok := reg.Lookup(v1alpha1.PolicyType(typ.Text))
	if !ok {
		return
	}
	name := res.ID.Name
	if meta, ok := res.Root.Get("metadata"); ok {
		if n, ok := meta.Get("name"); ok && n.Kind == tree.KindString {
			name = n.Text
		}
	}
	d := e.Defaults(name)
	for _, kv := range [...]struct{ key, value string }{
		{"slot", d.Slot},
		{"failureMode", string(d.FailureMode)},
		{"filterClass", string(d.FilterClass)},
	} {
		if kv.value == "" {
			continue
		}
		if _, ok := spec.Get(kv.key); ok {
			continue
		}
		spec.Members = append(spec.Members, tree.Member{
			Key: kv.key, KeyPos: spec.Pos,
			Value: &tree.Node{Kind: tree.KindString, Style: tree.StyleDefaulted, Text: kv.value, Pos: spec.Pos},
		})
		m.added++
	}
}

// place gives a materialized node and everything below it the position p
// of the object it was added to.
func place(n *tree.Node, p tree.Pos) {
	n.Pos = p
	for i := range n.Members {
		n.Members[i].KeyPos = p
		place(n.Members[i].Value, p)
	}
	for _, it := range n.Items {
		place(it, p)
	}
}

// memberSchema returns the schema of member name of an object whose
// schema, dispatch applied, is sel: the declared property, else the
// typed-map value schema, else nil (free content or an unknown member).
func memberSchema(sel *schemaidx.Node, name string) *schemaidx.Node {
	if sel == nil {
		return nil
	}
	if c, ok := sel.Property(name); ok {
		return c
	}
	if v, ok := sel.Values(); ok {
		return v
	}
	return nil
}

// itemSchema returns the schema of the elements of a list whose schema is
// s; nil for free content.
func itemSchema(s *schemaidx.Node) *schemaidx.Node {
	it, _ := s.Items()
	return it
}
