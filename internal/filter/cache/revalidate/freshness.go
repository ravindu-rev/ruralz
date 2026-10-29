// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package revalidate

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/httpfield"
)

// Freshness caps ("Response caching", Freshness row; target).
const (
	// MaxFreshness caps a freshness lifetime.
	MaxFreshness = 24 * time.Hour
	// MaxStaleWindow caps each of stale-while-revalidate and
	// stale-if-error.
	MaxStaleWindow = time.Hour
)

// Freshness is the freshness model of a stored response: its lifetime
// and the RFC 5861 windows that follow it.
type Freshness struct {
	// Lifetime is the freshness lifetime (RFC 9111 section 4.2.1), at most
	// MaxFreshness; zero for an invalid or conflicting value, which RFC
	// 9111 treats as stale.
	Lifetime time.Duration
	// Explicit reports that s-maxage, max-age or Expires gave the lifetime
	// (the Storable rule's "explicit freshness"); an invalid s-maxage or
	// max-age, which leaves the response stale, is not explicit.
	Explicit bool
	// StaleWhileRevalidate is how long after Lifetime the response may be
	// served stale while a revalidation runs, at most MaxStaleWindow.
	StaleWhileRevalidate time.Duration
	// StaleIfError is how long after Lifetime the response may replace an
	// error, at most MaxStaleWindow.
	StaleIfError time.Duration
	// StaleForbidden reports must-revalidate, proxy-revalidate or
	// s-maxage: both windows are then zero.
	StaleForbidden bool
}

// FreshnessOf returns the freshness of a response with header h received
// at received. For a shared cache, s-maxage takes precedence over
// max-age, and either makes Expires ignored; Expires counts from Date, or
// from received without a valid Date (RFC 9110 section 6.6.1). An invalid
// Expires, such as "0", is already expired, and Expires lines that
// disagree are treated as stale (RFC 9111 sections 4.2.1 and 5.3).
func FreshnessOf(h http.Header, received time.Time) Freshness {
	d := Parse(h)
	var f Freshness
	switch {
	case d.SMaxAge.Present:
		f.Lifetime = d.SMaxAge.Value
	case d.MaxAge.Present:
		f.Lifetime = d.MaxAge.Value
	case len(h["Expires"]) > 0:
		f.Lifetime = expiresLifetime(h, received)
	}
	f.Explicit = explicit(&d, h)
	f.Lifetime = min(max(f.Lifetime, 0), MaxFreshness)
	f.StaleForbidden = d.MustRevalidate || d.ProxyRevalidate || d.SMaxAge.Present
	if !f.StaleForbidden {
		f.StaleWhileRevalidate = window(d.StaleWhileRevalidate)
		f.StaleIfError = window(d.StaleIfError)
	}
	return f
}

// explicit reports whether d and h give an explicit freshness lifetime: a
// valid s-maxage, else a valid max-age, else an Expires field (an invalid
// date there still means "already expired").
func explicit(d *Directives, h http.Header) bool {
	switch {
	case d.SMaxAge.Present:
		return d.SMaxAge.Valid
	case d.MaxAge.Present:
		return d.MaxAge.Valid
	default:
		return len(h["Expires"]) > 0
	}
}

// window returns a valid stale window capped at MaxStaleWindow; an
// invalid one allows no stale serving.
func window(x Delta) time.Duration {
	if !x.Valid {
		return 0
	}
	return min(x.Value, MaxStaleWindow)
}

// expiresLifetime returns Expires minus the date value of h.
func expiresLifetime(h http.Header, received time.Time) time.Duration {
	vs := h["Expires"]
	for _, v := range vs[1:] {
		if strings.TrimSpace(v) != strings.TrimSpace(vs[0]) {
			return 0
		}
	}
	exp, ok := parseDate(vs[0])
	if !ok {
		return 0
	}
	return exp.Sub(dateValue(h, received))
}

// parseDate parses an HTTP-date in any of its three formats.
func parseDate(v string) (time.Time, bool) {
	t, err := http.ParseTime(strings.TrimSpace(v))
	return t, err == nil
}

// dateValue returns the Date of h, or received when h has no valid Date.
func dateValue(h http.Header, received time.Time) time.Time {
	if vs := h["Date"]; len(vs) > 0 {
		if t, ok := parseDate(vs[0]); ok {
			return t
		}
	}
	return received
}

// ageValue returns the Age field of h; 0 when absent or not delta-seconds
// (RFC 9111 section 4.2.3).
func ageValue(h http.Header) time.Duration {
	vs := h["Age"]
	if len(vs) == 0 {
		return 0
	}
	v, _ := parseDelta(httpfield.TrimOWS(vs[0]))
	return v
}

// InitialAge returns the corrected initial age of a response with header
// h, requested at requested and received at received (RFC 9111 section
// 4.2.3): the larger of its apparent age, received minus Date, and its Age
// field plus the response delay.
func InitialAge(h http.Header, requested, received time.Time) time.Duration {
	apparent := max(received.Sub(dateValue(h, received)), 0)
	delay := max(received.Sub(requested), 0)
	return max(apparent, ageValue(h)+delay)
}

// CurrentAge returns the age at now of a response with corrected initial
// age initial received at received: initial plus the time it has resided
// in the cache, which never counts negative when a clock goes back.
func CurrentAge(initial time.Duration, received, now time.Time) time.Duration {
	return initial + max(now.Sub(received), 0)
}

// FormatAge returns the Age field value of a response of age age: whole
// seconds, at most 2^31 (RFC 9111 section 5.1).
func FormatAge(age time.Duration) string {
	s := min(max(age, 0), MaxDelta) / time.Second
	return strconv.FormatInt(int64(s), 10)
}

// Use is how a stored response may serve a request.
type Use uint8

// Uses.
const (
	// UseFresh serves the response as a hit.
	UseFresh Use = iota + 1
	// UseStale serves the stale response at once, inside
	// stale-while-revalidate or the request's max-stale, and queues a
	// background revalidation.
	UseStale
	// UseValidate takes the Upstream path: the stored response serves
	// only after a 304 answers its conditional request, or under
	// stale-if-error (ServesOnError).
	UseValidate
)

// String returns the name of u.
func (u Use) String() string {
	switch u {
	case UseFresh:
		return "fresh"
	case UseStale:
		return "stale"
	case UseValidate:
		return "validate"
	default:
		return "unknown"
	}
}

// Use returns how the stored response of freshness f and current age age
// serves a request with directives req (nil for none), in order:
//
//  1. UseValidate when req revalidates (no-cache or max-age=0), or when
//     age is over the request's max-age (RFC 9111 section 5.2.1.1).
//  2. UseFresh while age plus the request's min-fresh is under the
//     lifetime (section 5.2.1.3).
//  3. UseValidate when stale is forbidden (must-revalidate,
//     proxy-revalidate, s-maxage), whatever the request accepts.
//  4. With a request max-stale, UseStale while the staleness, age minus
//     the lifetime, is at most its value (section 5.2.1.2), else
//     UseValidate.
//  5. UseValidate when the request sent max-age or min-fresh without
//     max-stale: such a client does not wish to receive a stale response.
//  6. UseStale while age is under the lifetime plus
//     stale-while-revalidate, else UseValidate.
func (f Freshness) Use(age time.Duration, req *Directives) Use {
	var none Directives
	if req == nil {
		req = &none
	}
	if req.Revalidates() || (req.MaxAge.Valid && age > req.MaxAge.Value) {
		return UseValidate
	}
	var minFresh time.Duration
	if req.MinFresh.Valid {
		minFresh = req.MinFresh.Value
	}
	switch {
	case age+minFresh < f.Lifetime:
		return UseFresh
	case f.StaleForbidden:
		return UseValidate
	case req.MaxStale.Valid:
		if age-f.Lifetime <= req.MaxStale.Value {
			return UseStale
		}
		return UseValidate
	case req.MaxAge.Valid || req.MinFresh.Valid:
		return UseValidate
	case age < f.Lifetime+f.StaleWhileRevalidate:
		return UseStale
	default:
		return UseValidate
	}
}

// ServesOnError reports whether the stored response of current age age may
// replace an error on the Upstream path (IsError) of a request with
// directives req (nil for none): while fresh, or stale within the larger of
// the response's stale-if-error and the request's own, at most
// MaxStaleWindow (RFC 5861 section 4), none of which applies when stale is
// forbidden ("Failure matrix": also while the breaker is open). The
// request's other directives do not change it.
func (f Freshness) ServesOnError(age time.Duration, req *Directives) bool {
	window := f.StaleIfError
	if !f.StaleForbidden && req != nil && req.StaleIfError.Valid {
		window = max(window, min(req.StaleIfError.Value, MaxStaleWindow))
	}
	return age < f.Lifetime+window
}

// TTL returns how long a stored response of current age age stays useful,
// the State Store TTL of a store or refresh: the lifetime plus the larger
// stale window, minus age. A result of zero or less leaves nothing to
// keep.
func (f Freshness) TTL(age time.Duration) time.Duration {
	return f.Lifetime + max(f.StaleWhileRevalidate, f.StaleIfError) - age
}

// IsError reports whether status is an error that stale-if-error covers:
// 500, 502, 503 or 504 (RFC 5861 section 4), whether the Upstream sent it
// or the Node generated it for a failed leg.
func IsError(status int) bool {
	switch status {
	case http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// Storable reports whether a response with header h may be stored, by the
// header part of the "Storable" rule: explicit freshness; no no-store,
// no-cache (in either form, never stored at M1) or private; no Vary
// member "*"; no Set-Cookie; and, when the request carried Authorization
// (authorized), public, s-maxage or must-revalidate (RFC 9111 section
// 3.5). Method and status rules stay with the caller.
func Storable(h http.Header, authorized bool) bool {
	d := Parse(h)
	switch {
	case d.NoStore, d.NoCache, d.Private:
		return false
	case len(h["Set-Cookie"]) > 0, varyAll(h):
		return false
	case authorized && !d.Public && !d.SMaxAge.Present && !d.MustRevalidate:
		return false
	default:
		return explicit(&d, h)
	}
}

// varyAll reports a Vary member "*".
func varyAll(h http.Header) bool {
	for _, v := range h["Vary"] {
		for m := range strings.SplitSeq(v, ",") {
			if httpfield.TrimOWS(m) == "*" {
				return true
			}
		}
	}
	return false
}
