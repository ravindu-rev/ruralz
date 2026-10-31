// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// The work of a call is counted while it is done, so Yield runs every 256
// work units inside a resource, spread over the work and not in bursts
// before or after it, since a time-based Yield collapses a burst into one
// Gosched. Every loop of the package over the input counts each element
// as it reads it, and every library call whose cost grows with the input
// goes through this file (TestLookupsMetered):
//
//   - tree.Node.Get and Delete are counted scans (get, remove), and a
//     member is deleted by moving the tail in steps (deleteAt);
//   - slices.DeleteFunc, clear and the sorts are counted loops
//     (deleteFunc, clearSteps, sortStable), copies and clears moving at
//     most moveStep elements per unit; the merge copies no tree;
//   - schemaidx.Node.Select looks each member an if/then rule tests up by
//     a scan of the instance, once per rule, which no caller can
//     interrupt, so it reads a proxy of the members a rule may test
//     (selectOf), and DispatchValues runs once per counted call
//     (dispatchTested);
//   - schemaidx.ItemElem reads a keyed entry's key by a counted scan and
//     names a set element by its canonical JSON only when it is small
//     (itemElem);
//   - input text a diagnostic carries is bounded where the merge builds
//     the diagnostic: a message quotes it clipped (clip), a path element
//     clips it (boundElem) and a resource identity shows it clipped and
//     quoted when it is not graphic (shown).
//
// What remains uncounted are single runtime operations on one value at
// memory speed: hashing a key for a map lookup, comparing two strings,
// allocating or growing a slice, and the RFC 8785 form of one number
// literal of a small set element. Each reads one value once, which the
// source limit bounds.

// moveStep is the most members or items one step of a copy, a clear or a
// compaction moves, counted as one work unit.
const moveStep = 64

// maxShown bounds, in bytes, the input text one diagnostic carries in each
// place, as config/canonical bounds it: a value quoted in its message, the
// resource kind and name of its identity or its message, and a member
// name, a list key or the canonical JSON of a set element in its path.
// The text form of a diagnostic thus stays short however large the
// content, so the diagnostics of a merge print in space linear in their
// count, and up to 10,001 diagnostics below one long key never each
// repeat it in full.
const maxShown = 64

// probe records how one call spreads its work between Yield calls, for
// tests (TestYieldSpread).
type probe struct {
	// since counts the work units charged since the last Yield.
	since int
	// gap is the most work units charged between two Yield calls, or
	// before the first.
	gap int
	// selectWidth is the most members of an object schemaidx Select read.
	selectWidth int
}

// last returns the most work units charged between two Yield calls, the
// work after the last Yield included.
func (p *probe) last() int { return max(p.gap, p.since) }

// setProbe makes a record its work in p, when p is not nil. A call starts
// where its caller yields (the loader yields between units), so the work
// before it does not count against its first Yield.
func (a *applier) setProbe(p *probe) {
	a.probe = p
	if p != nil {
		p.gap = max(p.gap, p.since)
		p.since = 0
	}
}

// tick counts one work unit, calling Yield each time yieldEvery more
// have passed. Every unit is counted as its work is done, one at a time.
func (a *applier) tick() {
	if p := a.probe; p != nil {
		p.since++
	}
	if a.units++; a.units >= yieldEvery {
		a.units = 0
		a.yield()
	}
}

// yield calls Options.Yield.
func (a *applier) yield() {
	if p := a.probe; p != nil {
		p.gap = max(p.gap, p.since)
		p.since = 0
	}
	if a.opts.Yield != nil {
		a.opts.Yield()
	}
}

// selected records that schemaidx Select read an object of n members.
func (a *applier) selected(n int) {
	if a.probe != nil {
		a.probe.selectWidth = max(a.probe.selectWidth, n)
	}
}

// memberAt returns the index of the first member key of object n, or -1,
// counting each member it reads (tree.Node.Get).
func (a *applier) memberAt(n *tree.Node, key string) int {
	if n == nil || n.Kind != tree.KindMap {
		return -1
	}
	for i := range n.Members {
		a.tick()
		if n.Members[i].Key == key {
			return i
		}
	}
	return -1
}

// get returns the value of the first member key of object n, as
// tree.Node.Get does, counting each member it reads.
func (a *applier) get(n *tree.Node, key string) (*tree.Node, bool) {
	if i := a.memberAt(n, key); i >= 0 {
		return n.Members[i].Value, true
	}
	return nil, false
}

// remove deletes the first member key of object n, as tree.Node.Delete
// does, and reports whether there was one.
func (a *applier) remove(n *tree.Node, key string) bool {
	i := a.memberAt(n, key)
	if i < 0 {
		return false
	}
	n.Members = deleteAt(a, n.Members, i)
	return true
}

// deleteAt removes s[i], as slices.Delete(s, i, i+1) does, moving the
// tail moveStep elements per unit.
func deleteAt[E any](a *applier, s []E, i int) []E {
	for j := i; j < len(s)-1; j += moveStep {
		end := min(len(s)-1, j+moveStep)
		copy(s[j:end], s[j+1:end+1])
		a.tick()
	}
	var zero E
	s[len(s)-1] = zero
	return s[:len(s)-1]
}

// deleteFunc removes the elements of s for which del reports true, as
// slices.DeleteFunc does, counting one unit per element and clearing the
// tail in steps.
func deleteFunc[E any](a *applier, s []E, del func(E) bool) []E {
	kept := s[:0]
	for _, e := range s {
		a.tick()
		if !del(e) {
			kept = append(kept, e)
		}
	}
	clearSteps(a, s[len(kept):])
	return kept
}

// clearSteps zeroes s, moveStep elements per unit.
func clearSteps[E any](a *applier, s []E) {
	for len(s) > 0 {
		k := min(len(s), moveStep)
		clear(s[:k])
		s = s[k:]
		a.tick()
	}
}

// sortStable sorts s by cmp, keeping equal elements in input order, as
// slices.SortStableFunc does: a bottom-up merge sort that counts one unit
// per element it moves (one comparison at most), so no step of the sort
// runs uncounted (the library sorts move elements between comparisons).
func sortStable[E any](a *applier, s []E, cmp func(x, y E) int) {
	n := len(s)
	if n < 2 {
		return
	}
	src, dst := s, make([]E, n)
	// bounded: log2(n) passes, each counting the n elements it moves.
	for width := 1; width < n; width *= 2 {
		// bounded: the runs of one pass; the loop inside counts each element it moves.
		for lo := 0; lo < n; lo += 2 * width {
			mid, hi := min(lo+width, n), min(lo+2*width, n)
			i, j := lo, mid
			for k := lo; k < hi; k++ {
				a.tick()
				if j >= hi || (i < mid && cmp(src[j], src[i]) >= 0) {
					dst[k] = src[i]
					i++
				} else {
					dst[k] = src[j]
					j++
				}
			}
		}
		src, dst = dst, src
	}
	if &src[0] != &s[0] {
		for i := 0; i < n; i += moveStep {
			copy(s[i:min(n, i+moveStep)], src[i:])
			a.tick()
		}
	}
}

// selectOf returns static.Select(inst), the schema of object inst with
// the if/then dispatch of static applied, where a member whose value is
// nil (one mergeMap removed) is absent. schemaidx Select finds each
// member a rule tests by a scan of inst, once per rule and round (cases
// times members, and the 23 PolicySpec rules), which no caller can count
// as it runs, so selectOf passes it a proxy: inst itself when it has at
// most linearScan members and none removed, else the first member of each
// name that the schema nodes the dispatch reaches declare, collected by a
// counted pass over inst. Select reads the proxy as it reads inst,
// because every member a rule reads is a declared property of the object
// whose rule it is (TestDispatchReadsDeclared over the committed schema)
// and a dispatch result declares every property of the nodes it passed
// through: when the result declares a property no node read so far
// declared, another pass collects it and Select runs again, which ends
// within the rounds Select takes. A schemaidx API exporting the members
// its rules read would make the proxy exact for every schema.
func (a *applier) selectOf(static *schemaidx.Node, inst *tree.Node) *schemaidx.Node {
	a.tick()
	if inst == nil || inst.Kind != tree.KindMap {
		return static.Select(inst)
	}
	var buf [linearScan]tree.Member
	proxy := &tree.Node{Kind: tree.KindMap, Members: buf[:0]}
	if len(inst.Members) <= linearScan {
		removed := false
		for _, m := range inst.Members {
			a.tick()
			removed = removed || m.Value == nil
		}
		if !removed {
			a.selected(len(inst.Members))
			return static.Select(inst)
		}
		// bounded: at most linearScan members, counted by the loop above.
		for _, m := range inst.Members {
			if m.Value != nil {
				proxy.Members = append(proxy.Members, m)
			}
		}
		a.selected(len(proxy.Members))
		return static.Select(proxy)
	}
	read := []*schemaidx.Node{static}
	a.collect(proxy, inst, static)
	for pass := 1; ; pass++ {
		sel := static.Select(proxy)
		a.selected(len(proxy.Members))
		if pass == maxSelectPasses || !a.declaresMore(sel, read) {
			return sel
		}
		read = append(read, sel)
		if !a.collect(proxy, inst, sel) {
			return sel
		}
	}
}

// maxSelectPasses bounds the passes of selectOf: one per dispatch round
// schemaidx Select takes (at most 8) and one more.
const maxSelectPasses = 9

// collect appends to proxy the first member of inst, with a value, of
// each name that node declares and proxy lacks, and reports whether it
// added one, counting each member of inst.
func (a *applier) collect(proxy, inst *tree.Node, node *schemaidx.Node) bool {
	added := false
	for _, m := range inst.Members {
		a.tick()
		if m.Value == nil {
			continue
		}
		if _, ok := node.Property(m.Key); !ok {
			continue
		}
		if a.memberAt(proxy, m.Key) < 0 {
			proxy.Members = append(proxy.Members, m)
			added = true
		}
	}
	return added
}

// declaresMore reports whether sel declares a property that none of the
// nodes in read declares, counting each property it compares.
func (a *applier) declaresMore(sel *schemaidx.Node, read []*schemaidx.Node) bool {
	// bounded: the nodes of at most maxSelectPasses dispatch results.
	for _, r := range read {
		if r == sel {
			return false
		}
	}
	// bounded: the declared properties of one schema node; each comparison is counted.
	for name := range sel.Properties() {
		known := false
		for _, r := range read {
			a.tick()
			if _, ok := r.Property(name); ok {
				known = true
				break
			}
		}
		if !known {
			return true
		}
	}
	return false
}

// dispatchTested reports whether an if/then rule of static tests member
// name against a string constant (schemaidx DispatchValues), one counted
// call: its cost is bounded by the rules of one schema node.
func (a *applier) dispatchTested(static *schemaidx.Node, name string) bool {
	a.tick()
	return len(static.DispatchValues(name)) > 0
}

// itemElem returns the key-aware path element of element i of a list
// whose schema is list, as schemaidx.ItemElem does, counting what it
// reads: a keyed entry's key member is found by a counted scan, and a
// set element is named by its canonical JSON only when that is at most
// maxShown bytes, else by its index, as config/canonical names it.
// Whether an element is small is decided by a counted walk that stops
// past maxShown bytes of its text (keys, strings and number literals as
// written), so only a small element is encoded. Callers pass composite
// elements and keyed entries; a set's scalar elements are never named
// here. A keyed entry's key stays whole on the merger's stack; a
// diagnostic's copy of the path clips it (boundElem).
func (a *applier) itemElem(list *schemaidx.Node, item *tree.Node, i int) diag.PathElem {
	lt, key := list.List()
	switch {
	case lt.Keyed():
		if k, ok := a.get(item, key); ok && keyScalar(k) {
			return diag.Keyed(key, k.Text)
		}
	case lt == schemaidx.ListSet && item != nil:
		if composite(item) && a.textSize(item, maxShown) > maxShown {
			return diag.Index(i)
		}
		e := schemaidx.ItemElem(list, item, i)
		if raw, ok := e.Item.(json.RawMessage); ok && len(raw) > maxShown {
			return diag.Index(i)
		}
		return e
	default:
	}
	return diag.Index(i)
}

// textSize returns the bytes of n's text, each node, key, string and
// number literal counted with its length plus one, stopping once the sum
// passes limit (it then returns a sum past limit), counting each node.
func (a *applier) textSize(n *tree.Node, limit int) int {
	a.tick()
	size := 1 + len(n.Text)
	for _, m := range n.Members {
		if size > limit {
			return size
		}
		size += 1 + len(m.Key) + a.textSize(m.Value, limit-size)
	}
	for _, it := range n.Items {
		if size > limit {
			return size
		}
		size += a.textSize(it, limit-size)
	}
	return size
}

// clip shortens s, input text a diagnostic carries, to its first maxShown
// bytes cut before a rune, marked with "...". The result is always a
// copy, as config/canonical's is: s may be a substring of the parser's
// copy of the source, which a diagnostic holding it would keep alive.
func clip(s string) string {
	if len(s) <= maxShown {
		return strings.Clone(s)
	}
	cut := maxShown
	// bounded: at most utf8.UTFMax-1 steps back to a rune start.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// boundElem returns path element e with the input text it names bounded,
// as config/canonical's boundElem bounds it: a member name, a keyed
// entry's key and a string set element are clipped (diag quotes them as
// JSON). itemElem already names a set element whose canonical JSON is
// longer than maxShown by its index.
func boundElem(e diag.PathElem) diag.PathElem {
	switch e.Kind {
	case diag.ElemField:
		e.Name = clip(e.Name)
	case diag.ElemKeyed:
		e.Key = clip(e.Key)
	case diag.ElemItem:
		if s, ok := e.Item.(string); ok {
			e.Item = clip(s)
		}
	case diag.ElemIndex:
	}
	return e
}

// shown returns a resource kind or name from the input as a diagnostic
// shows it, as config/canonical shows it: clipped (a copy), Go-quoted
// (strconv.QuoteToGraphic) when it holds a character that is not graphic
// (a newline, a control or format character), a quote, a backslash or
// invalid UTF-8, and `""` when it is empty. diag.AppendText prints an
// identity as it is, so the one-line text form then carries no newline and
// no terminal control sequence.
func shown(s string) string {
	if s == "" {
		return `""`
	}
	c := clip(s)
	if needsQuote(c) {
		return strconv.QuoteToGraphic(c)
	}
	return c
}

// needsQuote reports whether strconv.QuoteToGraphic would escape a
// character of s: one that is not graphic, a quote, a backslash or a byte
// of invalid UTF-8.
func needsQuote(s string) bool {
	// bounded: a clipped text of at most maxShown+3 bytes.
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if c < 0x20 || c == 0x7f || c == '"' || c == '\\' {
				return true
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || !strconv.IsGraphic(r) {
			return true
		}
		i += size
	}
	return false
}

// resourceID returns the identity a diagnostic carries for the resource
// id (shown).
func resourceID(id tree.ID) *diag.ResourceID {
	return &diag.ResourceID{Kind: shown(string(id.Kind)), Name: shown(id.Name)}
}

// idText returns the identity id as a message names it: "<Kind>/<name>",
// each part shown.
func idText(id tree.ID) string { return shown(string(id.Kind)) + "/" + shown(id.Name) }

// memberIndex finds the members of an object by the keys of the members
// of an overlay object: a counted scan when the overlay has at most
// linearScan members (at most linearScan scans, so linear in the object),
// else a map built once, member by member, so mergeMap costs time linear
// in both objects. It covers the members present when it was made; the
// first member of a key is found. The overlay's keys are claimed as
// mergeMap applies them, so a later overlay member with an earlier one's
// key is found as claimed: the first member of a key applies, as the
// profile keeps it (stage B).
type memberIndex struct {
	a       *applier
	members []tree.Member
	pos     map[string]int
}

// claimedKey marks a key in memberIndex.pos that an overlay member
// claimed.
const claimedKey = -1 << 31

func (a *applier) memberIndex(members []tree.Member, lookups int) memberIndex {
	x := memberIndex{a: a, members: members}
	if lookups > linearScan {
		// Not presized: a map grows table by table, while a presized one
		// would be allocated and cleared in one step.
		x.pos = map[string]int{}
		for i := range members {
			a.tick()
			if _, dup := x.pos[members[i].Key]; !dup {
				x.pos[members[i].Key] = i
			}
		}
	}
	return x
}

// claim returns the index of the first member with the key of overlay
// member j of ov, or -1, and claims the key; seen reports that an earlier
// member of ov claimed it.
func (x memberIndex) claim(ov []tree.Member, j int) (i int, seen bool) {
	key := ov[j].Key
	if x.pos != nil {
		x.a.tick()
		i, ok := x.pos[key]
		x.pos[key] = claimedKey
		switch {
		case !ok:
			return -1, false
		case i == claimedKey:
			return -1, true
		default:
			return i, false
		}
	}
	for _, earlier := range ov[:j] {
		x.a.tick()
		if earlier.Key == key {
			return -1, true
		}
	}
	for i := range x.members {
		x.a.tick()
		if x.members[i].Key == key {
			return i, false
		}
	}
	return -1, false
}
