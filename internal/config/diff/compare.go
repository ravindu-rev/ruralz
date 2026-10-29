// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// conversionData is the annotation that carries newer-only fields through
// a down-conversion. The canonical form leaves it out, so the diff does.
const conversionData = "ruralz.io/conversion-data"

// comparer compares the resources of one worker. It is owned by one
// goroutine; its caches hold schema nodes, which are immutable, and the
// class of each Policy it resolved.
type comparer struct {
	idx *schemaidx.Index
	reg *registry.Registry
	// bundles are FROM's and TO's resources, which resolve the Policies
	// attachments name.
	bundles [2]*hub.Bundle
	// since reports a schema level above 0, where since members at their
	// defaults are left out of the canonical form.
	since    bool
	kws      map[*schemaidx.Node]keywords
	defaults map[*schemaidx.Node]*tree.Node
	classes  map[policyKey]schemaidx.Impact

	// The resource being compared.
	id       hub.ID
	ops      []Op
	path     diag.Path
	scanPath diag.Path
	err      error
}

// newComparer returns a comparer over the hub schema idx and the Policy
// type registry reg, for the states from and to.
func newComparer(idx *schemaidx.Index, reg *registry.Registry, from, to *hub.Bundle) *comparer {
	return &comparer{
		idx: idx, reg: reg, bundles: [2]*hub.Bundle{from, to}, since: idx.Level() > 0,
		kws: map[*schemaidx.Node]keywords{}, defaults: map[*schemaidx.Node]*tree.Node{},
		classes: map[policyKey]schemaidx.Impact{},
	}
}

// pair compares one identity; it returns nil when the resource did not
// change.
func (c *comparer) pair(p pair) (*Resource, error) {
	switch {
	case p.from == nil:
		return c.whole(p.to, toSide, Added)
	case p.to == nil:
		return c.whole(p.from, fromSide, Removed)
	default:
		return c.modified(p.from, p.to)
	}
}

// rootPos returns the place of a resource's root on side s: its envelope
// schema with the kind dispatch applied.
func (c *comparer) rootPos(r *hub.Resource, s side) pos {
	static, _ := c.idx.Resource(string(r.Kind))
	return c.step(pos{side: s}, static, false, r.Tree)
}

// whole describes a resource only side s holds: identity only, with the
// impact classes of every field it holds, the Policies it attaches
// included.
func (c *comparer) whole(r *hub.Resource, s side, change Change) (*Resource, error) {
	c.id, c.path, c.scanPath = r.ID, c.path[:0], c.scanPath[:0]
	impact, err := c.scan(envelope(r.Tree), c.rootPos(r, s), false)
	if err != nil {
		return nil, err
	}
	// Adding or removing a Policy or Plugin adds or removes the control or
	// code it is.
	impact |= c.resourceImpact(r)
	out := &Resource{ID: r.ID, Change: change, Source: r.Source.File, Impact: impact}
	if change == Added && r.Kind == v1alpha1.KindPlugin {
		out.Flags = FlagCapabilityGrant
	}
	return out, nil
}

// modified compares a resource both sides hold; it returns nil when their
// canonical forms are equal.
func (c *comparer) modified(a, b *hub.Resource) (*Resource, error) {
	c.id, c.ops, c.path, c.scanPath, c.err = a.ID, c.ops[:0], c.path[:0], c.scanPath[:0], nil
	c.object(envelope(a.Tree), envelope(b.Tree), c.rootPos(a, fromSide), c.rootPos(b, toSide))
	if c.err != nil {
		return nil, c.err
	}
	if len(c.ops) == 0 {
		return nil, nil
	}
	ops := slices.Clone(c.ops)
	slices.SortStableFunc(ops, func(x, y Op) int { return cmp.Compare(x.Kind, y.Kind) })
	out := &Resource{ID: a.ID, Change: Modified, Source: cmp.Or(b.Source.File, a.Source.File), Ops: ops}
	fromType, toType := c.resourceImpact(a), c.resourceImpact(b)
	for i := range ops {
		if a.Kind == v1alpha1.KindPlugin && capabilityPath(ops[i].Path) {
			ops[i].Flags |= FlagCapabilityGrant
		}
		// A change to a Policy's or Plugin's spec changes the control or
		// code it is: an addition reads TO's class, a removal FROM's, any
		// other both.
		if underSpec(ops[i].Path) {
			switch ops[i].Kind {
			case OpAdd:
				ops[i].Impact |= toType
			case OpRemove:
				ops[i].Impact |= fromType
			default:
				ops[i].Impact |= fromType | toType
			}
		}
		out.Impact |= ops[i].Impact
		out.Flags |= ops[i].Flags
	}
	return out, nil
}

// capabilityPath reports a path at or below Plugin spec.capabilities or
// spec.image, whose changes are always flagged.
func capabilityPath(p diag.Path) bool {
	return underSpec(p) && len(p) >= 2 &&
		p[1].Kind == diag.ElemField && (p[1].Name == "capabilities" || p[1].Name == "image")
}

// underSpec reports a path at or below a resource's spec.
func underSpec(p diag.Path) bool {
	return len(p) >= 1 && p[0].Kind == diag.ElemField && p[0].Name == "spec"
}

// envelope returns the part of a resource tree the canonical form holds:
// kind, metadata without the conversion-data annotation, and spec; never
// apiVersion, so converting a Bundle yields an empty diff.
func envelope(root *tree.Node) *tree.Node {
	out := *root
	out.Members = make([]tree.Member, 0, len(root.Members))
	for _, m := range root.Members {
		switch m.Key {
		case "apiVersion":
			continue
		case "metadata":
			if m.Value != nil && m.Value.Kind == tree.KindMap {
				m.Value = stripConversionData(m.Value)
			}
		default:
		}
		out.Members = append(out.Members, m)
	}
	return &out
}

// stripConversionData returns meta without the conversion-data
// annotation, and without annotations when that was the only one, as the
// canonical encoder writes it.
func stripConversionData(meta *tree.Node) *tree.Node {
	ai := slices.IndexFunc(meta.Members, func(m tree.Member) bool { return m.Key == "annotations" })
	if ai < 0 {
		return meta
	}
	ann := meta.Members[ai].Value
	if ann == nil || ann.Kind != tree.KindMap {
		return meta
	}
	ci := slices.IndexFunc(ann.Members, func(m tree.Member) bool { return m.Key == conversionData })
	if ci < 0 {
		return meta
	}
	out := *meta
	if len(ann.Members) == 1 {
		out.Members = slices.Delete(slices.Clone(meta.Members), ai, ai+1)
		return &out
	}
	stripped := *ann
	stripped.Members = slices.Delete(slices.Clone(ann.Members), ci, ci+1)
	out.Members = slices.Clone(meta.Members)
	out.Members[ai].Value = &stripped
	return &out
}

// value compares two values at the current path.
func (c *comparer) value(a, b *tree.Node, pa, pb pos) {
	switch {
	case a.Kind == tree.KindMap && b.Kind == tree.KindMap:
		c.object(a, b, pa, pb)
	case a.Kind == tree.KindList && b.Kind == tree.KindList:
		c.list(a, b, pa, pb)
	case scalarEqual(a, b):
	default:
		c.replace(a, b, pa, pb)
	}
}

// object compares two objects member by member, in RFC 8785 order.
func (c *comparer) object(a, b *tree.Node, pa, pb pos) {
	union(a, b, func(name string, va, vb *tree.Node) bool {
		var ma, mb pos
		if va != nil {
			ma = c.member(pa, name, va)
			if c.omitted(ma.static, va) {
				va = nil
			}
		}
		if vb != nil {
			mb = c.member(pb, name, vb)
			if c.omitted(mb.static, vb) {
				vb = nil
			}
		}
		c.path = append(c.path, diag.Field(name))
		switch {
		case va != nil && vb != nil:
			c.value(va, vb, ma, mb)
		case va != nil:
			c.remove(va, ma)
		case vb != nil:
			c.add(vb, mb)
		}
		c.path = c.path[:len(c.path)-1]
		return c.err == nil
	})
}

// list compares two lists by their x-ruralz-list type: map and orderedMap
// by key, set by element, anything else (atomic, or no type in schemaless
// content) whole. Lists whose types differ between the sides compare
// whole.
func (c *comparer) list(a, b *tree.Node, pa, pb pos) {
	lt, key := pa.s.List()
	ltb, keyb := pb.s.List()
	switch {
	case lt != ltb || key != keyb:
	case lt.Keyed():
		if c.keyed(a, b, pa, pb, lt == schemaidx.ListOrderedMap, key) {
			return
		}
	case lt == schemaidx.ListSet:
		c.set(a, b, pa, pb)
		return
	default:
	}
	if !c.equal(a, b, pa.static, pb.static) {
		c.replace(a, b, pa, pb)
	}
}

// keysOf returns the key of every entry of a keyed list, and false when an
// entry has no scalar key or two entries share one; such a list compares
// whole. A number key takes its RFC 8785 text, as the canonical form
// writes it.
func keysOf(l *tree.Node, field string) ([]string, map[string]int, bool) {
	keys := make([]string, len(l.Items))
	at := make(map[string]int, len(l.Items))
	for i, it := range l.Items {
		k, ok := it.Get(field)
		if !ok {
			return nil, nil, false
		}
		switch k.Kind {
		case tree.KindString:
			keys[i] = k.Text
		case tree.KindInt, tree.KindFloat:
			keys[i] = canonicalNumber(k.Text)
		default:
			return nil, nil, false
		}
		if _, dup := at[keys[i]]; dup {
			return nil, nil, false
		}
		at[keys[i]] = i
	}
	return keys, at, true
}

// keyed compares map and orderedMap lists by key: an entry only FROM holds
// is removed, one only TO holds is added, and a shared entry compares
// field by field. In an orderedMap a shared entry whose position changed
// also moves. It returns false, having done nothing, when a list has no
// usable keys.
func (c *comparer) keyed(a, b *tree.Node, pa, pb pos, ordered bool, field string) bool {
	ka, atA, okA := keysOf(a, field)
	kb, atB, okB := keysOf(b, field)
	if !okA || !okB {
		return false
	}
	for i, k := range ka {
		if _, ok := atB[k]; !ok {
			c.path = append(c.path, diag.Keyed(field, k))
			c.remove(a.Items[i], c.item(pa, a.Items[i]))
			c.path = c.path[:len(c.path)-1]
		}
	}
	for j, k := range kb {
		c.path = append(c.path, diag.Keyed(field, k))
		if i, ok := atA[k]; ok {
			ea, eb := c.item(pa, a.Items[i]), c.item(pb, b.Items[j])
			if ordered && i != j {
				impact := ea.impact | eb.impact | c.refImpact(a.Items[i], ea) | c.refImpact(b.Items[j], eb)
				c.ops = append(c.ops, Op{Kind: OpMove, Path: slices.Clone(c.path), FromIndex: i, ToIndex: j, Impact: impact})
			}
			c.value(a.Items[i], b.Items[j], ea, eb)
		} else {
			c.add(b.Items[j], c.item(pb, b.Items[j]))
		}
		c.path = c.path[:len(c.path)-1]
		if c.err != nil {
			return true
		}
	}
	return true
}

// element is one set element in canonical form, keyed by its RFC 8785
// bytes.
type element struct {
	key  string
	view *tree.Node
	at   pos
}

// elements returns the elements of a set list at p.
func (c *comparer) elements(l *tree.Node, p pos) []element {
	out := make([]element, len(l.Items))
	for i, it := range l.Items {
		at := c.item(p, it)
		v := c.view(it, at.static)
		out[i] = element{key: string(schemaidx.AppendItemJSON(nil, v)), view: v, at: at}
	}
	return out
}

// set compares set lists by element: reordering is never a change, an
// element only FROM holds is removed and one only TO holds is added.
// Equal elements count, so a list holding one twice compares as a
// multiset.
func (c *comparer) set(a, b *tree.Node, pa, pb pos) {
	ea, eb := c.elements(a, pa), c.elements(b, pb)
	countA, countB := map[string]int{}, map[string]int{}
	for _, e := range ea {
		countA[e.key]++
	}
	for _, e := range eb {
		countB[e.key]++
	}
	for i, e := range ea {
		if countB[e.key] > 0 {
			countB[e.key]--
			continue
		}
		c.path = append(c.path, schemaidx.ItemElem(pa.s, e.view, i))
		c.remove(e.view, e.at)
		c.path = c.path[:len(c.path)-1]
	}
	for j, e := range eb {
		if countA[e.key] > 0 {
			countA[e.key]--
			continue
		}
		c.path = append(c.path, schemaidx.ItemElem(pb.s, e.view, j))
		c.add(e.view, e.at)
		c.path = c.path[:len(c.path)-1]
	}
}

// show returns v in canonical form and its impact classes for an
// operation, refusing a non-reference at a secret position.
func (c *comparer) show(v *tree.Node, p pos) (*tree.Node, schemaidx.Impact, bool) {
	if c.err != nil {
		return nil, 0, false
	}
	c.scanPath = c.scanPath[:0]
	impact, err := c.scan(v, p, true)
	if err != nil {
		c.err = err
		return nil, 0, false
	}
	view := c.view(v, p.static)
	// Both output forms write the value in canonical form, so it must
	// have one; every tree stage G hands over does.
	if _, err := canonical.AppendNode(nil, view); err != nil {
		c.err = fmt.Errorf("%w: %s %s: %w", ErrInput, c.id, c.path, err)
		return nil, 0, false
	}
	return view, impact, true
}

// add records the addition of v at the current path.
func (c *comparer) add(v *tree.Node, p pos) {
	if view, impact, ok := c.show(v, p); ok {
		c.ops = append(c.ops, Op{Kind: OpAdd, Path: slices.Clone(c.path), To: view, Impact: impact})
	}
}

// remove records the removal of v at the current path.
func (c *comparer) remove(v *tree.Node, p pos) {
	if view, impact, ok := c.show(v, p); ok {
		c.ops = append(c.ops, Op{Kind: OpRemove, Path: slices.Clone(c.path), From: view, Impact: impact})
	}
}

// replace records the replacement of a by b at the current path.
func (c *comparer) replace(a, b *tree.Node, pa, pb pos) {
	va, ia, ok := c.show(a, pa)
	if !ok {
		return
	}
	vb, ib, ok := c.show(b, pb)
	if !ok {
		return
	}
	c.ops = append(c.ops, Op{Kind: OpReplace, Path: slices.Clone(c.path), From: va, To: vb, Impact: ia | ib})
}
