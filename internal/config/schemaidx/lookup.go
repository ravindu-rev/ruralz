// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaidx

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// ErrSkipChildren is returned by a WalkFunc to skip the children of the
// current node; Walk then continues with the next sibling.
var ErrSkipChildren = errors.New("schemaidx: skip children")

// Info describes one instance position of a resource: its schema node and
// what the path from the resource root implies. The keywords of each
// position on the path are read from its schema with dispatch applied, so
// an annotation on a then or else branch counts like one on the field.
type Info struct {
	// Node is the schema with dispatch applied to the instance; nil when
	// no schema describes the position.
	Node *Node
	// Free reports a position without schema that the schema allows:
	// inside an undeclared member of an open object, or inside an array
	// without an items schema. A nil Node with Free false is not allowed
	// by the schema (an unknown field, RZ-CFG-006).
	Free bool
	// InSecret reports that the node or an ancestor carries
	// x-ruralz-secret (01 req 28 subtree rule; 02 req 64 security impact).
	InSecret bool
	// NoSubstitution reports a position where ${VAR} is forbidden
	// (RZ-CFG-011, 01 req 28) and render writes no $${ escape (02 req 46):
	// apiVersion, kind, metadata.name, x-ruralz-ref and x-ruralz-cel
	// fields, the elements of such array fields and the values of such
	// typed maps, and the whole x-ruralz-secret subtree. Mapping keys are
	// always forbidden and are not positions.
	NoSubstitution bool
	// PathImpact is the union of x-ruralz-impact from the resource root
	// to this node (02 req 64).
	PathImpact Impact

	// refOrCEL reports an x-ruralz-ref or x-ruralz-cel position, or an
	// element or typed-map value of one: its own elements and values
	// inherit it, as the generator passes noSubst into slice items and map
	// values.
	refOrCEL bool
}

// Known reports whether a schema describes the position.
func (i Info) Known() bool { return i.Node != nil }

// Keywords returns the x-ruralz-* annotations of the position's schema.
func (i Info) Keywords() Keywords { return i.Node.Keywords() }

// edge says how a position hangs off its parent.
type edge struct {
	// free is Info.Free.
	free bool
	// elem reports a list element.
	elem bool
	// value reports a typed-map value: a member resolved through
	// additionalProperties (Values), not a declared property.
	value bool
	// envelope reports apiVersion, kind or metadata.name.
	envelope bool
}

// edgeOf describes the child position e; value reports a typed-map value
// (memberSchema). parentDepth is the depth of the parent (0 for the
// resource root), parentName its member name.
func edgeOf(e diag.PathElem, free, value bool, parentDepth int, parentName string) edge {
	return edge{
		free:     free,
		elem:     e.Kind != diag.ElemField,
		value:    value,
		envelope: envelopeFixed(e, parentDepth, parentName),
	}
}

// step computes the Info of a child position of i whose schema, with
// dispatch applied to the child instance, is sel.
func (i Info) step(sel *Node, how edge) Info {
	out := Info{Node: sel, Free: how.free, InSecret: i.InSecret, PathImpact: i.PathImpact}
	out.refOrCEL = (how.elem || how.value) && i.refOrCEL
	if sel != nil {
		out.InSecret = out.InSecret || sel.kw.Secret
		out.PathImpact |= sel.kw.Impact
		out.refOrCEL = out.refOrCEL || sel.kw.Ref != "" || sel.kw.CEL != nil
	}
	out.NoSubstitution = out.InSecret || out.refOrCEL || how.envelope
	return out
}

// envelopeFixed reports apiVersion, kind and metadata.name at the
// resource root, where substitution is forbidden (01 req 28).
func envelopeFixed(e diag.PathElem, parentDepth int, parentName string) bool {
	if e.Kind != diag.ElemField {
		return false
	}
	switch parentDepth {
	case 0:
		return e.Name == "apiVersion" || e.Name == "kind"
	case 1:
		return parentName == "metadata" && e.Name == "name"
	default:
		return false
	}
}

// memberSchema resolves the schema of member name (typed-map values when not
// declared). It reports free for an undeclared member of an open object
// or of free content, value for a typed-map value, and ok false when the
// schema does not allow it.
func memberSchema(parent *Node, parentFree bool, name string) (child *Node, free, value, ok bool) {
	if parent == nil {
		return nil, parentFree, false, true
	}
	if c, ok := parent.Property(name); ok {
		return c, false, false, true
	}
	if v, ok := parent.Values(); ok {
		return v, false, true, true
	}
	if parent.Open() {
		return nil, true, false, true
	}
	return nil, false, false, false
}

// itemSchema resolves the schema of array elements.
func itemSchema(parent *Node, parentFree bool) (child *Node, free, ok bool) {
	if parent == nil {
		return nil, parentFree, true
	}
	if it, ok := parent.Items(); ok {
		return it, false, true
	}
	if parent.types.Has(TypeArray) {
		return nil, true, true
	}
	return nil, false, false
}

// Lookup returns the Info at path in a resource of kind. res is the
// envelope tree (apiVersion, kind, metadata, spec); it selects the if/then
// dispatch along the path (such as Policy spec.config by spec.type) and
// may be nil or lack the path, in which case dispatch falls back to the
// static schema. ok is false for an unknown kind or a path the schema
// does not allow.
func (x *Index) Lookup(kind string, res *tree.Node, path diag.Path) (Info, bool) {
	info, _, ok := x.resolve(kind, res, path, false)
	return info, ok
}

// SchemaPath returns the instance-independent schema path of path, the
// key of per-field tables such as the deprecation lifecycle (02 req 76):
// members joined by ".", "[]" for array elements and "{}" for typed-map
// values, such as "spec.upstreams[].weight" or "metadata.labels{}".
func (x *Index) SchemaPath(kind string, res *tree.Node, path diag.Path) (string, bool) {
	_, sp, ok := x.resolve(kind, res, path, true)
	return sp, ok
}

func (x *Index) resolve(kind string, res *tree.Node, path diag.Path, wantPath bool) (Info, string, bool) {
	root, ok := x.Resource(kind)
	if !ok {
		return Info{}, "", false
	}
	// node is the schema of the current position with dispatch applied.
	node := root.Select(res)
	info := Info{}.step(node, edge{})
	inst := res
	var sp strings.Builder
	parentName := ""
	for depth, e := range path {
		var child *Node
		var free, value bool
		switch e.Kind {
		case diag.ElemField:
			child, free, value, ok = memberSchema(node, info.Free, e.Name)
			if wantPath {
				if value {
					sp.WriteString("{}")
				} else {
					if sp.Len() > 0 {
						sp.WriteByte('.')
					}
					sp.WriteString(e.Name)
				}
			}
			inst = memberOf(inst, e.Name)
		default:
			child, free, ok = itemSchema(node, info.Free)
			if wantPath {
				sp.WriteString("[]")
			}
			inst = itemOf(inst, e)
		}
		if !ok {
			return Info{}, "", false
		}
		node = child.Select(inst)
		info = info.step(node, edgeOf(e, free, value, depth, parentName))
		parentName = ""
		if e.Kind == diag.ElemField {
			parentName = e.Name
		}
	}
	return info, sp.String(), true
}

func memberOf(inst *tree.Node, name string) *tree.Node {
	m, _ := inst.Get(name)
	return m
}

func itemOf(inst *tree.Node, e diag.PathElem) *tree.Node {
	if inst == nil || inst.Kind != tree.KindList {
		return nil
	}
	switch e.Kind {
	case diag.ElemIndex:
		if e.Index >= 0 && e.Index < len(inst.Items) {
			return inst.Items[e.Index]
		}
	case diag.ElemKeyed:
		for _, it := range inst.Items {
			if k, ok := it.Get(e.KeyField); ok && isKeyScalar(k) && k.Text == e.Key {
				return it
			}
		}
	case diag.ElemItem:
		for _, it := range inst.Items {
			if itemMatches(it, e.Item) {
				return it
			}
		}
	default:
	}
	return nil
}

func isKeyScalar(n *tree.Node) bool {
	return n.Kind == tree.KindString || n.Kind == tree.KindInt || n.Kind == tree.KindFloat
}

// itemMatches compares a set element with a diag.Item value. Numbers
// compare by their double value, as the canonical form encodes them
// (RFC 8785), so 1.50 matches 1.5.
func itemMatches(n *tree.Node, v any) bool {
	switch t := v.(type) {
	case string:
		return n.Kind == tree.KindString && n.Text == t
	case json.Number:
		return (n.Kind == tree.KindInt || n.Kind == tree.KindFloat) && sameDouble(n.Text, string(t))
	case bool:
		return n.Kind == tree.KindBool && n.Bool == t
	case json.RawMessage:
		return string(AppendItemJSON(nil, n)) == string(t)
	default:
		return false
	}
}

// sameDouble reports whether two number literals are equal texts or parse
// to the same finite double.
func sameDouble(a, b string) bool {
	if a == b {
		return true
	}
	x, errX := strconv.ParseFloat(a, 64)
	y, errY := strconv.ParseFloat(b, 64)
	return errX == nil && errY == nil && x == y
}

// ItemElem returns the key-aware path element of element i of a list
// whose schema is list (02 req 17; 01 req 34): a map or orderedMap entry
// with a scalar key member is [<key>=<value>]; a set element is
// [item=<value>] with the element's canonical JSON (AppendItemJSON), so
// equal elements such as 1.50 and 1.5 get one element ([item=1.5]);
// everything else, and every element of a list without x-ruralz-list, is
// [<i>].
func ItemElem(list *Node, item *tree.Node, i int) diag.PathElem {
	lt, key := list.List()
	switch {
	case lt.Keyed():
		if k, ok := item.Get(key); ok && isKeyScalar(k) {
			return diag.Keyed(key, k.Text)
		}
	case lt == ListSet && item != nil:
		switch item.Kind {
		case tree.KindString:
			return diag.Item(item.Text)
		case tree.KindInt, tree.KindFloat:
			return diag.Item(json.Number(canonicalNumber(item)))
		case tree.KindBool:
			return diag.Item(item.Bool)
		case tree.KindMap, tree.KindList:
			return diag.Item(json.RawMessage(AppendItemJSON(nil, item)))
		default:
		}
	default:
	}
	return diag.Index(i)
}

// Cursor is the position Walk reports. Walk reuses one Cursor: it is valid
// only during the WalkFunc call.
type Cursor struct {
	// Node is the instance node.
	Node *tree.Node
	// Info describes the position as it was when the call began.
	Info   Info
	path   diag.Path
	static *Node
}

// Schema returns the position's schema with the dispatch applied to
// Node's current members, which differs from Info.Node after the callback
// changed a dispatch member such as spec.type.
func (c *Cursor) Schema() *Node { return c.static.Select(c.Node) }

// Path returns a new copy of the key-aware path from the resource root.
func (c *Cursor) Path() diag.Path {
	out := make(diag.Path, len(c.path))
	copy(out, c.path)
	return out
}

// Depth returns the number of path elements (0 at the resource root).
func (c *Cursor) Depth() int { return len(c.path) }

// Last returns the last path element; false at the resource root.
func (c *Cursor) Last() (diag.PathElem, bool) {
	if len(c.path) == 0 {
		return diag.PathElem{}, false
	}
	return c.path[len(c.path)-1], true
}

// WalkFunc is called for every node Walk visits. It may add, replace or
// remove members and items of c.Node (Walk then visits the current ones),
// but not of its ancestors. Returning ErrSkipChildren skips the children;
// any other error stops the walk.
type WalkFunc func(c *Cursor) error

// Walk visits res, a resource envelope of kind, and every node below it in
// document order (members in authored order, items in order), each with
// its Info: if/then dispatch is applied per object from the instance (01
// req 25 reads the Policy config schema by spec.type), list elements get
// key-aware path elements (ItemElem), and InSecret, NoSubstitution and
// PathImpact accumulate from the root. Positions without schema are
// visited too, with a nil Info.Node. Walk returns an error for an unknown
// kind or a nil res, and fn's error otherwise.
func (x *Index) Walk(kind string, res *tree.Node, fn WalkFunc) error {
	node, ok := x.Resource(kind)
	if !ok {
		return errors.New("schemaidx: unknown kind " + strconv.Quote(kind))
	}
	if res == nil {
		return errors.New("schemaidx: nil resource tree")
	}
	w := &walker{fn: fn}
	return w.visit(res, node, Info{}, edge{}, "")
}

type walker struct {
	fn  WalkFunc
	cur Cursor
}

// visit reports inst with the dispatch of static applied, then descends.
// parent is the parent position's Info and how the edge to inst; name is
// inst's member name ("" for list elements and the root).
func (w *walker) visit(inst *tree.Node, static *Node, parent Info, how edge, name string) error {
	sel := static.Select(inst)
	info := parent.step(sel, how)
	w.cur.Node, w.cur.Info, w.cur.static = inst, info, static
	if err := w.fn(&w.cur); err != nil {
		if errors.Is(err, ErrSkipChildren) {
			return nil
		}
		return err
	}
	// The callback may have edited inst (for example a default added to
	// an object, or an overlay replacing spec.type); dispatch again, and
	// let the children inherit the keywords of the new result.
	if again := static.Select(inst); again != sel {
		info = parent.step(again, how)
	}
	node := info.Node
	depth := len(w.cur.path)
	switch inst.Kind {
	case tree.KindMap:
		for i := 0; i < len(inst.Members); i++ {
			m := inst.Members[i]
			e := diag.Field(m.Key)
			child, free, value, ok := memberSchema(node, info.Free, m.Key)
			if !ok {
				child, free, value = nil, false, false
			}
			if err := w.descend(m.Value, child, info, edgeOf(e, free, value, depth, name), e, m.Key); err != nil {
				return err
			}
		}
	case tree.KindList:
		child, free, ok := itemSchema(node, info.Free)
		if !ok {
			child, free = nil, false
		}
		for i := 0; i < len(inst.Items); i++ {
			e := ItemElem(node, inst.Items[i], i)
			if err := w.descend(inst.Items[i], child, info, edgeOf(e, free, false, depth, name), e, ""); err != nil {
				return err
			}
		}
	default:
	}
	return nil
}

func (w *walker) descend(inst *tree.Node, static *Node, parent Info, how edge, e diag.PathElem, name string) error {
	if inst == nil {
		return nil
	}
	w.cur.path = append(w.cur.path, e)
	err := w.visit(inst, static, parent, how, name)
	w.cur.path = w.cur.path[:len(w.cur.path)-1]
	return err
}

// AppendItemJSON appends n as RFC 8785 JSON, the diag.Item form of an
// object or array set element: members sorted by UTF-16 code units, no
// whitespace, strings escaped per RFC 8785, and every number in the
// ECMAScript form of its double value (an integer within ±(2^53−1) is its
// normalized decimal; a larger one, which 02 req 16 rejects later in
// authored configuration, becomes the decimal of its nearest double, as
// RFC 8785 prescribes). The number form and the member order are
// jsonval.AppendCanonicalNumber and jsonval.CompareUTF16, which
// config/canonical uses too. A literal outside the RFC 8259 grammar, or
// one that overflows a double, is written unchanged.
func AppendItemJSON(dst []byte, n *tree.Node) []byte {
	if n == nil {
		return append(dst, "null"...)
	}
	switch n.Kind {
	case tree.KindBool:
		return strconv.AppendBool(dst, n.Bool)
	case tree.KindInt, tree.KindFloat:
		return appendItemNumber(dst, n)
	case tree.KindString:
		return appendJCSString(dst, n.Text)
	case tree.KindMap:
		members := slices.Clone(n.Members)
		slices.SortStableFunc(members, func(a, b tree.Member) int { return jsonval.CompareUTF16(a.Key, b.Key) })
		dst = append(dst, '{')
		for i, m := range members {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJCSString(dst, m.Key)
			dst = append(dst, ':')
			dst = AppendItemJSON(dst, m.Value)
		}
		return append(dst, '}')
	case tree.KindList:
		dst = append(dst, '[')
		for i, it := range n.Items {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = AppendItemJSON(dst, it)
		}
		return append(dst, ']')
	default:
		return append(dst, "null"...)
	}
}

// appendItemNumber writes the RFC 8785 form of a number node: the
// jsonval.AppendCanonicalNumber form, which config/canonical also writes. A
// literal outside the RFC 8259 grammar or one that overflows a double is
// written unchanged.
func appendItemNumber(dst []byte, n *tree.Node) []byte {
	out, err := jsonval.AppendCanonicalNumber(dst, json.Number(n.Text))
	if err != nil {
		return append(dst, n.Text...)
	}
	return out
}

// canonicalNumber returns the RFC 8785 text of a number node, without
// allocating when it equals the node's text.
func canonicalNumber(n *tree.Node) string {
	var buf [32]byte
	b := appendItemNumber(buf[:0], n)
	if string(b) == n.Text {
		return n.Text
	}
	return string(b)
}

// appendJCSString writes s quoted per RFC 8785 3.2.2.2: only '"', '\' and
// U+0000 to U+001F are escaped, as \b \t \n \f \r or \u00xx (lowercase).
func appendJCSString(dst []byte, s string) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			dst = append(dst, '\\', c)
		case c == '\b':
			dst = append(dst, '\\', 'b')
		case c == '\t':
			dst = append(dst, '\\', 't')
		case c == '\n':
			dst = append(dst, '\\', 'n')
		case c == '\f':
			dst = append(dst, '\\', 'f')
		case c == '\r':
			dst = append(dst, '\\', 'r')
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		default:
			dst = append(dst, c)
		}
	}
	return append(dst, '"')
}
