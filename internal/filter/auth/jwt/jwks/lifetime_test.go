// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwks

import (
	"net/http"
	"testing"
	"time"
)

// TestLifetime checks "Keys live for max-age clamped to 5 minutes to 6
// hours, or 1 hour when absent or no-cache (target)".
func TestLifetime(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  time.Duration
	}{
		{"absent", nil, DefaultLifetime},
		{"max-age", []string{"max-age=600"}, 10 * time.Minute},
		{"with other directives", []string{"public, max-age=3600, must-revalidate"}, time.Hour},
		{"below the floor", []string{"max-age=60"}, MinLifetime},
		{"zero", []string{"max-age=0"}, MinLifetime},
		{"above the ceiling", []string{"max-age=86400"}, MaxLifetime},
		{"past int64", []string{"max-age=99999999999999999999999"}, MaxLifetime},
		{"no-cache", []string{"no-cache"}, DefaultLifetime},
		{"no-cache wins over max-age", []string{"max-age=600, no-cache"}, DefaultLifetime},
		{"qualified no-cache", []string{`no-cache="Set-Cookie", max-age=600`}, DefaultLifetime},
		{"no-cache on another line", []string{"max-age=600", "No-Cache"}, DefaultLifetime},
		{"case-insensitive name", []string{"MAX-AGE=900"}, 15 * time.Minute},
		{"quoted", []string{`max-age="900"`}, 15 * time.Minute},
		{"spaces around", []string{" max-age = 900 "}, 15 * time.Minute},
		{"invalid value", []string{"max-age=abc"}, DefaultLifetime},
		{"empty value", []string{"max-age="}, DefaultLifetime},
		{"negative", []string{"max-age=-5"}, DefaultLifetime},
		{"first valid wins", []string{"max-age=x, max-age=600", "max-age=900"}, 10 * time.Minute},
		{"s-maxage ignored", []string{"s-maxage=600"}, DefaultLifetime},
		{"no-store only", []string{"no-store"}, DefaultLifetime},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for _, l := range tt.lines {
				h.Add("Cache-Control", l)
			}
			if got := Lifetime(h); got != tt.want {
				t.Errorf("Lifetime(%q) = %v, want %v", tt.lines, got, tt.want)
			}
		})
	}
}

// TestRetryAfter checks that a Retry-After in delay-seconds or as an
// HTTP-date defers the next fetch, at most MaxRetryAfter, and that an
// absent, invalid or past value defers nothing.
func TestRetryAfter(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Time
	}{
		{"absent", "", time.Time{}},
		{"seconds", "120", t0.Add(2 * time.Minute)},
		{"zero", "0", time.Time{}},
		{"date", t0.Add(10 * time.Minute).Format(http.TimeFormat), t0.Add(10 * time.Minute)},
		{"past date", t0.Add(-time.Minute).Format(http.TimeFormat), time.Time{}},
		{"invalid", "soon", time.Time{}},
		{"negative", "-30", time.Time{}},
		{"over the bound", "999999", t0.Add(MaxRetryAfter)},
		{"date over the bound", t0.Add(30 * time.Hour).Format(http.TimeFormat), t0.Add(MaxRetryAfter)},
		{"past int64", "99999999999999999999999", t0.Add(MaxRetryAfter)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.value != "" {
				h.Set("Retry-After", tt.value)
			}
			if got := retryAfter(h, t0); !got.Equal(tt.want) {
				t.Errorf("retryAfter(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
