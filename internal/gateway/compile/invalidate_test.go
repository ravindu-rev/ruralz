// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// uri is a Route's URI criteria for TestDisjoint.
type uri struct {
	listeners []string
	hosts     []string
	path      *v1alpha1.PathMatch
}

// shape returns the URI shape of u.
func (u uri) shape() uriShape {
	rt := &v1alpha1.Route{Spec: v1alpha1.RouteSpec{Match: v1alpha1.RouteMatch{Hosts: u.hosts, Path: u.path}}}
	ls := u.listeners
	if ls == nil {
		ls = []string{"http"}
	}
	return shapeOf(rt, ls)
}

func exact(p string) *v1alpha1.PathMatch    { return &v1alpha1.PathMatch{Exact: p} }
func prefix(p string) *v1alpha1.PathMatch   { return &v1alpha1.PathMatch{Prefix: p} }
func template(p string) *v1alpha1.PathMatch { return &v1alpha1.PathMatch{Template: p} }
func regex(p string) *v1alpha1.PathMatch    { return &v1alpha1.PathMatch{Regex: p} }

// TestDisjoint covers the proof that two Routes share no URI: by their
// listeners, their hosts (exact hosts, "*." wildcards matching one or more
// labels, no hosts matching any) or their exact, prefix and template
// paths (prefixes on segment boundaries, template parameters taking one
// non-empty segment, a trailing slash significant); no path and a regex
// prove nothing. The relation is symmetric.
func TestDisjoint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		a, b     uri
		disjoint bool
	}{
		{"same everything", uri{path: prefix("/a")}, uri{path: prefix("/a")}, false},
		{"other listener", uri{listeners: []string{"public"}, path: prefix("/a")}, uri{listeners: []string{"internal"}, path: prefix("/a")}, true},
		{"one shared listener", uri{listeners: []string{"internal", "public"}}, uri{listeners: []string{"public"}}, false},
		{"other exact host", uri{hosts: []string{"a.example"}}, uri{hosts: []string{"b.example"}}, true},
		{"same exact host", uri{hosts: []string{"a.example", "c.example"}}, uri{hosts: []string{"B.example", "C.example"}}, false},
		{"wildcard covers host", uri{hosts: []string{"*.shop.example"}}, uri{hosts: []string{"api.shop.example"}}, false},
		{"wildcard needs a label", uri{hosts: []string{"*.shop.example"}}, uri{hosts: []string{"shop.example"}}, true},
		{"nested wildcards", uri{hosts: []string{"*.shop.example"}}, uri{hosts: []string{"*.example"}}, false},
		{"sibling wildcards", uri{hosts: []string{"*.shop.example"}}, uri{hosts: []string{"*.ample"}}, true},
		{"any host", uri{hosts: []string{"a.example"}}, uri{}, false},
		{"exact paths", uri{path: exact("/a")}, uri{path: exact("/b")}, true},
		{"equal exact paths", uri{path: exact("/a")}, uri{path: exact("/a")}, false},
		{"exact under prefix", uri{path: exact("/a/b")}, uri{path: prefix("/a")}, false},
		{"exact beside prefix", uri{path: exact("/ab")}, uri{path: prefix("/a")}, true},
		{"nested prefixes", uri{path: prefix("/a")}, uri{path: prefix("/a/b")}, false},
		{"prefix on a segment boundary", uri{path: prefix("/a")}, uri{path: prefix("/ab")}, true},
		{"trailing-slash prefix", uri{path: prefix("/a/")}, uri{path: prefix("/a")}, false},
		{"exact matches template", uri{path: exact("/users/7")}, uri{path: template("/users/{id}")}, false},
		{"exact too long for template", uri{path: exact("/users/7/x")}, uri{path: template("/users/{id}")}, true},
		{"exact other literal", uri{path: exact("/orders/7")}, uri{path: template("/users/{id}")}, true},
		{"root prefix and template", uri{path: prefix("/")}, uri{path: template("/")}, false},
		{"prefix covers template", uri{path: prefix("/users")}, uri{path: template("/users/{id}")}, false},
		{"prefix through a parameter", uri{path: prefix("/users/7")}, uri{path: template("/users/{id}")}, false},
		{"prefix longer than template", uri{path: prefix("/users/7/x")}, uri{path: template("/users/{id}")}, true},
		{"prefix other literal", uri{path: prefix("/orders")}, uri{path: template("/users/{id}")}, true},
		{"slash prefix before more segments", uri{path: prefix("/users/")}, uri{path: template("/users/{id}")}, false},
		{"slash prefix at the template's end", uri{path: prefix("/users/7/")}, uri{path: template("/users/{id}")}, true},
		{"slash prefix and trailing template", uri{path: prefix("/users/7/")}, uri{path: template("/users/{id}/")}, false},
		{"templates", uri{path: template("/users/{id}")}, uri{path: template("/users/me")}, false},
		{"templates of other lengths", uri{path: template("/users/{id}")}, uri{path: template("/users/{id}/orders")}, true},
		{"templates of other literals", uri{path: template("/users/{id}")}, uri{path: template("/orders/{id}")}, true},
		{"templates and a trailing slash", uri{path: template("/users/{id}/")}, uri{path: template("/users/{id}")}, true},
		{"no path", uri{}, uri{path: exact("/a")}, false},
		{"regex", uri{path: regex("^/a$")}, uri{path: exact("/b")}, false},
		{"hosts prove despite paths", uri{hosts: []string{"a.example"}, path: prefix("/")}, uri{hosts: []string{"b.example"}, path: prefix("/")}, true},
	} {
		a, b := tc.a.shape(), tc.b.shape()
		if got := disjoint(&a, &b); got != tc.disjoint {
			t.Errorf("%s: disjoint(a, b) = %v, want %v", tc.name, got, tc.disjoint)
		}
		if got := disjoint(&b, &a); got != tc.disjoint {
			t.Errorf("%s: disjoint(b, a) = %v, want %v", tc.name, got, tc.disjoint)
		}
	}
}

// entries returns the invalidation entries of rt: its Phase-less Policies.
func entries(rt *snapshot.Route) []*snapshot.Policy {
	var out []*snapshot.Policy
	for _, p := range rt.Chain.Policies {
		if p.Phases.Empty() {
			out = append(out, p)
		}
	}
	return out
}

// TestInvalidatingRoutes covers the invalidation entries: a Route without
// a cache Policy that may share a URI with a cache Route ends its
// Policies with the cache Policy of the overlapping cache Route whose
// stateStoreTimeout is largest, a Phase-less copy without when whose
// Filter, the cache Route's own, the executor calls Invalidate on, Index
// its position; a Route provably disjoint from every cache Route, or with
// a cache Policy of its own, gets none, and no Phase array holds an entry.
func TestInvalidatingRoutes(t *testing.T) {
	h := newHarness(t)
	s := h.mustCompile(h.validate(
		gateway(""),
		upstreamDoc("orders", "127.0.0.1:9001", ""),
		routeDoc("products", "/products", "orders", attach("cache-a")),
		routeDoc("special", "/products/special", "orders", attach("cache-b")),
		routeDoc("writer", "/products/", "orders", attach("hdr")),
		routeDoc("orders", "/orders", "orders", attach("hdr")),
		doc("Route", "pattern", "  match:\n    path:\n      regex: \"^/p.*\"\n  upstreams:\n    - name: orders\n"),
		routeDoc("own", "/products/own", "orders", attach("cache-a", "hdr")),
		routeDoc("single", "/products/single", "orders", ""),
		policyDoc("cache-a", "cache", cacheConfig, "  stateStoreTimeout: 30ms\n"),
		policyDoc("cache-b", "cache", cacheConfig, "  stateStoreTimeout: 70ms\n  when: request.method == \"GET\"\n"),
		policyDoc("hdr", "headers", headersConfig, ""),
	), nil).Snapshot
	byName := map[string]*snapshot.Route{}
	for _, rt := range s.Routes {
		byName[rt.Name] = rt
	}
	var cacheB *snapshot.Policy
	for _, p := range byName["special"].Chain.Policies {
		if p.Name == "cache-b" {
			cacheB = p
		}
	}
	for _, tc := range []struct {
		route string
		want  []string
	}{
		{"writer", []string{"cache-b"}},
		{"pattern", []string{"cache-b"}},
		{"single", []string{"cache-a"}},
		{"orders", nil},
		{"own", nil},
		{"products", nil},
		{"special", nil},
	} {
		rt := byName[tc.route]
		got := entries(rt)
		if !slices.Equal(names(got), tc.want) {
			t.Errorf("Route %s invalidation entries = %v, want %v", tc.route, names(got), tc.want)
			continue
		}
		for _, p := range got {
			if p.Index != len(rt.Chain.Policies)-1 || p.When != nil || p.FirstPhase != 0 || p.Type != v1alpha1.PolicyTypeCache {
				t.Errorf("Route %s entry %+v, want the last Policy, without when or Phase", tc.route, *p)
			}
			if _, ok := p.Filter.(filter.Invalidator); !ok {
				t.Errorf("Route %s entry Filter %T is no Invalidator", tc.route, p.Filter)
			}
			if p.Name == "cache-b" && (p.Filter != cacheB.Filter || p.StateStoreTimeout != 70*time.Millisecond || p == cacheB) {
				t.Errorf("Route %s entry = %+v, want a copy of special's cache-b with its Filter and 70ms", tc.route, *p)
			}
		}
		for ph := range rt.Chain.Client {
			for _, p := range rt.Chain.Client[ph] {
				if p.Phases.Empty() {
					t.Errorf("Route %s Phase %d holds the invalidation entry %s", tc.route, ph, p.Name)
				}
			}
		}
	}
	if cacheB.When == nil || cacheB.Phases.Empty() {
		t.Error("the cache Route's own Policy lost its when or Phases")
	}
}

// TestInvalidatingRoutesTies covers the choice among overlapping cache
// Routes with the same stateStoreTimeout, the first by Route name, and a
// cache Policy whose Filter does not invalidate, which gives no entry.
func TestInvalidatingRoutesTies(t *testing.T) {
	h := newHarness(t)
	h.behave(v1alpha1.PolicyTypeCache, "plain-cache", behavior{plain: true})
	s := h.mustCompile(h.validate(
		gateway(""),
		upstreamDoc("orders", "127.0.0.1:9001", ""),
		routeDoc("b-cache", "/x", "orders", attach("cache-b")),
		routeDoc("a-cache", "/x/y", "orders", attach("cache-a")),
		routeDoc("z-plain", "/x/y/z", "orders", attach("plain-cache")),
		routeDoc("writer", "/", "orders", ""),
		policyDoc("cache-a", "cache", cacheConfig, ""),
		policyDoc("cache-b", "cache", cacheConfig, ""),
		policyDoc("plain-cache", "cache", cacheConfig, ""),
	), nil).Snapshot
	for _, rt := range s.Routes {
		if rt.Name != "writer" {
			continue
		}
		if got := names(entries(rt)); !slices.Equal(got, []string{"cache-a"}) {
			t.Errorf("writer entries = %v, want cache-a of a-cache, first by name", got)
		}
	}
}
