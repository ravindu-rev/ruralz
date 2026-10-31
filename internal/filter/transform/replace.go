// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"bytes"
	"regexp"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/filter/transform/dotpath"
)

// literalReplacer replaces every occurrence of old by repl, as
// bytes.ReplaceAll does, appending into the caller's buffer: an empty old
// matches at the start and after each UTF-8 sequence. It allocates
// nothing beyond the growth of dst.
type literalReplacer struct {
	old, repl []byte
}

var _ dotpath.Replacer = (*literalReplacer)(nil)

// Replace appends s with every occurrence of old replaced to dst and
// reports whether the result differs from s.
func (r *literalReplacer) Replace(dst, s []byte) ([]byte, bool) {
	if len(r.old) == 0 {
		return r.replaceEmpty(dst, s)
	}
	i := bytes.Index(s, r.old)
	if i < 0 || bytes.Equal(r.old, r.repl) {
		return append(dst, s...), false
	}
	for i >= 0 {
		dst = append(dst, s[:i]...)
		dst = append(dst, r.repl...)
		s = s[i+len(r.old):]
		i = bytes.Index(s, r.old)
	}
	return append(dst, s...), true
}

// replaceEmpty inserts repl at the start of s and after each UTF-8
// sequence.
func (r *literalReplacer) replaceEmpty(dst, s []byte) ([]byte, bool) {
	if len(r.repl) == 0 {
		return append(dst, s...), false
	}
	dst = append(dst, r.repl...)
	for len(s) > 0 {
		_, size := utf8.DecodeRune(s)
		dst = append(dst, s[:size]...)
		dst = append(dst, r.repl...)
		s = s[size:]
	}
	return dst, true
}

// patternReplacer replaces every match of an RE2 pattern as
// regexp.Regexp.ReplaceAll does: $1 and $name in the replacement expand
// to the groups of each match. A string without a match costs no
// allocation; one with a match allocates as the regexp package does.
type patternReplacer struct {
	re   *regexp.Regexp
	repl []byte
}

var _ dotpath.Replacer = (*patternReplacer)(nil)

// Replace appends s with every match replaced to dst and reports whether
// the result differs from s.
func (r *patternReplacer) Replace(dst, s []byte) ([]byte, bool) {
	if !r.re.Match(s) {
		return append(dst, s...), false
	}
	out := r.re.ReplaceAll(s, r.repl)
	return append(dst, out...), !bytes.Equal(out, s)
}
