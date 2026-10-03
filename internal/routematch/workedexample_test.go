// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch_test

import (
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/routematch"
)

// route is a Route of the worked example: a name and its criteria.
type route struct {
	name string
	c    routematch.Criteria
}

// workedExample is the six Routes of DP "Worked example: routing
// precedence" (04 req 32).
func workedExample() []route {
	return []route{
		{"orders-export", routematch.Criteria{
			Hosts: []string{"api.shop.example"}, Path: &routematch.PathCriteria{Exact: "/v1/orders/export"}, Methods: []string{"GET"},
		}},
		{"orders-get", routematch.Criteria{
			Hosts: []string{"api.shop.example"}, Path: &routematch.PathCriteria{Template: "/v1/orders/{orderId}"}, Methods: []string{"GET"},
		}},
		{"orders-summary", routematch.Criteria{
			Hosts: []string{"api.shop.example"}, Path: &routematch.PathCriteria{Template: "/v1/orders/{orderId}/summary"}, Methods: []string{"GET"},
		}},
		{"orders-any", routematch.Criteria{
			Hosts: []string{"api.shop.example"}, Path: &routematch.PathCriteria{Prefix: "/v1/orders"},
		}},
		{"tenant-wild", routematch.Criteria{
			Hosts: []string{"*.shop.example"}, Path: &routematch.PathCriteria{Prefix: "/v1"},
		}},
		{"fallback", routematch.Criteria{
			Path: &routematch.PathCriteria{Prefix: "/"},
		}},
	}
}

// workedRequests are the six requests of the worked example plus the two
// the 04 test plan (item 6) adds, and normalization cases on top.
var workedRequests = []struct {
	method, host, path string
	winner             string
	params             []string
}{
	{"GET", "api.shop.example", "/v1/orders/export", "orders-export", nil},
	{"POST", "api.shop.example", "/v1/orders/export", "orders-any", nil},
	{"GET", "api.shop.example", "/v1/orders/42", "orders-get", []string{"42"}},
	{"GET", "api.shop.example", "/v1/orders/42/summary", "orders-summary", []string{"42"}},
	{"GET", "eu.shop.example", "/v1/orders/42", "tenant-wild", nil},
	{"GET", "shop.example", "/v1/orders", "fallback", nil},
	{"DELETE", "api.shop.example", "/v1/orders", "orders-any", nil},
	{"GET", "other.example", "/x", "fallback", nil},
	// Normalization reaches the same Routes (04 req 23, 24).
	{"GET", "API.Shop.Example:8443", "/v1/%6Frders/./export", "orders-export", nil},
	{"GET", "api.shop.example.", "/v1/orders/a%2fb", "orders-get", []string{"a/b"}},
	{"GET", "api.shop.example", "/v1/x/../orders/42/summary", "orders-summary", []string{"42"}},
	{"GET", "api.shop.example", "/v1/orders/42/", "orders-any", nil},
	{"GET", "api.shop.example", "/v1/ordersX", "tenant-wild", nil},
	{"GET", "shop.example", "/v1/ordersX", "fallback", nil},
	{"GET", "a.b.shop.example", "/v1", "tenant-wild", nil},
}

// candidate is one (Route, host entry) pair with its rank.
type candidate struct {
	route *route
	rank  routematch.Rank
	tmpl  routematch.Template
	path  string // normalized exact path or prefix
}

// compile turns the Routes into candidates, one per host entry.
func compile(t *testing.T, routes []route) []candidate {
	t.Helper()
	var out []candidate
	for i := range routes {
		r := &routes[i]
		hosts := []routematch.HostPattern{{}}
		if len(r.c.Hosts) > 0 {
			hosts = hosts[:0]
			for _, h := range r.c.Hosts {
				p, err := routematch.ParseHostPattern(h)
				if err != nil {
					t.Fatal(err)
				}
				hosts = append(hosts, p)
			}
		}
		for _, h := range hosts {
			rank, err := routematch.RankOf(h, &r.c, r.name)
			if err != nil {
				t.Fatal(err)
			}
			cand := candidate{route: r, rank: rank, tmpl: rank.Template, path: rank.Prefix}
			if p := r.c.Path; p != nil && p.Exact != "" {
				if cand.path, err = routematch.NormalizePath(p.Exact); err != nil {
					t.Fatal(err)
				}
			}
			out = append(out, cand)
		}
	}
	slices.SortFunc(out, func(a, b candidate) int { return routematch.Compare(a.rank, b.rank) })
	return out
}

// pathMatches applies the path criterion of cand to a normalized path.
func (cand *candidate) pathMatches(path string, raw []string) ([]string, bool) {
	switch cand.rank.Path {
	case routematch.PathExact:
		return raw, path == cand.path
	case routematch.PathTemplate:
		return cand.tmpl.Match(path, raw)
	case routematch.PathPrefix:
		return raw, routematch.PrefixMatch(cand.path, path)
	case routematch.PathRegex:
		return raw, false // no regex Route in the worked example
	default:
		return raw, true
	}
}

// referenceMatch is a brute-force matcher over the primitives: every
// candidate in Compare order, the first whose criteria hold wins.
func referenceMatch(cands []candidate, method, host, path string) (string, []string) {
	for i := range cands {
		cand := &cands[i]
		if !cand.rank.Host.Match(host) || !routematch.MethodMatch(cand.route.c.Methods, method) {
			continue
		}
		if raw, ok := cand.pathMatches(path, nil); ok {
			return cand.route.name, decode(raw)
		}
	}
	return "", nil
}

// hostIndex is the per-host entry of the indexed matcher: a segment trie of
// exact and template candidates and the other candidates in rank order.
type hostIndex struct {
	trie  routematch.Trie[*candidate]
	other []*candidate
}

// indexMatch is the Router's structure built from the primitives: host
// tables, then per host entry the trie (ranks 2 and 3) merged with the
// prefix and any-path candidates by Compare.
func indexMatch(tab *routematch.HostTable[hostIndex], method, host, path string) (string, []string) {
	var name string
	var params []string
	buf := make([]string, 0, 4)
	tab.Lookup(host, func(_ routematch.HostPattern, idx *hostIndex) bool {
		// The trie yields exact and template candidates in rank order;
		// merge them with the prefix and any-path candidates.
		var ordered []*candidate
		var raws [][]string
		idx.trie.Lookup(path, buf, func(c *candidate, raw []string) bool {
			ordered = append(ordered, c)
			raws = append(raws, slices.Clone(raw))
			return true
		})
		for _, c := range idx.other {
			if _, ok := c.pathMatches(path, nil); ok {
				ordered = append(ordered, c)
				raws = append(raws, nil)
			}
		}
		for i, c := range ordered {
			if routematch.MethodMatch(c.route.c.Methods, method) {
				name, params = c.route.name, decode(raws[i])
				return false
			}
		}
		return true
	})
	return name, params
}

func decode(raw []string) []string {
	var out []string
	for _, r := range raw {
		out = append(out, routematch.DecodeParam(r))
	}
	return out
}

func buildIndex(cands []candidate) *routematch.HostTable[hostIndex] {
	var tab routematch.HostTable[hostIndex]
	for i := range cands {
		c := &cands[i]
		idx := tab.Entry(c.rank.Host)
		switch c.rank.Path {
		case routematch.PathExact:
			idx.trie.InsertExact(c.path, c)
		case routematch.PathTemplate:
			idx.trie.InsertTemplate(c.tmpl, c)
		case routematch.PathPrefix, routematch.PathRegex, routematch.PathAny:
			idx.other = append(idx.other, c)
		}
	}
	return &tab
}

// TestWorkedExample: the DP worked example holds exactly on the primitives
// (04 req 32; WP-05 "Done when"), through a brute-force matcher and through
// the host table and segment trie.
func TestWorkedExample(t *testing.T) {
	routes := workedExample()
	cands := compile(t, routes)
	tab := buildIndex(cands)
	for _, rq := range workedRequests {
		host := routematch.NormalizeHost(rq.host)
		path, err := routematch.NormalizePath(rq.path)
		if err != nil {
			t.Fatalf("%s %s%s: %v", rq.method, rq.host, rq.path, err)
		}
		name, params := referenceMatch(cands, rq.method, host, path)
		if name != rq.winner || !slices.Equal(params, rq.params) {
			t.Errorf("reference: %s %s%s -> %s %q; want %s %q", rq.method, rq.host, rq.path, name, params, rq.winner, rq.params)
		}
		name, params = indexMatch(tab, rq.method, host, path)
		if name != rq.winner || !slices.Equal(params, rq.params) {
			t.Errorf("index: %s %s%s -> %s %q; want %s %q", rq.method, rq.host, rq.path, name, params, rq.winner, rq.params)
		}
	}
}

// TestWorkedExampleKeys: the six Routes have distinct match keys, so none
// is RZ-CFG-023, and a copy of one with reordered sets is.
func TestWorkedExampleKeys(t *testing.T) {
	seen := map[routematch.Key]string{}
	for _, r := range workedExample() {
		k := routematch.MatchKey(&r.c)
		if prev, dup := seen[k]; dup {
			t.Errorf("%s and %s share key %s", r.name, prev, k)
		}
		seen[k] = r.name
	}
	dup := routematch.Criteria{Hosts: []string{"API.shop.example"}, Path: &routematch.PathCriteria{Exact: "/v1/orders/export"}, Methods: []string{"GET"}}
	if seen[routematch.MatchKey(&dup)] != "orders-export" {
		t.Error("a case-folded copy of orders-export is not identical")
	}
}
