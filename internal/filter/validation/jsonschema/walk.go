// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
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
// numbers it parses whenever it compares them (minor review finding: an
// annotation or unknown keyword is never parsed, 07 req 76).
func parsedKeywords() []string {
	return []string{
		"const", "enum", "exclusiveMaximum", "exclusiveMinimum", "maxContains", "maxItems", "maxLength", "maxProperties",
		"maximum", "minContains", "minItems", "minLength", "minProperties", "minimum", "multipleOf",
	}
}

// refSite is one $ref or $dynamicRef member.
type refSite struct {
	// ptr is the member's pointer.
	ptr string
	// value is the reference as written.
	value string
	// base is the base URI in effect.
	base string
}

// anchorKey names an anchor within the schema resource at res.
type anchorKey struct{ res, name string }

// walker applies the rules that need no compiler to a decoded document and
// lists the document's schema positions: every keyword subschema position
// plus every position a reference into the document names, which the
// compiler also compiles as a schema. The rules are $schema, RE2 regular
// expressions, the number bounds of the values the library parses, unique
// $id and anchor names, references that resolve inside the document, and
// MaxSubschemas. They run in a fixed order (members sorted by name), so
// findings that the compiler would report in its map iteration order are
// reported deterministically.
type walker struct {
	doc any
	// positions lists the schema positions in walk order; seen indexes it.
	positions []string
	seen      map[string]bool
	// ids maps each schema resource's absolute URL to its pointer.
	ids map[string]string
	// anchors maps each $anchor and $dynamicAnchor to its schema.
	anchors map[anchorKey]string
	sites   []refSite
	issues  []Issue
}

func newWalker(doc any) *walker {
	return &walker{doc: doc, seen: map[string]bool{}, ids: map[string]string{baseURL: ""}, anchors: map[anchorKey]string{}}
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
	w.schema(w.doc, "", "", baseURL)
	for i := 0; i < len(w.sites) && len(w.positions) <= MaxSubschemas; i++ {
		w.follow(w.sites[i])
	}
	if len(w.positions) > MaxSubschemas {
		w.add("", fmt.Sprintf("schema has more than %d subschemas", MaxSubschemas))
		return
	}
	for _, s := range w.sites {
		w.checkRef(s)
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

// schema records the schema position v at ptr, inside the resource whose
// root is at res, under base, and walks its keyword subschemas. A position
// already walked, or any position past MaxSubschemas, is not walked again.
func (w *walker) schema(v any, ptr, res, base string) {
	if w.seen[ptr] || len(w.positions) > MaxSubschemas {
		return
	}
	w.seen[ptr] = true
	w.positions = append(w.positions, ptr)
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
	w.rules(obj, ptr)
	for _, kw := range []string{"$ref", "$dynamicRef"} {
		if s, ok := obj[kw].(string); ok {
			w.sites = append(w.sites, refSite{ptr: appendToken(ptr, kw), value: s, base: base})
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
			w.kid(v, at, res, base)
		case formList:
			w.list(v, at, res, base)
		case formMap:
			if m, ok := v.(map[string]any); ok {
				for _, k := range sortedKeys(m) {
					w.kid(m[k], appendToken(at, k), res, base)
				}
			}
		case formOneOrList:
			if _, isList := v.([]any); isList {
				w.list(v, at, res, base)
			} else {
				w.kid(v, at, res, base)
			}
		}
	}
}

// kid walks v at ptr when it is a schema (an object or a boolean).
func (w *walker) kid(v any, ptr, res, base string) {
	switch v.(type) {
	case map[string]any, bool:
		w.schema(v, ptr, res, base)
	}
}

// list walks the schemas of an array value.
func (w *walker) list(v any, ptr, res, base string) {
	if a, ok := v.([]any); ok {
		for i, e := range a {
			w.kid(e, ptr+"/"+strconv.Itoa(i), res, base)
		}
	}
}

// rules applies the RE2 rule to pattern and patternProperties and the
// number bounds to the values the library parses as numbers.
func (w *walker) rules(obj map[string]any, ptr string) {
	if s, ok := obj["pattern"].(string); ok {
		if _, err := regexp.Compile(s); err != nil {
			w.add(appendToken(ptr, "pattern"), "pattern is not a valid RE2 regular expression: "+err.Error())
		}
	}
	if pp, ok := obj["patternProperties"].(map[string]any); ok {
		for _, k := range sortedKeys(pp) {
			if _, err := regexp.Compile(k); err != nil {
				w.add(appendToken(appendToken(ptr, "patternProperties"), k), "patternProperties name is not a valid RE2 regular expression: "+err.Error())
			}
		}
	}
	for _, kw := range parsedKeywords() {
		if v, ok := obj[kw]; ok {
			w.checkNumbers(v, appendToken(ptr, kw))
		}
	}
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
// fragment is a JSON Pointer; checkRef reports what does not resolve.
func (w *walker) follow(s refSite) {
	u, res, frag, inDoc, err := resolveRef(w.ids, s)
	if err != nil || !inDoc || (frag != "" && frag[0] != '/') {
		return
	}
	switch v, _ := lookup(w.doc, res+frag); v.(type) {
	case map[string]any, bool:
		w.schema(v, res+frag, res, u)
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
