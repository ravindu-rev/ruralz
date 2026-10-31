// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package revalidate

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/httpfield"
)

// Conditional makes h, the header of a saved request, a revalidation of
// the stored response with header stored (RFC 9111 section 4.3.1). It
// first removes the request's own preconditions (If-Match, If-None-Match,
// If-Modified-Since, If-Unmodified-Since, If-Range) and Range, matching
// keys without regard to case, since only the stored response's
// validators may decide a 304 and a 206 refreshes nothing. It then sends
// the stored entity tag in If-None-Match and the stored Last-Modified in
// If-Modified-Since; with neither, the stored Date goes in
// If-Modified-Since (RFC 9110 section 13.1.3). A date that is not a valid
// HTTP-date is never sent. It reports whether any validator was set: a
// request without one can only get a full response.
func Conditional(h, stored http.Header) bool {
	for k := range h {
		if precondition(k) {
			delete(h, k)
		}
	}
	tag := first(stored, "Etag")
	if tag != "" {
		h["If-None-Match"] = []string{tag}
	}
	if lm := first(stored, "Last-Modified"); lm != "" {
		if _, ok := parseDate(lm); ok {
			h["If-Modified-Since"] = []string{lm}
			return true
		}
	}
	if tag != "" {
		return true
	}
	if date := first(stored, "Date"); date != "" {
		if _, ok := parseDate(date); ok {
			h["If-Modified-Since"] = []string{date}
			return true
		}
	}
	return false
}

// precondition reports a request field Conditional removes.
func precondition(k string) bool {
	switch len(k) {
	case 5:
		return strings.EqualFold(k, "Range")
	case 8:
		return strings.EqualFold(k, "If-Match") || strings.EqualFold(k, "If-Range")
	case 13:
		return strings.EqualFold(k, "If-None-Match")
	case 17:
		return strings.EqualFold(k, "If-Modified-Since")
	case 19:
		return strings.EqualFold(k, "If-Unmodified-Since")
	default:
		return false
	}
}

// first returns the first value of the canonical key k of h without
// surrounding whitespace, or "".
func first(h http.Header, k string) string {
	if vs := h[k]; len(vs) > 0 {
		return httpfield.TrimOWS(vs[0])
	}
	return ""
}

// Freshen returns the stored response header stored updated by nm, the
// header of a 304 received at received, and true; or nil and false when
// nm's validators do not select the stored response. stored and nm are not
// modified.
//
// The 304 selects the stored response (RFC 9111 section 4.3.4) when its
// strong entity tag equals the stored one by strong comparison, its weak
// entity tag equals the stored one by weak comparison or, without an
// entity tag, its Last-Modified equals the stored one. A 304 without
// either validator refreshes the response it answered, since the
// conditional request carried only that response's validators. Entity
// tags or dates that do not parse compare as exact strings.
//
// Every field of nm replaces the stored field of the same name (section
// 3.2) except Content-Length, Content-Range, the hop-by-hop fields, the
// fields nm's Connection names and Proxy-Authentication-Info (section
// 3.1). Age and Date describe the newest response for the age calculation
// (section 4.2.3): the stored Age is removed when nm has none, and a
// missing or invalid Date becomes received (RFC 9110 section 6.6.1).
func Freshen(stored, nm http.Header, received time.Time) (http.Header, bool) {
	if !selects(stored, nm) {
		return nil, false
	}
	out := stored.Clone()
	if out == nil {
		out = http.Header{}
	}
	named := httpfield.ConnectionOptions(nm)
	for k, vs := range nm {
		if updatable(k, named) {
			out[k] = append([]string(nil), vs...)
		}
	}
	if _, ok := nm["Age"]; !ok {
		delete(out, "Age")
	}
	if _, ok := parseDate(first(nm, "Date")); !ok {
		out["Date"] = []string{received.UTC().Format(http.TimeFormat)}
	}
	return out, true
}

// updatable reports whether a 304 field k updates the stored header;
// named are the 304's Connection options.
func updatable(k string, named []string) bool {
	if httpfield.IsHopByHop(k) || strings.EqualFold(k, "Content-Length") ||
		strings.EqualFold(k, "Content-Range") || strings.EqualFold(k, "Proxy-Authentication-Info") {
		return false
	}
	for _, n := range named {
		if strings.EqualFold(k, n) {
			return false
		}
	}
	return true
}

// selects reports whether the 304 header nm selects the stored response
// with header stored (see Freshen).
func selects(stored, nm http.Header) bool {
	if v := first(nm, "Etag"); v != "" {
		s := first(stored, "Etag")
		if s == "" {
			return false
		}
		n, st := parseETag(v), parseETag(s)
		switch {
		case !n.valid || !st.valid:
			return v == s
		case !n.weak:
			return !st.weak && n.opaque == st.opaque
		default:
			return n.opaque == st.opaque
		}
	}
	if v := first(nm, "Last-Modified"); v != "" {
		s := first(stored, "Last-Modified")
		if s == "" {
			return false
		}
		nt, okN := parseDate(v)
		st, okS := parseDate(s)
		if okN && okS {
			return nt.Equal(st)
		}
		return v == s
	}
	return true
}

// entityTag is a parsed entity-tag (RFC 9110 section 8.8.3).
type entityTag struct {
	weak bool
	// opaque is the opaque-tag with its quotes.
	opaque string
	valid  bool
}

// parseETag parses v, which has no surrounding whitespace.
func parseETag(v string) entityTag {
	var e entityTag
	if strings.HasPrefix(v, "W/") {
		e.weak, v = true, v[2:]
	}
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return e
	}
	for i := 1; i < len(v)-1; i++ {
		// etagc = %x21 / %x23-7E / obs-text
		if c := v[i]; c < 0x21 || c == '"' || c == 0x7f {
			return e
		}
	}
	e.opaque, e.valid = v, true
	return e
}

// NotModified reports whether a client's GET or HEAD with header req,
// which a stored response with header stored can serve, is answered 304
// from it (RFC 9111 section 4.3.2): the client validates its own copy
// against the stored one. An If-None-Match decides alone (RFC 9110 section
// 13.1.2): it matches "*" or, by weak comparison, the stored entity tag.
// Without one, a single valid If-Modified-Since (section 13.1.3) matches
// when the stored Last-Modified, else its Date, else received, is no later
// than it. If-Match, If-Unmodified-Since and If-Range never apply to a
// cache, and a member that is not an entity tag matches nothing.
func NotModified(req, stored http.Header, received time.Time) bool {
	if inm, ok := req["If-None-Match"]; ok {
		return noneMatch(inm, first(stored, "Etag"))
	}
	ims := req["If-Modified-Since"]
	if len(ims) != 1 {
		return false
	}
	since, ok := parseDate(ims[0])
	if !ok {
		return false
	}
	modified := received
	if t, ok := parseDate(first(stored, "Last-Modified")); ok {
		modified = t
	} else if t, ok := parseDate(first(stored, "Date")); ok {
		modified = t
	}
	return !modified.After(since)
}

// noneMatch reports whether an If-None-Match field (its lines) lists "*"
// or an entity tag that weakly matches the stored entity tag tag. A stored
// tag that does not parse matches only the same bytes.
func noneMatch(lines []string, tag string) bool {
	st := parseETag(tag)
	for _, v := range lines {
		for v != "" {
			var m string
			m, v = nextTag(v)
			switch {
			case m == "*":
				return true
			case tag == "":
			case !st.valid:
				if m == tag {
					return true
				}
			default:
				if e := parseETag(m); e.valid && e.opaque == st.opaque {
					return true
				}
			}
		}
	}
	return false
}

// nextTag splits the first member off an If-None-Match line: "*", an
// entity tag, whose opaque-tag may hold commas, or the bytes up to the
// next comma. Leading whitespace and empty members are skipped.
func nextTag(v string) (member, rest string) {
	i := skipSeparators(v, 0)
	start := i
	if strings.HasPrefix(v[i:], "W/") {
		i += 2
	}
	if i < len(v) && v[i] == '"' {
		if end := strings.IndexByte(v[i+1:], '"'); end >= 0 {
			i += end + 2
			return v[start:i], v[i:]
		}
	}
	end := strings.IndexByte(v[start:], ',')
	if end < 0 {
		return httpfield.TrimOWS(v[start:]), ""
	}
	return httpfield.TrimOWS(v[start : start+end]), v[start+end:]
}

// NotModifiedHeader returns the header fields of a 304 the cache answers
// from a stored response with header stored (RFC 9110 section 15.4.5):
// its Cache-Control, Content-Location, Date, ETag, Expires and Vary, and
// its Last-Modified when it has no ETag. The cache adds Age and
// Cache-Status as it does for a hit.
func NotModifiedHeader(stored http.Header) http.Header {
	out := http.Header{}
	for _, k := range []string{"Cache-Control", "Content-Location", "Date", "Etag", "Expires", "Vary"} {
		if vs := stored[k]; len(vs) > 0 {
			out[k] = append([]string(nil), vs...)
		}
	}
	if _, ok := stored["Etag"]; !ok {
		if vs := stored["Last-Modified"]; len(vs) > 0 {
			out["Last-Modified"] = append([]string(nil), vs...)
		}
	}
	return out
}

// sameVary reports whether a and b list the same Vary field names,
// ignoring case, order, repetition and whitespace: a 304 that changes them
// would leave the stored variant under names it was not selected by.
func sameVary(a, b http.Header) bool {
	return slices.Equal(varyNames(a), varyNames(b))
}

// varyNames returns the sorted, lowercased, distinct Vary members of h.
func varyNames(h http.Header) []string {
	var names []string
	for _, v := range h["Vary"] {
		for m := range strings.SplitSeq(v, ",") {
			if m = httpfield.TrimOWS(m); m != "" {
				names = append(names, strings.ToLower(m))
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}
