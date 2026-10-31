// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// typedResource returns a hub resource without a tree whose start is line
// in file f.yaml, so a finding's line names the resource.
func typedResource(k v1alpha1.Kind, name string, obj any, line int) *hub.Resource {
	return &hub.Resource{
		ID: hub.ID{Kind: k, Name: name}, Object: obj,
		Source: hub.Source{File: "f.yaml", Start: diag.Location{File: "f.yaml", Line: line, Column: 1}},
	}
}

// effectiveListeners is the oracle's listener set of a Route: its named
// listeners the Gateway has (any name without a Gateway), every Gateway
// listener without a list, and nil for "every listener" without a
// Gateway.
func effectiveListeners(rt *v1alpha1.Route, gw []string, hasGateway bool) (set []string, all bool) {
	if len(rt.Spec.Listeners) == 0 {
		if hasGateway {
			return gw, false
		}
		return nil, true
	}
	for _, l := range rt.Spec.Listeners {
		if !hasGateway || slices.Contains(gw, l) {
			set = append(set, l)
		}
	}
	return set, false
}

// TestCheckMatchesMatchesOracle: on generated Bundles, with and without a
// Gateway, checkMatches reports exactly the Routes the quadratic
// definition reports (equal routematch.MatchKey, a common effective
// listener), each against the earliest such Route, in one linear pass.
func TestCheckMatchesMatchesOracle(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // G404: a fixed seed makes the generated Bundles the same on every run.
	matches := []v1alpha1.RouteMatch{
		{Path: &v1alpha1.PathMatch{Prefix: "/a"}},
		{Path: &v1alpha1.PathMatch{Prefix: "/a/./"}, Methods: nil},
		{Hosts: []string{"x.example"}, Path: &v1alpha1.PathMatch{Prefix: "/a"}},
		{Hosts: []string{"X.Example."}, Path: &v1alpha1.PathMatch{Prefix: "/a"}},
		{Methods: []string{"GET"}},
	}
	names := []string{"a", "b", "c", "zz"}
	gwListeners := []string{"a", "b", "c"}
	v := newValidator(t, nil)
	reported := 0
	for round := range 400 {
		hasGateway := round%2 == 0
		var rs []*hub.Resource
		if hasGateway {
			gw := &v1alpha1.Gateway{Spec: v1alpha1.GatewaySpec{}}
			for _, l := range gwListeners {
				gw.Spec.Listeners = append(gw.Spec.Listeners, v1alpha1.Listener{Name: l, Protocol: "http", Port: 1})
			}
			rs = append(rs, typedResource(v1alpha1.KindGateway, "edge", gw, 1000))
		}
		n := 1 + rng.IntN(10)
		routes := make([]*v1alpha1.Route, n)
		for i := range n {
			rt := &v1alpha1.Route{Spec: v1alpha1.RouteSpec{Match: matches[rng.IntN(len(matches))]}}
			for _, l := range names {
				if rng.IntN(3) == 0 {
					rt.Spec.Listeners = append(rt.Spec.Listeners, l)
				}
			}
			routes[i] = rt
			rs = append(rs, typedResource(v1alpha1.KindRoute, fmt.Sprintf("r%02d", i), rt, i+1))
		}
		b := hub.NewBundle(rs)
		w := &work{}
		ix := v.index(b, nil, w)
		k := newKeeper(1 << 20)
		v.checkMatches(ix, locator{}, w, &sink{k: k})
		got := make([]int, n)
		for i := range got {
			got[i] = -1
		}
		for _, d := range k.list() {
			got[d.Line-1] = d.Related[0].Line - 1
			reported++
		}
		want := make([]int, n)
		for i := range n {
			want[i] = -1
			ci := routematch.CriteriaOf(&routes[i].Spec.Match)
			si, alli := effectiveListeners(routes[i], gwListeners, hasGateway)
			if !alli && len(si) == 0 {
				continue
			}
			for j := range i {
				cj := routematch.CriteriaOf(&routes[j].Spec.Match)
				if routematch.MatchKey(&ci) != routematch.MatchKey(&cj) {
					continue
				}
				sj, allj := effectiveListeners(routes[j], gwListeners, hasGateway)
				shared := alli && (allj || len(sj) > 0) || allj && len(si) > 0
				for _, l := range si {
					shared = shared || slices.Contains(sj, l)
				}
				if shared {
					want[i] = j
					break
				}
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("round %d (gateway %v): checkMatches reports %v, the oracle %v", round, hasGateway, got, want)
		}
	}
	if reported == 0 {
		t.Fatal("no generated Bundle had identical criteria; the generator tests nothing")
	}
}
