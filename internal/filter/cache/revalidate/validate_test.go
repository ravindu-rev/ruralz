// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package revalidate

import (
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"
)

// TestConditional checks the validators of a revalidation request (RFC
// 9111 section 4.3.1) and that the request's own preconditions and Range
// are removed first.
func TestConditional(t *testing.T) {
	lm := date(t0.Add(-time.Hour))
	tests := []struct {
		name   string
		req    http.Header
		stored http.Header
		want   http.Header
		ok     bool
	}{
		{
			name:   "entity tag and Last-Modified",
			stored: http.Header{"Etag": {`"v1"`}, "Last-Modified": {lm}, "Date": {date(t0)}},
			want:   http.Header{"If-None-Match": {`"v1"`}, "If-Modified-Since": {lm}},
			ok:     true,
		},
		{
			name:   "entity tag only",
			stored: http.Header{"Etag": {`W/"v1"`}, "Date": {date(t0)}},
			want:   http.Header{"If-None-Match": {`W/"v1"`}},
			ok:     true,
		},
		{
			name:   "Last-Modified only",
			stored: http.Header{"Last-Modified": {lm}, "Date": {date(t0)}},
			want:   http.Header{"If-Modified-Since": {lm}},
			ok:     true,
		},
		{
			name:   "Date without validators",
			stored: http.Header{"Date": {date(t0)}},
			want:   http.Header{"If-Modified-Since": {date(t0)}},
			ok:     true,
		},
		{
			name:   "invalid Last-Modified falls back to Date",
			stored: http.Header{"Last-Modified": {"last week"}, "Date": {date(t0)}},
			want:   http.Header{"If-Modified-Since": {date(t0)}},
			ok:     true,
		},
		{
			name:   "invalid Last-Modified beside an entity tag",
			stored: http.Header{"Etag": {`"v1"`}, "Last-Modified": {"last week"}, "Date": {date(t0)}},
			want:   http.Header{"If-None-Match": {`"v1"`}},
			ok:     true,
		},
		{
			name:   "no validator",
			stored: http.Header{"Date": {"never"}},
			want:   http.Header{},
			ok:     false,
		},
		{
			name: "the request's preconditions and Range are removed",
			req: http.Header{
				"If-Match": {`"a"`}, "If-None-Match": {`"client"`}, "If-Modified-Since": {lm},
				"If-Unmodified-Since": {lm}, "If-Range": {`"a"`}, "Range": {"bytes=0-1"},
				"if-none-match": {`"lower"`}, "Accept": {"application/json"},
			},
			stored: http.Header{"Etag": {`"v2"`}},
			want:   http.Header{"If-None-Match": {`"v2"`}, "Accept": {"application/json"}},
			ok:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.req.Clone()
			if h == nil {
				h = http.Header{}
			}
			ok := Conditional(h, tt.stored)
			if ok != tt.ok || !equalHeader(h, tt.want) {
				t.Errorf("Conditional(%v, %v) = %v, header %v; want %v, %v", tt.req, tt.stored, ok, h, tt.ok, tt.want)
			}
		})
	}
}

// equalHeader compares headers field by field.
func equalHeader(a, b http.Header) bool {
	return maps.EqualFunc(a, b, slices.Equal)
}

// TestFreshenSelects checks which 304 responses select the stored
// response (RFC 9111 section 4.3.4).
func TestFreshenSelects(t *testing.T) {
	lm := date(t0.Add(-time.Hour))
	tests := []struct {
		name   string
		stored http.Header
		nm     http.Header
		want   bool
	}{
		{name: "equal strong tags", stored: http.Header{"Etag": {`"v1"`}}, nm: http.Header{"Etag": {`"v1"`}}, want: true},
		{name: "strong tag against a weak stored tag", stored: http.Header{"Etag": {`W/"v1"`}}, nm: http.Header{"Etag": {`"v1"`}}, want: false},
		{name: "other strong tag", stored: http.Header{"Etag": {`"v1"`}}, nm: http.Header{"Etag": {`"v2"`}}, want: false},
		{name: "weak tag against a strong stored tag", stored: http.Header{"Etag": {`"v1"`}}, nm: http.Header{"Etag": {`W/"v1"`}}, want: true},
		{name: "other weak tag", stored: http.Header{"Etag": {`W/"v1"`}}, nm: http.Header{"Etag": {`W/"v2"`}}, want: false},
		{name: "tag without a stored tag", stored: http.Header{"Last-Modified": {lm}}, nm: http.Header{"Etag": {`"v1"`}}, want: false},
		{name: "malformed tags compare as strings", stored: http.Header{"Etag": {"v1"}}, nm: http.Header{"Etag": {"v1"}}, want: true},
		{name: "different malformed tags", stored: http.Header{"Etag": {"v1"}}, nm: http.Header{"Etag": {"v2"}}, want: false},
		{name: "equal Last-Modified", stored: http.Header{"Last-Modified": {lm}}, nm: http.Header{"Last-Modified": {lm}}, want: true},
		{
			name:   "equal Last-Modified in another format",
			stored: http.Header{"Last-Modified": {lm}},
			nm:     http.Header{"Last-Modified": {t0.Add(-time.Hour).Format(time.RFC850)}},
			want:   true,
		},
		{name: "other Last-Modified", stored: http.Header{"Last-Modified": {lm}}, nm: http.Header{"Last-Modified": {date(t0)}}, want: false},
		{name: "Last-Modified without a stored one", stored: http.Header{"Etag": {`"v1"`}}, nm: http.Header{"Last-Modified": {lm}}, want: false},
		{name: "no validator in the 304", stored: http.Header{"Etag": {`"v1"`}}, nm: http.Header{"Date": {date(t0)}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Freshen(tt.stored, tt.nm, t0)
			if ok != tt.want || (got == nil) == ok {
				t.Errorf("Freshen(%v, %v) = %v, %v; want ok %v", tt.stored, tt.nm, got, ok, tt.want)
			}
		})
	}
}

// TestFreshenMerge checks the header update of RFC 9111 section 3.2: 304
// fields replace stored ones except the fields a cache never updates or
// stores, and neither input changes.
func TestFreshenMerge(t *testing.T) {
	stored := http.Header{
		"Content-Type":   {"text/plain"},
		"Cache-Control":  {"max-age=60"},
		"Etag":           {`"v1"`},
		"Content-Length": {"5"},
		"X-Old":          {"a"},
		"X-Both":         {"old1", "old2"},
		"Date":           {date(t0.Add(-time.Hour))},
		"Age":            {"30"},
	}
	nm := http.Header{
		"Cache-Control":             {"max-age=120"},
		"Etag":                      {`"v1"`},
		"X-New":                     {"b"},
		"X-Both":                    {"new"},
		"Content-Length":            {"0"},
		"Content-Range":             {"bytes 0-1/2"},
		"Connection":                {"X-Hop"},
		"X-Hop":                     {"secret"},
		"Keep-Alive":                {"timeout=5"},
		"Transfer-Encoding":         {"chunked"},
		"Proxy-Authenticate":        {"Basic"},
		"Proxy-Authentication-Info": {"x"},
		"Date":                      {date(t0)},
	}
	storedCopy, nmCopy := stored.Clone(), nm.Clone()
	got, ok := Freshen(stored, nm, t0.Add(time.Second))
	if !ok {
		t.Fatal("Freshen refused a 304 with the stored entity tag")
	}
	want := http.Header{
		"Content-Type":   {"text/plain"},
		"Cache-Control":  {"max-age=120"},
		"Etag":           {`"v1"`},
		"Content-Length": {"5"},
		"X-Old":          {"a"},
		"X-New":          {"b"},
		"X-Both":         {"new"},
		"Date":           {date(t0)},
	}
	if !equalHeader(got, want) {
		t.Errorf("Freshen = %v, want %v", got, want)
	}
	if !equalHeader(stored, storedCopy) || !equalHeader(nm, nmCopy) {
		t.Error("Freshen modified its inputs")
	}
	got["X-New"][0] = "changed"
	if nm["X-New"][0] != "b" {
		t.Error("Freshen shares value slices with the 304")
	}
}

// TestFreshenAgeAndDate checks that the age restarts at the revalidation:
// the 304's Age or none, and its Date or the receipt time.
func TestFreshenAgeAndDate(t *testing.T) {
	stored := http.Header{"Etag": {`"v1"`}, "Age": {"300"}, "Date": {date(t0.Add(-time.Hour))}}
	received := t0.Add(2 * time.Second)
	tests := []struct {
		name     string
		nm       http.Header
		wantAge  []string
		wantDate string
	}{
		{name: "304 with Age and Date", nm: http.Header{"Age": {"5"}, "Date": {date(t0)}}, wantAge: []string{"5"}, wantDate: date(t0)},
		{name: "304 without Age or Date", nm: http.Header{}, wantDate: date(received)},
		{name: "304 with an invalid Date", nm: http.Header{"Date": {"now"}}, wantDate: date(received)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Freshen(stored, tt.nm, received)
			if !ok {
				t.Fatal("Freshen refused a 304 without validators")
			}
			if !slices.Equal(got["Age"], tt.wantAge) || got.Get("Date") != tt.wantDate {
				t.Errorf("Freshen: Age %q, Date %q; want %q, %q", got["Age"], got.Get("Date"), tt.wantAge, tt.wantDate)
			}
			if age := InitialAge(got, received, received); tt.wantAge == nil && age != 0 {
				t.Errorf("InitialAge after Freshen = %v, want 0", age)
			}
		})
	}
}

// TestNotModified checks a client's own validation of its copy against a
// stored response (RFC 9111 section 4.3.2): If-None-Match by weak
// comparison or "*", deciding alone; else one valid If-Modified-Since
// against Last-Modified, Date or the receipt time; never the
// preconditions that only an origin server evaluates.
func TestNotModified(t *testing.T) {
	lm := t0.Add(-time.Hour)
	tagged := http.Header{"Etag": {`"v1"`}, "Last-Modified": {date(lm)}, "Date": {date(t0)}}
	tests := []struct {
		name   string
		req    http.Header
		stored http.Header
		want   bool
	}{
		{name: "matching tag", req: http.Header{"If-None-Match": {`"v1"`}}, stored: tagged, want: true},
		{name: "weak comparison", req: http.Header{"If-None-Match": {`W/"v1"`}}, stored: tagged, want: true},
		{name: "weak stored tag", req: http.Header{"If-None-Match": {`"v1"`}}, stored: http.Header{"Etag": {`W/"v1"`}}, want: true},
		{name: "tag in a list", req: http.Header{"If-None-Match": {`"a", "v1"`}}, stored: tagged, want: true},
		{name: "tag on a second line", req: http.Header{"If-None-Match": {`"a"`, `"v1"`}}, stored: tagged, want: true},
		{name: "comma inside a tag", req: http.Header{"If-None-Match": {`"x,y", "z"`}}, stored: http.Header{"Etag": {`"x,y"`}}, want: true},
		{name: "star", req: http.Header{"If-None-Match": {"*"}}, stored: http.Header{"Date": {date(t0)}}, want: true},
		{name: "other tags", req: http.Header{"If-None-Match": {`"a", W/"b"`}}, stored: tagged, want: false},
		{name: "no stored tag", req: http.Header{"If-None-Match": {`"v1"`}}, stored: http.Header{"Last-Modified": {date(lm)}}, want: false},
		{name: "malformed members", req: http.Header{"If-None-Match": {`v1, "v1`}}, stored: tagged, want: false},
		{name: "unparsable stored tag matches its bytes", req: http.Header{"If-None-Match": {"v1"}}, stored: http.Header{"Etag": {"v1"}}, want: true},
		{
			name:   "If-None-Match decides alone",
			req:    http.Header{"If-None-Match": {`"a"`}, "If-Modified-Since": {date(t0)}},
			stored: tagged,
			want:   false,
		},
		{name: "not modified since", req: http.Header{"If-Modified-Since": {date(lm)}}, stored: tagged, want: true},
		{name: "modified since", req: http.Header{"If-Modified-Since": {date(lm.Add(-time.Second))}}, stored: tagged, want: false},
		{name: "Date without Last-Modified", req: http.Header{"If-Modified-Since": {date(t0)}}, stored: http.Header{"Date": {date(t0)}}, want: true},
		{name: "receipt time without Date", req: http.Header{"If-Modified-Since": {date(t0.Add(-time.Minute))}}, stored: http.Header{}, want: false},
		{name: "invalid If-Modified-Since", req: http.Header{"If-Modified-Since": {"yesterday"}}, stored: tagged, want: false},
		{name: "two If-Modified-Since lines", req: http.Header{"If-Modified-Since": {date(t0), date(t0)}}, stored: tagged, want: false},
		{name: "origin-only preconditions", req: http.Header{"If-Match": {`"v1"`}, "If-Unmodified-Since": {date(t0)}}, stored: tagged, want: false},
		{name: "no precondition", req: http.Header{}, stored: tagged, want: false},
	}
	for _, tt := range tests {
		if got := NotModified(tt.req, tt.stored, t0); got != tt.want {
			t.Errorf("%s: NotModified(%v, %v) = %v, want %v", tt.name, tt.req, tt.stored, got, tt.want)
		}
	}
}

// TestNotModifiedHeader checks the fields of a 304 the cache answers from
// a stored response (RFC 9110 section 15.4.5).
func TestNotModifiedHeader(t *testing.T) {
	stored := http.Header{
		"Cache-Control": {"max-age=60"}, "Content-Location": {"/v1/catalog.json"}, "Date": {date(t0)},
		"Etag": {`"v1"`}, "Expires": {date(t0.Add(time.Minute))}, "Vary": {"Accept-Encoding"},
		"Last-Modified": {date(t0.Add(-time.Hour))}, "Content-Type": {"application/json"},
		"Content-Length": {"42"}, "X-Request-Cost": {"3"},
	}
	want := http.Header{
		"Cache-Control": {"max-age=60"}, "Content-Location": {"/v1/catalog.json"}, "Date": {date(t0)},
		"Etag": {`"v1"`}, "Expires": {date(t0.Add(time.Minute))}, "Vary": {"Accept-Encoding"},
	}
	got := NotModifiedHeader(stored)
	if !equalHeader(got, want) {
		t.Errorf("NotModifiedHeader = %v, want %v", got, want)
	}
	got["Etag"][0] = "changed"
	if stored["Etag"][0] != `"v1"` {
		t.Error("NotModifiedHeader shares value slices with the stored header")
	}
	untagged := http.Header{"Last-Modified": {date(t0.Add(-time.Hour))}, "Date": {date(t0)}}
	if got := NotModifiedHeader(untagged); !equalHeader(got, untagged) {
		t.Errorf("NotModifiedHeader without ETag = %v, want Last-Modified and Date", got)
	}
}

// TestSameVary checks the comparison of Vary names that guards a refresh.
func TestSameVary(t *testing.T) {
	vary := func(vs ...string) http.Header {
		if vs == nil {
			return http.Header{}
		}
		return http.Header{"Vary": vs}
	}
	tests := []struct {
		name string
		a, b http.Header
		want bool
	}{
		{name: "none", a: vary(), b: vary(), want: true},
		{name: "case and order", a: vary("Accept-Encoding, Accept"), b: vary("accept", "ACCEPT-ENCODING"), want: true},
		{name: "repetition and whitespace", a: vary("Accept, ,Accept\t"), b: vary("accept"), want: true},
		{name: "added name", a: vary("Accept"), b: vary("Accept, Origin"), want: false},
		{name: "added Vary", a: vary(), b: vary("Accept"), want: false},
	}
	for _, tt := range tests {
		if got := sameVary(tt.a, tt.b); got != tt.want {
			t.Errorf("%s: sameVary(%v, %v) = %v, want %v", tt.name, tt.a, tt.b, got, tt.want)
		}
	}
}
