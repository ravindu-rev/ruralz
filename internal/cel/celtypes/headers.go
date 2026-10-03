// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"net/http"
	"slices"
	"strings"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// maxStackKey is the longest name canonicalized on the stack; a longer name
// is canonicalized by http.CanonicalHeaderKey, which may allocate.
const maxStackKey = 128

// headerSource is a response or step header map as CEL reads it (03 req
// 19): keys are the lowercased names of the fields received, repeated field
// lines are joined by ", " in received order, lookup is case-insensitive,
// and iteration is by ascending lowercased name. Every field is shown,
// Host included. The source points at the view's Header field; a nil
// pointer is an empty map.
type headerSource struct{ h *http.Header }

func (s headerSource) lookup(name string) (ref.Val, bool) {
	return lookupHeader(derefHeader(s.h), name, false)
}

func (s headerSource) sortedKeys() []string { return headerNames(derefHeader(s.h), false) }

func (s headerSource) size() int { return headerSize(derefHeader(s.h), false) }

func (s headerSource) raw() any { return rawHeaders(derefHeader(s.h), false) }

// requestHeaderSource is request.headers: a headerSource that never shows
// Host, which is request.host (03 req 19). It is a type of its own rather
// than a flag, so the source stays one pointer and boxes without
// allocating.
type requestHeaderSource struct{ h *http.Header }

func (s requestHeaderSource) lookup(name string) (ref.Val, bool) {
	return lookupHeader(derefHeader(s.h), name, true)
}

func (s requestHeaderSource) sortedKeys() []string { return headerNames(derefHeader(s.h), true) }

func (s requestHeaderSource) size() int { return headerSize(derefHeader(s.h), true) }

func (s requestHeaderSource) raw() any { return rawHeaders(derefHeader(s.h), true) }

// derefHeader returns the header map h points at; nil for a nil pointer.
func derefHeader(h *http.Header) http.Header {
	if h == nil {
		return nil
	}
	return *h
}

// lookupHeader returns the CEL string of the field named name, absent for
// Host when hideHost is set.
func lookupHeader(h http.Header, name string, hideHost bool) (ref.Val, bool) {
	if hideHost && isHost(name) {
		return nil, false
	}
	v, ok := joinedHeader(h, name)
	if !ok {
		return nil, false
	}
	return types.String(v), true
}

// rawHeaders materializes a header map as map[string]string (lowercased
// names), without Host when hideHost is set.
func rawHeaders(h http.Header, hideHost bool) map[string]string {
	names := headerNames(h, hideHost)
	m := make(map[string]string, len(names))
	for _, n := range names {
		m[n], _ = joinedHeader(h, n)
	}
	return m
}

// joinedHeader returns the field named name as CEL sees it, with the
// semantics of expr.JoinedHeader: the exact http.CanonicalHeaderKey key
// first (name itself when name is not a token), else the smallest key in
// byte order equal to name under ASCII case folding, so the choice is
// deterministic. A canonical key that folds to name is that exact key, so
// the scan never meets one. The canonical key of a name up to maxStackKey
// bytes is built on the stack, so a hit on a single-valued field allocates
// nothing; a longer name allocates it.
func joinedHeader(h http.Header, name string) (string, bool) {
	if len(h) == 0 {
		return "", false
	}
	var vs []string
	var ok bool
	if len(name) <= maxStackKey {
		var buf [maxStackKey]byte
		key := canonicalKey(buf[:0], name)
		vs, ok = h[string(key)]
	} else {
		vs, ok = h[http.CanonicalHeaderKey(name)]
	}
	if !ok {
		best := ""
		for k, v := range h {
			if asciiEqualFold(k, name) && (!ok || k < best) {
				vs, best, ok = v, k, true
			}
		}
	}
	if !ok {
		return "", false
	}
	return joinValues(vs), true
}

// joinValues joins field lines with ", " (RFC 9110 section 5.3).
func joinValues(vs []string) string {
	if len(vs) == 1 {
		return vs[0]
	}
	return strings.Join(vs, ", ")
}

// headerNames returns the distinct lowercased names of h in ascending byte
// order, without Host when hideHost is set.
func headerNames(h http.Header, hideHost bool) []string {
	names := make([]string, 0, len(h))
	for k := range h {
		if !hideHost || !isHost(k) {
			names = append(names, asciiLower(k))
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// headerSize counts the distinct names of h, without Host when hideHost is
// set, without allocating when every key is canonical (net/http's maps
// are).
func headerSize(h http.Header, hideHost bool) int {
	n := 0
	for k := range h {
		if hideHost && isHost(k) {
			continue
		}
		if !isCanonical(k) {
			return len(headerNames(h, hideHost))
		}
		n++
	}
	return n
}

// isHost reports the Host field name in any case.
func isHost(name string) bool { return asciiEqualFold(name, "host") }

// canonicalKey appends the textproto canonical form of s to dst: s itself
// when it holds a byte that is not a token character, else the first letter
// and every letter after '-' upper case, the others lower case.
func canonicalKey(dst []byte, s string) []byte {
	for i := range len(s) {
		if !isTokenByte(s[i]) {
			return append(dst, s...)
		}
	}
	upper := true
	for i := range len(s) {
		c := s[i]
		if upper && 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		} else if !upper && 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst = append(dst, c)
		upper = c == '-'
	}
	return dst
}

// isCanonical reports whether k is a token in textproto canonical form, so
// no other canonical key folds to the same name.
func isCanonical(k string) bool {
	if k == "" {
		return false
	}
	upper := true
	for i := range len(k) {
		c := k[i]
		if !isTokenByte(c) {
			return false
		}
		if upper && 'a' <= c && c <= 'z' || !upper && 'A' <= c && c <= 'Z' {
			return false
		}
		upper = c == '-'
	}
	return true
}

// isTokenByte reports an RFC 9110 token character.
func isTokenByte(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// asciiEqualFold reports whether a and b are equal under ASCII case folding
// (field names are ASCII; Unicode folding would match non-ASCII keys).
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		if lowerByte(a[i]) != lowerByte(b[i]) {
			return false
		}
	}
	return true
}

// asciiLower returns s with ASCII upper-case letters lowered.
func asciiLower(s string) string {
	for i := range len(s) {
		if 'A' <= s[i] && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				b[j] = lowerByte(b[j])
			}
			return string(b)
		}
	}
	return s
}

// lowerByte lowers an ASCII letter.
func lowerByte(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}
