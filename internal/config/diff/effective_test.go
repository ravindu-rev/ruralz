// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// burstPolicy is a ratelimit Policy in its own slot.
const burstPolicy = `apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: ratelimit-burst}
spec:
  type: ratelimit
  config:
    key: "source.ip"
    limits: [{requests: 100, window: 100ms}]
`

// TestEffectiveGatewayEditAltersRoutes: one Gateway Policy edit appears in
// the chain of every Route it reaches, each change naming its Filter class
// and the Policy before it.
func TestEffectiveGatewayEditAltersRoutes(t *testing.T) {
	a := shop(t, "to")
	b := with(a, "policies/burst.yaml", burstPolicy)
	b = edit(t, b, "ruralz.yaml", "    - name: geo-block-default\n", "    - name: geo-block-default\n    - name: ratelimit-burst\n")
	d := compare(t, render(t, a), render(t, b))
	sameLines(t, "effective", chainLines(d), []string{
		"Route/cart-grpc onRequestHeaders: + ratelimit-burst (admission, after ratelimit-global)",
		"Route/orders-summary onRequestHeaders: + ratelimit-burst (admission, after ratelimit-global)",
		"Route/support-chat onRequestHeaders: + ratelimit-burst (admission, after ratelimit-global)",
	})
	if d.Summary.RouteChainsChanged != 3 {
		t.Errorf("RouteChainsChanged = %d, want 3", d.Summary.RouteChainsChanged)
	}
	text := string(d.AppendText(nil))
	if !strings.Contains(text, "; 3 Route chains changed; ") {
		t.Errorf("summary line lacks the Route chain count:\n%s", text)
	}
}

// TestEffectiveRemovalAndFirst: a Policy leaving a chain is removed,
// naming the Policy before it in FROM's chain; one that runs first says so.
func TestEffectiveRemovalAndFirst(t *testing.T) {
	a := shop(t, "to")
	b := edit(t, a, "routes/cart-grpc.yaml", "  excludePolicies:\n    - name: cors-default                # not a browser API\n", "")
	d := compare(t, render(t, a), render(t, b))
	got := chainLines(d)
	want := []string{
		"Route/cart-grpc onRequestHeaders: + cors-default (cors, first)",
		"Route/cart-grpc onResponse: + cors-default (cors, after headers-security)",
	}
	sameLines(t, "effective", got, want)
	d = compare(t, render(t, b), render(t, a))
	sameLines(t, "mirrored", chainLines(d), []string{
		"Route/cart-grpc onRequestHeaders: - cors-default (cors, first)",
		"Route/cart-grpc onResponse: - cors-default (cors, after headers-security)",
	})
}

// TestEffectiveMoves: a Policy whose place changes relative to the others
// moves; the fewest Policies needed are reported.
func TestEffectiveMoves(t *testing.T) {
	a := shop(t, "to")
	b := edit(t, a, "plugins/geo-block.yaml", "  filterClass: authz                    # runs with authz, after auth", "  filterClass: custom")
	d := compare(t, render(t, a), render(t, b))
	sameLines(t, "effective", chainLines(d), []string{
		"Route/cart-grpc onRequestHeaders: > geo-block-default (custom, after ratelimit-global)",
		"Route/orders-summary onRequestHeaders: > geo-block-default (custom, after ratelimit-orders)",
		"Route/support-chat onRequestHeaders: > geo-block-default (custom, after ratelimit-global)",
	})
}

// TestEffectiveLegs: upstream-leg changes name their leg.
func TestEffectiveLegs(t *testing.T) {
	a := shop(t, "to")
	b := edit(t, a, "upstreams/services.yaml", "  timeout: 2s\n  policies:\n    - name: headers-internal\n", "  timeout: 2s\n  policies:\n    - name: upstream-oauth\n    - name: headers-internal\n")
	d := compare(t, render(t, a), render(t, b))
	sameLines(t, "effective", chainLines(d), []string{
		"Route/orders-summary onUpstreamRequest leg inventory: + upstream-oauth (upstream-auth, first)",
	})
	js := string(d.AppendJSON(nil))
	if !strings.Contains(js, `"phase": "onUpstreamRequest",`+"\n      "+`"leg": "inventory",`) {
		t.Errorf("JSON effective change lacks its leg:\n%s", js)
	}
	if c := d.Effective[0]; c.Leg != "inventory" || c.Phase != phase.OnUpstreamRequest || c.Class != phase.ClassUpstreamAuth || c.After != "" {
		t.Errorf("change = %+v", c)
	}
}

// TestEffectiveAddedAndRemovedRoutes: a Route only one side holds has its
// whole chain added or removed.
func TestEffectiveAddedAndRemovedRoutes(t *testing.T) {
	a := shop(t, "to")
	b := with(a, "routes/health.yaml", `apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: health
spec:
  match:
    path: {exact: /health}
  excludePolicies:
    - name: jwt-default
  upstreams:
    - name: inventory
`)
	d := compare(t, render(t, a), render(t, b))
	added := chainLines(d)
	if len(added) == 0 {
		t.Fatal("an added Route has no chain changes")
	}
	for _, l := range added {
		if !strings.HasPrefix(l, "Route/health ") || !strings.Contains(l, ": + ") {
			t.Errorf("added Route change %q is not an addition to Route/health", l)
		}
	}
	if !slices.Contains(added, "Route/health onUpstreamRequest leg inventory: + headers-internal (transform, first)") {
		t.Errorf("the added Route's upstream leg is missing: %q", added)
	}
	removed := chainLines(compare(t, render(t, b), render(t, a)))
	if len(removed) != len(added) {
		t.Fatalf("removing the Route lists %d changes, adding it %d", len(removed), len(added))
	}
	for i := range added {
		if strings.Replace(added[i], ": + ", ": - ", 1) != removed[i] {
			t.Errorf("change %d: added %q, removed %q", i, added[i], removed[i])
		}
	}
}

// TestEntryChanges pins the Phase comparison: adds, removes, then the
// fewest moves, each naming its predecessor on its side.
func TestEntryChanges(t *testing.T) {
	es := func(names ...string) []hub.Entry {
		out := make([]hub.Entry, len(names))
		for i, n := range names {
			out[i] = hub.Entry{Policy: n, Class: phase.ClassAuthz}
		}
		return out
	}
	line := func(cs []ChainChange) []string {
		var out []string
		for i := range cs {
			out = append(out, strings.TrimSpace(string(appendChainChange(nil, &cs[i]))))
		}
		return out
	}
	cases := []struct {
		name string
		a, b []hub.Entry
		want []string
	}{
		{"same", es("x", "y"), es("x", "y"), nil},
		{"swap", es("x", "y"), es("y", "x"), []string{"Route/r onRequestHeaders: > x (authz, after y)"}},
		{"rotate", es("a", "b", "c", "d"), es("d", "a", "b", "c"), []string{"Route/r onRequestHeaders: > d (authz, first)"}},
		{"insert and remove", es("a", "b", "c"), es("a", "n", "c"), []string{
			"Route/r onRequestHeaders: + n (authz, after a)",
			"Route/r onRequestHeaders: - b (authz, after a)",
		}},
		{"reverse", es("a", "b", "c"), es("c", "b", "a"), []string{
			"Route/r onRequestHeaders: > b (authz, after c)",
			"Route/r onRequestHeaders: > a (authz, after b)",
		}},
		{"twice", es("a", "a"), es("a"), []string{"Route/r onRequestHeaders: - a (authz, after a)"}},
		{"empty", nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := line(entryChanges(nil, "r", phase.OnRequestHeaders, "", tc.a, tc.b))
			sameLines(t, "changes", got, tc.want)
		})
	}
}

// TestIncreasingRun pins the longest increasing run the moves keep.
func TestIncreasingRun(t *testing.T) {
	cases := []struct {
		seq  []int
		want []int
	}{
		{nil, nil},
		{[]int{0, 1, 2}, []int{0, 1, 2}},
		{[]int{1, 0}, []int{0}},
		{[]int{3, 0, 1, 2}, []int{0, 1, 2}},
		{[]int{2, 1, 0}, []int{0}},
		{[]int{0, 4, 1, 2, 3}, []int{0, 1, 2, 3}},
	}
	for _, tc := range cases {
		got := slices.Sorted(maps.Keys(increasingRun(tc.seq)))
		if !slices.Equal(got, tc.want) {
			t.Errorf("increasingRun(%v) = %v, want %v", tc.seq, got, tc.want)
		}
	}
}
