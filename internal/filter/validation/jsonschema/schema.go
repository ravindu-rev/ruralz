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
	// MaxSubschemas bounds the schema positions of a document (proposed):
	// the library compiles a schema resource in one work queue that it
	// searches linearly, so compile time grows with the square of the
	// subschema count (about 0.2 s at this bound on a shared 4-vCPU
	// machine, against 2.3 s for the 37,000 empty subschemas 256 KiB can
	// hold; the evaluation analysis adds at most about 0.3 s, see the
	// package documentation).
	MaxSubschemas = 4096
	// MaxEvaluations bounds the subschema evaluations that validating one
	// value of a body may take, at any nesting depth up to 64 (proposed):
	// with MaxSubschemas positions, a document that evaluates each
	// subschema at most once per value always fits, while a recursive
	// reference reached through two paths, which doubles the work at every
	// level of nesting, does not (see work.go).
	MaxEvaluations = 4096
	// MaxIssues bounds the findings CompileSchema reports for one document.
	MaxIssues = 20
)

// baseURL is the URL every schema document is compiled under. It is a
// name only, in the reserved .invalid domain: the loader refuses every
// URL. It is hierarchical, so a relative reference such as "common.json"
// resolves to a URL outside the document and is refused rather than
// silently naming the document itself.
const baseURL = "https://schema.ruralz.invalid/policy.json"

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
	s, issues := compilePrepared(p, loader)
	return s, p.doc, issues
}

// compilePrepared runs the rule walk, compiles every schema position and
// inspects the compiled graph from each of them.
func compilePrepared(p *prepared, loader sjs.URLLoader) (*Schema, []Issue) {
	w := newWalker(p.doc)
	w.walk()
	if len(w.issues) > 0 {
		return nil, w.issues
	}
	return compileWalked(p, w, loader)
}

// compileWalked compiles a document whose walk found nothing.
func compileWalked(p *prepared, w *walker, loader sjs.URLLoader) (*Schema, []Issue) {
	c := sjs.NewCompiler()
	c.DefaultDraft(sjs.Draft2020)
	c.UseLoader(loader)
	if err := c.AddResource(baseURL, p.doc); err != nil {
		return nil, []Issue{{Message: message(err.Error())}}
	}
	// The library compiles a schema resource and every subschema it
	// reaches in one work queue, so the root goes first; the remaining
	// positions (unreferenced definitions, reference targets outside the
	// keywords) then find almost everything compiled. Compiling them too
	// applies the loader rule to every reference of the document.
	root, err := c.Compile(baseURL)
	if err != nil {
		return nil, w.compileIssues(err)
	}
	in := newInspector()
	in.visit(root, "")
	starts := []*sjs.Schema{root}
	for _, ptr := range w.positions[1:] {
		s, err := c.Compile(baseURL + "#" + encodeFragment(ptr))
		if err != nil {
			return nil, w.compileIssues(err)
		}
		in.visit(s, "")
		starts = append(starts, s)
	}
	if len(in.issues) > 0 {
		return nil, in.issues
	}
	wb, issues := analyze(starts)
	if len(issues) > 0 {
		return nil, issues
	}
	return &Schema{root: root, raw: p.raw, sum: p.sum, numbers: w.numbers || in.numbers, work: newWorkingSet(wb)}, nil
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
	leaves(verr, func(l *sjs.ValidationError) {
		p := base
		for _, tok := range l.InstanceLocation {
			p = appendToken(p, tok)
		}
		if _, seen := byPtr[p]; !seen {
			ptrs = append(ptrs, p)
		}
		byPtr[p] = appendUnique(byPtr[p], kindMessage(l.ErrorKind))
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

// loadIssues reports a refused load at every reference that leaves the
// document, in walk order, or at the document when none does (a $schema
// meta-schema URL, which the walk refuses earlier).
func (w *walker) loadIssues(u string) []Issue {
	var out []Issue
	for _, s := range w.sites {
		if len(out) == MaxIssues {
			break
		}
		if _, _, _, inDoc, err := resolveRef(w.ids, s); err == nil && !inDoc {
			out = append(out, Issue{Pointer: s.ptr, Message: message(fmt.Sprintf("reference %q resolves outside the schema document", s.value))})
		}
	}
	if len(out) == 0 {
		out = append(out, Issue{Message: message(fmt.Sprintf("a reference to %q resolves outside the schema document", u))})
	}
	return out
}

// joinURL resolves ref against base and drops the fragment, as the
// library resolves $id, $ref and $dynamicRef.
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

// leaves calls f for every error of the tree under e that has no causes.
func leaves(e *sjs.ValidationError, f func(*sjs.ValidationError)) {
	if len(e.Causes) == 0 {
		f(e)
		return
	}
	for _, c := range e.Causes {
		leaves(c, f)
	}
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
