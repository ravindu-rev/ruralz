// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"cmp"
	"encoding/json"
	"math"
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

// maxVarsNamed bounds the substituted variables one message names; the
// rest are counted, so one diagnostic stays one readable line.
const maxVarsNamed = 4

// patchMessage is the RZ-CFG-006 message of a $patch member in a base
// document, overlay.CheckBase's message (01 req 25).
const patchMessage = "$patch is valid only in overlays"

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

// MaxDiagnostics is the default diagnostic cap of one validation, the 01
// req 50 cap of a whole run; Options.MaxDiagnostics replaces it with what
// the run has left. A resource with more findings than the cap returns
// the first cap+1 in sorted order, whatever order the validator reported
// its errors in: a result longer than the cap means the cap was reached,
// and the run stops and appends one RZ-CFG-001 "too many diagnostics"
// (the pipeline's job, which a diag.Collector of the same cap does on the
// extra diagnostic). The cap also bounds what the mapping holds
// (Options.MaxDiagnostics).
const MaxDiagnostics = 10000

// mapper turns one resource's validation errors into diagnostics.
type mapper struct {
	v     *View
	res   *tree.Resource
	files *tree.FileTable
	// out holds fewer than 2*keep diagnostics; prune keeps the first keep
	// in sorted order, so memory stays bounded and the result does not
	// depend on the validator's error order (map iteration). keep is the
	// cap plus one: the extra diagnostic tells the caller the cap was
	// reached. No arithmetic on keep overflows, whatever the caller's cap
	// (newMapper, record).
	out  []entry
	keep int
	// bound is the last entry prune kept, nil before the first prune: a
	// later entry that does not sort before it can never be in the result.
	bound *entry
	// seen holds the dedupe keys of out.
	seen map[dedupeKey]bool
	// memo memoizes the positions, paths and lookups of locate.
	memo *memo
	// secrets collects the findings of each x-ruralz-secret position, keyed
	// by the position itself (the memoized step's secret mark, shared by
	// every position below it), so one diagnostic reports each secret
	// field: two positions can share a path text, such as the secretRef of
	// two keyed entries with one key (review finding: keyed by path text,
	// their findings merged at the first). order holds the same pending
	// findings; once they reach twice what the last prune kept, at least
	// 2*keep, pruneSecrets keeps those of the first keep groups, so they do
	// not grow with the secret fields in error (review finding: every
	// finding was held until the end).
	secrets map[*position]*secretFinding
	order   []*secretFinding
	// secretHeld is the number of findings the last pruneSecrets kept,
	// from which the next prune is counted, and secretPeak the most
	// pending at once, which the memory tests bound.
	secretHeld, secretPeak int
	// shapes memoizes secretShape per secret field schema.
	shapes map[*schemaidx.Node]string
	// shadowed holds the values of members an earlier member of the same
	// object and key hides (built.shadowed), which the checks after the
	// validator skip.
	shadowed map[*tree.Node]bool
	// work counts the call's work units for Options.Yield.
	work *work
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
	// mark is the position's secret mark, its key in mapper.secrets.
	mark *position
	// key is the finding's diagnostic without its message, what
	// pruneSecrets compares (compareKeys).
	key entry
	// reasons are distinct; each is worded from the schema alone, so their
	// number is bounded by the schema, not by the instance.
	reasons []string
}

// newMapper returns the mapper of one validation of res whose cap is limit:
// it keeps limit+1 diagnostics, limit taken as at least 1 and at most
// math.MaxInt-1 so that limit+1 does not overflow (a cap no resource
// reaches either way).
func newMapper(v *View, res *tree.Resource, files *tree.FileTable, limit int, opts Options) *mapper {
	start, ok := v.idx.Resource(string(res.ID.Kind))
	if !ok {
		start = v.idx.Root()
	}
	w := &work{yield: opts.Yield}
	m := &mapper{
		v: v, res: res, files: files, keep: min(max(limit, 1), math.MaxInt-1) + 1,
		seen: map[dedupeKey]bool{}, memo: newMemo(start, res.Root, w), secrets: map[*position]*secretFinding{},
		shapes: map[*schemaidx.Node]string{}, work: w,
	}
	m.memo.roots = m.roots
	return m
}

// roots marks the paths the mapper holds, which a memo sweep keeps: its
// diagnostics, the bound and the pending secret findings (sweep.go).
func (m *mapper) roots(mark func(*pathNode)) {
	for i := range m.out {
		mark(m.out[i].at)
	}
	if m.bound != nil {
		mark(m.bound.at)
	}
	for _, f := range m.order {
		mark(f.at.path)
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
	// len(m.out) >= 2*m.keep, written so that it cannot overflow: 2*m.keep
	// wraps for a cap of math.MaxInt/2 or more.
	if len(m.out)-m.keep >= m.keep {
		m.prune()
	}
}

// keyOf returns the dedupe key of e.
func keyOf(e *entry) dedupeKey {
	return dedupeKey{code: e.d.Code, msg: e.d.Message, text: e.at.textOf(), loc: e.d.Location}
}

// prune keeps the first keep diagnostics in sorted order and makes the
// last one the bound. A dropped diagnostic that is emitted again is
// dropped again: at least keep smaller ones stay kept.
func (m *mapper) prune() {
	m.sortEntries()
	clear(m.out[m.keep:])
	m.out = m.out[:m.keep]
	bound := m.out[m.keep-1]
	m.bound = &bound
	clear(m.seen)
	for i := range m.out {
		m.work.tick()
		m.seen[keyOf(&m.out[i])] = true
	}
}

// result returns the first keep diagnostics (the cap plus one) in the
// order of diag.List.Sort, each with its path materialized.
func (m *mapper) result() diag.List {
	if len(m.out) == 0 {
		return nil
	}
	m.sortEntries()
	n := min(len(m.out), m.keep)
	out := make(diag.List, n)
	for i := range out {
		m.work.tick()
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
// and the order is total. Each comparison is a work unit.
func (m *mapper) sortEntries() {
	slices.SortFunc(m.out, func(a, b entry) int {
		m.work.tick()
		return m.compareEntries(&a, &b)
	})
}

// compareEntries is the comparison of diag.List.Sort on entries. It
// stops at the first field that differs, and compares paths by their
// interned texts (memo.comparePaths), never by rendering them.
func (m *mapper) compareEntries(a, b *entry) int {
	if c := m.compareKeys(a, b); c != 0 {
		return c
	}
	return cmp.Compare(a.d.Message, b.d.Message)
}

// compareKeys is compareEntries without the message: the order of a
// diagnostic whose message is not known yet, a secret finding still
// gathering its reasons, which sorts after another whatever its message
// when compareKeys says so.
func (m *mapper) compareKeys(a, b *entry) int {
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
	return m.memo.comparePaths(a.at.textOf(), b.at.textOf())
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
	m.work.tick()
	p := m.locate(e.InstanceLocation)
	if p.secret != nil {
		m.secret(p, e)
		return
	}
	l, _ := m.v.parseLocation(e.SchemaURL)
	if d, rest, ok := l.def(); ok && (len(rest) == 0 || rest[0] == "anyOf") {
		// The definition itself, or one of its anyOf branches.
		if text, ok := syntaxText(d); ok {
			m.emitValue(p, text, "")
			return
		}
	}
	if text, ok := fieldSyntax(l, e.ErrorKind); ok {
		m.emitValue(p, text, "")
		return
	}
	switch k := e.ErrorKind.(type) {
	case *kind.Schema, *kind.Group, *kind.Reference, *kind.AllOf:
		m.visitCauses(e)
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
		m.emitValue(p, msg, hint)
	}
}

// visitCauses visits each cause of e and then releases it from e: the
// validator's error tree, a few hundred bytes per error, is freed while
// it is mapped, so the peak of stage F is the larger of the tree and
// what the mapping holds, not their sum (review finding: the whole tree
// stayed referenced until mapping ended). A cause that alternatives does
// not visit is released with its parent.
func (m *mapper) visitCauses(e *jsonschema.ValidationError) {
	for i, c := range e.Causes {
		m.visit(c)
		e.Causes[i] = nil
	}
}

// emitValue records an RZ-CFG-005 finding about the value at p. When stage
// E substituted variables into the value (tree.Node.Vars), the message
// names them: a re-typed value that does not fit, or any substituted value
// a keyword refuses, fails naming the variable (01 req 29). The findings
// about an object's members (missing required and unknown fields), about
// keys (the reserved prefix) and at secret positions, where stage E never
// substitutes, are worded without it.
func (m *mapper) emitValue(p position, msg, hint string) {
	var vars []string
	if p.node != nil {
		vars = p.node.Vars
	}
	m.emit(CodeSchema, p, p.pos, msg+substituted(vars, "the value"), hint)
}

// substituted returns the clause that names the variables stage E
// substituted into what a message is about, such as `; variable "PORT"
// was substituted into the value`, or "" for none. It names at most
// maxVarsNamed of them and counts the rest; it never contains a value.
func substituted(vars []string, into string) string {
	if len(vars) == 0 {
		return ""
	}
	names := make([]string, 0, min(len(vars), maxVarsNamed)+1)
	for _, v := range vars[:min(len(vars), maxVarsNamed)] {
		names = append(names, quote(v))
	}
	if more := len(vars) - maxVarsNamed; more > 0 {
		names = append(names, strconv.Itoa(more)+" more")
	}
	if len(vars) == 1 {
		return "; variable " + names[0] + " was substituted into " + into
	}
	return "; variables " + joinAnd(names) + " were substituted into " + into
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
			m.emitValue(p, "at least one of "+strings.Join(names, ", ")+" is required", "")
			return
		}
		m.emitValue(p, "exactly one of "+strings.Join(names, ", ")+" is required", "")
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
		m.emitValue(p, "exactly one of "+strings.Join(names, ", ")+" is allowed, found "+joinAnd(m.present(p.node, names)), "")
		return
	}
	m.emitValue(p, "matches more than one alternative; exactly one is allowed", "")
}

// not words a failed not. The generator's atMostOneOf is not with one
// required-only schema for two fields, and not with an anyOf of
// required-only pairs for three or more
// (internal/tool/schemagen fieldCombination); both name every field in
// schema order (01 req 33).
func (m *mapper) not(e *jsonschema.ValidationError, p position) {
	obj := m.v.at(e.SchemaURL)
	if names, ok := atMostOneOf(obj["not"]); ok {
		m.emitValue(p, "at most one of "+strings.Join(names, ", ")+" is allowed, found "+joinAnd(m.present(p.node, names)), "")
		return
	}
	m.emitValue(p, "matches a form that is not allowed here", "")
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
	m.emitValue(p, msg, hint)
}

// unknown reports each undeclared member of a closed object as RZ-CFG-006
// at its key, with the nearest declared name as the hint.
func (m *mapper) unknown(p position, names []string) {
	allowed := p.schema.PropertyNames()
	for _, name := range m.sortedNames(names) {
		m.work.tick()
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
			// overlay.CheckBase's message, 01 req 25's rule: CheckBase
			// reports it with the same code at the same key and path
			// before stage F, and content that enters at stage F
			// (FromResources) is reported here only, so the same rule
			// reads the same from either stage (review finding: stage F
			// added "unknown field").
			msg = patchMessage
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
// name, which could itself be the secret (01 req 33, RZ-CFG-012). When
// starting the field's finding prunes it at once (newSecret), the error's
// causes are not visited: they lie in the same subtree.
func (m *mapper) secret(p position, e *jsonschema.ValidationError) {
	f, ok := m.secrets[p.secret]
	if !ok {
		if f = m.newSecret(p.secret); f == nil {
			return
		}
	}
	var below [4]diag.PathElem
	rel := m.declaredPath(p.secret.schema, p.path.appendBelow(below[:0], p.secret.path))
	switch k := e.ErrorKind.(type) {
	case *kind.Schema, *kind.Group, *kind.Reference, *kind.AllOf, *kind.AnyOf, *kind.OneOf, *kind.Not:
		m.visitCauses(e)
		if len(e.Causes) == 0 {
			// not, or a oneOf that several branches matched: no cause
			// says what failed.
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

// newSecret starts the finding of the secret position mark. Once the
// pending findings reach twice what the last prune kept, at least
// 2*keep, it prunes them (pruneSecrets), and it returns nil when the new
// finding is pruned: the error's causes lie in the same subtree and
// would only start it again.
func (m *mapper) newSecret(mark *position) *secretFinding {
	f := &secretFinding{at: *mark, mark: mark, key: entry{
		d:  diag.Diagnostic{Code: CodeLiteral, Severity: diag.SeverityError, Location: m.location(mark.pos)},
		at: mark.path,
	}}
	m.secrets[mark] = f
	m.order = append(m.order, f)
	m.secretPeak = max(m.secretPeak, len(m.order))
	// len(m.order) >= 2*limit, written so that it cannot overflow.
	if limit := max(m.keep, m.secretHeld); len(m.order)-limit >= limit {
		m.pruneSecrets()
		if m.secrets[mark] != f {
			return nil
		}
	}
	return f
}

// pruneSecrets keeps the pending secret findings of the first keep
// groups of equal key (compareKeys) and drops the rest. Each kept group
// gives at least one diagnostic, distinct from every other group's and
// sorting before every dropped finding's whatever the messages, so no
// dropped finding is in the result. A later error of a dropped field
// starts a new finding, which sorts after the same groups or after ones
// that replaced them, so it is dropped again or cut from the result. A
// group holds more than one finding only when two secret positions share
// a location and a path text, which takes two keyed entries with one key
// and no source position. Each comparison and each finding dropped is a
// work unit.
func (m *mapper) pruneSecrets() {
	slices.SortFunc(m.order, func(a, b *secretFinding) int {
		m.work.tick()
		return m.compareKeys(&a.key, &b.key)
	})
	groups, end := 0, 0
	for end < len(m.order) && groups < m.keep {
		groups++
		first := m.order[end]
		for end++; end < len(m.order); end++ {
			m.work.tick()
			if m.compareKeys(&first.key, &m.order[end].key) != 0 {
				break
			}
		}
	}
	for _, f := range m.order[end:] {
		m.work.tick()
		delete(m.secrets, f.mark)
	}
	clear(m.order[end:])
	m.order = m.order[:end]
	m.secretHeld = end
}

// declaredPath words the path below a secret field whose schema is n,
// such as "secretRef.provider ", or "the field " at the field itself. A
// step the schema does not declare is never echoed. Looking a member name
// up in the schema hashes it, charged with its bytes: an undeclared name
// can be as long as the document.
func (m *mapper) declaredPath(n *schemaidx.Node, rel diag.Path) string {
	if len(rel) == 0 {
		return "the field "
	}
	for _, e := range rel {
		if e.Kind != diag.ElemField {
			return "a nested value "
		}
		m.work.charge(len(e.Name))
		child, ok := n.Property(e.Name)
		if !ok {
			return "a nested value "
		}
		n = child
	}
	return rel.String() + " "
}

// flushSecrets emits one RZ-CFG-012 per pending secret field finding and
// releases them.
func (m *mapper) flushSecrets() {
	for _, f := range m.order {
		m.work.tick()
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
	clear(m.secrets)
	m.order = nil
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
func (m *mapper) present(n *tree.Node, names []string) []string {
	var out []string
	for _, name := range names {
		if _, ok := m.member(n, name); ok {
			out = append(out, name)
		}
	}
	return out
}

// member returns the value of the first member named key of n, as
// tree.Node.Get does, charging one work unit per member it scans: an
// object can hold as many members as the instance.
func (m *mapper) member(n *tree.Node, key string) (*tree.Node, bool) {
	if n == nil || n.Kind != tree.KindMap {
		return nil, false
	}
	for i := range n.Members {
		m.work.tick()
		if n.Members[i].Key == key {
			return n.Members[i].Value, true
		}
	}
	return nil, false
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

// quote writes s as a JSON string, shortened to maxQuoted code points. It
// reads at most maxQuoted code points of s, however long s is.
func quote(s string) string {
	if head, long := clip(s); long {
		s = string([]rune(head)) + "..."
	}
	return jsonText(s)
}

// clip returns the first maxQuoted code points of s, an invalid UTF-8 byte
// counting as one as utf8.DecodeRuneInString and []rune count it, and
// whether s is longer. It reads at most that prefix: the authored texts
// quoted in messages (member names, keys, set elements) can be as long as
// the document, and counting or converting all of s would cost its length
// per message (review finding: []rune(s) allocated 4 bytes per byte).
func clip(s string) (string, bool) {
	i := 0
	for range maxQuoted {
		if i == len(s) {
			return s, false
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i], i < len(s)
}

func sortedCopy(l []string) []string {
	out := slices.Clone(l)
	slices.Sort(out)
	return out
}

// sortedNames is sortedCopy for a list as long as the instance, such as
// the unknown members of one object: each comparison is a work unit, and
// so is each byte of the common prefix it reads, since two long names can
// share most of their bytes.
func (m *mapper) sortedNames(l []string) []string {
	out := slices.Clone(l)
	slices.SortFunc(out, func(a, b string) int {
		n := commonPrefix(a, b)
		m.work.charge(1 + n)
		switch {
		case n < len(a) && n < len(b):
			return cmp.Compare(a[n], b[n])
		default:
			return cmp.Compare(len(a), len(b))
		}
	})
	return out
}
