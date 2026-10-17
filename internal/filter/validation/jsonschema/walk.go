// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
)

// keywordForm says where a keyword keeps its subschemas.
type keywordForm uint8

const (
	// formOne: the value is a subschema.
	formOne keywordForm = iota
	// formList: the value is an array of subschemas.
	formList
	// formMap: the value is an object whose members are subschemas.
	formMap
	// formOneOrList: a subschema or an array of them (items).
	formOneOrList
)

// subschemaKeyword is one keyword holding subschemas.
type subschemaKeyword struct {
	name string
	form keywordForm
}

// subschemaKeywords returns the keywords whose values the library's draft
// 2020-12 dialect treats as subschema positions (its Draft2020 subschema
// paths, which keep the legacy definitions, dependencies, items-array and
// additionalItems positions), sorted by name.
func subschemaKeywords() []subschemaKeyword {
	return []subschemaKeyword{
		{"$defs", formMap},
		{"additionalItems", formOne},
		{"additionalProperties", formOne},
		{"allOf", formList},
		{"anyOf", formList},
		{"contains", formOne},
		{"contentSchema", formOne},
		{"definitions", formMap},
		{"dependencies", formMap},
		{"dependentSchemas", formMap},
		{"else", formOne},
		{"if", formOne},
		{"items", formOneOrList},
		{"not", formOne},
		{"oneOf", formList},
		{"patternProperties", formMap},
		{"prefixItems", formList},
		{"properties", formMap},
		{"propertyNames", formOne},
		{"then", formOne},
		{"unevaluatedItems", formOne},
		{"unevaluatedProperties", formOne},
	}
}

// parsedKeywords lists the keywords whose values the library parses as
// numbers with math/big: the numeric and integer keywords, which it
// compiles (and the meta-schema checks), and const and enum, whose
// numbers it parses whenever it compares them (minor review finding: an
// annotation or unknown keyword is never parsed, 07 req 76).
func parsedKeywords() []string {
	return []string{
		"const", "enum", "exclusiveMaximum", "exclusiveMinimum", "maxContains", "maxItems", "maxLength", "maxProperties",
		"maximum", "minContains", "minItems", "minLength", "minProperties", "minimum", "multipleOf",
	}
}

// The lists of names (architecture R-77, review finding). The draft
// 2020-12 meta-schema puts uniqueItems on four lists of strings: type as an
// array, required, every member of dependentRequired, and every array
// member of dependencies. The library's meta-schema check compares their
// entries, whatever they are, before it checks that each is a string: it
// parses every number with math/big, every pair of up to 20 entries and
// past that every entry once for its hash, whose nil result for a literal
// math/big refuses it dereferences (a 241-byte {"type":[1e999999, ...20
// literals]} compiled for 11 s, 21 literals of exponent 9,999,999
// panicked), and compares arrays and objects in full, while its hash lets
// distinct ones collide. A list entry that is not a string, which the
// meta-schema refuses anyway, is therefore refused at the walk with the
// meta-schema's own message, so the library never compares one. Like the
// walk's other rules, this holds at every position the library compiles,
// which includes every position it checks against the meta-schema, and
// nowhere else: not below additionalItems, which draft 2020-12 ignores,
// unless the library compiles a schema there (walker.close).
const (
	// typeEntryMessage is the meta-schema's message for an entry of a type
	// list that is not one of its simple types.
	typeEntryMessage = "invalid JSON Schema: value must be one of 'array', 'boolean', 'integer', 'null', 'number', 'object', 'string'"
	// nameEntryMessage is the meta-schema's message for an entry of a list
	// of member names that is not a string, given the entry's JSON kind.
	nameEntryMessage = "invalid JSON Schema: got %s, want string"
)

// nameLists refuses every entry that is not a string of the type list, the
// required list and the lists of dependentRequired and dependencies of the
// schema object obj at ptr, in keyword and member order.
func (w *walker) nameLists(obj map[string]any, ptr string) {
	for _, kw := range []string{"dependencies", "dependentRequired"} {
		if m, ok := obj[kw].(map[string]any); ok {
			at := appendToken(ptr, kw)
			for _, k := range sortedKeys(m) {
				if list, ok := m[k].([]any); ok {
					w.stringEntries(list, appendToken(at, k), false)
				}
			}
		}
	}
	if list, ok := obj["required"].([]any); ok {
		w.stringEntries(list, appendToken(ptr, "required"), false)
	}
	if list, ok := obj["type"].([]any); ok {
		w.stringEntries(list, appendToken(ptr, "type"), true)
	}
}

// stringEntries reports every entry of list at ptr that is not a string,
// with the message of a type list or of a list of names.
func (w *walker) stringEntries(list []any, ptr string, types bool) {
	for i, e := range list {
		if _, ok := e.(string); ok {
			continue
		}
		msg := typeEntryMessage
		if !types {
			msg = fmt.Sprintf(nameEntryMessage, jsonKind(e))
		}
		w.add(ptr+"/"+strconv.Itoa(i), msg)
	}
}

// jsonKind names the JSON kind of a decoded value as the library's
// messages do.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "number"
	}
}

// referenceKeywords returns the keywords the library compiles as
// references, in the order it compiles them within one schema: $ref
// (objcompiler.go:90), the draft 2019-09 $recursiveRef, which its 2019-09
// compile layer, run for every draft from 2019-09 on, compiles under draft
// 2020-12 too (objcompiler.go:56-59, 269), and $dynamicRef (:319). Each
// resolves as $ref does, against the base URI of its resource, to a
// JSON Pointer, an anchor or another resource (enqueueRefVal,
// objcompiler.go:361-384), so the walk follows and checks each the same
// way (library audit class GC-01: $recursiveRef was missing here, so its
// targets escaped every rule of the walk).
func referenceKeywords() []string { return []string{"$ref", "$recursiveRef", "$dynamicRef"} }

// refStatus is what a reference resolves to (walker.resolve).
type refStatus uint8

const (
	// refUnresolved: the library never compiles the reference, so the walk
	// never resolved it.
	refUnresolved refStatus = iota
	// refBadURI: the reference is not a valid URI reference.
	refBadURI
	// refBadFragment: its fragment does not percent-decode.
	refBadFragment
	// refOutside: its URL names no resource the library has collected
	// when it compiles the reference, so the library would ask the loader
	// (or serve a bundled meta-schema without it).
	refOutside
	// refMissing: its JSON Pointer names no value of the document.
	refMissing
	// refNotSchema: its JSON Pointer names a value that is no schema.
	refNotSchema
	// refNoAnchor: its resource defines no such anchor when the library
	// compiles the reference.
	refNoAnchor
	// refResolved: target is the schema it names.
	refResolved
)

// resolution is what a reference resolves to: its status.
type resolution struct {
	status refStatus
}

// refSite is one member of a reference keyword (referenceKeywords), as the
// walk's last pass left it.
type refSite struct {
	// ptr is the member's pointer, pos that of its schema.
	ptr, pos string
	// value is the reference as written.
	value string
	// res is its resolution in the walk's last pass, for a reference at a
	// position the library compiles (walked live); refUnresolved for every
	// other, which the library never compiles, so never resolves, loads
	// or follows (minor review findings, library audit rows REFS-09 and
	// REFS-39: an inert reference's target recorded anchors and $ids that
	// live references then relied on, though the library never collected
	// them).
	res resolution
	// url is the URL the last pass resolved the reference to, without its
	// fragment, once it joined the reference with its base URI; "" when it
	// did not (an unresolved reference or one that is no URI reference).
	url string
}

// refTarget is a reference target the library collects apart, and the
// pointer of a reference to it.
type refTarget struct{ ptr, site string }

// compileBudget is the walk-time compile budget of architecture R-77 and
// 07 req 75 (decided 2026-10-16): 1 MiB of units, a unit being a byte of a
// pointer or URL each time the compile builds, hashes or compares it, or
// one step of a count product, past which the document is RZ-CFG-005 at
// the member, value, reference or $id that crosses it. The walk charges
// one term of it here (proposed: the term's unit, value and pointer await
// the owner's record), the fixpoint's (walker.walk): everything the passes
// after the first do, each step of their work and each byte of the
// pointers, URLs and strings they meet anew, a count product of the
// passes and their work. The first pass, the walk's own work on every
// position, reference and $id, is not charged here, nor is placing the
// targets each pass found among those found before, each target placed
// once (insertKnown): they are the subject of the budget's byte and count
// terms (library audit classes GC-03 and GC-09). An ordinary document
// takes one pass and charges nothing, or two, charging its second pass's
// steps, which repeat the first whatever the length of its pointers: about
// 61,000 units at MaxSubschemas positions holding three references each.
const compileBudget = 1 << 20

// stepKeywordTree is the step at which the library collects the
// document's keyword tree: before every reference target.
const stepKeywordTree = -1

// walker applies the rules that need no compiler to a decoded document and
// lists the document's schema positions, with a model of what the library
// collects and compiles from it (library audit class GC-02). The rules are
// $schema, RE2 regular expressions and the pattern bounds, the number
// bounds of the values the library parses, the string entries of the lists
// of names its meta-schema check compares, unique $id and anchor names,
// references that resolve inside the document, MaxSubschemas, and the
// bounds of the library's meta-schema checks. They run in a fixed order
// (members sorted by name), so findings that the compiler would report in
// its map iteration order are reported deterministically.
//
// The model is the library's own (roots.go:70-235, root.go:20-135,
// compiler.go:190-262, objcompiler.go:353-384). The library collects the
// document's keyword tree, every schema position the subschema keywords
// reach, below additionalItems too, with the base URI of each and the
// resources, $id and anchors there; then, as the compile checks them apart
// (compileWalked), every reference target outside the collected positions,
// in pointer order, each under the resource of the longest pointer prefix
// collected by then, with its own keyword tree. It compiles exactly what
// the compile asks for, every position walked live, and what that reaches:
// the subschemas of the keywords it compiles (not below additionalItems),
// the targets of the references it compiles, the root of the resource of
// every schema it compiles, and the $dynamicAnchor schemas of every
// resource root it compiles. A reference resolves against the resource of
// the longest pointer prefix of its schema, to the resources, $id and
// anchors collected when the library compiles it: a target checked apart
// compiles right after its own check, before the targets after it are
// collected, and everything else after all of them. The walk computes this
// to a fixpoint (walk), in the integer tables of model.go, so it walks
// live exactly the positions the library compiles, each with the library's
// base, and refuses at the reference every reference the library would
// load, or fail to resolve. Its result, the last pass's, is kept as the
// pointers below.
type walker struct {
	doc any
	// positions lists the schema positions the library collects, in its
	// order: the keyword tree, then each target in checkOrder with its
	// keyword tree; seen indexes it with how each was walked (walkedInert,
	// collected only, or walkedLive, compiled), and values holds each one's
	// schema.
	positions []string
	seen      map[string]uint8
	values    map[string]any
	// sites lists every reference of a collected position, in collection
	// order.
	sites []refSite
	// issues holds the walk's findings.
	issues []Issue
	// keywordTree is the length of the prefix of positions the root's
	// keywords reach. checkOrder lists the reference targets the library
	// collects apart, in pointer order, the order in which the compile
	// checks them (metaChecks), and targets pairs each with a reference to
	// it: the first the last pass resolved to it, or else the one that
	// first found it. ids and anchors count the resources and the anchors
	// collected, whose bookkeeping the library copies for each target it
	// checks apart (metaChecks).
	keywordTree  int
	checkOrder   []string
	targets      []refTarget
	ids, anchors int
	// model is the walk's model of the library (model.go), with its
	// passes and what they charge to the walk-time compile budget.
	model
	// insts sums the measures of the patterns (patternsize.go), and
	// tables caches the ranges of each Unicode class escape; instsOver is
	// set once the document's bound is reported. parsed counts the
	// patterns parsed: test instrumentation, by which
	// TestPatternParsesBounded pins that no pattern is parsed past the
	// bound.
	insts     int64
	parsed    int
	tables    map[string]int
	instsOver bool
}

func newWalker(doc any) *walker {
	w := &walker{doc: doc, seen: map[string]uint8{}, values: map[string]any{}, tables: map[string]int{}}
	w.model = newModel(doc)
	return w
}

// add records a finding, keeping at most MaxIssues.
func (w *walker) add(ptr, msg string) {
	if len(w.issues) < MaxIssues {
		w.issues = append(w.issues, Issue{Pointer: ptr, Message: message(msg)})
	}
}

// msgFollowBudget is the finding, at a reference, of a walk whose fixpoint
// spends more than the budget in its passes after the first (walker.walk),
// given the budget.
const msgFollowBudget = "following the reference targets outside the schema's keywords takes passes after the first that spend more than the %d units of the walk-time compile budget (move the schemas the references name under $defs)"

// walk runs the library's collection and compile model to a fixpoint,
// then applies the rules: at every position collected the $schema rule
// and unique $id and anchor names, at every position compiled the rules,
// and at every reference compiled the check that it resolves, then
// MaxSubschemas to the total and the bounds of the meta-schema checks.
//
// A pass collects the keyword tree and then the targets found so far, in
// pointer order, and closes the compiled set (close); a target it finds
// that no earlier pass did, it collects at once, so references into it
// resolve in the same pass, and the next pass collects it in its place
// in pointer order. The pass that finds no new target is the library's
// order exactly, and its result stands (library audit rows REFS-06 and
// REFS-47: a target found after the targets it encloses, or holding the
// $id an earlier reference names, was walked with the wrong base or
// never). The targets found only accumulate, so the passes end: a target
// an earlier pass found stays in checkOrder, so the compile compiles it,
// even if no reference of the last pass reaches it. A document needs more
// than two passes only when a target's base, and with it what the
// references in it name, changes once a target enclosing it is collected
// first, which ordinary schemas never do.
//
// A pass works in the integers of the model, at a cost that does not
// depend on the length of a pointer or URL, which only the first pass to
// meet one builds or hashes (model.go; check finding: the passes hashed
// every prefix of every pointer and built every target's pointer again,
// so 228 passes under a 200,000-byte member name took 30 s and 31 GB).
// What the passes after the first do, their steps and the bytes they meet
// anew, is charged to the walk-time compile budget (compileBudget);
// placing each pass's new targets among those found is not, since each
// target is placed once (insertKnown). The walk stops where it spends past
// the budget, finishing only the position or reference in hand, and is
// refused at the reference to the first new target of the pass before the
// one it stopped in: the target whose following made the walk take the
// pass that crossed the budget (TestFollowFixpoint,
// TestFollowFixpointCost).
func (w *walker) walk() {
	var known []foundTarget
	cause := -1
	for {
		w.beginPass()
		w.collect(rootNode, baseURLIndex, rootNode, stepKeywordTree)
		w.keywordTree = len(w.order)
		for _, t := range known {
			// Past MaxSubschemas positions or the budget the pass collects
			// no more, and goes through the targets left no further, rather
			// than charge a step for each of them past the budget.
			if w.halted() {
				break
			}
			w.step()
			if w.seenOf(t.node) == 0 {
				w.touch(t.node)
				w.nodes[t.node].prelude = len(w.prelude)
				w.prelude = append(w.prelude, t.node)
				w.collectTarget(t.node, len(w.prelude)-1)
			}
		}
		var next []foundTarget
		if !w.halted() {
			next = w.close()
		}
		w.marks = append(w.marks, w.spent)
		if w.over {
			w.finish()
			w.emitNotes()
			w.add("", fmt.Sprintf("schema has more than %d subschemas", MaxSubschemas))
			return
		}
		if w.stopped {
			// Only a pass after the first charges, and it runs once a pass
			// found a target, whose reference cause names.
			w.finish()
			w.emitNotes()
			w.add(w.siteTab[cause].ptr, fmt.Sprintf(msgFollowBudget, w.budget))
			return
		}
		if len(next) == 0 {
			break
		}
		// The passes from here on are the fixpoint's, charged to the
		// budget; placing what this one found charges nothing.
		cause, w.charging = next[0].site, true
		known = w.insertKnown(known, next)
	}
	w.finish()
	for _, t := range w.prelude {
		nd := &w.nodes[t]
		si := nd.first
		if si < 0 {
			si = nd.known
		}
		w.targets = append(w.targets, refTarget{ptr: nd.ptr, site: w.siteTab[si].ptr})
	}
	w.emit()
	if len(w.issues) == 0 {
		w.metaChecks()
	}
}

// finish keeps the last pass's model as the walker's pointers: the
// positions in collection order with how each was walked and its schema,
// the targets checked apart, every reference of a collected position with
// its resolution, and the counts of resources and anchors. Each pointer
// was built once, when its position or reference was first met, and is
// hashed here once more.
func (w *walker) finish() {
	w.positions = make([]string, len(w.order))
	w.seen = make(map[string]uint8, len(w.order))
	w.values = make(map[string]any, len(w.order))
	for i, n := range w.order {
		nd := &w.nodes[n]
		w.positions[i] = nd.ptr
		w.seen[nd.ptr] = nd.seen
		w.values[nd.ptr] = nd.v
	}
	for _, t := range w.prelude {
		w.checkOrder = append(w.checkOrder, w.nodes[t].ptr)
	}
	w.sites = make([]refSite, len(w.passSites))
	for i, si := range w.passSites {
		s := &w.siteTab[si]
		w.sites[i] = refSite{ptr: s.ptr, pos: w.nodes[s.node].ptr, value: s.value}
		if s.pass != w.passes {
			continue
		}
		w.sites[i].res.status = s.res.status
		if s.res.url >= 0 {
			w.sites[i].url = w.urlOf[s.res.url]
		}
	}
	w.ids, w.anchors = w.passIDs, w.passAnchors
}

// emitNotes reports the collection's findings, in collection order.
func (w *walker) emitNotes() {
	for _, n := range w.notes {
		w.addNote(n)
	}
}

// emit reports the last pass's findings: at each position in collection
// order, the collection's, then, at a live one, the rules'; then at each
// reference the library compiles, in collection order, what does not
// resolve.
func (w *walker) emit() {
	k := 0
	for i, n := range w.order {
		for ; k < len(w.notes) && w.notes[k].at == i; k++ {
			w.addNote(w.notes[k])
		}
		nd := &w.nodes[n]
		if nd.seen != walkedLive {
			continue
		}
		if obj, ok := nd.v.(map[string]any); ok {
			w.rules(obj, nd.ptr)
		}
	}
	for i, si := range w.passSites {
		if w.nodes[w.siteTab[si].node].seen == walkedLive {
			w.checkRef(w.sites[i])
		}
	}
}

// eachSubschemaToken calls f for every keyword subschema of the schema
// object obj that is a schema (an object or a boolean), in keyword and
// member order, with its keyword and, for a member of a list or a map
// (member set), the member's escaped JSON Pointer token.
func eachSubschemaToken(obj map[string]any, f func(v any, kw, tok string, member bool)) {
	kid := func(v any, kw, tok string, member bool) {
		switch v.(type) {
		case map[string]any, bool:
			f(v, kw, tok, member)
		}
	}
	list := func(v any, kw string) {
		if a, ok := v.([]any); ok {
			for i, e := range a {
				kid(e, kw, strconv.Itoa(i), true)
			}
		}
	}
	for _, sk := range subschemaKeywords() {
		v, ok := obj[sk.name]
		if !ok {
			continue
		}
		switch sk.form {
		case formOne:
			kid(v, sk.name, "", false)
		case formList:
			list(v, sk.name)
		case formMap:
			if m, ok := v.(map[string]any); ok {
				for _, k := range sortedKeys(m) {
					kid(m[k], sk.name, escapeToken(k), true)
				}
			}
		case formOneOrList:
			if _, isList := v.([]any); isList {
				list(v, sk.name)
			} else {
				kid(v, sk.name, "", false)
			}
		}
	}
}

// eachSubschema calls f for every keyword subschema of the schema object
// obj at ptr that is a schema (an object or a boolean), in keyword and
// member order, with its pointer.
func eachSubschema(obj map[string]any, ptr string, f func(v any, ptr string)) {
	var kw, at string
	eachSubschemaToken(obj, func(v any, k, tok string, member bool) {
		if at == "" || k != kw {
			kw, at = k, appendToken(ptr, k)
		}
		if member {
			f(v, at+"/"+tok)
		} else {
			f(v, at)
		}
	})
}

// checkNumbers applies the number bounds to every number of v.
func (w *walker) checkNumbers(v any, ptr string) {
	switch t := v.(type) {
	case json.Number:
		if !numberInRange(string(t)) {
			w.add(ptr, fmt.Sprintf("number is out of range: at most %d characters and a decimal exponent within ±%d", MaxNumberBytes, MaxNumberExponent))
		}
	case []any:
		for i, e := range t {
			w.checkNumbers(e, ptr+"/"+strconv.Itoa(i))
		}
	case map[string]any:
		for _, k := range sortedKeys(t) {
			w.checkNumbers(t[k], appendToken(ptr, k))
		}
	}
}

// How a schema position was walked: inert, collected by the library but
// never compiled nor checked against the meta-schema, or live, compiled.
const (
	walkedInert uint8 = 1 + iota
	walkedLive
)

// rules applies the RE2 rule and the program bounds to pattern and
// patternProperties, without compiling them (patternsize.go), the number
// bounds to the values the library parses as numbers, and the string rule
// to the lists of names the meta-schema check compares.
func (w *walker) rules(obj map[string]any, ptr string) {
	if s, ok := obj["pattern"].(string); ok {
		w.pattern(s, appendToken(ptr, "pattern"), "pattern")
	}
	if pp, ok := obj["patternProperties"].(map[string]any); ok {
		for _, k := range sortedKeys(pp) {
			w.pattern(k, appendToken(appendToken(ptr, "patternProperties"), k), "patternProperties name")
		}
	}
	for _, kw := range parsedKeywords() {
		if v, ok := obj[kw]; ok {
			w.checkNumbers(v, appendToken(ptr, kw))
		}
	}
	w.nameLists(obj, ptr)
}

// checkRef reports a reference the library compiles that does not resolve
// when it compiles it (resolve): one that leaves the document (the
// compiler's loader would refuse it; a bundled meta-schema would be served
// without it), also one naming a resource the library collects only after
// it compiles the reference, a JSON Pointer that does not resolve to a
// schema, and an anchor its resource does not define by then.
func (w *walker) checkRef(s refSite) {
	switch s.res.status {
	case refBadURI:
		w.add(s.ptr, fmt.Sprintf("reference %q is not a valid URI reference", s.value))
	case refBadFragment:
		w.add(s.ptr, fmt.Sprintf("reference %q is not a valid URI fragment", s.value))
	case refOutside:
		w.add(s.ptr, fmt.Sprintf("reference %q resolves outside the schema document", s.value))
	case refMissing:
		w.add(s.ptr, fmt.Sprintf("reference %q does not resolve within the schema document", s.value))
	case refNotSchema:
		w.add(s.ptr, fmt.Sprintf("reference %q names a value that is not a schema", s.value))
	case refNoAnchor:
		w.add(s.ptr, fmt.Sprintf("reference %q names an anchor its schema resource does not define", s.value))
	case refUnresolved, refResolved:
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
