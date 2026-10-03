// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package httpfield

import (
	"errors"
	"fmt"
)

// Value size limits (07 req 23 and 27 for header values, 07 req 12 and 40
// for query edits; all proposed). Together with a Policy's 32-entry cap
// they bound the bytes one Policy adds to 32 x 8 KiB (07 req 37).
const (
	// MaxValueBytes is the longest field value a Policy may write, literal
	// or computed.
	MaxValueBytes = 8 << 10
	// MaxComputedValueBytes is the limit for a value computed at runtime
	// (07 req 27); it equals MaxValueBytes.
	MaxComputedValueBytes = MaxValueBytes
	// MaxQueryNameBytes is the longest query parameter name a transform
	// may write (07 req 40).
	MaxQueryNameBytes = 256
	// MaxQueryValueBytes is the longest computed query parameter value a
	// transform may write (07 req 12).
	MaxQueryValueBytes = 8 << 10
)

// Errors returned by [CheckValue] and [NormalizeValue]. Each is wrapped
// with the offset of the offending byte, never with the value itself, so a
// computed value taken from a request never reaches a log.
var (
	// ErrValueControl reports a control byte other than HTAB (CR, LF, NUL,
	// 0x01-0x1F without 0x09, and DEL 0x7F).
	ErrValueControl = errors.New("httpfield: field value contains a control byte")
	// ErrValueWhitespace reports leading or trailing SP or HTAB.
	ErrValueWhitespace = errors.New("httpfield: field value has leading or trailing whitespace")
	// ErrValueTooLong reports a value over MaxValueBytes.
	ErrValueTooLong = errors.New("httpfield: field value is too long")
)

// isValueByte reports whether c may appear in a field value: HTAB, SP,
// VCHAR (0x21-0x7E) or obs-text (0x80-0xFF) (RFC 9110 section 5.5).
func isValueByte(c byte) bool {
	return c == '\t' || (c >= ' ' && c != 0x7f)
}

// isOWS reports whether c is optional whitespace: SP or HTAB.
func isOWS(c byte) bool {
	return c == ' ' || c == '\t'
}

// TrimOWS returns v without leading and trailing SP and HTAB (RFC 9110
// section 5.6.3). It never allocates.
func TrimOWS(v string) string {
	i, j := 0, len(v)
	for i < j && isOWS(v[i]) {
		i++
	}
	for j > i && isOWS(v[j-1]) {
		j--
	}
	return v[i:j]
}

// ValidValue reports whether v is a well-formed field value: every byte is
// HTAB, SP, VCHAR or obs-text, and v neither starts nor ends with SP or
// HTAB. The empty value is valid. It does not check the length; see
// [CheckValue].
func ValidValue(v string) bool {
	if v == "" {
		return true
	}
	if isOWS(v[0]) || isOWS(v[len(v)-1]) {
		return false
	}
	for i := range len(v) {
		if !isValueByte(v[i]) {
			return false
		}
	}
	return true
}

// CheckValue reports why v is not a field value a Policy may write (07 req
// 23): nil, or an error wrapping [ErrValueTooLong], [ErrValueControl] or
// [ErrValueWhitespace], checked in that order.
func CheckValue(v string) error {
	if len(v) > MaxValueBytes {
		return fmt.Errorf("%w: %d bytes, the limit is %d", ErrValueTooLong, len(v), MaxValueBytes)
	}
	for i := range len(v) {
		if !isValueByte(v[i]) {
			return fmt.Errorf("%w at offset %d", ErrValueControl, i)
		}
	}
	if v != "" && (isOWS(v[0]) || isOWS(v[len(v)-1])) {
		return ErrValueWhitespace
	}
	return nil
}

// NormalizeValue applies the computed-value rule of 07 req 27: leading and
// trailing SP and HTAB are trimmed, then the value is rejected if it holds
// a control byte other than HTAB or exceeds MaxComputedValueBytes. An empty
// result is valid (it sets an empty field). The returned string shares v's
// bytes.
func NormalizeValue(v string) (string, error) {
	v = TrimOWS(v)
	if len(v) > MaxComputedValueBytes {
		return "", fmt.Errorf("%w: %d bytes, the limit is %d", ErrValueTooLong, len(v), MaxComputedValueBytes)
	}
	for i := range len(v) {
		if !isValueByte(v[i]) {
			return "", fmt.Errorf("%w at offset %d", ErrValueControl, i)
		}
	}
	return v, nil
}
