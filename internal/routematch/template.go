// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"errors"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// Segment is one segment of a path template: a literal or a parameter,
// never both.
type Segment struct {
	// Literal is the normalized literal text; empty for a parameter.
	Literal string
	// Param is the parameter name of a "{name}" segment; empty for a
	// literal.
	Param string
}

// Template is a parsed path template such as "/v1/orders/{orderId}" (04
// req 29). Segments holds the segments between slashes; TrailingSlash
// records a final "/", which is significant. "/" is the template with no
// segments and a trailing slash.
type Template struct {
	// Segments are the template's segments, left to right.
	Segments []Segment
	// TrailingSlash is true when the template ends with "/".
	TrailingSlash bool
}

// ParseTemplate parses a path template by the grammar of 04 req 29: it
// begins with "/"; its segments split on "/"; each segment is either a
// literal (non-empty, without "{" or "}") or exactly "{name}" with name
// matching [A-Za-z_][A-Za-z0-9_]*; names are unique; there are no
// partial-segment captures and no catch-all. A literal is normalized like a
// request path segment (see [NormalizePath]) so it compares equal to the
// normalized request, and a literal that [NormalizePath] would reject or
// that is a dot segment is an error. Every error carries [CodeInvalid]
// (RZ-CFG-005).
func ParseTemplate(s string) (Template, error) {
	rest, ok := strings.CutPrefix(s, "/")
	if !ok {
		return Template{}, errcode.Errorf(CodeInvalid, "path template %q does not begin with /", s)
	}
	if rest == "" {
		return Template{TrailingSlash: true}, nil
	}
	var t Template
	if trimmed, ok := strings.CutSuffix(rest, "/"); ok {
		t.TrailingSlash = true
		rest = trimmed
	}
	n := 1 + strings.Count(rest, "/")
	t.Segments = make([]Segment, 0, n)
	pos := 0
	for part := range strings.SplitSeq(rest, "/") {
		pos++
		seg, err := parseSegment(s, pos, part)
		if err != nil {
			return Template{}, err
		}
		if seg.Param != "" {
			for _, prev := range t.Segments {
				if prev.Param == seg.Param {
					return Template{}, errcode.Errorf(CodeInvalid,
						"path template %q names parameter %q twice", s, seg.Param)
				}
			}
		}
		t.Segments = append(t.Segments, seg)
	}
	return t, nil
}

// parseSegment parses segment number pos (from 1) of template s.
func parseSegment(s string, pos int, part string) (Segment, error) {
	if part == "" {
		return Segment{}, errcode.Errorf(CodeInvalid, "path template %q has an empty segment %d", s, pos)
	}
	if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
		if name := part[1 : len(part)-1]; validParamName(name) {
			return Segment{Param: name}, nil
		}
		return Segment{}, errcode.Errorf(CodeInvalid,
			"path template %q: segment %d %q is not {name} with name matching [A-Za-z_][A-Za-z0-9_]*", s, pos, part)
	}
	if strings.ContainsAny(part, "{}") {
		return Segment{}, errcode.Errorf(CodeInvalid,
			"path template %q: segment %d %q has a brace outside a whole-segment {name} parameter", s, pos, part)
	}
	norm, err := NormalizePath("/" + part)
	if err != nil {
		return Segment{}, errcode.Errorf(CodeInvalid,
			"path template %q: segment %d %q is not a valid path segment (%s)", s, pos, part, rejectReason(err))
	}
	if norm == "/" {
		return Segment{}, errcode.Errorf(CodeInvalid,
			"path template %q: segment %d %q is a dot segment, which a normalized path never holds", s, pos, part)
	}
	return Segment{Literal: norm[1:]}, nil
}

// rejectReason returns the reason text of a [NormalizePath] error, for a
// configuration diagnostic that must not show a request code (RZ-RT-017 or
// RZ-RT-001).
func rejectReason(err error) string {
	if pe := (*PathError)(nil); errors.As(err, &pe) {
		return pe.Reason.String()
	}
	if errors.Is(err, ErrAsteriskForm) {
		return RejectNotOriginForm.String()
	}
	return err.Error()
}

func validParamName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		letter := 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || c == '_'
		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// CheckTemplate reports whether s is a valid path template (see
// [ParseTemplate]); configuration validation calls it for every
// match.path.template (RZ-CFG-005).
func CheckTemplate(s string) error {
	_, err := ParseTemplate(s)
	return err
}

// String returns the canonical template text, with normalized literals.
func (t Template) String() string {
	var b strings.Builder
	for _, s := range t.Segments {
		b.WriteByte('/')
		if s.Param != "" {
			b.WriteByte('{')
			b.WriteString(s.Param)
			b.WriteByte('}')
		} else {
			b.WriteString(s.Literal)
		}
	}
	if t.TrailingSlash || len(t.Segments) == 0 {
		b.WriteByte('/')
	}
	return b.String()
}

// Params returns the number of parameters.
func (t Template) Params() int {
	n := 0
	for _, s := range t.Segments {
		if s.Param != "" {
			n++
		}
	}
	return n
}

// Match reports whether the normalized path matches t. Each "{name}"
// matches exactly one non-empty segment; the raw (still escaped) captures
// are appended to raw in template order, to decode with [DecodeParam]. On a
// mismatch raw comes back with its original length. Match allocates nothing
// when raw has room for the captures.
func (t *Template) Match(path string, raw []string) ([]string, bool) {
	start := len(raw)
	if path == "" || path[0] != '/' {
		return raw, false
	}
	i := 1
	for _, s := range t.Segments {
		if i > len(path) {
			return raw[:start], false
		}
		seg, next := NextSegment(path, i)
		switch {
		case s.Param != "":
			if seg == "" {
				return raw[:start], false
			}
			raw = append(raw, seg)
		case seg != s.Literal:
			return raw[:start], false
		}
		i = next
	}
	// What is left of path must be "" (no trailing slash) or exactly one
	// empty segment (a trailing slash).
	switch {
	case i > len(path) && !t.TrailingSlash:
		return raw, true
	case i == len(path) && t.TrailingSlash:
		return raw, true
	default:
		return raw[:start], false
	}
}
