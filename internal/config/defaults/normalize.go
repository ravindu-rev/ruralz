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
	"unicode/utf8"

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
//     to the canonical encoder. A nil node (a member value or list
//     element a converter or a FromResources producer left nil) is a
//     null too, and a node of no JSON kind is RZ-CFG-005 likewise.
//
//   - A string, or a member name, that is not valid UTF-8 is RZ-CFG-005 at
//     its key-aware path and position, before any scalar normalization
//     (02 req 15 and 19): the canonical form holds only UTF-8 text, and
//     stage E substitutes environment values as they are. The message
//     never quotes the value, which may lie below an x-ruralz-secret
//     field.
//
//   - A number literal outside the RFC 8259 grammar is RZ-CFG-005, in
//     canonical content too, where only the range rule is waived.
//
//   - A member name that an object holds twice is RZ-CFG-005 at each
//     later member, with the first as related: the canonical form holds a
//     name once, and the profile rejected the authored ones, so only a
//     converter or a FromResources producer leaves one.
//
//   - In an object with declared members (not a typed map), an undeclared
//     member whose name equals a declared one under Unicode case folding
//     (strings.EqualFold, such as Key, KEY or the Kelvin sign K for key)
//     is RZ-CFG-005 at its name: encoding/json matches member names
//     case-insensitively, so a decode of the text, such as a Filter
//     factory's of a Policy's spec.config, would read it as the declared
//     member, which stage F checked and it was not. An open config admits
//     every other undeclared member (02 req 18).
//
// After these rules the decode never meets a value appendJSON refuses, and
// any encoding/json decode of the tree reads each declared member from
// that member alone. A diagnostic is never repeated as another: a null,
// nil or otherwise reported list entry, and a keyed entry whose key member
// was reported, is not reported again as an entry without a key member or
// as a duplicate, and a set element whose visit reported anything is left
// out of the duplicate check.
//
// Free content (undeclared members of open objects, the JSONSchemaDocument
// value) has its members sorted, its numbers checked, its nulls and
// invalid text rejected, and nothing else. files gives source locations
// and the file role (nil: line and column only, and the range rule
// applies). A diagnostic's path names each list element on it as
// normalized (a keyed entry by its key member, a set element by its RFC
// 8785 text), computed once per element for all the diagnostics below
// it, so a diagnostic costs the depth of its path. A diagnostic's message
// never holds a value below an x-ruralz-secret field. Normalize returns
// at most DefaultMaxDiagnostics+1 diagnostics, as Resource does at the
// default cap.
func Normalize(idx *schemaidx.Index, res *tree.Resource, files *tree.FileTable) diag.List {
	ds, _ := normalize(idx, res, files, newWork())
	return ds
}

// reported is what normalization tells the decode after an error: the
// nodes whose own failure it recorded a diagnostic for, whose decode
// failures are not reported again, and the values of the members it
// rejected by name (a duplicate name, a name that folds onto a declared
// one), below which the decode reports nothing, since the member is wrong
// whatever its value; each at most the resource's diagnostic budget.
type reported struct {
	nodes, members map[*tree.Node]struct{}
}

// has reports a node normalization reported.
func (r *reported) has(n *tree.Node) bool {
	if r == nil || n == nil {
		return false
	}
	_, ok := r.nodes[n]
	return ok
}

// rejected reports the value of a member normalization rejected by name.
func (r *reported) rejected(n *tree.Node) bool {
	if r == nil || n == nil {
		return false
	}
	_, ok := r.members[n]
	return ok
}

// normalize is Normalize on worker w: diagnostics draw on w's budget, and
// prior is non-nil when normalization found an error, even one the budget
// dropped.
func normalize(idx *schemaidx.Index, res *tree.Resource, files *tree.FileTable, w *work) (ds diag.List, prior *reported) {
	if idx == nil || res == nil || res.Root == nil {
		return nil, nil
	}
	s, ok := idx.Resource(string(res.ID.Kind))
	if !ok {
		s = nil
	}
	z := &normalizer{res: res, files: files, checkRange: !canonical(files, res), w: w}
	z.stack = append(z.stack, step{node: res.Root})
	z.visit(res.Root, s)
	if z.errs == 0 {
		return z.diags, nil
	}
	return z.diags, &reported{nodes: z.reported, members: z.rejected}
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
	// first is the number of diagnostics the normalizer had recorded when
	// it pushed the step: the ones recorded since lie below it, and an
	// element step names them all when it is popped (normalizer.pop).
	first int
	// elem is the name of an element step once stack.path computed it;
	// named reports that it did.
	elem  diag.PathElem
	named bool
}

// stack is the path of the node being visited; stack[0] is the root.
type stack []step

// path returns the key-aware path of the top of s, each list element
// named by schemaidx.ItemElem, which looks up a keyed entry's key member
// or encodes a set element (w counts both, and the path's length). An
// element step is named on the first path through it and keeps the name,
// so the paths of many failures below one element (the locator's) cost
// its size once and then the depth of each path. The normalizer, which
// names an element in the form it has after its visit, uses prefix and
// pop instead.
func (s stack) path(w *work) diag.Path {
	out := s.prefix()
	w.charge(len(out))
	steps := s[min(1, len(s)):]
	for i := range steps {
		st := &steps[i]
		if !st.item {
			continue
		}
		if !st.named {
			st.elem, st.named = w.itemElem(st.list, st.node, st.index), true
		}
		out[i] = st.elem
	}
	return out
}

// prefix returns the key-aware path of the top of s with every member
// step named and every list element step left as a zero placeholder,
// which normalizer.pop fills in: O(len(s)), whatever the size of the
// elements on the path.
func (s stack) prefix() diag.Path {
	out := make(diag.Path, 0, len(s))
	for _, st := range s[min(1, len(s)):] {
		if st.item {
			out = append(out, diag.PathElem{})
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
	// errs counts the errors, recorded or dropped by the budget; a list
	// visit compares it around each element to tell the elements that
	// failed.
	errs int
	// reported holds the nodes of the recorded diagnostics, and rejected
	// the values of the members rejected by name (reported).
	reported, rejected map[*tree.Node]struct{}
	w                  *work
}

// was reports a node whose own failure has a recorded diagnostic. A
// failure the budget dropped is not held, but then the budget is spent
// and every later diagnostic of the resource is dropped too, so a check
// that skips what was reported never records a repeat.
func (z *normalizer) was(n *tree.Node) bool {
	_, ok := z.reported[n]
	return ok
}

// push enters a member or list element of the node being visited.
func (z *normalizer) push(st step) {
	st.first = len(z.diags)
	z.stack = append(z.stack, st)
}

// pop leaves the top step. A list element step with diagnostics below it
// is named now, once, by schemaidx.ItemElem (a keyed entry's key member
// or a set element's RFC 8785 text, w counting both), in the hub form the
// visit just gave it, and the name fills its placeholder in the path of
// each of those diagnostics (stack.prefix). Naming it once per
// diagnostic instead would cost the diagnostics times the element's size,
// quadratic in the element for a null member per entry member.
func (z *normalizer) pop() {
	top := len(z.stack) - 1
	if st := z.stack[top]; st.item && len(z.diags) > st.first {
		e := z.w.itemElem(st.list, st.node, st.index)
		z.w.charge(len(z.diags) - st.first)
		for i := st.first; i < len(z.diags); i++ {
			z.diags[i].Path[top-1] = e
		}
	}
	z.stack = z.stack[:top]
}

// visit normalizes n, whose schema is s (nil for free content), and below
// it; lists are sorted after their elements were normalized.
func (z *normalizer) visit(n *tree.Node, s *schemaidx.Node) {
	z.w.tick()
	if n == nil {
		z.null(nil)
		return
	}
	switch n.Kind {
	case tree.KindMap:
		sel := z.w.selectSchema(s, n)
		if sel.Def() == defJSONSchemaDocument {
			sel = nil
		}
		z.stack[len(z.stack)-1].schema = sel
		lowerHosts(z.w, n, sel)
		z.sortMembers(n)
		names := memberNames{sel: sel, structLike: isStruct(sel)}
		for i := range n.Members {
			m := n.Members[i]
			child := memberSchema(sel, m.Key)
			z.push(step{node: m.Value, schema: child, name: m.Key})
			z.w.charge(len(m.Key))
			// Each at the name's position; the value's own checks still run.
			z.memberName(n, i, &names)
			z.visit(m.Value, child)
			z.pop()
		}
	case tree.KindList:
		it := itemSchema(s)
		var failed []bool
		if lt, _ := s.List(); lt == schemaidx.ListSet {
			failed = make([]bool, len(n.Items))
		}
		for i, item := range n.Items {
			errs := z.errs
			z.push(step{node: item, schema: it, item: true, list: s, index: i})
			z.visit(item, it)
			z.pop()
			if failed != nil {
				failed[i] = z.errs != errs
			}
		}
		z.list(n, s, failed)
	case tree.KindString:
		z.w.charge(len(n.Text))
		if !utf8.ValidString(n.Text) {
			z.report(n, nil, invalidText("string"))
			return
		}
		z.scalar(n, s)
	case tree.KindInt, tree.KindFloat:
		z.number(n, s)
	case tree.KindNull:
		z.null(n)
	case tree.KindBool:
	default:
		z.report(n, nil, fmt.Sprintf("node kind %d has no JSON value", n.Kind))
	}
}

// invalidText is the message of a string or member name that is not
// valid UTF-8; it never shows the text.
func invalidText(what string) string {
	return what + " is not valid UTF-8: the canonical form holds only UTF-8 text"
}

// memberNames is what the name checks of one object's members need: its
// schema, dispatch applied; whether it has declared members that a decode
// matches by name (isStruct: not a typed map, not free content); the
// declared names by foldName, built on its first undeclared member; and
// the index of the first member of the run of equal names that ends at
// the member being checked.
type memberNames struct {
	sel        *schemaidx.Node
	structLike bool
	folds      map[string]string
	run        int
}

// memberName checks the name of member i of object n, whose members are
// sorted (sortMembers), at the name's position: a name that is not valid
// UTF-8, a name equal to the one before it (a duplicate, with the first
// of its run as related), and an undeclared name that equals a declared
// one under case folding. A name gets at most one of these diagnostics;
// w counts the bytes compared and folded.
func (z *normalizer) memberName(n *tree.Node, i int, names *memberNames) {
	m := n.Members[i]
	if i > 0 {
		prev := n.Members[i-1].Key
		z.w.charge(1 + commonPrefix(prev, m.Key))
		if prev != m.Key {
			names.run = i
		}
	}
	switch {
	case !utf8.ValidString(m.Key):
		z.reportAt(nil, m.KeyPos, nil, invalidText("member name"))
	case names.run < i:
		first := n.Members[names.run]
		at := first.KeyPos
		if !at.Known() {
			at = z.posOf(first.Value)
		}
		z.reject(m, fmt.Sprintf("duplicate member name %s: the canonical form holds each name once", strconv.Quote(clip(m.Key))),
			diag.Related{Location: location(z.files, at), Message: "first member at"})
	case names.structLike:
		if declared, ok := z.foldsOnto(names, m.Key); ok {
			z.reject(m, fmt.Sprintf("member %s differs from the declared member %s only in letter case: encoding/json, which matches member names case-insensitively, would decode it as that member; rename or remove it",
				strconv.Quote(clip(m.Key)), strconv.Quote(clip(declared))))
		}
	default:
	}
}

// reject reports member m by its name, at the name's position, and,
// when the diagnostic is recorded, holds its value as rejected, so the
// decode blames nothing below it (the locator would match a folded name
// to the declared field and report its value's type).
func (z *normalizer) reject(m tree.Member, msg string, related ...diag.Related) {
	if !z.reportAt(nil, m.KeyPos, nil, msg, related...) || m.Value == nil {
		return
	}
	if z.rejected == nil {
		z.rejected = map[*tree.Node]struct{}{}
	}
	z.rejected[m.Value] = struct{}{}
}

// foldsOnto returns the declared member of the object names describes
// whose name equals name under Unicode case folding (foldName, the
// relation strings.EqualFold and encoding/json use) when name itself is
// not declared. The declared names are folded once per object, on its
// first undeclared member; w counts them and the bytes of name.
func (z *normalizer) foldsOnto(names *memberNames, name string) (string, bool) {
	if _, declared := names.sel.Property(name); declared {
		return "", false
	}
	if names.folds == nil {
		names.folds = map[string]string{}
		for p := range names.sel.Properties() {
			z.w.charge(1 + len(p))
			names.folds[foldName(p)] = p
		}
	}
	z.w.charge(len(name))
	p, ok := names.folds[foldName(name)]
	return p, ok
}

// null reports a null value or a nil node (02 req 19). Stage F admits a
// null only in free content, such as an open config member or an enum of
// the JSONSchemaDocument value, but no null has a ruralz.canonical.v1
// form.
func (z *normalizer) null(n *tree.Node) {
	what := "member"
	if z.stack[len(z.stack)-1].item {
		what = "element"
	}
	z.report(n, nil, "null is not allowed: the canonical form has no null; remove the "+what)
}

// report records an RZ-CFG-005 error at the top of the stack, extended by
// elem when set, located at node (or its nearest positioned ancestor).
// Its path names the list elements on the way when they are popped
// (pop), so a diagnostic costs the depth of the stack, not the size of
// the elements above it.
func (z *normalizer) report(node *tree.Node, elem *diag.PathElem, msg string, related ...diag.Related) {
	var at tree.Pos
	if node != nil {
		at = node.Pos
	}
	z.reportAt(node, at, elem, msg, related...)
}

// reportAt is report located at pos when it is known (else at the nearest
// positioned node on the stack); node, when not nil, is the node whose own
// failure this is, which the decode does not report again (reported). It
// reports whether the budget had room for the diagnostic.
func (z *normalizer) reportAt(node *tree.Node, pos tree.Pos, elem *diag.PathElem, msg string, related ...diag.Related) bool {
	z.errs++
	if !z.w.take() {
		return false
	}
	if node != nil {
		if z.reported == nil {
			z.reported = map[*tree.Node]struct{}{}
		}
		z.reported[node] = struct{}{}
	}
	p := z.stack.prefix()
	z.w.charge(len(p))
	if elem != nil {
		p = append(p, *elem)
	}
	at := z.stack.pos(z.res.Start)
	if pos.Known() {
		at = pos
	}
	z.diags = append(z.diags, diag.Diagnostic{
		Code: CodeSchema, Severity: diag.SeverityError, Location: location(z.files, at),
		Resource: z.res.ID.ResourceID(), Path: p, Message: msg, Related: related,
	})
	return true
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
			z.report(n, nil, z.byteSizeMessage(n.Text, err))
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

// byteSizeMessage returns the message of a ByteSize text that
// v1alpha1.ParseByteSize rejected with err: err's reason, with the value,
// which err quotes whole, shown clipped (or not at all below a secret
// field), so a megabyte of digits gives a short message.
func (z *normalizer) byteSizeMessage(text string, err error) string {
	msg, quoted := err.Error(), strconv.Quote(text)
	if !strings.Contains(msg, quoted) {
		return "invalid byte size " + z.shown(text)
	}
	return strings.Replace(msg, quoted, z.shown(text), 1)
}

// number writes an integral number at an integer-only position as integer
// text (01 req 37) and applies the number range rule (02 req 16, R-64);
// w counts the bytes of the literal, which both read.
func (z *normalizer) number(n *tree.Node, s *schemaidx.Node) {
	z.w.charge(len(n.Text))
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
// exempt from the range rule (R-64), not from the grammar.
func (z *normalizer) checkNumber(n *tree.Node) {
	if !z.checkRange {
		if !jsonval.ValidNumber(n.Text) {
			z.report(n, nil, fmt.Sprintf("invalid number %s", strconv.Quote(clip(n.Text))))
		}
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

// list normalizes a list by its x-ruralz-list type (02 req 17); failed
// marks the elements of a set whose visit reported an error.
func (z *normalizer) list(n *tree.Node, s *schemaidx.Node, failed []bool) {
	lt, key := s.List()
	switch lt {
	case schemaidx.ListSet:
		z.set(n, s, failed)
	case schemaidx.ListMap:
		z.keyed(n, s, key, true)
	case schemaidx.ListOrderedMap:
		z.keyed(n, s, key, false)
	default:
	}
}

// setElem is a set element with its RFC 8785 bytes; failed reports that
// its visit reported an error.
type setElem struct {
	canon  []byte
	node   *tree.Node
	failed bool
}

// set sorts a set by its elements' RFC 8785 bytes and reports an element
// equal to an earlier one at [item=<value>], with the first as related.
// An element whose visit reported an error (failed[i]: a null, a nil
// node, text that is not UTF-8, a number or scalar normalization refused,
// anything below it) is sorted but left out of the duplicate check: its
// normalized form is unknown, and its error is reported already, so one
// null is one diagnostic however many the set holds. Each comparison
// counts one unit and the bytes of the common prefix it reads. (The RFC
// 8785 text keeps the bytes of a string that is not UTF-8, so two such
// strings compare by their bytes.)
func (z *normalizer) set(n *tree.Node, s *schemaidx.Node, failed []bool) {
	if len(n.Items) == 0 {
		return
	}
	elems := make([]setElem, len(n.Items))
	for i, it := range n.Items {
		z.w.tick()
		elems[i] = setElem{canon: z.w.itemJSON(it), node: it, failed: failed[i]}
	}
	slices.SortStableFunc(elems, func(a, b setElem) int {
		z.w.charge(1 + commonPrefix(a.canon, b.canon))
		return bytes.Compare(a.canon, b.canon)
	})
	first := -1
	for i := range elems {
		n.Items[i] = elems[i].node
		if elems[i].failed {
			continue
		}
		if first >= 0 {
			z.w.charge(1 + commonPrefix(elems[i].canon, elems[first].canon))
		}
		if first < 0 || !bytes.Equal(elems[i].canon, elems[first].canon) {
			first = i
			continue
		}
		e := z.w.itemElem(s, elems[i].node, i)
		z.report(elems[i].node, &e, fmt.Sprintf("duplicate element %s in a set after normalization", z.shownJSON(elems[i].canon)),
			diag.Related{Location: location(z.files, z.posOf(elems[first].node)), Message: "first element at"})
	}
}

// keyed checks a map or orderedMap list: every entry has a scalar key, no
// two keys are equal; a map list is then sorted by key (Go string order).
// An entry its visit reported (a null, a nil node, a node of no JSON kind,
// text that is not UTF-8), and an entry whose key member its visit
// reported (the same, or a number or scalar normalization refused), is
// not reported again, neither as an entry without a key member nor as a
// duplicate; an entry with an error elsewhere keeps its key checks. Each
// sort comparison counts one unit and the bytes of the common prefix it
// reads.
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
		if it == nil || it.Kind == tree.KindNull || z.was(it) {
			continue
		}
		k, ok := z.w.get(it, key)
		if ok && (k == nil || k.Kind == tree.KindNull || z.was(k)) {
			continue
		}
		if !ok || (k.Kind != tree.KindString && k.Kind != tree.KindInt && k.Kind != tree.KindFloat) {
			e := diag.Index(i)
			z.report(it, &e, fmt.Sprintf("list entry has no %s key member", key))
			continue
		}
		entries[i].key = k.Text
		canon := string(z.w.itemJSON(k))
		if prev, dup := first[canon]; dup {
			e := z.w.itemElem(s, it, i)
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
		z.w.charge(1 + commonPrefix(a.key, b.key))
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
// placeholder below a secret field and for text that is not UTF-8 (which
// set leaves out of its duplicate check, so only a direct call meets
// one): a message never holds bytes that are not UTF-8, nor shows such a
// value.
func (z *normalizer) shownJSON(canon []byte) string {
	z.w.charge(len(canon))
	if z.stack.secret() || !utf8.Valid(canon) {
		return "value"
	}
	return clip(string(canon))
}

// commonPrefix returns the length of the longest common prefix of a and
// b, the bytes a comparison of them reads before it decides.
func commonPrefix[T string | []byte](a, b T) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// lowerHosts writes the host names of a RouteMatch (hosts) or a Listener
// (hostnames) in ASCII lower case, as the Router compares them (DNS names
// are case-insensitive in ASCII only, RFC 4343); w counts the lookup and
// the bytes of each name. A nil list or element is a null, which visit
// reports.
func lowerHosts(w *work, n *tree.Node, sel *schemaidx.Node) {
	var field string
	switch sel.Def() {
	case "RouteMatch":
		field = "hosts"
	case "Listener":
		field = "hostnames"
	default:
		return
	}
	l, ok := w.get(n, field)
	if !ok || l == nil || l.Kind != tree.KindList {
		return
	}
	for _, it := range l.Items {
		if it != nil && it.Kind == tree.KindString {
			w.charge(len(it.Text))
			it.Text = lowerASCII(it.Text)
		}
	}
}

// sortMembers orders object members by the UTF-16 code units of their
// names (RFC 8785 section 3.2.3); stable, so it never reorders equal
// names, which are then adjacent for memberName to report (the profile
// rejects authored ones; a converter may leave one). Each comparison
// counts one unit and the bytes of the common prefix of the two names,
// which jsonval.CompareUTF16 reads before it decides.
func (z *normalizer) sortMembers(n *tree.Node) {
	cmp := func(a, b tree.Member) int {
		z.w.charge(1 + commonPrefix(a.Key, b.Key))
		return jsonval.CompareUTF16(a.Key, b.Key)
	}
	if !slices.IsSortedFunc(n.Members, cmp) {
		slices.SortStableFunc(n.Members, cmp)
	}
}
