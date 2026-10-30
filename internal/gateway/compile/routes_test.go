// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/composition"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/forward"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// names returns the Policy names of ps.
func names(ps []*snapshot.Policy) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

// entryNames returns the Policy names of es.
func entryNames(es []hub.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Policy
	}
	return out
}

// TestChains covers each Route's compiled chain: its client Policies once
// each in request order (class, scope, position), Index their position,
// the auth Policies counted; per-Phase arrays in the effective chain's
// order, holding those same Policies; one LegChain per Upstream that every
// Route reaching it shares, its Policies indexed within the leg; and each
// Policy's settings from its effective chain entry with its attachment's
// Filter, consumptive view, when and metric handles.
func TestChains(t *testing.T) {
	h := newHarness(t)
	res := h.validate(
		gateway(attach("gw-key", "gw-hdr")),
		upstreamDoc("orders", "127.0.0.1:9001", attach("up-hdr", "up-tr")),
		upstreamDoc("audit", "127.0.0.1:9002", attach("audit-hdr")),
		routeDoc("a", "/a", "orders", attach("rl", "hdr")),
		doc("Route", "b", "  match:\n    path:\n      prefix: /b\n  upstreams:\n    - name: orders\n    - name: audit\n"+attach("hdr")),
		policyDoc("gw-key", "auth.api-key", `{}`, ""),
		policyDoc("gw-hdr", "headers", headersConfig, ""),
		policyDoc("rl", "ratelimit", rateLimitConfig, ""),
		policyDoc("hdr", "headers", `{"request":{"set":[{"name":"x-a","value":"1"}]},"response":{"set":[{"name":"x-b","value":"2"}]}}`, ""),
		policyDoc("up-hdr", "headers", headersConfig, ""),
		policyDoc("up-tr", "transform.request", `{"set":[{"target":"header","name":"x-t","valueExpression":"\"1\""}]}`, ""),
		policyDoc("audit-hdr", "headers", headersConfig, ""),
	)
	s := h.mustCompile(res, nil).Snapshot
	wantPolicies := map[string][]string{"a": {"gw-key", "rl", "gw-hdr", "hdr"}, "b": {"gw-key", "gw-hdr", "hdr"}}
	for _, rt := range s.Routes {
		ch := res.Validated.Chains[rt.Name]
		if got := names(rt.Chain.Policies); !slices.Equal(got, wantPolicies[rt.Name]) {
			t.Errorf("Route %s Policies = %v, want %v", rt.Name, got, wantPolicies[rt.Name])
		}
		if rt.Chain.AuthPolicies != 1 {
			t.Errorf("Route %s AuthPolicies = %d, want 1", rt.Name, rt.Chain.AuthPolicies)
		}
		for i, p := range rt.Chain.Policies {
			if p.Index != i {
				t.Errorf("Route %s: %s Index %d, want %d", rt.Name, p.Name, p.Index, i)
			}
		}
		for ph := range phase.Count {
			got := rt.Chain.Client[ph]
			if want := entryNames(ch.Client[ph]); !slices.Equal(names(got), want) {
				t.Errorf("Route %s %v = %v, want the effective chain's %v", rt.Name, phase.Phase(ph), names(got), want)
			}
			for _, p := range got {
				if rt.Chain.Policies[p.Index] != p {
					t.Errorf("Route %s %v: %s is not its Policies entry", rt.Name, phase.Phase(ph), p.Name)
				}
			}
		}
		for _, leg := range ch.Legs {
			lc := rt.Chain.Legs[leg.Upstream]
			if lc == nil || lc != s.LegChains[leg.Upstream] || lc.Upstream != leg.Upstream {
				t.Errorf("Route %s leg %s = %p, want the snapshot's shared %p", rt.Name, leg.Upstream, lc, s.LegChains[leg.Upstream])
				continue
			}
			for ph := range phase.Count {
				if want := entryNames(leg.Phases[ph]); !slices.Equal(names(lc.Phases[ph]), want) {
					t.Errorf("leg %s %v = %v, want %v", leg.Upstream, phase.Phase(ph), names(lc.Phases[ph]), want)
				}
			}
		}
		for _, e := range distinctEntries(ch) {
			var p *snapshot.Policy
			for _, q := range rt.Chain.Policies {
				if q.Name == e.Policy && q.Scope == e.Scope {
					p = q
				}
			}
			if p == nil {
				t.Errorf("Route %s lost %s", rt.Name, e.Policy)
				continue
			}
			if p.Type != e.Type || p.Class != e.Class || p.Position != e.Position || p.FailureMode != e.FailureMode ||
				p.FirstPhase != e.FirstPhase() || p.Phases != e.Phases || p.SpanName != "ruralz.filter."+e.Policy {
				t.Errorf("Route %s %s = %+v, want the entry %+v", rt.Name, e.Policy, *p, e)
			}
			if fakeOf(p.Filter) == nil || p.Metrics == nil || p.Metrics != s.Metrics.Policy(e.Policy) {
				t.Errorf("Route %s %s: Filter %v, Metrics %p; want the built Filter and the plan's handles", rt.Name, e.Policy, p.Filter, p.Metrics)
			}
			if lf, ok := p.Filter.(*limitFilter); (p.Consumptive != nil) != ok || (ok && p.Consumptive != lf) {
				t.Errorf("Route %s %s: Consumptive %v, want the Filter when consumptive", rt.Name, e.Policy, p.Consumptive)
			}
		}
	}
	orders := s.LegChains["orders"]
	var legPolicies []*snapshot.Policy
	for ph := range phase.Count {
		for _, p := range orders.Phases[ph] {
			if !slices.Contains(legPolicies, p) {
				legPolicies = append(legPolicies, p)
			}
		}
	}
	if got := names(legPolicies); !slices.Equal(got, []string{"up-hdr", "up-tr"}) || legPolicies[0].Index != 0 || legPolicies[1].Index != 1 {
		t.Errorf("orders leg Policies = %v, want up-hdr and up-tr indexed 0 and 1 within the leg", got)
	}
	if len(s.LegChains) != 2 {
		t.Errorf("LegChains = %v, want orders and audit", s.LegChains)
	}
}

// distinctEntries returns the client entries of ch, each Policy and scope
// once.
func distinctEntries(ch *hub.Chain) []hub.Entry {
	var out []hub.Entry
	for ph := range phase.Count {
		for _, e := range ch.Client[ph] {
			if !slices.ContainsFunc(out, func(x hub.Entry) bool { return x.Policy == e.Policy && x.Scope == e.Scope }) {
				out = append(out, e)
			}
		}
	}
	return out
}

// TestRouteSettings covers each compiled Route: Routes in name order with
// Index their position, the CEL route view prepared once, the bound
// listeners (spec.listeners, else every listener) in name order, the
// protocol, the timeout (spec.timeout, else and for 0 the 15 s default),
// the largest stateStoreTimeout of its State Store Policies, its metric
// handles, and its forwarder: the plain-upstreams forwarder, or the
// composition.
func TestRouteSettings(t *testing.T) {
	h := newHarness(t)
	s := h.mustCompile(h.validate(
		doc("Gateway", "edge", "  listeners:\n    - name: public\n      protocol: http\n      port: 8080\n    - name: internal\n      protocol: http\n      port: 8081\n"+
			"  stateStore:\n    timeout: 40ms\n"),
		upstreamDoc("orders", "127.0.0.1:9001", ""),
		upstreamDoc("audit", "127.0.0.1:9002", ""),
		doc("Route", "z", "  match:\n    path:\n      prefix: /z\n  listeners: [public, internal]\n  timeout: 5s\n  upstreams:\n    - name: orders\n"+attach("rl", "cache")),
		doc("Route", "m", "  match:\n    path:\n      prefix: /m\n  listeners: [internal]\n  timeout: 0s\n  upstreams:\n    - name: orders\n"+attach("hdr")),
		"apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: c\n  labels:\n    team: shop\nspec:\n  match:\n    path:\n      prefix: /c\n"+
			"  composition:\n    mode: sequential\n    steps:\n      - name: s1\n        upstream: audit\n        path: /x\n",
		policyDoc("rl", "ratelimit", rateLimitConfig, "  stateStoreTimeout: 80ms\n"),
		policyDoc("cache", "cache", cacheConfig, ""),
		policyDoc("hdr", "headers", headersConfig, ""),
	), nil).Snapshot
	if got := names2(s.Routes); !slices.Equal(got, []string{"c", "m", "z"}) {
		t.Fatalf("Routes = %v, want name order", got)
	}
	for i, rt := range s.Routes {
		if int(rt.Index) != i || rt.View == nil || rt.View.Name != rt.Name || rt.View.Prepared == nil || rt.Metrics != s.Metrics.Route(rt.Name) ||
			rt.Protocol != v1alpha1.UpstreamProtocolHTTP {
			t.Errorf("Route %s: Index %d, View %+v, Metrics %p, Protocol %s", rt.Name, rt.Index, rt.View, rt.Metrics, rt.Protocol)
		}
	}
	c, m, z := s.Routes[0], s.Routes[1], s.Routes[2]
	if c.View.Labels["team"] != "shop" || !slices.Contains(h.cel.routes, c.View) {
		t.Errorf("Route c view = %+v, want its labels, prepared by the CEL compiler", c.View)
	}
	for _, tc := range []struct {
		rt        *snapshot.Route
		listeners []string
		timeout   time.Duration
		storeMax  time.Duration
	}{
		{c, []string{"internal", "public"}, 15 * time.Second, 0},
		{m, []string{"internal"}, 15 * time.Second, 0},
		{z, []string{"internal", "public"}, 5 * time.Second, 80 * time.Millisecond},
	} {
		if !slices.Equal(tc.rt.Listeners, tc.listeners) || tc.rt.Timeout != tc.timeout || tc.rt.StateStoreMax != tc.storeMax {
			t.Errorf("Route %s: listeners %v, timeout %v, StateStoreMax %v; want %v, %v, %v",
				tc.rt.Name, tc.rt.Listeners, tc.rt.Timeout, tc.rt.StateStoreMax, tc.listeners, tc.timeout, tc.storeMax)
		}
	}
	if _, ok := c.Forward.(*composition.Composition); !ok {
		t.Errorf("Route c Forward = %T, want the composition", c.Forward)
	}
	if _, ok := z.Forward.(*forward.Forwarder); !ok {
		t.Errorf("Route z Forward = %T, want the plain-upstreams forwarder", z.Forward)
	}
}

// names2 returns the Route names of rs.
func names2(rs []*snapshot.Route) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

// TestBodyNeeds covers each Route's body decisions: the request body is
// gated for a client Policy that gates it (an onRequestBody Policy, a
// transform.request whose config needs the body), for an Upstream-scope
// transform.request that needs it, and for a composition that does; the
// client response is gated for a client-scope transform.response whose
// config or when reads the body, never for an Upstream-scope one; it is
// teed for a cache Policy.
func TestBodyNeeds(t *testing.T) {
	const (
		bodyRule   = `{"rule":"request.body.amount < 100"}`
		bodyTR     = `{"body":"request.body"}`
		headerTR   = `{"set":[{"target":"header","name":"x-t","valueExpression":"\"1\""}]}`
		bodyResp   = `{"remove":[{"target":"body","name":"secret"}]}`
		headerResp = `{"set":[{"target":"header","name":"x-r","valueExpression":"\"1\""}]}`
	)
	for _, tc := range []struct {
		name             string
		typ, config      string
		when             string
		upstream         bool
		want             snapshot.BodyNeeds
		compositionGates bool
	}{
		{name: "response when reads the body", typ: "transform.response", config: headerResp, when: "response.body.ok == true", want: snapshot.BodyNeeds{ResponseGate: true}},
		{name: "response when reads the status", typ: "transform.response", config: headerResp, when: "response.status == 200"},
		{name: "headers", typ: "headers", config: headersConfig},
		{name: "body rule", typ: "authz.cel", config: bodyRule, want: snapshot.BodyNeeds{RequestGate: true}},
		{name: "client body transform", typ: "transform.request", config: bodyTR, want: snapshot.BodyNeeds{RequestGate: true}},
		{name: "client header transform", typ: "transform.request", config: headerTR},
		{name: "upstream body transform", typ: "transform.request", config: bodyTR, upstream: true, want: snapshot.BodyNeeds{RequestGate: true}},
		{name: "upstream header transform", typ: "transform.request", config: headerTR, upstream: true},
		{name: "client body response", typ: "transform.response", config: bodyResp, want: snapshot.BodyNeeds{ResponseGate: true}},
		{name: "client header response", typ: "transform.response", config: headerResp},
		{name: "upstream body response", typ: "transform.response", config: bodyResp, upstream: true},
		{name: "cache", typ: "cache", config: cacheConfig, want: snapshot.BodyNeeds{ResponseTee: true}},
		{name: "composition", typ: "headers", config: headersConfig, compositionGates: true, want: snapshot.BodyNeeds{RequestGate: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			upPolicies, routePolicies := "", attach("p")
			if tc.upstream {
				upPolicies, routePolicies = attach("p"), ""
			}
			route := routeDoc("r", "/", "orders", routePolicies)
			if tc.compositionGates {
				route = doc("Route", "r", "  match:\n    path:\n      prefix: /\n    methods: [POST]\n  composition:\n    mode: aggregate\n    steps:\n"+
					"      - name: s1\n        upstream: orders\n        path: /one\n      - name: s2\n        upstream: audit\n        path: /two\n"+routePolicies)
			}
			when := ""
			if tc.when != "" {
				when = "  when: '" + tc.when + "'\n"
			}
			s := h.mustCompile(h.validate(
				gateway(""), upstreamDoc("orders", "127.0.0.1:9001", upPolicies), upstreamDoc("audit", "127.0.0.1:9002", ""),
				route, policyDoc("p", tc.typ, tc.config, when),
			), nil).Snapshot
			if got := s.Routes[0].Body; got != tc.want {
				t.Errorf("Body = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestUpstreamRefusals covers the Upstreams step 10 refuses: a hashKey,
// retryOn or failureWhen the snapshot's Builder refuses is a finding at
// its field, and an Upstream the Upstream layer refuses (upstream.Check),
// such as a client certificate that does not pair with its key, is one at
// its spec; every refusal is reported and the Upstream set stays
// uncompiled, so no runtime opens.
func TestUpstreamRefusals(t *testing.T) {
	ca, err := pki.NewCA(pki.CAOptions{NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	other, err := pki.NewCA(pki.CAOptions{NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.env.set("RURALZ_SECRET_CERT", string(ca.CertPEM))
	h.env.set("RURALZ_SECRET_KEY", string(other.KeyPEM))
	const hashKey, retryOn, failureWhen = `request.headers["x-user"]`, `response.status == 502`, `response.status == 500`
	h.cel.refuse(hashKey, "RZ-CFG-015")
	h.cel.refuse(retryOn, "RZ-CFG-014")
	h.cel.refuse(failureWhen, "RZ-CFG-015")
	got := h.rejects(h.validate(
		gateway(""),
		upstreamDoc("hashed", "127.0.0.1:9001", "  loadBalancing:\n    algorithm: ring-hash\n    hashKey: '"+hashKey+"'\n"),
		upstreamDoc("retried", "127.0.0.1:9002", "  retries:\n    retryOn: '"+retryOn+"'\n  circuitBreaker:\n    failureWhen: '"+failureWhen+"'\n"),
		upstreamDoc("paired", "127.0.0.1:9003", "  tls:\n    clientCertificate: {secretRef: {provider: env, name: RURALZ_SECRET_CERT}}\n"+
			"    clientKey: {secretRef: {provider: env, name: RURALZ_SECRET_KEY}}\n"),
		upstreamDoc("fine", "127.0.0.1:9004", ""),
		routeDoc("a", "/a", "hashed", ""), routeDoc("b", "/b", "retried", ""), routeDoc("c", "/c", "paired", ""), routeDoc("d", "/d", "fine", ""),
	))
	want := []string{
		"error RZ-CFG-015 Upstream/hashed spec.loadBalancing.hashKey: " + refusedMessage,
		"error RZ-CFG-014 Upstream/retried spec.retries.retryOn: " + refusedMessage,
		"error RZ-CFG-015 Upstream/retried spec.circuitBreaker.failureWhen: " + refusedMessage,
		"error RZ-CFG-026 Upstream/paired spec:",
	}
	if len(got) != len(want) {
		t.Fatalf("findings\n%s\nwant %d", strings.Join(got, "\n"), len(want))
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			t.Errorf("finding %d = %q, want %q", i, got[i], want[i])
		}
	}
	if got := h.runtimes(); len(got) != 0 {
		t.Errorf("Upstream runtimes = %v, want none", got)
	}
}

// TestUpstreamPrograms covers the programs an Upstream runtime gets: its
// hashKey, retryOn and failureWhen compiled once at their places by the
// snapshot's Builder, a ring-hash Upstream compiling only with its
// hashKey; an Upstream no Route reaches compiles none.
func TestUpstreamPrograms(t *testing.T) {
	h := newHarness(t)
	h.mustCompile(h.validate(
		gateway(""),
		upstreamDoc("orders", "127.0.0.1:9001", "  loadBalancing:\n    algorithm: ring-hash\n    hashKey: request.path\n"+
			"  retries:\n    retryOn: 'response.status == 502'\n  circuitBreaker:\n    failureWhen: 'response.status == 500'\n"),
		upstreamDoc("idle", "127.0.0.1:9002", "  retries:\n    retryOn: 'response.status == 504'\n"),
		routeDoc("a", "/a", "orders", ""),
	), nil)
	for _, tc := range []struct {
		src   string
		place expr.PlaceID
	}{
		{"request.path", expr.PlaceHashKey},
		{"response.status == 502", expr.PlaceRetryOn},
		{"response.status == 500", expr.PlaceFailureWhen},
	} {
		if got := h.cel.sitesOf(tc.src); len(got) != 1 || !reflect.DeepEqual(got[0], expr.Site{Place: tc.place}) {
			t.Errorf("%q compiled at %+v, want once at %s", tc.src, got, tc.place)
		}
	}
	if got := h.cel.sitesOf("response.status == 504"); len(got) != 0 {
		t.Errorf("the unreached Upstream's retryOn compiled at %+v", got)
	}
	if got := h.runtimes(); !slices.Equal(got, []string{"orders"}) {
		t.Errorf("runtimes = %v, want orders", got)
	}
}

// TestForwarderRefusals covers the forwarders step 10 refuses, which
// validation refuses first: a plain-upstreams forwarder is a finding at
// the Route's spec.upstreams, a composition at its spec.composition.
func TestForwarderRefusals(t *testing.T) {
	h := newHarness(t)
	res := h.validate(
		gateway(""), upstreamDoc("orders", "127.0.0.1:9001", ""), upstreamDoc("audit", "127.0.0.1:9002", ""),
		routeDoc("a", "/a", "orders", ""),
		doc("Route", "c", "  match:\n    path:\n      prefix: /c\n  composition:\n    mode: sequential\n    steps:\n"+
			"      - name: s1\n        upstream: orders\n        path: /one\n      - name: s2\n        upstream: audit\n        path: /two\n"),
	)
	// Edit the typed views the compiler reads, past validation.
	a, _ := res.Validated.Bundle.Route("a")
	a.Spec.Upstreams = append(a.Spec.Upstreams, a.Spec.Upstreams[0])
	one := int32(1)
	gatewayOf(t, res.Validated).Spec.Limits = &v1alpha1.Limits{MaxCompositionSteps: &one}
	got := h.rejects(res)
	want := []string{
		`error RZ-CFG-005 Route/a spec.upstreams: Route "a": upstreams names Upstream "orders" twice`,
		`error RZ-CFG-031 Route/c spec.composition: composition: route "c": composition has 2 steps, more than limits.maxCompositionSteps (1)`,
	}
	if len(got) != len(want) {
		t.Fatalf("findings\n%s\nwant %d", strings.Join(got, "\n"), len(want))
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			t.Errorf("finding %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// gatewayOf returns the typed Gateway of v.
func gatewayOf(t *testing.T, v *hub.Validated) *v1alpha1.Gateway {
	t.Helper()
	for _, r := range v.Bundle.Resources() {
		if gw, ok := hub.Object[v1alpha1.Gateway](r); ok && gw != nil {
			return gw
		}
	}
	t.Fatal("no Gateway")
	return nil
}

// TestRouters covers the Routers: one per listener, over the Routes bound
// to it, matching with their compiled match.when; a match.when the
// Builder refuses is a finding at the Route's spec.match.when, and a
// Router the Router package refuses, such as two Routes with identical
// match criteria, which validation refuses first, one at the listener.
func TestRouters(t *testing.T) {
	h := newHarness(t)
	docs := []string{
		doc("Gateway", "edge", "  listeners:\n    - name: public\n      protocol: http\n      port: 8080\n    - name: internal\n      protocol: http\n      port: 8081\n"),
		upstreamDoc("orders", "127.0.0.1:9001", ""),
		routeDoc("open", "/shop", "orders", ""),
		doc("Route", "posts", "  match:\n    path:\n      prefix: /shop\n    when: request.method == \"POST\"\n  upstreams:\n    - name: orders\n"),
		doc("Route", "admin", "  match:\n    path:\n      prefix: /admin\n  listeners: [internal]\n  upstreams:\n    - name: orders\n"),
	}
	s := h.mustCompile(h.validate(docs...), nil).Snapshot
	if len(s.Routers) != 2 {
		t.Fatalf("Routers = %v, want one per listener", s.Routers)
	}
	for _, tc := range []struct{ listener, method, path, want string }{
		{"public", http.MethodGet, "/shop/cart", "open"},
		{"public", http.MethodPost, "/shop/cart", "posts"},
		{"public", http.MethodGet, "/admin", ""},
		{"internal", http.MethodGet, "/admin/x", "admin"},
		{"internal", http.MethodPost, "/shop", "posts"},
	} {
		if got := match(t, s, tc.listener, tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s %s matched %q, want %q", tc.listener, tc.method, tc.path, got, tc.want)
		}
	}

	h = newHarness(t)
	h.cel.refuse(`request.method == "POST"`, "RZ-CFG-015")
	if got := h.rejects(h.validate(docs...)); len(got) != 1 || !strings.Contains(got[0], "error RZ-CFG-015 Route/posts spec.match.when: "+refusedMessage) {
		t.Errorf("findings = %q, want the refused match.when", got)
	}

	h = newHarness(t)
	res := h.validate(docs...)
	posts, _ := res.Validated.Bundle.Route("posts")
	open, _ := res.Validated.Bundle.Route("open")
	posts.Spec.Match = open.Spec.Match
	got := h.rejects(res)
	if len(got) != 2 || !strings.Contains(got[0], `error RZ-CFG-023 Gateway/edge spec.listeners[name=public]: router: routes "open" and "posts" have identical match criteria`) ||
		!strings.Contains(got[1], `error RZ-CFG-023 Gateway/edge spec.listeners[name=internal]:`) {
		t.Errorf("findings\n%s\nwant one per listener", strings.Join(got, "\n"))
	}
}

// TestRouterWhenDeadline checks that the Routers run each match.when under
// its own Route's deadline: the request start plus the Route's effective
// timeout, its spec.timeout or the 15 s default, at which a tracked
// comprehension stops (docs/architecture/02-configuration-model.md
// "Limits").
func TestRouterWhenDeadline(t *testing.T) {
	h := newHarness(t)
	const shortWhen, defaultWhen = `request.method == "POST"`, `request.method == "PUT"`
	short, def := h.cel.spy(shortWhen), h.cel.spy(defaultWhen)
	s := h.mustCompile(h.validate(
		gateway(""),
		upstreamDoc("orders", "127.0.0.1:9001", ""),
		doc("Route", "posts", "  match:\n    path:\n      prefix: /shop\n    when: '"+shortWhen+"'\n  upstreams:\n    - name: orders\n  timeout: 30ms\n"),
		doc("Route", "puts", "  match:\n    path:\n      prefix: /shop\n    when: '"+defaultWhen+"'\n  upstreams:\n    - name: orders\n"),
	), nil).Snapshot
	req := &expr.Request{Method: http.MethodPut, Scheme: "http", Host: "shop.example", Path: "/shop", Header: http.Header{}}
	q := &snapshot.MatchRequest{
		Host: "shop.example", Path: "/shop", Method: http.MethodPut, Header: http.Header{},
		Vars: &expr.Vars{Request: req, Now: t0}, Start: t0,
	}
	res := s.Routers["http"].Match(t.Context(), q, nil)
	if res.Err != nil || res.Route < 0 || s.Routes[res.Route].Name != "puts" {
		t.Fatalf("Match = %+v, want puts", res)
	}
	if got, want := short.deadlines(), []time.Time{t0.Add(30 * time.Millisecond)}; !slices.Equal(got, want) {
		t.Errorf("posts' match.when saw deadlines %v, want %v", got, want)
	}
	if got, want := def.deadlines(), []time.Time{t0.Add(resilience.DefaultRouteTimeout)}; !slices.Equal(got, want) {
		t.Errorf("puts' match.when saw deadlines %v, want %v", got, want)
	}
}

// match returns the name of the Route the Router of listener l matches for
// a request with method and path, "" for none.
func match(t *testing.T, s *snapshot.Snapshot, l, method, path string) string {
	t.Helper()
	r := s.Routers[l]
	if r == nil {
		t.Fatalf("no Router for listener %s", l)
	}
	req := &expr.Request{Method: method, Scheme: "http", Host: "shop.example", Path: path, Header: http.Header{}}
	q := &snapshot.MatchRequest{Host: "shop.example", Path: path, Method: method, Header: http.Header{}, Vars: &expr.Vars{Request: req, Now: t0}}
	res := r.Match(t.Context(), q, nil)
	if res.Err != nil {
		t.Fatalf("Match: %v", res.Err)
	}
	if res.Route < 0 {
		return ""
	}
	return s.Routes[res.Route].Name
}

// TestNoRoutes covers a Revision without Routes: it compiles to a
// snapshot with no Route, an empty Router per listener, an empty Upstream
// set and no Filter.
func TestNoRoutes(t *testing.T) {
	h := newHarness(t)
	c := h.mustCompile(h.validate(gateway(""), upstreamDoc("idle", "127.0.0.1:9000", ""), policyDoc("hdr", "headers", headersConfig, "")), nil)
	s := c.Snapshot
	if len(s.Routes) != 0 || len(s.Routers) != 1 || s.Routers["http"] == nil || len(s.LegChains) != 0 || len(h.builds()) != 0 || c.UpstreamConnections != 0 {
		t.Errorf("snapshot: %d Routes, Routers %v, LegChains %v, %d builds, %d connections", len(s.Routes), s.Routers, s.LegChains, len(h.builds()), c.UpstreamConnections)
	}
	if got := match(t, s, "http", http.MethodGet, "/"); got != "" {
		t.Errorf("the empty Router matched %q", got)
	}
	if got := h.runtimes(); len(got) != 0 {
		t.Errorf("Upstream runtimes = %v, want none", got)
	}
}

// TestUpstreamConnections covers the connection total: the summed
// circuitBreaker.maxConnections of the Upstreams some Route reaches, 1,024
// for one that sets none.
func TestUpstreamConnections(t *testing.T) {
	h := newHarness(t)
	c := h.mustCompile(h.validate(
		gateway(""),
		upstreamDoc("orders", "127.0.0.1:9001", "  circuitBreaker:\n    maxConnections: 10\n"),
		upstreamDoc("audit", "127.0.0.1:9002", ""),
		upstreamDoc("idle", "127.0.0.1:9003", "  circuitBreaker:\n    maxConnections: 99\n"),
		routeDoc("a", "/a", "orders", ""), routeDoc("b", "/b", "audit", ""),
	), nil)
	if c.UpstreamConnections != 10+1024 {
		t.Errorf("UpstreamConnections = %d, want 1034", c.UpstreamConnections)
	}
}
