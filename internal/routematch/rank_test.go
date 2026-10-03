// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

func mustRank(t *testing.T, host string, c *Criteria, name string) Rank {
	t.Helper()
	p := HostPattern{}
	if host != "" {
		var err error
		if p, err = ParseHostPattern(host); err != nil {
			t.Fatal(err)
		}
	}
	r, err := RankOf(p, c, name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func pathC(kind, v string) *Criteria {
	c := &Criteria{Path: &PathCriteria{}}
	switch kind {
	case "exact":
		c.Path.Exact = v
	case "template":
		c.Path.Template = v
	case "prefix":
		c.Path.Prefix = v
	case "regex":
		c.Path.Regex = v
	default:
		c.Path = nil
	}
	return c
}

// TestCompareRanks has one pair per precedence rank (04 req 31; test plan
// item 6 names the pairs): the first Rank of each pair takes precedence.
func TestCompareRanks(t *testing.T) {
	withMethods := pathC("prefix", "/v1")
	withMethods.Methods = []string{"GET"}
	regexMore := pathC("regex", "/v1/.*")
	regexMore.When = "true"
	fourConstraints := &Criteria{
		Methods: []string{"GET"}, Headers: []HeaderCriteria{{Name: "a", Exact: "b"}},
		GRPC: &GRPCCriteria{Service: "s"}, When: "true",
	}
	threeConstraints := &Criteria{
		Methods: []string{"GET", "POST", "PUT"}, Headers: []HeaderCriteria{{Name: "a", Exact: "b"}, {Name: "c", Exact: "d"}},
		When: "true",
	}
	cases := []struct {
		name        string
		first, then Rank
	}{
		{"1 exact host before wildcard", mustRank(t, "api.shop.example", pathC("", ""), "z"), mustRank(t, "*.shop.example", pathC("exact", "/a"), "a")},
		{"1 longer wildcard first", mustRank(t, "*.api.shop.example", pathC("", ""), "z"), mustRank(t, "*.shop.example", pathC("exact", "/a"), "a")},
		{"1 wildcard before any host", mustRank(t, "*.example", pathC("", ""), "z"), mustRank(t, "", pathC("exact", "/a"), "a")},
		{"2 exact before template", mustRank(t, "", pathC("exact", "/v1/orders/export"), "z"), mustRank(t, "", pathC("template", "/v1/orders/{id}"), "a")},
		{"2 template before prefix", mustRank(t, "", pathC("template", "/v1/orders/{id}"), "z"), mustRank(t, "", pathC("prefix", "/v1/orders/42"), "a")},
		{"2 prefix before regex", mustRank(t, "", pathC("prefix", "/"), "z"), mustRank(t, "", regexMore, "a")},
		{"2 regex before no path", mustRank(t, "", pathC("regex", ".*"), "z"), mustRank(t, "", &Criteria{When: "true"}, "a")},
		{"3 literal beats param", mustRank(t, "", pathC("template", "/v1/{x}/export"), "z"), mustRank(t, "", pathC("template", "/{a}/orders/export"), "a")},
		{"3 decided at first differing segment", mustRank(t, "", pathC("template", "/v1/orders/{id}"), "z"), mustRank(t, "", pathC("template", "/v1/{x}/export"), "a")},
		{"3 longer template when a prefix of kinds", mustRank(t, "", pathC("template", "/v1/{x}/y"), "z"), mustRank(t, "", pathC("template", "/v1/{x}"), "a")},
		{"3 trailing slash counts as literal", mustRank(t, "", pathC("template", "/v1/{x}/"), "z"), mustRank(t, "", pathC("template", "/v1/{x}"), "a")},
		{"3 root before param", mustRank(t, "", pathC("template", "/"), "z"), mustRank(t, "", pathC("template", "/{x}"), "a")},
		{"4 longer prefix first", mustRank(t, "", pathC("prefix", "/v1/orders"), "z"), mustRank(t, "", withMethods, "a")},
		{"4 normalized prefix length", mustRank(t, "", pathC("prefix", "/v1/o"), "z"), mustRank(t, "", pathC("prefix", "/v1/%6F/.."), "a")},
		{"5 more constraints first", mustRank(t, "", fourConstraints, "z"), mustRank(t, "", threeConstraints, "a")},
		{"5 regex by constraints only", mustRank(t, "", regexMore, "z"), mustRank(t, "", pathC("regex", "^/v1/orders/[0-9]+$"), "a")},
		{"6 name in byte order", mustRank(t, "", pathC("prefix", "/v1"), "B"), mustRank(t, "", pathC("prefix", "/v2"), "a")},
		{"6 regex by name", mustRank(t, "", pathC("regex", "z"), "a"), mustRank(t, "", pathC("regex", "a"), "b")},
		{"catch-all when true ranks after path routes", mustRank(t, "", pathC("prefix", "/"), "z"), mustRank(t, "", &Criteria{When: "true"}, "a")},
	}
	for _, tc := range cases {
		if Compare(tc.first, tc.then) >= 0 || Compare(tc.then, tc.first) <= 0 {
			t.Errorf("%s: Compare = %d, reverse %d", tc.name, Compare(tc.first, tc.then), Compare(tc.then, tc.first))
		}
		if Compare(tc.first, tc.first) != 0 {
			t.Errorf("%s: a rank does not equal itself", tc.name)
		}
	}
}

// TestCompareTotalOrder checks that Compare is a total order over a mixed
// population (antisymmetric and transitive), so sorting is deterministic
// (04 req 31).
func TestCompareTotalOrder(t *testing.T) {
	hosts := []string{"", "a.example", "*.example", "*.a.example"}
	paths := [][2]string{
		{"", ""},
		{"exact", "/a"},
		{"template", "/v1/{x}"},
		{"template", "/{a}/b"},
		{"template", "/{a}/{b}/"},
		{"template", "/"},
		{"template", "/v1/{x}/y"},
		{"prefix", "/"},
		{"prefix", "/v1"},
		{"regex", "a"},
	}
	var ranks []Rank
	for i, h := range hosts {
		for j, p := range paths {
			c := pathC(p[0], p[1])
			if (i+j)%2 == 0 {
				c.Methods = []string{"GET"}
			}
			if (i+j)%3 == 0 {
				c.When = "true"
			}
			ranks = append(ranks, mustRank(t, h, c, string(rune('a'+(i*7+j)%5))))
		}
	}
	sign := func(n int) int { return min(max(n, -1), 1) }
	for _, a := range ranks {
		for _, b := range ranks {
			if sign(Compare(a, b)) != -sign(Compare(b, a)) {
				t.Fatalf("not antisymmetric: %+v %+v", a, b)
			}
			for _, c := range ranks {
				if Compare(a, b) <= 0 && Compare(b, c) <= 0 && Compare(a, c) > 0 {
					t.Fatalf("not transitive: %+v <= %+v <= %+v", a, b, c)
				}
			}
		}
	}
	// Sorting any permutation gives one order.
	sorted := slices.Clone(ranks)
	slices.SortStableFunc(sorted, Compare)
	for k := 1; k <= 5; k++ {
		// Deterministic permutations: reversed, then rotated by k.
		shuffled := slices.Clone(ranks)
		slices.Reverse(shuffled)
		shuffled = slices.Concat(shuffled[k:], shuffled[:k])
		slices.SortStableFunc(shuffled, Compare)
		for i := range sorted {
			if Compare(sorted[i], shuffled[i]) != 0 {
				t.Fatalf("order depends on input order at %d", i)
			}
		}
	}
}

func TestRankOf(t *testing.T) {
	r := mustRank(t, "*.shop.example", &Criteria{
		Path: &PathCriteria{Prefix: "/v1/%6Frders"}, Methods: []string{"GET"}, Headers: []HeaderCriteria{{Name: "a", Exact: "b"}},
		GRPC: &GRPCCriteria{Service: "s"}, When: "true",
	}, "r")
	if r.Path != PathPrefix || r.Prefix != "/v1/orders" || r.Constraints != 4 || r.Host.Tier() != HostWildcard || r.Name != "r" {
		t.Errorf("RankOf = %+v", r)
	}
	for _, c := range []*Criteria{
		{Path: &PathCriteria{Template: "/v1/{}"}},
		{Path: &PathCriteria{Prefix: "v1"}},
		{Path: &PathCriteria{Prefix: "/%00"}},
	} {
		if _, err := RankOf(HostPattern{}, c, "x"); err == nil {
			t.Errorf("RankOf(%+v) accepted an invalid path", *c.Path)
		} else if code, _ := errcode.CodeOf(err); code != CodeInvalid {
			t.Errorf("RankOf(%+v) = %v; want %s", *c.Path, err, CodeInvalid)
		}
	}
	if n := Constraints(&Criteria{Methods: []string{"GET", "POST"}, Headers: []HeaderCriteria{{}, {}}}); n != 2 {
		t.Errorf("Constraints counts entries: %d", n)
	}
}
