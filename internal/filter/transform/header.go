// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"net/http"
	"slices"
	"strings"
)

// Canonical keys of the fields a transform writes besides its header
// entries.
const (
	keyContentType  = "Content-Type"
	keyContentRange = "Content-Range"
	keyETag         = "Etag"
	keyAcceptRanges = "Accept-Ranges"
)

// headerEdit is one header edit, applied at commit in Policy order: line
// replaces every line of key, a nil line removes the field.
type headerEdit struct {
	key  string
	line []string
}

// hasVariant reports whether h holds one of the fields in names, the
// canonical keys the Policy writes, under a differently cased key; lens is
// the lenBit union of their lengths, so most keys are skipped cheaply.
func hasVariant(h http.Header, names []string, lens uint64) bool {
	for k := range h {
		if lens&lenBit(len(k)) == 0 {
			continue
		}
		for _, n := range names {
			if len(n) == len(k) && n != k && asciiEqualFold(n, k) {
				return true
			}
		}
	}
	return false
}

// foldVariants merges the differently cased keys of every field in names
// into its canonical key: the canonical key's lines first, then the other
// keys' lines sorted by key, as the headers Policy folds them
// (docs/architecture/03-data-plane.md "Headers Policy"). It allocates, so
// it runs only when hasVariant is true.
func foldVariants(h http.Header, names []string) {
	var variants []string
	for _, n := range names {
		variants = variants[:0]
		for k := range h {
			if len(k) == len(n) && k != n && asciiEqualFold(n, k) {
				variants = append(variants, k)
			}
		}
		if len(variants) == 0 {
			continue
		}
		slices.Sort(variants)
		lines := slices.Clone(h[n])
		for _, k := range variants {
			lines = append(lines, h[k]...)
			delete(h, k)
		}
		if len(lines) == 0 {
			delete(h, n)
			continue
		}
		h[n] = slices.Clip(lines)
	}
}

// weakenETag turns every strong entity-tag line of h's ETag weak by
// prefixing W/, since a rewritten body no longer equals the Upstream's
// bytes (docs/architecture/03-data-plane.md "Transform Policies",
// "Validators"); weak tags and other lines stay.
func weakenETag(h http.Header) {
	lines := h[keyETag]
	var out []string
	for i, l := range lines {
		if !strings.HasPrefix(l, `"`) {
			continue
		}
		if out == nil {
			out = slices.Clone(lines)
		}
		out[i] = "W/" + l
	}
	if out != nil {
		h[keyETag] = out
	}
}

// hasField reports whether h holds the field whose canonical key is key,
// under that key or one that differs only in ASCII case, without
// allocating.
func hasField(h http.Header, key string) bool {
	if _, ok := h[key]; ok {
		return true
	}
	for k := range h {
		if len(k) == len(key) && asciiEqualFold(k, key) {
			return true
		}
	}
	return false
}

// lenBit maps a key length to one bit: bit n for n below 64, bit 0 for 64
// and over (no key is empty).
func lenBit(n int) uint64 {
	if n >= 64 {
		return 1
	}
	return 1 << n
}

// asciiEqualFold reports whether a and b are equal ignoring ASCII case;
// unlike strings.EqualFold it never folds a non-ASCII rune, such as the
// Kelvin sign, onto an ASCII letter.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
