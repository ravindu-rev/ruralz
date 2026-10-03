// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"errors"
	"testing"
)

// Tests for offsets to line and column (01 req 15: RZ-CFG-001 "at the
// decoder's offset converted to line and column"; columns count code
// points like diag.Location).

func TestLocator(t *testing.T) {
	data := []byte("{\n\t\"aé\U0001F600\": 1,\r\n  \"b\":\r2\n}")
	cases := []struct {
		off  int
		want Position
	}{
		{0, Position{1, 1}},
		{1, Position{1, 2}},  // the LF itself
		{2, Position{2, 1}},  // the tab
		{3, Position{2, 2}},  // the quote
		{4, Position{2, 3}},  // a
		{5, Position{2, 4}},  // é (2 bytes)
		{6, Position{2, 4}},  // inside é
		{7, Position{2, 5}},  // 😀 (4 bytes)
		{9, Position{2, 5}},  // inside 😀
		{11, Position{2, 6}}, // closing quote
		{14, Position{2, 9}}, // 1
		{16, Position{2, 11}},
		{18, Position{3, 1}}, // CR LF is one break
		{20, Position{3, 3}}, // "b"
		{24, Position{3, 7}}, // CR alone breaks the line
		{25, Position{4, 1}},
		{26, Position{4, 2}},
		{27, Position{5, 1}},
		{28, Position{5, 2}}, // end of input
		{500, Position{5, 2}},
	}
	l := NewLocator(data)
	for _, c := range cases {
		if got := l.Position(c.off); got != c.want {
			t.Fatalf("Position(%d) = %+v, want %+v", c.off, got, c.want)
		}
		if got := PositionOf(data, c.off); got != c.want {
			t.Fatalf("PositionOf(%d) = %+v, want %+v", c.off, got, c.want)
		}
	}
	// Going backwards restarts.
	if got := l.Position(3); got != (Position{2, 2}) {
		t.Fatalf("backwards Position(3) = %+v", got)
	}
	// Invalid bytes count one column each.
	if got := PositionOf([]byte("\xff\xfe\"x"), 3); got != (Position{1, 4}) {
		t.Fatalf("invalid bytes: %+v", got)
	}
}

func TestLocatorWithScanner(t *testing.T) {
	// A scanner error offset maps to the position a diagnostic reports.
	data := []byte("{\n  \"name\": \"x\",\n  \"name\": \"y\"\n}")
	err := Validate(data, ScanOptions{})
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v", err)
	}
	l := NewLocator(data)
	if first, dup := l.Position(e.Other), l.Position(e.Offset); first != (Position{2, 3}) || dup != (Position{3, 3}) {
		t.Fatalf("positions first %+v, duplicate %+v", first, dup)
	}
}
