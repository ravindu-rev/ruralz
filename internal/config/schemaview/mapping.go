// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"cmp"
	"encoding/json"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// maxEnumListed is the largest enum whose values a message lists; a longer
// one (PolicyType has 23 values) relies on the nearest-value hint.
const maxEnumListed = 12

// maxQuoted bounds the code points of an authored value quoted in a
// message, so one diagnostic stays one readable line.
const maxQuoted = 64

// syntaxText is the message of a scalar definition that has a documented
// syntax: every failure at the definition uses the syntax text instead of
// the keyword (01 req 33: Duration and ByteSize, docs/architecture/
// 02-configuration-model.md "Schema keywords that drive tooling").
func syntaxText(def string) (string, bool) {
	switch def {
	case "Duration":
		return "must be a Go duration such as 50ms", true
	case "ByteSize":
		return "must be an integer or quantity such as 10Mi", true
	case "Decimal":
		return `must be a decimal number written as a string, such as "1.50"`, true
	default:
		return "", false
	}
}

// fieldSyntax returns the syntax text of a string field whose pattern and
// length limits stand for a documented format, selected by schema location
// (definition and property), never by the regular expression: the text
// describes the format, the regex would not (CR 145).
func fieldSyntax(l location, k jsonschema.ErrorKind) (string, bool) {
	switch k.(type) {
	case *kind.Pattern, *kind.MinLength, *kind.MaxLength:
	default:
		return "", false
	}
	switch {
	case l.property("OTLP", "endpoint"):
		if _, ok := k.(*kind.Pattern); ok {
			return "must be an http or https URL with a host and an optional port, no path other than /, and no user information, query or fragment", true
		}
	case l.property("ObjectMeta", "name"):
		return "must be an RFC 1123 label of 1 to 63 characters: lower-case letters, digits and '-', starting and ending with a letter or digit", true
	case l.property("APIKey", "hash"):
		return "must be sha256: followed by 64 lower-case hexadecimal digits", true
	case l.property("BasicCredential", "hash"):
		return "must be pbkdf2-sha256:<salt>:<key> with a 22-character base64url salt and a 43-character base64url key", true
	case l.property("Pricing", "currency"):
		return "must be an upper-case ISO 4217 currency code such as EUR", true
	default:
		if d, rest, ok := l.def(); ok && d == "CountryCode" && len(rest) == 0 {
			return "must be an upper-case ISO 3166-1 alpha-2 country code such as DE", true
		}
	}
	return "", false
}

// MaxDiagnostics bounds the diagnostics Validate returns for one
// resource: the 01 req 50 cap of one run. A resource with more findings
// returns the first MaxDiagnostics in sorted order, whatever order the
// validator reported its errors in, and a run that reaches the cap stops
// and appends RZ-CFG-001 "too many diagnostics" (the pipeline's job).
const MaxDiagnostics = 10000

// mapper turns one resource's validation errors into diagnostics.
type mapper struct {
	v     *View
	res   *tree.Resource
	files *tree.FileTable
	// out holds at most 2*limit diagnostics; prune keeps the first limit
	// in sorted order, so memory stays bounded and the result does not
	// depend on the validator's error order (map iteration).
	out   []entry
	limit int
	// bound is the last entry prune kept, nil before the first prune: a
	// later entry that does not sort before it can never be in the result.
	bound *entry
	// seen holds the dedupe keys of out.
	seen map[dedupeKey]bool
	// memo memoizes the positions, paths and lookups of locate.
	memo *memo
	// secrets collects the findings of each x-ruralz-secret position, keyed
	// by its path text, so one diagnostic reports each secret field.
	secrets map[*textNode]*secretFinding
	order   []*secretFinding
	// shapes memoizes secretShape per secret field schema.
	shapes map[*schemaidx.Node]string
}

// entry is one diagnostic before its path is materialized: at is the
// shared path node, rendered as a diag.Path only for the diagnostics
// result keeps.
type entry struct {
	d  diag.Diagnostic
	at *pathNode
	// near, when set, is the nearest-name hint, computed only for the
	// diagnostics result keeps.
	near *nearest
}

// dedupeKey identifies a diagnostic by every field the sort compares; the
// path by its interned text, so no path is rendered to compare or dedupe.
type dedupeKey struct {
	code, msg string
	text      *textNode
	loc       diag.Location
}

// nearest is a deferred "did you mean" hint: the declared name nearest to
// name among the allowed ones (01 req 33).
type nearest struct {
	name    string
	allowed []string
}

// hint returns the hint text; "" when no allowed name is near.
func (n *nearest) hint() string {
	if near, ok := diag.Nearest(n.name, n.allowed); ok && near != n.name {
		return "did you mean " + quote(near) + "?"
	}
	return ""
}

// secretFinding gathers the errors inside one x-ruralz-secret subtree.
type secretFinding struct {
	at position
	// reasons are distinct; each is worded from the schema alone, so their
	// number is bounded by the schema, not by the instance.
	reasons []string
}

func newMapper(v *View, res *tree.Resource, files *tree.FileTable, limit int) *mapper {
	start, ok := v.idx.Resource(string(res.ID.Kind))
	if !ok {
		start = v.idx.Root()
	}
	return &mapper{
		v: v, res: res, files: files, limit: max(limit, 1),
		seen: map[dedupeKey]bool{}, memo: newMemo(start, res.Root), secrets: map[*textNode]*secretFinding{},
		shapes: map[*schemaidx.Node]string{},
	}
}

// locate resolves an instance location of this resource.
func (m *mapper) locate(loc []string) position {
	return m.memo.locate(loc)
}

// emit records one diagnostic at p, unless an identical one exists (a
// type failure can be reported by both a property and a dispatched
// branch).
func (m *mapper) emit(code string, p position, at tree.Pos, msg, hint string, related ...diag.Related) {
	m.record(code, p, at, msg, hint, nil, related)
}

// record is emit with an optional deferred hint, which replaces hint. Its
// cost does not depend on the size of the path's elements: the path is
// shared, compared by its interned text and rendered by no one here.
func (m *mapper) record(code string, p position, at tree.Pos, msg, hint string, near *nearest, related []diag.Related) {
	e := entry{d: diag.Diagnostic{
		Code: code, Severity: diag.SeverityError, Location: m.location(at),
		Message: msg, Hint: hint, Related: related,
	}, at: p.path, near: near}
	if m.bound != nil && m.compareEntries(&e, m.bound) >= 0 {
		return
	}
	key := dedupeKey{code: code, msg: msg, text: p.path.textOf(), loc: e.d.Location}
	if m.seen[key] {
		return
	}
	m.seen[key] = true
	m.out = append(m.out, e)
	if len(m.out) >= 2*m.limit {
		m.prune()
	}
}

// keyOf returns the dedupe key of e.
func keyOf(e *entry) dedupeKey {
	return dedupeKey{code: e.d.Code, msg: e.d.Message, text: e.at.textOf(), loc: e.d.Location}
}

// prune keeps the first limit diagnostics in sorted order and makes the
// last one the bound. A dropped diagnostic that is emitted again is
// dropped again: at least limit smaller ones stay kept.
func (m *mapper) prune() {
	m.sortEntries()
	clear(m.out[m.limit:])
	m.out = m.out[:m.limit]
	bound := m.out[m.limit-1]
	m.bound = &bound
	clear(m.seen)
	for i := range m.out {
		m.seen[keyOf(&m.out[i])] = true
	}
}

// result returns the first limit diagnostics in the order of
// diag.List.Sort, each with its path materialized.
func (m *mapper) result() diag.List {
	if len(m.out) == 0 {
		return nil
	}
	m.sortEntries()
	n := min(len(m.out), m.limit)
	out := make(diag.List, n)
	for i := range out {
		out[i] = m.out[i].d
		out[i].Path = m.out[i].at.path()
		if m.res.ID != (tree.ID{}) {
			out[i].Resource = m.res.ID.ResourceID()
		}
		if near := m.out[i].near; near != nil {
			out[i].Hint = near.hint()
		}
	}
	return out
}

// sortEntries orders out as diag.List.Sort orders diagnostics, by
// (environment, file, line, column, code, path, message). The dedupe key
// covers every field compared, so distinct entries never compare equal
// and the order is total.
func (m *mapper) sortEntries() {
	slices.SortFunc(m.out, func(a, b entry) int { return m.compareEntries(&a, &b) })
}

// compareEntries is the comparison of diag.List.Sort on entries. It
// stops at the first field that differs, and compares paths by their
// interned texts (memo.comparePaths), never by rendering them.
func (m *mapper) compareEntries(a, b *entry) int {
	if c := cmp.Compare(a.d.Environment, b.d.Environment); c != 0 {
		return c
	}
	if c := cmp.Compare(a.d.File, b.d.File); c != 0 {
		return c
	}
	if c := cmp.Compare(a.d.Line, b.d.Line); c != 0 {
		return c
	}
	if c := cmp.Compare(a.d.Column, b.d.Column); c != 0 {
		return c
	}
	if c := cmp.Compare(a.d.Code, b.d.Code); c != 0 {
		return c
	}
	if c := m.memo.comparePaths(a.at.textOf(), b.at.textOf()); c != 0 {
		return c
	}
	return cmp.Compare(a.d.Message, b.d.Message)
}

// location converts a tree position, falling back to the document start.
func (m *mapper) location(at tree.Pos) diag.Location {
	if !at.Known() {
		at = m.res.Start
	}
	if !at.Known() {
		return diag.Location{}
	}
	if m.files == nil {
		return diag.Location{Line: int(at.Line), Column: int(at.Column)}
	}
	return m.files.Location(at)
}

// visit maps one error and its causes. Wrappers (the schema, groups,
// $ref, allOf) pass through to their causes; anyOf, oneOf and not are
// worded as a whole; every other kind is a leaf.
func (m *mapper) visit(e *jsonschema.ValidationError) {
	p := m.locate(e.InstanceLocation)
	if p.secret != nil {
		m.secret(p, e)
		return
	}
	l, _ := m.v.parseLocation(e.SchemaURL)
	if d, rest, ok := l.def(); ok && (len(rest) == 0 || rest[0] == "anyOf") {
		// The definition itself, or one of its anyOf branches.
		if text, ok := syntaxText(d); ok {
			m.emit(CodeSchema, p, p.pos, text, "")
			return
		}
	}
	if text, ok := fieldSyntax(l, e.ErrorKind); ok {
		m.emit(CodeSchema, p, p.pos, text, "")
		return
	}
	switch k := e.ErrorKind.(type) {
	case *kind.Schema, *kind.Group, *kind.Reference, *kind.AllOf:
		for _, c := range e.Causes {
			m.visit(c)
		}
	case *kind.AnyOf:
		m.alternatives(e, p, "anyOf")
	case *kind.OneOf:
		if len(k.Subschemas) == 0 {
			m.alternatives(e, p, "oneOf")
			return
		}
		m.oneOfMany(e, p)
	case *kind.Not:
		m.not(e, p)
	case *kind.AdditionalProperties:
		m.unknown(p, k.Properties)
	case *kind.Required:
		for _, name := range sortedCopy(k.Missing) {
			m.emit(CodeSchema, p, p.pos, "missing required field "+name, "")
		}
	default:
		msg, hint := m.leafMessage(e, p)
		m.emit(CodeSchema, p, p.pos, msg, hint)
	}
}

// alternatives words a failed anyOf, or a oneOf that no branch matched.
// Branches that only require a member are the generator's atLeastOneOf
// and exactlyOneOf; otherwise the reported cause is the first branch that
// admits the instance's JSON type, and when none does, the allowed types
// are named (01 req 33).
func (m *mapper) alternatives(e *jsonschema.ValidationError, p position, keyword string) {
	obj := m.v.at(e.SchemaURL)
	branches, _ := obj[keyword].([]any)
	if names, ok := requiredOnly(branches); ok {
		if keyword == "anyOf" {
			m.emit(CodeSchema, p, p.pos, "at least one of "+strings.Join(names, ", ")+" is required", "")
			return
		}
		m.emit(CodeSchema, p, p.pos, "exactly one of "+strings.Join(names, ", ")+" is required", "")
		return
	}
	var want []string
	for _, c := range e.Causes {
		types, mismatch := m.typeMismatch(c, len(e.InstanceLocation))
		if !mismatch {
			m.visit(c)
			return
		}
		for _, t := range types {
			if !slices.Contains(want, t) {
				want = append(want, t)
			}
		}
	}
	m.emitType(p, want)
}

// oneOfMany words a oneOf that more than one branch matched.
func (m *mapper) oneOfMany(e *jsonschema.ValidationError, p position) {
	obj := m.v.at(e.SchemaURL)
	branches, _ := obj["oneOf"].([]any)
	if names, ok := requiredOnly(branches); ok {
		m.emit(CodeSchema, p, p.pos, "exactly one of "+strings.Join(names, ", ")+" is allowed, found "+joinAnd(present(p.node, names)), "")
		return
	}
	m.emit(CodeSchema, p, p.pos, "matches more than one alternative; exactly one is allowed", "")
}

// not words a failed not. The generator's atMostOneOf is not with one
// required-only schema for two fields, and not with an anyOf of
// required-only pairs for three or more
// (internal/tool/schemagen fieldCombination); both name every field in
// schema order (01 req 33).
func (m *mapper) not(e *jsonschema.ValidationError, p position) {
	obj := m.v.at(e.SchemaURL)
	if names, ok := atMostOneOf(obj["not"]); ok {
		m.emit(CodeSchema, p, p.pos, "at most one of "+strings.Join(names, ", ")+" is allowed, found "+joinAnd(present(p.node, names)), "")
		return
	}
	m.emit(CodeSchema, p, p.pos, "matches a form that is not allowed here", "")
}

// atMostOneOf reports whether the subschema of a not keyword is the
// generator's atMostOneOf, {"required": [a, b]} or {"anyOf": [{"required":
// [a, b]}, {"required": [a, c]}, ...]} with every branch a pair, and
// returns the field names in schema order.
func atMostOneOf(sub any) ([]string, bool) {
	obj, ok := sub.(map[string]any)
	if !ok {
		return nil, false
	}
	branches := []any{obj}
	if alts, ok := obj["anyOf"].([]any); ok && len(obj) == 1 {
		branches = alts
	}
	for _, b := range branches {
		bo, _ := b.(map[string]any)
		if req, ok := bo["required"].([]any); !ok || len(req) != 2 {
			return nil, false
		}
	}
	return requiredOnly(branches)
}

// typeMismatch reports whether a branch error is a type failure at the
// branch's own instance (depth) after unwrapping single-cause wrappers,
// with the branch's allowed types in schema order.
func (m *mapper) typeMismatch(e *jsonschema.ValidationError, depth int) ([]string, bool) {
	for {
		switch k := e.ErrorKind.(type) {
		case *kind.Type:
			if len(e.InstanceLocation) != depth {
				return nil, false
			}
			if t := typesOf(m.v.at(e.SchemaURL)); len(t) > 0 {
				return t, true
			}
			return k.Want, true
		case *kind.Reference, *kind.Group, *kind.Schema, *kind.AllOf:
			if len(e.Causes) != 1 {
				return nil, false
			}
			e = e.Causes[0]
		default:
			return nil, false
		}
	}
}

// emitType reports a type failure naming every allowed type.
func (m *mapper) emitType(p position, want []string) {
	msg, hint := typeText(p, want)
	m.emit(CodeSchema, p, p.pos, msg, hint)
}

// unknown reports each undeclared member of a closed object as RZ-CFG-006
// at its key, with the nearest declared name as the hint.
func (m *mapper) unknown(p position, names []string) {
	allowed := p.schema.PropertyNames()
	for _, name := range sortedCopy(names) {
		at := p.pos
		if i, ok := m.memo.index(p.node, name); ok && p.node.Members[i].KeyPos.Known() {
			at = p.node.Members[i].KeyPos
		}
		msg := "unknown field " + quote(name)
		var near *nearest
		switch {
		case name == "namespace" && p.path.isField("metadata"):
			msg += ": Bundles have no namespace"
		case name == "status" && p.path == nil:
			msg += ": only Ruralz Control writes status"
		case name == "$patch":
			msg += ": $patch is allowed only in overlay files"
		default:
			near = &nearest{name: name, allowed: allowed}
		}
		m.record(CodeUnknownField, m.positionAt(p, diag.Field(name)), at, msg, "", near, nil)
	}
}

// positionAt returns p extended by one path element.
func (m *mapper) positionAt(p position, e diag.PathElem) position {
	p.path = m.memo.child(p.path, e)
	return p
}

// leafMessage words a single-keyword failure.
func (m *mapper) leafMessage(e *jsonschema.ValidationError, p position) (msg, hint string) {
	obj := m.v.at(e.SchemaURL)
	switch k := e.ErrorKind.(type) {
	case *kind.Type:
		want := typesOf(obj)
		if len(want) == 0 {
			want = k.Want
		}
		return typeText(p, want)
	case *kind.Const:
		return "must be " + jsonText(k.Want), ""
	case *kind.Enum:
		return enumText(k)
	case *kind.Pattern:
		return "must match the pattern " + k.Want, ""
	case *kind.MinLength:
		if k.Want == 1 {
			return "must not be empty", ""
		}
		return "must be at least " + strconv.Itoa(k.Want) + " characters long", ""
	case *kind.MaxLength:
		return "must be at most " + strconv.Itoa(k.Want) + " characters long", ""
	case *kind.MinItems:
		return "must have at least " + count(k.Want, "entry", "entries"), ""
	case *kind.MaxItems:
		return "must have at most " + count(k.Want, "entry", "entries"), ""
	case *kind.MinProperties:
		return "must have at least " + count(k.Want, "member", "members"), ""
	case *kind.MaxProperties:
		return "must have at most " + count(k.Want, "member", "members"), ""
	case *kind.Minimum:
		return "must be at least " + bound(obj, "minimum", k.Want), ""
	case *kind.Maximum:
		return "must be at most " + bound(obj, "maximum", k.Want), ""
	case *kind.ExclusiveMinimum:
		return "must be greater than " + bound(obj, "exclusiveMinimum", k.Want), ""
	case *kind.ExclusiveMaximum:
		return "must be less than " + bound(obj, "exclusiveMaximum", k.Want), ""
	case *kind.MultipleOf:
		return "must be a multiple of " + bound(obj, "multipleOf", k.Want), ""
	case *kind.UniqueItems:
		return "entries " + strconv.Itoa(k.Duplicates[0]) + " and " + strconv.Itoa(k.Duplicates[1]) + " are equal", ""
	case *kind.FalseSchema:
		return "is not allowed here", ""
	case *kind.PropertyNames:
		return "member name " + quote(k.Property) + " is not allowed", ""
	case *kind.Contains:
		return "must contain a matching entry", ""
	case *kind.MinContains:
		return "must contain at least " + count(k.Want, "matching entry", "matching entries"), ""
	case *kind.MaxContains:
		return "must contain at most " + count(k.Want, "matching entry", "matching entries"), ""
	case *kind.DependentRequired:
		return k.Prop + " requires " + joinAnd(k.Missing), ""
	case *kind.Dependency:
		return k.Prop + " requires " + joinAnd(k.Missing), ""
	default:
		return "does not match the schema (" + strings.Join(e.ErrorKind.KeywordPath(), "/") + ")", ""
	}
}

// typeText words a type failure, naming every allowed type in schema
// order. A plain scalar such as 0777 or true is typed before the schema is
// known, so a non-string at a string position hints quoting (01 risk 5).
func typeText(p position, want []string) (msg, hint string) {
	got := typeName(p.node)
	msg = "must be " + joinTypes(want)
	if got != "" {
		msg += ", not " + got
	}
	if slices.Contains(want, "string") && p.node != nil {
		switch p.node.Kind {
		case tree.KindInt, tree.KindFloat, tree.KindBool:
			hint = "quote the value to make it a string"
		default:
		}
	}
	return msg, hint
}

// enumText lists the allowed values of a short enum and hints the nearest
// one; the authored value itself is not repeated (the position shows it).
func enumText(k *kind.Enum) (msg, hint string) {
	var names []string
	for _, w := range k.Want {
		if s, ok := w.(string); ok {
			names = append(names, s)
		}
	}
	if got, ok := k.Got.(string); ok {
		if near, ok := diag.Nearest(got, names); ok {
			hint = "did you mean " + quote(near) + "?"
		}
	}
	if len(k.Want) > maxEnumListed {
		return "is not one of the " + strconv.Itoa(len(k.Want)) + " allowed values", hint
	}
	vals := make([]string, 0, len(k.Want))
	for _, w := range k.Want {
		vals = append(vals, jsonText(w))
	}
	if len(vals) == 1 {
		return "must be " + vals[0], hint
	}
	return "must be one of " + strings.Join(vals, ", "), hint
}

// secret records an error inside an x-ruralz-secret subtree. The finding
// is reported once per secret field, at the field, with reasons worded
// from the schema alone: never the value, and never an authored member
// name, which could itself be the secret (01 req 33, RZ-CFG-012).
func (m *mapper) secret(p position, e *jsonschema.ValidationError) {
	key := p.secret.path.textOf()
	f, ok := m.secrets[key]
	if !ok {
		f = &secretFinding{at: *p.secret}
		m.secrets[key] = f
		m.order = append(m.order, f)
	}
	var below [4]diag.PathElem
	rel := declaredPath(p.secret.schema, p.path.appendBelow(below[:0], p.secret.path))
	switch k := e.ErrorKind.(type) {
	case *kind.Schema, *kind.Group, *kind.Reference, *kind.AllOf, *kind.AnyOf, *kind.OneOf, *kind.Not:
		for _, c := range e.Causes {
			m.visit(c)
		}
		if len(e.Causes) == 0 {
			f.add(rel + "is not valid")
		}
	case *kind.AdditionalProperties:
		f.add(rel + "accepts only " + joinOr(p.schema.PropertyNames()))
	case *kind.Required:
		f.add(rel + "needs " + joinAnd(sortedCopy(k.Missing)))
	case *kind.Type:
		want := typesOf(m.v.at(e.SchemaURL))
		if len(want) == 0 {
			want = k.Want
		}
		f.add(rel + "must be " + joinTypes(want))
	case *kind.Enum:
		msg, _ := enumText(&kind.Enum{Want: k.Want})
		f.add(rel + msg)
	case *kind.MinLength:
		f.add(rel + "must not be empty")
	case *kind.Pattern:
		f.add(rel + "does not match the required pattern")
	default:
		f.add(rel + "is not valid")
	}
}

// add records a reason once.
func (f *secretFinding) add(reason string) {
	if !slices.Contains(f.reasons, reason) {
		f.reasons = append(f.reasons, reason)
	}
}

// declaredPath words the path below a secret field whose schema is n,
// such as "secretRef.provider ", or "the field " at the field itself. A
// step the schema does not declare is never echoed.
func declaredPath(n *schemaidx.Node, rel diag.Path) string {
	if len(rel) == 0 {
		return "the field "
	}
	for _, e := range rel {
		if e.Kind != diag.ElemField {
			return "a nested value "
		}
		child, ok := n.Property(e.Name)
		if !ok {
			return "a nested value "
		}
		n = child
	}
	return rel.String() + " "
}

// flushSecrets emits one RZ-CFG-012 per secret field with findings.
func (m *mapper) flushSecrets() {
	for _, f := range m.order {
		shape, ok := m.shapes[f.at.schema]
		if !ok {
			shape = m.secretShape(f.at.schema, 0)
			m.shapes[f.at.schema] = shape
		}
		var msg string
		if f.at.node == nil || f.at.node.Kind != tree.KindMap {
			msg = "a literal value is not allowed in a secret field; reference the secret as " + shape
		} else {
			msg = "invalid secret reference: " + strings.Join(sortedCopy(f.reasons), "; ") + "; write it as " + shape
		}
		m.emit(CodeLiteral, f.at, f.at.pos, msg, "")
	}
}

// secretShape describes the required shape of a secret field from its
// schema, members in schema order, such as "{secretRef: {provider, name}}".
func (m *mapper) secretShape(n *schemaidx.Node, depth int) string {
	req := m.v.required(n.Def())
	if len(req) == 0 {
		req = n.Required()
	}
	if len(req) == 0 {
		return "a secretRef"
	}
	parts := make([]string, 0, len(req))
	for _, r := range req {
		child, _ := n.Property(r)
		if depth < 2 && len(child.Required()) > 0 {
			parts = append(parts, r+": "+m.secretShape(child, depth+1))
			continue
		}
		parts = append(parts, r)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// typeName names the JSON type of an instance node with its article.
func typeName(n *tree.Node) string {
	if n == nil {
		return ""
	}
	switch n.Kind {
	case tree.KindNull:
		return "null"
	case tree.KindBool:
		return "a boolean"
	case tree.KindInt:
		return "an integer"
	case tree.KindFloat:
		return "a number"
	case tree.KindString:
		return "a string"
	case tree.KindMap:
		return "an object"
	case tree.KindList:
		return "an array"
	default:
		return ""
	}
}

// article prefixes a schema type name.
func article(t string) string {
	switch t {
	case "object", "array", "integer":
		return "an " + t
	case "null":
		return "null"
	default:
		return "a " + t
	}
}

// joinTypes names every allowed type in schema order: "an object or a
// boolean" (CR 145).
func joinTypes(types []string) string {
	words := make([]string, len(types))
	for i, t := range types {
		words[i] = article(t)
	}
	return joinOr(words)
}

// joinOr joins words as "a", "a or b", "a, b or c".
func joinOr(words []string) string {
	return joinWith(words, " or ")
}

// joinAnd joins words as "a", "a and b", "a, b and c".
func joinAnd(words []string) string {
	return joinWith(words, " and ")
}

func joinWith(words []string, last string) string {
	switch len(words) {
	case 0:
		return "none"
	case 1:
		return words[0]
	default:
		return strings.Join(words[:len(words)-1], ", ") + last + words[len(words)-1]
	}
}

// present returns the names that are members of the object n.
func present(n *tree.Node, names []string) []string {
	var out []string
	for _, name := range names {
		if _, ok := n.Get(name); ok {
			out = append(out, name)
		}
	}
	return out
}

// count words n things.
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// bound returns a numeric keyword as the schema writes it, falling back to
// the validator's rational.
func bound(obj map[string]any, keyword string, r *big.Rat) string {
	if s, ok := numberText(obj, keyword); ok {
		return s
	}
	if r == nil {
		return "?"
	}
	if r.IsInt() {
		return r.Num().String()
	}
	f, _ := r.Float64()
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// jsonText writes a schema value as JSON without HTML escaping.
func jsonText(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "?"
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// quote writes s as a JSON string, shortened to maxQuoted code points.
func quote(s string) string {
	if utf8.RuneCountInString(s) > maxQuoted {
		r := []rune(s)
		s = string(r[:maxQuoted]) + "..."
	}
	return jsonText(s)
}

func sortedCopy(l []string) []string {
	out := slices.Clone(l)
	slices.Sort(out)
	return out
}
