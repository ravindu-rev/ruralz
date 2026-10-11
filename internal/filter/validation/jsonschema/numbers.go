// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/json"
	"strings"
)

// Number bounds (proposed, see the package documentation): the longest
// number literal and the largest decimal exponent magnitude that a schema
// document may hold, and that a request body may hold when its schema
// compares numbers. Both admit every IEEE 754 double (exponents up to
// about ±324) and integers of over 400 bits, while bounding the
// math/big work the validator spends on one literal to a few microseconds.
const (
	// MaxNumberBytes is the longest number literal, in bytes.
	MaxNumberBytes = 128
	// MaxNumberExponent bounds the magnitude of a literal's decimal
	// exponent (the digits after "e" or "E").
	MaxNumberExponent = 1000
)

// numberInRange reports whether the RFC 8259 number literal lit is within
// MaxNumberBytes and MaxNumberExponent. Leading zeros of the exponent do
// not count, so 1e0005 is in range when its length is.
func numberInRange(lit string) bool {
	if len(lit) > MaxNumberBytes {
		return false
	}
	i := strings.IndexAny(lit, "eE")
	if i < 0 {
		return true
	}
	exp := lit[i+1:]
	if exp != "" && (exp[0] == '+' || exp[0] == '-') {
		exp = exp[1:]
	}
	exp = strings.TrimLeft(exp, "0")
	if len(exp) > 4 {
		return false
	}
	n := 0
	for j := 0; j < len(exp); j++ {
		n = n*10 + int(exp[j]-'0')
	}
	return n <= MaxNumberExponent
}

// numbersInRange reports whether every number in the decoded tree v is in
// range. It allocates nothing; recursion is bounded by the decoder's
// nesting limit.
func numbersInRange(v any) bool {
	switch t := v.(type) {
	case json.Number:
		return numberInRange(string(t))
	case []any:
		for _, e := range t {
			if !numbersInRange(e) {
				return false
			}
		}
	case map[string]any:
		for _, e := range t {
			if !numbersInRange(e) {
				return false
			}
		}
	}
	return true
}
