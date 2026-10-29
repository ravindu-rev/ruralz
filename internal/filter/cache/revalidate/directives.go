// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package revalidate

import (
	"net/http"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/httpfield"
)

// MaxDelta is the largest delta-seconds value: a larger argument reads as
// 2^31 s, as RFC 9111 section 1.2.2 requires of a cache.
const MaxDelta = (1 << 31) * time.Second

// Delta is the argument of a delta-seconds directive (max-age, s-maxage,
// stale-while-revalidate, stale-if-error).
type Delta struct {
	// Present reports that the directive appeared at least once.
	Present bool
	// Valid reports that every occurrence carried the same delta-seconds
	// argument, as a token or a quoted-string (RFC 9111 section 5.2).
	// Occurrences that disagree, or a missing, empty, signed or non-digit
	// argument, make the directive invalid: RFC 9111 section 4.2.1 treats
	// such freshness information as stale.
	Valid bool
	// Value is the argument, at most MaxDelta; zero unless Valid.
	Value time.Duration
}

// add records one occurrence: the first sets the value, a later one that
// differs or is invalid makes the directive invalid.
func (x *Delta) add(v time.Duration, ok bool) {
	switch {
	case !x.Present:
		x.Present, x.Valid = true, ok
		if ok {
			x.Value = v
		}
	case !ok || !x.Valid || x.Value != v:
		x.Valid, x.Value = false, 0
	}
}

// Directives are the Cache-Control directives the Response Cache reads,
// from a response or a request (RFC 9111 section 5.2, RFC 5861). Names
// compare without regard to case and unknown directives are ignored. A
// no-cache or private directive with a field-name argument counts as the
// plain directive: neither form is stored at M1.
type Directives struct {
	// NoStore, NoCache, Private and Public are the response directives of
	// the Storable rule; NoStore and NoCache are also request directives.
	NoStore, NoCache, Private, Public bool
	// MustRevalidate and ProxyRevalidate forbid serving stale.
	MustRevalidate, ProxyRevalidate bool
	// OnlyIfCached is the request directive answered with 504 on a miss.
	OnlyIfCached bool
	// MaxAge and SMaxAge give the freshness lifetime (SMaxAge also forbids
	// stale); MaxAge is also a request directive bounding the age the
	// client accepts (RFC 9111 section 5.2.1.1).
	MaxAge, SMaxAge Delta
	// StaleWhileRevalidate and StaleIfError are the RFC 5861 windows;
	// StaleIfError is also the request extension widening stale-if-error
	// for that request (RFC 5861 section 4).
	StaleWhileRevalidate, StaleIfError Delta
	// MinFresh and MaxStale are request directives: the freshness the
	// client wants left, and the staleness it accepts (RFC 9111 sections
	// 5.2.1.3 and 5.2.1.2). A max-stale without an argument accepts any
	// staleness and reads as MaxDelta.
	MinFresh, MaxStale Delta
}

// Parse returns the directives of every Cache-Control field line of h,
// which has canonical keys as net/http produces them. Empty list elements
// are skipped, optional whitespace is accepted around "=" and commas
// inside a quoted-string do not split elements.
func Parse(h http.Header) Directives {
	var d Directives
	for _, v := range h["Cache-Control"] {
		for v != "" {
			var e element
			e, v = nextElement(v)
			d.set(&e)
		}
	}
	return d
}

// Revalidates reports whether request directives d ask that no stored
// response serve without validation at the Upstream: no-cache, or a valid
// max-age=0 ("Request directives"); the other request directives act
// through Freshness.Use and Freshness.ServesOnError.
func (d *Directives) Revalidates() bool {
	return d.NoCache || (d.MaxAge.Valid && d.MaxAge.Value == 0)
}

// element is one list element of a Cache-Control field line.
type element struct {
	name string
	arg  string
	// hasArg reports an "=" after the name.
	hasArg bool
	// malformed reports bytes after the element before the next comma, or
	// an unterminated quoted-string.
	malformed bool
}

// set applies e. A malformed element still sets a restrictive boolean
// directive, the most restrictive reading (RFC 9111 section 4.2.1), but
// never public, and makes a delta-seconds directive invalid.
func (d *Directives) set(e *element) {
	n := e.name
	switch {
	case strings.EqualFold(n, "no-store"):
		d.NoStore = true
	case strings.EqualFold(n, "no-cache"):
		d.NoCache = true
	case strings.EqualFold(n, "private"):
		d.Private = true
	case strings.EqualFold(n, "public"):
		d.Public = d.Public || !e.malformed
	case strings.EqualFold(n, "must-revalidate"):
		d.MustRevalidate = true
	case strings.EqualFold(n, "proxy-revalidate"):
		d.ProxyRevalidate = true
	case strings.EqualFold(n, "only-if-cached"):
		d.OnlyIfCached = true
	case strings.EqualFold(n, "max-age"):
		d.MaxAge.add(e.delta())
	case strings.EqualFold(n, "s-maxage"):
		d.SMaxAge.add(e.delta())
	case strings.EqualFold(n, "stale-while-revalidate"):
		d.StaleWhileRevalidate.add(e.delta())
	case strings.EqualFold(n, "stale-if-error"):
		d.StaleIfError.add(e.delta())
	case strings.EqualFold(n, "min-fresh"):
		d.MinFresh.add(e.delta())
	case strings.EqualFold(n, "max-stale"):
		if !e.hasArg && !e.malformed {
			d.MaxStale.add(MaxDelta, true)
		} else {
			d.MaxStale.add(e.delta())
		}
	}
}

// delta returns e's argument as delta-seconds; ok is false for a
// malformed element or a missing argument.
func (e *element) delta() (time.Duration, bool) {
	if e.malformed || !e.hasArg {
		return 0, false
	}
	return parseDelta(e.arg)
}

// parseDelta parses delta-seconds (1*DIGIT), saturating at MaxDelta; ok is
// false for an empty, signed or non-digit value.
func parseDelta(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	const limit = int64(MaxDelta / time.Second)
	var n int64
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if n < limit {
			n = min(n*10+int64(c-'0'), limit)
		}
	}
	return time.Duration(n) * time.Second, true
}

// nextElement splits the first non-empty list element off v.
func nextElement(v string) (element, string) {
	i := skipSeparators(v, 0)
	start := i
	for i < len(v) && httpfield.IsTokenChar(v[i]) {
		i++
	}
	e := element{name: v[start:i]}
	if e.name == "" {
		// Not a token: the element names no directive.
		e.malformed = true
		return e, v[skipElement(v, i):]
	}
	j := skipOWS(v, i)
	if j < len(v) && v[j] == '=' {
		e.hasArg = true
		j = skipOWS(v, j+1)
		if j < len(v) && v[j] == '"' {
			var ok bool
			e.arg, j, ok = quotedString(v, j)
			e.malformed = !ok
		} else {
			s := j
			for j < len(v) && httpfield.IsTokenChar(v[j]) {
				j++
			}
			e.arg = v[s:j]
		}
	}
	j = skipOWS(v, j)
	if j < len(v) && v[j] != ',' {
		e.malformed = true
		j = skipElement(v, j)
	}
	return e, v[j:]
}

// skipSeparators skips optional whitespace and empty list elements from i.
func skipSeparators(v string, i int) int {
	for i < len(v) && (v[i] == ',' || v[i] == ' ' || v[i] == '\t') {
		i++
	}
	return i
}

// skipOWS skips spaces and horizontal tabs from i.
func skipOWS(v string, i int) int {
	for i < len(v) && (v[i] == ' ' || v[i] == '\t') {
		i++
	}
	return i
}

// skipElement returns the index of the next comma at or after i that is
// outside a quoted-string, or len(v).
func skipElement(v string, i int) int {
	for i < len(v) && v[i] != ',' {
		if v[i] == '"' {
			_, i, _ = quotedString(v, i)
			continue
		}
		i++
	}
	return i
}

// quotedString reads the quoted-string starting at v[i] == '"' and returns
// its unescaped content and the index after the closing quote. ok is false
// for an unterminated string, which then runs to the end of v.
func quotedString(v string, i int) (string, int, bool) {
	i++ // the opening quote
	start := i
	var b []byte // nil until a quoted-pair needs unescaping
	for i < len(v) {
		switch c := v[i]; c {
		case '"':
			if b == nil {
				return v[start:i], i + 1, true
			}
			return string(b), i + 1, true
		case '\\':
			if b == nil {
				b = append(make([]byte, 0, len(v)-start), v[start:i]...)
			}
			if i+1 < len(v) {
				i++
			}
			b = append(b, v[i])
		default:
			if b != nil {
				b = append(b, c)
			}
		}
		i++
	}
	return "", len(v), false
}
