// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// location is a schema location of the view split into its JSON pointer
// tokens, such as ["$defs", "OTLP", "properties", "endpoint"].
type location []string

// parseLocation splits a schema location reported by the validator, the
// view's base URL, "#" and a JSON pointer whose tokens are percent-encoded
// (jsonschema/v6 encodes each token with url.PathEscape after the RFC 6901
// "~0" and "~1" escapes). It returns false for a location outside the view.
func (v *View) parseLocation(loc string) (location, bool) {
	rest, ok := strings.CutPrefix(loc, v.base)
	if !ok {
		return nil, false
	}
	frag, ok := strings.CutPrefix(rest, "#")
	if !ok && rest != "" {
		return nil, false
	}
	if frag == "" {
		return location{}, true
	}
	if frag[0] != '/' {
		return nil, false
	}
	toks := strings.Split(frag[1:], "/")
	for i, t := range toks {
		u, err := url.PathUnescape(t)
		if err != nil {
			return nil, false
		}
		toks[i] = strings.ReplaceAll(strings.ReplaceAll(u, "~1", "/"), "~0", "~")
	}
	return toks, true
}

// def returns the $defs name the location lies in and the tokens below the
// definition; false outside $defs (the root dispatch).
func (l location) def() (name string, rest location, ok bool) {
	if len(l) < 2 || l[0] != "$defs" {
		return "", nil, false
	}
	return l[1], l[2:], true
}

// property reports whether the location is the schema of member name of
// definition def: $defs/<def>/properties/<name>.
func (l location) property(def, name string) bool {
	d, rest, ok := l.def()
	return ok && d == def && len(rest) == 2 && rest[0] == "properties" && rest[1] == name
}

// at returns the schema object at loc in the decoded document; nil when
// the location does not name an object (a boolean schema, or a location
// outside the view).
func (v *View) at(loc string) map[string]any {
	l, ok := v.parseLocation(loc)
	if !ok {
		return nil
	}
	cur := v.doc
	for _, t := range l {
		switch c := cur.(type) {
		case map[string]any:
			cur = c[t]
		case []any:
			i, err := strconv.Atoi(t)
			if err != nil || i < 0 || i >= len(c) {
				return nil
			}
			cur = c[i]
		default:
			return nil
		}
	}
	m, _ := cur.(map[string]any)
	return m
}

// requiredOnly reports whether every branch of a combination keyword is a
// schema whose only keyword is required, the form the generator emits for
// exactlyOneOf (oneOf), atLeastOneOf (anyOf) and atMostOneOf (not), and
// returns the member names in schema order without duplicates.
func requiredOnly(branches []any) ([]string, bool) {
	if len(branches) == 0 {
		return nil, false
	}
	var names []string
	for _, b := range branches {
		obj, ok := b.(map[string]any)
		if !ok || len(obj) != 1 {
			return nil, false
		}
		req, ok := stringArray(obj["required"])
		if !ok || len(req) == 0 {
			return nil, false
		}
		for _, r := range req {
			if !slices.Contains(names, r) {
				names = append(names, r)
			}
		}
	}
	return names, true
}

// typesOf returns the type keyword of a schema object in schema order: a
// string, or the elements of an array of strings.
func typesOf(obj map[string]any) []string {
	switch t := obj["type"].(type) {
	case string:
		return []string{t}
	case []any:
		out, _ := stringArray(t)
		return out
	default:
		return nil
	}
}

// required returns the required member names of definition def in schema
// order; nil when def has none.
func (v *View) required(def string) []string {
	root, _ := v.doc.(map[string]any)
	defs, _ := root["$defs"].(map[string]any)
	obj, _ := defs[def].(map[string]any)
	out, _ := stringArray(obj["required"])
	return out
}

// numberText returns a numeric keyword's text as the schema writes it.
func numberText(obj map[string]any, keyword string) (string, bool) {
	n, ok := obj[keyword].(json.Number)
	return string(n), ok
}
