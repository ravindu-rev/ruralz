// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package httpfield

import (
	"net/url"
	"strings"
)

// Query edits a raw query string (the request-target part after "?", without
// the "?") without reordering or re-encoding the pairs it does not touch.
// "&" separates segments and ";" is data. A segment's key is the text before
// its first "=" (the whole segment when it has none); keys compare with a
// name after percent-decoding and "+" to space, or by their raw text when
// they hold an invalid percent escape. Empty segments (as in "a=1&&b=2") are
// kept and match no name.
//
// Without edits, [Query.AppendRaw] reproduces the parsed string byte for
// byte. The zero value is an empty query. A Query is not safe for
// concurrent use; copying one shares its storage.
type Query struct {
	segs []string
}

// ParseQuery splits raw into its segments. It never fails and allocates
// one slice; the segments share raw's bytes.
func ParseQuery(raw string) Query {
	var q Query
	q.Reset(raw)
	return q
}

// Reset replaces q's content with the segments of raw, reusing q's
// storage.
func (q *Query) Reset(raw string) {
	clear(q.segs)
	q.segs = q.segs[:0]
	if raw == "" {
		return
	}
	if n := strings.Count(raw, "&") + 1; cap(q.segs) < n {
		q.segs = make([]string, 0, n)
	}
	for {
		seg, rest, found := strings.Cut(raw, "&")
		q.segs = append(q.segs, seg)
		if !found {
			return
		}
		raw = rest
	}
}

// Get returns the value of the first pair whose key equals name, decoded
// like a key (a value with an invalid percent escape is returned raw), and
// whether there is one. A bare key ("k") has the empty value.
func (q *Query) Get(name string) (string, bool) {
	for _, s := range q.segs {
		if key, value, _ := strings.Cut(s, "="); s != "" && keyEquals(key, name) {
			if v, err := url.QueryUnescape(value); err == nil {
				return v, true
			}
			return value, true
		}
	}
	return "", false
}

// Set gives name the value: the first pair whose key equals name is
// replaced, in its place, by one pair "name=value", and later pairs with
// that key are dropped; without such a pair, "name=value" is appended.
// Name and value are written percent-encoded per RFC 3986: every byte
// outside the unreserved set (ALPHA, DIGIT, "-", ".", "_", "~") becomes
// %XX with uppercase hex digits, a space %20, so the pair decodes back to
// name and value whatever their bytes. Other segments are untouched
// (docs/architecture/03-data-plane.md "Transform Policies").
func (q *Query) Set(name, value string) {
	out := q.segs[:0]
	found := false
	for _, s := range q.segs {
		if key, _, _ := strings.Cut(s, "="); s != "" && keyEquals(key, name) {
			if found {
				continue
			}
			found = true
			s = encodePair(name, value)
		}
		out = append(out, s)
	}
	clear(q.segs[len(out):])
	if !found {
		out = append(out, encodePair(name, value))
	}
	q.segs = out
}

// encodePair returns "name=value" with both percent-encoded per RFC 3986
// (AppendQueryEscape).
func encodePair(name, value string) string {
	b := make([]byte, 0, len(name)+len(value)+1)
	b = AppendQueryEscape(b, name)
	b = append(b, '=')
	return string(AppendQueryEscape(b, value))
}

// AppendQueryEscape appends s to dst percent-encoded per RFC 3986 section
// 2.1: every byte outside the unreserved set (ALPHA, DIGIT, "-", ".", "_",
// "~") becomes "%" and two uppercase hex digits, so a space is %20, never
// "+". The result is one query name or value.
func AppendQueryEscape(dst []byte, s string) []byte {
	const upperHex = "0123456789ABCDEF"
	for i := range len(s) {
		c := s[i]
		if isUnreserved(c) {
			dst = append(dst, c)
			continue
		}
		dst = append(dst, '%', upperHex[c>>4], upperHex[c&0x0F])
	}
	return dst
}

// isUnreserved reports an RFC 3986 unreserved byte: ALPHA, DIGIT, "-",
// ".", "_" or "~".
func isUnreserved(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	default:
		return c == '-' || c == '.' || c == '_' || c == '~'
	}
}

// Del removes every pair whose key equals name. Other segments are
// untouched.
func (q *Query) Del(name string) {
	out := q.segs[:0]
	for _, s := range q.segs {
		if key, _, _ := strings.Cut(s, "="); s != "" && keyEquals(key, name) {
			continue
		}
		out = append(out, s)
	}
	clear(q.segs[len(out):])
	q.segs = out
}

// AppendRaw appends the query string, segments joined with "&", to dst.
func (q *Query) AppendRaw(dst []byte) []byte {
	for i, s := range q.segs {
		if i > 0 {
			dst = append(dst, '&')
		}
		dst = append(dst, s...)
	}
	return dst
}

// String returns the query string, segments joined with "&".
func (q *Query) String() string {
	return strings.Join(q.segs, "&")
}

// keyEquals reports whether the raw key decodes to name ("+" is space,
// "%XX" a byte); a key with an invalid percent escape compares by its raw
// text. It never allocates.
func keyEquals(raw, name string) bool {
	if !strings.ContainsAny(raw, "%+") || !validEscapes(raw) {
		return raw == name
	}
	j := 0
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch c {
		case '+':
			c = ' '
		case '%':
			c = unhex(raw[i+1])<<4 | unhex(raw[i+2])
			i += 2
		}
		if j >= len(name) || name[j] != c {
			return false
		}
		j++
	}
	return j == len(name)
}

// validEscapes reports whether every "%" in s starts a two-hex-digit
// escape.
func validEscapes(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
			return false
		}
		i += 2
	}
	return true
}

func isHex(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}
