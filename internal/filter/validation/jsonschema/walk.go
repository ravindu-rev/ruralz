// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
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
// numbers it parses whenever it compares them (an annotation or unknown
// keyword is never parsed).
func parsedKeywords() []string {
	return []string{
		"const", "enum", "exclusiveMaximum", "exclusiveMinimum", "maxContains", "maxItems", "maxLength", "maxProperties",
		"maximum", "minContains", "minItems", "minLength", "minProperties", "minimum", "multipleOf",
	}
}

// The lists of names. The draft 2020-12 meta-schema puts uniqueItems on
// four lists of strings: type as an array, required, every member of
// dependentRequired, and every array member of dependencies. The
// library's meta-schema check compares their entries, whatever they are,
// before it checks that each is a string: it parses every number with
// math/big, every pair of up to 20 entries and past that every entry once
// for its hash, whose nil result for a literal math/big refuses it
// dereferences (a 241-byte {"type":[1e999999, ...20 literals]} compiled for
// 11 s, 21 literals of exponent 9,999,999 panicked), and compares arrays
// and objects in full, while its hash lets distinct ones collide. A list
// entry that is not a string, which the meta-schema refuses anyway, is
// therefore refused at the walk with the meta-schema's own message, so the
// library never compares one. Like the walk's other rules, this holds at
// every position the library compiles or checks against the meta-schema:
// not below additionalItems, which draft 2020-12 ignores, unless a
// reference, an anchor or an $id makes the library compile it
// (walker.schema).
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

// refSite is one $ref or $dynamicRef member.
type refSite struct {
	// ptr is the member's pointer, pos that of its schema.
	ptr, pos string
	// value is the reference as written.
	value string
	// base is the base URI in effect.
	base string
	// inert marks a reference walked where the library compiles nothing
	// (see walker.schema); again one whose schema was walked inert first,
	// which is followed again but checked once.
	inert, again bool
}

// compiled reports whether the library compiles the reference s: it is not
// inert, or its schema was walked live after it (a reference the library
// never compiles never reaches the loader, so one leaving the document is
// no finding there).
func (w *walker) compiled(s refSite) bool {
	return !s.inert || w.seen[s.pos] == walkedLive
}

// refTarget is the position a JSON Pointer reference names, and the
// pointer of the reference.
type refTarget struct{ ptr, site string }

// anchorKey names an anchor within the schema resource at res.
type anchorKey struct{ res, name string }

// walker applies the rules that need no compiler to a decoded document and
// lists the document's schema positions: every keyword subschema position
// plus every position a reference into the document names, which the
// compiler also compiles as a schema. The rules are $schema, RE2 regular
// expressions and the pattern bounds, the number bounds of the values the
// library parses, the string entries of the lists of names its meta-schema
// check compares, unique $id and anchor names, references that resolve
// inside the document, MaxSubschemas, and the bounds of the library's
// meta-schema checks. They run in a fixed order (members sorted by name), so
// findings that the compiler would report in its map iteration order are
// reported deterministically.
type walker struct {
	doc any
	// positions lists the schema positions in walk order; seen indexes it,
	// with how each was walked (walkedInert or walkedLive).
	positions []string
	seen      map[string]uint8
	// ids maps each schema resource's absolute URL to its pointer.
	ids map[string]string
	// anchors maps each $anchor and $dynamicAnchor to its schema.
	anchors map[anchorKey]string
	sites   []refSite
	issues  []Issue
	// keywordTree is the length of the prefix of positions the root's
	// keywords reach; targets lists every reference target, with the
	// pointer of its first reference, and checkOrder the targets the
	// library checks against the meta-schema apart, in the order the
	// compile must check them (metaChecks).
	keywordTree int
	targets     []refTarget
	checkOrder  []string
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
	return &walker{doc: doc, seen: map[string]uint8{}, ids: map[string]string{baseURL: ""}, anchors: map[anchorKey]string{}, tables: map[string]int{}}
}

// add records a finding, keeping at most MaxIssues.
func (w *walker) add(ptr, msg string) {
	if len(w.issues) < MaxIssues {
		w.issues = append(w.issues, Issue{Pointer: ptr, Message: message(msg)})
	}
}

// walk walks the keyword positions from the root, then the positions
// references name (which may name more), checks every reference and
// applies MaxSubschemas to the total.
func (w *walker) walk() {
	w.schema(w.doc, "", "", baseURL, false)
	w.keywordTree = len(w.positions)
	for i := 0; i < len(w.sites) && len(w.positions) <= MaxSubschemas; i++ {
		w.follow(w.sites[i])
	}
	if len(w.positions) > MaxSubschemas {
		w.add("", fmt.Sprintf("schema has more than %d subschemas", MaxSubschemas))
		return
	}
	for _, s := range w.sites {
		// A schema walked inert, then live, has its references twice: the
		// first is checked when the library compiles it.
		if !s.again && w.compiled(s) {
			w.checkRef(s)
		}
	}
	if len(w.issues) == 0 {
		w.metaChecks()
	}
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

// How a schema position was walked: inert, at a position the library
// never compiles nor checks against the meta-schema, or live.
const (
	walkedInert uint8 = 1 + iota
	walkedLive
)

// schema records the schema position v at ptr, inside the resource whose
// root is at res, under base, and walks its keyword subschemas. A position
// already walked, or any position past MaxSubschemas, is not walked again,
// except that an inert position later reached live is walked again live,
// without being recorded again.
//
// A position is inert below additionalItems, which the library collects as
// a schema position, its $id and anchors included, but under draft 2020-12
// does not compile, and which the meta-schema does not define: it is
// compiled only as the target of a JSON Pointer reference, which follow
// walks live, or, when a schema position of additionalItems holds an $id
// or an anchor, through it, so such an additionalItems is walked live
// whole. The rules apply at live positions only, where the compile also
// checks against the meta-schema what no check of the library covers
// (uncheckedPositions); $schema is checked everywhere, and a reference
// wherever the library compiles it (walker.compiled).
func (w *walker) schema(v any, ptr, res, base string, inert bool) {
	was := w.seen[ptr]
	if was == walkedLive || (was == walkedInert && inert) || len(w.positions) > MaxSubschemas {
		return
	}
	first := was == 0
	w.seen[ptr] = walkedLive
	if inert {
		w.seen[ptr] = walkedInert
	}
	if first {
		w.positions = append(w.positions, ptr)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return
	}
	if id, ok := obj["$id"].(string); ok {
		// As the library: an $id without its fragment starts a resource.
		if id, _, _ = strings.Cut(id, "#"); id != "" {
			if u, ok := joinURL(base, id); ok {
				base, res = u, ptr
				if prev, dup := w.ids[u]; !dup {
					w.ids[u] = ptr
				} else if prev != ptr {
					w.add(appendToken(ptr, "$id"), fmt.Sprintf("$id %q is already the identifier of the schema at %q", id, prev))
				}
			}
		}
	}
	if first {
		w.recordOnce(obj, ptr, res)
	}
	if !inert {
		w.rules(obj, ptr)
	}
	for _, kw := range []string{"$ref", "$dynamicRef"} {
		if s, ok := obj[kw].(string); ok {
			w.sites = append(w.sites, refSite{ptr: appendToken(ptr, kw), pos: ptr, value: s, base: base, inert: inert, again: !first})
		}
	}
	eachSubschema(obj, ptr, func(v any, at string) {
		w.schema(v, at, res, base, inert || (at[len(ptr):] == "/additionalItems" && !identifies(v)))
	})
}

// identifies reports whether the schema v, or a schema position below it,
// has an $id, $anchor or $dynamicAnchor string, through which the library
// may compile a schema that no JSON Pointer reference names: the target of
// a reference to an anchor or a resource, the root of a resource it
// compiles a subschema of, and every $dynamicAnchor of a resource it
// compiles. As the library collects identifiers, only schema positions
// count, not a property name or a const, enum or examples value.
func identifies(v any) bool {
	obj, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for _, kw := range []string{"$id", "$anchor", "$dynamicAnchor"} {
		if _, ok := obj[kw].(string); ok {
			return true
		}
	}
	found := false
	eachSubschema(obj, "", func(sv any, _ string) { found = found || identifies(sv) })
	return found
}

// recordOnce checks the $schema of the schema object obj at ptr and
// records its anchors in the resource at res, on its first walk.
func (w *walker) recordOnce(obj map[string]any, ptr, res string) {
	if s, ok := obj["$schema"]; ok {
		if str, isStr := s.(string); !isStr || str != Draft {
			w.add(appendToken(ptr, "$schema"), "$schema must be "+Draft)
		}
	}
	for _, kw := range []string{"$anchor", "$dynamicAnchor"} {
		if name, ok := obj[kw].(string); ok {
			k := anchorKey{res, name}
			if prev, dup := w.anchors[k]; !dup {
				w.anchors[k] = ptr
			} else if prev != ptr {
				w.add(appendToken(ptr, kw), fmt.Sprintf("anchor %q is already defined by the schema at %q", name, prev))
			}
		}
	}
}

// eachSubschema calls f for every keyword subschema of the schema object
// obj at ptr that is a schema (an object or a boolean), in keyword and
// member order.
func eachSubschema(obj map[string]any, ptr string, f func(v any, ptr string)) {
	kid := func(v any, at string) {
		switch v.(type) {
		case map[string]any, bool:
			f(v, at)
		}
	}
	list := func(v any, at string) {
		if a, ok := v.([]any); ok {
			for i, e := range a {
				kid(e, at+"/"+strconv.Itoa(i))
			}
		}
	}
	for _, kw := range subschemaKeywords() {
		v, ok := obj[kw.name]
		if !ok {
			continue
		}
		at := appendToken(ptr, kw.name)
		switch kw.form {
		case formOne:
			kid(v, at)
		case formList:
			list(v, at)
		case formMap:
			if m, ok := v.(map[string]any); ok {
				for _, k := range sortedKeys(m) {
					kid(m[k], appendToken(at, k))
				}
			}
		case formOneOrList:
			if _, isList := v.([]any); isList {
				list(v, at)
			} else {
				kid(v, at)
			}
		}
	}
}

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

// resolveRef resolves a reference the way the library does: the URL without
// its fragment names the document or one of its $id resources (res is
// that resource's pointer, inDoc false for any other URL), and the
// fragment is percent-decoded.
func resolveRef(ids map[string]string, s refSite) (u, res, frag string, inDoc bool, err error) {
	u, ok := joinURL(s.base, s.value)
	if !ok {
		return "", "", "", false, fmt.Errorf("reference %q is not a valid URI reference", s.value)
	}
	res, inDoc = ids[u]
	_, raw, _ := strings.Cut(s.value, "#")
	if frag, err = url.PathUnescape(raw); err != nil {
		return "", "", "", false, fmt.Errorf("reference %q is not a valid URI fragment", s.value)
	}
	return u, res, frag, inDoc, nil
}

// follow walks the position a reference into the document names when its
// fragment is a JSON Pointer, live unless the reference is inert, which
// the library never compiles; checkRef reports what does not resolve.
func (w *walker) follow(s refSite) {
	u, res, frag, inDoc, err := resolveRef(w.ids, s)
	if err != nil || !inDoc || (frag != "" && frag[0] != '/') {
		return
	}
	switch v, _ := lookup(w.doc, res+frag); v.(type) {
	case map[string]any, bool:
		if !s.inert {
			w.targets = append(w.targets, refTarget{ptr: res + frag, site: s.ptr})
		}
		w.schema(v, res+frag, res, u, s.inert)
	}
}

// checkRef reports a reference that leaves the document (the compiler's
// loader would refuse it; a bundled meta-schema would be served without
// it), a JSON Pointer that does not resolve to a schema, and an anchor
// its resource does not define.
func (w *walker) checkRef(s refSite) {
	_, res, frag, inDoc, err := resolveRef(w.ids, s)
	switch {
	case err != nil:
		w.add(s.ptr, err.Error())
	case !inDoc:
		w.add(s.ptr, fmt.Sprintf("reference %q resolves outside the schema document", s.value))
	case frag == "" || frag[0] == '/':
		switch v, ok := lookup(w.doc, res+frag); v.(type) {
		case map[string]any, bool:
		default:
			if ok {
				w.add(s.ptr, fmt.Sprintf("reference %q names a value that is not a schema", s.value))
			} else {
				w.add(s.ptr, fmt.Sprintf("reference %q does not resolve within the schema document", s.value))
			}
		}
	default:
		if _, ok := w.anchors[anchorKey{res, frag}]; !ok {
			w.add(s.ptr, fmt.Sprintf("reference %q names an anchor its schema resource does not define", s.value))
		}
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
