// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// workedExample is the six Routes of docs/architecture/03-data-plane.md
// "Worked example: routing precedence".
func workedExample() []spec {
	api := []string{"api.shop.example"}
	get := []string{http.MethodGet}
	return []spec{
		{"orders-export", v1alpha1.RouteMatch{Hosts: api, Path: exactPath("/v1/orders/export"), Methods: get}},
		{"orders-get", v1alpha1.RouteMatch{Hosts: api, Path: templatePath("/v1/orders/{orderId}"), Methods: get}},
		{"orders-summary", v1alpha1.RouteMatch{Hosts: api, Path: templatePath("/v1/orders/{orderId}/summary"), Methods: get}},
		{"orders-any", v1alpha1.RouteMatch{Hosts: api, Path: prefixPath("/v1/orders")}},
		{"tenant-wild", v1alpha1.RouteMatch{Hosts: []string{"*.shop.example"}, Path: prefixPath("/v1")}},
		{"fallback", v1alpha1.RouteMatch{Path: prefixPath("/")}},
	}
}

// workedRequest is a request of the worked example and its winner.
type workedRequest struct {
	rq     req
	winner string
	params []expr.Param
}

// workedRequests are the worked example's six requests, then more on the
// same Routes, normalization included.
func workedRequests() []workedRequest {
	return []workedRequest{
		{req{"GET", "api.shop.example", "/v1/orders/export", nil}, "orders-export", nil},
		{req{"POST", "api.shop.example", "/v1/orders/export", nil}, "orders-any", nil},
		{req{"GET", "api.shop.example", "/v1/orders/42", nil}, "orders-get", []expr.Param{p("orderId", "42")}},
		{req{"GET", "api.shop.example", "/v1/orders/42/summary", nil}, "orders-summary", []expr.Param{p("orderId", "42")}},
		{req{"GET", "eu.shop.example", "/v1/orders/42", nil}, "tenant-wild", nil},
		{req{"GET", "shop.example", "/v1/orders", nil}, "fallback", nil},
		{req{"DELETE", "api.shop.example", "/v1/orders", nil}, "orders-any", nil},
		{req{"GET", "other.example", "/x", nil}, "fallback", nil},
		{req{"GET", "API.Shop.Example:8443", "/v1/%6Frders/./export", nil}, "orders-export", nil},
		{req{"GET", "api.shop.example.", "/v1/orders/a%2fb", nil}, "orders-get", []expr.Param{p("orderId", "a/b")}},
		{req{"GET", "api.shop.example", "/v1/x/../orders/42/summary", nil}, "orders-summary", []expr.Param{p("orderId", "42")}},
		{req{"GET", "api.shop.example", "/v1/orders/42/", nil}, "orders-any", nil},
		{req{"GET", "api.shop.example", "/v1/ordersX", nil}, "tenant-wild", nil},
		{req{"GET", "shop.example", "/v1/ordersX", nil}, "fallback", nil},
		{req{"GET", "a.b.shop.example", "/v1", nil}, "tenant-wild", nil},
	}
}

// TestWorkedExample checks the worked example's winners and captures.
func TestWorkedExample(t *testing.T) {
	f := newFixture(t, nil, workedExample()...)
	for _, w := range workedRequests() {
		f.expect(t, w.rq, w.winner, w.params...)
	}
}

// TestCompileIgnoresInputOrder checks that every order of the Routes gives
// the same winners.
func TestCompileIgnoresInputOrder(t *testing.T) {
	specs := workedExample()
	for shift := range len(specs) {
		for _, reverse := range []bool{false, true} {
			order := make([]spec, 0, len(specs))
			for i := range specs {
				j := (i + shift) % len(specs)
				if reverse {
					j = len(specs) - 1 - j
				}
				order = append(order, specs[j])
			}
			f := newFixture(t, nil, order...)
			for _, w := range workedRequests() {
				f.expect(t, w.rq, w.winner, w.params...)
			}
		}
	}
}

// TestHostPrecedence checks rank 1: the exact host, then wildcard hosts
// with the longer suffix first, then Routes without hosts; a wildcard
// never matches its bare suffix, and the request host is compared
// lower-cased without port or trailing dot, an IPv6 literal by its
// canonical text.
func TestHostPrecedence(t *testing.T) {
	f := newFixture(t, nil,
		spec{"any", v1alpha1.RouteMatch{Path: prefixPath("/")}},
		spec{"wild-short", v1alpha1.RouteMatch{Hosts: []string{"*.shop.example"}, Path: prefixPath("/")}},
		spec{"wild-long", v1alpha1.RouteMatch{Hosts: []string{"*.eu.shop.example"}, Path: prefixPath("/")}},
		spec{"exact-host", v1alpha1.RouteMatch{Hosts: []string{"api.shop.example"}, Path: prefixPath("/")}},
		spec{"ipv6", v1alpha1.RouteMatch{Hosts: []string{"[2001:DB8:0::1]"}, Path: prefixPath("/")}},
	)
	for host, want := range map[string]string{
		"api.shop.example":      "exact-host",
		"API.SHOP.EXAMPLE:8443": "exact-host",
		"api.shop.example.":     "exact-host",
		"x.eu.shop.example":     "wild-long",
		"a.b.eu.shop.example":   "wild-long",
		"eu.shop.example":       "wild-short",
		"shop.example":          "any",
		"xshop.example":         "any",
		"other.example":         "any",
		"":                      "any",
		"[2001:db8::1]:8443":    "ipv6",
	} {
		f.expect(t, req{host: host, path: "/x"}, want)
	}
}

// TestHostTierBeatsPathTier checks that rank 1 comes before rank 2: an
// exact-host Route without a path beats a wildcard-host Route with the
// exact path.
func TestHostTierBeatsPathTier(t *testing.T) {
	f := newFixture(t, nil,
		spec{"wild-exact", v1alpha1.RouteMatch{Hosts: []string{"*.shop.example"}, Path: exactPath("/v1")}},
		spec{"host-only", v1alpha1.RouteMatch{Hosts: []string{"api.shop.example"}}},
	)
	f.expect(t, req{host: "api.shop.example", path: "/v1"}, "host-only")
	f.expect(t, req{host: "eu.shop.example", path: "/v1"}, "wild-exact")
}

// expectOrder checks that specs are tried in their order for rq: with
// specs[k:] compiled, specs[k] wins.
func expectOrder(t *testing.T, rq req, specs ...spec) {
	t.Helper()
	for k := range specs {
		f := newFixture(t, nil, specs[k:]...)
		if g := f.match(t, rq); g.route != specs[k].name || g.err != nil {
			t.Errorf("with %d Routes from %q: winner %q (%v), want %q", len(specs)-k, specs[k].name, g.route, g.err, specs[k].name)
		}
	}
}

// TestPathTierPrecedence checks rank 2 at one host entry: exact, then
// template, then prefix, then regex, then no path.
func TestPathTierPrecedence(t *testing.T) {
	expectOrder(t, req{path: "/v1/orders"},
		spec{"e-exact", v1alpha1.RouteMatch{Path: exactPath("/v1/orders")}},
		spec{"d-template", v1alpha1.RouteMatch{Path: templatePath("/v1/{x}")}},
		spec{"c-prefix", v1alpha1.RouteMatch{Path: prefixPath("/v1")}},
		spec{"b-regex", v1alpha1.RouteMatch{Path: regexPath("/v1/.*")}},
		spec{"a-any", v1alpha1.RouteMatch{Methods: []string{http.MethodGet}}},
	)
}

// TestTemplatePrecedence checks rank 3: segment by segment from the left,
// a literal beats a parameter.
func TestTemplatePrecedence(t *testing.T) {
	expectOrder(t, req{path: "/v1/b/x"},
		spec{"c", v1alpha1.RouteMatch{Path: templatePath("/v1/b/{c}")}},
		spec{"b", v1alpha1.RouteMatch{Path: templatePath("/v1/{a}/x")}},
		spec{"a", v1alpha1.RouteMatch{Path: templatePath("/{d}/b/x")}},
	)
}

// TestPrefixPrecedence checks rank 4: the longer prefix first, a trailing
// slash included.
func TestPrefixPrecedence(t *testing.T) {
	expectOrder(t, req{path: "/v1/orders/42"},
		spec{"d-slash", v1alpha1.RouteMatch{Path: prefixPath("/v1/orders/")}},
		spec{"c-orders", v1alpha1.RouteMatch{Path: prefixPath("/v1/orders")}},
		spec{"b-v1", v1alpha1.RouteMatch{Path: prefixPath("/v1")}},
		spec{"a-root", v1alpha1.RouteMatch{Path: prefixPath("/")}},
	)
}

// TestPrefixSegmentBoundaries checks that a prefix matches whole segments
// only, that a trailing slash needs the path to go on, and that an
// encoded slash is no boundary.
func TestPrefixSegmentBoundaries(t *testing.T) {
	f := newFixture(t, nil,
		spec{"v1", v1alpha1.RouteMatch{Path: prefixPath("/v1")}},
		spec{"v1-slash", v1alpha1.RouteMatch{Path: prefixPath("/v1/")}},
		spec{"a", v1alpha1.RouteMatch{Path: prefixPath("/a")}},
	)
	for path, want := range map[string]string{
		"/v1x":   "",
		"/v1":    "v1",
		"/v1/":   "v1-slash",
		"/v1//x": "v1-slash",
		"/a":     "a",
		"/a/b":   "a",
		"/a%2Fb": "",
		"/":      "",
	} {
		f.expect(t, req{path: path}, want)
	}
}

// TestConstraintsThenName checks ranks 5 and 6: more of methods, headers,
// grpc and when first, then metadata.name in byte order.
func TestConstraintsThenName(t *testing.T) {
	f := newFixture(t, nil,
		spec{"a-none", v1alpha1.RouteMatch{Path: prefixPath("/")}},
		spec{"b-alt", v1alpha1.RouteMatch{Path: prefixPath("/"), Methods: []string{http.MethodGet, http.MethodPost}}},
		spec{"y-one", v1alpha1.RouteMatch{Path: prefixPath("/"), Methods: []string{http.MethodGet}}},
		spec{"z-two", v1alpha1.RouteMatch{
			Path: prefixPath("/"), Methods: []string{http.MethodGet},
			Headers: []v1alpha1.HeaderMatch{{Name: "x-tier", Present: present(true)}},
		}},
	)
	f.expect(t, req{path: "/x", header: http.Header{"X-Tier": {"gold"}}}, "z-two")
	f.expect(t, req{path: "/x"}, "b-alt")
	f.expect(t, req{method: http.MethodPost, path: "/x"}, "b-alt")
	f.expect(t, req{method: http.MethodDelete, path: "/x"}, "a-none")
}

// TestRegexOrderIgnoresSpecificity checks that regex paths order by ranks
// 5 and 6 only: a broader expression with the smaller name wins.
func TestRegexOrderIgnoresSpecificity(t *testing.T) {
	f := newFixture(t, nil,
		spec{"b-narrow", v1alpha1.RouteMatch{Path: regexPath("/v1/orders/[0-9]+")}},
		spec{"a-broad", v1alpha1.RouteMatch{Path: regexPath("/v1/.*")}},
		spec{"c-guarded", v1alpha1.RouteMatch{Path: regexPath("/v1/orders/.*"), Methods: []string{http.MethodGet}}},
	)
	f.expect(t, req{path: "/v1/orders/42"}, "c-guarded")
	f.expect(t, req{method: http.MethodPost, path: "/v1/orders/42"}, "a-broad")
	f.expect(t, req{path: "/v2/v1/orders"}, "")
}

// TestFallthrough checks that a failed methods, headers or grpc criterion,
// or a false match.when, moves on to the next candidate.
func TestFallthrough(t *testing.T) {
	fallback := spec{"fallback", v1alpha1.RouteMatch{Path: prefixPath("/")}}
	cases := []struct {
		name  string
		route spec
		pass  req
		fail  []req
	}{
		{
			name:  "methods",
			route: spec{"guarded", v1alpha1.RouteMatch{Path: exactPath("/x"), Methods: []string{http.MethodGet}}},
			pass:  req{path: "/x"},
			fail:  []req{{method: http.MethodHead, path: "/x"}, {method: "get", path: "/x"}},
		},
		{
			name:  "headers",
			route: spec{"guarded", v1alpha1.RouteMatch{Path: exactPath("/x"), Headers: []v1alpha1.HeaderMatch{{Name: "x-tier", Exact: "gold"}}}},
			pass:  req{path: "/x", header: http.Header{"X-Tier": {"gold"}}},
			fail:  []req{{path: "/x"}, {path: "/x", header: http.Header{"X-Tier": {"silver"}}}},
		},
		{
			name:  "grpc",
			route: spec{"guarded", v1alpha1.RouteMatch{Path: prefixPath("/"), GRPC: &v1alpha1.GRPCMatch{Service: "pkg.Svc", Method: "Get"}}},
			pass:  req{method: http.MethodPost, path: "/pkg.Svc/Get"},
			fail:  []req{{method: http.MethodPost, path: "/pkg.Svc/Put"}},
		},
		{
			name:  "when",
			route: spec{"guarded", v1alpha1.RouteMatch{Path: exactPath("/x"), When: `request.method == "POST"`}},
			pass:  req{method: http.MethodPost, path: "/x"},
			fail:  []req{{path: "/x"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil, tc.route, fallback)
			f.expect(t, tc.pass, "guarded")
			for _, rq := range tc.fail {
				f.expect(t, rq, "fallback")
			}
		})
	}
}

// TestWhenErrorStopsMatching checks that a match.when runtime error ends
// matching with RZ-RT-006 instead of falling through to a less protected
// Route.
func TestWhenErrorStopsMatching(t *testing.T) {
	f := newFixture(t, nil,
		spec{"admin", v1alpha1.RouteMatch{Path: exactPath("/admin"), When: `request.headers["x-role"] == "admin"`}},
		spec{"public", v1alpha1.RouteMatch{Path: prefixPath("/")}},
	)
	g := f.match(t, req{path: "/admin"})
	if g.code != "RZ-RT-006" || g.route != "" || len(g.params) != 0 {
		t.Errorf("missing x-role = %q %v %v, want RZ-RT-006 and no Route", g.route, g.params, g.err)
	}
	var ee *expr.EvalError
	if !errors.As(g.err, &ee) || !strings.Contains(g.err.Error(), `"admin"`) {
		t.Errorf("error %v does not wrap the evaluation error or name the Route", g.err)
	}
	f.expect(t, req{path: "/admin", header: http.Header{"X-Role": {"admin"}}}, "admin")
	f.expect(t, req{path: "/admin", header: http.Header{"X-Role": {"user"}}}, "public")
}

// TestWhenSeesCandidateCaptures checks that match.when reads the
// candidate's decoded captures as request.pathParams, and none for a
// candidate without a template.
func TestWhenSeesCandidateCaptures(t *testing.T) {
	f := newFixture(t, nil,
		spec{"seven", v1alpha1.RouteMatch{Path: templatePath("/v1/{id}"), When: `request.pathParams.id == "7"`}},
		spec{"slash", v1alpha1.RouteMatch{Path: templatePath("/v1/{key}"), When: `request.pathParams.key == "a/b"`}},
		spec{"other", v1alpha1.RouteMatch{Path: templatePath("/v1/{item}")}},
		spec{"plain", v1alpha1.RouteMatch{Path: exactPath("/v1/x"), When: `size(request.pathParams) == 0`}},
	)
	f.expect(t, req{path: "/v1/7"}, "seven", p("id", "7"))
	f.expect(t, req{path: "/v1/a%2Fb"}, "slash", p("key", "a/b"))
	f.expect(t, req{path: "/v1/8"}, "other", p("item", "8"))
	f.expect(t, req{path: "/v1/x"}, "plain")
}

// TestNoRouteLeavesNoCaptures checks that a request no Route takes gets
// Route -1, no error, and an activation without captures, even after a
// template candidate's match.when saw some.
func TestNoRouteLeavesNoCaptures(t *testing.T) {
	f := newFixture(t, nil,
		spec{"one", v1alpha1.RouteMatch{Path: templatePath("/v1/{id}"), When: `request.pathParams.id == "1"`}},
	)
	g := f.match(t, req{path: "/v1/2"})
	if g.route != "" || g.err != nil || len(g.params) != 0 {
		t.Errorf("Match = %q %v %v, want no Route", g.route, g.params, g.err)
	}
}

// TestSlashesAreSignificant checks that repeated and trailing slashes
// count for exact paths and templates, and that a parameter never takes
// an empty segment.
func TestSlashesAreSignificant(t *testing.T) {
	f := newFixture(t, nil,
		spec{"exact-slash", v1alpha1.RouteMatch{Path: exactPath("/v1/")}},
		spec{"exact", v1alpha1.RouteMatch{Path: exactPath("/v1")}},
		spec{"double", v1alpha1.RouteMatch{Path: exactPath("/v1//x")}},
		spec{"root", v1alpha1.RouteMatch{Path: exactPath("/")}},
		spec{"tmpl-slash", v1alpha1.RouteMatch{Path: templatePath("/t/{id}/")}},
		spec{"tmpl", v1alpha1.RouteMatch{Path: templatePath("/t/{id}")}},
	)
	f.expect(t, req{path: "/v1/"}, "exact-slash")
	f.expect(t, req{path: "/v1"}, "exact")
	f.expect(t, req{path: "/v1//x"}, "double")
	f.expect(t, req{path: "/"}, "root")
	f.expect(t, req{path: ""}, "root")
	f.expect(t, req{path: "/v1/x"}, "")
	f.expect(t, req{path: "/t/7/"}, "tmpl-slash", p("id", "7"))
	f.expect(t, req{path: "/t/7"}, "tmpl", p("id", "7"))
	f.expect(t, req{path: "/t//"}, "")
	f.expect(t, req{path: "/t/"}, "")
}

// TestHeaderCriteria checks exact and regex criteria against the joined
// field lines, presence and absence, and case-insensitive names that read
// the canonical key first, else the smallest key in byte order.
func TestHeaderCriteria(t *testing.T) {
	f := newFixture(t, nil,
		spec{"exact", v1alpha1.RouteMatch{Path: exactPath("/e"), Headers: []v1alpha1.HeaderMatch{{Name: "X-Tier", Exact: "a, b"}}}},
		spec{"regex", v1alpha1.RouteMatch{Path: exactPath("/r"), Headers: []v1alpha1.HeaderMatch{{Name: "x-tier", Regex: "go.d"}}}},
		spec{"present", v1alpha1.RouteMatch{Path: exactPath("/p"), Headers: []v1alpha1.HeaderMatch{{Name: "X-Env", Present: present(true)}}}},
		spec{"absent", v1alpha1.RouteMatch{Path: exactPath("/a"), Headers: []v1alpha1.HeaderMatch{{Name: "X-Env", Present: present(false)}}}},
	)
	cases := []struct {
		path   string
		header http.Header
		want   string
	}{
		{"/e", http.Header{"X-Tier": {"a", "b"}}, "exact"},
		{"/e", http.Header{"X-Tier": {"a, b"}}, "exact"},
		{"/e", http.Header{"X-Tier": {"a"}}, ""},
		{"/r", http.Header{"X-Tier": {"gold"}}, "regex"},
		{"/r", http.Header{"X-Tier": {"golden"}}, ""},
		{"/r", http.Header{"x-tier": {"gold"}}, "regex"},
		{"/r", http.Header{"X-Tier": {"silver"}, "x-tier": {"gold"}}, ""},
		{"/r", http.Header{"x-TIER": {"gold"}, "x-tier": {"silver"}}, "regex"},
		{"/r", http.Header{"x-TIER": {"silver"}, "x-tier": {"gold"}}, ""},
		{"/p", http.Header{"X-Env": {"prod"}}, "present"},
		{"/p", nil, ""},
		{"/a", nil, "absent"},
		{"/a", http.Header{"x-env": {"prod"}}, ""},
	}
	for _, tc := range cases {
		f.expect(t, req{path: tc.path, header: tc.header}, tc.want)
	}
}

// TestGRPCCriteria checks the grpc criterion: the exact /service/method,
// or the /service/ prefix without a method.
func TestGRPCCriteria(t *testing.T) {
	f := newFixture(t, nil,
		spec{"get", v1alpha1.RouteMatch{GRPC: &v1alpha1.GRPCMatch{Service: "pkg.Orders", Method: "Get"}}},
		spec{"svc", v1alpha1.RouteMatch{GRPC: &v1alpha1.GRPCMatch{Service: "pkg.Orders"}}},
	)
	for path, want := range map[string]string{
		"/pkg.Orders/Get":  "get",
		"/pkg.Orders/List": "svc",
		"/pkg.Other/Get":   "",
		"/pkg.Orders":      "",
	} {
		f.expect(t, req{method: http.MethodPost, path: path}, want)
	}
}

// TestListenerHostnames checks that listener hostnames bound the listener:
// a host outside them reaches no Route, not even one without hosts.
func TestListenerHostnames(t *testing.T) {
	f := newFixture(t, []string{"api.shop.example", "*.tenant.example"},
		spec{"any", v1alpha1.RouteMatch{Path: prefixPath("/")}},
		spec{"api", v1alpha1.RouteMatch{Hosts: []string{"api.shop.example"}, Path: prefixPath("/v1")}},
	)
	cases := []struct{ host, path, want string }{
		{"api.shop.example", "/v1", "api"},
		{"api.shop.example", "/x", "any"},
		{"API.SHOP.EXAMPLE:443", "/x", "any"},
		{"x.tenant.example", "/x", "any"},
		{"tenant.example", "/x", ""},
		{"other.example", "/x", ""},
		{"", "/x", ""},
	}
	for _, tc := range cases {
		f.expect(t, req{host: tc.host, path: tc.path}, tc.want)
	}
}

// TestCapturesAreDecoded checks that captures are percent-decoded.
func TestCapturesAreDecoded(t *testing.T) {
	f := newFixture(t, nil, spec{"files", v1alpha1.RouteMatch{Path: templatePath("/files/{dir}/{name}")}})
	f.expect(t, req{path: "/files/a%2Fb/%C3%A9t%C3%A9"}, "files", p("dir", "a/b"), p("name", "été"))
	f.expect(t, req{path: "/files/x/100%25"}, "files", p("dir", "x"), p("name", "100%"))
}

// TestRouteWithSeveralHosts checks a Route reached through several host
// entries, equal spellings counted once: through each entry it ranks by
// that entry's tier.
func TestRouteWithSeveralHosts(t *testing.T) {
	f := newFixture(t, nil,
		spec{"multi", v1alpha1.RouteMatch{
			Hosts: []string{"api.shop.example", "*.shop.example", "API.Shop.Example."}, Path: prefixPath("/v1"), Methods: []string{http.MethodGet},
		}},
		spec{"wild-exact", v1alpha1.RouteMatch{Hosts: []string{"*.shop.example"}, Path: exactPath("/v1/x")}},
	)
	f.expect(t, req{host: "api.shop.example", path: "/v1/x"}, "multi")
	f.expect(t, req{host: "eu.shop.example", path: "/v1/x"}, "wild-exact")
	f.expect(t, req{method: http.MethodPost, host: "api.shop.example", path: "/v1/x"}, "wild-exact")
	f.expect(t, req{method: http.MethodPost, host: "eu.shop.example", path: "/v1/y"}, "")
	if n := f.r.hosts.Len(); n != 2 {
		t.Errorf("host entries = %d, want 2", n)
	}
}

// TestCompileRejects checks the criteria Compile refuses and their codes;
// a missing or stray match.when program is a caller error without a code.
func TestCompileRejects(t *testing.T) {
	prg := celtest.Program(t, celtest.At(expr.PlaceRouteMatchWhen), "true")
	route := func(m v1alpha1.RouteMatch) []Route { return []Route{{Name: "r", Match: &m}} }
	cases := []struct {
		name      string
		hostnames []string
		routes    []Route
		code      string
	}{
		{"wildcard inside a host", nil, route(v1alpha1.RouteMatch{Hosts: []string{"*.*.example"}}), "RZ-CFG-005"},
		{"host with a port", nil, route(v1alpha1.RouteMatch{Hosts: []string{"api.example:80"}}), "RZ-CFG-005"},
		{"unclosed template parameter", nil, route(v1alpha1.RouteMatch{Path: templatePath("/v1/{id")}), "RZ-CFG-005"},
		{"repeated template parameter", nil, route(v1alpha1.RouteMatch{Path: templatePath("/v1/{id}/{id}")}), "RZ-CFG-005"},
		{"invalid regex", nil, route(v1alpha1.RouteMatch{Path: regexPath("(")}), "RZ-CFG-005"},
		{"exact path without a slash", nil, route(v1alpha1.RouteMatch{Path: exactPath("v1")}), "RZ-CFG-005"},
		{"prefix with an encoded NUL", nil, route(v1alpha1.RouteMatch{Path: prefixPath("/v1/%00")}), "RZ-CFG-005"},
		{"path without a form", nil, route(v1alpha1.RouteMatch{Path: &v1alpha1.PathMatch{}}), "RZ-CFG-005"},
		{"path with two forms", nil, route(v1alpha1.RouteMatch{Path: &v1alpha1.PathMatch{Exact: "/a", Prefix: "/a"}}), "RZ-CFG-005"},
		{"header with two forms", nil, route(v1alpha1.RouteMatch{Headers: []v1alpha1.HeaderMatch{{Name: "x", Exact: "a", Regex: "b"}}}), "RZ-CFG-005"},
		{"header name with a space", nil, route(v1alpha1.RouteMatch{Headers: []v1alpha1.HeaderMatch{{Name: "x y", Exact: "a"}}}), "RZ-CFG-005"},
		{"grpc without a service", nil, route(v1alpha1.RouteMatch{GRPC: &v1alpha1.GRPCMatch{Method: "Get"}}), "RZ-CFG-005"},
		{"graphql", nil, route(v1alpha1.RouteMatch{GraphQL: &v1alpha1.GraphQLMatch{OperationType: "query"}}), "RZ-CFG-040"},
		{"topic", nil, route(v1alpha1.RouteMatch{Topic: "orders"}), "RZ-CFG-040"},
		{"listener hostname", []string{"api example"}, nil, "RZ-CFG-005"},
		{"identical criteria", nil, []Route{
			{Name: "b", Match: &v1alpha1.RouteMatch{Path: exactPath("/v1/orders")}},
			{Name: "a", Match: &v1alpha1.RouteMatch{Path: exactPath("/v1/%6Frders")}},
		}, "RZ-CFG-023"},
		{"match.when without a program", nil, route(v1alpha1.RouteMatch{When: "true"}), ""},
		{"program without match.when", nil, []Route{{Name: "r", Match: &v1alpha1.RouteMatch{Methods: []string{"GET"}}, When: prg}}, ""},
	}
	for _, tc := range cases {
		r, err := Compile(tc.hostnames, tc.routes)
		code, _ := errcode.CodeOf(err)
		if err == nil || r != nil || code != tc.code {
			t.Errorf("%s: Compile = %v, %v; want code %q", tc.name, r, err, tc.code)
		}
	}
	_, err := Compile(nil, []Route{
		{Name: "b", Match: &v1alpha1.RouteMatch{Methods: []string{"GET"}}},
		{Name: "a", Match: &v1alpha1.RouteMatch{Methods: []string{"GET"}}},
	})
	if err == nil || !strings.Contains(err.Error(), `routes "a" and "b"`) {
		t.Errorf("identical criteria error = %v, want both names in byte order", err)
	}
}

// TestMaxParams checks MaxParams: the most captures of any template.
func TestMaxParams(t *testing.T) {
	f := newFixture(t, nil,
		spec{"two", v1alpha1.RouteMatch{Path: templatePath("/{a}/{b}")}},
		spec{"three", v1alpha1.RouteMatch{Path: templatePath("/x/{a}/{b}/{c}")}},
		spec{"none", v1alpha1.RouteMatch{Path: exactPath("/x")}},
	)
	if n := f.r.MaxParams(); n != 3 {
		t.Errorf("MaxParams = %d, want 3", n)
	}
	if n := newFixture(t, nil, workedExample()[0]).r.MaxParams(); n != 0 {
		t.Errorf("MaxParams without templates = %d, want 0", n)
	}
}

// TestNilMatchTakesEveryRequest checks that a Route without criteria
// matches every request on the listener.
func TestNilMatchTakesEveryRequest(t *testing.T) {
	r, err := Compile(nil, []Route{{Index: 0, Name: "all"}})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{r: r, names: []string{"all"}}
	f.expect(t, req{method: http.MethodPut, host: "any.example", path: "/a/b"}, "all")
}

// TestConcurrentMatch runs Match from several goroutines on one Router,
// each with its own activation and storage.
func TestConcurrentMatch(t *testing.T) {
	f := newFixture(t, nil, workedExample()...)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				for _, w := range workedRequests() {
					f.expect(t, w.rq, w.winner, w.params...)
				}
			}
		})
	}
	wg.Wait()
}
