// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/cache/revalidate"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/sfv"
)

// memberName is the Cache-Status member name of the Response Cache
// (RFC 9211): every member it appends is this token with its parameters.
const memberName = "ruralz"

// reply returns the response a stored response with status, header stored
// and body serves at age, its receipt at storedAt, under the client's
// preconditions and Range in cond:
//
//   - a 304 from it when the stored status is 2xx and cond validates the
//     client's own copy (revalidate.NotModified), with the fields of
//     revalidate.NotModifiedHeader and Age, before any Range applies (RFC
//     9110 section 13.2.1 ignores preconditions for other statuses);
//   - for a GET of a stored 200 with one satisfiable bytes range that no
//     failed If-Range voids, a 206 of that part, with Content-Length,
//     Content-Range and Accept-Ranges;
//   - for an unsatisfiable range, when allow416, 416 RZ-RT-026 with
//     Content-Range bytes */length, a generated problem document that
//     carries the Cache-Status member itself, since the cache's onResponse
//     does not run on it; without allow416 the whole response;
//   - otherwise the stored response with Age and its Content-Length.
//
// Every response but the 416 is Cached: onResponse runs every Policy on it.
func (f *Filter) reply(st *state, cond http.Header, status int, stored http.Header, body []byte, age time.Duration, storedAt time.Time, allow416 bool) *filter.Response {
	ageLine := []string{revalidate.FormatAge(age)}
	if status >= http.StatusOK && status < http.StatusMultipleChoices && revalidate.NotModified(cond, stored, storedAt) {
		h := revalidate.NotModifiedHeader(stored)
		h["Age"] = ageLine
		return &filter.Response{Status: http.StatusNotModified, Header: h, Cached: true}
	}
	h := stored.Clone()
	if h == nil {
		h = http.Header{}
	}
	delete(h, "Content-Length")
	h["Age"] = ageLine
	if !st.head && status == http.StatusOK {
		switch first, last, k := byteRange(cond, stored, len(body)); k {
		case rangePart:
			part := body[first : last+1]
			h["Content-Range"] = []string{"bytes " + strconv.Itoa(first) + "-" + strconv.Itoa(last) + "/" + strconv.Itoa(len(body))}
			h["Accept-Ranges"] = []string{"bytes"}
			h["Content-Length"] = []string{strconv.Itoa(len(part))}
			return &filter.Response{Status: http.StatusPartialContent, Header: h, Body: part, Cached: true}
		case rangeUnsatisfiable:
			if allow416 {
				return &filter.Response{Status: http.StatusRequestedRangeNotSatisfiable, Code: CodeRange, Header: http.Header{
					"Content-Range": {"bytes */" + strconv.Itoa(len(body))},
					"Cache-Status":  {member(st)},
				}}
			}
		default:
		}
	}
	if status != http.StatusNoContent {
		h["Content-Length"] = []string{strconv.Itoa(len(body))}
	}
	return &filter.Response{Status: status, Header: h, Body: body, Cached: true}
}

// rangeKind is how a Range field applies to a stored response.
type rangeKind uint8

// Range outcomes.
const (
	// rangeWhole serves the whole response: no Range, several ranges,
	// another unit, an invalid Range or a failed If-Range.
	rangeWhole rangeKind = iota
	// rangePart serves one satisfiable byte range as a 206.
	rangePart
	// rangeUnsatisfiable is a range past the representation: 416.
	rangeUnsatisfiable
)

// byteRange evaluates the Range of a request with header h against a
// stored 200 with header stored and n body bytes (RFC 9110 sections
// 14.1.1, 14.2 and 13.1.5): one bytes range, an int-range or a
// suffix-range, gives the part [first, last]; a first-pos at or past n, or
// a suffix-length of 0, is unsatisfiable; and a non-zero suffix of an
// empty representation serves the whole of it.
func byteRange(h, stored http.Header, n int) (first, last int, k rangeKind) {
	v, count := single(h, "Range")
	if count != 1 || !ifRange(h, stored) {
		return 0, 0, rangeWhole
	}
	unit, spec, ok := strings.Cut(httpfield.TrimOWS(v), "=")
	if !ok || !strings.EqualFold(httpfield.TrimOWS(unit), "bytes") {
		return 0, 0, rangeWhole
	}
	spec = httpfield.TrimOWS(spec)
	if strings.IndexByte(spec, ',') >= 0 {
		return 0, 0, rangeWhole
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, rangeWhole
	}
	if a == "" {
		s, ok := digits(b)
		switch {
		case !ok:
			return 0, 0, rangeWhole
		case s == 0:
			return 0, 0, rangeUnsatisfiable
		case n == 0:
			return 0, 0, rangeWhole
		case clampInt(s) >= n:
			return 0, n - 1, rangePart
		default:
			return n - clampInt(s), n - 1, rangePart
		}
	}
	lo, ok := digits(a)
	if !ok {
		return 0, 0, rangeWhole
	}
	hi := uint64(math.MaxUint64)
	if b != "" {
		if hi, ok = digits(b); !ok || hi < lo {
			return 0, 0, rangeWhole
		}
	}
	if clampInt(lo) >= n {
		return 0, 0, rangeUnsatisfiable
	}
	return clampInt(lo), min(clampInt(hi), n-1), rangePart
}

// clampInt converts a range position to an int, saturating at the
// largest int, which no body length exceeds.
func clampInt(v uint64) int {
	if v > math.MaxInt {
		return math.MaxInt
	}
	return int(v)
}

// digits parses a non-empty run of ASCII digits, saturating at the
// largest uint64.
func digits(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		d := uint64(c - '0')
		if v > (math.MaxUint64-d)/10 {
			v = math.MaxUint64
			continue
		}
		v = v*10 + d
	}
	return v, true
}

// ifRange reports whether the request's If-Range, if any, lets its Range
// apply to the stored response with header stored (RFC 9110 section
// 13.1.5): an entity tag equal to the stored one by strong comparison, or
// an HTTP-date equal to the stored Last-Modified when that is at least 1 s
// before the stored Date. Several If-Range lines never match.
func ifRange(h, stored http.Header) bool {
	v, count := single(h, "If-Range")
	switch {
	case count == 0:
		return true
	case count > 1:
		return false
	}
	v = httpfield.TrimOWS(v)
	if strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "W/") {
		tag, weak, ok := entityTag(v)
		st, sweak, sok := entityTag(firstLine(stored, "Etag"))
		return ok && sok && !weak && !sweak && tag == st
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return false
	}
	lm, err := http.ParseTime(firstLine(stored, "Last-Modified"))
	if err != nil || !t.Equal(lm) {
		return false
	}
	d, err := http.ParseTime(firstLine(stored, "Date"))
	return err == nil && !lm.After(d.Add(-time.Second))
}

// entityTag parses an entity-tag (RFC 9110 section 8.8.3): its
// opaque-tag with quotes and whether it is weak.
func entityTag(v string) (opaque string, weak, ok bool) {
	if strings.HasPrefix(v, "W/") {
		weak, v = true, v[2:]
	}
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return "", false, false
	}
	for i := 1; i < len(v)-1; i++ {
		if c := v[i]; c < 0x21 || c == '"' || c == 0x7f {
			return "", false, false
		}
	}
	return v, weak, true
}

// single returns the first line of the field canonical in h, in any case
// of its name, and the number of its lines.
func single(h http.Header, canonical string) (string, int) {
	vs := h[canonical]
	v, n := "", len(vs)
	if n > 0 {
		v = vs[0]
	}
	for k, o := range h {
		if k != canonical && len(k) == len(canonical) && strings.EqualFold(k, canonical) {
			if n == 0 && len(o) > 0 {
				v = o[0]
			}
			n += len(o)
		}
	}
	return v, n
}

// lines returns the number of lines of the field canonical in h, in any
// case of its name.
func lines(h http.Header, canonical string) int {
	_, n := single(h, canonical)
	return n
}

// firstLine returns the first line of the canonical key k of h without
// surrounding whitespace, or "".
func firstLine(h http.Header, k string) string {
	if vs := h[k]; len(vs) > 0 {
		return httpfield.TrimOWS(vs[0])
	}
	return ""
}

// conditionFields are the request fields a validation replaces: the
// preconditions and Range (RFC 9111 section 4.3.1).
func conditionFields() [6]string {
	return [6]string{"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range", "Range"}
}

// conditions returns a copy of the request's preconditions and Range
// under canonical keys, nil when it has none.
func conditions(h http.Header) http.Header {
	var out http.Header
	for _, name := range conditionFields() {
		for k, vs := range h {
			if len(k) == len(name) && strings.EqualFold(k, name) && len(vs) > 0 {
				if out == nil {
					out = http.Header{}
				}
				out[name] = append(out[name], vs...)
			}
		}
	}
	return out
}

// strip deletes every line of the field canonical from h, in any case of
// its name.
func strip(h http.Header, canonical string) {
	delete(h, canonical)
	for k := range h {
		if len(k) == len(canonical) && strings.EqualFold(k, canonical) {
			delete(h, k)
		}
	}
}

// ttlOf returns the Cache-Status ttl of a response of freshness fr at age:
// the whole seconds of freshness left, rounded down, so a stale response
// has a negative ttl.
func ttlOf(fr revalidate.Freshness, age time.Duration) int64 {
	d := fr.Lifetime - age
	s := int64(d / time.Second)
	if d < 0 && d%time.Second != 0 {
		s--
	}
	return s
}

// member returns st's Cache-Status member (RFC 9211): the token ruralz
// with hit, fwd, fwd-status, ttl, stored and collapsed as they apply,
// never key or detail.
func member(st *state) string {
	var params [6]sfv.Param
	ps := params[:0]
	if st.hit {
		ps = append(ps, sfv.Param{Key: "hit", Value: sfv.Boolean(true)})
	}
	if st.fwd != fwdNone {
		ps = append(ps, sfv.Param{Key: "fwd", Value: sfv.Token(fwdName(st.fwd))})
	}
	if st.fwdStatus != 0 {
		ps = append(ps, sfv.Param{Key: "fwd-status", Value: sfv.Integer(int64(st.fwdStatus))})
	}
	if st.hasTTL {
		ps = append(ps, sfv.Param{Key: "ttl", Value: sfv.Integer(st.ttl)})
	}
	if st.stored {
		ps = append(ps, sfv.Param{Key: "stored", Value: sfv.Boolean(true)})
	}
	if st.collapsed {
		ps = append(ps, sfv.Param{Key: "collapsed", Value: sfv.Boolean(true)})
	}
	var buf [96]byte
	b, err := sfv.AppendMember(buf[:0], sfv.ItemMember(sfv.Token(memberName), ps...))
	if err != nil {
		return memberName
	}
	return string(b)
}
