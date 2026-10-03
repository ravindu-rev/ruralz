// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"encoding/json"
	"iter"
	"maps"
)

// Tree values are the types Decode builds: *Object or map[string]any,
// []any, string, json.Number, bool and nil. Clone, Equal and Cost walk the
// whole tree, which must be acyclic; the encoder instead stops a cycle with
// ErrDepth.

// Clone returns a deep copy of a tree value, so a working copy can change
// without touching the original (07 req 54, 55, 68). Scalars and values of
// other types are returned as they are.
func Clone(v any) any {
	switch x := v.(type) {
	case *Object:
		return x.Clone()
	case map[string]any:
		if x == nil {
			return x
		}
		c := make(map[string]any, len(x))
		for k, e := range x {
			c[k] = Clone(e)
		}
		return c
	case []any:
		if x == nil {
			return x
		}
		c := make([]any, len(x))
		for i, e := range x {
			c[i] = Clone(e)
		}
		return c
	default:
		return v
	}
}

// Equal reports whether two tree values are equal in the JSON data model:
// objects (*Object or map[string]any, in any combination) have the same
// members regardless of order, arrays the same elements in order, numbers
// the same literal text, strings and booleans the same value. A nil
// *Object, map or slice equals nil, as the encoder writes it as null.
// Values of other types are never equal.
func Equal(a, b any) bool {
	if isNull(a) || isNull(b) {
		return isNull(a) && isNull(b)
	}
	switch x := a.(type) {
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case json.Number:
		y, ok := b.(json.Number)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case *Object:
		return objectEqual(x.Len(), x.All(), b)
	case map[string]any:
		return objectEqual(len(x), maps.All(x), b)
	default:
		return false
	}
}

// isNull reports nil or a nil *Object, map or slice.
func isNull(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case *Object:
		return x == nil
	case map[string]any:
		return x == nil
	case []any:
		return x == nil
	default:
		return false
	}
}

// objectEqual compares the n members ms of a non-nil object with b.
func objectEqual(n int, ms iter.Seq2[string, any], b any) bool {
	var get func(string) (any, bool)
	switch y := b.(type) {
	case *Object:
		if y.Len() != n {
			return false
		}
		get = y.Get
	case map[string]any:
		if len(y) != n {
			return false
		}
		get = func(name string) (any, bool) {
			v, ok := y[name]
			return v, ok
		}
	default:
		return false
	}
	for name, va := range ms {
		vb, ok := get(name)
		if !ok || !Equal(va, vb) {
			return false
		}
	}
	return true
}

// Cost returns the cost of a tree value with the accounting Decode uses
// (see CostValue); for a value Decode built from input without duplicate
// names it equals the cost Decode reported. A Go number of another type
// costs as an 8-byte scalar; any other value costs CostValue.
func Cost(v any) int64 {
	if isNull(v) {
		return CostValue
	}
	switch x := v.(type) {
	case bool:
		return CostValue
	case string:
		return CostValue + CostString + int64(len(x))
	case json.Number:
		return CostValue + CostString + int64(len(x))
	case []any:
		n := int64(CostValue + CostArray)
		for _, e := range x {
			n += Cost(e)
		}
		return n
	case *Object:
		n := int64(CostValue + CostObject)
		for _, m := range x.members {
			n += CostMember + int64(len(m.Name)) + Cost(m.Value)
		}
		if len(x.members) > indexedMembers {
			n += int64(len(x.members)) * CostIndexEntry
		}
		return n
	case map[string]any:
		n := int64(CostValue + CostObject)
		for k, e := range x {
			n += CostMember + int64(len(k)) + Cost(e)
		}
		return n
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return CostValue + 8
	default:
		return CostValue
	}
}
