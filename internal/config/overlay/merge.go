// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"slices"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// merger merges one overlay document, or checks one base document; id is
// the resource every diagnostic names.
type merger struct {
	a  *applier
	id tree.ID
	// stack is checkBase's path from the resource root.
	stack diag.Path
}

func (m *merger) errorAt(code string, p tree.Pos, path diag.Path, msg, hint string, related ...diag.Related) {
	m.a.report(diag.Diagnostic{
		Code: code, Location: m.a.loc(p), Resource: m.id.ResourceID(), Path: path,
		Message: msg, Hint: hint, Related: related,
	})
}

// takeAction removes the ruralz.io/patch annotation from an overlay
// document root and returns its value: "" (merge), PatchDelete or
// PatchReplace. ok is false for an unsupported value (RZ-CFG-005); the
// document is then not applied.
func (m *merger) takeAction(root *tree.Node) (action string, ok bool) {
	md, _ := root.Get("metadata")
	ann, _ := md.Get("annotations")
	v, found := ann.Get(PatchAnnotation)
	if !found {
		return "", true
	}
	ann.Delete(PatchAnnotation)
	if len(ann.Members) == 0 {
		md.Delete("annotations")
	}
	if v.Kind == tree.KindString && (v.Text == PatchDelete || v.Text == PatchReplace) {
		return v.Text, true
	}
	m.errorAt(CodeSchema, v.Pos,
		diag.Path{diag.Field("metadata"), diag.Field("annotations"), diag.Field(PatchAnnotation)},
		"unsupported "+PatchAnnotation+" value "+scalarText(v), "use delete or replace")
	return "", false
}

// opaque reports the JSONSchemaDocument position, whose value an overlay
// replaces whole and nothing inside it is merged.
func opaque(s *schemaidx.Node) bool { return s.Def() == schemaDocumentDef }

// memberSchema returns the schema of member name of an object whose
// dispatched schema is sel: the declared property, else the typed-map
// value schema, else nil (free content or an unknown member).
func memberSchema(sel *schemaidx.Node, name string) *schemaidx.Node {
	if c, ok := sel.Property(name); ok {
		return c
	}
	if v, ok := sel.Values(); ok {
		return v
	}
	return nil
}

// itemSchema returns the schema of list elements, or nil.
func itemSchema(list *schemaidx.Node) *schemaidx.Node {
	it, _ := list.Items()
	return it
}

func composite(n *tree.Node) bool { return n.Kind == tree.KindMap || n.Kind == tree.KindList }

// merge merges overlay value ov into base value b (nil when absent) at a
// position whose undispatched schema is static and returns the result.
func (m *merger) merge(b, ov *tree.Node, static *schemaidx.Node, path diag.Path) *tree.Node {
	if b == nil || opaque(static) {
		return m.normalize(ov, static, path)
	}
	switch {
	case ov.Kind == tree.KindMap && b.Kind == tree.KindMap:
		return m.mergeMap(b, ov, static, path)
	case ov.Kind == tree.KindList && b.Kind == tree.KindList && keyed(static):
		return m.mergeKeyed(b, ov, static, path)
	default:
		return m.normalize(ov, static, path)
	}
}

// normalize returns ov merged against an empty base: directives are
// consumed, null members dropped, and misplaced directives reported.
func (m *merger) normalize(ov *tree.Node, static *schemaidx.Node, path diag.Path) *tree.Node {
	if opaque(static) {
		return ov
	}
	switch ov.Kind {
	case tree.KindMap:
		return m.mergeMap(&tree.Node{Kind: tree.KindMap, Style: ov.Style, Pos: ov.Pos}, ov, static, path)
	case tree.KindList:
		if keyed(static) {
			return m.mergeKeyed(&tree.Node{Kind: tree.KindList, Style: ov.Style, Pos: ov.Pos}, ov, static, path)
		}
		it := itemSchema(static)
		kept := ov.Items[:0]
		for i, e := range ov.Items {
			if !composite(e) {
				kept = append(kept, e)
				continue
			}
			ep := path.Append(schemaidx.ItemElem(static, e, i))
			if e.Kind == tree.KindMap && !opaque(it) && !m.wholeListEntry(e, static, ep) {
				continue
			}
			kept = append(kept, m.normalize(e, it, ep))
		}
		clear(ov.Items[len(kept):])
		ov.Items = kept
		return ov
	default:
		return ov
	}
}

// wholeListEntry reports a $patch member of e, an object element of a
// list the overlay replaces whole (set, atomic or without x-ruralz-list),
// as RZ-CFG-005 (01 req 25) and removes it. It returns false when e held
// only the directive, which is then dropped from the list.
func (m *merger) wholeListEntry(e *tree.Node, list *schemaidx.Node, ep diag.Path) bool {
	j := slices.IndexFunc(e.Members, func(mem tree.Member) bool { return mem.Key == PatchKey })
	if j < 0 {
		return true
	}
	hint := wholeList(list) + " is replaced whole: write the complete list"
	if replaceDirective(e) {
		m.errorAt(CodeSchema, e.Pos, ep, "{$patch: replace} is valid only in an orderedMap list", hint)
	} else {
		m.errorAt(CodeSchema, e.Members[j].KeyPos, ep.Append(diag.Field(PatchKey)),
			"$patch is valid only in an entry of a map or orderedMap list", hint)
	}
	e.Members = slices.Delete(e.Members, j, j+1)
	return len(e.Members) > 0
}

// wholeList names a list an overlay replaces whole, for hints.
func wholeList(list *schemaidx.Node) string {
	switch lt, _ := list.List(); lt {
	case schemaidx.ListNone:
		return "a list without x-ruralz-list"
	case schemaidx.ListAtomic:
		return "an atomic list"
	default:
		return "a " + lt.String() + " list"
	}
}

// keyed reports a map or orderedMap list with a key field.
func keyed(list *schemaidx.Node) bool {
	lt, key := list.List()
	return lt.Keyed() && key != ""
}

// pendingMember is a composite member mergeMap resolves once the merged
// object's dispatch is known.
type pendingMember struct {
	key  string
	base *tree.Node
	ov   *tree.Node
}

// mergeMap merges overlay object ov into base object b (01 req 25):
// members merge recursively, an overlay scalar or non-keyed list wins, an
// overlay null removes the member, and new members are appended in
// overlay order. Scalars and removals apply first, so the dispatch that
// selects the composite members' schemas (a Policy config by spec.type)
// reads the merged object. An overlay scalar equal to the base scalar
// keeps the base node and its position.
func (m *merger) mergeMap(b, ov *tree.Node, static *schemaidx.Node, path diag.Path) *tree.Node {
	var todo []pendingMember
	for _, om := range ov.Members {
		if om.Key == PatchKey {
			m.misplaced(om.KeyPos, path, static)
			continue
		}
		v := om.Value
		if v.Kind == tree.KindNull {
			b.Delete(om.Key)
			continue
		}
		i := slices.IndexFunc(b.Members, func(bm tree.Member) bool { return bm.Key == om.Key })
		if i < 0 {
			b.Members = append(b.Members, tree.Member{Key: om.Key, KeyPos: om.KeyPos, Value: v})
			if composite(v) {
				todo = append(todo, pendingMember{key: om.Key, ov: v})
			}
			continue
		}
		bv := b.Members[i].Value
		switch {
		case !composite(v):
			if !scalarEqual(bv, v) {
				b.Members[i].KeyPos, b.Members[i].Value = om.KeyPos, v
			}
		case bv.Kind == v.Kind:
			todo = append(todo, pendingMember{key: om.Key, base: bv, ov: v})
		default:
			b.Members[i].KeyPos, b.Members[i].Value = om.KeyPos, v
			todo = append(todo, pendingMember{key: om.Key, ov: v})
		}
	}
	sel := static.Select(b)
	for _, t := range todo {
		child := memberSchema(sel, t.key)
		res := m.merge(t.base, t.ov, child, path.Append(diag.Field(t.key)))
		for i := range b.Members {
			if b.Members[i].Key == t.key {
				b.Members[i].Value = res
			}
		}
	}
	return b
}

// misplaced reports a $patch member of an overlay object outside a
// keyed-list entry (RZ-CFG-005, 01 req 25). static is nil in free
// content, whose $patch is no more data than anywhere else: the
// directive is valid only in overlays and only where 01 req 25 places it.
func (m *merger) misplaced(p tree.Pos, path diag.Path, static *schemaidx.Node) {
	hint := "delete a keyed-list entry with {<key>: <value>, $patch: delete}; remove or replace a resource with the " +
		PatchAnnotation + " annotation"
	if static == nil {
		hint = "content without a schema merges as data, objects member by member and lists replaced whole; remove $patch"
	}
	m.errorAt(CodeSchema, p, path.Append(diag.Field(PatchKey)), "$patch is not valid in an object", hint)
}

// mergeKeyed merges overlay list ov into base list b, both map or
// orderedMap lists keyed by the list's key field (01 req 25): entries
// matched by key merge recursively in place, new entries are appended in
// overlay order, {<key>: v, $patch: delete} removes the entry (absent
// key: no-op), and {$patch: replace} as the first element of an
// orderedMap list makes the result the remaining overlay entries in order.
func (m *merger) mergeKeyed(b, ov *tree.Node, list *schemaidx.Node, path diag.Path) *tree.Node {
	lt, key := list.List()
	item := itemSchema(list)
	result := slices.Clone(b.Items)
	seen := map[string]*tree.Node{}
	for i, e := range ov.Items {
		ep := path.Append(schemaidx.ItemElem(list, e, i))
		if replaceDirective(e) {
			switch {
			case lt != schemaidx.ListOrderedMap:
				m.errorAt(CodeSchema, e.Pos, ep, "{$patch: replace} is valid only in an orderedMap list",
					"a "+lt.String()+" list merges by "+strconv.Quote(key)+"; remove entries with $patch: delete")
			case i > 0:
				m.errorAt(CodeSchema, e.Pos, ep, "{$patch: replace} must be the first element of the list", "")
			default:
				result = nil
			}
			continue
		}
		k, ok := m.entryKey(e, key, ep)
		if !ok {
			continue
		}
		id := keyID(k)
		if prev, dup := seen[id]; dup {
			m.errorAt(CodeSchema, k.Pos, ep,
				"two entries with "+key+" "+scalarText(k)+" in one overlay list", "",
				diag.Related{Location: m.a.loc(prev.Pos), Message: "first entry"})
			continue
		}
		seen[id] = k
		if _, del := e.Get(PatchKey); del {
			if len(e.Members) != 2 {
				m.errorAt(CodeSchema, e.Pos, ep,
					"a $patch: delete entry holds only "+strconv.Quote(key)+" and $patch", "")
				continue
			}
			result = slices.DeleteFunc(result, func(x *tree.Node) bool { return matchesKey(x, key, id) })
			continue
		}
		if j := slices.IndexFunc(result, func(x *tree.Node) bool { return matchesKey(x, key, id) }); j >= 0 {
			result[j] = m.mergeMap(result[j], e, item, ep)
			continue
		}
		result = append(result, m.normalize(e, item, ep))
	}
	b.Items = result
	return b
}

// entryKey validates an overlay entry of a keyed list and returns its key
// node: the entry is an object holding the key field as a scalar, and a
// $patch member, when present, is delete.
func (m *merger) entryKey(e *tree.Node, key string, ep diag.Path) (*tree.Node, bool) {
	if e.Kind != tree.KindMap {
		m.errorAt(CodeSchema, e.Pos, ep, "list entry must be an object with its key field "+strconv.Quote(key), "")
		return nil, false
	}
	if pv, ok := e.Get(PatchKey); ok && (pv.Kind != tree.KindString || pv.Text != PatchDelete) {
		m.errorAt(CodeSchema, pv.Pos, ep.Append(diag.Field(PatchKey)),
			"unsupported $patch value "+scalarText(pv)+" in a list entry",
			"use $patch: delete, or {$patch: replace} as the first element of an orderedMap list")
		return nil, false
	}
	k, ok := e.Get(key)
	if !ok || !keyScalar(k) {
		m.errorAt(CodeSchema, e.Pos, ep, "list entry has no key field "+strconv.Quote(key), "")
		return nil, false
	}
	return k, true
}

// replaceDirective reports the element {$patch: replace}.
func replaceDirective(e *tree.Node) bool {
	if e.Kind != tree.KindMap || len(e.Members) != 1 {
		return false
	}
	v := e.Members[0].Value
	return e.Members[0].Key == PatchKey && v.Kind == tree.KindString && v.Text == PatchReplace
}

// keyScalar reports a node usable as a list key (as schemaidx.ItemElem).
func keyScalar(n *tree.Node) bool {
	return n.Kind == tree.KindString || n.Kind == tree.KindInt || n.Kind == tree.KindFloat
}

// keyID identifies a key value: strings and numbers never match each
// other.
func keyID(n *tree.Node) string {
	if n.Kind == tree.KindString {
		return "s" + n.Text
	}
	return "n" + n.Text
}

// matchesKey reports a base entry whose key field has the key id.
func matchesKey(x *tree.Node, key, id string) bool {
	k, ok := x.Get(key)
	return ok && keyScalar(k) && keyID(k) == id
}

// scalarEqual reports two scalars of one kind and value.
func scalarEqual(a, b *tree.Node) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case tree.KindBool:
		return a.Bool == b.Bool
	case tree.KindNull:
		return true
	case tree.KindInt, tree.KindFloat, tree.KindString:
		return a.Text == b.Text
	default:
		return false
	}
}

// scalarText quotes a scalar for a message, or names its kind.
func scalarText(n *tree.Node) string {
	switch n.Kind {
	case tree.KindString:
		return strconv.Quote(n.Text)
	case tree.KindInt, tree.KindFloat:
		return n.Text
	case tree.KindBool:
		return strconv.FormatBool(n.Bool)
	case tree.KindNull:
		return "null"
	case tree.KindMap:
		return "(an object)"
	default:
		return "(a list)"
	}
}

// checkBase reports every $patch member of a base document (RZ-CFG-006,
// 01 req 25), free content included; only the JSONSchemaDocument value
// is skipped. It keeps one path stack and copies it only to report.
func (m *merger) checkBase(n *tree.Node, static *schemaidx.Node) {
	if opaque(static) {
		return
	}
	switch n.Kind {
	case tree.KindMap:
		sel := static.Select(n)
		for _, mem := range n.Members {
			switch {
			case mem.Key == PatchKey:
				m.errorAt(CodeBasePatch, mem.KeyPos, m.stack.Append(diag.Field(mem.Key)), "$patch is valid only in overlays", "")
			case composite(mem.Value):
				m.stack = append(m.stack, diag.Field(mem.Key))
				m.checkBase(mem.Value, memberSchema(sel, mem.Key))
				m.stack = m.stack[:len(m.stack)-1]
			default:
			}
		}
	case tree.KindList:
		it := itemSchema(static)
		for i, e := range n.Items {
			if composite(e) {
				m.stack = append(m.stack, schemaidx.ItemElem(static, e, i))
				m.checkBase(e, it)
				m.stack = m.stack[:len(m.stack)-1]
			}
		}
	default:
	}
}
