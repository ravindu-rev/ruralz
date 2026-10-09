// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// position is where a validation error's instance location lies in the
// resource tree.
type position struct {
	// path is the key-aware path from the resource root (01 req 48); nil
	// at the root.
	path *pathNode
	// node is the instance node; nil when the tree lacks the location.
	node *tree.Node
	// pos is the nearest known source position at or above node: a
	// defaulted node points at its parent (01 req 36).
	pos tree.Pos
	// schema is the schema of the position with dispatch applied to the
	// instance; nil where no schema describes it (open content).
	schema *schemaidx.Node
	// secret is the x-ruralz-secret position at or above the location,
	// nil outside every secret subtree.
	secret *position
}

// memberSchema returns the schema of member name of an object whose
// schema is parent: the declared property, else the typed-map value
// schema; nil for an undeclared member.
func memberSchema(parent *schemaidx.Node, name string) *schemaidx.Node {
	if p, ok := parent.Property(name); ok {
		return p
	}
	if v, ok := parent.Values(); ok {
		return v
	}
	return nil
}

// memberIndexMin is the member count from which an object's members are
// looked up through a memoized index instead of a scan: every first step
// into one large object (thousands of unknown members or typed-map
// values) would otherwise scan it again, quadratic in its size.
const memberIndexMin = 16

// memo memoizes, for one Validate call, everything locate and the
// diagnostics repeat per error: the position each instance location
// prefix reaches (a trie of steps, so each position's path, element text,
// schema dispatch and secret mark are computed once however many errors
// lie at or below it), the member index of large objects, the secret mark
// of each schema node, the interned path texts and the common prefixes of
// long path segments. Mapping work is then linear in the number of errors
// times their location depth plus the instance size.
type memo struct {
	start *schemaidx.Node
	root  *tree.Node
	// top is the step at the resource root, built on first use.
	top     *step
	members map[*tree.Node]map[string]int
	secrets map[*schemaidx.Node]bool
	texts   map[textAt]*textNode
	lcp     map[textPair]int
	// read counts the members and elements that scans, index builds,
	// dispatches and path elements read: the work the scale tests bound,
	// linear in the instance size.
	read int
}

// step is a memoized position with the steps below it, keyed by the
// location token (a member name or a decimal list index). It holds its
// path node and, when the path's text is new, the interned text node, so
// one step is one allocation. The schema has no typed map or open object
// whose members carry errors below them, so the tokens compared per error
// are short except at a location's last step.
type step struct {
	position
	tok  string
	pn   pathNode
	tn   textNode
	few  []*step
	many map[string]*step
}

// fewSteps is the child count up to which a step scans its children
// instead of indexing them: most positions have a handful.
const fewSteps = 8

// find returns the child step at tok.
func (s *step) find(tok string) (*step, bool) {
	if s.many != nil {
		c, ok := s.many[tok]
		return c, ok
	}
	for _, c := range s.few {
		if c.tok == tok {
			return c, true
		}
	}
	return nil, false
}

// add records child step c.
func (s *step) add(c *step) {
	if s.many == nil && len(s.few) < fewSteps {
		s.few = append(s.few, c)
		return
	}
	if s.many == nil {
		s.many = make(map[string]*step, 2*fewSteps)
		for _, f := range s.few {
			s.many[f.tok] = f
		}
		s.few = nil
	}
	s.many[c.tok] = c
}

// newMemo returns the memo of one resource whose schema is start and
// whose tree is root.
func newMemo(start *schemaidx.Node, root *tree.Node) *memo {
	return &memo{start: start, root: root}
}

// index returns the position in n.Members of the first member named key,
// the member tree.Node.Get returns; false when n is not an object or has
// no such member.
func (x *memo) index(n *tree.Node, key string) (int, bool) {
	if n == nil || n.Kind != tree.KindMap {
		return 0, false
	}
	if len(n.Members) < memberIndexMin {
		for i := range n.Members {
			x.read++
			if n.Members[i].Key == key {
				return i, true
			}
		}
		return 0, false
	}
	names, ok := x.members[n]
	if !ok {
		names = make(map[string]int, len(n.Members))
		for i := len(n.Members) - 1; i >= 0; i-- {
			names[n.Members[i].Key] = i
		}
		x.read += len(n.Members)
		if x.members == nil {
			x.members = map[*tree.Node]map[string]int{}
		}
		x.members[n] = names
	}
	i, ok := names[key]
	return i, ok
}

// get returns the value of member key of n, as tree.Node.Get does.
func (x *memo) get(n *tree.Node, key string) (*tree.Node, bool) {
	i, ok := x.index(n, key)
	if !ok {
		return nil, false
	}
	return n.Members[i].Value, true
}

// selectFor returns s.Select(inst); each dispatch case reads members of
// inst. Steps call it once per position.
func (x *memo) selectFor(s *schemaidx.Node, inst *tree.Node) *schemaidx.Node {
	if inst != nil && inst.Kind == tree.KindMap {
		x.read += len(inst.Members)
	}
	return s.Select(inst)
}

// itemElem returns schemaidx.ItemElem(list, item, i). It reads the
// element (a keyed entry's members, a set element's canonical JSON), which
// may be large; steps call it once per element.
func (x *memo) itemElem(list *schemaidx.Node, item *tree.Node, i int) diag.PathElem {
	if item != nil {
		x.read += 1 + len(item.Members) + len(item.Items)
	}
	return schemaidx.ItemElem(list, item, i)
}

// secret reports whether schema s carries x-ruralz-secret, memoized per
// schema node: Keywords returns a copy of every annotation.
func (x *memo) secret(s *schemaidx.Node) bool {
	if s == nil {
		return false
	}
	is, ok := x.secrets[s]
	if !ok {
		is = s.Keywords().Secret
		if x.secrets == nil {
			x.secrets = map[*schemaidx.Node]bool{}
		}
		x.secrets[s] = is
	}
	return is
}

// locate follows an instance location (member names and decimal list
// indexes, as jsonschema/v6 reports them) from the resource root. Each
// step applies the schema dispatch to the instance (a Policy's config
// schema by spec.type) and names list elements by their key-aware element
// (schemaidx.ItemElem), so the path is stable under reordering. Each
// distinct location prefix is resolved once per Validate.
func (x *memo) locate(loc []string) position {
	s := x.top
	if s == nil {
		s = &step{position: position{node: x.root, schema: x.selectFor(x.start, x.root)}}
		if x.root != nil {
			s.pos = x.root.Pos
		}
		x.markSecret(&s.position)
		x.top = s
	}
	for _, tok := range loc {
		s = x.step(s, tok)
	}
	return s.position
}

// step returns the step below s at location token tok.
func (x *memo) step(s *step, tok string) *step {
	if c, ok := s.find(tok); ok {
		return c
	}
	parent, parentSchema := s.node, s.schema
	var child *tree.Node
	var childSchema *schemaidx.Node
	var elem diag.PathElem
	switch {
	case parent != nil && parent.Kind == tree.KindList:
		i, err := strconv.Atoi(tok)
		if err != nil {
			elem = diag.Field(tok)
		} else {
			if i >= 0 && i < len(parent.Items) {
				child = parent.Items[i]
			}
			elem = x.itemElem(parentSchema, child, i)
		}
		childSchema, _ = parentSchema.Items()
	default:
		child, _ = x.get(parent, tok)
		elem = diag.Field(tok)
		childSchema = memberSchema(parentSchema, tok)
	}
	c := &step{position: s.position, tok: tok}
	c.pn = pathNode{parent: s.path, elem: elem}
	c.pn.text = x.intern(s.path.textOf(), elem, &c.tn)
	c.path = &c.pn
	c.node = child
	c.schema = x.selectFor(childSchema, child)
	if child != nil && child.Pos.Known() {
		c.pos = child.Pos
	}
	x.markSecret(&c.position)
	s.add(c)
	return c
}

// markSecret records p as the first x-ruralz-secret position on its path
// when its schema is the first secret one.
func (x *memo) markSecret(p *position) {
	if p.secret != nil || !x.secret(p.schema) {
		return
	}
	s := *p
	s.secret = nil
	p.secret = &s
}
