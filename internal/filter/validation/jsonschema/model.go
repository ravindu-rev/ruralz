// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"cmp"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The walk's model of what the library collects and compiles (walker),
// kept in tables indexed by integers, so that a pass of the walk's
// fixpoint costs the same whatever the length of the pointers and URLs it
// handles (passes that hashed every pointer prefix of every position,
// keyed every table by pointer, rebuilt every reference target's pointer
// and sorted the targets by their pointers again would take 30 s and
// 31 GB for 228 passes under a 200,000-byte member name). Each schema
// position is a node, interned the first time a pass meets it, its
// pointer built then, once. The pointers are kept as a trie of their
// tokens: the resource of a position is the first resource up its trie
// entries, and two positions are ordered by their tokens below the entry
// they share. URLs and the strings of a reference, an $id or an anchor are
// interned once, and what resolving a reference computes from the
// document alone, joining it with a base URL and walking its JSON Pointer
// from a resource, once per base and reference and per resource and
// fragment. A pass therefore works in integers only, and builds or hashes
// a pointer, URL or string only where it meets one no pass met before: a
// new node, trie entry, string or URL, a new join and a new fragment walk,
// each charged its bytes once the first pass has ended (charge). Between
// passes the targets a pass found are placed among those found before
// (insertKnown), which charges nothing, each target being placed once.

// The nodes of the model that have a fixed index.
const (
	// rootNode is the document's root, at the trie's root entry.
	rootNode int = 0
	// baseURLIndex is the index of the document's URL, baseURL.
	baseURLIndex int = 0
)

// What a JSON Pointer from a resource names when it names no schema
// (walker.pointerTarget): no value, or a value that is no schema.
const (
	targetMissing   int = -1
	targetNotSchema int = -2
)

// node is a schema position some pass collected or a reference named: its
// pointer and schema, what collect reads of it, computed once, and its
// state in the current pass, valid while its pass equals the model's
// passes.
type node struct {
	ptr   string
	v     any
	entry int
	// ready is set once prepare has read the position: whether it is an
	// object, its $id without the fragment (idRef, an index of strOf),
	// its $anchor and $dynamicAnchor (indexes of strOf), each -1 when
	// absent, whether its $schema is refused (badSchema), its references
	// (indexes of siteTab) and its subschemas.
	ready                bool
	obj, badSchema       bool
	idRef, anchor, dynam int
	sites                []int
	kids                 []kidRef
	// known is the reference that first found the position as a target
	// outside what the library collected (an index of siteTab), or -1.
	known int
	// The pass state: how the pass walked it, its index in order, its URL
	// (an index of urlOf) when it is a resource, its index in the prelude
	// when the pass collects it apart, the first reference the pass
	// resolved to it, and, at a resource, its $dynamicAnchor names.
	pass    int
	seen    uint8
	at      int
	resURL  int
	prelude int
	first   int
	dynamic []int
}

// kidRef is a subschema of a node: its node, and whether it is the
// node's additionalItems, which the library collects but does not compile
// under draft 2020-12.
type kidRef struct {
	node       int
	additional bool
}

// trieEntry is an entry of the trie of the pointers: the entry above it, the
// escaped token that leads to it from there, its node or -1, and a memo of
// the resource of the positions at and below it (walker.resourceOf), valid
// while memoEpoch is the model's epoch.
type trieEntry struct {
	parent             int
	tok                string
	node               int
	memoEpoch, memoRes int
}

// trieKey names an entry of the trie by the entry above it and its token.
type trieKey struct {
	parent int
	tok    string
}

// site is a reference of a node, a member of one of its reference keywords:
// its node, its pointer and value, the value interned (ref), and its
// fragment as the library reads it, percent-decoded: badFrag when it does
// not decode, else interned as frag when it is a JSON Pointer ("" included)
// or as anchor when it names an anchor, the other -1. res is its
// resolution in the pass it was last resolved in.
type site struct {
	node              int
	ptr, value        string
	ref, frag, anchor int
	badFrag           bool
	pass              int
	res               siteResolution
}

// siteResolution is a reference's status, its target node when resolved,
// and the URL it resolved to (an index of urlOf), -1 when it was not
// joined.
type siteResolution struct {
	status refStatus
	target int
	url    int
}

// anchorEntry is an anchor of a resource: its node and the step that
// collected it, in the pass that recorded it.
type anchorEntry struct{ node, step, pass int }

// note is a finding the collection makes at the position whose index in
// order is at, rendered only when reported (walker.addNote): a duplicate
// $id (noteID, name its value), a refused $schema (noteSchema), or a
// duplicate anchor (noteAnchor, name the anchor and kw its keyword), prev
// the node that holds the $id or anchor first.
type note struct {
	at, node, prev, name int
	kind                 uint8
	kw                   string
}

// The kinds of note.
const (
	noteID uint8 = iota
	noteSchema
	noteAnchor
)

// foundTarget is a reference target a pass found outside what the library
// had collected, and the reference (an index of siteTab).
type foundTarget struct{ node, site int }

// model holds the tables and the pass state of the walk's model. The
// tables persist across passes: the nodes, the trie of their pointers
// (entries, indexed by trie), the references, the interned strings and
// URLs, the joins of a URL and a reference, and the targets of a fragment
// from a resource. The pass state is in the tables, stamped with the pass
// (nodes, sites, the URLs' resources in idNode, idStep and idPass, the
// anchors), and in the lists below, which each pass starts empty.
type model struct {
	nodes     []node
	entries   []trieEntry
	trie      map[trieKey]int
	siteTab   []site
	strs      map[string]int
	strOf     []string
	urls      map[string]int
	urlOf     []string
	joins     map[[2]int]int
	targetsAt map[[2]int]int
	idNode    []int
	idStep    []int
	idPass    []int
	anchorTab map[[2]int]anchorEntry
	// passes numbers the passes, from 1, and stamps the pass state. epoch
	// changes whenever a pass starts or a resource is recorded, which
	// drops the memos of the resource of the trie entries.
	passes int
	epoch  int
	// order lists the nodes the pass collected, in order; passSites their
	// references, in order; notes the collection's findings; prelude the
	// targets the pass collects apart, in pointer order (checkOrder);
	// queue what close walks live. passIDs and passAnchors count the
	// resources and anchors recorded.
	order                []int
	passSites            []int
	notes                []note
	prelude              []int
	queue                []int
	passIDs, passAnchors int
	// over is set once a pass collects more than MaxSubschemas positions.
	over bool
	// charging is set once the first pass ends: the work of the passes
	// from then on is the fixpoint's, charged to the walk-time compile
	// budget, budget (compileBudget; tests lower it). spent is what it
	// charged, steps of work and bytes met anew; stopped is set once spent
	// exceeds budget, when spent was stoppedAt.
	charging      bool
	budget, spent int
	steps, bytes  int
	stopped       bool
	stoppedAt     int
	// pathA and pathB are the scratch lists of comparePositions.
	pathA, pathB []int
	// marks holds what the fixpoint had spent at the end of each pass, and
	// placed counts the targets insertKnown placed: test instrumentation,
	// by which TestFollowFixpointCost pins the reference a refusal names
	// and that each target is placed once.
	marks  []int
	placed int
}

// newModel returns the model of doc: its root node at the trie's root
// entry, and its URL.
func newModel(doc any) model {
	m := model{
		nodes:     []node{{ptr: "", v: doc, idRef: -1, anchor: -1, dynam: -1, known: -1}},
		entries:   []trieEntry{{parent: -1, node: rootNode}},
		trie:      map[trieKey]int{},
		strs:      map[string]int{},
		urls:      map[string]int{},
		joins:     map[[2]int]int{},
		targetsAt: map[[2]int]int{},
		anchorTab: map[[2]int]anchorEntry{},
		budget:    compileBudget,
	}
	m.urlOf = []string{baseURL}
	m.urls[baseURL] = baseURLIndex
	m.idNode, m.idStep, m.idPass = []int{0}, []int{0}, []int{0}
	return m
}

// step charges one step of the fixpoint's work (charge).
func (w *walker) step() {
	if w.charging {
		w.steps++
		w.spend(1)
	}
}

// charge charges n bytes the fixpoint builds or hashes anew: a pointer, a
// token, a URL or a string it meets for the first time.
func (w *walker) charge(n int) {
	if w.charging {
		w.bytes += n
		w.spend(n)
	}
}

// spend adds n units to what the fixpoint spent, and stops it past the
// budget, keeping what it had spent then in stoppedAt.
func (w *walker) spend(n int) {
	if w.spent += n; w.spent > w.budget && !w.stopped {
		w.stopped, w.stoppedAt = true, w.spent
	}
}

// halted reports whether the pass stops: past MaxSubschemas positions or
// past the budget.
func (w *walker) halted() bool { return w.over || w.stopped }

// beginPass starts a pass: a new stamp for the pass state, and empty
// lists.
func (w *walker) beginPass() {
	w.passes++
	w.epoch++
	w.order, w.passSites, w.notes, w.prelude = w.order[:0], w.passSites[:0], w.notes[:0], w.prelude[:0]
	w.passIDs, w.passAnchors, w.over = 0, 0, false
}

// touch makes the pass state of node n the current pass's, empty when it
// was an earlier pass's.
func (w *walker) touch(n int) {
	nd := &w.nodes[n]
	if nd.pass == w.passes {
		return
	}
	nd.pass, nd.seen, nd.at, nd.resURL, nd.prelude, nd.first, nd.dynamic = w.passes, 0, -1, -1, -1, -1, nd.dynamic[:0]
}

// seenOf returns how the pass walked node n: 0 when it did not collect it.
func (w *walker) seenOf(n int) uint8 {
	if nd := &w.nodes[n]; nd.pass == w.passes {
		return nd.seen
	}
	return 0
}

// isResource reports whether the pass recorded node n as a resource.
func (w *walker) isResource(n int) bool {
	nd := &w.nodes[n]
	return nd.pass == w.passes && nd.resURL >= 0
}

// intern returns the index of s in strOf, adding it the first time.
func (w *walker) intern(s string) int {
	if i, ok := w.strs[s]; ok {
		return i
	}
	w.charge(len(s))
	i := len(w.strOf)
	w.strs[s] = i
	w.strOf = append(w.strOf, s)
	return i
}

// internURL returns the index of the URL u in urlOf, adding it the first
// time with an empty resource.
func (w *walker) internURL(u string) int {
	if i, ok := w.urls[u]; ok {
		return i
	}
	w.charge(len(u))
	i := len(w.urlOf)
	w.urls[u] = i
	w.urlOf = append(w.urlOf, u)
	w.idNode, w.idStep, w.idPass = append(w.idNode, 0), append(w.idStep, 0), append(w.idPass, 0)
	return i
}

// child returns the trie entry below e for the escaped token tok, adding
// it the first time.
func (w *walker) child(e int, tok string) int {
	w.charge(len(tok))
	k := trieKey{e, tok}
	if c, ok := w.trie[k]; ok {
		return c
	}
	c := len(w.entries)
	w.entries = append(w.entries, trieEntry{parent: e, tok: tok, node: -1})
	w.trie[k] = c
	return c
}

// nodeAt returns the node at trie entry e, whose schema is v, adding it the
// first time with the pointer prefix+suffix.
func (w *walker) nodeAt(e int, v any, prefix, suffix string) int {
	if n := w.entries[e].node; n >= 0 {
		return n
	}
	ptr := prefix + suffix
	w.charge(len(ptr))
	n := len(w.nodes)
	w.nodes = append(w.nodes, node{ptr: ptr, v: v, entry: e, idRef: -1, anchor: -1, dynam: -1, known: -1})
	w.entries[e].node = n
	return n
}

// prepare reads node n once: its $id, anchors, $schema, references and
// subschemas, each subschema a node of its own.
func (w *walker) prepare(n int) {
	if w.nodes[n].ready {
		return
	}
	w.nodes[n].ready = true
	obj, ok := w.nodes[n].v.(map[string]any)
	if !ok {
		return
	}
	idRef, anchor, dynam := -1, -1, -1
	// As the library: an $id without its fragment starts a resource.
	if id, ok := obj["$id"].(string); ok {
		if id, _, _ = strings.Cut(id, "#"); id != "" {
			idRef = w.intern(id)
		}
	}
	if s, ok := obj["$anchor"].(string); ok {
		anchor = w.intern(s)
	}
	if s, ok := obj["$dynamicAnchor"].(string); ok {
		dynam = w.intern(s)
	}
	var sites []int
	for _, kw := range referenceKeywords() {
		if s, ok := obj[kw].(string); ok {
			sites = append(sites, w.newSite(n, kw, s))
		}
	}
	ptr, e := w.nodes[n].ptr, w.nodes[n].entry
	var kids []kidRef
	eachSubschemaToken(obj, func(v any, kw, tok string, member bool) {
		w.step()
		c, suffix := w.child(e, kw), "/"+kw
		if member {
			c, suffix = w.child(c, tok), suffix+"/"+tok
		}
		kids = append(kids, kidRef{w.nodeAt(c, v, ptr, suffix), kw == "additionalItems"})
	})
	nd := &w.nodes[n]
	nd.obj, nd.badSchema = true, badDialect(obj)
	nd.idRef, nd.anchor, nd.dynam, nd.sites, nd.kids = idRef, anchor, dynam, sites, kids
}

// newSite adds the reference value of keyword kw at node n.
func (w *walker) newSite(n int, kw, value string) int {
	ptr := w.nodes[n].ptr + "/" + kw
	w.charge(len(ptr))
	s := site{node: n, ptr: ptr, value: value, ref: w.intern(value), frag: -1, anchor: -1}
	_, raw, _ := strings.Cut(value, "#")
	frag, err := url.PathUnescape(raw)
	switch {
	case err != nil:
		s.badFrag = true
	case frag == "" || frag[0] == '/':
		s.frag = w.intern(frag)
	default:
		s.anchor = w.intern(frag)
	}
	w.siteTab = append(w.siteTab, s)
	return len(w.siteTab) - 1
}

// joinID returns the URL (an index of urlOf) of the reference or $id ref
// (an index of strOf) joined with the base URL base, or -1 when it is not
// a URI reference, computed once per base and reference.
func (w *walker) joinID(base, ref int) int {
	k := [2]int{base, ref}
	if u, ok := w.joins[k]; ok {
		return u
	}
	b, r := w.urlOf[base], w.strOf[ref]
	w.charge(len(b) + len(r))
	u := -1
	if s, ok := joinURL(b, r); ok {
		u = w.internURL(s)
	}
	w.joins[k] = u
	return u
}

// idOf returns the resource the pass recorded for the URL u and the step
// that recorded it.
func (w *walker) idOf(u int) (n, step int, ok bool) {
	if w.idPass[u] != w.passes {
		return -1, 0, false
	}
	return w.idNode[u], w.idStep[u], true
}

// anchorKey is the key of the anchor name (an index of strOf) of the
// resource node res in anchorTab.
func anchorKey(res, name int) [2]int { return [2]int{res, name} }

// anchorOf returns the anchor name (an index of strOf) of the resource
// node res as the pass recorded it.
func (w *walker) anchorOf(res, name int) (anchorEntry, bool) {
	a, ok := w.anchorTab[anchorKey(res, name)]
	return a, ok && a.pass == w.passes
}

// addNote reports the collection finding nt, rendering it only while the
// walk keeps findings.
func (w *walker) addNote(nt note) {
	if len(w.issues) >= MaxIssues {
		return
	}
	ptr := w.nodes[nt.node].ptr
	switch nt.kind {
	case noteID:
		w.add(appendToken(ptr, "$id"), fmt.Sprintf("$id %q is already the identifier of the schema at %q", w.strOf[nt.name], w.nodes[nt.prev].ptr))
	case noteSchema:
		w.add(appendToken(ptr, "$schema"), msgDialect)
	default:
		w.add(appendToken(ptr, nt.kw), fmt.Sprintf("anchor %q is already defined by the schema at %q", w.strOf[nt.name], w.nodes[nt.prev].ptr))
	}
}

// collect records node n as the library collects a schema position
// (roots.go:81-191), under the base URL base, whose resource is the node
// res, at step: unless the pass collected it already, the position, its
// $id, which starts a resource, the anchors it gives the resource of its
// base, its references, and every position its subschema keywords reach,
// below additionalItems too, each inert until close makes it live. A
// duplicate $id or anchor is a finding (note): the $id names the resource
// of the schema that holds it first, the one the library's lookup of the
// base finds (roots.go:150-159), and the anchor defines nothing. The
// $schema rule applies at every position collected: the library reads a
// $schema at each, before it reads the $id, and loads one that names no
// draft (roots.go:94-110). Past MaxSubschemas positions, or past the
// budget, nothing more is collected.
func (w *walker) collect(n, base, res, step int) {
	w.step()
	if w.halted() || w.seenOf(n) != 0 {
		return
	}
	w.touch(n)
	at := len(w.order)
	w.nodes[n].seen, w.nodes[n].at = walkedInert, at
	w.order = append(w.order, n)
	if len(w.order) > MaxSubschemas {
		w.over = true
		return
	}
	w.prepare(n)
	nd := w.nodes[n]
	if nd.idRef >= 0 {
		if u := w.joinID(base, nd.idRef); u >= 0 {
			base = u
			if prev, _, dup := w.idOf(u); !dup {
				res = n
			} else if prev != n {
				w.notes = append(w.notes, note{at: at, node: n, prev: prev, name: nd.idRef, kind: noteID})
				res = prev
			}
		}
	}
	if res == n {
		// A position that starts a resource, or the document's root,
		// which walk collects as its own resource whatever it holds.
		if _, _, dup := w.idOf(base); !dup {
			w.idNode[base], w.idStep[base], w.idPass[base] = n, step, w.passes
			w.passIDs++
			w.nodes[n].resURL = base
			w.epoch++
		}
		res = n
	}
	if !nd.obj {
		return
	}
	if nd.badSchema {
		w.notes = append(w.notes, note{at: at, node: n, kind: noteSchema})
	}
	for _, a := range [2]struct {
		kw   string
		name int
	}{{"$anchor", nd.anchor}, {"$dynamicAnchor", nd.dynam}} {
		if a.name < 0 {
			continue
		}
		if prev, dup := w.anchorOf(res, a.name); !dup {
			w.anchorTab[anchorKey(res, a.name)] = anchorEntry{node: n, step: step, pass: w.passes}
			w.passAnchors++
		} else if prev.node != n {
			w.notes = append(w.notes, note{at: at, node: n, prev: prev.node, name: a.name, kind: noteAnchor, kw: a.kw})
			continue
		}
		if a.kw == "$dynamicAnchor" {
			w.nodes[res].dynamic = append(w.nodes[res].dynamic, a.name)
		}
	}
	w.passSites = append(w.passSites, nd.sites...)
	for _, k := range nd.kids {
		if w.halted() {
			break
		}
		w.collect(k.node, base, res, step)
	}
}

// collectTarget collects the reference target t, a node the pass has not
// collected, as the library collects a target it checks apart
// (roots.go:193-235): under the resource of the longest pointer prefix of
// its pointer collected by then, with its keyword tree.
func (w *walker) collectTarget(t, step int) {
	res := w.resourceOf(t)
	w.collect(t, w.nodes[res].resURL, res, step)
}

// resourceOf returns the resource the library gives node n (root.go:20-32):
// the node of the longest pointer prefix of n's pointer that the pass
// recorded as a resource, or the root, found up the trie from n's entry
// and memoized on every entry it passes until the next resource is
// recorded.
func (w *walker) resourceOf(n int) int {
	// The entries from start up to stop, which is excluded, get the memo:
	// up to the entry of the resource found, or up to one memoized already.
	start := w.nodes[n].entry
	r, stop := rootNode, -1
	for e := start; e >= 0; e = w.entries[e].parent {
		w.step()
		ent := &w.entries[e]
		if ent.memoEpoch == w.epoch {
			r, stop = ent.memoRes, e
			break
		}
		if ent.node >= 0 && w.isResource(ent.node) {
			r, stop = ent.node, ent.parent
			break
		}
	}
	for e := start; e != stop; e = w.entries[e].parent {
		w.entries[e].memoEpoch, w.entries[e].memoRes = w.epoch, r
	}
	return r
}

// close walks live, from the root's keyword tree and the targets of the
// prelude, everything the library compiles from them (compiler.go:190-262):
// with each position, the root of its resource, the $dynamicAnchor schemas
// of a resource root, and the target of each of its references, which it
// resolves; and with each, the keyword tree below it but for
// additionalItems. A target not collected yet it collects at once and
// returns, with the reference that found it, in the order found.
//
// A target of the prelude compiles right after its own check, before the
// targets after it are collected, so its own references and $dynamicAnchor
// schemas see only the resources and anchors collected up to its step;
// everything else compiles after every target is collected, and sees them
// all (compileWalked).
func (w *walker) close() []foundTarget {
	w.queue = w.queue[:0]
	w.makeLive(rootNode)
	for _, t := range w.prelude {
		w.makeLive(t)
	}
	var found []foundTarget
	for i := 0; i < len(w.queue) && !w.halted(); i++ {
		p := w.queue[i]
		w.step()
		limit := math.MaxInt
		if st := w.nodes[p].prelude; st >= 0 {
			limit = st
		}
		res := w.resourceOf(p)
		w.makeLive(res)
		if res == p {
			for _, name := range w.nodes[p].dynamic {
				if w.stopped {
					break
				}
				w.step()
				if a, ok := w.anchorOf(p, name); ok && a.step <= limit {
					w.makeLive(a.node)
				}
			}
		}
		for _, si := range w.nodes[p].sites {
			if w.stopped {
				break
			}
			w.step()
			r := w.resolve(si, limit)
			w.siteTab[si].pass, w.siteTab[si].res = w.passes, r
			if r.status != refResolved {
				continue
			}
			t := r.target
			if w.seenOf(t) == 0 {
				// At the step after the prelude's: a step is compared only
				// with that of a target of the prelude (resolve, and the
				// $dynamicAnchor schemas above), so the targets a pass
				// finds share one.
				w.collectTarget(t, len(w.prelude))
				found = append(found, foundTarget{t, si})
			}
			w.touch(t)
			if w.nodes[t].first < 0 {
				w.nodes[t].first = si
			}
			w.makeLive(t)
		}
	}
	return found
}

// makeLive walks the collected node n live, with every position its
// subschema keywords reach but additionalItems, which the library does not
// compile under draft 2020-12, and queues each for close. Past the budget
// every call returns at once, before its step, so it walks nothing more;
// past MaxSubschemas positions it still walks what was collected, which
// the walk's findings then do not use.
func (w *walker) makeLive(n int) {
	if w.stopped {
		return
	}
	w.step()
	if w.seenOf(n) != walkedInert {
		return
	}
	w.nodes[n].seen = walkedLive
	w.queue = append(w.queue, n)
	for _, k := range w.nodes[n].kids {
		if !k.additional {
			w.makeLive(k.node)
		}
	}
}

// resolve resolves the reference si as the library does when it compiles
// it (objcompiler.go:361-384, root.go:34-76): against the base URI of the
// resource of its schema, the URL without its fragment naming the document
// or a resource collected by step limit, and the fragment, percent-decoded,
// a JSON Pointer from that resource or an anchor it defines by then.
func (w *walker) resolve(si, limit int) siteResolution {
	s := w.siteTab[si]
	out := siteResolution{target: -1, url: -1}
	u := w.joinID(w.nodes[w.resourceOf(s.node)].resURL, s.ref)
	if u < 0 {
		out.status = refBadURI
		return out
	}
	out.url = u
	if s.badFrag {
		out.status = refBadFragment
		return out
	}
	// The document's own URL always names its root (root.go:58-62).
	res := rootNode
	if u != baseURLIndex {
		n, step, ok := w.idOf(u)
		if !ok || step > limit {
			out.status = refOutside
			return out
		}
		res = n
	}
	if s.frag >= 0 {
		switch t := w.pointerTarget(res, s.frag); t {
		case targetMissing:
			out.status = refMissing
		case targetNotSchema:
			out.status = refNotSchema
		default:
			out.status, out.target = refResolved, t
		}
		return out
	}
	a, ok := w.anchorOf(res, s.anchor)
	if !ok || a.step > limit {
		out.status = refNoAnchor
		return out
	}
	out.status, out.target = refResolved, a.node
	return out
}

// pointerTarget returns the node the JSON Pointer frag (an index of strOf)
// names from the resource node res, or targetMissing or targetNotSchema,
// computed once per resource and fragment.
func (w *walker) pointerTarget(res, frag int) int {
	k := [2]int{res, frag}
	if t, ok := w.targetsAt[k]; ok {
		return t
	}
	t := w.walkPointer(res, w.strOf[frag])
	w.targetsAt[k] = t
	return t
}

// walkPointer resolves the JSON Pointer frag, "" or starting with "/", from
// the node res as the library does (lookup): an object member by name, an
// array element by a decimal index; the value named is a node when it is a
// schema, at the trie entry of its tokens.
func (w *walker) walkPointer(res int, frag string) int {
	w.charge(len(frag))
	toks, _ := splitPointer(frag)
	v, e := w.nodes[res].v, w.nodes[res].entry
	for _, raw := range toks {
		w.step()
		tok, ok := unescapeToken(raw)
		if !ok {
			return targetMissing
		}
		switch t := v.(type) {
		case map[string]any:
			if v, ok = t[tok]; !ok {
				return targetMissing
			}
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(t) {
				return targetMissing
			}
			v = t[i]
		default:
			return targetMissing
		}
		e = w.child(e, raw)
	}
	switch v.(type) {
	case map[string]any, bool:
		return w.nodeAt(e, v, w.nodes[res].ptr, frag)
	}
	return targetNotSchema
}

// insertKnown adds the targets a pass found to known, which lists every
// target found so far in pointer order, as a stable sort of the list with
// them appended would: each after every target ordered before or with it.
//
// Placing the targets charges nothing to the walk-time compile budget,
// whose fixpoint term charges the work the passes after the first repeat
// or meet anew (charging both tokens of every comparison would refuse
// two-pass documents of 500 targets with 210-byte names, which otherwise
// compile in about 0.1 s, at 1.47 million units). A pass finds only
// targets no earlier pass found, so each target is placed once in the
// whole walk, by a binary search of about log2 K comparisons among K
// targets (comparePositions), and the targets are at most MaxSubschemas+1,
// every one being collected in the pass that places it and the walk ending
// past MaxSubschemas positions: placing them is bounded by the targets,
// not by the passes. The comparisons read the targets' pointers, kept once
// as the tokens of the trie, whose bytes times their uses are the subject
// of the budget's byte term, like the first pass's work.
func (w *walker) insertKnown(known, found []foundTarget) []foundTarget {
	for _, f := range found {
		i := sort.Search(len(known), func(i int) bool { return w.comparePositions(known[i].node, f.node) > 0 })
		known = slices.Insert(known, i, f)
		w.nodes[f.node].known = f.site
		w.placed++
	}
	return known
}

// comparePositions orders the pointers of nodes a and b as comparePointers
// does, token by token, from the trie: the tokens above the entry they
// share are equal, so it compares those below it. It charges nothing
// (insertKnown).
func (w *walker) comparePositions(a, b int) int {
	w.pathA = w.entryPath(w.nodes[a].entry, w.pathA)
	w.pathB = w.entryPath(w.nodes[b].entry, w.pathB)
	pa, pb := w.pathA, w.pathB
	i := 0
	for i < len(pa) && i < len(pb) && pa[i] == pb[i] {
		i++
	}
	for ; i < len(pa) && i < len(pb); i++ {
		if c := compareTokens(w.entries[pa[i]].tok, w.entries[pb[i]].tok); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(pa), len(pb))
}

// entryPath returns in buf the trie entries from the root's down to e, the
// root's excluded.
func (w *walker) entryPath(e int, buf []int) []int {
	buf = buf[:0]
	for ; e > 0; e = w.entries[e].parent {
		buf = append(buf, e)
	}
	slices.Reverse(buf)
	return buf
}
