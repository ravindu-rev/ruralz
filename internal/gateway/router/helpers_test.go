// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"net/http"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// spec is a Route of a test: its name and match criteria, match.when
// included.
type spec struct {
	name string
	m    v1alpha1.RouteMatch
}

// fixture is a compiled Router and the names of its Routes by index.
type fixture struct {
	r     *Router
	names []string
}

// compileSpecs turns specs into Routes, compiling every match.when.
func compileSpecs(tb testing.TB, specs []spec) []Route {
	tb.Helper()
	c := celtest.Compiler(tb)
	routes := make([]Route, len(specs))
	idx := int32(0)
	for i, s := range specs {
		m := s.m
		routes[i] = Route{Index: idx, Name: s.name, Match: &m}
		if m.When != "" {
			prg, iss := c.NewBuilder(nil, expr.InputCaps{}).Compile(celtest.At(expr.PlaceRouteMatchWhen), m.When)
			if prg == nil {
				tb.Fatalf("compile match.when %q: %+v", m.When, iss)
			}
			routes[i].When = prg
		}
		idx++
	}
	return routes
}

// newFixture compiles specs for a listener with hostnames.
func newFixture(tb testing.TB, hostnames []string, specs ...spec) *fixture {
	tb.Helper()
	r, err := Compile(hostnames, compileSpecs(tb, specs))
	if err != nil {
		tb.Fatalf("Compile: %v", err)
	}
	f := &fixture{r: r}
	for _, s := range specs {
		f.names = append(f.names, s.name)
	}
	return f
}

// req is a request of a test; the method defaults to GET.
type req struct {
	method, host, path string
	header             http.Header
}

// got is a Match result in test terms.
type got struct {
	// route is the winner's name, "" for none.
	route  string
	params []expr.Param
	// code is the RZ code of the error, "" for none.
	code string
	err  error
}

// match normalizes rq as the handler does and runs Match with a fresh
// activation, checking that the activation ends with the winner's
// captures.
func (f *fixture) match(tb testing.TB, rq req) got {
	tb.Helper()
	path, err := routematch.NormalizePath(rq.path)
	if err != nil {
		tb.Fatalf("NormalizePath(%q): %v", rq.path, err)
	}
	method := rq.method
	if method == "" {
		method = http.MethodGet
	}
	v := &expr.Vars{Request: &expr.Request{
		Method: method, Scheme: "https", Host: routematch.NormalizeHost(rq.host), Path: path, Header: rq.header,
	}}
	q := snapshot.MatchRequest{Host: rq.host, Path: path, Method: method, Header: rq.header, Vars: v}
	res := f.r.Match(tb.Context(), &q, make([]expr.Param, 0, f.r.MaxParams()))
	if !slices.Equal(v.Request.PathParams, res.Params) {
		tb.Errorf("%s %s%s: request.pathParams = %v after Match, want the result's %v", method, rq.host, rq.path, v.Request.PathParams, res.Params)
	}
	g := got{params: res.Params, err: res.Err}
	if res.Route >= 0 {
		g.route = f.names[res.Route]
	}
	g.code, _ = errcode.CodeOf(res.Err)
	if res.Err != nil && res.Route != -1 {
		tb.Errorf("%s %s%s: Route = %d with error %v, want -1", method, rq.host, rq.path, res.Route, res.Err)
	}
	return g
}

// expect checks that rq reaches route with params (nil for none) and no
// error.
func (f *fixture) expect(tb testing.TB, rq req, route string, params ...expr.Param) {
	tb.Helper()
	g := f.match(tb, rq)
	if g.err != nil || g.route != route || !slices.Equal(g.params, params) && len(g.params)+len(params) > 0 {
		tb.Errorf("%s %s%s = %q %v (%v), want %q %v", rq.method, rq.host, rq.path, g.route, g.params, g.err, route, params)
	}
}

// p returns a capture whose path segment needs no decoding: its raw form
// is its value.
func p(name, value string) expr.Param { return expr.Param{Name: name, Value: value, Raw: value} }

// pr returns a capture decoded to value from the percent-encoded segment
// raw.
func pr(name, value, raw string) expr.Param { return expr.Param{Name: name, Value: value, Raw: raw} }

// prefixPath, exactPath, templatePath and regexPath return path criteria.
func prefixPath(s string) *v1alpha1.PathMatch   { return &v1alpha1.PathMatch{Prefix: s} }
func exactPath(s string) *v1alpha1.PathMatch    { return &v1alpha1.PathMatch{Exact: s} }
func templatePath(s string) *v1alpha1.PathMatch { return &v1alpha1.PathMatch{Template: s} }
func regexPath(s string) *v1alpha1.PathMatch    { return &v1alpha1.PathMatch{Regex: s} }

// present returns a pointer to b for headers[].present.
func present(b bool) *bool { return &b }
