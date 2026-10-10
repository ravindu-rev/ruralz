// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import "testing"

// TestSrcCursor covers the cursor that maps token positions to byte
// offsets of a segment (01 req 14: lines, and columns in code points),
// forward, backward and past the text.
func TestSrcCursor(t *testing.T) {
	text := "ab\néé: x\n\nz"
	c := newCursor(text)
	for _, tt := range []struct{ line, col, want int }{
		{1, 1, 0},
		{1, 3, 2},
		{2, 2, 5},
		{2, 4, 8},
		{4, 1, 12},
		{2, 1, 3},
		{1, 2, 1},
		{3, 1, 11},
		{4, 2, 13},
		{1, 4, -1},
		{5, 1, -1},
		{0, 1, -1},
		{1, 0, -1},
	} {
		if got := c.offset(tt.line, tt.col); got != tt.want {
			t.Errorf("offset(%d, %d) = %d, want %d", tt.line, tt.col, got, tt.want)
		}
	}
	if l, col := advance(text, 0, 8, 1, 1); l != 2 || col != 4 {
		t.Errorf("advance = %d:%d", l, col)
	}
	if lineEnd(text, 3) != 10 || lineEnd(text, 12) != len(text) || leadingSpaces("  a") != 2 || !markerAt("--- x") || markerAt("----") {
		t.Error("helpers")
	}
}
