// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Value is the expr.Value of the CEL implementation: a read-only decoded
// JSON document (a body or JWT claims), a dyn result, or an error value for
// a body that did not decode. A nil *Value and the zero Value are null. A
// Value is immutable and safe for concurrent readers; it must not outlive
// the request memory it views (03 req 39).
type Value struct {
	// v is the CEL value; nil is null. It is never a *types.Err.
	v ref.Val
	// err is the error of an error value. It is kept as a plain error and
	// wrapped in a new *types.Err on every read, because cel-go writes the
	// AST node ID into the error values an evaluation produces
	// (types.LabelErrNode): one *types.Err shared by the readers of a body
	// would be written by concurrent evaluations (03 req 25, 38, 39).
	err error
	// native is the decoded tree of a FromNative Value, which AppendBody and
	// Native write from so numbers keep their literal text at every level.
	native   any
	isNative bool
}

// Compile-time check that *Value implements expr.Value.
var _ expr.Value = (*Value)(nil)

// FromNative returns the Value of a decoded JSON tree: map[string]any,
// []any, string, json.Number, bool or nil (a nil map or slice is null). The
// tree is viewed, never copied, so the caller must not modify it afterwards.
// Objects iterate keys in ascending byte order; a json.Number that is an
// integer literal within int64 reads as CEL int, any other number as double
// (03 req 25). Another Go type reads as an error wrapping ErrUnsupported.
func FromNative(native any) *Value {
	x := valueOf(nativeVal(native))
	x.native, x.isNative = native, true
	return x
}

// FromVal returns the Value of a CEL value, such as a dyn program result. A
// CEL error value becomes an error Value.
func FromVal(v ref.Val) *Value { return valueOf(v) }

// ErrorValue returns a Value that reads as a runtime error wrapping ErrBody
// and err: the body of 03 req 25 that did not decode under a JSON content
// type, so an expression that reads it fails safe.
func ErrorValue(err error) *Value {
	return &Value{err: fmt.Errorf("%w: %w", ErrBody, err)}
}

// Null returns JSON null, the body under a non-JSON content type.
func Null() *Value { return &Value{v: types.NullValue} }

// valueOf returns the Value of v, keeping a CEL error value as its plain
// error so that no *types.Err is stored.
func valueOf(v ref.Val) *Value {
	if e, ok := v.(*types.Err); ok {
		return &Value{err: plainError(e)}
	}
	return &Value{v: v}
}

// plainError returns the error a CEL error value wraps, so that
// types.WrapErr builds a new *types.Err from it.
func plainError(e *types.Err) error {
	if err := e.Unwrap(); err != nil {
		return err
	}
	return fmt.Errorf("%w: CEL error value without an error", ErrUnsupported)
}

// Val returns the CEL value. An error value returns a new *types.Err on
// every call, which cel-go may label with a node ID without affecting other
// readers.
func (x *Value) Val() ref.Val {
	switch {
	case x == nil:
		return types.NullValue
	case x.err != nil:
		return types.WrapErr(x.err)
	case x.v == nil:
		return types.NullValue
	}
	return x.v
}

// Err returns the error of an error value, else nil.
func (x *Value) Err() error {
	if x == nil {
		return nil
	}
	return x.err
}

// IsNull reports JSON null.
func (x *Value) IsNull() bool {
	return x == nil || x.err == nil && (x.v == nil || x.v == types.NullValue)
}

// AppendBody appends the transform body form of the value (03 req 41): a
// top-level string as its UTF-8 bytes, top-level bytes raw, and anything
// else as JSON text: maps with keys in ascending byte order (int, uint and
// bool keys written as strings), decoded numbers verbatim at every level,
// int, uint and double as numbers, nested bytes as base64, timestamps as
// RFC 3339 and durations as proto3 JSON strings. NaN, infinities, invalid
// UTF-8 in a JSON string, duplicate keys and values without a JSON form
// wrap ErrNotJSON; an error value returns its error.
func (x *Value) AppendBody(dst []byte) ([]byte, error) {
	if x != nil && x.isNative {
		if s, ok := x.native.(string); ok {
			return append(dst, s...), nil
		}
		return appendNative(dst, x.native, 0)
	}
	if err := x.Err(); err != nil {
		return dst, err
	}
	switch v := x.Val().(type) {
	case types.String:
		return append(dst, v...), nil
	case types.Bytes:
		return append(dst, v...), nil
	default:
		return appendJSON(dst, v, 0)
	}
}

// Native returns a new tree equal to the value, for code that edits
// documents: map[string]any, []any, string, json.Number, bool or nil. A map
// or list gives the tree that decoding the AppendBody output would give
// (07 req 57); a top-level string or bytes gives its contents as a string.
// The tree shares nothing with the Value.
func (x *Value) Native() (any, error) {
	if x != nil && x.isNative {
		if s, ok := x.native.(string); ok {
			return s, nil
		}
		return cloneNative(x.native, 0)
	}
	if err := x.Err(); err != nil {
		return nil, err
	}
	switch v := x.Val().(type) {
	case types.String:
		return string(v), nil
	case types.Bytes:
		return string(v), nil
	default:
		return toNative(v, 0)
	}
}

// ToVal returns the CEL value of any expr.Value: a *Value directly, another
// implementation through its Native tree. A nil Value is a body that is not
// available: an error wrapping ErrBody. An error is a new *types.Err on
// every call.
func ToVal(v expr.Value) ref.Val {
	switch x := v.(type) {
	case nil:
		return errVal(errBodyUnavailable())
	case *Value:
		return x.Val()
	}
	n, err := v.Native()
	if err != nil {
		return errVal(fmt.Errorf("%w: %w", ErrBody, err))
	}
	return nativeVal(n)
}

// nativeVal returns the CEL view of a JSON tree value.
func nativeVal(n any) ref.Val {
	switch x := n.(type) {
	case nil:
		return types.NullValue
	case string:
		return types.String(x)
	case bool:
		return types.Bool(x)
	case json.Number:
		return numberVal(x)
	case map[string]any:
		if x == nil {
			return types.NullValue
		}
		return mapView[*jsonObject]{&jsonObject{x}}
	case []any:
		if x == nil {
			return types.NullValue
		}
		return listView[*jsonArray]{&jsonArray{x}}
	}
	return errVal(fmt.Errorf("%w: %T in a JSON tree", ErrUnsupported, n))
}

// numberVal is a JSON number as CEL reads it (03 req 25): an integer
// literal within int64 is int, every other number double (an out-of-range
// magnitude is an infinity).
func numberVal(n json.Number) ref.Val {
	s := string(n)
	if !validNumber(s) {
		return errVal(fmt.Errorf("%w: malformed JSON number", ErrUnsupported))
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return types.Int(i)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return errVal(fmt.Errorf("%w: malformed JSON number", ErrUnsupported))
	}
	return types.Double(f)
}

// validNumber reports an RFC 8259 number:
// -? (0 | [1-9][0-9]*) (.[0-9]+)? ([eE][+-]?[0-9]+)?
func validNumber(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && '1' <= s[i] && s[i] <= '9':
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	default:
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		if i >= len(s) || !isDigit(s[i]) {
			return false
		}
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if i >= len(s) || !isDigit(s[i]) {
			return false
		}
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
	return i == len(s)
}

// isDigit reports an ASCII digit.
func isDigit(c byte) bool { return '0' <= c && c <= '9' }
