// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"net/http"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// stepHeader is one merged step's response header and timing for
// cacheFields: sent and received are offsets from epoch.
type stepHeader struct {
	h              http.Header
	sent, received time.Duration
}

// fresh returns a step answered at once with Cache-Control cc.
func fresh(cc string) stepHeader {
	return stepHeader{h: http.Header{"Cache-Control": {cc}}}
}

// cacheOf returns the merged Cache-Control and Vary of steps at now
// (an offset from epoch).
func cacheOf(steps []stepHeader, now time.Duration) (string, string) {
	r := &run{c: &Composition{}}
	merged := make([]int, len(steps))
	for i, s := range steps {
		r.res = append(r.res, result{header: s.h, sent: epoch().Add(s.sent), received: epoch().Add(s.received)})
		merged[i] = i
	}
	return r.cacheFields(merged, epoch().Add(now))
}

// TestMergedCacheControl checks the most restrictive caching of the steps
// a response merges (docs/architecture/03-data-plane.md "Merge rules"
// table, RFC 9111 sections 4.2.1 and 4.2.3).
func TestMergedCacheControl(t *testing.T) {
	date := epoch().Format(http.TimeFormat)
	tests := []struct {
		name  string
		steps []stepHeader
		now   time.Duration
		want  string
	}{
		{"least max-age", []stepHeader{fresh("max-age=60"), fresh("max-age=30")}, 0, "max-age=30"},
		{"no-store wins", []stepHeader{fresh("max-age=60"), fresh("no-store, max-age=30")}, 0, "no-store"},
		{"no explicit freshness", []stepHeader{fresh("max-age=60"), fresh("public")}, 0, "no-store"},
		{"no Cache-Control at all", []stepHeader{fresh("max-age=60"), {h: http.Header{}}}, 0, "no-store"},
		{"private from one step", []stepHeader{fresh("public, max-age=60"), fresh("private, max-age=60")}, 0, "private, max-age=60"},
		{"private from Set-Cookie", []stepHeader{fresh("public, max-age=60"), {h: http.Header{"Cache-Control": {"public, max-age=60"}, "Set-Cookie": {"s=1"}}}}, 0, "private, max-age=60"},
		{"public only when every step has it", []stepHeader{fresh("public, max-age=60"), fresh("max-age=60")}, 0, "max-age=60"},
		{"public everywhere", []stepHeader{fresh("public, max-age=60"), fresh("Public, max-age=90")}, 0, "public, max-age=60"},
		{"revalidation directives from any step", []stepHeader{fresh("no-cache, max-age=60"), fresh("must-revalidate, proxy-revalidate, max-age=60")}, 0, "no-cache, must-revalidate, proxy-revalidate, max-age=60"},
		{"qualified no-cache and private", []stepHeader{fresh(`no-cache="Set-Cookie, X", private="Y", max-age=60`)}, 0, "private, no-cache, max-age=60"},
		{"Expires minus Date", []stepHeader{fresh("max-age=600"), {h: http.Header{"Date": {date}, "Expires": {epoch().Add(120 * time.Second).Format(http.TimeFormat)}}}}, 0, "max-age=120"},
		{"invalid Expires is already expired", []stepHeader{fresh("max-age=600"), {h: http.Header{"Expires": {"0"}}}}, 0, "max-age=0"},
		{"max-age over Expires", []stepHeader{{h: http.Header{"Cache-Control": {"max-age=50"}, "Date": {date}, "Expires": {epoch().Add(time.Hour).Format(http.TimeFormat)}}}}, 0, "max-age=50"},
		{"Age value less", []stepHeader{{h: http.Header{"Cache-Control": {"max-age=60"}, "Age": {"25"}}}}, 0, "max-age=35"},
		{"response delay counts", []stepHeader{{h: http.Header{"Cache-Control": {"max-age=60"}}, sent: 0, received: 10 * time.Second}}, 10 * time.Second, "max-age=50"},
		{"apparent age from Date", []stepHeader{{h: http.Header{"Cache-Control": {"max-age=60"}, "Date": {date}}, sent: 40 * time.Second, received: 40 * time.Second}}, 40 * time.Second, "max-age=20"},
		{"resident time counts", []stepHeader{fresh("max-age=60")}, 15 * time.Second, "max-age=45"},
		{"freshness spent", []stepHeader{{h: http.Header{"Cache-Control": {"max-age=10"}, "Age": {"30"}}}}, 0, "max-age=0"},
		{"s-maxage from one step", []stepHeader{fresh("max-age=60, s-maxage=300"), fresh("max-age=90")}, 0, "max-age=60, s-maxage=90"},
		{"s-maxage least", []stepHeader{fresh("max-age=60, s-maxage=300"), fresh("max-age=90, s-maxage=200")}, 0, "max-age=60, s-maxage=200"},
		{"only s-maxage leaves no private freshness", []stepHeader{fresh("s-maxage=100"), fresh("max-age=60")}, 0, "max-age=0, s-maxage=60"},
		{"stale directives when every step sets them", []stepHeader{fresh("max-age=60, stale-while-revalidate=30, stale-if-error=600"), fresh("max-age=60, stale-while-revalidate=10, stale-if-error=60")}, 0, "max-age=60, stale-while-revalidate=10, stale-if-error=60"},
		{"stale directives dropped when a step lacks them", []stepHeader{fresh("max-age=60, stale-while-revalidate=30"), fresh("max-age=60, stale-if-error=60")}, 0, "max-age=60"},
		{"first of a repeated directive", []stepHeader{{h: http.Header{"Cache-Control": {"max-age=20", "max-age=90"}}}}, 0, "max-age=20"},
		{"invalid max-age is stale", []stepHeader{fresh("max-age=soon")}, 0, "max-age=0"},
		{"quoted value", []stepHeader{fresh(`max-age="40"`)}, 0, "max-age=40"},
		{"delta-seconds above 2^31", []stepHeader{fresh("max-age=99999999999")}, 0, "max-age=2147483648"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := cacheOf(tt.steps, tt.now); got != tt.want {
				t.Errorf("Cache-Control = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMergedVary checks the merged Vary: the union of the steps' names,
// compared ignoring case and kept in their first spelling and order, or *
// when any step has it.
func TestMergedVary(t *testing.T) {
	tests := []struct {
		name  string
		lines [][]string
		want  string
	}{
		{"none", [][]string{nil, nil}, ""},
		{"union", [][]string{{"Accept, Accept-Language"}, {"accept", "Origin"}}, "Accept, Accept-Language, Origin"},
		{"star", [][]string{{"Accept"}, {"*"}}, "*"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var steps []stepHeader
			for _, l := range tt.lines {
				h := http.Header{"Cache-Control": {"max-age=1"}}
				if l != nil {
					h["Vary"] = l
				}
				steps = append(steps, stepHeader{h: h})
			}
			if _, got := cacheOf(steps, 0); got != tt.want {
				t.Errorf("Vary = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMergedResponseFields checks that a merged response carries the
// merged Cache-Control and Vary and no other step field.
func TestMergedResponseFields(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.leg("a").fn = answer(func() *snapshot.UpstreamResponse {
		r := jsonResp(200, `{"a":1}`)
		r.Header["Cache-Control"] = []string{"public, max-age=60"}
		r.Header["Vary"] = []string{"Accept"}
		r.Header["X-Step"] = []string{"a"}
		r.Header["Etag"] = []string{`"v1"`}
		return r
	})
	f.leg("b").fn = answer(func() *snapshot.UpstreamResponse {
		r := jsonResp(200, `{"b":1}`)
		r.Header["Cache-Control"] = []string{"public, max-age=30"}
		r.Header["Set-Cookie"] = []string{"s=1"}
		return r
	})
	comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("a", "a", "/a"), stepTo("b", "b", "/b")))
	resp, err := f.forward(comp)
	if err != nil {
		t.Fatal(err)
	}
	read(t, resp)
	if got := resp.Header.Get("Cache-Control"); got != "private, max-age=30" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := resp.Header.Get("Vary"); got != "Accept" {
		t.Errorf("Vary = %q", got)
	}
	for _, k := range []string{"X-Step", "Etag", "Set-Cookie"} {
		if v, ok := resp.Header[k]; ok {
			t.Errorf("merged response carries %s: %v", k, v)
		}
	}
}
