// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// Compile rules of 07 req 75.
const (
	// Draft is the only JSON Schema dialect: the default, and the exact
	// value every $schema in a document must carry.
	Draft = "https://json-schema.org/draft/2020-12/schema"
	// MaxSchemaBytes bounds the canonical (RFC 8785) form of a schema
	// document (proposed).
	MaxSchemaBytes = 256 << 10
	// MaxFanOut bounds the nested applicator fan-out (proposed).
	MaxFanOut = 1024
	// MaxSubschemas bounds the schema positions of a document (architecture
	// R-77): the library compiles a schema resource in one work queue that
	// it searches linearly, so compile time grows with the square of the
	// subschema count (about 0.3 s at this bound on a shared 4-vCPU
	// machine, against 2.3 s for the 37,000 empty subschemas 256 KiB can
	// hold; the evaluation analysis adds at most about 0.4 s, see the
	// package documentation). A position counts again for every
	// meta-schema check the library repeats on it (metacheck.go).
	MaxSubschemas = 4096
	// MaxEvaluations bounds the subschema evaluations that validating one
	// value of a body may take (architecture R-77): with MaxSubschemas
	// positions, a document that evaluates each subschema at most once per
	// value always fits, while a recursive reference reached through two
	// paths, which doubles the work at every level of nesting, crosses it
	// 10 to 12 levels down (see work.go). The first nesting depth at which
	// a value may cross it is the schema's nesting limit: a body value
	// nested that deep on a path the states do not bound is refused (413,
	// package documentation).
	MaxEvaluations = 4096
	// MinNesting is the smallest nesting limit a schema may have
	// (architecture R-77): a schema whose evaluations may cross
	// MaxEvaluations at a value nested less deep is refused.
	MinNesting = 16
	// MaxIssues bounds the findings CompileSchema reports for one document.
	MaxIssues = 20
	// MaxPatternInstructions bounds the measure of one pattern or
	// patternProperties name (architecture R-77), taken before anything
	// compiles it, in instructions of 40 bytes: what its compiled form
	// keeps, the instructions of the program Go's regexp compiles it into
	// (every copy a repetition expands to included), the ranges of its
	// classes, its literals' and classes' nodes and the regexp's own
	// structures, and the parser's work on its Unicode class escapes and
	// case-insensitive ranges (patternsize.go).
	MaxPatternInstructions = 1 << 14
	// MaxDocumentInstructions bounds the measures of all the patterns and
	// patternProperties names of one document together (architecture
	// R-77).
	MaxDocumentInstructions = 1 << 17
)

// baseURL is the URL every schema document is compiled under. It is a
// name only, in the reserved .invalid domain: the loader refuses every
// URL. It is hierarchical, so a relative reference such as "common.json"
// resolves to a URL outside the document and is refused rather than
// silently naming the document itself.
const baseURL = "https://schema.ruralz.invalid/policy.json"

// decideURL names the wrapper of the decision pass, compiled with the
// document.
const decideURL = "https://schema.ruralz.invalid/decide.json"

// checkURL names the resource whose allOf names the reference targets the
// library checks apart, compiled first (metacheck.go).
const checkURL = "https://schema.ruralz.invalid/check.json"

// maxMessageBytes bounds one finding's message, which may quote schema
// values from the library's meta-schema errors.
const maxMessageBytes = 256

// Sentinel errors of the compile step.
var (
	// ErrSchema marks every error CompileSchema returns (a *CompileError).
	ErrSchema = errors.New("validation.json-schema: schema does not compile")
	// ErrRemoteReference is what the loader answers for every URL: a
	// reference that leaves the document is never loaded.
	ErrRemoteReference = errors.New("validation.json-schema: references outside the schema document are not allowed")
)

// Issue is one reason a schema document does not compile.
type Issue struct {
	// Pointer is the RFC 6901 JSON Pointer of the offending location in
	// the schema document; "" is the document itself.
	Pointer string
	// Message is one line of operator-facing text.
	Message string
}

// CompileError lists the issues of a schema document that does not
// compile, at most MaxIssues of them, in document order. It matches
// ErrSchema with errors.Is.
type CompileError struct {
	Issues []Issue
}

// Error returns the first issue and the count of the others.
func (e *CompileError) Error() string {
	if len(e.Issues) == 0 {
		return ErrSchema.Error()
	}
	first := e.Issues[0]
	msg := ErrSchema.Error() + ": " + first.Message
	if first.Pointer != "" {
		msg += " at " + first.Pointer
	}
	if n := len(e.Issues) - 1; n > 0 {
		msg += fmt.Sprintf(" (and %d more)", n)
	}
	return msg
}

// Unwrap returns ErrSchema.
func (e *CompileError) Unwrap() error { return ErrSchema }

// Schema is a compiled schema document. It is immutable and safe for
// concurrent use.
type Schema struct {
	root *sjs.Schema
	// raw is the normalized document: members sorted, numbers verbatim, no
	// insignificant whitespace (jsonval.Append).
	raw []byte
	sum [32]byte
	// numbers is set when some subschema compares numbers, so request
	// numbers must obey the number bounds.
	numbers bool
	// decide validates with root for a boolean result only: the decision
	// pass (working.go).
	decide *sjs.Schema
	// work is the runtime model of the working set and the work of a body
	// (working.go).
	work workingSet
}

// Raw returns a copy of the normalized schema document (M2: OpenAPI
// export, AI tools inputSchemaFrom).
func (s *Schema) Raw() json.RawMessage { return append(json.RawMessage(nil), s.raw...) }

// Sum returns the SHA-256 of the normalized document, the identity under
// which compiled schemas are shared and carried over (07 req 77).
func (s *Schema) Sum() [32]byte { return s.sum }

// ComparesNumbers reports whether validation compares numbers, so a body
// number outside the number bounds is refused (package documentation).
func (s *Schema) ComparesNumbers() bool { return s.numbers }

// CompileSchema compiles a schema document under the rules of 07 req 75.
// Every failure is a *CompileError.
func CompileSchema(raw json.RawMessage) (*Schema, error) {
	s, _, issues := compileRaw(raw, refusingLoader{})
	if len(issues) > 0 {
		return nil, &CompileError{Issues: issues}
	}
	return s, nil
}

// refusingLoader refuses every URL. Documents added with AddResource and
// the library's bundled meta-schemas are served without it, so only
// references that leave the document reach it; the bundled meta-schemas
// are refused after compilation (inspect).
type refusingLoader struct{}

// Load always fails with ErrRemoteReference.
func (refusingLoader) Load(u string) (any, error) {
	return nil, fmt.Errorf("%w: %s", ErrRemoteReference, u)
}

// prepared is a decoded schema document with its identity.
type prepared struct {
	doc any
	raw []byte
	sum [32]byte
}

// prepare decodes raw strictly, checks its type and canonical size, and
// computes its normalized bytes and digest.
func prepare(raw []byte) (*prepared, []Issue) {
	doc, _, err := jsonval.Decode(raw, jsonval.Options{MapObjects: true})
	if err != nil {
		return nil, []Issue{{Message: message("schema is not valid JSON: " + err.Error())}}
	}
	switch doc.(type) {
	case map[string]any, bool:
	default:
		return nil, []Issue{{Message: "schema must be an object or a boolean"}}
	}
	if !canonicalWithin(doc, MaxSchemaBytes) {
		return nil, []Issue{{Message: fmt.Sprintf("schema is larger than %d bytes in canonical form", MaxSchemaBytes)}}
	}
	norm, err := jsonval.Append(nil, doc)
	if err != nil {
		return nil, []Issue{{Message: message("schema cannot be encoded: " + err.Error())}}
	}
	return &prepared{doc: doc, raw: norm, sum: sha256.Sum256(norm)}, nil
}

// canonicalWithin reports whether the RFC 8785 form of doc fits in limit
// bytes; a number a double cannot hold (the number bounds or stage G
// refuse it) is measured verbatim instead.
func canonicalWithin(doc any, limit int) bool {
	e := jsonval.Encoder{Options: jsonval.EncodeOptions{Order: jsonval.OrderUTF16, Numbers: jsonval.NumbersCanonical, MaxBytes: limit}}
	_, err := e.Append(nil, doc)
	if errors.Is(err, jsonval.ErrNumberRange) {
		e.Options.Numbers = jsonval.NumbersVerbatim
		_, err = e.Append(nil, doc)
	}
	return err == nil
}

// compileRaw prepares and compiles raw with loader; it returns the decoded
// document (nil when raw does not decode) for mapping issue pointers.
func compileRaw(raw []byte, loader sjs.URLLoader) (*Schema, any, []Issue) {
	p, issues := prepare(raw)
	if len(issues) > 0 {
		return nil, nil, issues
	}
	s, issues, _ := compilePrepared(p, loader, nil)
	return s, p.doc, issues
}

// compilePrepared runs the rule walk, compiles every schema position, and
// inspects and analyzes every schema the library compiled, with a regexp
// engine of its own. stop, when not nil, is asked before the library
// compiles the document and again before the evaluation analysis, the
// stages a canceled snapshot compile skips (minor review finding); its
// error ends the compile and is returned unchanged.
func compilePrepared(p *prepared, loader sjs.URLLoader, stop func() error) (*Schema, []Issue, error) {
	return compileWith(p, loader, stop, newRegexpEngine())
}

// msgPanicked is the finding for a compile that panicked (compileWith).
const msgPanicked = "the compile of this schema failed with an internal error, so it is refused"

// compileWith is compilePrepared with the regexp engine eng, which it
// freezes before it returns, whatever the outcome (audit class GC-21).
//
// A panic during the compile, in the library or in the compile's own code,
// is recovered into one finding at the document, msgPanicked, so that
// CompileSchema, Check and Factory.Build, which all compile through here,
// refuse the document with RZ-CFG-005 instead of ending the process (a
// backstop of library audit classes GC-01 and GC-02: the library's
// meta-schema check dereferenced math/big's nil result for a number it
// refuses at a position the walk did not walk, which ended stage H in
// every binary). It is a backstop only, and the work done before a panic
// is spent: the walk walks live every position the library compiles,
// through each of its reference keywords and with its base URIs (classes
// GC-01 and GC-02), so it refuses before the library compiles what the
// audit found to panic, and this fires only for a panic nothing found yet
// (TestCompileRecovers).
func compileWith(p *prepared, loader sjs.URLLoader, stop func() error, eng *regexpEngine) (s *Schema, issues []Issue, err error) {
	defer eng.freeze()
	defer func() {
		if recover() != nil {
			s, issues, err = nil, []Issue{{Message: msgPanicked}}, nil
		}
	}()
	w := newWalker(p.doc)
	w.walk()
	if len(w.issues) > 0 {
		return nil, w.issues, nil
	}
	return compileWalked(p, w, loader, stop, eng)
}

// compileWalked compiles a document whose walk found nothing with the
// regexp engine eng, asking stop as compilePrepared does. It freezes eng
// before it returns, whatever the outcome (audit class GC-21): once the
// compile has ended, no expression compiles and nothing is cached.
func compileWalked(p *prepared, w *walker, loader sjs.URLLoader, stop func() error, eng *regexpEngine) (*Schema, []Issue, error) {
	defer eng.freeze()
	if stop != nil {
		if err := stop(); err != nil {
			return nil, nil, err
		}
	}
	c, starts, issues := compileLibrary(p, w, loader, eng)
	if len(issues) > 0 {
		return nil, issues, nil
	}
	root := starts[0]
	// What the library compiled is all it can evaluate: the walked
	// positions, what they reach, and the schemas no edge leads to, the
	// resource roots and their $dynamicAnchor schemas, which the dynamic
	// scope reaches (compiled.go). The walk's $schema rule applies first
	// at every position the library read a $schema the walk did not, as it
	// would at the walk, then the inspection covers every compiled schema,
	// a compiled schema at a position the walk did not walk live is
	// refused, and the analysis covers the rest.
	compiled, listed := compiledSchemas(c)
	in, starts, issues := inspectCompiled(c, w, starts, compiled, listed)
	if len(issues) > 0 {
		return nil, issues, nil
	}
	if stop != nil {
		if err := stop(); err != nil {
			return nil, nil, err
		}
	}
	ws, issues := analyze(starts)
	if len(issues) > 0 {
		return nil, issues, nil
	}
	// The decision pass validates through a second resource whose not
	// applies the document: the library evaluates the subschema of not
	// for a boolean result, keeping no location of its errors and
	// stopping early where it can (validator.go), and the dynamic scope
	// the wrapper adds holds no anchor.
	if err := c.AddResource(decideURL, map[string]any{"not": map[string]any{"$ref": baseURL}}); err != nil {
		return nil, []Issue{{Message: message(err.Error())}}, nil
	}
	decide, err := c.Compile(decideURL)
	if err != nil {
		return nil, []Issue{{Message: message(err.Error())}}, nil
	}
	return &Schema{root: root, decide: decide, raw: p.raw, sum: p.sum, numbers: in.numbers, work: ws}, nil, nil
}

// compileLibrary has the library compile what the walk w walked live of
// the document, in the compile's order, with the regexp engine eng: the
// positions below additionalItems it compiles checked against the
// meta-schema first (checkApart), then the targets it checks apart
// (checkOrder), the root and every other position walked live. It returns
// the compiler and the compiled root and positions, the root first, or the
// findings of the first step that fails. The walk models these steps
// exactly (walker), so a document whose walk found nothing compiles here
// without asking the loader, and the library compiles exactly what the
// walk walked live (TestWalkMatchesLibrary).
func compileLibrary(p *prepared, w *walker, loader sjs.URLLoader, eng *regexpEngine) (*sjs.Compiler, []*sjs.Schema, []Issue) {
	c := sjs.NewCompiler()
	c.DefaultDraft(sjs.Draft2020)
	c.UseLoader(loader)
	c.UseRegexpEngine(eng.compile)
	if err := c.AddResource(baseURL, p.doc); err != nil {
		return nil, nil, []Issue{{Message: message(err.Error())}}
	}
	// What the library compiles below additionalItems it never checks
	// against the meta-schema, so the compile does, before the library
	// compiles any of it (metacheck.go).
	if unchecked := w.uncheckedPositions(); len(unchecked) > 0 {
		if issues := checkApart(p.doc, unchecked, loader, eng); len(issues) > 0 {
			return nil, nil, issues
		}
	}
	// The reference targets the library checks apart against the
	// meta-schema are checked first, in the walk's order: one work queue
	// that starts with references to them, in that order, collects and
	// checks each before anything else of the document (metacheck.go),
	// each compiling right after its own check, before the next is
	// collected, as the walk models (walker.close).
	if len(w.checkOrder) > 0 {
		refs := make([]any, len(w.checkOrder))
		for i, ptr := range w.checkOrder {
			refs[i] = map[string]any{"$ref": baseURL + "#" + encodeFragment(ptr)}
		}
		if err := c.AddResource(checkURL, map[string]any{"allOf": refs}); err != nil {
			return nil, nil, []Issue{{Message: message(err.Error())}}
		}
		if _, err := c.Compile(checkURL); err != nil {
			return nil, nil, w.compileIssues(err)
		}
	}
	// The library compiles a schema resource and every subschema it
	// reaches in one work queue, so the root goes first; the remaining
	// positions the walk walked live (unreferenced definitions, reference
	// targets outside the keywords) then find almost everything compiled.
	// The inert positions, which the library collects but never compiles,
	// stay uncompiled, as the walk's rules leave them alone (walker). Every
	// reference target is collected by now, so these compiles resolve
	// every reference against all of them (walker.close).
	root, err := c.Compile(baseURL)
	if err != nil {
		return nil, nil, w.compileIssues(err)
	}
	starts := []*sjs.Schema{root}
	for _, ptr := range w.positions[1:] {
		if w.seen[ptr] == walkedInert {
			continue
		}
		s, err := c.Compile(baseURL + "#" + encodeFragment(ptr))
		if err != nil {
			return nil, nil, w.compileIssues(err)
		}
		starts = append(starts, s)
	}
	return c, starts, nil
}

// checkApart checks the schema positions ptrs of doc against the draft
// 2020-12 meta-schema as the library's own checks do, with format
// assertions and the regexp engine eng, and returns the findings, at most
// MaxIssues of them. The library serves the meta-schema without loader.
// The meta-schema compiled here asserts format 'regex' through eng; it is
// dropped when checkApart returns, before the compile freezes eng.
func checkApart(doc any, ptrs []string, loader sjs.URLLoader, eng *regexpEngine) []Issue {
	mc := sjs.NewCompiler()
	mc.AssertFormat()
	mc.UseLoader(loader)
	mc.UseRegexpEngine(eng.compile)
	meta, err := mc.Compile(Draft)
	if err != nil {
		return []Issue{{Message: message(err.Error())}}
	}
	var out []Issue
	for _, ptr := range ptrs {
		v, _ := lookup(doc, ptr)
		err := meta.Validate(v)
		if err == nil {
			continue
		}
		var verr *sjs.ValidationError
		if !errors.As(err, &verr) {
			return append(out, Issue{Pointer: ptr, Message: message(err.Error())})
		}
		if out = append(out, metaIssues(baseURL+"#"+encodeFragment(ptr), verr)...); len(out) >= MaxIssues {
			return out[:MaxIssues]
		}
	}
	return out
}

// errEngineFrozen is what the regexp engine of a compile answers once the
// compile has ended (audit class GC-21).
var errEngineFrozen = errors.New("validation.json-schema: the schema compile has ended, so no regular expression compiles")

// regexpEngine is the regexp engine one compile gives the library
// (Compiler.UseRegexpEngine). It compiles every distinct regular expression
// of the document once: the library compiles a pattern or
// patternProperties name in every meta-schema check that covers it and
// again for the schema (review finding: a reference ladder compiled each of
// eight of the largest patterns 59 times). A compiled regexp is safe for
// concurrent use, so positions with the same expression share it, and a
// compiled pattern or patternProperties name keeps only its own
// *compiledPattern, never the engine.
//
// The compile freezes its engine before it returns (audit class GC-21):
// the cache is dropped and every later call fails, compiling and caching
// nothing. The library keeps its engine in a compiled schema only through
// an asserted format 'regex' (Format.Validate, called on every string
// evaluated there, objcompiler.go:186-192), which arises only under a
// dialect a $schema other than Draft gives; the compile refuses such a
// $schema wherever the library reads one (walker.collect, and
// checkCollected) and an asserted format at every schema the library
// compiled (inspect.go). Were a frozen engine called anyway, the format
// fails closed, and the engine writes nothing but its atomic call count,
// so concurrent requests can neither end the process with a concurrent
// map write nor keep request strings (library audit CC-01 and CC-02: one
// config.schema with an asserted format 'regex' ended the process under
// load, and every distinct body string stayed compiled for the life of
// the Schema).
type regexpEngine struct {
	// cache maps an expression to its compile; nil once frozen.
	cache map[string]cachedRegexp
	// frozen is set when the compile that owns the engine ends, before
	// the compiled Schema is shared.
	frozen bool
	// calls counts every call, before the freeze and after it, so tests
	// prove that validation never calls the engine.
	calls atomic.Int64
}

// cachedRegexp is one compile's result.
type cachedRegexp struct {
	re  *compiledPattern
	err error
}

// newRegexpEngine returns an empty engine for one compile.
func newRegexpEngine() *regexpEngine {
	return &regexpEngine{cache: map[string]cachedRegexp{}}
}

// compile is the library's regexp engine: Go's regexp without its
// one-pass matcher (compilePattern), once per expression; errEngineFrozen
// once the compile has ended.
func (e *regexpEngine) compile(expr string) (sjs.Regexp, error) {
	e.calls.Add(1)
	if e.frozen {
		return nil, errEngineFrozen
	}
	r, ok := e.cache[expr]
	if !ok {
		r.re, r.err = compilePattern(expr)
		e.cache[expr] = r
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.re, nil
}

// freeze ends the engine's compile: it drops the cache, so nothing the
// engine compiled stays reachable through it, and makes every later call
// fail.
func (e *regexpEngine) freeze() {
	e.frozen = true
	e.cache = nil
}

// compileIssues maps a compiler error to findings.
func (w *walker) compileIssues(err error) []Issue {
	var (
		meta *sjs.SchemaValidationError
		load *sjs.LoadURLError
	)
	switch {
	case errors.As(err, &meta):
		var verr *sjs.ValidationError
		if errors.As(meta.Err, &verr) {
			if out := metaIssues(meta.URL, verr); len(out) > 0 {
				return out
			}
		}
	case errors.As(err, &load):
		return w.loadIssues(load.URL)
	}
	return []Issue{{Message: message(err.Error())}}
}

// metaIssues maps a meta-schema validation failure of the document at
// location to one finding per failing position that has no failing
// position inside it: the leaves' instance locations are positions in the
// schema document, relative to location. Messages of one position are
// sorted and joined, so the result does not depend on the library's map
// iteration order.
func metaIssues(location string, verr *sjs.ValidationError) []Issue {
	base, ok := pointerOf(location)
	if !ok {
		return nil
	}
	byPtr := map[string][]string{}
	var ptrs []string
	leaves(verr, func(l *sjs.ValidationError) bool {
		p := base
		for _, tok := range l.InstanceLocation {
			p = appendToken(p, tok)
		}
		if _, seen := byPtr[p]; !seen {
			ptrs = append(ptrs, p)
		}
		byPtr[p] = appendUnique(byPtr[p], kindMessage(l.ErrorKind))
		return true
	})
	slices.SortFunc(ptrs, comparePointers)
	out := make([]Issue, 0, min(len(ptrs), MaxIssues))
	for i, p := range ptrs {
		if len(out) == MaxIssues {
			break
		}
		// A failure inside a value is more specific than the alternatives
		// an anyOf or oneOf of the meta-schema tried on the whole value
		// ("type": ["string", 7] fails at /type/1, not at /type); sorted
		// pointers put such a descendant right after its ancestor.
		if i+1 < len(ptrs) && strings.HasPrefix(ptrs[i+1], p+"/") {
			continue
		}
		msgs := byPtr[p]
		slices.Sort(msgs)
		out = append(out, Issue{Pointer: p, Message: message("invalid JSON Schema: " + strings.Join(msgs, "; or "))})
	}
	return out
}

// loadIssues reports a refused load at every reference the library
// compiles that leaves the document, in walk order, or at the document
// when none does. The walk refuses both before the library compiles, so
// this is a fallback the library's own error alone reaches.
func (w *walker) loadIssues(u string) []Issue {
	var out []Issue
	for _, s := range w.sites {
		if len(out) == MaxIssues {
			break
		}
		if w.seen[s.pos] == walkedLive && s.res.status == refOutside {
			out = append(out, Issue{Pointer: s.ptr, Message: message(fmt.Sprintf("reference %q resolves outside the schema document", s.value))})
		}
	}
	if len(out) == 0 {
		out = append(out, Issue{Message: message(fmt.Sprintf("a reference to %q resolves outside the schema document", u))})
	}
	return out
}

// joinURL resolves ref against base and drops the fragment, as the
// library resolves $id and the reference keywords (referenceKeywords).
func joinURL(base, ref string) (string, bool) {
	b, err := url.Parse(base)
	if err != nil {
		return "", false
	}
	ref, _, _ = strings.Cut(ref, "#")
	r, err := url.Parse(ref)
	if err != nil {
		return "", false
	}
	out := b.ResolveReference(r)
	if !r.IsAbs() && b.Opaque != "" {
		out.Opaque = b.Opaque
	}
	return out.String(), true
}

// leaves calls f for every error of the tree under e that has no causes,
// in order, until f returns false; it reports whether every call returned
// true.
func leaves(e *sjs.ValidationError, f func(*sjs.ValidationError) bool) bool {
	if len(e.Causes) == 0 {
		return f(e)
	}
	for _, c := range e.Causes {
		if !leaves(c, f) {
			return false
		}
	}
	return true
}

// kindMessage returns the library's English text of one error kind.
func kindMessage(k sjs.ErrorKind) string {
	s := (&sjs.ValidationError{ErrorKind: k}).Error()
	return strings.TrimPrefix(s, "at '': ")
}

// message returns s as one line of at most maxMessageBytes, with the
// compile URL of the document removed from locations it quotes.
func message(s string) string {
	s = strings.ReplaceAll(s, `"`+baseURL+`"`, `"#"`)
	s = strings.ReplaceAll(s, baseURL, "")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxMessageBytes {
		return s
	}
	cut := maxMessageBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

func appendUnique(list []string, s string) []string {
	for _, e := range list {
		if e == s {
			return list
		}
	}
	return append(list, s)
}
