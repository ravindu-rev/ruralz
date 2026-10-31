// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwks

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Key set lifetimes (Security and identity, "JWT and OIDC": "Keys live
// for max-age clamped to 5 minutes to 6 hours, or 1 hour when absent or
// no-cache (target), refresh at half-life").
const (
	// MinLifetime is the shortest lifetime of a key set.
	MinLifetime = 5 * time.Minute
	// MaxLifetime is the longest lifetime of a key set.
	MaxLifetime = 6 * time.Hour
	// DefaultLifetime applies without max-age or with no-cache.
	DefaultLifetime = time.Hour
)

// MaxRetryAfter bounds how long a Retry-After defers an issuer's next
// fetch: 6 hours (target), the longest a key set lives, so a response
// cannot stop an issuer's refreshes for longer than its keys would last.
const MaxRetryAfter = MaxLifetime

// Lifetime returns how long the key set of a JWKS response lives: the
// first valid Cache-Control max-age clamped to MinLifetime..MaxLifetime;
// DefaultLifetime when the response carries no valid max-age or carries
// no-cache (with or without field names). Directive names are matched
// case-insensitively and a quoted max-age is accepted (RFC 9111 section
// 5.2).
func Lifetime(h http.Header) time.Duration {
	maxAge, noCache := cacheControl(h.Values("Cache-Control"))
	if noCache || maxAge < 0 {
		return DefaultLifetime
	}
	if maxAge > int64(MaxLifetime/time.Second) {
		return MaxLifetime
	}
	return max(time.Duration(maxAge)*time.Second, MinLifetime)
}

// cacheControl returns the first valid max-age in seconds (-1 when none)
// and whether no-cache is present.
func cacheControl(lines []string) (maxAge int64, noCache bool) {
	maxAge = -1
	for _, line := range lines {
		for d := range strings.SplitSeq(line, ",") {
			name, arg, _ := strings.Cut(strings.TrimSpace(d), "=")
			name = strings.TrimSpace(name)
			switch {
			case strings.EqualFold(name, "no-cache"):
				noCache = true
			case strings.EqualFold(name, "max-age") && maxAge < 0:
				maxAge = deltaSeconds(arg)
			}
		}
	}
	return maxAge, noCache
}

// deltaSeconds parses delta-seconds (RFC 9111 section 1.2.2), optionally
// quoted; -1 when invalid. A value past the int64 range saturates.
func deltaSeconds(arg string) int64 {
	arg = strings.TrimSpace(arg)
	if len(arg) >= 2 && arg[0] == '"' && arg[len(arg)-1] == '"' {
		arg = arg[1 : len(arg)-1]
	}
	if arg == "" {
		return -1
	}
	for i := range len(arg) {
		if arg[i] < '0' || arg[i] > '9' {
			return -1
		}
	}
	n, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		// Only all-digit values reach here: the error is a range error.
		return 1<<63 - 1
	}
	return n
}

// retryAfter returns the time a response's Retry-After (RFC 9110 section
// 10.2.3: delay-seconds or an HTTP-date) defers the next fetch to, at most
// MaxRetryAfter after now; the zero time when the field is absent, invalid
// or not in the future.
func retryAfter(h http.Header, now time.Time) time.Time {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return time.Time{}
	}
	var d time.Duration
	if s := deltaSeconds(v); s >= 0 {
		if s > int64(MaxRetryAfter/time.Second) {
			return now.Add(MaxRetryAfter)
		}
		d = time.Duration(s) * time.Second
	} else {
		at, err := http.ParseTime(v)
		if err != nil {
			return time.Time{}
		}
		d = at.Sub(now)
	}
	if d <= 0 {
		return time.Time{}
	}
	return now.Add(min(d, MaxRetryAfter))
}
