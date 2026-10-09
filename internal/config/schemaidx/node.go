// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaidx

import (
	"encoding/json"
	"iter"
	"maps"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Node is one resolved schema position: the conjunction of a schema
// object, its $ref chain and its allOf members, with keywords merged (the
// nearest declaration wins, so a property's own default and annotations
// take precedence over its definition's). if/then/else dispatch stays
// pending until Select applies it to an instance object. A Node is
// immutable; every method is safe on a nil Node and returns zero values.
type Node struct {
	idx *Index
	// parts are the conjunctive schema objects, nearest first.
	parts []*raw
	// decided are the if owners whose branch was already applied.
	decided []*raw

	def      string
	scalar   Scalar
	types    TypeSet
	values   []any
	hasEnum  bool
	required []string
	names    []string
	props    map[string]*Node
	items    *Node
	addl     *Node
	closed   bool
	dflt     any
	hasDflt  bool
	kw       Keywords
	cases    []*raw
	// variants holds the precomputed dispatch results, keyed by the mask
	// of matching cases ('1' matched, '0' not).
	variants map[string]*Node
}

// builder resolves raw parts into Nodes. At Load it writes the shared memo
// and precomputes dispatch variants; at run time (a combination of matching
// cases Load did not precompute) it reads the shared memo and writes only a
// local one, so Index stays immutable.
type builder struct {
	idx        *Index
	shared     map[string]*Node
	local      map[string]*Node
	precompute bool
	// built counts the Nodes this builder made; past limit it returns
	// empty placeholders and sets overflow, so an adversarial schema whose
	// dispatch combinations grow exponentially fails Load instead of
	// running away.
	built    int
	limit    int
	overflow bool
}

// maxNodes bounds the Nodes one builder makes: about 100 times what the
// committed ruralz/v1alpha1 schema needs.
const maxNodes = 1 << 16

// expand returns parts with every $ref target and allOf member, nearest
// first, without duplicates.
func expand(parts []*raw) []*raw {
	var out []*raw
	seen := map[*raw]bool{}
	var add func(r *raw)
	add = func(r *raw) {
		if r == nil || seen[r] {
			return
		}
		seen[r] = true
		out = append(out, r)
		add(r.ref)
		for _, a := range r.allOf {
			add(a)
		}
	}
	for _, p := range parts {
		add(p)
	}
	return out
}

func nodeKey(parts, decided []*raw) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(strconv.Itoa(p.id))
		b.WriteByte(',')
	}
	b.WriteByte('|')
	ids := make([]int, len(decided))
	for i, d := range decided {
		ids[i] = d.id
	}
	slices.Sort(ids)
	for _, id := range ids {
		b.WriteString(strconv.Itoa(id))
		b.WriteByte(',')
	}
	return b.String()
}

// node returns the Node of the conjunction of parts, with the if/then
// rules owned by decided already applied.
func (b *builder) node(parts, decided []*raw) *Node {
	exp := expand(parts)
	if len(exp) == 0 {
		return nil
	}
	key := nodeKey(exp, decided)
	if n, ok := b.shared[key]; ok {
		return n
	}
	if n, ok := b.local[key]; ok {
		return n
	}
	n := &Node{idx: b.idx, parts: exp, decided: decided}
	if b.built++; b.built > b.limit {
		b.overflow = true
		return n
	}
	if b.local != nil {
		b.local[key] = n
	} else {
		b.shared[key] = n
	}
	b.fill(n)
	return n
}

func (b *builder) fill(n *Node) {
	n.types = AllTypes
	props := map[string][]*raw{}
	var items, addl []*raw
	for _, p := range n.parts {
		if n.def == "" {
			n.def = p.def
		}
		if s := scalarOf(p.def); s != ScalarNone && n.scalar == ScalarNone {
			n.scalar = s
		}
		n.types &= p.eff
		if p.hasValues {
			if !n.hasEnum {
				n.values, n.hasEnum = slices.Clone(p.values), true
			} else {
				n.values = slices.DeleteFunc(n.values, func(v any) bool {
					return !slices.ContainsFunc(p.values, func(w any) bool { return jsonEqual(v, w) })
				})
			}
		}
		for _, r := range p.required {
			if !slices.Contains(n.required, r) {
				n.required = append(n.required, r)
			}
		}
		for name, pr := range p.props {
			props[name] = append(props[name], pr)
		}
		if p.items != nil {
			items = append(items, p.items)
		}
		if p.addl != nil {
			addl = append(addl, p.addl)
		}
		n.closed = n.closed || p.addlFalse
		if p.hasDflt && !n.hasDflt {
			n.dflt, n.hasDflt = p.dflt, true
		}
		mergeKeywords(&n.kw, p.kw)
		if p.ifc != nil && !slices.Contains(n.decided, p) {
			n.cases = append(n.cases, p)
		}
	}
	slices.Sort(n.required)
	n.names = slices.Sorted(maps.Keys(props))
	if len(n.names) > 0 {
		n.props = make(map[string]*Node, len(n.names))
		for _, name := range n.names {
			n.props[name] = b.node(props[name], nil)
		}
	}
	if len(items) > 0 {
		n.items = b.node(items, nil)
	}
	if len(addl) > 0 {
		n.addl = b.node(addl, nil)
	}
	if b.precompute && len(n.cases) > 0 {
		n.variants = map[string]*Node{}
		mask := make([]byte, len(n.cases))
		for i := range mask {
			mask[i] = '0'
		}
		if v := b.variant(n, mask); v != n {
			n.variants[string(mask)] = v
		}
		for i := range n.cases {
			mask[i] = '1'
			if v := b.variant(n, mask); v != n {
				n.variants[string(mask)] = v
			}
			mask[i] = '0'
		}
	}
}

// mergeKeywords adds the keywords of a farther part: the nearest list,
// reference, CEL spec and since level win; secret, impact and validations
// accumulate.
func mergeKeywords(dst *Keywords, k Keywords) {
	if dst.List == ListNone {
		dst.List, dst.ListKey = k.List, k.ListKey
	}
	if dst.Ref == "" {
		dst.Ref = k.Ref
	}
	dst.Secret = dst.Secret || k.Secret
	if dst.CEL == nil {
		dst.CEL = k.CEL
	}
	dst.Impact |= k.Impact
	if dst.Since == 0 {
		dst.Since = k.Since
	}
	dst.Validations = append(dst.Validations, k.Validations...)
}

// branches returns the branches n's pending cases contribute by mask: a
// matched case its then branch, an unmatched case its else branch.
func (n *Node) branches(mask []byte) []*raw {
	var out []*raw
	for i, c := range n.cases {
		br := c.els
		if mask[i] == '1' {
			br = c.then
		}
		if br != nil {
			out = append(out, br)
		}
	}
	return out
}

// variant applies n's pending cases by mask. It returns n itself when no
// branch applies.
func (b *builder) variant(n *Node, mask []byte) *Node {
	branches := n.branches(mask)
	if len(branches) == 0 {
		return n
	}
	decided := slices.Concat(n.decided, n.cases)
	return b.node(slices.Concat(n.parts, branches), decided)
}

// runtimeVariant builds a dispatch result Load did not precompute (two or
// more rules matching at once), reading the shared memo and writing a
// local one.
func (n *Node) runtimeVariant(mask []byte) *Node {
	if len(n.branches(mask)) == 0 {
		return n
	}
	b := &builder{idx: n.idx, shared: n.idx.memo, local: map[string]*Node{}, limit: maxNodes}
	return b.variant(n, mask)
}

// maxSelectRounds bounds nested dispatch (a then branch carrying its own
// if); the generator emits one level.
const maxSelectRounds = 8

// Select applies the node's if/then/else dispatch to the instance object
// inst (for example PolicySpec.config by inst's type member, or the root by
// kind). A nil or non-object inst, or no pending dispatch, returns n.
func (n *Node) Select(inst *tree.Node) *Node {
	cur := n
	for range maxSelectRounds {
		if cur == nil || len(cur.cases) == 0 || inst == nil || inst.Kind != tree.KindMap {
			return cur
		}
		var buf [32]byte // PolicySpec has 23 rules; more spill to the heap
		mask := cur.mask(inst, buf[:0])
		next, ok := cur.variants[string(mask)]
		if !ok {
			next = cur.runtimeVariant(mask)
		}
		if next == cur {
			return cur
		}
		cur = next
	}
	return cur
}

// mask appends to dst the evaluation of n's pending cases on the instance
// object: '1' for a matching case, '0' otherwise.
func (n *Node) mask(inst *tree.Node, dst []byte) []byte {
	for _, c := range n.cases {
		b := byte('0')
		if c.ifc.eval(inst) {
			b = '1'
		}
		dst = append(dst, b)
	}
	return dst
}

// SelectValue applies the dispatch as if the instance object had exactly
// one member, member, with the string value.
func (n *Node) SelectValue(member, value string) *Node {
	if n == nil || len(n.cases) == 0 {
		return n
	}
	return n.Select(stringMember(member, value))
}

// stringMember returns the object {member: value}.
func stringMember(member, value string) *tree.Node {
	return &tree.Node{Kind: tree.KindMap, Members: []tree.Member{
		{Key: member, Value: &tree.Node{Kind: tree.KindString, Text: value}},
	}}
}

// eval reports whether the instance object satisfies the condition. A
// const constrains only a present member ("properties" semantics);
// required members must be present.
func (c *cond) eval(inst *tree.Node) bool {
	for _, r := range c.required {
		if _, ok := inst.Get(r); !ok {
			return false
		}
	}
	for _, k := range c.consts {
		if m, ok := inst.Get(k.Member); ok && !constEqual(k.Value, m) {
			return false
		}
	}
	return true
}

// constEqual compares a scalar const with an instance node; numbers
// compare by value.
func constEqual(c any, n *tree.Node) bool {
	switch v := c.(type) {
	case nil:
		return n.Kind == tree.KindNull
	case string:
		return n.Kind == tree.KindString && n.Text == v
	case bool:
		return n.Kind == tree.KindBool && n.Bool == v
	case json.Number:
		return (n.Kind == tree.KindInt || n.Kind == tree.KindFloat) && numberEqual(n.Text, string(v))
	default:
		return false
	}
}

// jsonEqual compares JSON values decoded with UseNumber; numbers compare by
// value.
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case json.Number:
		y, ok := b.(json.Number)
		return ok && numberEqual(string(x), string(y))
	case []any:
		y, ok := b.([]any)
		return ok && slices.EqualFunc(x, y, jsonEqual)
	case map[string]any:
		y, ok := b.(map[string]any)
		return ok && maps.EqualFunc(x, y, jsonEqual)
	default:
		return false
	}
}

func numberEqual(a, b string) bool {
	if a == b {
		return true
	}
	x, okA := new(big.Rat).SetString(a)
	y, okB := new(big.Rat).SetString(b)
	return okA && okB && x.Cmp(y) == 0
}

// Def returns the nearest $defs name of the node, such as "Limits" or
// "Duration"; "" for an inline schema.
func (n *Node) Def() string {
	if n == nil {
		return ""
	}
	return n.def
}

// Scalar returns the scalar definition the node's values normalize by
// (02 req 15; 01 req 29 re-types ByteSize and IntOrString).
func (n *Node) Scalar() Scalar {
	if n == nil {
		return ScalarNone
	}
	return n.scalar
}

// Types returns the instance types the node admits (anyOf and oneOf
// alternatives united, allOf parts intersected); 0 for a nil Node.
func (n *Node) Types() TypeSet {
	if n == nil {
		return 0
	}
	return n.types
}

// Enum returns the string values a const or enum restricts the node to,
// for nearest-value hints; nil when unrestricted.
func (n *Node) Enum() []string {
	if n == nil || !n.hasEnum {
		return nil
	}
	out := make([]string, 0, len(n.values))
	for _, v := range n.values {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Required returns the required member names, sorted.
func (n *Node) Required() []string {
	if n == nil {
		return nil
	}
	return slices.Clone(n.required)
}

// Property returns the schema of the declared member name.
func (n *Node) Property(name string) (*Node, bool) {
	if n == nil {
		return nil, false
	}
	p, ok := n.props[name]
	return p, ok
}

// Properties iterates the declared members in name order.
func (n *Node) Properties() iter.Seq2[string, *Node] {
	return func(yield func(string, *Node) bool) {
		if n == nil {
			return
		}
		for _, name := range n.names {
			if !yield(name, n.props[name]) {
				return
			}
		}
	}
}

// PropertyNames returns the declared member names, sorted (for
// nearest-name hints and enumeration).
func (n *Node) PropertyNames() []string {
	if n == nil {
		return nil
	}
	return slices.Clone(n.names)
}

// Defaults iterates the declared members that carry a default, in name
// order (01 req 36, 02 req 9).
func (n *Node) Defaults() iter.Seq2[string, *Node] {
	return func(yield func(string, *Node) bool) {
		for name, p := range n.Properties() {
			if p.hasDflt && !yield(name, p) {
				return
			}
		}
	}
}

// Items returns the schema of array elements.
func (n *Node) Items() (*Node, bool) {
	if n == nil || n.items == nil {
		return nil, false
	}
	return n.items, true
}

// Values returns the additionalProperties schema of a typed map (labels,
// annotations, variables).
func (n *Node) Values() (*Node, bool) {
	if n == nil || n.addl == nil {
		return nil, false
	}
	return n.addl, true
}

// Closed reports additionalProperties: false: undeclared members are
// unknown fields (RZ-CFG-006).
func (n *Node) Closed() bool { return n != nil && n.closed }

// Open reports an object that accepts undeclared members with no schema
// (+ruralz:open configs, free-form objects such as Plugin configSchema):
// they are preserved and normalized generically (02 req 18).
func (n *Node) Open() bool {
	return n != nil && n.types.Has(TypeObject) && !n.closed && n.addl == nil
}

// Keywords returns the node's x-ruralz-* annotations (a copy).
func (n *Node) Keywords() Keywords {
	if n == nil {
		return Keywords{}
	}
	return n.kw.clone()
}

// List returns the x-ruralz-list type and key field.
func (n *Node) List() (ListType, string) {
	if n == nil {
		return ListNone, ""
	}
	return n.kw.List, n.kw.ListKey
}

// Since returns the x-ruralz-since level; 0 for level 0.
func (n *Node) Since() int {
	if n == nil {
		return 0
	}
	return n.kw.Since
}

// Default returns a copy of the default value, in the encoding/json model
// with json.Number numbers.
func (n *Node) Default() (any, bool) {
	if n == nil || !n.hasDflt {
		return nil, false
	}
	return copyJSON(n.dflt), true
}

// DefaultNode returns the default as a fresh tree node styled
// tree.StyleDefaulted with an unknown position; nil without a default. A
// number at an integer-only position is integer text (01 req 37), also
// inside an object or array default: members and elements take their own
// schema, with dispatch applied to the default's objects.
func (n *Node) DefaultNode() *tree.Node {
	if n == nil || !n.hasDflt {
		return nil
	}
	out := toTree(n.dflt)
	normalizeIntegers(out, n)
	return out
}

// integerOnly reports a position whose numbers must be integers.
func (n *Node) integerOnly() bool {
	return n.Types().Has(TypeInteger) && !n.Types().Has(TypeNumber)
}

// normalizeIntegers rewrites the integral numbers of t at integer-only
// positions of the schema s as integer text (01 req 37), descending into
// members (dispatch applied) and elements. A position without schema is
// left as it is.
func normalizeIntegers(t *tree.Node, s *Node) {
	if t == nil || s == nil {
		return
	}
	switch t.Kind {
	case tree.KindFloat:
		if txt, ok := integerText(t.Text); ok && s.integerOnly() {
			t.Kind, t.Text = tree.KindInt, txt
		}
	case tree.KindMap:
		sel := s.Select(t)
		for _, m := range t.Members {
			child, _, _, _ := memberSchema(sel, false, m.Key)
			normalizeIntegers(m.Value, child)
		}
	case tree.KindList:
		if it, ok := s.Items(); ok {
			for _, e := range t.Items {
				normalizeIntegers(e, it)
			}
		}
	default:
	}
}

// DispatchValues returns the string const values that the node's pending
// if/then rules test member against, in schema order (the kinds at the
// root, the Policy types at PolicySpec).
func (n *Node) DispatchValues(member string) []string {
	if n == nil {
		return nil
	}
	var out []string
	for _, c := range n.cases {
		for _, k := range c.ifc.consts {
			if s, ok := k.Value.(string); ok && k.Member == member && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// PolicyConfig returns the config schema a PolicySpec node selects for
// policyType through its if/then dispatch on type (02 req 11); false when
// no rule matches.
func (n *Node) PolicyConfig(policyType string) (*Node, bool) {
	if !slices.Contains(n.DispatchValues("type"), policyType) {
		return nil, false
	}
	return n.SelectValue("type", policyType).Property("config")
}

func copyJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, e := range t {
			m[k] = copyJSON(e)
		}
		return m
	case []any:
		l := make([]any, len(t))
		for i, e := range t {
			l[i] = copyJSON(e)
		}
		return l
	default:
		return v
	}
}

// toTree converts a JSON value to a defaulted tree node; object members in
// name order, an integer literal as KindInt with its normalized decimal and
// any other number as KindFloat with its text.
func toTree(v any) *tree.Node {
	n := &tree.Node{Style: tree.StyleDefaulted}
	switch t := v.(type) {
	case nil:
		n.Kind = tree.KindNull
	case bool:
		n.Kind, n.Bool = tree.KindBool, t
	case string:
		n.Kind, n.Text = tree.KindString, t
	case json.Number:
		n.Kind, n.Text = tree.KindFloat, string(t)
		if s, ok := integerText(string(t)); ok && isIntegerLiteral(string(t)) {
			n.Kind, n.Text = tree.KindInt, s
		}
	case map[string]any:
		n.Kind = tree.KindMap
		for _, k := range slices.Sorted(maps.Keys(t)) {
			n.Members = append(n.Members, tree.Member{Key: k, Value: toTree(t[k])})
		}
	case []any:
		n.Kind = tree.KindList
		n.Items = make([]*tree.Node, len(t))
		for i, e := range t {
			n.Items[i] = toTree(e)
		}
	}
	return n
}

// integerText returns the normalized decimal of a number literal with an
// integral value ("1.0" and "1e3" give "1" and "1000").
func integerText(s string) (string, bool) {
	r, ok := new(big.Rat).SetString(s)
	if !ok || !r.IsInt() {
		return "", false
	}
	return r.Num().String(), true
}
