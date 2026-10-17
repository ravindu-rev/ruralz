// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// linearScan is the most members or entries a lookup scans one by one.
// Longer objects and keyed lists are indexed by key once per merge, so a
// merge costs time linear in its input (01 req 54).
const linearScan = 8

// merger merges one overlay document, or checks one base document; id is
// the resource every diagnostic names.
type merger struct {
	a  *applier
	id tree.ID
	// stack is the key-aware path of the current position from the
	// resource root, pushed and popped as the walk descends and copied
	// only into a diagnostic, bounded (here). A list element's path
	// element is computed from the element as the walk reaches it
	// (itemElem).
	stack diag.Path
	// shown holds the first elements of stack as a diagnostic shows them
	// (boundElem): here bounds the elements past it once for every
	// diagnostic below them, and pop cuts it back to the stack.
	shown diag.Path
	// resource is the identity every diagnostic of the document names,
	// made for the first one (resourceID).
	resource *diag.ResourceID
	// recheck counts the enclosing objects whose dispatch the overlay
	// changed; while it is positive, the outermost one rechecks its whole
	// subtree (see mergeMap), so nested objects skip their own check.
	recheck int
}

// errorAt reports a diagnostic at the current position, extra appended to
// its path, copied with its input text bounded (here); the resource is
// named as resourceID shows it.
func (m *merger) errorAt(code string, p tree.Pos, extra []diag.PathElem, msg, hint string, related ...diag.Related) {
	if m.resource == nil {
		m.resource = resourceID(m.id)
	}
	r := *m.resource
	m.a.report(diag.Diagnostic{
		Code: code, Location: m.a.loc(p), Resource: &r, Path: m.here(extra),
		Message: msg, Hint: hint, Related: related,
	})
}

// here returns a copy of the path of the current position, extra
// appended, with the input text of every element bounded (boundElem), so
// up to 10,001 diagnostics below one long key or list key print in space
// linear in their count (review finding).
func (m *merger) here(extra []diag.PathElem) diag.Path {
	for j := len(m.shown); j < len(m.stack); j++ {
		m.a.tick()
		m.shown = append(m.shown, boundElem(m.stack[j]))
	}
	out := make(diag.Path, 0, len(m.shown)+len(extra))
	for _, e := range m.shown {
		m.a.tick()
		out = append(out, e)
	}
	for _, e := range extra {
		m.a.tick()
		out = append(out, boundElem(e))
	}
	return out
}

// push appends e to the path of the current position; pop removes it.
func (m *merger) push(e diag.PathElem) { m.stack = append(m.stack, e) }

func (m *merger) pop() {
	m.stack = m.stack[:len(m.stack)-1]
	if len(m.shown) > len(m.stack) {
		m.shown = m.shown[:len(m.stack)]
	}
}

// takeAction removes the ruralz.io/patch annotation from an overlay
// document root and returns the value of its first member: "" (merge),
// PatchDelete or PatchReplace. ok is false for an unsupported value
// (RZ-CFG-005); the document is then not applied. The annotation never
// survives into the result (01 req 25), for any input: every
// ruralz.io/patch member of the annotations object is removed (review
// finding: only the first one was, and a second one was merged into the
// result), and an annotations object left empty is removed with every
// later annotations member of metadata, which the first one's key hides
// (mergeMap merges the first member of a key) and whose removal would
// otherwise bring into the merge. Stage B leaves every key once
// (RZ-CFG-002), so the pipeline meets one of each.
func (m *merger) takeAction(root *tree.Node) (action string, ok bool) {
	md, _ := m.a.get(root, "metadata")
	ann, _ := m.a.get(md, "annotations")
	v, found := m.a.get(ann, PatchAnnotation)
	if !found {
		return "", true
	}
	ann.Members = deleteFunc(m.a, ann.Members, func(a tree.Member) bool { return a.Key == PatchAnnotation })
	if len(ann.Members) == 0 {
		md.Members = deleteFunc(m.a, md.Members, func(mem tree.Member) bool { return mem.Key == "annotations" })
	}
	if v.Kind == tree.KindString && (v.Text == PatchDelete || v.Text == PatchReplace) {
		return v.Text, true
	}
	m.errorAt(CodeSchema, v.Pos,
		[]diag.PathElem{diag.Field("metadata"), diag.Field("annotations"), diag.Field(PatchAnnotation)},
		"unsupported "+PatchAnnotation+" value "+scalarText(v), "use delete or replace")
	return "", false
}

// replaceSpec prepares the root of an overlay document carrying
// ruralz.io/patch: replace, whose spec replaces the base spec whole (01
// req 25): the spec is the overlay's with its directives consumed, kept
// even when nothing but removals remain in it, as {$patch: replace} keeps
// a list. The merge then reads it as a value merged against no base.
func (m *merger) replaceSpec(root *tree.Node, static *schemaidx.Node) {
	sel := m.a.selectOf(static, root)
	for i := range root.Members {
		m.a.tick()
		if mem := &root.Members[i]; mem.Key == "spec" && composite(mem.Value) {
			m.push(diag.Field(mem.Key))
			mem.Value = m.element(mem.Value, memberSchema(sel, mem.Key))
			m.pop()
		}
	}
}

// opaque reports the JSONSchemaDocument position, whose value an overlay
// replaces whole and nothing inside it is merged.
func opaque(s *schemaidx.Node) bool { return s.Def() == schemaDocumentDef }

// memberSchema returns the schema of member name of an object whose
// dispatched schema is sel: the declared property, else the typed-map
// value schema, else nil (free content or an unknown member).
func memberSchema(sel *schemaidx.Node, name string) *schemaidx.Node {
	if c, ok := sel.Property(name); ok {
		return c
	}
	if v, ok := sel.Values(); ok {
		return v
	}
	return nil
}

// itemSchema returns the schema of list elements, or nil.
func itemSchema(list *schemaidx.Node) *schemaidx.Node {
	it, _ := list.Items()
	return it
}

func composite(n *tree.Node) bool { return n.Kind == tree.KindMap || n.Kind == tree.KindList }

// merge merges overlay value ov into base value b (nil when absent) at a
// position whose undispatched schema is static and returns the result:
// nil when b is absent and ov holds nothing but removals (normalize), so
// the caller adds no member. Removals against a base value of another
// kind remove nothing and leave b in place (01 req 25 "absent key:
// no-op").
func (m *merger) merge(b, ov *tree.Node, static *schemaidx.Node) *tree.Node {
	if b == nil || opaque(static) {
		return m.normalize(ov, static)
	}
	switch {
	case ov.Kind == tree.KindMap && b.Kind == tree.KindMap:
		return m.mergeMap(b, ov, static)
	case ov.Kind == tree.KindList && b.Kind == tree.KindList && keyed(static):
		return m.mergeKeyed(b, ov, static)
	default:
	}
	if n := m.normalize(ov, static); n != nil {
		return n
	}
	return b
}

// normalize returns ov merged against an empty base: directives are
// consumed, null members dropped, and misplaced directives reported.
//
// It returns nil for an object with members or a keyed list with entries
// of which nothing but removals remains: null members, $patch: delete
// entries and, at any depth, members whose own value is such a removal.
// Removing what is absent changes nothing (01 req 25 "absent key: no-op",
// 01 risk 18), so the caller adds no member, as Kubernetes strategic
// merge does for a delete-only list (contract change request against 01
// req 25 for the object case). A written {} or [], an atomic or set list
// and an applied {$patch: replace} are values: they stay, empty or not.
func (m *merger) normalize(ov *tree.Node, static *schemaidx.Node) *tree.Node {
	if opaque(static) {
		return ov
	}
	switch ov.Kind {
	case tree.KindMap:
		n := m.mergeMap(&tree.Node{Kind: tree.KindMap, Style: ov.Style, Pos: ov.Pos}, ov, static)
		if len(n.Members) == 0 && len(ov.Members) > 0 {
			return nil
		}
		return n
	case tree.KindList:
		if keyed(static) {
			empty := &tree.Node{Kind: tree.KindList, Style: ov.Style, Pos: ov.Pos}
			n := m.mergeKeyed(empty, ov, static)
			if n == empty && len(n.Items) == 0 && len(ov.Items) > 0 {
				return nil
			}
			return n
		}
		it := itemSchema(static)
		kept := ov.Items[:0]
		for i, e := range ov.Items {
			m.a.tick()
			if m.a.full {
				break
			}
			if !composite(e) {
				kept = append(kept, e)
				continue
			}
			m.push(m.a.itemElem(static, e, i))
			if e.Kind == tree.KindMap && !opaque(it) && !m.wholeListEntry(e, static) {
				m.pop()
				continue
			}
			kept = append(kept, m.element(e, it))
			m.pop()
		}
		clearSteps(m.a, ov.Items[len(kept):])
		ov.Items = kept
		return ov
	default:
		return ov
	}
}

// element is normalize for a value that stays even when nothing but
// removals remain in it, which it then holds as an empty object or list:
// an element of a list replaced whole, whose length the overlay wrote, a
// new keyed-list entry (its key always stays) and a document root.
func (m *merger) element(e *tree.Node, static *schemaidx.Node) *tree.Node {
	if n := m.normalize(e, static); n != nil {
		return n
	}
	return &tree.Node{Kind: e.Kind, Style: e.Style, Pos: e.Pos}
}

// wholeListEntry reports a $patch member of e, an object element of a
// list the overlay replaces whole (set, atomic or without x-ruralz-list)
// at the current position, as RZ-CFG-005 (01 req 25) and removes it. It
// returns false when e held only the directive, which is then dropped
// from the list.
func (m *merger) wholeListEntry(e *tree.Node, list *schemaidx.Node) bool {
	j := m.a.memberAt(e, PatchKey)
	if j < 0 {
		return true
	}
	hint := wholeList(list) + " is replaced whole: write the complete list"
	if replaceDirective(e) {
		m.errorAt(CodeSchema, e.Pos, nil, "{$patch: replace} is valid only in an orderedMap list", hint)
	} else {
		m.errorAt(CodeSchema, e.Members[j].KeyPos, []diag.PathElem{diag.Field(PatchKey)},
			"$patch is valid only in an entry of a map or orderedMap list", hint)
	}
	e.Members = deleteAt(m.a, e.Members, j)
	return len(e.Members) > 0
}

// wholeList names a list an overlay replaces whole, for hints.
func wholeList(list *schemaidx.Node) string {
	switch lt, _ := list.List(); lt {
	case schemaidx.ListNone:
		return "a list without x-ruralz-list"
	case schemaidx.ListAtomic:
		return "an atomic list"
	default:
		return "a " + lt.String() + " list"
	}
}

// keyed reports a map or orderedMap list with a key field.
func keyed(list *schemaidx.Node) bool {
	lt, key := list.List()
	return lt.Keyed() && key != ""
}

// pendingMember is a composite member mergeMap resolves once the merged
// object's dispatch is known.
type pendingMember struct {
	// at indexes the member in the merged object.
	at int
	// from indexes the overlay member that supplies ov.
	from int
	key  string
	// base is the base value, nil when the base lacks the member.
	base *tree.Node
	ov   *tree.Node
	// keyPos is the overlay's key position, which the member takes when
	// the overlay supplies its value.
	keyPos tree.Pos
	// settled marks a member settle merged before the dispatch was read.
	settled bool
}

// replaced reports a member whose base value the overlay replaces with a
// value of another kind; the base value comes back when that value holds
// nothing but removals (merge).
func (t pendingMember) replaced() bool { return t.base != nil && t.base.Kind != t.ov.Kind }

// overScalar reports a member whose base value is a scalar the overlay
// replaces with a composite, which settle merges before the dispatch is
// read when the dispatch reads the member.
func (t pendingMember) overScalar() bool { return t.base != nil && !composite(t.base) }

// mergeMap merges overlay object ov into base object b (01 req 25):
// members merge recursively, an overlay scalar or non-keyed list wins, an
// overlay null removes the member, and new members are appended in
// overlay order. Scalars and removals apply first, so the dispatch that
// selects the composite members' schemas (a Policy config by spec.type)
// reads the merged object. An overlay scalar equal to the base scalar
// keeps the base node and its position; a member whose value the overlay
// supplies takes the overlay's key position. A new member whose value
// holds nothing but removals is not added (normalize). An object's keys
// are unique (stage B keeps the first member of a key, RZ-CFG-002), and
// so an overlay member whose key an earlier member of ov had is skipped:
// the first member of a key applies, as the profile keeps it, and
// nothing else changes.
//
// The dispatch is read once scalars and removals apply and settle has
// merged the members the dispatch reads whose base scalar the overlay
// replaces with a composite: such a base scalar stays when that value
// holds nothing but removals, so a Policy spec.type the overlay writes as
// {a: null} is still the base type. The other pending values do not
// decide it: every if/then rule of the rendered schema tests a scalar
// member it requires to equal a constant (TestDispatchTestsScalars), which
// no composite satisfies, so a new member reads the same present or
// dropped as a removal, and a base composite, which stays in place while
// the dispatch is read, the same as an overlay composite of the other kind
// that replaces it.
//
// When the overlay changes the object's dispatch, base content keeps its
// place but is read under the merged schema, so a $patch that was data
// inside a JSON Schema document can end up outside one: redispatched
// reports and removes it.
func (m *merger) mergeMap(b, ov *tree.Node, static *schemaidx.Node) *tree.Node {
	m.a.tick()
	// orig is the base object's dispatch, as CheckBase read it: no
	// enclosing object's dispatch changed (recheck is 0), so static is
	// the base view of this position. An empty base (normalize) holds no
	// base content to recheck.
	var orig *schemaidx.Node
	track := m.recheck == 0 && len(b.Members) > 0
	if track {
		orig = m.a.selectOf(static, b)
	}
	keys := m.a.memberIndex(b.Members, len(ov.Members))
	var buf [linearScan]pendingMember // most objects: no heap allocation
	todo := buf[:0]
	// A member the first pass removes keeps its slot with a nil value
	// until the merge is done, so every pending member's index stays
	// valid; selectOf reads such a member as absent.
	removed, scalarsReplaced := false, false
	// blame indexes the first overlay member a dispatch rule tests whose
	// member the merge changes, the related location of a redispatch
	// diagnostic (dispatchPos): the first pass finds the scalar changes,
	// the merge of the pending members a composite replacing a scalar.
	blame := -1
	for j, om := range ov.Members {
		m.a.tick()
		if om.Key == PatchKey {
			m.misplaced(om.KeyPos, static)
			continue
		}
		i, seen := keys.claim(ov.Members, j)
		if seen {
			continue
		}
		v := om.Value
		switch {
		case v.Kind == tree.KindNull:
			if i >= 0 {
				b.Members[i].Value, removed = nil, true
				blame = m.blamed(blame, j, om, static)
			}
		case i < 0:
			b.Members = append(b.Members, tree.Member{Key: om.Key, KeyPos: om.KeyPos, Value: v})
			if composite(v) {
				todo = append(todo, pendingMember{at: len(b.Members) - 1, from: j, key: om.Key, ov: v, keyPos: om.KeyPos})
			} else {
				blame = m.blamed(blame, j, om, static)
			}
		case !composite(v):
			if !scalarEqual(b.Members[i].Value, v) {
				b.Members[i].KeyPos, b.Members[i].Value = om.KeyPos, v
				blame = m.blamed(blame, j, om, static)
			}
		default:
			t := pendingMember{at: i, from: j, key: om.Key, base: b.Members[i].Value, ov: v, keyPos: om.KeyPos}
			scalarsReplaced = scalarsReplaced || t.overScalar()
			todo = append(todo, t)
		}
	}
	if scalarsReplaced {
		blame = m.settle(b, static, ov, todo, blame)
	}
	sel := m.a.selectOf(static, b)
	changed := track && sel != orig
	var held map[*tree.Node]bool
	if changed {
		held = m.exemptHolders(b, orig, todo)
		m.recheck++
	}
	dropped := removed
	for _, t := range todo {
		if m.a.full {
			break
		}
		if t.settled {
			continue
		}
		m.push(diag.Field(t.key))
		res := m.merge(t.base, t.ov, memberSchema(sel, t.key))
		m.pop()
		mem := &b.Members[t.at]
		switch res {
		case nil:
			mem.Value, dropped = nil, true
		case t.base:
			mem.Value = res
		default:
			mem.KeyPos, mem.Value = t.keyPos, res
			if t.overScalar() && (blame < 0 || t.from < blame) && m.tested(static, ov.Members[t.from]) {
				blame = t.from
			}
		}
	}
	if dropped {
		b.Members = deleteFunc(m.a, b.Members, func(mem tree.Member) bool { return mem.Value == nil })
	}
	if changed {
		m.recheck--
		if len(held) > 0 {
			m.redispatched(b, sel, held, dispatchPos(ov, blame))
		}
	}
	return b
}

// settle merges, before mergeMap reads the dispatch of b, each pending
// member of todo that the dispatch reads and whose base value is a scalar
// the overlay replaces with a composite, marks it settled and returns
// blame updated as mergeMap updates it. The dispatch must read the value
// the merge leaves in such a member, not the overlay value (01 req 25:
// the type dispatch uses the merged spec.type): the base scalar when the
// overlay value holds nothing but removals (merge, 01 req 25 "absent key:
// no-op"), what remains of the overlay value otherwise. The member is
// merged under its schema in the dispatch the base scalars give, which is
// the schema the merged dispatch gives it, since no branch of the
// rendered schema changes the schema of a member its rules test
// (TestDispatchMembersFixed). It is merged once, its diagnostics reported
// within the budget, and no value is copied (review finding: a trial
// normalized a copy of every such value, the members no rule reads
// included, before the merge and past the diagnostic budget).
//
// Every other such member keeps its base scalar while the dispatch is
// read and merges with the other pending members: the dispatch reads the
// same whatever value the merge leaves in it. When standing every such
// overlay value in at once leaves the dispatch as the base scalars give
// it, the dispatch reads none of them. Otherwise a member the dispatch
// reads is a declared property (TestDispatchReadsDeclared) whose overlay
// value standing in alone changes the dispatch (reads). Both tests rest
// on the dispatch being monotone: a composite fails every rule that tests
// its member, so standing more of them in only fails more rules, and a
// failing rule adds no schema. That holds while every rule tests scalar
// constants (TestDispatchTestsScalars) and has no else branch, which
// would add a schema when its rule fails (TestDispatchTestsScalars checks
// the rendered schema has none), and while one rule decides each
// dispatch, so selectOf results compare by pointer (a contract change
// request asks schemaidx to memoize the variants of several rules).
// settle does nothing once the diagnostic budget is spent.
func (m *merger) settle(b *tree.Node, static *schemaidx.Node, ov *tree.Node, todo []pendingMember, blame int) int {
	if m.a.full {
		return blame
	}
	sel := m.a.selectOf(static, b)
	m.standIn(b, todo, true)
	same := m.a.selectOf(static, b) == sel
	m.standIn(b, todo, false)
	if same {
		return blame
	}
	for i := range todo {
		m.a.tick()
		todo[i].settled = todo[i].overScalar() && m.reads(b, static, sel, todo[i])
	}
	for i := range todo {
		m.a.tick()
		t := &todo[i]
		if !t.settled || m.a.full {
			continue
		}
		m.push(diag.Field(t.key))
		res := m.merge(t.base, t.ov, memberSchema(sel, t.key))
		m.pop()
		if res == t.base {
			continue
		}
		mem := &b.Members[t.at]
		mem.KeyPos, mem.Value = t.keyPos, res
		if (blame < 0 || t.from < blame) && m.tested(static, ov.Members[t.from]) {
			blame = t.from
		}
	}
	return blame
}

// reads reports whether the dispatch of b, whose base scalars give sel,
// reads t, a pending member whose base value is a scalar: whether t is a
// declared property of static or sel (every member a rule reads is one,
// TestDispatchReadsDeclared) whose overlay value standing in alone
// changes the dispatch. A member no rule reads costs one property lookup,
// however many there are.
func (m *merger) reads(b *tree.Node, static, sel *schemaidx.Node, t pendingMember) bool {
	_, declared := static.Property(t.key)
	if !declared {
		_, declared = sel.Property(t.key)
	}
	if !declared {
		return false
	}
	b.Members[t.at].Value = t.ov
	changes := m.a.selectOf(static, b) != sel
	b.Members[t.at].Value = t.base
	return changes
}

// standIn puts the overlay value of every member settle resolves in the
// object, or with in false the base scalar back.
func (m *merger) standIn(b *tree.Node, todo []pendingMember, in bool) {
	for _, t := range todo {
		m.a.tick()
		if t.overScalar() {
			b.Members[t.at].Value = t.base
			if in {
				b.Members[t.at].Value = t.ov
			}
		}
	}
}

// blamed returns j when blame is unset and om, overlay member j, which
// changes its member, is tested by a dispatch rule of static (see
// dispatchPos).
func (m *merger) blamed(blame, j int, om tree.Member, static *schemaidx.Node) int {
	if blame < 0 && m.tested(static, om) {
		return j
	}
	return blame
}

// tested reports an overlay member that an if/then rule of static tests
// (DispatchValues lists the string constants; a member tested only
// against a boolean, number or null is not found) and whose value has a
// position.
func (m *merger) tested(static *schemaidx.Node, om tree.Member) bool {
	return om.Value.Pos.Known() && m.a.dispatchTested(static, om.Key)
}

// exemptHolders returns the objects holding a $patch member inside the
// JSONSchemaDocument values of b's base content, read under orig, the
// base object's dispatch: CheckBase exempted exactly these. b is the
// object after mergeMap's first pass and settle. Values the overlay added
// or stood in are not base content and are skipped (an empty base, as
// normalize passes, has none); a base value an overlay value of another
// kind replaces is read too, because it stays when that value holds
// nothing but removals.
func (m *merger) exemptHolders(b *tree.Node, orig *schemaidx.Node, todo []pendingMember) map[*tree.Node]bool {
	var held map[*tree.Node]bool
	collect := func(doc *tree.Node) { held = m.holders(doc, held) }
	var supplied map[int]bool
	for _, t := range todo {
		m.a.tick()
		if t.base == nil || t.replaced() {
			if supplied == nil {
				supplied = map[int]bool{}
			}
			supplied[t.at] = true
		}
		if t.replaced() && composite(t.base) {
			m.eachPatch(t.base, memberSchema(orig, t.key), nil, collect)
		}
	}
	for i, mem := range b.Members {
		m.a.tick()
		if mem.Value != nil && composite(mem.Value) && !supplied[i] {
			m.eachPatch(mem.Value, memberSchema(orig, mem.Key), nil, collect)
		}
	}
	return held
}

// holders adds to held every object in n, the value of a
// JSONSchemaDocument position, that holds a $patch member, and returns
// held (allocated on the first one).
func (m *merger) holders(n *tree.Node, held map[*tree.Node]bool) map[*tree.Node]bool {
	m.a.tick()
	switch n.Kind {
	case tree.KindMap:
		for _, mem := range n.Members {
			if mem.Key == PatchKey {
				if held == nil {
					held = map[*tree.Node]bool{}
				}
				held[n] = true
			}
			held = m.holders(mem.Value, held)
		}
	case tree.KindList:
		for _, e := range n.Items {
			held = m.holders(e, held)
		}
	default:
	}
	return held
}

// basePatchMessage is the message of every RZ-CFG-006 a base $patch
// takes, the text 01 req 25 gives the rule ("valid only in overlays"),
// whichever check finds it: CheckBase, or redispatched with the reason
// appended (review finding: the same base $patch read differently by the
// path that found it).
const basePatchMessage = "$patch is valid only in overlays"

// redispatched reports each $patch member of a held object that the
// merged object b, whose dispatch the overlay changed from the base's to
// sel, reads outside a JSONSchemaDocument value, as RZ-CFG-006 (01 req
// 25: $patch in a base document is valid only in overlays; CheckBase
// exempted it as JSON Schema document data, which the merged object no
// longer reads it as), and removes it once the walk is done, so no
// directive survives into the result. The related location at is the
// overlay member that changed the dispatch (dispatchPos). b is the object
// at the current position.
func (m *merger) redispatched(b *tree.Node, sel *schemaidx.Node, held map[*tree.Node]bool, at tree.Pos) {
	related := diag.Related{Location: m.a.loc(at), Message: "the overlay changes its schema in"}
	var found []*tree.Node
	m.eachMember(b, sel, func(obj *tree.Node, j int) {
		if !held[obj] {
			return
		}
		m.errorAt(CodeBasePatch, obj.Members[j].KeyPos, []diag.PathElem{diag.Field(PatchKey)},
			basePatchMessage+": this base $patch is no longer inside a JSON Schema document after the overlay merge",
			"the overlay changes how this value is read, as a new Policy spec.type does; remove $patch from the base value or keep the base type",
			related)
		found = append(found, obj)
	}, nil)
	for _, obj := range found {
		obj.Members = deleteFunc(m.a, obj.Members, func(mem tree.Member) bool { return mem.Key == PatchKey })
	}
}

// dispatchPos returns the value position of member blame of ov, the first
// overlay member a dispatch rule tests against a string constant whose
// member the merge changed (removed, added, given another scalar or
// replaced by what remains of a composite: a Policy spec.type), else, when
// blame is -1, the position of ov itself. A member the merge leaves as the
// base wrote it (an equal scalar, a null for an absent member, a value
// holding nothing but removals) changed nothing and is never named.
// Members are found through schemaidx DispatchValues (tested), which lists
// string constants only, so a member tested against a boolean, number or
// null constant is not named and the overlay object is. The committed
// schema's rules test string constants only (kind and Policy spec.type);
// the contract change request for a schemaidx API listing every member a
// rule tests would let the related location name such members too.
func dispatchPos(ov *tree.Node, blame int) tree.Pos {
	if blame >= 0 {
		return ov.Members[blame].Value.Pos
	}
	return ov.Pos
}

// misplaced reports a $patch member of an overlay object outside a
// keyed-list entry (RZ-CFG-005, 01 req 25). static is nil in free
// content, whose $patch is no more data than anywhere else: the
// directive is valid only in overlays and only where 01 req 25 places it.
func (m *merger) misplaced(p tree.Pos, static *schemaidx.Node) {
	hint := "delete a keyed-list entry with {<key>: <value>, $patch: delete}; remove or replace a resource with the " +
		PatchAnnotation + " annotation"
	if static == nil {
		hint = "content without a schema merges as data, objects member by member and lists replaced whole; remove $patch"
	}
	m.errorAt(CodeSchema, p, []diag.PathElem{diag.Field(PatchKey)}, "$patch is not valid in an object", hint)
}

// mergeKeyed merges overlay list ov into base list b, both map or
// orderedMap lists keyed by the list's key field (01 req 25): entries
// matched by key merge recursively in place, new entries are appended in
// overlay order, {<key>: v, $patch: delete} removes every entry with that
// key (absent key: no-op), and {$patch: replace} as the first element of
// an orderedMap list makes the result the remaining overlay entries in
// order. A replaced list is the overlay's: mergeKeyed then returns a new
// node with the overlay list's position and style (01 req 25: each
// result node keeps the position of the document that supplied it);
// otherwise it returns b.
func (m *merger) mergeKeyed(b, ov *tree.Node, list *schemaidx.Node) *tree.Node {
	m.a.tick()
	lt, key := list.List()
	item := itemSchema(list)
	out := b
	x := m.entries(key, b.Items, len(ov.Items))
	seen := map[entryID]*tree.Node{}
	for i, e := range ov.Items {
		m.a.tick()
		if m.a.full {
			break
		}
		m.push(m.a.itemElem(list, e, i))
		m.overlayEntry(&x, &out, ov, e, i, lt, key, item, seen)
		m.pop()
	}
	out.Items = x.result()
	return out
}

// overlayEntry merges element i of overlay list ov, entry e at the
// current position, into x (mergeKeyed); a {$patch: replace} first element
// makes *out a new list.
func (m *merger) overlayEntry(x *entries, out **tree.Node, ov, e *tree.Node, i int, lt schemaidx.ListType, key string,
	item *schemaidx.Node, seen map[entryID]*tree.Node,
) {
	if replaceDirective(e) {
		switch {
		case lt != schemaidx.ListOrderedMap:
			m.errorAt(CodeSchema, e.Pos, nil, "{$patch: replace} is valid only in an orderedMap list",
				"a "+lt.String()+" list merges by "+strconv.Quote(key)+"; remove entries with $patch: delete")
		case i > 0:
			m.errorAt(CodeSchema, e.Pos, nil, "{$patch: replace} must be the first element of the list", "")
		default:
			*x = m.entries(key, nil, len(ov.Items))
			*out = &tree.Node{Kind: tree.KindList, Style: ov.Style, Pos: ov.Pos}
		}
		return
	}
	k, ok := m.entryKey(e, key)
	if !ok {
		return
	}
	id := entryID{number: k.Kind != tree.KindString, text: k.Text}
	if prev, dup := seen[id]; dup {
		m.errorAt(CodeSchema, k.Pos, nil,
			"two entries with "+key+" "+scalarText(k)+" in one overlay list", "",
			diag.Related{Location: m.a.loc(prev.Pos), Message: "first entry"})
		return
	}
	seen[id] = k
	if _, del := m.a.get(e, PatchKey); del {
		if len(e.Members) != 2 {
			m.errorAt(CodeSchema, e.Pos, nil,
				"a $patch: delete entry holds only "+strconv.Quote(key)+" and $patch", "")
			return
		}
		x.remove(id)
		return
	}
	if j := x.find(id); j >= 0 {
		x.items[j] = m.mergeMap(x.items[j], e, item)
		return
	}
	x.add(id, m.element(e, item))
}

// entryID identifies a keyed-list key value: strings and numbers never
// match each other.
type entryID struct {
	number bool
	text   string
}

// slot is the key of one keyed-list entry; ok is false for an entry
// without a usable key, which no overlay entry matches.
type slot struct {
	id entryID
	ok bool
}

// entries holds the entries of a keyed list while mergeKeyed merges into
// it. A short list of small entries, or any list an overlay list of at
// most linearScan entries merges into (at most linearScan scans), is
// scanned, each key read by a counted scan as it is compared; a longer
// list, or one whose entries hold many members, is indexed by key once,
// entry by entry (head and more), so mergeKeyed costs time linear in both
// lists (01 req 54). A removed entry is nil until result compacts the
// list.
type entries struct {
	m     *merger
	key   string
	items []*tree.Node
	// scan is set when the overlay list has at most linearScan entries:
	// the list is then never indexed.
	scan bool
	// width counts the members of the entries, which bounds what a scan
	// reads to find the keys.
	width int
	// head maps each key to the first live entry with it, and more maps
	// an entry to the next entry after it with the same key, for the keys
	// a list repeats only. Both are nil while the list is scanned; maps
	// grow table by table, so no step allocates for the whole list.
	head map[entryID]int
	more map[int]int
	dead bool
}

// entries returns the entries of a keyed list by the key field key,
// merged in place: items is the base list's own slice, which mergeKeyed
// only changes after the overlay's first element, and a {$patch: replace}
// first element starts from no entries instead, so the base list is
// never read after it changed.
func (m *merger) entries(key string, items []*tree.Node, lookups int) entries {
	x := entries{m: m, key: key, items: items, scan: lookups <= linearScan}
	for _, it := range items {
		m.a.tick()
		x.width += len(it.Members)
	}
	if x.long() {
		x.index()
	}
	return x
}

// long reports a list to index rather than scan: more than linearScan
// entries, or entries holding more than linearScan² members, merged with
// an overlay list of more than linearScan entries.
func (x *entries) long() bool {
	return !x.scan && (len(x.items) > linearScan || x.width > linearScan*linearScan)
}

// slotOf reads the key field of a keyed-list entry by a counted scan.
func (x *entries) slotOf(it *tree.Node) slot {
	if k, ok := x.m.a.get(it, x.key); ok && keyScalar(k) {
		return slot{id: entryID{number: k.Kind != tree.KindString, text: k.Text}, ok: true}
	}
	return slot{}
}

// index reads every live entry's key once and builds head and more.
func (x *entries) index() {
	x.head = map[entryID]int{}
	for i := len(x.items) - 1; i >= 0; i-- {
		x.m.a.tick()
		if x.items[i] == nil {
			continue
		}
		k := x.slotOf(x.items[i])
		if !k.ok {
			continue
		}
		if h, ok := x.head[k.id]; ok {
			if x.more == nil {
				x.more = map[int]int{}
			}
			x.more[i] = h
		}
		x.head[k.id] = i
	}
}

// find returns the index of the first live entry with key id, or -1.
func (x *entries) find(id entryID) int {
	if x.head != nil {
		x.m.a.tick()
		if j, ok := x.head[id]; ok {
			return j
		}
		return -1
	}
	want := slot{id: id, ok: true}
	for j, it := range x.items {
		x.m.a.tick()
		if it != nil && x.slotOf(it) == want {
			return j
		}
	}
	return -1
}

// remove removes every live entry with key id.
func (x *entries) remove(id entryID) {
	if x.head != nil {
		x.m.a.tick()
		j, ok := x.head[id]
		if !ok {
			return
		}
		delete(x.head, id)
		for {
			x.m.a.tick()
			x.items[j], x.dead = nil, true
			next, more := x.more[j]
			if !more {
				return
			}
			j = next
		}
	}
	want := slot{id: id, ok: true}
	for j, it := range x.items {
		x.m.a.tick()
		if it != nil && x.slotOf(it) == want {
			x.items[j], x.dead = nil, true
		}
	}
}

// add appends entry n with key id, which no live entry has (find returned
// -1), indexing the list once it grows long.
func (x *entries) add(id entryID, n *tree.Node) {
	x.items = append(x.items, n)
	if x.head != nil {
		x.head[id] = len(x.items) - 1
		return
	}
	x.width += len(n.Members)
	if x.long() {
		x.index()
	}
}

// result returns the merged entries, removed ones dropped.
func (x *entries) result() []*tree.Node {
	if x.dead {
		x.items = deleteFunc(x.m.a, x.items, func(n *tree.Node) bool { return n == nil })
	}
	return x.items
}

// entryKey validates an overlay entry of a keyed list at the current
// position and returns its key node: the entry is an object holding the
// key field as a scalar, and a $patch member, when present, is delete.
func (m *merger) entryKey(e *tree.Node, key string) (*tree.Node, bool) {
	if e.Kind != tree.KindMap {
		m.errorAt(CodeSchema, e.Pos, nil, "list entry must be an object with its key field "+strconv.Quote(key), "")
		return nil, false
	}
	if pv, ok := m.a.get(e, PatchKey); ok && (pv.Kind != tree.KindString || pv.Text != PatchDelete) {
		m.errorAt(CodeSchema, pv.Pos, []diag.PathElem{diag.Field(PatchKey)},
			"unsupported $patch value "+scalarText(pv)+" in a list entry",
			"use $patch: delete, or {$patch: replace} as the first element of an orderedMap list")
		return nil, false
	}
	k, ok := m.a.get(e, key)
	if !ok || !keyScalar(k) {
		m.errorAt(CodeSchema, e.Pos, nil, "list entry has no key field "+strconv.Quote(key), "")
		return nil, false
	}
	return k, true
}

// replaceDirective reports the element {$patch: replace}.
func replaceDirective(e *tree.Node) bool {
	if e.Kind != tree.KindMap || len(e.Members) != 1 {
		return false
	}
	v := e.Members[0].Value
	return e.Members[0].Key == PatchKey && v.Kind == tree.KindString && v.Text == PatchReplace
}

// keyScalar reports a node usable as a list key (as schemaidx.ItemElem).
func keyScalar(n *tree.Node) bool {
	return n.Kind == tree.KindString || n.Kind == tree.KindInt || n.Kind == tree.KindFloat
}

// scalarEqual reports two scalars of one kind and value.
func scalarEqual(a, b *tree.Node) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case tree.KindBool:
		return a.Bool == b.Bool
	case tree.KindNull:
		return true
	case tree.KindInt, tree.KindFloat, tree.KindString:
		return a.Text == b.Text
	default:
		return false
	}
}

// scalarText quotes a scalar for a message, clipped (clip), or names its
// kind.
func scalarText(n *tree.Node) string {
	switch n.Kind {
	case tree.KindString:
		return strconv.Quote(clip(n.Text))
	case tree.KindInt, tree.KindFloat:
		return clip(n.Text)
	case tree.KindBool:
		return strconv.FormatBool(n.Bool)
	case tree.KindNull:
		return "null"
	case tree.KindMap:
		return "(an object)"
	default:
		return "(a list)"
	}
}

// checkBase reports every $patch member of a base document (RZ-CFG-006,
// 01 req 25), free content included; only the JSONSchemaDocument value
// is skipped.
func (m *merger) checkBase(n *tree.Node, static *schemaidx.Node) {
	m.eachPatch(n, static, func(obj *tree.Node, j int) {
		m.errorAt(CodeBasePatch, obj.Members[j].KeyPos, []diag.PathElem{diag.Field(PatchKey)}, basePatchMessage, "")
	}, nil)
}

// eachPatch walks n, a value at the current position whose undispatched
// schema is static, with dispatch applied to every object as it stands:
// patch receives each object holding a $patch member (j indexes it)
// outside the values of the JSONSchemaDocument definition, and doc, when
// non-nil, each such value, which the walk never enters. The $patch
// member's own value is not walked. m.stack holds the path of the
// current position; callbacks copy it to keep it. The walk stops once
// the diagnostic budget is spent.
func (m *merger) eachPatch(n *tree.Node, static *schemaidx.Node, patch func(obj *tree.Node, j int), doc func(*tree.Node)) {
	m.a.tick()
	if opaque(static) {
		if doc != nil {
			doc(n)
		}
		return
	}
	switch n.Kind {
	case tree.KindMap:
		m.eachMember(n, m.a.selectOf(static, n), patch, doc)
	case tree.KindList:
		it := itemSchema(static)
		for i, e := range n.Items {
			m.a.tick()
			if m.a.full {
				return
			}
			if composite(e) {
				m.push(m.a.itemElem(static, e, i))
				m.eachPatch(e, it, patch, doc)
				m.pop()
			}
		}
	default:
	}
}

// eachMember is eachPatch on object n read under sel, its dispatched
// schema.
func (m *merger) eachMember(n *tree.Node, sel *schemaidx.Node, patch func(obj *tree.Node, j int), doc func(*tree.Node)) {
	for j, mem := range n.Members {
		m.a.tick()
		if m.a.full {
			return
		}
		switch {
		case mem.Key == PatchKey:
			if patch != nil {
				patch(n, j)
			}
		case composite(mem.Value):
			m.push(diag.Field(mem.Key))
			m.eachPatch(mem.Value, memberSchema(sel, mem.Key), patch, doc)
			m.pop()
		default:
		}
	}
}
