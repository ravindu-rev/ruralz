// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package revalidate

import (
	"net/http"
	"testing"
	"time"
)

// cc returns a header with one Cache-Control field line per value.
func cc(lines ...string) http.Header {
	return http.Header{"Cache-Control": lines}
}

// valid returns a valid Delta of s seconds.
func valid(s int64) Delta {
	return Delta{Present: true, Valid: true, Value: time.Duration(s) * time.Second}
}

// invalid is a present Delta whose argument was invalid or conflicting.
var invalid = Delta{Present: true}

// TestParse checks the Cache-Control grammar of RFC 9111 section 5.2: the
// directives the cache reads, both argument forms, and the readings of
// invalid, conflicting and malformed elements.
func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  Directives
	}{
		{name: "no field", want: Directives{}},
		{
			name:  "boolean directives",
			lines: []string{"no-store, no-cache, private, public, must-revalidate, proxy-revalidate, only-if-cached"},
			want: Directives{
				NoStore: true, NoCache: true, Private: true, Public: true,
				MustRevalidate: true, ProxyRevalidate: true, OnlyIfCached: true,
			},
		},
		{
			name:  "names ignore case",
			lines: []string{"No-Store, MAX-AGE=60, S-MaxAge=5"},
			want:  Directives{NoStore: true, MaxAge: valid(60), SMaxAge: valid(5)},
		},
		{
			name:  "delta directives",
			lines: []string{"max-age=60, s-maxage=30, stale-while-revalidate=10, stale-if-error=20"},
			want: Directives{
				MaxAge: valid(60), SMaxAge: valid(30),
				StaleWhileRevalidate: valid(10), StaleIfError: valid(20),
			},
		},
		{name: "quoted argument", lines: []string{`max-age="60"`}, want: Directives{MaxAge: valid(60)}},
		{name: "missing argument", lines: []string{"max-age"}, want: Directives{MaxAge: invalid}},
		{name: "empty argument", lines: []string{"max-age="}, want: Directives{MaxAge: invalid}},
		{name: "empty quoted argument", lines: []string{`max-age=""`}, want: Directives{MaxAge: invalid}},
		{name: "negative argument", lines: []string{"max-age=-1"}, want: Directives{MaxAge: invalid}},
		{name: "signed argument", lines: []string{"max-age=+5"}, want: Directives{MaxAge: invalid}},
		{name: "fractional argument", lines: []string{"stale-if-error=1.5"}, want: Directives{StaleIfError: invalid}},
		{
			name:  "overflow saturates at 2^31",
			lines: []string{"max-age=99999999999999999999999"},
			want:  Directives{MaxAge: Delta{Present: true, Valid: true, Value: MaxDelta}},
		},
		{name: "equal duplicates", lines: []string{"max-age=60, max-age=60"}, want: Directives{MaxAge: valid(60)}},
		{name: "conflicting duplicates", lines: []string{"max-age=60, max-age=30"}, want: Directives{MaxAge: invalid}},
		{name: "invalid then valid", lines: []string{"max-age=x, max-age=30"}, want: Directives{MaxAge: invalid}},
		{name: "conflict across lines", lines: []string{"max-age=60", "max-age=30"}, want: Directives{MaxAge: invalid}},
		{name: "lines combine", lines: []string{"no-store", "max-age=5"}, want: Directives{NoStore: true, MaxAge: valid(5)}},
		{name: "empty elements", lines: []string{", ,max-age=5,, ,"}, want: Directives{MaxAge: valid(5)}},
		{name: "whitespace around equals", lines: []string{"max-age = 60 ,\tpublic"}, want: Directives{MaxAge: valid(60), Public: true}},
		{
			name:  "comma inside a quoted-string",
			lines: []string{`no-cache="Set-Cookie, X-Shop", max-age=5`},
			want:  Directives{NoCache: true, MaxAge: valid(5)},
		},
		{
			name:  "escaped quote inside a quoted-string",
			lines: []string{`private="a\"b, c", max-age=7`},
			want:  Directives{Private: true, MaxAge: valid(7)},
		},
		{name: "qualified private", lines: []string{`private="Set-Cookie"`}, want: Directives{Private: true}},
		{name: "unknown directives", lines: []string{"immutable, foo=bar, x-ext=\"1,2\", max-age=5"}, want: Directives{MaxAge: valid(5)}},
		{name: "trailing bytes invalidate a delta", lines: []string{"max-age=60 junk, public"}, want: Directives{MaxAge: invalid, Public: true}},
		{name: "malformed restrictive directive applies", lines: []string{"no-store junk"}, want: Directives{NoStore: true}},
		{name: "malformed public is ignored", lines: []string{"public junk"}, want: Directives{}},
		{name: "unterminated quote", lines: []string{`max-age="60`}, want: Directives{MaxAge: invalid}},
		{name: "unterminated quote swallows the rest", lines: []string{`no-cache="x, max-age=5`}, want: Directives{NoCache: true}},
		{name: "element that is not a token", lines: []string{`"x,y", max-age=5`}, want: Directives{MaxAge: valid(5)}},
		{name: "quote directly after a name", lines: []string{`no-cache"x", max-age=9`}, want: Directives{NoCache: true, MaxAge: valid(9)}},
		{name: "min-fresh", lines: []string{"min-fresh=30"}, want: Directives{MinFresh: valid(30)}},
		{name: "invalid min-fresh", lines: []string{"min-fresh"}, want: Directives{MinFresh: invalid}},
		{name: "max-stale with a value", lines: []string{`max-stale="20"`}, want: Directives{MaxStale: valid(20)}},
		{
			name:  "bare max-stale accepts any staleness",
			lines: []string{"Max-Stale"},
			want:  Directives{MaxStale: Delta{Present: true, Valid: true, Value: MaxDelta}},
		},
		{name: "bare and valued max-stale conflict", lines: []string{"max-stale, max-stale=20"}, want: Directives{MaxStale: invalid}},
		{name: "empty max-stale", lines: []string{"max-stale="}, want: Directives{MaxStale: invalid}},
		{name: "malformed bare max-stale", lines: []string{"max-stale junk"}, want: Directives{MaxStale: invalid}},
		{name: "request stale-if-error", lines: []string{"stale-if-error=600"}, want: Directives{StaleIfError: valid(600)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.lines != nil {
				h = cc(tt.lines...)
			}
			if got := Parse(h); got != tt.want {
				t.Errorf("Parse(%q) = %+v, want %+v", tt.lines, got, tt.want)
			}
		})
	}
}

// TestRevalidates checks the "Request directives" row: no-cache or a valid
// max-age=0 make every stored response take the Upstream path; another
// max-age does not.
func TestRevalidates(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"", false},
		{"no-cache", true},
		{`no-cache="X"`, true},
		{"max-age=0", true},
		{`max-age="0"`, true},
		{"max-age=00", true},
		{"max-age=5", false},
		{"max-age=x", false},
		{"max-age=0, max-age=5", false},
		{"no-store", false},
		{"only-if-cached", false},
	}
	for _, tt := range tests {
		d := Parse(cc(tt.line))
		if got := d.Revalidates(); got != tt.want {
			t.Errorf("Parse(%q).Revalidates() = %v, want %v", tt.line, got, tt.want)
		}
	}
}

// FuzzParse checks that any field value parses without a panic and that
// every Delta stays within its invariants.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"max-age=60", `no-cache="a,b"`, `private="a\"`, "max-age=99999999999999999999", ",,,", `"`, `\`, "s-maxage = 1 x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		d := Parse(cc(v, v))
		for _, x := range []Delta{d.MaxAge, d.SMaxAge, d.StaleWhileRevalidate, d.StaleIfError, d.MinFresh, d.MaxStale} {
			if x.Valid && !x.Present || !x.Valid && x.Value != 0 || x.Value < 0 || x.Value > MaxDelta {
				t.Fatalf("Parse(%q) gave %+v", v, x)
			}
		}
	})
}
