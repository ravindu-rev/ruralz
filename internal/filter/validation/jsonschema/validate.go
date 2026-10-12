// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"errors"
	"fmt"
	"slices"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// errValidator is the cause of cannot decide when the library fails
// rather than decides (07 req 13): a reference cycle that never descends
// into the value, which CompileSchema refuses, so it is a backstop.
var errValidator = errors.New("validation.json-schema: the validator cannot decide")

// Validate validates the decoded instance v (the encoding/json data model
// with json.Number numbers: map[string]any, []any, string, json.Number,
// bool, nil). On failure it returns the absolute keyword location of the
// first failing leaf in pointer order (comparePointers), a JSON Pointer
// into the schema document (07 req 81). The library reports sibling
// failures in map iteration order, so pointer order is what makes the
// choice deterministic. The location is "" for the document root, and also
// when no leaf has a location in the document or the library cannot
// decide (an internal error the compile rules exclude).
//
// Like Handle, Validate refuses, not ok at the root, a v holding a number
// the library would parse outside the number bounds (which would make the
// library panic or spend milliseconds per literal) or a value as deep as
// the schema's nesting limit on a path whose state does not bound it.
// Unlike Handle, it bounds neither the memory nor the work of validation,
// so it is for values its caller trusts; ValidateWithin applies Handle's
// bounds to the others (M2: OpenAPI examples, AI tool inputs).
func (s *Schema) Validate(v any) (keywordLocation string, ok bool) {
	scan := bodyScan{ws: &s.work, numbers: s.numbers, workLimit: maxWorkLimit}
	scan.scan(v)
	if scan.bad || scan.deep {
		return "", false
	}
	loc, ok, _ := s.validate(v, nil)
	return loc, ok
}

// ValidateWithin is Validate within the bounds Handle applies to a body
// under a limits.maxRequestBodyBytes of rawLimit (the configuration
// default when 0 or less; package documentation "Working set" and "Work
// bound"), for values from untrusted sources (minor review finding: the
// model that bounds Handle was not available to M2 callers). Before the
// library runs, a v whose decision pass may hold more than
// WorkingSetFactor times rawLimit besides v or take more than WorkPerByte
// work units per byte of it, or that nests as deep as the schema's nesting
// limit on a path whose state does not bound it, is an error matching
// filter.ErrTooLarge. A v holding a number out of the number bounds is not
// ok at the root, as is a failing v whose location pass does not fit the
// same bounds; an internal validator error is returned as such. It
// reserves nothing: its caller accounts for the memory, at most
// WorkingSetFactor times rawLimit besides v.
func (s *Schema) ValidateWithin(v any, rawLimit int64) (keywordLocation string, ok bool, err error) {
	limits := limitsOf(rawLimit)
	scan := bodyScan{ws: &s.work, numbers: s.numbers, workLimit: limits.work}
	scan.scan(v)
	if scan.bad {
		return "", false, nil
	}
	if err := limits.refusal(&scan); err != nil {
		return "", false, err
	}
	if s.valid(v) {
		return "", true, nil
	}
	if scan.locateMemory() > limits.workingSet || scan.locateOver() {
		return "", false, nil
	}
	loc, _, err := s.validate(v, nil)
	return loc, false, err
}

// valid is the decision pass: it reports whether v is valid, without the
// location of a failure. The library answers it through the wrapper,
// which fails exactly when the document accepts v; a failure the library
// cannot decide counts as invalid, so the location pass reports it.
func (s *Schema) valid(v any) bool {
	return s.decide.Validate(v) != nil
}

// walkCheck is how many leaves the location walk visits between two calls
// of its stop function.
const walkCheck = 256

// validate is Validate that also reports an internal validator error: a
// failure whose leaves include one that is not a decision about v. A valid
// v is ("", true, nil). The walk over the leaves stops when stop (nil for
// never), called every walkCheck leaves, reports true: the location is
// then "", the document root. The walk takes work per leaf that grows with
// the length of the leaf's location, which the work model charges
// (walkWork, review finding); a leaf at the location of the best one so
// far, the common case of a dense failing body, costs no more than
// comparing the two locations, which share their bytes.
func (s *Schema) validate(v any, stop func() bool) (keywordLocation string, ok bool, err error) {
	res := s.root.Validate(v)
	if res == nil {
		return "", true, nil
	}
	var verr *sjs.ValidationError
	if !errors.As(res, &verr) {
		return "", false, fmt.Errorf("%w: %T", errValidator, res)
	}
	return locationOf(verr, stop)
}

// locationOf is the location walk of validate over the error tree verr.
func locationOf(verr *sjs.ValidationError, stop func() bool) (keywordLocation string, ok bool, err error) {
	var (
		best, bestLoc string
		bestKw        []string
		found         bool
		internal      *sjs.ValidationError
		visited       int
	)
	complete := leaves(verr, func(l *sjs.ValidationError) bool {
		if visited++; stop != nil && visited%walkCheck == 0 && stop() {
			return false
		}
		switch l.ErrorKind.(type) {
		case *kind.RefCycle, *kind.InvalidJsonValue:
			if internal == nil {
				internal = l
			}
			return true
		}
		loc, kw, ok := keywordParts(l)
		if !ok || (found && loc == bestLoc && slices.Equal(kw, bestKw)) {
			return true
		}
		p, ok := keywordAt(loc, kw)
		if ok && (!found || comparePointers(p, best) < 0) {
			best, bestLoc, bestKw, found = p, loc, kw, true
		}
		return true
	})
	if internal != nil {
		// The kind and schema location only: the library's text may quote
		// the value (07 req 87).
		at, _ := pointerOf(internal.SchemaURL)
		return "", false, fmt.Errorf("%w: %T at %q", errValidator, internal.ErrorKind, at)
	}
	if !complete {
		return "", false, nil
	}
	return best, false, nil
}

// keywordParts returns the schema location URL of one error and the
// tokens of its failing keyword. The library leaves the keyword of a
// failed "not" empty; it is named here so the location points at the
// keyword.
func keywordParts(e *sjs.ValidationError) (loc string, kw []string, ok bool) {
	loc = e.SchemaURL
	switch k := e.ErrorKind.(type) {
	case nil:
		return "", nil, false
	case *kind.Reference:
		loc = k.URL
	case *kind.Not:
		kw = []string{"not"}
	case *kind.Dependency:
		// The library names the keyword "dependency"; the document
		// spells it "dependencies".
		kw = []string{"dependencies", k.Prop}
	default:
		kw = e.ErrorKind.KeywordPath()
	}
	return loc, kw, true
}

// keywordAt returns the JSON Pointer of the schema location URL loc
// extended by the keyword tokens kw; ok is false for a location outside
// the document.
func keywordAt(loc string, kw []string) (string, bool) {
	p, ok := pointerOf(loc)
	if !ok {
		return "", false
	}
	for _, t := range kw {
		p = appendToken(p, t)
	}
	return p, true
}
