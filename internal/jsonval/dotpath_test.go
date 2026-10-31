// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestParseDotPath checks the dot path syntax
// (docs/architecture/03-data-plane.md "Transform Policies", Paths): "."
// separates segments; "\.", "\*" and "\\" write a literal dot, star or
// backslash; an unescaped "*" alone is the every-element segment; an
// empty path or segment, another escape, a trailing backslash and an
// unescaped "*" inside a segment are errors that never quote the path.
func TestParseDotPath(t *testing.T) {
	name := func(s string) DotSegment { return DotSegment{Name: s} }
	star := DotSegment{Star: true}
	tests := []struct {
		in   string
		want DotPath
		err  error
	}{
		{"data", DotPath{name("data")}, nil},
		{"data.order.id", DotPath{name("data"), name("order"), name("id")}, nil},
		{"items.*.sku", DotPath{name("items"), star, name("sku")}, nil},
		{"*", DotPath{star}, nil},
		{`a\.b.c`, DotPath{name("a.b"), name("c")}, nil},
		{`a\*`, DotPath{name("a*")}, nil},
		{`\*`, DotPath{name("*")}, nil},
		{`a\\b`, DotPath{name(`a\b`)}, nil},
		{"items.0", DotPath{name("items"), name("0")}, nil},
		{"", nil, ErrEmptySegment},
		{"a..b", nil, ErrEmptySegment},
		{".a", nil, ErrEmptySegment},
		{"a.", nil, ErrEmptySegment},
		{`a\b`, nil, ErrBadEscape},
		{`a\`, nil, ErrBadEscape},
		{"a*", nil, ErrBareStar},
		{"**", nil, ErrBareStar},
		{"a.*b", nil, ErrBareStar},
		{`*\.`, nil, ErrBareStar},
	}
	for _, tt := range tests {
		got, err := ParseDotPath(tt.in)
		if tt.err != nil {
			if !errors.Is(err, tt.err) {
				t.Errorf("ParseDotPath(%q) = %v, %v; want %v", tt.in, got, err, tt.err)
			} else if tt.in != "" && strings.Contains(err.Error(), tt.in) {
				t.Errorf("ParseDotPath(%q) error %q quotes the path", tt.in, err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseDotPath(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
	}
}

// TestDotSegmentIndex checks the array index a segment names: all ASCII
// digits within an int; anything else, the "*" segment included, is no
// index.
func TestDotSegmentIndex(t *testing.T) {
	for _, tt := range []struct {
		seg  DotSegment
		want int
		ok   bool
	}{
		{DotSegment{Name: "0"}, 0, true},
		{DotSegment{Name: "17"}, 17, true},
		{DotSegment{Name: "007"}, 7, true},
		{DotSegment{Name: "-1"}, 0, false},
		{DotSegment{Name: "1a"}, 0, false},
		{DotSegment{Name: ""}, 0, false},
		{DotSegment{Name: "99999999999999999999"}, 0, false},
		{DotSegment{Star: true}, 0, false},
	} {
		if got, ok := tt.seg.Index(); got != tt.want || ok != tt.ok {
			t.Errorf("%+v.Index() = %d, %v; want %d, %v", tt.seg, got, ok, tt.want, tt.ok)
		}
	}
	if !(DotPath{{Name: "a"}, {Star: true}}).HasStar() || (DotPath{{Name: "a"}}).HasStar() {
		t.Error("HasStar")
	}
}
