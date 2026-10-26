// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import "strings"

// lineEnd returns the offset of the line break that ends the line holding
// byte offset off, or the length of the text.
func lineEnd(text string, off int) int {
	if nl := strings.IndexByte(text[off:], '\n'); nl >= 0 {
		return off + nl
	}
	return len(text)
}

// leadingSpaces returns the number of spaces that start s.
func leadingSpaces(s string) int {
	n := 0
	for n < len(s) && s[n] == ' ' {
		n++
	}
	return n
}

// markerAt reports whether line is a document marker ("---" or "...").
func markerAt(line string) bool {
	return markerLine(line, "---") || markerLine(line, "...")
}
