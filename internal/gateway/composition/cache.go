// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxDeltaSeconds is the largest delta-seconds value kept: a larger one
// reads as 2^31 (RFC 9111 section 1.2.2).
const maxDeltaSeconds = 1 << 31

// delta is one delta-seconds directive value.
type delta struct {
	set bool
	v   int64
}

// directives are the Cache-Control directives a merge reads.
type directives struct {
	noStore, noCache, private, public  bool
	mustRevalidate, proxyRevalidate    bool
	maxAge, sMaxAge, swr, staleIfError delta
}

// cacheFields returns the Cache-Control and Vary fields of a merged
// response from the steps it merges (docs/architecture/03-data-plane.md
// "Merge rules"), now being the time the merged response is made:
//
//   - Cache-Control is no-store when a step has no-store or no explicit
//     freshness (max-age, s-maxage or Expires; RFC 9111 section 4.2.1), or
//     when no step merges. Otherwise private when a step has private or a
//     Set-Cookie field, public only when every step has it and none is
//     private, and no-cache, must-revalidate and proxy-revalidate when a
//     step has them; max-age is the least freshness a step has left (its
//     max-age, else Expires minus Date, less its current age of RFC 9111
//     section 4.2.3, at least 0), and, when a step sets s-maxage, s-maxage
//     the least shared freshness left (s-maxage, then max-age, then
//     Expires); stale-while-revalidate and stale-if-error only when every
//     step sets them, at their least value.
//   - Vary is the union of the steps' field names, compared ignoring case
//     and kept in their first spelling and order; * when a step has it.
func (r *run) cacheFields(merged []int, now time.Time) (cacheControl, vary string) {
	return mergeCacheControl(r, merged, now), mergeVary(r, merged)
}

// mergeCacheControl returns the merged Cache-Control value.
func mergeCacheControl(r *run, merged []int, now time.Time) string {
	if len(merged) == 0 {
		return "no-store"
	}
	var (
		private, noCache, mustRevalidate, proxyRevalidate bool
		public, anyShared                                 = true, false
		maxAge, sMaxAge                                   = int64(-1), int64(-1)
		swr, staleIfError                                 = delta{set: true, v: -1}, delta{set: true, v: -1}
	)
	for _, i := range merged {
		res := &r.res[i]
		d := parseCacheControl(res.header.Values("Cache-Control"))
		expires, hasExpires := res.header["Expires"]
		if d.noStore || (!d.maxAge.set && !d.sMaxAge.set && !hasExpires) {
			return "no-store"
		}
		private = private || d.private || len(res.header.Values("Set-Cookie")) > 0
		public = public && d.public
		noCache = noCache || d.noCache
		mustRevalidate = mustRevalidate || d.mustRevalidate
		proxyRevalidate = proxyRevalidate || d.proxyRevalidate
		date := res.received
		if t, err := http.ParseTime(res.header.Get("Date")); err == nil {
			date = t
		}
		age := currentAge(res, date, now)
		// A step's lifetime from Expires: Expires minus Date, an invalid
		// Expires being already expired (RFC 9111 section 5.3).
		var fromExpires time.Duration
		if hasExpires && len(expires) > 0 {
			if t, err := http.ParseTime(expires[0]); err == nil {
				fromExpires = t.Sub(date)
			}
		}
		lifetime := fromExpires
		if d.maxAge.set {
			lifetime = time.Duration(d.maxAge.v) * time.Second
		}
		maxAge = least(maxAge, left(lifetime, age))
		shared := lifetime
		if d.sMaxAge.set {
			anyShared = true
			shared = time.Duration(d.sMaxAge.v) * time.Second
		}
		sMaxAge = least(sMaxAge, left(shared, age))
		swr = leastAll(swr, d.swr)
		staleIfError = leastAll(staleIfError, d.staleIfError)
	}
	var parts []string
	switch {
	case private:
		parts = append(parts, "private")
	case public:
		parts = append(parts, "public")
	}
	if noCache {
		parts = append(parts, "no-cache")
	}
	if mustRevalidate {
		parts = append(parts, "must-revalidate")
	}
	if proxyRevalidate {
		parts = append(parts, "proxy-revalidate")
	}
	parts = append(parts, "max-age="+strconv.FormatInt(maxAge, 10))
	if anyShared {
		parts = append(parts, "s-maxage="+strconv.FormatInt(sMaxAge, 10))
	}
	if swr.set {
		parts = append(parts, "stale-while-revalidate="+strconv.FormatInt(swr.v, 10))
	}
	if staleIfError.set {
		parts = append(parts, "stale-if-error="+strconv.FormatInt(staleIfError.v, 10))
	}
	return strings.Join(parts, ", ")
}

// currentAge is a step response's current age (RFC 9111 section 4.2.3):
// the larger of its apparent age and its Age value plus the response
// delay, plus the time it has resided since it arrived.
func currentAge(res *result, date, now time.Time) time.Duration {
	apparent := max(res.received.Sub(date), 0)
	delay := max(res.received.Sub(res.sent), 0)
	var ageValue time.Duration
	if lines := res.header.Values("Age"); len(lines) > 0 {
		if v, ok := parseDelta(strings.TrimSpace(lines[0])); ok {
			ageValue = time.Duration(v) * time.Second
		}
	}
	return max(apparent, ageValue+delay) + max(now.Sub(res.received), 0)
}

// left returns the whole seconds of freshness a lifetime leaves past age,
// at least 0.
func left(lifetime, age time.Duration) int64 {
	return int64(max(lifetime-age, 0) / time.Second)
}

// least returns the smaller of acc and v, acc being -1 before any value.
func least(acc, v int64) int64 {
	if acc < 0 || v < acc {
		return v
	}
	return acc
}

// leastAll folds one step's directive into acc: acc stays set only while
// every step sets the directive, at the least value (v is -1 before any
// step).
func leastAll(acc, d delta) delta {
	if !acc.set || !d.set {
		return delta{}
	}
	if acc.v < 0 || d.v < acc.v {
		return delta{set: true, v: d.v}
	}
	return acc
}

// parseCacheControl reads the directives of a Cache-Control field's lines:
// names ignore case, a value may be quoted, and of a directive given twice
// the first value counts (RFC 9111 section 4.2.1). A max-age or s-maxage
// whose value is not delta-seconds reads as 0, a stale response; such a
// stale-while-revalidate or stale-if-error is ignored.
func parseCacheControl(lines []string) directives {
	var d directives
	for _, line := range lines {
		for line != "" {
			var item string
			item, line = nextDirective(line)
			name, value, _ := strings.Cut(item, "=")
			name = strings.ToLower(strings.TrimSpace(name))
			value = unquote(strings.TrimSpace(value))
			switch name {
			case "no-store":
				d.noStore = true
			case "no-cache":
				d.noCache = true
			case "private":
				d.private = true
			case "public":
				d.public = true
			case "must-revalidate":
				d.mustRevalidate = true
			case "proxy-revalidate":
				d.proxyRevalidate = true
			case "max-age":
				d.maxAge = first(d.maxAge, value, true)
			case "s-maxage":
				d.sMaxAge = first(d.sMaxAge, value, true)
			case "stale-while-revalidate":
				d.swr = first(d.swr, value, false)
			case "stale-if-error":
				d.staleIfError = first(d.staleIfError, value, false)
			}
		}
	}
	return d
}

// first returns cur when it is set, else value read as delta-seconds: an
// invalid value is 0 with zeroInvalid, else unset.
func first(cur delta, value string, zeroInvalid bool) delta {
	if cur.set {
		return cur
	}
	if v, ok := parseDelta(value); ok {
		return delta{set: true, v: v}
	}
	if zeroInvalid {
		return delta{set: true}
	}
	return delta{}
}

// parseDelta reads delta-seconds: one or more digits, a value above 2^31
// read as 2^31.
func parseDelta(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v > maxDeltaSeconds {
		return maxDeltaSeconds, true
	}
	return v, true
}

// nextDirective splits the first directive off a Cache-Control line at a
// comma outside a quoted string.
func nextDirective(line string) (item, rest string) {
	quoted := false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\\' && quoted:
			i++
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			return line[:i], line[i+1:]
		}
	}
	return line, ""
}

// unquote returns a quoted-string's content with its escapes removed, or
// s itself when it is not quoted.
func unquote(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	s = s[1 : len(s)-1]
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// mergeVary returns the merged Vary value: the union of the steps' field
// names compared ignoring case, in their first spelling and order, or *
// when a step has it; "" when none has one.
func mergeVary(r *run, merged []int) string {
	var names []string
	for _, i := range merged {
		for _, line := range r.res[i].header.Values("Vary") {
			for name := range strings.SplitSeq(line, ",") {
				name = strings.TrimSpace(name)
				switch {
				case name == "":
				case name == "*":
					return "*"
				case !containsFold(names, name):
					names = append(names, name)
				}
			}
		}
	}
	return strings.Join(names, ", ")
}

// containsFold reports whether names holds name, ignoring case.
func containsFold(names []string, name string) bool {
	for _, n := range names {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}
