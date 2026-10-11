// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Normalize rewrites res.Root, a hub resource after Materialize and
// conversion, to its normalized form under idx, the hub schema (02 req 15
// to 20; 01 req 37), and returns its RZ-CFG-005 diagnostics in tree order:
//
//   - Scalars by definition name: a Duration becomes
//     time.ParseDuration(v).String() (90s is 1m30s; one that overflows
//     time.Duration is RZ-CFG-005); a ByteSize string becomes its integer
//     number of bytes (v1alpha1.ParseByteSize; 10Mi is 10485760); a
//     Decimal becomes its shortest equal decimal (3.00 is 3); an
//     IntOrString keeps its JSON type.
//
//   - An integral number at a position that admits only integers becomes
//     integer text (1.0 is 1, 1e3 is 1000).
//
//   - The host names of Route spec.match.hosts and Gateway
//     spec.listeners[].hostnames become ASCII lower case, as the Router
//     compares them.
//
//   - Lists by x-ruralz-list: set elements sorted by the bytes of their
//     RFC 8785 encoding, map entries by their key's string value;
//     orderedMap, atomic and unannotated lists keep authored order. An
//     equal element or key that normalization produced (90s and 1m30s in
//     one set) is RZ-CFG-005 at the later entry; stage F reported the
//     authored ones.
//
//   - Object members in RFC 8785 order (UTF-16 code units), so equal
//     resources have equal trees whatever their authored member order.
//
//   - Unless res comes from a tree.RoleCanonical file: every number must
//     keep its value through the canonical form's IEEE 754 doubles, so an
//     integer literal outside ±(2^53−1) and a number outside the double
//     range are RZ-CFG-005 (02 req 16, R-64), checked after scalar
//     normalization, which catches a ByteSize such as 8Pi.
//
//   - A null anywhere, typed or free, is RZ-CFG-005 at its key-aware path
//     (02 req 19): the hub and the canonical form have no null, so a
//     null left after the overlays is removed by its author, not carried
//     to the canonical encoder.
//
// Free content (undeclared members of open objects, the JSONSchemaDocument
// value) has its members sorted, its numbers checked and its nulls
// rejected, and nothing else. files gives source locations and the file
// role (nil: line and column only, and the range rule applies). A
// diagnostic's message never holds a value below an x-ruralz-secret
// field. Normalize returns at most MaxDiagnostics diagnostics.
func Normalize(idx *schemaidx.Index, res *tree.Resource, files *tree.FileTable) diag.List {
	ds, _ := normalize(idx, res, files, newWork(nil))
	return ds
}

// normalize is Normalize on worker w: diagnostics draw on w's budget, and
// failed reports an error even when the budget dropped it.
func normalize(idx *schemaidx.Index, res *tree.Resource, files *tree.FileTable, w *work) (ds diag.List, failed bool) {
	if idx == nil || res == nil || res.Root == nil {
		return nil, false
	}
	s, ok := idx.Resource(string(res.ID.Kind))
	if !ok {
		s = nil
	}
	z := &normalizer{res: res, files: files, checkRange: !canonical(files, res), w: w}
	z.stack = append(z.stack, step{node: res.Root})
	z.visit(res.Root, s)
	return z.diags, z.failed
}

// step is one position on the path from the resource root to the node
// being visited: an object member or a list element.
type step struct {
	// node is the value at this position.
	node *tree.Node
	// schema is its schema, dispatch applied for an object; nil when free.
	schema *schemaidx.Node
	// name is the member name of a member step.
	name string
	// item reports a list element; list is the list's schema and index
	// the element's position.
	item  bool
	list  *schemaidx.Node
	index int
}

// stack is the path of the node being visited; stack[0] is the root.
type stack []step

// path returns the key-aware path of the top of s. List element steps
// are named by schemaidx.ItemElem, computed only here, when a diagnostic
// needs them.
func (s stack) path() diag.Path {
	out := make(diag.Path, 0, len(s))
	for _, st := range s[min(1, len(s)):] {
		if st.item {
			out = append(out, schemaidx.ItemElem(st.list, st.node, st.index))
		} else {
			out = append(out, diag.Field(st.name))
		}
	}
	return out
}

// pos returns the position of the nearest step whose node has a known
// position, or fallback.
func (s stack) pos(fallback tree.Pos) tree.Pos {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i].node != nil && s[i].node.Pos.Known() {
			return s[i].node.Pos
		}
	}
	return fallback
}

// secret reports a path through an x-ruralz-secret field, whose values a
// message must not show.
func (s stack) secret() bool {
	for _, st := range s {
		if st.schema != nil && st.schema.Keywords().Secret {
			return true
		}
	}
	return false
}

type normalizer struct {
	res        *tree.Resource
	files      *tree.FileTable
	checkRange bool
	stack      stack
	diags      diag.List
	// failed reports an error, recorded or dropped by the budget.
	failed bool
	w      *work
}

func (z *normalizer) push(st step) { z.stack = append(z.stack, st) }

func (z *normalizer) pop() { z.stack = z.stack[:len(z.stack)-1] }

// visit normalizes n, whose schema is s (nil for free content), and below
// it; lists are sorted after their elements were normalized.
func (z *normalizer) visit(n *tree.Node, s *schemaidx.Node) {
	if n == nil {
		return
	}
	z.w.tick()
	switch n.Kind {
	case tree.KindMap:
		sel := s.Select(n)
		if sel.Def() == defJSONSchemaDocument {
			sel = nil
		}
		z.stack[len(z.stack)-1].schema = sel
		lowerHosts(n, sel)
		z.sortMembers(n)
		for i := range n.Members {
			m := n.Members[i]
			child := memberSchema(sel, m.Key)
			z.push(step{node: m.Value, schema: child, name: m.Key})
			z.visit(m.Value, child)
			z.pop()
		}
	case tree.KindList:
		it := itemSchema(s)
		for i, item := range n.Items {
			z.push(step{node: item, schema: it, item: true, list: s, index: i})
			z.visit(item, it)
			z.pop()
		}
		z.list(n, s)
	case tree.KindString:
		z.scalar(n, s)
	case tree.KindInt, tree.KindFloat:
		z.number(n, s)
	case tree.KindNull:
		z.null(n)
	default:
	}
}

// null reports a null value (02 req 19). Stage F admits one only in free
// content, such as an open config member or an enum of the
// JSONSchemaDocument value, but no null has a ruralz.canonical.v1 form.
func (z *normalizer) null(n *tree.Node) {
	what := "member"
	if z.stack[len(z.stack)-1].item {
		what = "element"
	}
	z.report(n, nil, "null is not allowed: the canonical form has no null; remove the "+what)
}

// report records an RZ-CFG-005 error at the top of the stack, extended by
// elem when set, located at node (or its nearest positioned ancestor).
func (z *normalizer) report(node *tree.Node, elem *diag.PathElem, msg string, related ...diag.Related) {
	z.failed = true
	if !z.w.take() {
		return
	}
	p := z.stack.path()
	if elem != nil {
		p = append(p, *elem)
	}
	at := z.stack.pos(z.res.Start)
	if node != nil && node.Pos.Known() {
		at = node.Pos
	}
	z.diags = append(z.diags, diag.Diagnostic{
		Code: CodeSchema, Severity: diag.SeverityError, Location: location(z.files, at),
		Resource: z.res.ID.ResourceID(), Path: p, Message: msg, Related: related,
	})
}

// shown returns text for a message: quoted and clipped, or a placeholder
// below a secret field.
func (z *normalizer) shown(text string) string {
	if z.stack.secret() {
		return "the value"
	}
	return strconv.Quote(clip(text))
}

// scalar normalizes a string by its schema definition (02 req 15).
func (z *normalizer) scalar(n *tree.Node, s *schemaidx.Node) {
	switch s.Scalar() {
	case schemaidx.ScalarDuration:
		d, err := time.ParseDuration(n.Text)
		switch {
		case err != nil && durationSyntax(n.Text):
			z.report(n, nil, fmt.Sprintf("duration %s is out of range: the largest is %s", z.shown(n.Text), time.Duration(1<<63-1)))
		case err != nil || d < 0:
			z.report(n, nil, fmt.Sprintf("invalid duration %s: want a Go duration such as 50ms", z.shown(n.Text)))
		default:
			n.Text = d.String()
		}
	case schemaidx.ScalarByteSize:
		b, err := v1alpha1.ParseByteSize(n.Text)
		if err != nil {
			z.report(n, nil, err.Error())
			return
		}
		n.Kind, n.Text = tree.KindInt, strconv.FormatInt(int64(b), 10)
		z.checkNumber(n)
	case schemaidx.ScalarDecimal:
		if d, ok := shortestDecimal(n.Text); ok {
			n.Text = d
		}
	default:
	}
}

// number writes an integral number at an integer-only position as integer
// text (01 req 37) and applies the number range rule (02 req 16, R-64).
func (z *normalizer) number(n *tree.Node, s *schemaidx.Node) {
	if n.Kind == tree.KindFloat && integerOnly(s) {
		text, ok, tooLarge := integerText(n.Text)
		switch {
		case tooLarge && z.checkRange:
			z.report(n, nil, fmt.Sprintf("integer %s is outside the I-JSON range -%d to %d", clip(n.Text), jsonval.MaxSafeInteger, jsonval.MaxSafeInteger))
			return
		case ok:
			n.Kind, n.Text = tree.KindInt, text
		default:
		}
	}
	z.checkNumber(n)
}

// checkNumber reports a number the canonical form would not keep (02 req
// 16): an integer literal outside ±(2^53−1), a number that overflows a
// double, or text outside the RFC 8259 grammar. Canonical content is
// exempt (R-64).
func (z *normalizer) checkNumber(n *tree.Node) {
	if !z.checkRange {
		return
	}
	err := jsonval.CheckNumber(json.Number(n.Text))
	switch {
	case err == nil:
	case errors.Is(err, jsonval.ErrNumberRange) && !strings.ContainsAny(n.Text, ".eE"):
		z.report(n, nil, fmt.Sprintf("integer %s is outside the I-JSON range -%d to %d", clip(n.Text), jsonval.MaxSafeInteger, jsonval.MaxSafeInteger))
	case errors.Is(err, jsonval.ErrNumberRange):
		z.report(n, nil, fmt.Sprintf("number %s is outside the range of an IEEE 754 double", clip(n.Text)))
	default:
		z.report(n, nil, fmt.Sprintf("invalid number %s", strconv.Quote(clip(n.Text))))
	}
}

// integerOnly reports a position whose numbers must be integers.
func integerOnly(s *schemaidx.Node) bool {
	t := s.Types()
	return t.Has(schemaidx.TypeInteger) && !t.Has(schemaidx.TypeNumber)
}

// list normalizes a list by its x-ruralz-list type (02 req 17).
func (z *normalizer) list(n *tree.Node, s *schemaidx.Node) {
	lt, key := s.List()
	switch lt {
	case schemaidx.ListSet:
		z.set(n, s)
	case schemaidx.ListMap:
		z.keyed(n, s, key, true)
	case schemaidx.ListOrderedMap:
		z.keyed(n, s, key, false)
	default:
	}
}

// setElem is a set element with its RFC 8785 bytes.
type setElem struct {
	canon []byte
	node  *tree.Node
}

// set sorts a set by its elements' RFC 8785 bytes and reports an element
// equal to an earlier one at [item=<value>], with the first as related.
func (z *normalizer) set(n *tree.Node, s *schemaidx.Node) {
	if len(n.Items) == 0 {
		return
	}
	elems := make([]setElem, len(n.Items))
	for i, it := range n.Items {
		z.w.tick()
		elems[i] = setElem{canon: schemaidx.AppendItemJSON(nil, it), node: it}
	}
	slices.SortStableFunc(elems, func(a, b setElem) int {
		z.w.tick()
		return bytes.Compare(a.canon, b.canon)
	})
	first := 0
	for i := range elems {
		n.Items[i] = elems[i].node
		if i == 0 || !bytes.Equal(elems[i].canon, elems[first].canon) {
			first = i
			continue
		}
		e := schemaidx.ItemElem(s, elems[i].node, i)
		z.report(elems[i].node, &e, fmt.Sprintf("duplicate element %s in a set after normalization", z.shownJSON(elems[i].canon)),
			diag.Related{Location: location(z.files, z.posOf(elems[first].node)), Message: "first element at"})
	}
}

// keyed checks a map or orderedMap list: every entry has a scalar key, no
// two keys are equal; a map list is then sorted by key (Go string order).
func (z *normalizer) keyed(n *tree.Node, s *schemaidx.Node, key string, sorted bool) {
	type entry struct {
		key  string
		node *tree.Node
	}
	entries := make([]entry, len(n.Items))
	first := make(map[string]*tree.Node, len(n.Items))
	for i, it := range n.Items {
		z.w.tick()
		entries[i].node = it
		k, ok := it.Get(key)
		if !ok || (k.Kind != tree.KindString && k.Kind != tree.KindInt && k.Kind != tree.KindFloat) {
			e := diag.Index(i)
			z.report(it, &e, fmt.Sprintf("list entry has no %s key member", key))
			continue
		}
		entries[i].key = k.Text
		canon := string(schemaidx.AppendItemJSON(nil, k))
		if prev, dup := first[canon]; dup {
			e := schemaidx.ItemElem(s, it, i)
			z.report(k, &e, fmt.Sprintf("duplicate entry: %s %s is already used by an earlier entry after normalization", key, z.shown(k.Text)),
				diag.Related{Location: location(z.files, z.posOf(prev)), Message: "first entry at"})
			continue
		}
		first[canon] = k
	}
	if !sorted {
		return
	}
	slices.SortStableFunc(entries, func(a, b entry) int {
		z.w.tick()
		return strings.Compare(a.key, b.key)
	})
	for i := range entries {
		n.Items[i] = entries[i].node
	}
}

// posOf returns n's position, or the nearest positioned ancestor's.
func (z *normalizer) posOf(n *tree.Node) tree.Pos {
	if n != nil && n.Pos.Known() {
		return n.Pos
	}
	return z.stack.pos(z.res.Start)
}

// shownJSON returns canonical JSON for a message, clipped, or a
// placeholder below a secret field.
func (z *normalizer) shownJSON(canon []byte) string {
	if z.stack.secret() {
		return "value"
	}
	return clip(string(canon))
}

// lowerHosts writes the host names of a RouteMatch (hosts) or a Listener
// (hostnames) in ASCII lower case, as the Router compares them (DNS names
// are case-insensitive in ASCII only, RFC 4343).
func lowerHosts(n *tree.Node, sel *schemaidx.Node) {
	var field string
	switch sel.Def() {
	case "RouteMatch":
		field = "hosts"
	case "Listener":
		field = "hostnames"
	default:
		return
	}
	l, ok := n.Get(field)
	if !ok || l.Kind != tree.KindList {
		return
	}
	for _, it := range l.Items {
		if it.Kind == tree.KindString {
			it.Text = lowerASCII(it.Text)
		}
	}
}

// sortMembers orders object members by the UTF-16 code units of their
// names (RFC 8785 section 3.2.3); stable, so it never reorders equal
// names, which the profile already rejected. Each comparison counts
// toward the yield timer.
func (z *normalizer) sortMembers(n *tree.Node) {
	cmp := func(a, b tree.Member) int {
		z.w.tick()
		return jsonval.CompareUTF16(a.Key, b.Key)
	}
	if !slices.IsSortedFunc(n.Members, cmp) {
		slices.SortStableFunc(n.Members, cmp)
	}
}
