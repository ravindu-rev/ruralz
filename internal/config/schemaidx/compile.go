// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaidx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// raw is one schema object of the document, compiled once per JSON
// location. Keywords the index does not navigate by (description, pattern,
// minimum and the other assertions, and the navigation-neutral
// applicators propertyNames, contains and dependentRequired, which only
// constrain keys or require an element) are ignored; schemaview validates
// them.
type raw struct {
	id  int
	ptr string
	// def is the $defs name when ptr is "#/$defs/<name>".
	def string
	// ref is the resolved $ref target; refPtr its pointer until resolved.
	ref    *raw
	refPtr string
	// types is the type constraint of this object alone (type, const,
	// enum); eff adds $ref, allOf, anyOf and oneOf once resolved.
	types TypeSet
	eff   TypeSet
	// values holds the const or enum values; hasValues when constrained.
	values    []any
	hasValues bool
	props     map[string]*raw
	required  []string
	items     *raw
	addl      *raw
	addlFalse bool
	allOf     []*raw
	anyOf     []*raw
	oneOf     []*raw
	ifc       *cond
	then, els *raw
	dflt      any
	hasDflt   bool
	kw        Keywords
}

// cond is a supported "if" schema: required members and member consts.
type cond struct {
	required []string
	consts   []Condition
}

// Condition is one member constraint of an if/then dispatch rule: the
// instance object's member Member equals the JSON scalar Value (a string,
// json.Number, bool or nil).
type Condition struct {
	// Member is the object member name, such as "type".
	Member string
	// Value is the const value.
	Value any
}

// compiler turns the decoded document into raw nodes.
type compiler struct {
	doc   any
	root  *raw
	defs  map[string]*raw
	byPtr map[string]*raw
	all   []*raw
	// refs are the nodes carrying x-ruralz-ref.
	refs []*raw
}

func schemaErr(ptr, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrSchema, ptr, fmt.Sprintf(format, args...))
}

// compileDocument decodes and compiles a schema document.
func compileDocument(data []byte) (*compiler, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSchema, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing data after the schema document", ErrSchema)
	}
	if _, ok := doc.(map[string]any); !ok {
		return nil, schemaErr("#", "the document must be a JSON object")
	}
	c := &compiler{doc: doc, defs: map[string]*raw{}, byPtr: map[string]*raw{}}
	root, err := c.schema(doc, "#")
	if err != nil {
		return nil, err
	}
	c.root = root
	// Resolve every $ref; resolving compiles targets, which may add more.
	for i := 0; i < len(c.all); i++ {
		r := c.all[i]
		if r.refPtr == "" {
			continue
		}
		target, ptr, err := c.resolve(r.refPtr)
		if err != nil {
			return nil, schemaErr(r.ptr, "$ref %q: %v", r.refPtr, err)
		}
		if r.ref, err = c.schema(target, ptr); err != nil {
			return nil, err
		}
	}
	memo := make(map[*raw]TypeSet, len(c.all))
	for _, r := range c.all {
		r.eff = effTypes(r, memo)
	}
	for _, r := range c.all {
		for _, b := range slices.Concat(r.anyOf, r.oneOf) {
			if err := navFree(b, map[*raw]bool{}); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

// rootAPIVersion returns the root properties.apiVersion const.
func (c *compiler) rootAPIVersion() (string, bool) {
	p, ok := c.root.props["apiVersion"]
	if !ok || !p.hasValues || len(p.values) != 1 {
		return "", false
	}
	s, ok := p.values[0].(string)
	return s, ok
}

// defNames returns the root $defs names, sorted.
func (c *compiler) defNames() []string {
	return slices.Sorted(maps.Keys(c.defs))
}

// checkRefKinds requires every x-ruralz-ref to name a dispatched kind when
// the root dispatches kinds.
func (c *compiler) checkRefKinds(kinds []string) error {
	if len(kinds) == 0 {
		return nil
	}
	for _, r := range c.refs {
		if !slices.Contains(kinds, r.kw.Ref) {
			return schemaErr(r.ptr, "x-ruralz-ref %q is not a resource kind", r.kw.Ref)
		}
	}
	return nil
}

// schema compiles the schema at ptr, once per location.
func (c *compiler) schema(v any, ptr string) (*raw, error) {
	if r, ok := c.byPtr[ptr]; ok {
		return r, nil
	}
	r := &raw{id: len(c.all), ptr: ptr, types: AllTypes}
	c.byPtr[ptr] = r
	c.all = append(c.all, r)
	if name, ok := strings.CutPrefix(ptr, "#/$defs/"); ok && !strings.Contains(name, "/") {
		r.def = unescapeToken(name)
	}
	switch t := v.(type) {
	case bool:
		if !t {
			r.types = 0
		}
		return r, nil
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			if err := c.keyword(r, k, t[k], ptr+"/"+escapeToken(k)); err != nil {
				return nil, err
			}
		}
		return r, nil
	default:
		return nil, schemaErr(ptr, "a schema must be an object or a boolean")
	}
}

// keyword compiles one keyword of r.
func (c *compiler) keyword(r *raw, k string, v any, ptr string) error {
	var err error
	switch k {
	case "$ref":
		s, ok := v.(string)
		if !ok || !strings.HasPrefix(s, "#") {
			return schemaErr(ptr, "only local $ref values (#...) are supported")
		}
		r.refPtr = s
	case "$defs":
		m, ok := v.(map[string]any)
		if !ok {
			return schemaErr(ptr, "$defs must be an object")
		}
		for _, name := range slices.Sorted(maps.Keys(m)) {
			d, err := c.schema(m[name], ptr+"/"+escapeToken(name))
			if err != nil {
				return err
			}
			if r.ptr == "#" {
				c.defs[name] = d
			}
		}
	case "type":
		err = c.typeKeyword(r, v, ptr)
	case "const":
		r.values, r.hasValues = []any{v}, true
		r.types &= typesOf(v)
	case "enum":
		l, ok := v.([]any)
		if !ok {
			return schemaErr(ptr, "enum must be an array")
		}
		r.values, r.hasValues = l, true
		var t TypeSet
		for _, e := range l {
			t |= typesOf(e)
		}
		r.types &= t
	case "properties":
		m, ok := v.(map[string]any)
		if !ok {
			return schemaErr(ptr, "properties must be an object")
		}
		r.props = make(map[string]*raw, len(m))
		for _, name := range slices.Sorted(maps.Keys(m)) {
			if r.props[name], err = c.schema(m[name], ptr+"/"+escapeToken(name)); err != nil {
				return err
			}
		}
	case "required":
		r.required, err = stringList(v, ptr, "required")
	case "items":
		if _, ok := v.([]any); ok {
			return schemaErr(ptr, "array-form items is not supported")
		}
		r.items, err = c.schema(v, ptr)
	case "additionalProperties":
		if b, ok := v.(bool); ok {
			r.addlFalse = !b
			return nil
		}
		r.addl, err = c.schema(v, ptr)
	case "allOf", "anyOf", "oneOf":
		var list []*raw
		if list, err = c.schemaList(v, ptr, k); err != nil {
			return err
		}
		switch k {
		case "allOf":
			r.allOf = list
		case "anyOf":
			r.anyOf = list
		default:
			r.oneOf = list
		}
	case "not":
		_, err = c.schema(v, ptr)
	case "if":
		r.ifc, err = parseCond(v, ptr)
	case "then":
		r.then, err = c.schema(v, ptr)
	case "else":
		r.els, err = c.schema(v, ptr)
	case "default":
		r.dflt, r.hasDflt = v, true
	case "patternProperties", "prefixItems", "additionalItems",
		"dependentSchemas", "dependencies", "unevaluatedProperties", "unevaluatedItems",
		"$dynamicRef", "$dynamicAnchor", "$recursiveRef", "$recursiveAnchor", "$anchor", "definitions":
		// These change which schema a member or element takes, or whether
		// an object is closed, or how references resolve.
		return schemaErr(ptr, "keyword %s is not supported by the schema index", k)
	case "$id":
		// A nested $id would change the base of the local references.
		if r.ptr != "#" {
			return schemaErr(ptr, "$id is supported only at the document root")
		}
	default:
		if strings.HasPrefix(k, "x-ruralz-") {
			return c.extension(r, k, v, ptr)
		}
	}
	return err
}

func (c *compiler) typeKeyword(r *raw, v any, ptr string) error {
	var names []string
	switch t := v.(type) {
	case string:
		names = []string{t}
	case []any:
		var err error
		if names, err = stringList(t, ptr, "type"); err != nil {
			return err
		}
	default:
		return schemaErr(ptr, "type must be a string or an array of strings")
	}
	var set TypeSet
	for _, n := range names {
		t, ok := parseType(n)
		if !ok {
			return schemaErr(ptr, "unknown type %q", n)
		}
		set |= t
	}
	r.types &= set
	return nil
}

func (c *compiler) schemaList(v any, ptr, k string) ([]*raw, error) {
	l, ok := v.([]any)
	if !ok || len(l) == 0 {
		return nil, schemaErr(ptr, "%s must be a non-empty array", k)
	}
	out := make([]*raw, len(l))
	for i, e := range l {
		var err error
		if out[i], err = c.schema(e, ptr+"/"+strconv.Itoa(i)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// extension parses one x-ruralz-* keyword and asserts its shape
// (docs/architecture/02-configuration-model.md "Schema keywords that drive
// tooling"). An unknown x-ruralz-* keyword is an error: the vocabulary is
// closed, and a new keyword needs code here.
func (c *compiler) extension(r *raw, k string, v any, ptr string) error {
	switch k {
	case "x-ruralz-list":
		m, ok := v.(map[string]any)
		if !ok {
			return schemaErr(ptr, "x-ruralz-list must be an object")
		}
		for key := range m {
			if key != "type" && key != "key" {
				return schemaErr(ptr, "x-ruralz-list has unknown member %q", key)
			}
		}
		ts, _ := m["type"].(string)
		lt, ok := parseListType(ts)
		if !ok {
			return schemaErr(ptr, "x-ruralz-list type %q is not map, orderedMap, set or atomic", ts)
		}
		key, hasKey := m["key"]
		ks, _ := key.(string)
		switch {
		case lt.Keyed() && (!hasKey || ks == ""):
			return schemaErr(ptr, "x-ruralz-list %s needs a key", ts)
		case !lt.Keyed() && hasKey:
			return schemaErr(ptr, "x-ruralz-list %s takes no key", ts)
		}
		r.kw.List, r.kw.ListKey = lt, ks
	case "x-ruralz-ref":
		s, ok := v.(string)
		if !ok || s == "" {
			return schemaErr(ptr, "x-ruralz-ref must be a kind name")
		}
		r.kw.Ref = s
		c.refs = append(c.refs, r)
	case "x-ruralz-secret":
		b, ok := v.(bool)
		if !ok {
			return schemaErr(ptr, "x-ruralz-secret must be a boolean")
		}
		r.kw.Secret = b
	case "x-ruralz-cel":
		spec, err := parseCEL(v, ptr)
		if err != nil {
			return err
		}
		r.kw.CEL = spec
	case "x-ruralz-impact":
		names, err := stringList(v, ptr, k)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return schemaErr(ptr, "x-ruralz-impact must not be empty")
		}
		for _, n := range names {
			i, ok := ParseImpact(n)
			if !ok {
				return schemaErr(ptr, "unknown impact class %q", n)
			}
			r.kw.Impact |= i
		}
	case "x-ruralz-since":
		n, ok := v.(json.Number)
		level, err := strconv.Atoi(string(n))
		if !ok || err != nil || level < 0 {
			return schemaErr(ptr, "x-ruralz-since must be a non-negative integer")
		}
		r.kw.Since = level
	case "x-ruralz-validations":
		l, ok := v.([]any)
		if !ok {
			return schemaErr(ptr, "x-ruralz-validations must be an array")
		}
		for i, e := range l {
			val, err := parseValidation(e, ptr+"/"+strconv.Itoa(i))
			if err != nil {
				return err
			}
			r.kw.Validations = append(r.kw.Validations, val)
		}
	default:
		return schemaErr(ptr, "unknown keyword %s", k)
	}
	return nil
}

// parseValidation parses {"rule": <CEL>, "message"?: <string>}.
func parseValidation(e any, ptr string) (Validation, error) {
	bad := schemaErr(ptr, "a validation is {\"rule\": <CEL>, \"message\"?: <string>}")
	m, ok := e.(map[string]any)
	if !ok {
		return Validation{}, bad
	}
	var v Validation
	for key, val := range m {
		s, ok := val.(string)
		switch {
		case !ok:
			return Validation{}, bad
		case key == "rule":
			v.Rule = s
		case key == "message":
			v.Message = s
		default:
			return Validation{}, bad
		}
	}
	if v.Rule == "" {
		return Validation{}, bad
	}
	return v, nil
}

func parseCEL(v any, ptr string) (*CELSpec, error) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 2 {
		return nil, schemaErr(ptr, "x-ruralz-cel must be {\"variables\": [...], \"result\": ...}")
	}
	vars, err := stringList(m["variables"], ptr, "x-ruralz-cel variables")
	if err != nil {
		return nil, err
	}
	if len(vars) == 0 {
		return nil, schemaErr(ptr, "x-ruralz-cel variables must not be empty")
	}
	result, _ := m["result"].(string)
	switch result {
	case "bool", "string", "dyn":
	default:
		return nil, schemaErr(ptr, "x-ruralz-cel result %q is not bool, string or dyn", result)
	}
	return &CELSpec{Variables: vars, Result: result}, nil
}

// parseCond accepts the dispatch form the generator emits:
// {"properties": {"<m>": {"const": <scalar>}}, "required": ["<m>"]}.
func parseCond(v any, ptr string) (*cond, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, schemaErr(ptr, "if must be an object")
	}
	c := &cond{}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		switch k {
		case "required":
			var err error
			if c.required, err = stringList(m[k], ptr, "required"); err != nil {
				return nil, err
			}
		case "properties":
			props, ok := m[k].(map[string]any)
			if !ok {
				return nil, schemaErr(ptr, "if properties must be an object")
			}
			for _, name := range slices.Sorted(maps.Keys(props)) {
				p, ok := props[name].(map[string]any)
				cv, hasConst := p["const"]
				if !ok || !hasConst || len(p) != 1 || !isScalar(cv) {
					return nil, schemaErr(ptr, "if properties.%s must be {\"const\": <scalar>}", name)
				}
				c.consts = append(c.consts, Condition{Member: name, Value: cv})
			}
		default:
			return nil, schemaErr(ptr, "if supports only properties with const and required, not %s", k)
		}
	}
	return c, nil
}

func isScalar(v any) bool {
	switch v.(type) {
	case nil, string, bool, json.Number:
		return true
	default:
		return false
	}
}

func stringList(v any, ptr, what string) ([]string, error) {
	l, ok := v.([]any)
	if !ok {
		return nil, schemaErr(ptr, "%s must be an array of strings", what)
	}
	out := make([]string, len(l))
	for i, e := range l {
		if out[i], ok = e.(string); !ok {
			return nil, schemaErr(ptr, "%s must be an array of strings", what)
		}
	}
	return out, nil
}

// typesOf returns the instance type of a const or enum value.
func typesOf(v any) TypeSet {
	switch t := v.(type) {
	case nil:
		return TypeNull
	case bool:
		return TypeBoolean
	case string:
		return TypeString
	case json.Number:
		if isIntegerLiteral(string(t)) {
			return TypeInteger
		}
		return TypeNumber | TypeInteger
	case map[string]any:
		return TypeObject
	case []any:
		return TypeArray
	default:
		return 0
	}
}

// isIntegerLiteral reports whether a JSON number has no fraction or exponent.
func isIntegerLiteral(s string) bool { return !strings.ContainsAny(s, ".eE") }

// effTypes is the type set a raw node admits: its own constraint
// intersected with $ref and allOf, and with the union of each anyOf and
// oneOf. if/then parts are conditional and not included.
func effTypes(r *raw, memo map[*raw]TypeSet) TypeSet {
	if t, ok := memo[r]; ok {
		return t
	}
	memo[r] = AllTypes // a recursive reference constrains nothing further
	t := r.types
	if r.ref != nil {
		t &= effTypes(r.ref, memo)
	}
	for _, a := range r.allOf {
		t &= effTypes(a, memo)
	}
	for _, alts := range [][]*raw{r.anyOf, r.oneOf} {
		if len(alts) == 0 {
			continue
		}
		var u TypeSet
		for _, a := range alts {
			u |= effTypes(a, memo)
		}
		t &= u
	}
	memo[r] = t
	return t
}

// navFree rejects navigation inside anyOf and oneOf branches: the index
// merges only conjunctive parts, and the generator uses the combinators for
// scalar type unions and field combinations (exactlyOneOf, atLeastOneOf).
func navFree(r *raw, seen map[*raw]bool) error {
	if seen[r] {
		return nil
	}
	seen[r] = true
	if len(r.props) > 0 || r.items != nil || r.addl != nil || r.addlFalse || r.ifc != nil ||
		r.hasDflt || !isZeroKeywords(r.kw) {
		return schemaErr(r.ptr, "anyOf and oneOf branches may not declare properties, items, "+
			"additionalProperties, if, default or x-ruralz-* keywords")
	}
	for _, next := range slices.Concat([]*raw{r.ref}, r.allOf, r.anyOf, r.oneOf) {
		if next == nil {
			continue
		}
		if err := navFree(next, seen); err != nil {
			return err
		}
	}
	return nil
}

func isZeroKeywords(k Keywords) bool {
	return k.List == ListNone && k.ListKey == "" && k.Ref == "" && !k.Secret && k.CEL == nil &&
		k.Impact == 0 && k.Since == 0 && len(k.Validations) == 0
}

// resolve follows a local $ref ("#" plus a JSON pointer) and returns the
// target value with its canonical pointer.
func (c *compiler) resolve(ref string) (any, string, error) {
	frag := strings.TrimPrefix(ref, "#")
	if frag == "" {
		return c.doc, "#", nil
	}
	if !strings.HasPrefix(frag, "/") || strings.Contains(frag, "%") {
		return nil, "", fmt.Errorf("not a JSON pointer fragment")
	}
	cur := c.doc
	ptr := "#"
	for _, tok := range strings.Split(frag[1:], "/") {
		name := unescapeToken(tok)
		switch t := cur.(type) {
		case map[string]any:
			next, ok := t[name]
			if !ok {
				return nil, "", fmt.Errorf("no member %q", name)
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(name)
			if err != nil || i < 0 || i >= len(t) {
				return nil, "", fmt.Errorf("no element %q", name)
			}
			cur = t[i]
		default:
			return nil, "", fmt.Errorf("cannot descend into a scalar at %q", name)
		}
		ptr += "/" + escapeToken(name)
	}
	return cur, ptr, nil
}

func escapeToken(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

func unescapeToken(s string) string {
	return strings.NewReplacer("~1", "/", "~0", "~").Replace(s)
}
