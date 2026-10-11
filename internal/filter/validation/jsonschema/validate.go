// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"errors"
	"fmt"

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
func (s *Schema) Validate(v any) (keywordLocation string, ok bool) {
	loc, ok, _ := s.validate(v)
	return loc, ok
}

// validate is Validate that also reports an internal validator error: a
// failure whose leaves include one that is not a decision about v.
func (s *Schema) validate(v any) (keywordLocation string, ok bool, err error) {
	res := s.root.Validate(v)
	if res == nil {
		return "", true, nil
	}
	var verr *sjs.ValidationError
	if !errors.As(res, &verr) {
		return "", false, fmt.Errorf("%w: %T", errValidator, res)
	}
	best, found := "", false
	var internal *sjs.ValidationError
	leaves(verr, func(l *sjs.ValidationError) {
		switch l.ErrorKind.(type) {
		case *kind.RefCycle, *kind.InvalidJsonValue:
			if internal == nil {
				internal = l
			}
			return
		}
		p, ok := keywordPointer(l)
		if ok && (!found || comparePointers(p, best) < 0) {
			best, found = p, true
		}
	})
	if internal != nil {
		// The kind and schema location only: the library's text may quote
		// the value (07 req 87).
		at, _ := pointerOf(internal.SchemaURL)
		return "", false, fmt.Errorf("%w: %T at %q", errValidator, internal.ErrorKind, at)
	}
	return best, false, nil
}

// keywordPointer returns the absolute keyword location of one error as a
// JSON Pointer into the document: the location of the schema that failed
// plus the failing keyword. The library leaves the keyword of a failed
// "not" empty; it is named here so the location points at the keyword.
func keywordPointer(e *sjs.ValidationError) (string, bool) {
	loc := e.SchemaURL
	var kw []string
	switch k := e.ErrorKind.(type) {
	case nil:
		return "", false
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
	p, ok := pointerOf(loc)
	if !ok {
		return "", false
	}
	for _, t := range kw {
		p = appendToken(p, t)
	}
	return p, true
}
