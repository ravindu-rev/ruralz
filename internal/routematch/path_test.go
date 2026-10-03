// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"bufio"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// normalizeCases are the 04 req 23 cases (test plan item 5 plus edge
// cases), shared with the fuzz seeds.
var normalizeCases = []struct {
	name string
	in   string
	want string
	// reason is set for a rejected path, with the offending offset.
	reason RejectReason
	offset int
}{
	// Step 3: dot segments (RFC 3986 section 5.2.4).
	{name: "dot segments", in: "/a/./b/../c", want: "/a/c"},
	{name: "climb above root clamps", in: "/..", want: "/"},
	{name: "climb then segment", in: "/../a", want: "/a"},
	{name: "deep climb", in: "/a/b/../../../c", want: "/c"},
	{name: "final dotdot keeps slash", in: "/a/b/..", want: "/a/"},
	{name: "final dot keeps slash", in: "/a/.", want: "/a/"},
	{name: "root dot", in: "/.", want: "/"},
	{name: "root dot slash", in: "/./", want: "/"},
	{name: "dot then slash", in: "/a/./", want: "/a/"},
	{name: "dotdot then slash", in: "/a/../", want: "/"},
	{name: "empty segment popped", in: "/a//../b", want: "/a/b"},
	{name: "empty segment after climb", in: "/a/..//b", want: "//b"},
	{name: "three dots is a segment", in: "/a/.../b", want: "/a/.../b"},
	{name: "dotdot with suffix is a segment", in: "/a/..b/c", want: "/a/..b/c"},
	{name: "encoded dotdot", in: "/a/%2E%2E/b", want: "/b"},
	{name: "encoded dot lower", in: "/a/%2e/b", want: "/a/b"},
	{name: "mixed encoded dotdot", in: "/a/b/.%2e", want: "/a/"},
	{name: "dotdot and encoded slash is a segment", in: "/a/..%2F/b", want: "/a/..%2F/b"},
	// Step 2: escapes.
	{name: "unreserved letter decoded", in: "/%41", want: "/A"},
	{name: "tilde decoded", in: "/%7Euser", want: "/~user"},
	{name: "lower tilde decoded", in: "/%7e", want: "/~"},
	{name: "digit hyphen underscore decoded", in: "/%30%2D%5F", want: "/0-_"},
	{name: "encoded slash kept upper-cased", in: "/%2f", want: "/%2F"},
	{name: "encoded slash one segment", in: "/a%2fb/c", want: "/a%2Fb/c"},
	{name: "reserved escape upper-cased", in: "/%3a", want: "/%3A"},
	{name: "utf-8 escape upper-cased", in: "/%c3%a9", want: "/%C3%A9"},
	{name: "percent sign kept", in: "/%25", want: "/%25"},
	{name: "no double decoding", in: "/%2500", want: "/%2500"},
	{name: "encoded space kept", in: "/a%20", want: "/a%20"},
	{name: "encoded high byte kept", in: "/%80%ff", want: "/%80%FF"},
	{name: "raw space encoded", in: "/a b", want: "/a%20b"},
	{name: "raw brackets encoded", in: "/a[b]", want: "/a%5Bb%5D"},
	{name: "raw non-ascii encoded", in: "/\xc3\xa9", want: "/%C3%A9"},
	{name: "raw question mark encoded", in: "/a?b", want: "/a%3Fb"},
	{name: "raw quote and braces encoded", in: "/\"{}|^`<>", want: "/%22%7B%7D%7C%5E%60%3C%3E"},
	// Step 4 and already-normal paths.
	{name: "repeated and trailing slash kept", in: "/a//b/", want: "/a//b/"},
	{name: "double slash root", in: "//", want: "//"},
	{name: "root", in: "/", want: "/"},
	{name: "empty is root", in: "", want: "/"},
	{name: "sub-delims and pchar kept", in: "/!$&'()*+,;=:@-._~", want: "/!$&'()*+,;=:@-._~"},
	{name: "upper escapes kept", in: "/v1/a%20b%2Fc", want: "/v1/a%20b%2Fc"},
	// Step 1: rejections (OQ-security-and-identity-21 (a)).
	{name: "encoded NUL", in: "/%00", reason: RejectEncodedNUL, offset: 1},
	{name: "raw backslash", in: "/a\\b", reason: RejectBackslash, offset: 2},
	{name: "encoded backslash upper", in: "/%5C", reason: RejectEncodedBackslash, offset: 1},
	{name: "encoded backslash lower", in: "/a/%5c", reason: RejectEncodedBackslash, offset: 3},
	{name: "invalid escape", in: "/%zz", reason: RejectBadEscape, offset: 1},
	{name: "half escape", in: "/a%4", reason: RejectBadEscape, offset: 2},
	{name: "bare percent", in: "/%", reason: RejectBadEscape, offset: 1},
	{name: "second digit invalid", in: "/%4g", reason: RejectBadEscape, offset: 1},
	{name: "raw NUL", in: "/a\x00", reason: RejectControlByte, offset: 2},
	{name: "raw newline", in: "/a\nb", reason: RejectControlByte, offset: 2},
	{name: "raw tab", in: "/\t", reason: RejectControlByte, offset: 1},
	{name: "raw DEL", in: "/\x7f", reason: RejectControlByte, offset: 1},
	{name: "encoded LF", in: "/a%0A", reason: RejectEncodedControlByte, offset: 2},
	{name: "encoded CR LF lower", in: "/a/%0d%0a", reason: RejectEncodedControlByte, offset: 3},
	{name: "encoded SOH", in: "/%01", reason: RejectEncodedControlByte, offset: 1},
	{name: "encoded US", in: "/x%1F", reason: RejectEncodedControlByte, offset: 2},
	{name: "encoded DEL", in: "/%7f", reason: RejectEncodedControlByte, offset: 1},
	{name: "relative", in: "a/b", reason: RejectNotOriginForm, offset: 0},
	{name: "double asterisk", in: "**", reason: RejectNotOriginForm, offset: 0},
	{name: "asterisk then path", in: "*/a", reason: RejectNotOriginForm, offset: 0},
	{name: "first offense wins", in: "/%zz\\", reason: RejectBadEscape, offset: 1},
}

// TestNormalizePath covers 04 req 23 steps 1 to 4 and the RZ-RT-017 error.
func TestNormalizePath(t *testing.T) {
	for _, tc := range normalizeCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePath(tc.in)
			if tc.reason == 0 {
				if err != nil || got != tc.want {
					t.Fatalf("NormalizePath(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("NormalizePath(%q) = %q; want a rejection", tc.in, got)
			}
			if got != "" {
				t.Errorf("rejected path returned %q", got)
			}
			if !errors.Is(err, ErrRejected) {
				t.Errorf("error %v does not match ErrRejected", err)
			}
			if code, _ := errcode.CodeOf(err); code != CodeRejected || errcode.Status(code) != 400 {
				t.Errorf("code %q status %d; want %s and 400", code, errcode.Status(code), CodeRejected)
			}
			var pe *PathError
			if !errors.As(err, &pe) || pe.Reason != tc.reason || pe.Offset != tc.offset {
				t.Errorf("error %v; want reason %v at %d", err, tc.reason, tc.offset)
			}
		})
	}
}

// TestNormalizePathAsteriskForm: asterisk-form matches no Route (04 req 23):
// "*" is 404 RZ-RT-001, not a 400 RZ-RT-017 rejection, while every other
// target that does not start with "/" stays RZ-RT-017.
func TestNormalizePathAsteriskForm(t *testing.T) {
	got, err := NormalizePath("*")
	if got != "" || !errors.Is(err, ErrAsteriskForm) {
		t.Fatalf(`NormalizePath("*") = %q, %v; want ErrAsteriskForm`, got, err)
	}
	if errors.Is(err, ErrRejected) {
		t.Errorf("asterisk-form error %v matches ErrRejected", err)
	}
	if pe := (*PathError)(nil); errors.As(err, &pe) {
		t.Errorf("asterisk-form error %v is a PathError", err)
	}
	if code, _ := errcode.CodeOf(err); code != CodeNoRoute || errcode.Status(code) != 404 {
		t.Errorf("code %q status %d; want %s and 404", code, errcode.Status(code), CodeNoRoute)
	}
	for _, in := range []string{"**", "*/a", " *", "a", "?"} {
		_, err := NormalizePath(in)
		if code, _ := errcode.CodeOf(err); code != CodeRejected || !errors.Is(err, ErrRejected) || errors.Is(err, ErrAsteriskForm) {
			t.Errorf("NormalizePath(%q) = %v; want %s only", in, err, CodeRejected)
		}
	}
}

// TestNormalizePathIdempotent: normalization is idempotent (04 req 23).
func TestNormalizePathIdempotent(t *testing.T) {
	for _, tc := range normalizeCases {
		if tc.reason != 0 {
			continue
		}
		again, err := NormalizePath(tc.want)
		if err != nil || again != tc.want {
			t.Errorf("NormalizePath(%q) = %q, %v; want it unchanged", tc.want, again, err)
		}
	}
}

// TestNormalizePathAllocs: an already-normal path costs no allocation (WP-05
// "Done when"; 04 req 33 zero Ruralz-owned allocations per match).
func TestNormalizePathAllocs(t *testing.T) {
	for _, p := range []string{"/", "/v1/orders/42", "/v1/a%20b%2Fc/", "/a//b/", "/!$&'()*+,;=:@-._~"} {
		var out string
		allocs := testing.AllocsPerRun(100, func() { out, _ = NormalizePath(p) })
		if allocs != 0 || out != p {
			t.Errorf("NormalizePath(%q): %v allocations, %q", p, allocs, out)
		}
	}
	if allocs := testing.AllocsPerRun(100, func() { _, _ = NormalizePath("") }); allocs != 0 {
		t.Errorf("empty path: %v allocations", allocs)
	}
}

func TestPathErrorText(t *testing.T) {
	_, err := NormalizePath("/%00")
	if got, want := err.Error(), "RZ-RT-017: routematch: request target rejected: encoded NUL at byte 1"; got != want {
		t.Errorf("Error() = %q; want %q", got, want)
	}
	for r := RejectEncodedNUL; r <= RejectNotOriginForm; r++ {
		if s := r.String(); s == "" || strings.HasPrefix(s, "reason ") {
			t.Errorf("reason %d has no text", r)
		}
	}
	if got := RejectReason(99).String(); got != "reason 99" {
		t.Errorf("unknown reason = %q", got)
	}
	if (&PathError{}).Is(errors.New("other")) {
		t.Error("PathError matches an unrelated error")
	}
}

func TestNextSegment(t *testing.T) {
	cases := []struct {
		path string
		want []string
	}{
		{"/", []string{""}},
		{"/a", []string{"a"}},
		{"/a/", []string{"a", ""}},
		{"/a//b", []string{"a", "", "b"}},
		{"/v1/orders%2F42/x", []string{"v1", "orders%2F42", "x"}},
		{"//", []string{"", ""}},
	}
	for _, tc := range cases {
		var got []string
		for i := 1; i <= len(tc.path); {
			seg, next := NextSegment(tc.path, i)
			got = append(got, seg)
			i = next
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") || len(got) != len(tc.want) {
			t.Errorf("segments of %q = %q; want %q", tc.path, got, tc.want)
		}
		if want := strings.Split(tc.path[1:], "/"); strings.Join(want, "|") != strings.Join(got, "|") {
			t.Errorf("segments of %q differ from strings.Split: %q", tc.path, want)
		}
	}
}

// TestDecodeParam: captures reach CEL decoded (04 req 29).
func TestDecodeParam(t *testing.T) {
	cases := []struct{ in, want string }{
		{"42", "42"},
		{"a%2Fb", "a/b"},
		{"%C3%A9", "é"},
		{"a%20b%25", "a b%"},
		{"bad%zz", "bad%zz"},
		{"trail%4", "trail%4"},
		{"%", "%"},
	}
	for _, tc := range cases {
		if got := DecodeParam(tc.in); got != tc.want {
			t.Errorf("DecodeParam(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = DecodeParam("order-42") }); allocs != 0 {
		t.Errorf("DecodeParam without escapes: %v allocations", allocs)
	}
}

// TestPrefixMatch covers segment-boundary prefixes (04 req 28; test plan
// item 5).
func TestPrefixMatch(t *testing.T) {
	cases := []struct {
		prefix, path string
		want         bool
	}{
		{"/v1/orders", "/v1/orders", true},
		{"/v1/orders", "/v1/orders/", true},
		{"/v1/orders", "/v1/orders/42", true},
		{"/v1/orders", "/v1/ordersX", false},
		{"/v1/orders", "/v1/orders%2F42", false},
		{"/v1/orders", "/v1/order", false},
		{"/", "/", true},
		{"/", "/anything/at/all", true},
		{"/v1/", "/v1", false},
		{"/v1/", "/v1/", true},
		{"/v1/", "/v1/x", true},
		{"/v1/", "/v1x", false},
		{"/v1", "/v2", false},
	}
	for _, tc := range cases {
		if got := PrefixMatch(tc.prefix, tc.path); got != tc.want {
			t.Errorf("PrefixMatch(%q, %q) = %v; want %v", tc.prefix, tc.path, got, tc.want)
		}
	}
}

// TestCheckPath: path.exact and path.prefix values (RZ-CFG-005).
func TestCheckPath(t *testing.T) {
	for _, ok := range []string{"/", "/v1/orders", "/v1/", "/a%2Fb", "/a/../b"} {
		if err := CheckPath(ok); err != nil {
			t.Errorf("CheckPath(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "v1", "*", "/%00", "/a\\b", "/%zz", "/a\nb", "/a%0A"} {
		err := CheckPath(bad)
		if code, _ := errcode.CodeOf(err); code != CodeInvalid {
			t.Errorf("CheckPath(%q) = %v; want %s", bad, err, CodeInvalid)
		}
		if err != nil && (strings.Contains(err.Error(), CodeRejected) || strings.Contains(err.Error(), CodeNoRoute)) {
			t.Errorf("CheckPath(%q) shows the request code: %v", bad, err)
		}
	}
}

// TestRequestPath: the path as received keeps %2F even when net/url
// re-escapes EscapedPath (04 req 23: an encoded slash never splits a
// segment).
func TestRequestPath(t *testing.T) {
	cases := []struct{ target, want string }{
		{"/a%2Fb/x", "/a%2Fb/x"},
		{"/a%2Fb/{x}", "/a%2Fb/%7Bx%7D"},
		{"/a%2Fb/x|y", "/a%2Fb/x%7Cy"},
		{"/a%2fb/[x]", "/a%2Fb/%5Bx%5D"},
		{"/caf\xc3\xa9/%2F", "/caf%C3%A9/%2F"},
		{"/v1/orders/42", "/v1/orders/42"},
		{"/v1/%7Eu/./x/..", "/v1/~u/"},
	}
	for _, tc := range cases {
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader("GET " + tc.target + " HTTP/1.1\r\nHost: h\r\n\r\n")))
		if err != nil {
			t.Fatalf("%q: %v", tc.target, err)
		}
		got, err := NormalizePath(RequestPath(req.URL))
		if err != nil || got != tc.want {
			t.Errorf("NormalizePath(RequestPath(%q)) = %q, %v; want %q", tc.target, got, err, tc.want)
		}
	}
	// Encoded control bytes reach the handler (net/url rejects only raw
	// ones), so NormalizePath rejects them (04 req 23 step 1).
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader("GET /a%0D%0Ab HTTP/1.1\r\nHost: h\r\n\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizePath(RequestPath(req.URL)); !errors.Is(err, ErrRejected) {
		t.Errorf("encoded CR LF: %v; want ErrRejected", err)
	}
	u := &url.URL{Path: "/a b"}
	if got := RequestPath(u); got != "/a%20b" {
		t.Errorf("RequestPath without RawPath = %q", got)
	}
	req, _ = http.ReadRequest(bufio.NewReader(strings.NewReader("GET /a%2Fb/{x} HTTP/1.1\r\nHost: h\r\n\r\n")))
	if allocs := testing.AllocsPerRun(100, func() { _ = RequestPath(req.URL) }); allocs != 0 {
		t.Errorf("RequestPath: %v allocations", allocs)
	}
}

// TestRequestPathTargetForms pins what net/http hands the handler for the
// request-target forms that match no Route (04 req 22, 23; test plan item 6):
// asterisk-form reaches NormalizePath as "*" and comes back 404 RZ-RT-001;
// CONNECT authority-form has an empty path, which normalizes to "/", so the
// caller must answer CONNECT before normalizing (see NormalizePath).
func TestRequestPathTargetForms(t *testing.T) {
	read := func(line string) *http.Request {
		t.Helper()
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(line + "\r\nHost: h\r\n\r\n")))
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		return req
	}
	for _, line := range []string{"OPTIONS * HTTP/1.1", "GET * HTTP/1.1"} {
		p := RequestPath(read(line).URL)
		_, err := NormalizePath(p)
		if code, _ := errcode.CodeOf(err); p != "*" || !errors.Is(err, ErrAsteriskForm) || errcode.Status(code) != 404 {
			t.Errorf("%q: RequestPath %q, NormalizePath error %v; want \"*\" and 404 %s", line, p, err, CodeNoRoute)
		}
	}
	if p := RequestPath(read("CONNECT api.shop.example:443 HTTP/1.1").URL); p != "" {
		t.Errorf("CONNECT RequestPath = %q; want empty", p)
	}
	if got, err := NormalizePath(RequestPath(read("GET http://h HTTP/1.1").URL)); err != nil || got != "/" {
		t.Errorf("absolute-form without a path = %q, %v; want /", got, err)
	}
}
