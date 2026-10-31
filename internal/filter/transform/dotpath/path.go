// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package dotpath

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// Kind is the kind of a path segment.
type Kind uint8

// Segment kinds.
const (
	// Name names an object member.
	Name Kind = iota + 1
	// Index is a numeric segment: it indexes an array and names a member
	// of an object.
	Index
	// Star matches every element of an array, in order.
	Star
)

// String returns "name", "index" or "star".
func (k Kind) String() string {
	switch k {
	case Name:
		return "name"
	case Index:
		return "index"
	case Star:
		return "star"
	default:
		return "invalid"
	}
}

// Segment is one segment of a Path.
type Segment struct {
	// Kind is the segment kind.
	Kind Kind
	// Name is the member name a Name or Index segment selects, its escapes
	// decoded; "" for Star.
	Name string
	// Index is the element index of an Index segment.
	Index int
}

// Path is a parsed dot path. The zero Path has no segment; Parse never
// returns one.
type Path struct {
	segs []Segment
	src  string
	star bool
}

// ErrSyntax matches every error Parse returns.
var ErrSyntax = errors.New("dotpath: invalid path")

// SyntaxError is a path Parse refuses: what jsonval.ParseDotPath refuses,
// or text that is not UTF-8. Its text names the rule and the byte offset,
// never the path; errors.Is also matches the jsonval sentinel.
type SyntaxError struct {
	err error
}

// Error returns "dotpath: <rule> at byte <n>".
func (e *SyntaxError) Error() string {
	return "dotpath: " + strings.TrimPrefix(e.err.Error(), "jsonval: ")
}

// Unwrap returns the parser's error.
func (e *SyntaxError) Unwrap() error { return e.err }

// Is reports whether target is ErrSyntax.
func (e *SyntaxError) Is(target error) bool { return target == ErrSyntax }

// Parse parses a dot path with jsonval.ParseDotPath, the one parser of
// the syntax the transform Policies and a composition step's target share
// (docs/architecture/03-data-plane.md "Transform Policies", "Paths"): "."
// separates segments, and \., \* and \\ write a literal dot, star or
// backslash; an empty segment, the empty path included, an unescaped "*"
// inside a longer segment, or any other backslash escape, a trailing
// backslash included, is an error, as is text that is not UTF-8, since a
// write may create a member the path names. A segment that is exactly an
// unescaped "*" is Star. A segment of ASCII digits that fits an int is
// Index (jsonval.DotSegment.Index): it indexes an array and names a member
// of an object; any other segment, "*" written \* included, is Name and
// only names a member.
func Parse(s string) (Path, error) {
	if !utf8.ValidString(s) {
		return Path{}, &SyntaxError{err: errors.New("invalid UTF-8 in a dot path at byte " + strconv.Itoa(invalidOffset(s)))}
	}
	dp, err := jsonval.ParseDotPath(s)
	if err != nil {
		return Path{}, &SyntaxError{err: err}
	}
	p := Path{src: s, segs: make([]Segment, 0, len(dp))}
	for _, d := range dp {
		seg := Segment{Kind: Name, Name: d.Name}
		switch n, ok := d.Index(); {
		case d.Star:
			seg = Segment{Kind: Star}
			p.star = true
		case ok:
			seg = Segment{Kind: Index, Name: d.Name, Index: n}
		}
		p.segs = append(p.segs, seg)
	}
	return p, nil
}

// invalidOffset returns the offset of the first byte of s that does not
// start a valid UTF-8 sequence.
func invalidOffset(s string) int {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return len(s)
}

// Len returns the number of segments.
func (p Path) Len() int { return len(p.segs) }

// Seg returns segment i; it panics when i is out of range.
func (p Path) Seg(i int) Segment { return p.segs[i] }

// HasStar reports whether a segment is Star, which a path an operation
// writes to may not hold.
func (p Path) HasStar() bool { return p.star }

// String returns the path as it was parsed.
func (p Path) String() string { return p.src }
