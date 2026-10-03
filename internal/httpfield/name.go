// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package httpfield

import (
	"errors"
	"fmt"
)

// MaxNameBytes is the longest field name a Policy may write, in bytes (07
// req 21, proposed).
const MaxNameBytes = 256

// tchar (RFC 9110 section 5.6.2) as two 64-bit masks over the ASCII range:
// "!" / "#" / "$" / "%" / "&" / "'" / "*" / "+" / "-" / "." / "^" / "_" /
// "`" / "|" / "~" / DIGIT / ALPHA. Bit c of tcharLo is byte c for c < 64,
// bit c-64 of tcharHi is byte c for 64 <= c < 128.
const (
	tcharLo uint64 = 1<<'!' | 1<<'#' | 1<<'$' | 1<<'%' | 1<<'&' | 1<<'\'' |
		1<<'*' | 1<<'+' | 1<<'-' | 1<<'.' | (1<<10-1)<<'0'
	tcharHi uint64 = (1<<26-1)<<('A'-64) | 1<<('^'-64) | 1<<('_'-64) |
		1<<('`'-64) | (1<<26-1)<<('a'-64) | 1<<('|'-64) | 1<<('~'-64)
)

// Errors returned by [CheckName]. Each is wrapped with the offending
// detail; match them with errors.Is.
var (
	// ErrNameEmpty reports an empty field name.
	ErrNameEmpty = errors.New("httpfield: field name is empty")
	// ErrNameTooLong reports a field name over MaxNameBytes.
	ErrNameTooLong = errors.New("httpfield: field name is too long")
	// ErrNameChar reports a byte that is not an RFC 9110 tchar.
	ErrNameChar = errors.New("httpfield: field name is not an RFC 9110 token")
)

// IsTokenChar reports whether c is an RFC 9110 tchar: ALPHA, DIGIT or one
// of !#$%&'*+-.^_`|~ .
func IsTokenChar(c byte) bool {
	switch {
	case c < 64:
		return tcharLo&(1<<c) != 0
	case c < 128:
		return tcharHi&(1<<(c-64)) != 0
	default:
		return false
	}
}

// IsToken reports whether s is an RFC 9110 token (1*tchar) of any length.
func IsToken(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if !IsTokenChar(s[i]) {
			return false
		}
	}
	return true
}

// ValidName reports whether name is a field name a Policy may write: an
// RFC 9110 token of 1 to MaxNameBytes bytes (07 req 21). Pseudo-header
// names such as ":authority" are not tokens and fail.
func ValidName(name string) bool {
	return len(name) <= MaxNameBytes && IsToken(name)
}

// CheckName is ValidName with the reason: nil, or an error wrapping
// [ErrNameEmpty], [ErrNameTooLong] or [ErrNameChar]. The message names the
// offending byte by value and offset; it is meant for configuration
// diagnostics, whose names are operator-authored.
func CheckName(name string) error {
	switch {
	case name == "":
		return ErrNameEmpty
	case len(name) > MaxNameBytes:
		return fmt.Errorf("%w: %d bytes, the limit is %d", ErrNameTooLong, len(name), MaxNameBytes)
	}
	for i := range len(name) {
		if c := name[i]; !IsTokenChar(c) {
			return fmt.Errorf("%w: byte %q at offset %d", ErrNameChar, c, i)
		}
	}
	return nil
}

// lowerEq reports whether s equals lower ignoring ASCII case; lower must be
// lowercase ASCII. Unlike strings.EqualFold it never folds non-ASCII runes
// (such as the Kelvin sign) onto ASCII letters.
func lowerEq(s, lower string) bool {
	if len(s) != len(lower) {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

// equalFold reports whether a and b are equal ignoring ASCII case.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
