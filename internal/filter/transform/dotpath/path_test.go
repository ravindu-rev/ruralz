// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package dotpath

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// Tests for the dot path syntax (docs/architecture/03-data-plane.md
// "Transform Policies", "Paths").

// TestParse checks the segments of valid paths: "." separation, the \.,
// \* and \\ escapes, the whole-segment star, numeric segments of ASCII
// digits that fit an int, and the segments that only name members.
func TestParse(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []Segment
		star bool
	}{
		{"one name", "total", []Segment{{Kind: Name, Name: "total"}}, false},
		{"nested names", "customer.email", []Segment{{Kind: Name, Name: "customer"}, {Kind: Name, Name: "email"}}, false},
		{"star", "items.*.sku", []Segment{{Kind: Name, Name: "items"}, {Kind: Star}, {Kind: Name, Name: "sku"}}, true},
		{"index", "items.0.sku", []Segment{{Kind: Name, Name: "items"}, {Kind: Index, Name: "0", Index: 0}, {Kind: Name, Name: "sku"}}, false},
		{"large index", "a.2147483647", []Segment{{Kind: Name, Name: "a"}, {Kind: Index, Name: "2147483647", Index: 2147483647}}, false},
		{"digits past an int name a member", "a.99999999999999999999", []Segment{{Kind: Name, Name: "a"}, {Kind: Name, Name: "99999999999999999999"}}, false},
		{"leading zero", "a.007", []Segment{{Kind: Name, Name: "a"}, {Kind: Index, Name: "007", Index: 7}}, false},
		{"negative names a member", "a.-1", []Segment{{Kind: Name, Name: "a"}, {Kind: Name, Name: "-1"}}, false},
		{"escaped dot", `a\.b.c`, []Segment{{Kind: Name, Name: "a.b"}, {Kind: Name, Name: "c"}}, false},
		{"escaped star is a name", `a.\*`, []Segment{{Kind: Name, Name: "a"}, {Kind: Name, Name: "*"}}, false},
		{"escaped backslash", `a\\b`, []Segment{{Kind: Name, Name: `a\b`}}, false},
		{"escapes in one segment", `x\.\*\\y`, []Segment{{Kind: Name, Name: `x.*\y`}}, false},
		{"escaped star inside a segment", `a\*b`, []Segment{{Kind: Name, Name: "a*b"}}, false},
		{"escaped digits are a name", `1\.0`, []Segment{{Kind: Name, Name: "1.0"}}, false},
		{"unicode name", "caf\xc3\xa9", []Segment{{Kind: Name, Name: "caf\xc3\xa9"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", tc.in, err)
			}
			got := make([]Segment, p.Len())
			for i := range got {
				got[i] = p.Seg(i)
			}
			if !reflect.DeepEqual(got, tc.want) || p.HasStar() != tc.star || p.String() != tc.in {
				t.Errorf("Parse(%q) = %+v, star %v, %q; want %+v, star %v", tc.in, got, p.HasStar(), p.String(), tc.want, tc.star)
			}
		})
	}
}

// TestParseErrors checks the syntax errors, those of jsonval.ParseDotPath
// that a composition step's target shares, and text that is not UTF-8:
// an empty path or segment, a backslash escape other than \., \* and \\,
// a trailing one included, and an unescaped * inside a longer segment,
// each named by its rule and byte offset.
func TestParseErrors(t *testing.T) {
	cases := []struct {
		in   string
		kind error
		text string
	}{
		{"", jsonval.ErrEmptySegment, "dotpath: empty dot path segment"},
		{".a", jsonval.ErrEmptySegment, "dotpath: empty dot path segment at byte 0"},
		{"a.", jsonval.ErrEmptySegment, "dotpath: empty dot path segment at byte 2"},
		{"a..b", jsonval.ErrEmptySegment, "dotpath: empty dot path segment at byte 2"},
		{`a\b`, jsonval.ErrBadEscape, "dotpath: invalid dot path escape at byte 1"},
		{`a\`, jsonval.ErrBadEscape, "dotpath: invalid dot path escape at byte 1"},
		{`a.b\n`, jsonval.ErrBadEscape, "dotpath: invalid dot path escape at byte 3"},
		{`\x`, jsonval.ErrBadEscape, "dotpath: invalid dot path escape at byte 0"},
		{"a*b", jsonval.ErrBareStar, "dotpath: unescaped * inside a dot path segment at byte 0"},
		{"items.**", jsonval.ErrBareStar, "dotpath: unescaped * inside a dot path segment at byte 6"},
		{"a.\xffb", nil, "dotpath: invalid UTF-8 in a dot path at byte 2"},
	}
	for _, tc := range cases {
		_, err := Parse(tc.in)
		var se *SyntaxError
		if !errors.Is(err, ErrSyntax) || !errors.As(err, &se) || (tc.kind != nil && !errors.Is(err, tc.kind)) || err.Error() != tc.text {
			t.Errorf("Parse(%q) = %v, want %q", tc.in, err, tc.text)
		}
		if strings.Contains(err.Error(), tc.in) && tc.in != "" {
			t.Errorf("Parse(%q): the error quotes the path", tc.in)
		}
	}
}

// TestKindString checks the kind names used in messages.
func TestKindString(t *testing.T) {
	for k, want := range map[Kind]string{Name: "name", Index: "index", Star: "star", 0: "invalid"} {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", k, got, want)
		}
	}
}
