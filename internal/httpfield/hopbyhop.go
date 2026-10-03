// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package httpfield

import (
	"net/http"
	"strings"
)

// Direction is the way a message travels through the Node when it is
// forwarded.
type Direction uint8

// Directions for [StripHopByHop]. The zero Direction is not one of them;
// StripHopByHop treats it, and any other undefined value, by the stricter
// rule of each: Expect is dropped as toward an Upstream and TE is dropped
// as toward the client.
const (
	// TowardUpstream is a client request forwarded to an Upstream: Expect
	// is dropped and "TE: trailers" is kept (05 req 28).
	TowardUpstream Direction = iota + 1
	// TowardClient is an Upstream response forwarded to the client (05 req
	// 30).
	TowardClient
)

// ConnectionOptions returns the connection options listed in the
// Connection fields of h, lowercased, in order: the names of the fields
// that are hop-by-hop for this message (RFC 9110 section 7.6.1). Elements
// that are not tokens name no field and are skipped, and an option equal
// to the one before it is listed once. It returns nil when there is none.
// h uses canonical keys, as net/http produces them.
//
// It allocates the slice and one string per option that has an uppercase
// letter, so its cost grows with the Connection value a client sends; it
// serves diagnostics and tests. The request path uses [StripHopByHop] and
// [StripConnectionOptions], which allocate nothing per option.
func ConnectionOptions(h http.Header) []string {
	var out []string
	for _, v := range h["Connection"] {
		for v != "" {
			var opt string
			opt, v = nextElement(v)
			if !IsToken(opt) || (len(out) > 0 && lowerEq(opt, out[len(out)-1])) {
				continue
			}
			out = append(out, strings.ToLower(opt))
		}
	}
	return out
}

// StripHopByHop removes from h every field that must not be forwarded (04
// req 22, 05 req 28 and 30, 07 req 33): the fixed hop-by-hop fields of
// [IsHopByHop] and every field a Connection field names (elements that
// are not tokens name no field, as in [ConnectionOptions]). Toward an
// Upstream it also drops Expect and, when a TE field listed the "trailers"
// member, sets TE to exactly "trailers" (RFC 9110 section 10.1.4; RFC 9113
// section 8.2.2 allows no other TE value). Toward the client it keeps
// Expect; a Direction other than the two defined ones drops both Expect
// and TE.
//
// Keys are matched without regard to ASCII case throughout: the fixed
// fields, Expect, the Connection and TE fields read, and the fields a
// Connection option names. The cost is linear in the size of h, however
// many options a client lists: each option is looked up at most once, in
// canonical form, and never compared with every key. When every key of h
// is canonical, as net/http produces them, nothing is allocated unless
// "TE: trailers" is kept or an option longer than [MaxNameBytes] is looked
// up; a map with a non-canonical key costs one set of the lowercased
// options.
//
// The Connection-named fields are those of h when StripHopByHop runs. A
// client request is therefore passed through [StripConnectionOptions] at
// admission, before the request Phases run, so that a client's
// "Connection: X-Shop-Consumer" cannot remove a field a headers Policy set
// for the Upstream; StripHopByHop at leg build then removes the fixed
// fields, which no Policy may write (07 req 22). An Upstream response is
// stripped before the response Phases run for the same reason. Fields the
// Node adds after the strip (forwarding fields, traceparent) are not
// affected either way.
func StripHopByHop(h http.Header, d Direction) {
	var connBuf [1][]string // a canonical map has at most one Connection key
	conns := connBuf[:0]
	keepTrailers := false
	for k, vs := range h {
		switch {
		case IsHopByHop(k):
			// Read Connection and TE before anything is deleted: a
			// Connection option may name TE.
			if lowerEq(k, "connection") {
				conns = append(conns, vs)
			} else if d == TowardUpstream && lowerEq(k, "te") {
				keepTrailers = keepTrailers || listHas(vs, "trailers")
			}
			delete(h, k)
		case d != TowardClient && lowerEq(k, "expect"):
			delete(h, k)
		}
	}
	stripNamed(h, conns)
	if keepTrailers {
		h["Te"] = []string{"trailers"}
	}
}

// StripConnectionOptions removes from a client request's header h every
// Connection field and every field a Connection option names, except the
// fixed hop-by-hop fields of [IsHopByHop]: those stay for [StripHopByHop]
// at leg build, which removes them and keeps the "trailers" member of a TE
// field that "Connection: TE" named, as RFC 9110 section 10.1.4 requires a
// TE sender to do. The request handler calls it at admission, before the
// request Phases run, so that the client's Connection field cannot remove
// a field a Policy sets for the Upstream (04 req 22, 05 req 28). Keys
// match without regard to ASCII case; the cost and allocations are those
// of StripHopByHop.
func StripConnectionOptions(h http.Header) {
	var connBuf [1][]string
	conns := connBuf[:0]
	for k, vs := range h {
		if lowerEq(k, "connection") {
			conns = append(conns, vs)
			delete(h, k)
		}
	}
	stripNamed(h, conns)
}

// stripNamed deletes from h every field a token element of the Connection
// values conns names, except the fixed hop-by-hop fields: StripHopByHop
// has deleted those already and StripConnectionOptions keeps them.
//
// The first option that needs a lookup triggers one pass over the keys of
// h. When a key is not canonical (see isCanonicalKey) the options go to
// stripNamedFolded. Otherwise each option whose length no key has is
// skipped, and the rest are put in canonical form in a stack buffer and
// looked up, so the cost is linear in the Connection bytes whatever the
// number of fields. An option equal to the one before it is skipped
// without a lookup, and so is a one-byte option looked up before: those
// are the densest a client can send ("a,b,a,b,..."), and there are fewer
// than 128 of them. Neither the lookup nor the delete copies the buffer.
func stripNamed(h http.Header, conns [][]string) {
	var (
		buf     [MaxNameBytes]byte
		key     = buf[:0] // grows onto the heap only for an option over MaxNameBytes
		prev    string
		scanned bool
		lens    uint64    // lenBit of every key length
		seen1   [2]uint64 // canonical bytes of the one-byte options looked up
	)
	for _, vs := range conns {
		for _, v := range vs {
			for v != "" {
				var opt string
				opt, v = nextElement(v)
				if equalFold(opt, prev) || IsHopByHop(opt) {
					continue
				}
				prev = opt
				if !scanned {
					scanned = true
					var canonical bool
					if canonical, lens = scanKeys(h); !canonical {
						stripNamedFolded(h, conns)
						return
					}
				}
				if lens&lenBit(len(opt)) == 0 {
					continue
				}
				var ok bool
				if key, ok = appendCanonicalToken(key[:0], opt); !ok {
					continue
				}
				if len(key) == 1 { // a token byte is below 128
					c := key[0]
					if seen1[c>>6]&(1<<(c&63)) != 0 {
						continue
					}
					seen1[c>>6] |= 1 << (c & 63)
				}
				delete(h, string(key))
			}
		}
	}
}

// scanKeys reports whether every key of h is canonical and returns the
// lenBit union of the key lengths.
func scanKeys(h http.Header) (canonical bool, lens uint64) {
	canonical = true
	for k := range h {
		canonical = canonical && isCanonicalKey(k)
		lens |= lenBit(len(k))
	}
	return canonical, lens
}

// lenBit maps a name length to one bit: bit n for n below 64, bit 0 for 64
// and over (no key or option is empty).
func lenBit(n int) uint64 {
	if n >= 64 {
		return 1
	}
	return 1 << n
}

// stripNamedFolded is stripNamed for a header map with a key that is not
// canonical, which net/http never produces: it collects the lowercased
// options other than the fixed fields in a set and deletes every key whose
// lowercase form is in it. It allocates the set; the cost stays linear in
// the Connection and key bytes.
func stripNamedFolded(h http.Header, conns [][]string) {
	named := make(map[string]struct{})
	for _, vs := range conns {
		for _, v := range vs {
			for v != "" {
				var opt string
				opt, v = nextElement(v)
				if IsToken(opt) && !IsHopByHop(opt) {
					named[strings.ToLower(opt)] = struct{}{}
				}
			}
		}
	}
	if len(named) == 0 {
		return
	}
	var buf [MaxNameBytes]byte
	lower := buf[:0]
	for k := range h {
		lower = appendLower(lower[:0], k)
		if _, ok := named[string(lower)]; ok {
			delete(h, k)
		}
	}
}

// nextElement splits the first element off the comma-separated list v and
// trims the SP and HTAB around it (RFC 9110 section 5.6.1). It scans byte
// by byte: a client can send tens of thousands of one-byte elements, for
// which a call to strings.Cut costs several times the scan.
func nextElement(v string) (elem, rest string) {
	i := 0
	for i < len(v) && v[i] != ',' {
		i++
	}
	if i < len(v) {
		rest = v[i+1:]
	}
	return TrimOWS(v[:i]), rest
}

// listHas reports whether one of the comma-separated lists in values has
// an element equal to name, ignoring ASCII case and surrounding SP or HTAB
// (RFC 9110 section 5.6.1).
func listHas(values []string, name string) bool {
	for _, v := range values {
		for v != "" {
			var elem string
			elem, v = nextElement(v)
			if equalFold(elem, name) {
				return true
			}
		}
	}
	return false
}

// appendCanonicalToken appends s to dst in the form
// textproto.CanonicalMIMEHeaderKey gives an RFC 9110 token: the first
// letter and every letter after "-" uppercase, every other letter
// lowercase. It reports false, with dst unusable, when s is not a token.
func appendCanonicalToken(dst []byte, s string) ([]byte, bool) {
	if s == "" {
		return dst, false
	}
	upper := true
	for i := range len(s) {
		c := s[i]
		if !IsTokenChar(c) {
			return dst, false
		}
		switch {
		case upper && 'a' <= c && c <= 'z':
			c -= 'a' - 'A'
		case !upper && 'A' <= c && c <= 'Z':
			c += 'a' - 'A'
		}
		dst = append(dst, c)
		upper = c == '-'
	}
	return dst, true
}

// isCanonicalKey reports whether the header key k is as net/http stores
// it: a token in the form of appendCanonicalToken, or not a token at all
// (textproto leaves such a key unchanged, and no Connection option, being
// a token, can name it).
func isCanonicalKey(k string) bool {
	canonical, upper := true, true
	for i := range len(k) {
		c := k[i]
		switch {
		case !IsTokenChar(c):
			return true
		case upper && 'a' <= c && c <= 'z', !upper && 'A' <= c && c <= 'Z':
			canonical = false
		}
		upper = c == '-'
	}
	return canonical
}

// appendLower appends s to dst with ASCII letters lowercased.
func appendLower(dst []byte, s string) []byte {
	for i := range len(s) {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	return dst
}
