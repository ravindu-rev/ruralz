// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"cmp"
	"strings"
	"unicode/utf8"
)

// CompareUTF16 compares two strings by their UTF-16 code units, the RFC 8785
// section 3.2.3 member order, returning -1, 0 or +1. It differs from byte
// order only between a supplementary character (U+10000 and above, a
// surrogate pair) and a BMP character from U+E000 to U+FFFF. Invalid UTF-8
// bytes compare as U+FFFD, then by bytes.
func CompareUTF16(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	if i == len(a) || i == len(b) {
		return cmp.Compare(len(a), len(b))
	}
	if a[i] < utf8.RuneSelf && b[i] < utf8.RuneSelf {
		return cmp.Compare(int(a[i]), int(b[i]))
	}
	// Back up to the start of the rune holding the first difference; for
	// valid UTF-8 both strings start a rune at the same offset.
	for i > 0 && (!utf8.RuneStart(a[i]) || !utf8.RuneStart(b[i])) {
		i--
	}
	ra, _ := utf8.DecodeRuneInString(a[i:])
	rb, _ := utf8.DecodeRuneInString(b[i:])
	if c := cmp.Compare(utf16Lead(ra), utf16Lead(rb)); c != 0 {
		return c
	}
	if ra != rb {
		// Both supplementary with the same high surrogate: the low
		// surrogates follow code point order.
		return cmp.Compare(int(ra), int(rb))
	}
	// Equal replacement runes from invalid bytes: fall back to bytes.
	return strings.Compare(a[i:], b[i:])
}

// utf16Lead returns the first UTF-16 code unit of r.
func utf16Lead(r rune) int {
	if r < 0x10000 {
		return int(r)
	}
	return 0xD800 + int((r-0x10000)>>10)
}
