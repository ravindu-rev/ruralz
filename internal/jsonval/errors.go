// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"errors"
	"strconv"
)

// Sentinel errors. Scan and decode failures are *[Error] values that match
// these with errors.Is.
var (
	// ErrSyntax reports input that is not one RFC 8259 JSON text. Invalid
	// UTF-8 and unpaired surrogate escapes match it too.
	ErrSyntax = errors.New("jsonval: invalid JSON")
	// ErrInvalidUTF8 reports input or a string that is not valid UTF-8.
	ErrInvalidUTF8 = errors.New("jsonval: invalid UTF-8")
	// ErrSurrogate reports a \u escape of an unpaired UTF-16 surrogate.
	ErrSurrogate = errors.New("jsonval: unpaired surrogate escape")
	// ErrDuplicateName reports an object with two members of the same name.
	ErrDuplicateName = errors.New("jsonval: duplicate object name")
	// ErrDepth reports nesting deeper than the configured limit.
	ErrDepth = errors.New("jsonval: nesting too deep")
	// ErrTooLarge reports a decoded value over its cost budget.
	ErrTooLarge = errors.New("jsonval: decoded value over its limit")
	// ErrInvalidNumber reports a number literal outside the RFC 8259
	// grammar.
	ErrInvalidNumber = errors.New("jsonval: invalid number literal")
	// ErrNumberRange reports a number the output form cannot represent:
	// NaN, an infinity, a literal that overflows a double, or, in the RFC
	// 8785 form, a Go integer outside ±(2^53−1). CheckNumber also returns it
	// for an integer literal outside that range (02 req 16).
	ErrNumberRange = errors.New("jsonval: number out of range")
	// ErrUnsupportedType reports a Go value the encoder cannot write.
	ErrUnsupportedType = errors.New("jsonval: unsupported value type")
	// ErrOutputTooLarge reports encoder output over EncodeOptions.MaxBytes.
	ErrOutputTooLarge = errors.New("jsonval: encoded value over its limit")
)

// Error is a scan or decode failure at a byte offset of the input. Msg is a
// fixed description; it never quotes input bytes, so errors are safe to log
// (07 req 87).
type Error struct {
	// Kind is one of the package sentinels: ErrSyntax, ErrInvalidUTF8,
	// ErrSurrogate, ErrDuplicateName, ErrDepth or ErrTooLarge.
	Kind error
	// Msg describes the failure without quoting input.
	Msg string
	// Offset is the byte offset where the failure starts.
	Offset int
	// Other is the offset of the first member for ErrDuplicateName and -1
	// otherwise.
	Other int
}

// Error returns "jsonval: <message> at offset <n>".
func (e *Error) Error() string {
	b := make([]byte, 0, 64)
	b = append(b, "jsonval: "...)
	b = append(b, e.Msg...)
	b = append(b, " at offset "...)
	b = strconv.AppendInt(b, int64(e.Offset), 10)
	if e.Other >= 0 {
		b = append(b, " (first at offset "...)
		b = strconv.AppendInt(b, int64(e.Other), 10)
		b = append(b, ')')
	}
	return string(b)
}

// Unwrap returns Kind, plus ErrSyntax when Kind is ErrInvalidUTF8 or
// ErrSurrogate.
func (e *Error) Unwrap() []error {
	if errors.Is(e.Kind, ErrInvalidUTF8) || errors.Is(e.Kind, ErrSurrogate) {
		return []error{e.Kind, ErrSyntax}
	}
	return []error{e.Kind}
}

// Fixed error messages.
const (
	msgEOF          = "unexpected end of input"
	msgValueStart   = "invalid character at start of value"
	msgAfterTop     = "invalid character after top-level value"
	msgLiteral      = "invalid literal"
	msgNumber       = "invalid number"
	msgControl      = "control character in string"
	msgEscape       = "invalid escape sequence"
	msgUTF8         = "invalid UTF-8"
	msgSurrogate    = "unpaired surrogate escape"
	msgColon        = "expected ':' after object member name"
	msgName         = "expected object member name"
	msgAfterMember  = "expected ',' or '}' after object member"
	msgAfterElement = "expected ',' or ']' after array element"
	msgDepth        = "nesting too deep"
	msgDuplicate    = "duplicate object member name"
	msgTooLarge     = "decoded value over its limit"
)
