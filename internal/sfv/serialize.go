// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sfv

import (
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

// tchar (RFC 9110 section 5.6.2) as two 64-bit masks over the ASCII range,
// as in internal/httpfield (this L0 package imports no other Ruralz
// package): bit c of tcharLo is byte c for c < 64, bit c-64 of tcharHi is
// byte c for 64 <= c < 128.
const (
	tcharLo uint64 = 1<<'!' | 1<<'#' | 1<<'$' | 1<<'%' | 1<<'&' | 1<<'\'' |
		1<<'*' | 1<<'+' | 1<<'-' | 1<<'.' | (1<<10-1)<<'0'
	tcharHi uint64 = (1<<26-1)<<('A'-64) | 1<<('^'-64) | 1<<('_'-64) |
		1<<('`'-64) | (1<<26-1)<<('a'-64) | 1<<('|'-64) | 1<<('~'-64)
)

func isTchar(c byte) bool {
	switch {
	case c < 64:
		return tcharLo&(1<<c) != 0
	case c < 128:
		return tcharHi&(1<<(c-64)) != 0
	default:
		return false
	}
}

func isAlpha(c byte) bool { return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') }

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

func isLCAlpha(c byte) bool { return 'a' <= c && c <= 'z' }

// isKeyChar reports whether c may follow the first character of a key:
// lcalpha, DIGIT, "_", "-", "." or "*".
func isKeyChar(c byte) bool {
	return isLCAlpha(c) || isDigit(c) || c == '_' || c == '-' || c == '.' || c == '*'
}

// isTokenChar reports whether c may follow the first character of a
// Token: tchar, ":" or "/".
func isTokenChar(c byte) bool { return isTchar(c) || c == ':' || c == '/' }

// AppendList appends the serialization of l to dst (RFC 9651 section
// 4.1.1): members separated by ", ". An empty List appends nothing; the
// caller omits the field. On failure dst is returned unchanged with an
// error wrapping ErrSerialize.
func AppendList(dst []byte, l List) ([]byte, error) {
	start := len(dst)
	for i := range l {
		if i > 0 {
			dst = append(dst, ", "...)
		}
		var err error
		if dst, err = appendMember(dst, &l[i]); err != nil {
			return dst[:start], err
		}
	}
	return dst, nil
}

// AppendMember appends m to the List serialization in dst, preceded by
// ", " when dst is not empty, so a field value can be built member by
// member without a List. On failure dst is returned unchanged.
func AppendMember(dst []byte, m Member) ([]byte, error) {
	start := len(dst)
	if start > 0 {
		dst = append(dst, ", "...)
	}
	dst, err := appendMember(dst, &m)
	if err != nil {
		return dst[:start], err
	}
	return dst, nil
}

func appendMember(dst []byte, m *Member) ([]byte, error) {
	if !m.Inner {
		return appendItem(dst, m.Value, m.Params)
	}
	// Inner List (RFC 9651 section 4.1.1.1).
	dst = append(dst, '(')
	for i := range m.Items {
		if i > 0 {
			dst = append(dst, ' ')
		}
		var err error
		if dst, err = appendItem(dst, m.Items[i].Value, m.Items[i].Params); err != nil {
			return dst, err
		}
	}
	dst = append(dst, ')')
	return AppendParams(dst, m.Params)
}

// AppendItem appends the serialization of it to dst (RFC 9651 section
// 4.1.3). On failure dst is returned unchanged.
func AppendItem(dst []byte, it Item) ([]byte, error) {
	start := len(dst)
	dst, err := appendItem(dst, it.Value, it.Params)
	if err != nil {
		return dst[:start], err
	}
	return dst, nil
}

func appendItem(dst []byte, v BareItem, params []Param) ([]byte, error) {
	dst, err := AppendBareItem(dst, v)
	if err != nil {
		return dst, err
	}
	return AppendParams(dst, params)
}

// AppendParams appends the serialization of params to dst (RFC 9651
// section 4.1.1.2): ";key" for a Boolean true value, else ";key=value".
// Duplicate keys fail, since the result would not parse back to params. On
// failure dst is returned unchanged.
func AppendParams(dst []byte, params []Param) ([]byte, error) {
	start := len(dst)
	for i := range params {
		p := &params[i]
		for j := range i {
			if params[j].Key == p.Key {
				return dst[:start], fmt.Errorf("%w: duplicate parameter key %q", ErrSerialize, p.Key)
			}
		}
		dst = append(dst, ';')
		var err error
		if dst, err = AppendKey(dst, p.Key); err != nil {
			return dst[:start], err
		}
		if p.Value.kind == KindBoolean && p.Value.num == 1 {
			continue
		}
		dst = append(dst, '=')
		if dst, err = AppendBareItem(dst, p.Value); err != nil {
			return dst[:start], err
		}
	}
	return dst, nil
}

// AppendKey appends a parameter or dictionary key (RFC 9651 section
// 4.1.1.3): lcalpha or "*", then lcalpha, DIGIT, "_", "-", "." or "*". On
// failure dst is returned unchanged.
func AppendKey(dst []byte, key string) ([]byte, error) {
	if key == "" || (!isLCAlpha(key[0]) && key[0] != '*') {
		return dst, fmt.Errorf("%w: key %q must start with a lowercase letter or \"*\"", ErrSerialize, key)
	}
	for i := 1; i < len(key); i++ {
		if !isKeyChar(key[i]) {
			return dst, fmt.Errorf("%w: key %q has byte %q at offset %d", ErrSerialize, key, key[i], i)
		}
	}
	return append(dst, key...), nil
}

// AppendBareItem appends the serialization of v to dst (RFC 9651 section
// 4.1.3.1). On failure dst is returned unchanged.
func AppendBareItem(dst []byte, v BareItem) ([]byte, error) {
	switch v.kind {
	case KindInteger:
		return appendInteger(dst, v.num)
	case KindDecimal:
		return appendDecimal(dst, v.dec)
	case KindString:
		return appendString(dst, v.str)
	case KindToken:
		return appendToken(dst, v.str)
	case KindByteSequence:
		dst = append(dst, ':')
		dst = base64.StdEncoding.AppendEncode(dst, []byte(v.str))
		return append(dst, ':'), nil
	case KindBoolean:
		if v.num == 1 {
			return append(dst, "?1"...), nil
		}
		return append(dst, "?0"...), nil
	case KindDate:
		start := len(dst)
		dst, err := appendInteger(append(dst, '@'), v.num)
		if err != nil {
			return dst[:start], err
		}
		return dst, nil
	case KindDisplayString:
		return appendDisplayString(dst, v.str)
	default:
		return dst, fmt.Errorf("%w: bare item of %v", ErrSerialize, v.kind)
	}
}

// appendInteger serializes an Integer (RFC 9651 section 4.1.4).
func appendInteger(dst []byte, n int64) ([]byte, error) {
	if n < MinInteger || n > MaxInteger {
		return dst, fmt.Errorf("%w: integer %d out of range", ErrSerialize, n)
	}
	return strconv.AppendInt(dst, n, 10), nil
}

// appendDecimal serializes a Decimal (RFC 9651 section 4.1.5): rounded to
// three fractional digits, half to even (strconv rounds the exact binary
// value correctly, so a decimal tie float64 cannot hold rounds by its
// binary value; see Decimal), at most 12 integer digits, trailing
// fractional zeros removed but one digit kept. A value that rounds to zero
// is written without a sign.
func appendDecimal(dst []byte, f float64) ([]byte, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return dst, fmt.Errorf("%w: decimal %v is not finite", ErrSerialize, f)
	}
	start := len(dst)
	dst = strconv.AppendFloat(dst, f, 'f', 3, 64)
	digits := dst[start:]
	if digits[0] == '-' {
		digits = digits[1:]
	}
	if len(digits)-4 > 12 { // digits is "<integer>.ddd"
		return dst[:start], fmt.Errorf("%w: decimal %v has more than 12 integer digits", ErrSerialize, f)
	}
	for dst[len(dst)-1] == '0' && dst[len(dst)-2] != '.' {
		dst = dst[:len(dst)-1]
	}
	if dst[start] == '-' && string(dst[start+1:]) == "0.0" {
		dst = append(dst[:start], "0.0"...)
	}
	return dst, nil
}

// appendString serializes a String (RFC 9651 section 4.1.6): printable
// ASCII only, with "\" and DQUOTE escaped.
func appendString(dst []byte, s string) ([]byte, error) {
	start := len(dst)
	dst = append(dst, '"')
	for i := range len(s) {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			return dst[:start], fmt.Errorf("%w: string has byte %q at offset %d", ErrSerialize, c, i)
		}
		if c == '"' || c == '\\' {
			dst = append(dst, '\\')
		}
		dst = append(dst, c)
	}
	return append(dst, '"'), nil
}

// appendToken serializes a Token (RFC 9651 section 4.1.7).
func appendToken(dst []byte, s string) ([]byte, error) {
	if s == "" || (!isAlpha(s[0]) && s[0] != '*') {
		return dst, fmt.Errorf("%w: token %q must start with a letter or \"*\"", ErrSerialize, s)
	}
	for i := 1; i < len(s); i++ {
		if !isTokenChar(s[i]) {
			return dst, fmt.Errorf("%w: token %q has byte %q at offset %d", ErrSerialize, s, s[i], i)
		}
	}
	return append(dst, s...), nil
}

// appendDisplayString serializes a Display String (RFC 9651 section
// 4.1.11): "%" DQUOTE, then each UTF-8 byte, percent-encoded in lowercase
// hex when it is "%", DQUOTE, a control byte or not ASCII, then DQUOTE.
func appendDisplayString(dst []byte, s string) ([]byte, error) {
	if !utf8.ValidString(s) {
		return dst, fmt.Errorf("%w: display string is not valid UTF-8", ErrSerialize)
	}
	const hex = "0123456789abcdef"
	dst = append(dst, '%', '"')
	for i := range len(s) {
		c := s[i]
		if c == '%' || c == '"' || c < 0x20 || c > 0x7e {
			dst = append(dst, '%', hex[c>>4], hex[c&0xf])
			continue
		}
		dst = append(dst, c)
	}
	return append(dst, '"'), nil
}
