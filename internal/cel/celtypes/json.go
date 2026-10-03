// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

// maxDepth bounds the nesting AppendBody and Native walk (the jsonval
// decoder's own ceiling); CEL results and decoded bodies stay far below it.
const maxDepth = 10_000

// errDepth is the error of a value nested deeper than maxDepth.
func errDepth() error { return fmt.Errorf("%w: nested deeper than %d", ErrNotJSON, maxDepth) }

// appendJSON appends the JSON text of v at nesting depth depth.
func appendJSON(dst []byte, v ref.Val, depth int) ([]byte, error) {
	if depth > maxDepth {
		return dst, errDepth()
	}
	switch x := v.(type) {
	case types.Null:
		return append(dst, "null"...), nil
	case types.Bool:
		return strconv.AppendBool(dst, bool(x)), nil
	case types.Int:
		return strconv.AppendInt(dst, int64(x), 10), nil
	case types.Uint:
		return strconv.AppendUint(dst, uint64(x), 10), nil
	case types.Double:
		return appendDouble(dst, float64(x))
	case types.String:
		return appendString(dst, string(x))
	case types.Bytes:
		return appendBase64(dst, x), nil
	case types.Timestamp:
		dst = append(dst, '"')
		dst = appendTimestamp(dst, x.Time)
		return append(dst, '"'), nil
	case types.Duration:
		dst = append(dst, '"')
		dst = appendDuration(dst, x.Duration)
		return append(dst, '"'), nil
	case mapView[*jsonObject]:
		return appendNativeMap(dst, x.s.tree(), depth)
	case listView[*jsonArray]:
		return appendNativeList(dst, x.s.l, depth)
	case traits.Mapper:
		return appendMap(dst, x, depth)
	case traits.Lister:
		return appendList(dst, x, depth)
	case *types.Err:
		return dst, x
	case nil:
		return dst, fmt.Errorf("%w: missing value", ErrNotJSON)
	}
	return dst, fmt.Errorf("%w: %s", ErrNotJSON, v.Type().TypeName())
}

// mapEntry is one map key and its JSON member name.
type mapEntry struct {
	name string
	key  ref.Val
}

// sortedEntries returns the keys of m with their member names in ascending
// byte order; two keys with one name wrap ErrNotJSON.
func sortedEntries(m traits.Mapper) ([]mapEntry, error) {
	var entries []mapEntry
	it := m.Iterator()
	for it.HasNext() == types.True {
		k := it.Next()
		name, err := keyName(k)
		if err != nil {
			return nil, err
		}
		entries = append(entries, mapEntry{name, k})
	}
	slices.SortFunc(entries, func(a, b mapEntry) int { return strings.Compare(a.name, b.name) })
	for i := 1; i < len(entries); i++ {
		if entries[i].name == entries[i-1].name {
			return nil, fmt.Errorf("%w: two map keys write the same member name", ErrNotJSON)
		}
	}
	return entries, nil
}

// keyName is the JSON member name of a map key: a string as it is, an int,
// uint or bool in its CEL text form (the proto3 JSON map key rule).
func keyName(k ref.Val) (string, error) {
	switch x := k.(type) {
	case types.String:
		return string(x), nil
	case types.Int:
		return strconv.FormatInt(int64(x), 10), nil
	case types.Uint:
		return strconv.FormatUint(uint64(x), 10), nil
	case types.Bool:
		return strconv.FormatBool(bool(x)), nil
	case *types.Err:
		return "", x
	}
	return "", fmt.Errorf("%w: map key of type %s", ErrNotJSON, k.Type().TypeName())
}

// appendMap appends a CEL map as a JSON object with sorted member names.
func appendMap(dst []byte, m traits.Mapper, depth int) ([]byte, error) {
	entries, err := sortedEntries(m)
	if err != nil {
		return dst, err
	}
	dst = append(dst, '{')
	for i, e := range entries {
		if i > 0 {
			dst = append(dst, ',')
		}
		if dst, err = appendString(dst, e.name); err != nil {
			return dst, err
		}
		dst = append(dst, ':')
		if dst, err = appendJSON(dst, m.Get(e.key), depth+1); err != nil {
			return dst, err
		}
	}
	return append(dst, '}'), nil
}

// appendList appends a CEL list as a JSON array.
func appendList(dst []byte, l traits.Lister, depth int) ([]byte, error) {
	n := int(l.Size().(types.Int))
	dst = append(dst, '[')
	for i := range n {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		if dst, err = appendJSON(dst, l.Get(types.Int(i)), depth+1); err != nil {
			return dst, err
		}
	}
	return append(dst, ']'), nil
}

// appendNative appends a JSON tree value; numbers are written verbatim.
func appendNative(dst []byte, n any, depth int) ([]byte, error) {
	if depth > maxDepth {
		return dst, errDepth()
	}
	switch x := n.(type) {
	case nil:
		return append(dst, "null"...), nil
	case bool:
		return strconv.AppendBool(dst, x), nil
	case string:
		return appendString(dst, x)
	case json.Number:
		if !validNumber(string(x)) {
			return dst, fmt.Errorf("%w: malformed JSON number", ErrUnsupported)
		}
		return append(dst, x...), nil
	case map[string]any:
		if x == nil {
			return append(dst, "null"...), nil
		}
		return appendNativeMap(dst, x, depth)
	case []any:
		if x == nil {
			return append(dst, "null"...), nil
		}
		return appendNativeList(dst, x, depth)
	}
	return dst, fmt.Errorf("%w: %T in a JSON tree", ErrUnsupported, n)
}

// appendNativeMap appends a JSON object with members in ascending byte
// order of their names.
func appendNativeMap(dst []byte, m map[string]any, depth int) ([]byte, error) {
	if depth > maxDepth {
		return dst, errDepth()
	}
	dst = append(dst, '{')
	for i, k := range sortedMapKeys(m) {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		if dst, err = appendString(dst, k); err != nil {
			return dst, err
		}
		dst = append(dst, ':')
		if dst, err = appendNative(dst, m[k], depth+1); err != nil {
			return dst, err
		}
	}
	return append(dst, '}'), nil
}

// appendNativeList appends a JSON array.
func appendNativeList(dst []byte, l []any, depth int) ([]byte, error) {
	if depth > maxDepth {
		return dst, errDepth()
	}
	dst = append(dst, '[')
	for i, e := range l {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		if dst, err = appendNative(dst, e, depth+1); err != nil {
			return dst, err
		}
	}
	return append(dst, ']'), nil
}

// hexDigits are the lowercase hex digits of \u00xx escapes.
const hexDigits = "0123456789abcdef"

// appendString appends s as a JSON string with the escaping of
// jsonval.AppendString (07 req 60): '"' and '\\' escaped, the short escapes
// for U+0008, U+0009, U+000A, U+000C and U+000D, \u00xx for the other
// control characters, everything else as it is. Invalid UTF-8 wraps
// ErrNotJSON.
func appendString(dst []byte, s string) ([]byte, error) {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				return dst, fmt.Errorf("%w: invalid UTF-8 in a string", ErrNotJSON)
			}
			i += size
			continue
		}
		if c >= 0x20 && c != '"' && c != '\\' {
			i++
			continue
		}
		dst = append(dst, s[start:i]...)
		switch c {
		case '"', '\\':
			dst = append(dst, '\\', c)
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\r':
			dst = append(dst, '\\', 'r')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
		}
		i++
		start = i
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"'), nil
}

// appendBase64 appends b as a JSON string of its standard base64 form (the
// proto3 JSON bytes form).
func appendBase64(dst, b []byte) []byte {
	dst = append(dst, '"')
	dst = base64.StdEncoding.AppendEncode(dst, b)
	return append(dst, '"')
}

// appendDouble appends f as a JSON number in the form encoding/json writes:
// shortest round-trip digits, exponent notation below 1e-6 and from 1e21.
// NaN and infinities wrap ErrNotJSON.
func appendDouble(dst []byte, f float64) ([]byte, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return dst, fmt.Errorf("%w: %v is not a JSON number", ErrNotJSON, f)
	}
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	dst = strconv.AppendFloat(dst, f, format, -1, 64)
	if format == 'e' {
		// Shorten e-09 to e-9, as encoding/json does.
		if n := len(dst); n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst, nil
}

// appendTimestamp appends t in RFC 3339 form, UTC ("Z"), with the shortest
// fraction: what CEL's string(timestamp) gives for a UTC time.
func appendTimestamp(dst []byte, t time.Time) []byte {
	return t.UTC().AppendFormat(dst, time.RFC3339Nano)
}

// appendDuration appends d in proto3 JSON form: seconds with the shortest
// exact fraction and the suffix "s" ("1.5s", "-0.000000001s").
func appendDuration(dst []byte, d time.Duration) []byte {
	// Truncating division keeps both parts within int64, even for
	// math.MinInt64, so negating them cannot overflow.
	secs, frac := int64(d)/1e9, int64(d)%1e9
	if d < 0 {
		dst = append(dst, '-')
		secs, frac = -secs, -frac
	}
	dst = strconv.AppendInt(dst, secs, 10)
	if frac != 0 {
		var digits [9]byte
		for i := 8; i >= 0; i-- {
			digits[i] = byte('0' + frac%10)
			frac /= 10
		}
		n := 9
		for digits[n-1] == '0' {
			n--
		}
		dst = append(dst, '.')
		dst = append(dst, digits[:n]...)
	}
	return append(dst, 's')
}

// toNative returns a new JSON tree equal to the JSON text appendJSON
// writes for v.
func toNative(v ref.Val, depth int) (any, error) {
	if depth > maxDepth {
		return nil, errDepth()
	}
	switch x := v.(type) {
	case types.Null:
		return nil, nil
	case types.Bool:
		return bool(x), nil
	case types.Int:
		return json.Number(strconv.FormatInt(int64(x), 10)), nil
	case types.Uint:
		return json.Number(strconv.FormatUint(uint64(x), 10)), nil
	case types.Double:
		b, err := appendDouble(nil, float64(x))
		if err != nil {
			return nil, err
		}
		return json.Number(b), nil
	case types.String:
		if !utf8.ValidString(string(x)) {
			return nil, fmt.Errorf("%w: invalid UTF-8 in a string", ErrNotJSON)
		}
		return string(x), nil
	case types.Bytes:
		return base64.StdEncoding.EncodeToString(x), nil
	case types.Timestamp:
		return string(appendTimestamp(nil, x.Time)), nil
	case types.Duration:
		return string(appendDuration(nil, x.Duration)), nil
	case mapView[*jsonObject]:
		if x.s.tree() == nil {
			return map[string]any{}, nil
		}
		return cloneNative(x.s.m, depth)
	case listView[*jsonArray]:
		return cloneNative(x.s.l, depth)
	case traits.Mapper:
		return mapToTree(x, depth)
	case traits.Lister:
		n := int(x.Size().(types.Int))
		out := make([]any, n)
		for i := range n {
			e, err := toNative(x.Get(types.Int(i)), depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = e
		}
		return out, nil
	case *types.Err:
		return nil, x
	case nil:
		return nil, fmt.Errorf("%w: missing value", ErrNotJSON)
	}
	return nil, fmt.Errorf("%w: %s", ErrNotJSON, v.Type().TypeName())
}

// mapToTree returns a CEL map as map[string]any.
func mapToTree(m traits.Mapper, depth int) (any, error) {
	entries, err := sortedEntries(m)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(entries))
	for _, e := range entries {
		v, err := toNative(m.Get(e.key), depth+1)
		if err != nil {
			return nil, err
		}
		out[e.name] = v
	}
	return out, nil
}

// cloneNative deep-copies a JSON tree value, checking it as appendNative
// does.
func cloneNative(n any, depth int) (any, error) {
	if depth > maxDepth {
		return nil, errDepth()
	}
	switch x := n.(type) {
	case nil, bool:
		return x, nil
	case string:
		if !utf8.ValidString(x) {
			return nil, fmt.Errorf("%w: invalid UTF-8 in a string", ErrNotJSON)
		}
		return x, nil
	case json.Number:
		if !validNumber(string(x)) {
			return nil, fmt.Errorf("%w: malformed JSON number", ErrUnsupported)
		}
		return x, nil
	case map[string]any:
		if x == nil {
			return nil, nil
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			if !utf8.ValidString(k) {
				return nil, fmt.Errorf("%w: invalid UTF-8 in a member name", ErrNotJSON)
			}
			c, err := cloneNative(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = c
		}
		return out, nil
	case []any:
		if x == nil {
			return nil, nil
		}
		out := make([]any, len(x))
		for i, e := range x {
			c, err := cloneNative(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: %T in a JSON tree", ErrUnsupported, n)
}
