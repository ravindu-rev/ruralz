// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"slices"
	"strings"
	"testing"
)

// trieHit is one value yielded by a lookup with its captures.
type trieHit struct {
	name string
	raw  string
}

func buildTrie(t *testing.T, exact []string, templates []string) *Trie[string] {
	t.Helper()
	var tr Trie[string]
	for _, p := range exact {
		n, err := NormalizePath(p)
		if err != nil {
			t.Fatal(err)
		}
		tr.InsertExact(n, "exact:"+p)
	}
	for _, s := range templates {
		tp, err := ParseTemplate(s)
		if err != nil {
			t.Fatal(err)
		}
		tr.InsertTemplate(tp, s)
	}
	return &tr
}

func trieLookup(tr *Trie[string], path string) []trieHit {
	var hits []trieHit
	tr.Lookup(path, make([]string, 0, 4), func(v string, raw []string) bool {
		hits = append(hits, trieHit{v, strings.Join(raw, ",")})
		return true
	})
	return hits
}

// TestTrieLookupOrder covers the segment trie (04 req 28) and ranks 2 and 3
// (04 req 31): exact values, then templates with a literal before a
// parameter, segment by segment from the left.
func TestTrieLookupOrder(t *testing.T) {
	templates := []string{
		"/{a}/{b}/{c}",
		"/v1/{x}/export",
		"/v1/orders/{id}",
		"/v1/orders/{id}/summary",
		"/v1/orders/export",
		"/{a}/orders/{c}",
		"/v1/{x}/",
		"/",
		"/{a}",
	}
	exact := []string{"/v1/orders/export", "/v1/orders/%65xport", "/", "/v1//x"}
	tr := buildTrie(t, exact, templates)
	if tr.Len() != len(templates)+len(exact) {
		t.Errorf("Len() = %d", tr.Len())
	}
	cases := []struct {
		path string
		want []trieHit
	}{
		{"/v1/orders/export", []trieHit{
			{"exact:/v1/orders/export", ""},
			{"exact:/v1/orders/%65xport", ""},
			{"/v1/orders/export", ""},
			{"/v1/orders/{id}", "export"},
			{"/v1/{x}/export", "orders"},
			{"/{a}/orders/{c}", "v1,export"},
			{"/{a}/{b}/{c}", "v1,orders,export"},
		}},
		{"/v1/orders/42/summary", []trieHit{{"/v1/orders/{id}/summary", "42"}}},
		{"/v1/orders/42/", []trieHit{}},
		{"/v1/orders/", []trieHit{{"/v1/{x}/", "orders"}}},
		{"/", []trieHit{{"exact:/", ""}, {"/", ""}}},
		{"/v1", []trieHit{{"/{a}", "v1"}}},
		{"/v1//x", []trieHit{{"exact:/v1//x", ""}}},
		{"//", []trieHit{}},
		{"/v1/a%2Fb/export", []trieHit{{"/v1/{x}/export", "a%2Fb"}, {"/{a}/{b}/{c}", "v1,a%2Fb,export"}}},
		{"", []trieHit{}},
		{"v1", []trieHit{}},
	}
	for _, tc := range cases {
		got := trieLookup(tr, tc.path)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("Lookup(%q) =\n%q\nwant\n%q", tc.path, got, tc.want)
		}
	}
}

// TestTrieOrderMatchesCompare: the template order of a lookup equals rank 3
// of Compare over the matching templates (04 req 31).
func TestTrieOrderMatchesCompare(t *testing.T) {
	templates := []string{"/{a}/{b}/{c}", "/v1/{x}/export", "/v1/orders/{id}", "/v1/orders/export", "/{a}/orders/{c}", "/{a}/orders/export", "/v1/{b}/{c}"}
	tr := buildTrie(t, nil, templates)
	var hits []string
	tr.Lookup("/v1/orders/export", nil, func(v string, _ []string) bool { hits = append(hits, v); return true })
	ranks := make([]Rank, 0, len(templates))
	for _, s := range templates {
		r, err := RankOf(HostPattern{}, &Criteria{Path: &PathCriteria{Template: s}}, s)
		if err != nil {
			t.Fatal(err)
		}
		ranks = append(ranks, r)
	}
	slices.SortFunc(ranks, Compare)
	want := make([]string, 0, len(ranks))
	for _, r := range ranks {
		want = append(want, r.Name)
	}
	if !slices.Equal(hits, want) {
		t.Errorf("trie order %q; Compare order %q", hits, want)
	}
}

func TestTrieLookupStopAndAllocs(t *testing.T) {
	tr := buildTrie(t, []string{"/v1/orders/export"}, []string{"/v1/orders/{id}", "/{a}/{b}/{c}", "/v1/{x}/{y}"})
	for stop := 1; stop <= 4; stop++ {
		n := 0
		tr.Lookup("/v1/orders/export", nil, func(string, []string) bool { n++; return n < stop })
		if n != stop {
			t.Errorf("stop after %d: yielded %d", stop, n)
		}
	}
	buf := make([]string, 0, 4)
	count := 0
	yield := func(string, []string) bool { count++; return true }
	if allocs := testing.AllocsPerRun(100, func() { tr.Lookup("/v1/orders/export", buf, yield) }); allocs != 0 {
		t.Errorf("Lookup: %v allocations", allocs)
	}
	var empty Trie[int]
	empty.Lookup("/a", nil, func(int, []string) bool { t.Error("empty trie yielded"); return true })
}
