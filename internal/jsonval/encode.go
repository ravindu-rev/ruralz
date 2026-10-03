// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Order selects the order in which the encoder writes object members.
type Order uint8

// Member orders.
const (
	// OrderBytes writes members by ascending byte order of their names,
	// whatever their order in the tree (07 req 60).
	OrderBytes Order = iota
	// OrderInsertion writes *Object members in their order; map[string]any
	// members, which have none, fall back to OrderBytes.
	OrderInsertion
	// OrderUTF16 writes members by ascending UTF-16 code units of their
	// names, the RFC 8785 order (02 req 24).
	OrderUTF16
)

// NumberForm selects how the encoder writes numbers.
type NumberForm uint8

// Number forms.
const (
	// NumbersVerbatim writes a json.Number as its literal (checked against
	// the grammar), Go integers in decimal and Go floats with AppendFloat
	// (07 req 60).
	NumbersVerbatim NumberForm = iota
	// NumbersCanonical writes every number in the RFC 8785 form: literals
	// through AppendCanonicalNumber, Go integers within ±MaxSafeInteger in
	// decimal (others are ErrNumberRange, since a double would round them)
	// and Go floats with AppendFloat (02 req 16, 24).
	NumbersCanonical
)

// EncodeOptions configures an Encoder. The zero value is the transform
// form of 07 req 60.
type EncodeOptions struct {
	// Order is the object member order.
	Order Order
	// Numbers is the number form.
	Numbers NumberForm
	// MaxBytes, when positive, limits the bytes one Append call adds:
	// encoding stops with ErrOutputTooLarge as soon as the output would
	// pass it, so an output cap (07 req 70; 05 req 47) is enforced without
	// building the oversized body first. Strings, member names and number
	// literals are refused before they are copied, escapes are checked as
	// they are written, and other tokens add at most a few dozen bytes, so
	// a refused Append leaves at most MaxBytes plus that much in dst.
	MaxBytes int
}

// maxEncodeDepth bounds encoder recursion, so a cyclic tree fails with
// ErrDepth instead of exhausting the stack.
const maxEncodeDepth = MaxDepthLimit

// Encoder writes tree values as compact JSON: no insignificant whitespace,
// no trailing newline, minimal string escaping (only '"', '\\' and U+0000 to
// U+001F; no HTML escaping, so '<', '>', '&', U+2028 and U+2029 stay
// literal) and deterministic member order. It accepts the tree types plus Go
// integers and floats. A nil *Object, map or slice is written as null. An
// Encoder reuses its sort scratch between calls, so after warm-up it
// allocates nothing beyond the growth of dst; an *Object already in the
// output order is not sorted at all. Each Append drops sort scratch over
// about 64 KiB, so a pooled Encoder never pins scratch sized by the largest
// object it wrote (07 req 73).
type Encoder struct {
	// Options selects member order, number form and output limit.
	Options EncodeOptions
	idx     []int
	keys    []string
	limit   int
}

// Append appends v in the transform form (OrderBytes, NumbersVerbatim; 07
// req 60).
func Append(dst []byte, v any) ([]byte, error) {
	var e Encoder
	return e.Append(dst, v)
}

// AppendCanonical appends v in the RFC 8785 form (OrderUTF16,
// NumbersCanonical; 02 req 24, 25).
func AppendCanonical(dst []byte, v any) ([]byte, error) {
	e := Encoder{Options: EncodeOptions{Order: OrderUTF16, Numbers: NumbersCanonical}}
	return e.Append(dst, v)
}

// Append appends v to dst. On error dst holds a partial value.
func (e *Encoder) Append(dst []byte, v any) ([]byte, error) {
	e.limit = math.MaxInt
	if e.Options.MaxBytes > 0 && len(dst) <= math.MaxInt-e.Options.MaxBytes {
		e.limit = len(dst) + e.Options.MaxBytes
	}
	dst, err := e.value(dst, v, 0)
	e.release()
	return dst, err
}

// Encoder scratch bounds (07 req 73): position and key entries kept for
// reuse, about maxScratch bytes each.
const (
	maxScratchIdx  = maxScratch / 8
	maxScratchKeys = maxScratch / 16
)

// release drops sort scratch over the bounds. Both stacks are empty
// between calls, and keys holds no strings.
func (e *Encoder) release() {
	if cap(e.idx) > maxScratchIdx {
		e.idx = nil
	}
	if cap(e.keys) > maxScratchKeys {
		e.keys = nil
	}
}

// value appends one value and checks the output limit.
func (e *Encoder) value(dst []byte, v any, depth int) ([]byte, error) {
	dst, err := e.appendValue(dst, v, depth)
	if err == nil && len(dst) > e.limit {
		return dst, ErrOutputTooLarge
	}
	return dst, err
}

// appendValue appends one value.
func (e *Encoder) appendValue(dst []byte, v any, depth int) ([]byte, error) {
	if isNull(v) {
		return append(dst, "null"...), nil
	}
	switch x := v.(type) {
	case bool:
		if x {
			return append(dst, "true"...), nil
		}
		return append(dst, "false"...), nil
	case string:
		// Refuse a string that cannot fit, quotes included, before copying
		// it; appendString checks escape growth as it goes.
		if len(x)+2 > e.limit-len(dst) {
			return dst, ErrOutputTooLarge
		}
		return appendString(dst, x, e.limit)
	case json.Number:
		return e.number(dst, x)
	case []any:
		return e.array(dst, x, depth)
	case *Object:
		return e.object(dst, x, depth)
	case map[string]any:
		return e.mapObject(dst, x, depth)
	case float64:
		return AppendFloat(dst, x)
	case float32:
		// The shortest float32 digits, read back as a double.
		f, _ := strconv.ParseFloat(strconv.FormatFloat(float64(x), 'g', -1, 32), 64)
		return AppendFloat(dst, f)
	case int:
		return e.int(dst, int64(x))
	case int8:
		return e.int(dst, int64(x))
	case int16:
		return e.int(dst, int64(x))
	case int32:
		return e.int(dst, int64(x))
	case int64:
		return e.int(dst, x)
	case uint:
		return e.uint(dst, uint64(x))
	case uint8:
		return e.uint(dst, uint64(x))
	case uint16:
		return e.uint(dst, uint64(x))
	case uint32:
		return e.uint(dst, uint64(x))
	case uint64:
		return e.uint(dst, x)
	default:
		return dst, fmt.Errorf("%w: %T", ErrUnsupportedType, v)
	}
}

// number appends a json.Number.
func (e *Encoder) number(dst []byte, n json.Number) ([]byte, error) {
	if e.Options.Numbers == NumbersCanonical {
		// The canonical form of any literal is at most 25 bytes.
		return AppendCanonicalNumber(dst, n)
	}
	// Refuse a literal that cannot fit before checking or copying it.
	if len(n) > e.limit-len(dst) {
		return dst, ErrOutputTooLarge
	}
	if !ValidNumber(string(n)) {
		return dst, ErrInvalidNumber
	}
	return append(dst, n...), nil
}

// int appends a Go signed integer.
func (e *Encoder) int(dst []byte, n int64) ([]byte, error) {
	if e.Options.Numbers == NumbersCanonical && (n > MaxSafeInteger || n < -MaxSafeInteger) {
		return dst, ErrNumberRange
	}
	return strconv.AppendInt(dst, n, 10), nil
}

// uint appends a Go unsigned integer.
func (e *Encoder) uint(dst []byte, n uint64) ([]byte, error) {
	if e.Options.Numbers == NumbersCanonical && n > MaxSafeInteger {
		return dst, ErrNumberRange
	}
	return strconv.AppendUint(dst, n, 10), nil
}

// array appends an array.
func (e *Encoder) array(dst []byte, a []any, depth int) ([]byte, error) {
	if depth >= maxEncodeDepth {
		return dst, ErrDepth
	}
	dst = append(dst, '[')
	for i, v := range a {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		if dst, err = e.value(dst, v, depth+1); err != nil {
			return dst, err
		}
	}
	return append(dst, ']'), nil
}

// compare returns the member name comparison of the options.
func (e *Encoder) compare() func(a, b string) int {
	if e.Options.Order == OrderUTF16 {
		return CompareUTF16
	}
	return strings.Compare
}

// object appends an *Object.
func (e *Encoder) object(dst []byte, o *Object, depth int) ([]byte, error) {
	if depth >= maxEncodeDepth {
		return dst, ErrDepth
	}
	ms := o.members
	dst = append(dst, '{')
	cmpName := e.compare()
	if e.Options.Order == OrderInsertion || slices.IsSortedFunc(ms, func(a, b Member) int { return cmpName(a.Name, b.Name) }) {
		for i := range ms {
			var err error
			if dst, err = e.member(dst, i, ms[i].Name, ms[i].Value, depth); err != nil {
				return dst, err
			}
		}
		return append(dst, '}'), nil
	}
	// Sort member positions on the shared scratch stack; nested objects
	// push above this frame, so read it by offset.
	base := len(e.idx)
	for i := range ms {
		e.idx = append(e.idx, i)
	}
	slices.SortFunc(e.idx[base:], func(a, b int) int { return cmpName(ms[a].Name, ms[b].Name) })
	for k := range ms {
		m := ms[e.idx[base+k]]
		var err error
		if dst, err = e.member(dst, k, m.Name, m.Value, depth); err != nil {
			e.idx = e.idx[:base]
			return dst, err
		}
	}
	e.idx = e.idx[:base]
	return append(dst, '}'), nil
}

// mapObject appends a map[string]any.
func (e *Encoder) mapObject(dst []byte, m map[string]any, depth int) ([]byte, error) {
	if depth >= maxEncodeDepth {
		return dst, ErrDepth
	}
	base := len(e.keys)
	for k := range m {
		e.keys = append(e.keys, k)
	}
	slices.SortFunc(e.keys[base:], e.compare())
	dst = append(dst, '{')
	for k := range len(m) {
		name := e.keys[base+k]
		var err error
		if dst, err = e.member(dst, k, name, m[name], depth); err != nil {
			clear(e.keys[base:])
			e.keys = e.keys[:base]
			return dst, err
		}
	}
	clear(e.keys[base:])
	e.keys = e.keys[:base]
	return append(dst, '}'), nil
}

// member appends one member, preceded by a comma unless it is the first.
func (e *Encoder) member(dst []byte, k int, name string, v any, depth int) ([]byte, error) {
	if k > 0 {
		dst = append(dst, ',')
	}
	// Refuse a name that cannot fit with its quotes and colon before
	// copying it.
	if len(name)+3 > e.limit-len(dst) {
		return dst, ErrOutputTooLarge
	}
	dst, err := appendString(dst, name, e.limit)
	if err != nil {
		return dst, err
	}
	dst = append(dst, ':')
	return e.value(dst, v, depth+1)
}

// hexDigits are the lowercase hex digits of \u00xx escapes.
const hexDigits = "0123456789abcdef"

// AppendString appends s as a JSON string with minimal escaping: '"' and
// '\\' are escaped, U+0008, U+0009, U+000A, U+000C and U+000D use their
// short escapes, other characters below U+0020 use \u00xx with lowercase
// hex, and everything else, including '/', U+007F, U+2028 and U+2029, is
// written as it is. This is both the 07 req 60 and the RFC 8785 string
// form. Invalid UTF-8 in s is ErrInvalidUTF8.
func AppendString(dst []byte, s string) ([]byte, error) {
	return appendString(dst, s, math.MaxInt)
}

// appendString is AppendString with an output limit: it fails with
// ErrOutputTooLarge, without writing past limit, when the string would end
// beyond it.
func appendString(dst []byte, s string, limit int) ([]byte, error) {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				return dst, ErrInvalidUTF8
			}
			i += size
			continue
		}
		if c >= 0x20 && c != '"' && c != '\\' {
			i++
			continue
		}
		short := byte(0)
		switch c {
		case '"', '\\':
			short = c
		case '\b':
			short = 'b'
		case '\t':
			short = 't'
		case '\n':
			short = 'n'
		case '\f':
			short = 'f'
		case '\r':
			short = 'r'
		}
		w := 6
		if short != 0 {
			w = 2
		}
		// Escapes grow the output up to six times the input; stop before
		// the run and this escape would pass the limit.
		if len(dst)+(i-start)+w > limit {
			return dst, ErrOutputTooLarge
		}
		dst = append(dst, s[start:i]...)
		if short != 0 {
			dst = append(dst, '\\', short)
		} else {
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
		}
		i++
		start = i
	}
	if len(dst)+(len(s)-start)+1 > limit {
		return dst, ErrOutputTooLarge
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"'), nil
}
