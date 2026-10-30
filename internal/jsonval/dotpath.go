// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"errors"
	"strconv"
	"strings"
)

// Dot path errors; ParseDotPath wraps them with the offending text.
var (
	// ErrEmptySegment reports an empty path or an empty segment ("a..b",
	// ".a", "a.").
	ErrEmptySegment = errors.New("jsonval: empty dot path segment")
	// ErrBadEscape reports a backslash escape other than "\.", "\*" and
	// "\\", or a trailing backslash.
	ErrBadEscape = errors.New("jsonval: invalid dot path escape")
	// ErrBareStar reports an unescaped "*" inside a longer segment: "*"
	// matches every element only as a whole segment, and "\*" writes a
	// literal star.
	ErrBareStar = errors.New("jsonval: unescaped * inside a dot path segment")
)

// DotPath is a parsed dot path into a JSON tree, the syntax of
// docs/architecture/03-data-plane.md "Transform Policies" (Paths) that a
// composition step's target uses too: "." separates segments; "\.", "\*"
// and "\\" write a literal dot, star or backslash; a segment that is an
// unescaped "*" matches every element of an array, in order; a numeric
// segment indexes an array and names a member of an object.
type DotPath []DotSegment

// DotSegment is one segment of a DotPath.
type DotSegment struct {
	// Name is the segment's text with its escapes decoded; empty for the
	// "*" segment.
	Name string
	// Star is set for the "*" segment.
	Star bool
}

// Index returns the array index a segment names: its Name when that is
// all ASCII digits and fits an int. A "*" segment and any other name are
// no index.
func (s DotSegment) Index() (int, bool) {
	if s.Star || s.Name == "" {
		return 0, false
	}
	for i := range len(s.Name) {
		if c := s.Name[i]; c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s.Name)
	if err != nil {
		return 0, false
	}
	return n, true
}

// HasStar reports whether p holds a "*" segment.
func (p DotPath) HasStar() bool {
	for _, s := range p {
		if s.Star {
			return true
		}
	}
	return false
}

// ParseDotPath parses s. An empty path or segment is ErrEmptySegment, a
// backslash escape other than "\.", "\*" and "\\" or a trailing backslash
// ErrBadEscape, and an unescaped "*" inside a longer segment ErrBareStar
// (all RZ-CFG-005 at validation); the error names the offending segment by
// its byte offset in s, never quoting s.
func ParseDotPath(s string) (DotPath, error) {
	if s == "" {
		return nil, ErrEmptySegment
	}
	var (
		out   DotPath
		buf   strings.Builder
		start int
		star  bool // the segment so far is exactly one unescaped "*"
		bare  bool // the segment holds an unescaped "*"
	)
	end := func(at int) error {
		switch {
		case at == start:
			return dotPathError(ErrEmptySegment, start)
		case star && at-start == 1:
			out = append(out, DotSegment{Star: true})
		case bare:
			return dotPathError(ErrBareStar, start)
		default:
			out = append(out, DotSegment{Name: buf.String()})
		}
		buf.Reset()
		star, bare = false, false
		return nil
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '.':
			if err := end(i); err != nil {
				return nil, err
			}
			start = i + 1
		case '\\':
			if i+1 >= len(s) {
				return nil, dotPathError(ErrBadEscape, i)
			}
			switch e := s[i+1]; e {
			case '.', '*', '\\':
				buf.WriteByte(e)
				i++
			default:
				return nil, dotPathError(ErrBadEscape, i)
			}
		case '*':
			star = i == start
			bare = true
			buf.WriteByte(c)
		default:
			buf.WriteByte(c)
		}
	}
	if err := end(len(s)); err != nil {
		return nil, err
	}
	return out, nil
}

// dotPathError returns kind with the byte offset where it was found.
func dotPathError(kind error, offset int) error {
	return &dotPathErr{kind: kind, offset: offset}
}

// dotPathErr is a ParseDotPath failure at a byte offset of the path.
type dotPathErr struct {
	kind   error
	offset int
}

// Error returns "<kind> at byte <offset>".
func (e *dotPathErr) Error() string {
	return e.kind.Error() + " at byte " + strconv.Itoa(e.offset)
}

// Unwrap returns the kind, so errors.Is matches the sentinel.
func (e *dotPathErr) Unwrap() error { return e.kind }
