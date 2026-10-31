// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package revalidate

import (
	"net/http"
	"testing"
	"time"
)

// t0 is the receipt time of the responses below.
var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// date formats t as an HTTP-date.
func date(t time.Time) string { return t.UTC().Format(http.TimeFormat) }

// sec returns n seconds.
func sec(n int) time.Duration { return time.Duration(n) * time.Second }

// TestFreshnessOf checks the freshness lifetime of RFC 9111 section 4.2.1
// for a shared cache, the Freshness row's caps and the stale limits.
func TestFreshnessOf(t *testing.T) {
	tests := []struct {
		name string
		h    http.Header
		want Freshness
	}{
		{
			name: "s-maxage over max-age",
			h:    cc("max-age=60, s-maxage=30"),
			want: Freshness{Lifetime: sec(30), Explicit: true, StaleForbidden: true},
		},
		{
			name: "max-age over Expires",
			h:    http.Header{"Cache-Control": {"max-age=60"}, "Expires": {date(t0.Add(time.Hour))}, "Date": {date(t0)}},
			want: Freshness{Lifetime: sec(60), Explicit: true},
		},
		{
			name: "Expires minus Date",
			h:    http.Header{"Expires": {date(t0.Add(80 * time.Second))}, "Date": {date(t0.Add(-10 * time.Second))}},
			want: Freshness{Lifetime: sec(90), Explicit: true},
		},
		{
			name: "Expires without Date counts from receipt",
			h:    http.Header{"Expires": {date(t0.Add(40 * time.Second))}},
			want: Freshness{Lifetime: sec(40), Explicit: true},
		},
		{
			name: "invalid Date counts from receipt",
			h:    http.Header{"Expires": {date(t0.Add(40 * time.Second))}, "Date": {"yesterday"}},
			want: Freshness{Lifetime: sec(40), Explicit: true},
		},
		{
			name: "invalid Expires is already expired",
			h:    http.Header{"Expires": {"0"}, "Date": {date(t0)}},
			want: Freshness{Explicit: true},
		},
		{
			name: "Expires before Date",
			h:    http.Header{"Expires": {date(t0.Add(-time.Minute))}, "Date": {date(t0)}},
			want: Freshness{Explicit: true},
		},
		{
			name: "Expires lines that disagree are stale",
			h:    http.Header{"Expires": {date(t0.Add(time.Minute)), date(t0.Add(time.Hour))}, "Date": {date(t0)}},
			want: Freshness{Explicit: true},
		},
		{
			name: "equal Expires lines",
			h:    http.Header{"Expires": {date(t0.Add(time.Minute)), date(t0.Add(time.Minute))}, "Date": {date(t0)}},
			want: Freshness{Lifetime: time.Minute, Explicit: true},
		},
		{
			name: "invalid max-age is stale and ignores Expires",
			h:    http.Header{"Cache-Control": {"max-age=soon"}, "Expires": {date(t0.Add(time.Hour))}, "Date": {date(t0)}},
			want: Freshness{},
		},
		{
			name: "invalid s-maxage still forbids stale",
			h:    cc("s-maxage=x, max-age=60, stale-while-revalidate=30"),
			want: Freshness{StaleForbidden: true},
		},
		{name: "no explicit freshness", h: cc("public"), want: Freshness{}},
		{name: "lifetime capped at 24 h", h: cc("max-age=172800"), want: Freshness{Lifetime: MaxFreshness, Explicit: true}},
		{
			name: "stale windows",
			h:    cc("max-age=60, stale-while-revalidate=30, stale-if-error=90"),
			want: Freshness{Lifetime: sec(60), Explicit: true, StaleWhileRevalidate: sec(30), StaleIfError: sec(90)},
		},
		{
			name: "stale windows capped at 1 h",
			h:    cc("max-age=60, stale-while-revalidate=7200, stale-if-error=86400"),
			want: Freshness{Lifetime: sec(60), Explicit: true, StaleWhileRevalidate: MaxStaleWindow, StaleIfError: MaxStaleWindow},
		},
		{
			name: "invalid stale window allows nothing",
			h:    cc("max-age=60, stale-while-revalidate=x, stale-if-error=1, stale-if-error=2"),
			want: Freshness{Lifetime: sec(60), Explicit: true},
		},
		{
			name: "must-revalidate forbids stale",
			h:    cc("max-age=60, must-revalidate, stale-while-revalidate=30, stale-if-error=90"),
			want: Freshness{Lifetime: sec(60), Explicit: true, StaleForbidden: true},
		},
		{
			name: "proxy-revalidate forbids stale",
			h:    cc("max-age=60, proxy-revalidate, stale-while-revalidate=30, stale-if-error=90"),
			want: Freshness{Lifetime: sec(60), Explicit: true, StaleForbidden: true},
		},
		{
			name: "s-maxage forbids stale",
			h:    cc("s-maxage=60, stale-while-revalidate=30, stale-if-error=90"),
			want: Freshness{Lifetime: sec(60), Explicit: true, StaleForbidden: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FreshnessOf(tt.h, t0); got != tt.want {
				t.Errorf("FreshnessOf(%v) = %+v, want %+v", tt.h, got, tt.want)
			}
		})
	}
}

// TestInitialAge checks the corrected initial age of RFC 9111 section
// 4.2.3: the larger of the apparent age and the Age field plus the
// response delay.
func TestInitialAge(t *testing.T) {
	tests := []struct {
		name      string
		h         http.Header
		requested time.Time
		want      time.Duration
	}{
		{name: "response delay", h: http.Header{"Date": {date(t0)}}, requested: t0.Add(-250 * time.Millisecond), want: 250 * time.Millisecond},
		{name: "Age plus delay", h: http.Header{"Date": {date(t0)}, "Age": {"30"}}, requested: t0.Add(-2 * time.Second), want: sec(32)},
		{name: "apparent age wins", h: http.Header{"Date": {date(t0.Add(-time.Minute))}, "Age": {"10"}}, requested: t0.Add(-time.Second), want: time.Minute},
		{name: "Date ahead of the Node", h: http.Header{"Date": {date(t0.Add(30 * time.Second))}}, requested: t0, want: 0},
		{name: "missing Date", h: http.Header{"Age": {"7"}}, requested: t0, want: sec(7)},
		{name: "invalid Age counts zero", h: http.Header{"Date": {date(t0)}, "Age": {"-3"}}, requested: t0, want: 0},
		{name: "Age with whitespace", h: http.Header{"Date": {date(t0)}, "Age": {" 12 "}}, requested: t0, want: sec(12)},
		{name: "Age saturates", h: http.Header{"Date": {date(t0)}, "Age": {"99999999999999"}}, requested: t0, want: MaxDelta},
		{name: "request after receipt", h: http.Header{"Date": {date(t0)}, "Age": {"4"}}, requested: t0.Add(time.Second), want: sec(4)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InitialAge(tt.h, tt.requested, t0); got != tt.want {
				t.Errorf("InitialAge(%v) = %v, want %v", tt.h, got, tt.want)
			}
		})
	}
}

// TestCurrentAge checks the resident time and that a clock going back
// never lowers the age.
func TestCurrentAge(t *testing.T) {
	if got := CurrentAge(sec(10), t0, t0.Add(5*time.Second)); got != sec(15) {
		t.Errorf("CurrentAge(10s, +5s) = %v, want 15s", got)
	}
	if got := CurrentAge(sec(10), t0, t0.Add(-5*time.Second)); got != sec(10) {
		t.Errorf("CurrentAge(10s, -5s) = %v, want 10s", got)
	}
}

// TestFormatAge checks the Age field value: whole seconds, never negative,
// at most 2^31 (RFC 9111 section 5.1).
func TestFormatAge(t *testing.T) {
	tests := []struct {
		age  time.Duration
		want string
	}{
		{0, "0"},
		{1900 * time.Millisecond, "1"},
		{-5 * time.Second, "0"},
		{sec(3600), "3600"},
		{MaxDelta + time.Hour, "2147483648"},
	}
	for _, tt := range tests {
		if got := FormatAge(tt.age); got != tt.want {
			t.Errorf("FormatAge(%v) = %q, want %q", tt.age, got, tt.want)
		}
	}
}

// TestUse checks how a stored response serves: fresh, stale inside
// stale-while-revalidate, else the Upstream path, and the request
// directives that force the Upstream path.
func TestUse(t *testing.T) {
	f := Freshness{Lifetime: sec(60), Explicit: true, StaleWhileRevalidate: sec(30), StaleIfError: sec(300)}
	noSWR := Freshness{Lifetime: sec(60), Explicit: true, StaleIfError: sec(300)}
	forbidden := Freshness{Lifetime: sec(60), Explicit: true, StaleForbidden: true}
	req := func(line string) *Directives {
		d := Parse(cc(line))
		return &d
	}
	tests := []struct {
		name string
		f    Freshness
		age  time.Duration
		req  *Directives
		want Use
	}{
		{name: "new", f: f, age: 0, want: UseFresh},
		{name: "last fresh instant", f: f, age: sec(60) - time.Nanosecond, want: UseFresh},
		{name: "stale", f: f, age: sec(60), want: UseStale},
		{name: "end of stale-while-revalidate", f: f, age: sec(90) - time.Nanosecond, want: UseStale},
		{name: "past stale-while-revalidate", f: f, age: sec(90), want: UseValidate},
		{name: "stale-if-error does not serve stale hits", f: f, age: sec(200), want: UseValidate},
		{name: "stale forbidden", f: forbidden, age: sec(60), want: UseValidate},
		{name: "request no-cache", f: f, age: 0, req: req("no-cache"), want: UseValidate},
		{name: "request max-age=0", f: f, age: 0, req: req("max-age=0"), want: UseValidate},
		// RFC 9111 section 5.2.1.1: the age the client accepts.
		{name: "request max-age above the age", f: f, age: sec(10), req: req("max-age=30"), want: UseFresh},
		{name: "request max-age at the age", f: f, age: sec(30), req: req("max-age=30"), want: UseFresh},
		{name: "request max-age below the age", f: f, age: sec(31), req: req("max-age=30"), want: UseValidate},
		{name: "request max-age refuses stale", f: f, age: sec(70), req: req("max-age=100"), want: UseValidate},
		{name: "invalid request max-age is ignored", f: f, age: sec(70), req: req("max-age=soon"), want: UseStale},
		// Section 5.2.1.3: the freshness the client wants left.
		{name: "request min-fresh satisfied", f: f, age: sec(40), req: req("min-fresh=10"), want: UseFresh},
		{name: "request min-fresh not satisfied", f: f, age: sec(50), req: req("min-fresh=10"), want: UseValidate},
		{name: "request min-fresh refuses stale", f: f, age: sec(70), req: req("min-fresh=0"), want: UseValidate},
		// Section 5.2.1.2: the staleness the client accepts, within the
		// stale limits.
		{name: "request max-stale beyond stale-while-revalidate", f: f, age: sec(120), req: req("max-stale=60"), want: UseStale},
		{name: "request max-stale without stale-while-revalidate", f: noSWR, age: sec(61), req: req("max-stale=60"), want: UseStale},
		{name: "request max-stale at its value", f: noSWR, age: sec(120), req: req("max-stale=60"), want: UseStale},
		{name: "request max-stale past its value", f: f, age: sec(121), req: req("max-stale=60"), want: UseValidate},
		{name: "request max-stale smaller than stale-while-revalidate", f: f, age: sec(80), req: req("max-stale=10"), want: UseValidate},
		{name: "bare request max-stale", f: noSWR, age: time.Hour, req: req("max-stale"), want: UseStale},
		{name: "request max-stale within the stale limits", f: forbidden, age: sec(61), req: req("max-stale"), want: UseValidate},
		{name: "request max-stale with max-age", f: noSWR, age: sec(70), req: req("max-age=100, max-stale=20"), want: UseStale},
		{name: "request max-stale with min-fresh", f: noSWR, age: sec(55), req: req("min-fresh=10, max-stale=20"), want: UseStale},
		{name: "request max-stale does not lift max-age", f: noSWR, age: sec(70), req: req("max-age=65, max-stale"), want: UseValidate},
		{name: "conflicting request max-stale is ignored", f: noSWR, age: sec(61), req: req("max-stale=5, max-stale=60"), want: UseValidate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.Use(tt.age, tt.req); got != tt.want {
				t.Errorf("%+v.Use(%v, %+v) = %v, want %v", tt.f, tt.age, tt.req, got, tt.want)
			}
		})
	}
}

// TestServesOnError checks stale-if-error ("Failure matrix"): while fresh
// or within the larger of the response's and the request's window (RFC
// 5861 section 4, at most 1 h), never when a stale limit forbids stale.
func TestServesOnError(t *testing.T) {
	f := Freshness{Lifetime: sec(60), Explicit: true, StaleIfError: sec(120)}
	none := Freshness{Lifetime: sec(60), Explicit: true}
	forbidden := FreshnessOf(cc("max-age=60, must-revalidate, stale-if-error=120"), t0)
	req := func(line string) *Directives {
		d := Parse(cc(line))
		return &d
	}
	tests := []struct {
		name string
		f    Freshness
		age  time.Duration
		req  *Directives
		want bool
	}{
		{name: "fresh", f: f, age: sec(10), want: true},
		{name: "inside stale-if-error", f: f, age: sec(179), want: true},
		{name: "past stale-if-error", f: f, age: sec(180), want: false},
		{name: "stale forbidden", f: forbidden, age: sec(61), want: false},
		{name: "request stale-if-error alone", f: none, age: sec(100), req: req("stale-if-error=60"), want: true},
		{name: "past the request's stale-if-error", f: none, age: sec(120), req: req("stale-if-error=60"), want: false},
		{name: "request stale-if-error widens", f: f, age: sec(300), req: req("stale-if-error=600"), want: true},
		{name: "request stale-if-error never narrows", f: f, age: sec(150), req: req("stale-if-error=10"), want: true},
		{name: "request stale-if-error capped at 1 h", f: none, age: sec(60) + time.Hour, req: req("stale-if-error=86400"), want: false},
		{name: "request stale-if-error within the stale limits", f: forbidden, age: sec(61), req: req("stale-if-error=600"), want: false},
		{name: "invalid request stale-if-error", f: none, age: sec(61), req: req("stale-if-error=x"), want: false},
		{name: "other request directives change nothing", f: f, age: sec(179), req: req("no-cache, max-age=0, min-fresh=600"), want: true},
	}
	for _, tt := range tests {
		if got := tt.f.ServesOnError(tt.age, tt.req); got != tt.want {
			t.Errorf("%s: ServesOnError(%v) = %v, want %v", tt.name, tt.age, got, tt.want)
		}
	}
}

// TestIsError checks the statuses RFC 5861 section 4 calls errors.
func TestIsError(t *testing.T) {
	for status, want := range map[int]bool{
		500: true, 502: true, 503: true, 504: true,
		0: false, 200: false, 304: false, 404: false, 429: false, 501: false, 505: false,
	} {
		if got := IsError(status); got != want {
			t.Errorf("IsError(%d) = %v, want %v", status, got, want)
		}
	}
}

// TestTTL checks the State Store TTL: lifetime plus the larger stale
// window, minus the age.
func TestTTL(t *testing.T) {
	f := Freshness{Lifetime: sec(60), StaleWhileRevalidate: sec(30), StaleIfError: sec(90)}
	if got := f.TTL(sec(10)); got != sec(140) {
		t.Errorf("TTL(10s) = %v, want 140s", got)
	}
	g := Freshness{Lifetime: sec(60), StaleWhileRevalidate: sec(120)}
	if got := g.TTL(0); got != sec(180) {
		t.Errorf("TTL(0) = %v, want 180s", got)
	}
	if got := f.TTL(sec(200)); got > 0 {
		t.Errorf("TTL(200s) = %v, want nothing left", got)
	}
}

// TestStorable checks the header part of the "Storable" rule.
func TestStorable(t *testing.T) {
	with := func(line string, extra ...string) http.Header {
		h := cc(line)
		for i := 0; i+1 < len(extra); i += 2 {
			h.Add(extra[i], extra[i+1])
		}
		return h
	}
	tests := []struct {
		name       string
		h          http.Header
		authorized bool
		want       bool
	}{
		{name: "max-age", h: cc("max-age=60"), want: true},
		{name: "s-maxage", h: cc("s-maxage=60"), want: true},
		{name: "Expires", h: http.Header{"Expires": {date(t0.Add(time.Minute))}}, want: true},
		{name: "no explicit freshness", h: cc("public"), want: false},
		{name: "invalid max-age", h: cc("max-age=x"), want: false},
		{name: "no-store", h: cc("max-age=60, no-store"), want: false},
		{name: "no-cache", h: cc("max-age=60, no-cache"), want: false},
		{name: "qualified no-cache", h: cc(`max-age=60, no-cache="Set-Cookie"`), want: false},
		{name: "private", h: cc("max-age=60, private"), want: false},
		{name: "qualified private", h: cc(`max-age=60, private="X-User"`), want: false},
		{name: "Vary star", h: with("max-age=60", "Vary", "Accept, *"), want: false},
		{name: "Vary names", h: with("max-age=60", "Vary", "Accept-Encoding"), want: true},
		{name: "Set-Cookie", h: with("max-age=60", "Set-Cookie", "a=b"), want: false},
		{name: "Authorization without permission", h: cc("max-age=60"), authorized: true, want: false},
		{name: "Authorization under public", h: cc("public, max-age=60"), authorized: true, want: true},
		{name: "Authorization under s-maxage", h: cc("s-maxage=60"), authorized: true, want: true},
		{name: "Authorization under must-revalidate", h: cc("max-age=60, must-revalidate"), authorized: true, want: true},
	}
	for _, tt := range tests {
		if got := Storable(tt.h, tt.authorized); got != tt.want {
			t.Errorf("%s: Storable(%v, %v) = %v, want %v", tt.name, tt.h, tt.authorized, got, tt.want)
		}
	}
}

// TestStorableStatus checks the status part of the "Storable" rule: every
// final status but 206 and 304.
func TestStorableStatus(t *testing.T) {
	for status, want := range map[int]bool{
		100: false, 199: false, 200: true, 203: true, 204: true, 206: false, 301: true, 304: false,
		404: true, 410: true, 500: true, 599: true, 600: false, 0: false,
	} {
		if got := StorableStatus(status); got != want {
			t.Errorf("StorableStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

// TestPartial checks the partial composition response mark,
// ruralz-partial: true, compared without regard to case or whitespace.
func TestPartial(t *testing.T) {
	tests := []struct {
		h    http.Header
		want bool
	}{
		{http.Header{"Ruralz-Partial": {"true"}}, true},
		{http.Header{"Ruralz-Partial": {" TRUE "}}, true},
		{http.Header{"Ruralz-Partial": {"false"}}, false},
		{http.Header{}, false},
	}
	for _, tt := range tests {
		if got := Partial(tt.h); got != tt.want {
			t.Errorf("Partial(%v) = %v, want %v", tt.h, got, tt.want)
		}
	}
}
