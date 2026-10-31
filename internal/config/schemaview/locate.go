// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"encoding/json"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// position is where a validation error's instance location lies in the
// resource tree.
type position struct {
	// path is the key-aware path from the resource root; nil at the
	// root.
	path *pathNode
	// node is the instance node; nil when the tree lacks the location.
	node *tree.Node
	// pos is the nearest known source position at or above node: a
	// defaulted node points at its parent.
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
// lie at or below it while the step is held), the member index of large
// objects, the secret mark of each schema node, the interned path texts
// and the common prefixes of long path segments. Mapping work is then
// linear in the number of errors times their location depth plus the
// instance size, up to the logarithmic factor of sorting the diagnostics.
// lcp holds the pairs of long sibling segments compared whose common
// prefix reaches lcpMemo, at most scanned/lcpMemo of them; every other
// long pair is remembered only as each segment's last comparison
// (lcpCache), so the memo's size does not grow with the comparisons
// (TestLongSiblingPrefixes).
//
// Steps and interned texts are not kept for every error: a sweep (sweep.go)
// drops those that no diagnostic the mapper holds needs and that were
// cheap to resolve, whenever the memo has grown by more than it kept, so
// what the memo holds does not grow with the errors that the cap drops;
// keeping one step per error would cost about 1 KB per error whatever the
// cap.
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
	// dispatches, path elements and step lookups read: the work the scale
	// tests bound, linear in the instance size.
	read int
	// scanned counts the bytes that the common prefixes of two long
	// sibling segments read (commonPrefix), which the scale tests bound
	// per document byte.
	scanned int
	// work counts the call's work units for Options.Yield; nil yields
	// nothing.
	work *work
	// roots, set by the mapper, marks the paths it holds (mapper.roots),
	// which a sweep keeps with everything above them.
	roots func(mark func(*pathNode))
	// grown counts the steps and interned texts added since the last
	// sweep and kept those the last sweep kept; peak is the most the memo
	// held at once and sweeps the number of sweeps, which the memory tests
	// bound and count. created numbers the steps (step.seq).
	grown, kept, peak, sweeps, created int
	// minSweep is the least growth between two sweeps, sweepMin when zero;
	// tests lower it to sweep at nearly every step and text.
	minSweep int
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
	// costly marks a step whose resolution read at least keepCost
	// members, elements and bytes (memo.step): every sweep keeps it, so a
	// large object's dispatch or a large element's key-aware text is
	// computed once however many errors pass it.
	costly bool
	// seq numbers the steps in the order they were added, the order a
	// sweep keeps them in.
	seq int
}

// fewSteps is the child count up to which a step scans its children
// instead of indexing them: most positions have a handful.
const fewSteps = 8

// find returns the child step at tok, the children it compared and the
// bytes it read to compare them: for each entry of few it scanned, the
// token's bytes when the two lengths are equal (strings of different
// lengths differ without a byte read), or for the lookup in many one
// child and the token's bytes, which it hashes. A step with many children
// must not be scanned per error: that is quadratic in its children, so
// compared counts every child a scan reads and a lost index shows in the
// work counts. The bytes must be counted per child too: eight
// equal-length siblings of 64 KiB each read 512 KiB per lookup, not the
// token's length once.
func (s *step) find(tok string) (c *step, compared, bytes int, ok bool) {
	if s.many != nil {
		c, ok := s.many[tok]
		return c, 1, len(tok), ok
	}
	for i, c := range s.few {
		if len(c.tok) != len(tok) {
			continue
		}
		bytes += len(tok)
		if c.tok == tok {
			return c, i + 1, bytes, true
		}
	}
	return nil, len(s.few), bytes, false
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
// whose tree is root, counting work units on w (nil counts nothing).
func newMemo(start *schemaidx.Node, root *tree.Node, w *work) *memo {
	return &memo{start: start, root: root, work: w}
}

// index returns the position in n.Members of the first member named key,
// the member tree.Node.Get returns; false when n is not an object or has
// no such member. Each member scanned or indexed is a work unit, and so
// is each byte of a name it compares in full (one as long as key),
// hashes into the index or looks up in it: a member name can be as long
// as the document.
func (x *memo) index(n *tree.Node, key string) (int, bool) {
	if n == nil || n.Kind != tree.KindMap {
		return 0, false
	}
	if len(n.Members) < memberIndexMin {
		for i := range n.Members {
			x.read++
			x.work.tick()
			if k := n.Members[i].Key; len(k) == len(key) {
				x.work.charge(len(k))
				if k == key {
					return i, true
				}
			}
		}
		return 0, false
	}
	names, ok := x.members[n]
	if !ok {
		names = make(map[string]int, len(n.Members))
		for i := len(n.Members) - 1; i >= 0; i-- {
			x.work.charge(1 + len(n.Members[i].Key))
			names[n.Members[i].Key] = i
		}
		x.read += len(n.Members)
		if x.members == nil {
			x.members = map[*tree.Node]map[string]int{}
		}
		x.members[n] = names
	}
	x.work.charge(len(key))
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
// inst. Steps call it once per position. Select is a span that cannot
// yield (Options.Yield), charged one unit per member of inst after it,
// although each of its cases scans those members.
func (x *memo) selectFor(s *schemaidx.Node, inst *tree.Node) *schemaidx.Node {
	sel := s.Select(inst)
	if inst != nil && inst.Kind == tree.KindMap {
		x.read += len(inst.Members)
		x.work.charge(len(inst.Members))
	}
	return sel
}

// itemElem returns schemaidx.ItemElem(list, item, i). It reads the
// element (a keyed entry's members, a set element's canonical JSON or
// scalar text), which may be large; steps call it once per element.
// Computing the element is a span that cannot yield (Options.Yield); the
// element's members and items and its bytes (elemBytes) are charged after
// it.
func (x *memo) itemElem(list *schemaidx.Node, item *tree.Node, i int) diag.PathElem {
	e := schemaidx.ItemElem(list, item, i)
	if item != nil {
		k := 1 + len(item.Members) + len(item.Items)
		x.read += k
		x.work.charge(k + elemBytes(e, item))
	}
	return e
}

// elemBytes returns the bytes schemaidx.ItemElem reads or writes to name
// item as e: a set element's canonical JSON (an object or array) with the
// numbers it parses (itemJSONBytes), or the text of a number set element,
// which it parses and formats; 0 for every other element, whose text it
// neither reads nor copies. A string set element and a keyed entry's key
// are read when the path is rendered (memo.intern charges them).
func elemBytes(e diag.PathElem, item *tree.Node) int {
	if e.Kind != diag.ElemItem {
		return 0
	}
	if raw, ok := e.Item.(json.RawMessage); ok {
		return itemJSONBytes(item, len(raw))
	}
	return numberBytes(item)
}

// itemJSONBytes returns the bytes schemaidx.AppendItemJSON reads and
// writes for n, whose canonical JSON is canon bytes long: those bytes and
// the text of every number in n, which it parses into a form that can be
// much shorter than the literal.
func itemJSONBytes(n *tree.Node, canon int) int {
	return canon + numberBytes(n)
}

// numberBytes returns the bytes of the number literals at and below n.
func numberBytes(n *tree.Node) int {
	if n == nil {
		return 0
	}
	b := 0
	if n.Kind == tree.KindInt || n.Kind == tree.KindFloat {
		b = len(n.Text)
	}
	for _, m := range n.Members {
		b += numberBytes(m.Value)
	}
	for _, it := range n.Items {
		b += numberBytes(it)
	}
	return b
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

// step returns the step below s at location token tok. The lookup reads
// one member per child it compares (step.find) and the bytes it compares
// or hashes, each a work unit, so a location with a long member name
// charges its length per lookup and per equal-length sibling scanned.
// Resolving a new step reads tok once more (memberSchema hashes it into
// the schema's members, or strconv.Atoi parses a list index), charged
// with its bytes. A step whose resolution read at least keepCost
// members, elements and bytes is marked costly, which sweeps keep.
func (x *memo) step(s *step, tok string) *step {
	c, compared, bytes, ok := s.find(tok)
	x.read += compared
	x.work.charge(compared + bytes)
	if ok {
		return c
	}
	x.sweepIfGrown(s.path)
	x.work.tick()
	readBefore, bytes := x.read, len(tok)
	x.work.charge(len(tok))
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
			bytes += elemBytes(elem, child)
		}
		childSchema, _ = parentSchema.Items()
	default:
		child, _ = x.get(parent, tok)
		elem = diag.Field(tok)
		childSchema = memberSchema(parentSchema, tok)
	}
	c = &step{position: s.position, tok: tok, seq: x.created}
	x.created++
	c.pn = pathNode{parent: s.path, elem: elem}
	c.pn.text = x.intern(s.path.textOf(), elem, &c.tn)
	c.path = &c.pn
	c.node = child
	c.schema = x.selectFor(childSchema, child)
	if child != nil && child.Pos.Known() {
		c.pos = child.Pos
	}
	c.costly = x.read-readBefore+bytes+len(c.pn.text.text) >= keepCost
	x.markSecret(&c.position)
	s.add(c)
	x.grow()
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
