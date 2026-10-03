// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

func lit(s string) Segment   { return Segment{Literal: s} }
func param(s string) Segment { return Segment{Param: s} }

// templateCases are the 04 req 29 grammar cases (test plan item 5), shared
// with the fuzz seeds.
var templateCases = []struct {
	in   string
	want Template
	// msg is set for an invalid template: a fragment of the error.
	msg string
}{
	{in: "/v1/{id}", want: Template{Segments: []Segment{lit("v1"), param("id")}}},
	{in: "/v1/{id}/x", want: Template{Segments: []Segment{lit("v1"), param("id"), lit("x")}}},
	{in: "/v1/orders/{orderId}/summary", want: Template{Segments: []Segment{lit("v1"), lit("orders"), param("orderId"), lit("summary")}}},
	{in: "/{a}/{b}", want: Template{Segments: []Segment{param("a"), param("b")}}},
	{in: "/{_x9}", want: Template{Segments: []Segment{param("_x9")}}},
	{in: "/v1/", want: Template{Segments: []Segment{lit("v1")}, TrailingSlash: true}},
	{in: "/v1/{id}/", want: Template{Segments: []Segment{lit("v1"), param("id")}, TrailingSlash: true}},
	{in: "/", want: Template{TrailingSlash: true}},
	{in: "/v1", want: Template{Segments: []Segment{lit("v1")}}},
	{in: "/%7euser/{id}", want: Template{Segments: []Segment{lit("~user"), param("id")}}},
	{in: "/a%2fb/{id}", want: Template{Segments: []Segment{lit("a%2Fb"), param("id")}}},
	{in: "/a b/{id}", want: Template{Segments: []Segment{lit("a%20b"), param("id")}}},
	{in: "/.well-known/{x}", want: Template{Segments: []Segment{lit(".well-known"), param("x")}}},
	{in: "/{a}/{a}", msg: "names parameter \"a\" twice"},
	{in: "/v1/{id}.json", msg: "brace outside"},
	{in: "/v1/x{id}", msg: "brace outside"},
	{in: "/v1/{}", msg: "is not {name}"},
	{in: "/v1/{1a}", msg: "is not {name}"},
	{in: "/v1/{a-b}", msg: "is not {name}"},
	{in: "/v1/{a}}", msg: "is not {name}"},
	{in: "/v1/{id", msg: "brace outside"},
	{in: "/v1/{", msg: "brace outside"},
	{in: "/v1/{*rest}", msg: "is not {name}"},
	{in: "/v1/{a b}", msg: "is not {name}"},
	{in: "/v1/}", msg: "brace outside"},
	{in: "v1/{id}", msg: "does not begin with /"},
	{in: "", msg: "does not begin with /"},
	{in: "//", msg: "empty segment 1"},
	{in: "/a//{b}", msg: "empty segment 2"},
	{in: "/a/./b", msg: "dot segment"},
	{in: "/a/%2E%2E/{b}", msg: "dot segment"},
	{in: "/a/%00", msg: "encoded NUL"},
	{in: "/a\\b", msg: "backslash"},
	{in: "/%5c/{x}", msg: "encoded backslash"},
	{in: "/a%zz", msg: "invalid percent escape"},
	{in: "/a\x01", msg: "control byte"},
}

// TestParseTemplate covers the template grammar (04 req 29, RZ-CFG-005).
func TestParseTemplate(t *testing.T) {
	for _, tc := range templateCases {
		got, err := ParseTemplate(tc.in)
		if tc.msg == "" {
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseTemplate(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
			}
			if err := CheckTemplate(tc.in); err != nil {
				t.Errorf("CheckTemplate(%q) = %v", tc.in, err)
			}
			continue
		}
		if code, _ := errcode.CodeOf(err); code != CodeInvalid || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("ParseTemplate(%q) = %v; want %s with %q", tc.in, err, CodeInvalid, tc.msg)
		}
		if err := CheckTemplate(tc.in); err == nil {
			t.Errorf("CheckTemplate(%q) accepted an invalid template", tc.in)
		}
	}
}

func TestTemplateStringRoundTrip(t *testing.T) {
	cases := map[string]string{
		"/v1/{id}":     "/v1/{id}",
		"/v1/{id}/":    "/v1/{id}/",
		"/":            "/",
		"/%7euser/{x}": "/~user/{x}",
		"/a%2fb":       "/a%2Fb",
	}
	for in, want := range cases {
		tp, err := ParseTemplate(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := tp.String(); got != want {
			t.Errorf("ParseTemplate(%q).String() = %q; want %q", in, got, want)
		}
		again, err := ParseTemplate(tp.String())
		if err != nil || !reflect.DeepEqual(again, tp) {
			t.Errorf("round trip of %q: %+v, %v", in, again, err)
		}
	}
	if n := (Template{Segments: []Segment{lit("a"), param("b"), param("c")}}).Params(); n != 2 {
		t.Errorf("Params() = %d; want 2", n)
	}
}

// TestTemplateMatch: "{name}" matches exactly one non-empty segment and the
// trailing slash is significant (04 req 28, 29).
func TestTemplateMatch(t *testing.T) {
	cases := []struct {
		tmpl, path string
		want       []string // nil: no match
	}{
		{"/v1/orders/{orderId}", "/v1/orders/42", []string{"42"}},
		{"/v1/orders/{orderId}", "/v1/orders/export", []string{"export"}},
		{"/v1/orders/{orderId}", "/v1/orders/", nil},
		{"/v1/orders/{orderId}", "/v1/orders", nil},
		{"/v1/orders/{orderId}", "/v1/orders/42/", nil},
		{"/v1/orders/{orderId}", "/v1/orders/42/summary", nil},
		{"/v1/orders/{orderId}", "/v1/orders/a%2Fb", []string{"a%2Fb"}},
		{"/v1/orders/{orderId}", "/V1/orders/42", nil},
		{"/v1/orders/{orderId}/summary", "/v1/orders/42/summary", []string{"42"}},
		{"/v1/{id}/", "/v1/42/", []string{"42"}},
		{"/v1/{id}/", "/v1/42", nil},
		{"/v1/{id}/", "/v1/42//", nil},
		{"/{a}/{b}", "/x/y", []string{"x", "y"}},
		{"/{a}/{b}", "//y", nil},
		{"/{a}/{b}", "/x/", nil},
		{"/", "/", []string{}},
		{"/", "/a", nil},
		{"/", "//", nil},
		{"/v1", "/v1", []string{}},
		{"/v1", "/v1/", nil},
		{"/v1", "", nil},
		{"/v1", "v1", nil},
	}
	for _, tc := range cases {
		tp, err := ParseTemplate(tc.tmpl)
		if err != nil {
			t.Fatal(err)
		}
		prefix := []string{"keep"}
		got, ok := tp.Match(tc.path, prefix)
		if ok != (tc.want != nil) {
			t.Errorf("%q.Match(%q) = %v; want %v", tc.tmpl, tc.path, ok, tc.want != nil)
			continue
		}
		if !ok {
			if !slices.Equal(got, prefix) {
				t.Errorf("%q.Match(%q) left captures %q", tc.tmpl, tc.path, got)
			}
			continue
		}
		if !slices.Equal(got[1:], tc.want) || got[0] != "keep" {
			t.Errorf("%q.Match(%q) captured %q; want %q", tc.tmpl, tc.path, got[1:], tc.want)
		}
	}
	tp, _ := ParseTemplate("/v1/orders/{orderId}/items/{item}")
	buf := make([]string, 0, 4)
	if allocs := testing.AllocsPerRun(100, func() { _, _ = tp.Match("/v1/orders/42/items/7", buf) }); allocs != 0 {
		t.Errorf("Match: %v allocations", allocs)
	}
}
