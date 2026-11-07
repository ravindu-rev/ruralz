// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redirect

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// builder returns a CEL builder whose every input is unbounded, so every
// program that can pass the runtime cost limit is tracked.
func builder(t *testing.T) expr.Builder {
	t.Helper()
	c, err := cel.New()
	if err != nil {
		t.Fatal(err)
	}
	return c.NewBuilder(nil, expr.InputCaps{})
}

// site is the site of a redirect's locationExpression.
func site() expr.Site { return expr.Site{Place: expr.PlaceRedirectLocation} }

// routeWith returns a Route whose match.path is m with redirect r.
func routeWith(m *v1alpha1.PathMatch, r v1alpha1.RouteRedirect) *v1alpha1.Route {
	rt := &v1alpha1.Route{}
	rt.Metadata.Name = "moved"
	rt.Spec.Match.Path = m
	rt.Spec.Redirect = &r
	return rt
}

// prefixMatch and exactMatch are the path criteria of the tests.
func prefixMatch(p string) *v1alpha1.PathMatch { return &v1alpha1.PathMatch{Prefix: p} }
func exactMatch(p string) *v1alpha1.PathMatch  { return &v1alpha1.PathMatch{Exact: p} }

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// compileOK compiles rt's redirect, failing the test on an error.
func compileOK(t *testing.T, rt *v1alpha1.Route) *Redirect {
	t.Helper()
	r, err := Compile(rt, builder(t), site())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return r
}

// request is the request of the Location tests: http on a non-default
// port, a normalized path and a query with an escape.
func request() *snapshot.RedirectRequest {
	return &snapshot.RedirectRequest{Scheme: "http", Host: "api.shop.example:8080", Path: "/v1/orders/42", RawQuery: "a=1&b=%20"}
}

// TestStatus: a redirect answers with its status, 302 by default.
func TestStatus(t *testing.T) {
	for _, tc := range []struct {
		status *int32
		want   int
	}{
		{nil, 302}, {ptr[int32](301), 301}, {ptr[int32](302), 302}, {ptr[int32](307), 307}, {ptr[int32](308), 308},
	} {
		r := compileOK(t, routeWith(exactMatch("/old"), v1alpha1.RouteRedirect{Status: tc.status}))
		if got := r.Status(); got != tc.want {
			t.Errorf("status %v: Status() = %d, want %d", tc.status, got, tc.want)
		}
	}
}

// TestLocationFields: the Location is the request URL with the redirect's
// scheme, host, path and query applied, each defaulting to the request's;
// a scheme that changes the request's drops the request's port, a host
// replaces host and port, and query drop removes the query.
func TestLocationFields(t *testing.T) {
	keep, drop := ptr(v1alpha1.RedirectQueryKeep), ptr(v1alpha1.RedirectQueryDrop)
	for _, tc := range []struct {
		name string
		r    v1alpha1.RouteRedirect
		in   *snapshot.RedirectRequest
		want string
	}{
		{"nothing set keeps the URL", v1alpha1.RouteRedirect{}, request(), "http://api.shop.example:8080/v1/orders/42?a=1&b=%20"},
		{"query keep", v1alpha1.RouteRedirect{Query: keep}, request(), "http://api.shop.example:8080/v1/orders/42?a=1&b=%20"},
		{"query drop", v1alpha1.RouteRedirect{Query: drop}, request(), "http://api.shop.example:8080/v1/orders/42"},
		{"scheme https drops the port", v1alpha1.RouteRedirect{Scheme: v1alpha1.RedirectSchemeHTTPS}, request(), "https://api.shop.example/v1/orders/42?a=1&b=%20"},
		{"scheme http on http keeps the port", v1alpha1.RouteRedirect{Scheme: v1alpha1.RedirectSchemeHTTP}, request(), "http://api.shop.example:8080/v1/orders/42?a=1&b=%20"},
		{"host replaces host and port", v1alpha1.RouteRedirect{Host: "www.shop.example"}, request(), "http://www.shop.example/v1/orders/42?a=1&b=%20"},
		{"host with a port", v1alpha1.RouteRedirect{Host: "www.shop.example:8443"}, request(), "http://www.shop.example:8443/v1/orders/42?a=1&b=%20"},
		{"scheme and host", v1alpha1.RouteRedirect{Scheme: v1alpha1.RedirectSchemeHTTPS, Host: "[2001:db8::1]:8443"}, request(), "https://[2001:db8::1]:8443/v1/orders/42?a=1&b=%20"},
		{"path replaces the path", v1alpha1.RouteRedirect{Path: "/new"}, request(), "http://api.shop.example:8080/new?a=1&b=%20"},
		{"path in normalized form", v1alpha1.RouteRedirect{Path: "/a b/%7euser/%2F"}, request(), "http://api.shop.example:8080/a%20b/~user/%2F?a=1&b=%20"},
		{"path, host, scheme and query drop", v1alpha1.RouteRedirect{Scheme: v1alpha1.RedirectSchemeHTTPS, Host: "shop.example", Path: "/", Query: drop}, request(), "https://shop.example/"},
		{
			"IPv6 request host loses its port on a scheme change",
			v1alpha1.RouteRedirect{Scheme: v1alpha1.RedirectSchemeHTTPS},
			&snapshot.RedirectRequest{Scheme: "http", Host: "[2001:db8::1]:8080", Path: "/x"}, "https://[2001:db8::1]/x",
		},
		{
			"https request on https keeps its port",
			v1alpha1.RouteRedirect{Scheme: v1alpha1.RedirectSchemeHTTPS, Path: "/y"},
			&snapshot.RedirectRequest{Scheme: "https", Host: "api.shop.example:8443", Path: "/x"}, "https://api.shop.example:8443/y",
		},
		{
			"empty query writes no ?",
			v1alpha1.RouteRedirect{},
			&snapshot.RedirectRequest{Scheme: "https", Host: "api.shop.example", Path: "/x"}, "https://api.shop.example/x",
		},
		{
			"a request without a host gets an absolute-path reference",
			v1alpha1.RouteRedirect{Path: "/new"},
			&snapshot.RedirectRequest{Scheme: "http", Path: "/x", RawQuery: "q=1"}, "/new?q=1",
		},
		{
			"a hostless reference never starts with //",
			v1alpha1.RouteRedirect{},
			&snapshot.RedirectRequest{Scheme: "http", Path: "//evil.example/x"}, "/.//evil.example/x",
		},
		{
			"a hostless request takes host",
			v1alpha1.RouteRedirect{Host: "shop.example"},
			&snapshot.RedirectRequest{Scheme: "http", Path: "//evil.example/x"}, "http://shop.example//evil.example/x",
		},
		{
			"a request path with repeated slashes stays under the request's host",
			v1alpha1.RouteRedirect{Scheme: v1alpha1.RedirectSchemeHTTPS},
			&snapshot.RedirectRequest{Scheme: "http", Host: "shop.example", Path: "//evil.example/x"}, "https://shop.example//evil.example/x",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := compileOK(t, routeWith(exactMatch("/v1/orders/42"), tc.r)).Location(tc.in)
			if err != nil || got != tc.want {
				t.Errorf("Location() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// TestLocationPrefix: prefix replaces the match.path.prefix the request
// matched, a trailing slash of either ignored, so segments join with one
// slash; prefix / removes the matched prefix.
func TestLocationPrefix(t *testing.T) {
	for _, tc := range []struct {
		match, prefix, path, want string
	}{
		{"/foo", "/xyz", "/foo/bar", "/xyz/bar"},
		{"/foo", "/xyz/", "/foo/bar", "/xyz/bar"},
		{"/foo/", "/xyz", "/foo/bar", "/xyz/bar"},
		{"/foo/", "/xyz/", "/foo/bar", "/xyz/bar"},
		{"/foo", "/xyz", "/foo", "/xyz"},
		{"/foo", "/xyz", "/foo/", "/xyz/"},
		{"/foo", "/", "/foo/bar", "/bar"},
		{"/foo", "/", "/foo/", "/"},
		{"/foo", "/", "/foo", "/"},
		{"/", "/v2", "/orders", "/v2/orders"},
		{"/", "/v2/", "/", "/v2/"},
		{"/v1", "/v2", "/v1//orders", "/v2//orders"},
		{"/%7Ev1", "/v2", "/~v1/x", "/v2/x"},
		{"/v1", "/v%32", "/v1/x", "/v2/x"},
	} {
		r := compileOK(t, routeWith(prefixMatch(tc.match), v1alpha1.RouteRedirect{Prefix: tc.prefix, Query: ptr(v1alpha1.RedirectQueryDrop)}))
		got, err := r.Location(&snapshot.RedirectRequest{Scheme: "https", Host: "shop.example", Path: tc.path})
		if want := "https://shop.example" + tc.want; err != nil || got != want {
			t.Errorf("prefix %s to %s on %s: Location() = %q, %v; want %q", tc.match, tc.prefix, tc.path, got, err, want)
		}
	}
}

// TestCompileRefuses: Compile refuses with RZ-CFG-005 what validation
// refuses first, a locationExpression that does not compile with the
// builder's RZ-CFG-014, and one over the static cost ceiling with
// RZ-CFG-015.
func TestCompileRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   *v1alpha1.Route
		code string
	}{
		{"no redirect", &v1alpha1.Route{}, CodeInvalid},
		{"status 303", routeWith(exactMatch("/a"), v1alpha1.RouteRedirect{Status: ptr[int32](303)}), CodeInvalid},
		{"host with user information", routeWith(exactMatch("/a"), v1alpha1.RouteRedirect{Host: "user@shop.example"}), CodeInvalid},
		{"path with a query", routeWith(exactMatch("/a"), v1alpha1.RouteRedirect{Path: "/b?c"}), CodeInvalid},
		{"path with a dot segment", routeWith(exactMatch("/a"), v1alpha1.RouteRedirect{Path: "/b/../c"}), CodeInvalid},
		{"prefix on an exact Route", routeWith(exactMatch("/a"), v1alpha1.RouteRedirect{Prefix: "/b"}), CodeInvalid},
		{"prefix without match.path", routeWith(nil, v1alpha1.RouteRedirect{Prefix: "/b"}), CodeInvalid},
		{"path and prefix", routeWith(prefixMatch("/a"), v1alpha1.RouteRedirect{Path: "/b", Prefix: "/c"}), CodeInvalid},
		{"locationExpression beside host", routeWith(exactMatch("/a"), v1alpha1.RouteRedirect{LocationExpression: `"https://a.example/"`, Host: "b.example"}), CodeInvalid},
		{"locationExpression syntax", routeWith(exactMatch("/a"), v1alpha1.RouteRedirect{LocationExpression: `"https://a.example/" +`}), "RZ-CFG-014"},
		{"locationExpression type", routeWith(exactMatch("/a"), v1alpha1.RouteRedirect{LocationExpression: `request.method == "GET"`}), "RZ-CFG-014"},
		{"locationExpression variable", routeWith(exactMatch("/a"), v1alpha1.RouteRedirect{LocationExpression: `response.headers["location"]`}), "RZ-CFG-014"},
		{
			"locationExpression over the cost ceiling",
			routeWith(exactMatch("/a"), v1alpha1.RouteRedirect{LocationExpression: `request.headers.exists(k, request.headers.exists(j, k + j == "x")) ? "https://a.example/" : "https://b.example/"`}),
			"RZ-CFG-015",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.rt, builder(t), site())
			if code, _ := errcode.CodeOf(err); code != tc.code {
				t.Errorf("Compile() = %v, want %s", err, tc.code)
			}
		})
	}
}

// varsWith returns the activation of a locationExpression over a GET of
// https://shop.example/a?q=1 with header fields h.
func varsWith(h http.Header) *expr.Vars {
	req := &expr.Request{Method: http.MethodGet, Scheme: "https", Host: "shop.example", Path: "/a", Header: h}
	req.SetRawQuery("q=1")
	return &expr.Vars{Request: req, Route: &expr.Route{Name: "moved"}}
}

// TestLocationExpression: locationExpression builds the whole Location
// over the request's Base variables; a result is trimmed of outer spaces
// and tabs and must be a non-empty URI reference of at most 8 KiB without
// control bytes, else ErrLocation; a runtime error, the cost limit
// included, fails the Location with the program's error.
func TestLocationExpression(t *testing.T) {
	compileExpr := func(src string) *Redirect {
		return compileOK(t, routeWith(exactMatch("/a"), v1alpha1.RouteRedirect{Status: ptr[int32](308), LocationExpression: src}))
	}
	r := compileExpr(`"https://" + request.host + "/v2" + request.path + "?" + url.queryEscape(request.query.q)`)
	if r.Status() != 308 {
		t.Errorf("Status() = %d, want 308", r.Status())
	}
	got, err := r.Location(&snapshot.RedirectRequest{Vars: varsWith(http.Header{})})
	if want := "https://shop.example/v2/a?1"; err != nil || got != want {
		t.Errorf("Location() = %q, %v; want %q", got, err, want)
	}
	for _, tc := range []struct {
		name, src, want string
	}{
		{"relative reference", `request.path + "/"`, "/a/"},
		{"outer spaces trimmed", `" https://a.example/ \t"`, "https://a.example/"},
		{"reserved characters", `"https://a.example/p;q=1/[x]@y!$&'()*+,=?k=v#f"`, "https://a.example/p;q=1/[x]@y!$&'()*+,=?k=v#f"},
	} {
		got, err := compileExpr(tc.src).Location(&snapshot.RedirectRequest{Vars: varsWith(http.Header{})})
		if err != nil || got != tc.want {
			t.Errorf("%s: Location() = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		name, src string
	}{
		{"empty", `""`},
		{"spaces only", `"  "`},
		{"a CR LF", `"https://a.example/\r\nSet-Cookie: x=1"`},
		{"a NUL", `"https://a.example/\u0000"`},
		{"a space inside", `"https://a.example/a b"`},
		{"a non-ASCII byte", `"https://a.example/é"`},
		{"a quote", `"https://a.example/\""`},
		{"over 8 KiB", `"https://a.example/" + request.headers["x-long"]`},
		{"not a URI", `"%zz"`},
	} {
		h := http.Header{"X-Long": {strings.Repeat("a", MaxLocationBytes)}}
		_, err := compileExpr(tc.src).Location(&snapshot.RedirectRequest{Vars: varsWith(h)})
		if !errors.Is(err, ErrLocation) {
			t.Errorf("%s: Location() error = %v, want ErrLocation", tc.name, err)
		}
	}
	_, err = compileExpr(`request.headers["x-missing"]`).Location(&snapshot.RedirectRequest{Vars: varsWith(http.Header{})})
	var ee *expr.EvalError
	if !errors.As(err, &ee) || errors.Is(err, ErrLocation) {
		t.Errorf("missing header: Location() error = %v, want the program's runtime error", err)
	}
	// 150,000 tokens charge about 1,080,000 units, past the 1,000,000-unit
	// runtime limit.
	long := http.Header{"X-Scope": {strings.TrimSpace(strings.Repeat("x ", 150_000))}}
	costly := compileExpr(`request.headers["x-scope"].split(" ").exists(s, s == "never") ? "https://a.example/" : "https://b.example/"`)
	_, err = costly.Location(&snapshot.RedirectRequest{Vars: varsWith(long)})
	if !errors.As(err, &ee) || ee.Kind != expr.KindCostLimit {
		t.Errorf("past the cost limit: Location() error = %v, want KindCostLimit", err)
	}
	got, err = costly.Location(&snapshot.RedirectRequest{Vars: varsWith(http.Header{"X-Scope": {"never"}})})
	if err != nil || got != "https://a.example/" {
		t.Errorf("after the cost limit: Location() = %q, %v", got, err)
	}
}
