// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// reversedPhases is the order rule of every Phase, written out instead of
// derived from phase.IsResponse, so a test built on it fails when the
// code's rule changes: onUpstreamResponseHeaders, onUpstreamResponseBody
// and onResponse run in exact reverse, and so does onChunk, ordered as a
// response Phase; onLog and the request Phases (onRequestHeaders,
// onRequestBody, onRoute, onUpstreamRequest) keep request order.
func reversedPhases() map[phase.Phase]bool {
	return map[phase.Phase]bool{
		phase.OnRequestHeaders:          false,
		phase.OnRequestBody:             false,
		phase.OnRoute:                   false,
		phase.OnUpstreamRequest:         false,
		phase.OnUpstreamResponseHeaders: true,
		phase.OnUpstreamResponseBody:    true,
		phase.OnResponse:                true,
		phase.OnLog:                     false,
		phase.OnChunk:                   true,
	}
}

// inOrder returns names in request order, or reversed when rev is set.
func inOrder(rev bool, names ...string) []string {
	if rev {
		slices.Reverse(names)
	}
	return names
}

// TestReversedPhasesCoverEveryPhase keeps reversedPhases complete: a Phase
// added to the contract without an order rule here fails.
func TestReversedPhasesCoverEveryPhase(t *testing.T) {
	rules := reversedPhases()
	for p := range phase.Count {
		if _, ok := rules[p]; !ok {
			t.Errorf("no order rule for %s", p)
		}
	}
	if len(rules) != int(phase.Count) {
		t.Errorf("%d order rules for %d Phases", len(rules), phase.Count)
	}
}

// directionCase is one Phase of one leg kind and the exact order of its
// entries.
type directionCase struct {
	name  string
	build func(b *builder)
	// leg is "" for the client leg of Route r, else the Upstream of r's
	// leg.
	leg  string
	p    phase.Phase
	want []string
	// runsIn lists every Phase of that leg the case's Policies run in,
	// the ones their types select; nil means p alone.
	runsIn []phase.Phase
}

// genericDirectionCases puts, for every Phase on each leg kind, plugin
// Policies whose Plugin subscribes to that Phase alone into one chain,
// each adjacent pair differing in exactly one key: on the client leg
// class (g-auth before g-xform), scope (g-xform before r-xform) and
// position (r-xform before r-xform2); on an Upstream leg class (u-auth
// before u-xform) and position (u-xform before u-xform2). The expected
// order is request order, reversed where reversedPhases says so.
func genericDirectionCases() []directionCase {
	rev := reversedPhases()
	var out []directionCase
	for p := range phase.Count {
		if p.ClientLeg() {
			out = append(out, directionCase{
				name: "client " + p.String() + " plugins",
				build: func(b *builder) {
					b.plugin("pl", p.V1alpha1())
					b.policy("g-xform", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl", v1alpha1.FilterClassTransform))
					b.policy("g-auth", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl", v1alpha1.FilterClassAuth))
					b.policy("r-xform", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl", v1alpha1.FilterClassTransform))
					b.policy("r-xform2", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl", v1alpha1.FilterClassTransform))
					b.gateway("g-xform", "g-auth")
					b.route("r", []string{"r-xform", "r-xform2"}, nil, nil)
				},
				p:    p,
				want: inOrder(rev[p], "g-auth", "g-xform", "r-xform", "r-xform2"),
			})
		}
		if p.UpstreamLeg() {
			out = append(out, directionCase{
				name: "leg " + p.String() + " plugins",
				build: func(b *builder) {
					b.plugin("pl", p.V1alpha1())
					b.policy("u-xform", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl", v1alpha1.FilterClassTransform))
					b.policy("u-auth", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl", v1alpha1.FilterClassUpstreamAuth))
					b.policy("u-xform2", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl", v1alpha1.FilterClassTransform))
					b.upstream("up", "u-xform", "u-auth", "u-xform2")
					b.route("r", nil, nil, []string{"up"})
				},
				leg:  "up",
				p:    p,
				want: inOrder(rev[p], "u-auth", "u-xform", "u-xform2"),
			})
		}
	}
	return out
}

// typedDirectionCases are the direction cases with registry types:
// transform.response and headers on one Upstream, ai.token-budget and
// ai.guardrail at Gateway and Route (onRequestBody, onChunk, onResponse and
// onLog at once), and onRoute plugins at both client scopes.
func typedDirectionCases() []directionCase {
	xresp := &v1alpha1.TransformResponseConfig{Body: "response.body"}
	upstreamBody := func(b *builder) {
		b.plugin("pl-body", v1alpha1.PhaseOnUpstreamResponseBody)
		b.policy("xresp-a", v1alpha1.PolicyTypeTransformResponse, xresp)
		b.policy("plug-ua", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl-body", v1alpha1.FilterClassUpstreamAuth))
		b.policy("xresp-b", v1alpha1.PolicyTypeTransformResponse, xresp)
		b.upstream("up", "xresp-a", "plug-ua", "xresp-b")
		b.route("r", nil, nil, []string{"up"})
	}
	upstreamHeaders := func(b *builder) {
		b.plugin("pl-hdr", v1alpha1.PhaseOnUpstreamResponseHeaders)
		b.policy("h1", v1alpha1.PolicyTypeHeaders, headersCfg(false, true))
		b.policy("h2", v1alpha1.PolicyTypeHeaders, headersCfg(true, true))
		b.policy("plug-ua", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl-hdr", v1alpha1.FilterClassUpstreamAuth))
		b.upstream("up", "h1", "h2", "plug-ua")
		b.route("r", nil, nil, []string{"up"})
	}
	ai := func(b *builder) {
		b.policy("tb-g", v1alpha1.PolicyTypeAITokenBudget, nil)
		b.policy("gr-g", v1alpha1.PolicyTypeAIGuardrail, nil)
		b.policy("tb-r", v1alpha1.PolicyTypeAITokenBudget, nil)
		b.policy("gr-r", v1alpha1.PolicyTypeAIGuardrail, nil)
		b.gateway("tb-g", "gr-g")
		b.route("r", []string{"tb-r", "gr-r"}, nil, nil)
	}
	route := func(b *builder) {
		b.plugin("pl-route", v1alpha1.PhaseOnRoute)
		b.policy("route-g", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl-route", v1alpha1.FilterClassCustom))
		b.policy("route-r", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl-route", v1alpha1.FilterClassCustom))
		b.policy("route-auth", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl-route", v1alpha1.FilterClassAuth))
		b.gateway("route-g")
		b.route("r", []string{"route-r", "route-auth"}, nil, nil)
	}
	// The Phases each build's types select on its leg: headers per
	// configured direction, transform.response with a body in
	// onUpstreamResponseBody, ai.token-budget in onRequestBody, onChunk and
	// onLog, ai.guardrail in onRequestBody, onChunk and onResponse, and a
	// plugin in its Plugin's Phases.
	bodyIn := []phase.Phase{phase.OnUpstreamResponseBody}
	headersIn := []phase.Phase{phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders}
	aiIn := []phase.Phase{phase.OnRequestBody, phase.OnResponse, phase.OnLog, phase.OnChunk}
	routeIn := []phase.Phase{phase.OnRoute}
	return []directionCase{
		// Upstream-auth before transform, then position, all reversed.
		{"leg onUpstreamResponseBody transform.response and plugin", upstreamBody, "up", phase.OnUpstreamResponseBody, []string{"xresp-b", "xresp-a", "plug-ua"}, bodyIn},
		{"leg onUpstreamResponseHeaders headers and plugin", upstreamHeaders, "up", phase.OnUpstreamResponseHeaders, []string{"h2", "h1", "plug-ua"}, headersIn},
		{"leg onUpstreamRequest headers, request order", upstreamHeaders, "up", phase.OnUpstreamRequest, []string{"h2"}, headersIn},
		// Admission before validation, Gateway before Route; onChunk and
		// onResponse reversed, onRequestBody and onLog in request order.
		{"client onChunk ai.token-budget and ai.guardrail", ai, "", phase.OnChunk, []string{"gr-r", "gr-g", "tb-r", "tb-g"}, aiIn},
		{"client onRequestBody ai.token-budget and ai.guardrail", ai, "", phase.OnRequestBody, []string{"tb-g", "tb-r", "gr-g", "gr-r"}, aiIn},
		{"client onResponse ai.guardrail", ai, "", phase.OnResponse, []string{"gr-r", "gr-g"}, aiIn},
		{"client onLog ai.token-budget", ai, "", phase.OnLog, []string{"tb-g", "tb-r"}, aiIn},
		// onRoute is a request Phase: auth first, then Gateway before
		// Route.
		{"client onRoute plugins at Gateway and Route", route, "", phase.OnRoute, []string{"route-auth", "route-g", "route-r"}, routeIn},
	}
}

// TestPhaseDirection covers the direction rule of every Phase on each leg
// kind it runs on, onUpstreamResponseBody, onChunk (client leg and legs)
// and onRoute included. Every case asserts the exact order of its Phase,
// for one worker and for four, and that the Policies run in no other Phase
// of that leg than the ones their types select (runsIn), for the typed
// cases too.
func TestPhaseDirection(t *testing.T) {
	cases := append(genericDirectionCases(), typedDirectionCases()...)
	covered := map[string]bool{}
	for _, tc := range cases {
		for _, workers := range []int{1, 4} {
			t.Run(fmt.Sprintf("%s/workers=%d", tc.name, workers), func(t *testing.T) {
				b := newBuilder()
				tc.build(b)
				chains, ds, err := mustNew(t, nil, Options{Body: bodyOracle{}}).Run(t.Context(), b.bundle(), nil, workers)
				if err != nil || len(ds) != 0 {
					t.Fatalf("Run: %v\n%s", err, diagText(t, ds))
				}
				c := chains["r"]
				phases := &c.Client
				if tc.leg != "" {
					l, ok := c.Leg(tc.leg)
					if !ok {
						t.Fatalf("no leg %s", tc.leg)
					}
					phases = &l.Phases
				}
				if got := names(phases[tc.p]); !slices.Equal(got, tc.want) {
					t.Errorf("%s = %q, want %q", tc.p, got, tc.want)
				}
				runsIn := tc.runsIn
				if runsIn == nil {
					runsIn = []phase.Phase{tc.p}
				}
				for p := range phase.Count {
					if in := slices.Contains(runsIn, p); in != (len(phases[p]) > 0) {
						t.Errorf("%s holds %q; the case's types select %v", p, names(phases[p]), runsIn)
					}
				}
			})
		}
		kind := "client"
		if tc.leg != "" {
			kind = "leg"
		}
		if len(tc.want) >= 2 {
			covered[kind+" "+tc.p.String()] = true
		}
	}
	// Every Phase of each leg kind has a case with two or more entries.
	for p := range phase.Count {
		if p.ClientLeg() && !covered["client "+p.String()] {
			t.Errorf("no client-leg case orders two entries in %s", p)
		}
		if p.UpstreamLeg() && !covered["leg "+p.String()] {
			t.Errorf("no upstream-leg case orders two entries in %s", p)
		}
	}
}

// TestPhaseDirectionGenericOnly checks that each generic case subscribes
// its Policies to its Phase alone, so the case pins that Phase and no
// other.
func TestPhaseDirectionGenericOnly(t *testing.T) {
	for _, tc := range genericDirectionCases() {
		b := newBuilder()
		tc.build(b)
		chains, _, err := mustNew(t, nil, Options{}).Run(t.Context(), b.bundle(), nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		c := chains["r"]
		for p := range phase.Count {
			n := len(c.Client[p])
			for _, l := range c.Legs {
				n += len(l.Phases[p])
			}
			if want := len(tc.want); (p == tc.p && n != want) || (p != tc.p && n != 0) {
				t.Errorf("%s: %d entries in %s", tc.name, n, p)
			}
		}
	}
}
