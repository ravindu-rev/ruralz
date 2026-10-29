// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"encoding/json"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// pos is one side's place in the schema while the diff walks a tree. It
// mirrors schemaidx.Info: the schema before and after the if/then
// dispatch on the instance, whether the place is schemaless content the
// schema allows, the impact classes on the path from the resource root,
// and whether a secret field encloses it; side names the state the tree
// belongs to, which resolves the Policies it attaches.
type pos struct {
	// static is the schema before dispatch; nil without schema.
	static *schemaidx.Node
	// s is static with the dispatch applied to the instance.
	s *schemaidx.Node
	// free reports schemaless content the schema allows (open objects).
	free bool
	// impact is the union of x-ruralz-impact from the root to here.
	impact schemaidx.Impact
	// secret reports this place or an enclosing one carries
	// x-ruralz-secret.
	secret bool
	// side is FROM or TO.
	side side
}

// keywords are the x-ruralz-* keywords the diff reads, cached per schema
// node because schemaidx.Node.Keywords copies.
type keywords struct {
	impact schemaidx.Impact
	secret bool
	// ref is the x-ruralz-ref target kind; "" when absent.
	ref string
}

// kw returns the cached keywords of n.
func (c *comparer) kw(n *schemaidx.Node) keywords {
	if n == nil {
		return keywords{}
	}
	if k, ok := c.kws[n]; ok {
		return k
	}
	all := n.Keywords()
	k := keywords{impact: all.Impact, secret: all.Secret, ref: all.Ref}
	c.kws[n] = k
	return k
}

// step returns the place of inst, a child of parent whose schema before
// dispatch is static.
func (c *comparer) step(parent pos, static *schemaidx.Node, free bool, inst *tree.Node) pos {
	sel := static.Select(inst)
	k := c.kw(sel)
	return pos{
		static: static, s: sel, free: free,
		impact: parent.impact | k.impact,
		secret: parent.secret || k.secret,
		side:   parent.side,
	}
}

// member returns the place of member name, holding inst, of an object at
// parent.
func (c *comparer) member(parent pos, name string, inst *tree.Node) pos {
	child, free := memberSchema(parent.s, parent.free, name)
	return c.step(parent, child, free, inst)
}

// item returns the place of the list element inst of a list at parent.
func (c *comparer) item(parent pos, inst *tree.Node) pos {
	child, free := itemSchema(parent.s, parent.free)
	return c.step(parent, child, free, inst)
}

// memberSchema resolves member name of an object whose schema (dispatch
// applied) is parent: a declared property, else the typed-map value
// schema, else schemaless content when the object is open.
func memberSchema(parent *schemaidx.Node, parentFree bool, name string) (*schemaidx.Node, bool) {
	if parent == nil {
		return nil, parentFree
	}
	if p, ok := parent.Property(name); ok {
		return p, false
	}
	if v, ok := parent.Values(); ok {
		return v, false
	}
	return nil, parent.Open()
}

// itemSchema resolves the elements of a list whose schema is parent.
func itemSchema(parent *schemaidx.Node, parentFree bool) (*schemaidx.Node, bool) {
	if parent == nil {
		return nil, parentFree
	}
	if it, ok := parent.Items(); ok {
		return it, false
	}
	return nil, parent.Types().Has(schemaidx.TypeArray)
}

// defaultOf returns the cached default of a since position, or nil.
func (c *comparer) defaultOf(s *schemaidx.Node) *tree.Node {
	if d, ok := c.defaults[s]; ok {
		return d
	}
	d := s.DefaultNode()
	c.defaults[s] = d
	return d
}

// omitted reports a member the canonical form leaves out: one whose
// schema (static, before dispatch) carries x-ruralz-since above 0 and whose
// value equals its default, compared in canonical form. At schema level 0,
// every M1 schema, nothing is omitted.
func (c *comparer) omitted(static *schemaidx.Node, v *tree.Node) bool {
	if !c.since || static.Since() <= 0 {
		return false
	}
	def := c.defaultOf(static)
	return def != nil && c.equal(v, def, static, static)
}

// isNumber reports an integer or float node.
func isNumber(n *tree.Node) bool { return n.Kind == tree.KindInt || n.Kind == tree.KindFloat }

// canonicalNumber returns the RFC 8785 text of a number literal, or the
// literal itself when it has none.
func canonicalNumber(text string) string {
	var buf [32]byte
	b, err := jsonval.AppendCanonicalNumber(buf[:0], json.Number(text))
	if err != nil {
		return text
	}
	return string(b)
}

// scalarEqual reports two equal scalars in canonical form: numbers by
// their RFC 8785 text, so 1.50 equals 1.5. Containers are never equal
// here.
func scalarEqual(a, b *tree.Node) bool {
	if isNumber(a) && isNumber(b) {
		return a.Text == b.Text || canonicalNumber(a.Text) == canonicalNumber(b.Text)
	}
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case tree.KindBool:
		return a.Bool == b.Bool
	case tree.KindString:
		return a.Text == b.Text
	case tree.KindNull:
		return true
	default:
		return false
	}
}

// equal reports whether a and b have equal canonical forms: equal
// ruralz.canonical.v1 bytes, with since omission applied inside both. sa
// and sb are their schemas before dispatch; they are read only when the
// schema level is above 0.
func (c *comparer) equal(a, b *tree.Node, sa, sb *schemaidx.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	switch {
	case a.Kind == tree.KindList && b.Kind == tree.KindList:
		if len(a.Items) != len(b.Items) {
			return false
		}
		var ia, ib *schemaidx.Node
		if c.since {
			ia, _ = sa.Items()
			ib, _ = sb.Items()
		}
		for i := range a.Items {
			if !c.equal(a.Items[i], b.Items[i], ia, ib) {
				return false
			}
		}
		return true
	case a.Kind == tree.KindMap && b.Kind == tree.KindMap:
		if c.since {
			sa, sb = sa.Select(a), sb.Select(b)
		}
		return union(a, b, func(name string, va, vb *tree.Node) bool {
			var ca, cb *schemaidx.Node
			if c.since {
				ca, _ = memberSchema(sa, false, name)
				cb, _ = memberSchema(sb, false, name)
			}
			if va != nil && c.omitted(ca, va) {
				va = nil
			}
			if vb != nil && c.omitted(cb, vb) {
				vb = nil
			}
			return (va == nil && vb == nil) || (va != nil && vb != nil && c.equal(va, vb, ca, cb))
		})
	default:
		return scalarEqual(a, b)
	}
}

// union calls fn with every member name of the objects a and b in RFC
// 8785 order (UTF-16 code units) and the value on each side, nil where
// absent, until fn returns false; it reports whether fn always returned
// true. Normalized trees hold their members in that order already, so the
// walk is a merge without sorting.
func union(a, b *tree.Node, fn func(name string, va, vb *tree.Node) bool) bool {
	ma, mb := sortedMembers(a.Members), sortedMembers(b.Members)
	i, j := 0, 0
	for i < len(ma) || j < len(mb) {
		var ok bool
		switch {
		case j == len(mb) || (i < len(ma) && jsonval.CompareUTF16(ma[i].Key, mb[j].Key) < 0):
			ok = fn(ma[i].Key, ma[i].Value, nil)
			i++
		case i == len(ma) || jsonval.CompareUTF16(ma[i].Key, mb[j].Key) > 0:
			ok = fn(mb[j].Key, nil, mb[j].Value)
			j++
		default:
			ok = fn(ma[i].Key, ma[i].Value, mb[j].Value)
			i++
			j++
		}
		if !ok {
			return false
		}
	}
	return true
}

// sortedMembers returns ms in RFC 8785 order: ms itself when it already
// is, else a sorted copy.
func sortedMembers(ms []tree.Member) []tree.Member {
	cmpKeys := func(a, b tree.Member) int { return jsonval.CompareUTF16(a.Key, b.Key) }
	if slices.IsSortedFunc(ms, cmpKeys) {
		return ms
	}
	return slices.SortedStableFunc(slices.Values(ms), cmpKeys)
}

// view returns n in canonical form for output: n itself, or at a schema
// level above 0, when a since member at its default sits inside, a copy
// without such members. static is n's schema before dispatch.
func (c *comparer) view(n *tree.Node, static *schemaidx.Node) *tree.Node {
	if !c.since || n == nil {
		return n
	}
	switch n.Kind {
	case tree.KindMap:
		sel := static.Select(n)
		var out *tree.Node
		for i, m := range n.Members {
			cs, _ := memberSchema(sel, false, m.Key)
			v := m.Value
			drop := c.omitted(cs, v)
			if !drop {
				v = c.view(v, cs)
			}
			if out == nil && (drop || v != m.Value) {
				cp := *n
				cp.Members = slices.Clone(n.Members[:i])
				out = &cp
			}
			if out != nil && !drop {
				out.Members = append(out.Members, tree.Member{Key: m.Key, KeyPos: m.KeyPos, Value: v})
			}
		}
		if out != nil {
			return out
		}
	case tree.KindList:
		is, _ := static.Items()
		var out *tree.Node
		for i, it := range n.Items {
			v := c.view(it, is)
			if out == nil && v != it {
				cp := *n
				cp.Items = slices.Clone(n.Items[:i])
				out = &cp
			}
			if out != nil {
				out.Items = append(out.Items, v)
			}
		}
		if out != nil {
			return out
		}
	default:
	}
	return n
}

// scan walks v at p and every node below it, skipping the since members
// the canonical form leaves out, and returns the union of the impact
// classes on the way: the x-ruralz-impact classes of each place and the
// class of each Policy attached there. With show set, v is a value the
// diff shows: every value at or below a secret position must then be part
// of a well-formed reference, as render requires, and anything else fails
// with RZ-CFG-012, naming the path but never the value.
func (c *comparer) scan(v *tree.Node, p pos, show bool) (schemaidx.Impact, error) {
	impact := p.impact | c.attached(v, p)
	if show && p.secret && !reference(p.s, v) {
		return impact, errcode.Errorf(CodeLiteral, "%s %s: a secret field holds a value that is not a secretRef; a diff shows only the reference",
			c.id, c.path.Append(c.scanPath...))
	}
	// descend scans a child; the path is kept only for a shown value,
	// whose error names it.
	descend := func(child *tree.Node, at pos, elem func() diag.PathElem) error {
		if show {
			c.scanPath = append(c.scanPath, elem())
			defer func() { c.scanPath = c.scanPath[:len(c.scanPath)-1] }()
		}
		sub, err := c.scan(child, at, show)
		impact |= sub
		return err
	}
	switch v.Kind {
	case tree.KindMap:
		for _, m := range v.Members {
			cs, _ := memberSchema(p.s, p.free, m.Key)
			if m.Value == nil || c.omitted(cs, m.Value) {
				continue
			}
			if err := descend(m.Value, c.member(p, m.Key, m.Value), func() diag.PathElem { return diag.Field(m.Key) }); err != nil {
				return impact, err
			}
		}
	case tree.KindList:
		for i, it := range v.Items {
			if it == nil {
				continue
			}
			if err := descend(it, c.item(p, it), func() diag.PathElem { return schemaidx.ItemElem(p.s, it, i) }); err != nil {
				return impact, err
			}
		}
	default:
	}
	return impact, nil
}

// reference reports whether n may stand at a secret position whose schema
// (dispatch applied) is s, by the rule render applies: the schema declares
// the position, admits n's type and, when it enumerates string values,
// holds n's value. A literal at a SecretValue field, an undeclared member
// and a provider outside its enum all fail it.
func reference(s *schemaidx.Node, n *tree.Node) bool {
	if s == nil || !s.Types().Has(typeOf(n)) {
		return false
	}
	if enum := s.Enum(); len(enum) > 0 {
		return n.Kind == tree.KindString && slices.Contains(enum, n.Text)
	}
	return true
}

// typeOf returns the JSON Schema instance type of n.
func typeOf(n *tree.Node) schemaidx.TypeSet {
	switch n.Kind {
	case tree.KindNull:
		return schemaidx.TypeNull
	case tree.KindBool:
		return schemaidx.TypeBoolean
	case tree.KindInt:
		return schemaidx.TypeInteger
	case tree.KindFloat:
		return schemaidx.TypeNumber
	case tree.KindString:
		return schemaidx.TypeString
	case tree.KindMap:
		return schemaidx.TypeObject
	case tree.KindList:
		return schemaidx.TypeArray
	default:
		return 0
	}
}
