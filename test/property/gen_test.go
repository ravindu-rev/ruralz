// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package property

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The random Bundles of FuzzPrecedence, FuzzEffectiveChain and FuzzDiff:
// a Gateway, Upstreams, Routes, one Consumer and Policies of every served
// type but auth.mtls, attached at random, written as source files.

// policyKind is one Policy type the generator attaches, with a config the
// offline checks accept and the facts of foundation pack section 10 the
// oracles read.
type policyKind struct {
	typ    v1alpha1.PolicyType
	class  phase.Class
	scopes phase.ScopeSet
	// slot is the registry slot; "" means the Policy's own name.
	slot   string
	config string
	// body: the type runs in onRequestBody at Gateway or Route scope with
	// class validation, so a chain with a cache Policy beside it is
	// RZ-CFG-038, which the generator never builds.
	body bool
	// cache: the cache type.
	cache bool
	// remote: building it starts a fetch from the network (a JWKS), which
	// a Node's compile must not need in a test.
	remote bool
}

// The scope sets of policyKind.scopes, as phase.Scopes builds them.
const (
	scopesGR  = phase.ScopeSet(1<<phase.ScopeGateway | 1<<phase.ScopeRoute)
	scopesR   = phase.ScopeSet(1 << phase.ScopeRoute)
	scopesU   = phase.ScopeSet(1 << phase.ScopeUpstream)
	scopesGRU = phase.ScopeSet(1<<phase.ScopeGateway | 1<<phase.ScopeRoute | 1<<phase.ScopeUpstream)
)

// secretToken is replaced in a config by the secretRef file path.
const secretToken = "@SECRET@"

// policyKinds returns the types the generator draws from.
func policyKinds() []policyKind {
	return []policyKind{
		{typ: v1alpha1.PolicyTypeCORS, class: phase.ClassCORS, scopes: scopesGR, slot: "cors", config: `{allowOrigins: ["https://a.example"], allowMethods: [GET]}`},
		{typ: v1alpha1.PolicyTypeAuthAPIKey, class: phase.ClassAuth, scopes: scopesGR, slot: "auth", config: `{header: x-api-key}`},
		{typ: v1alpha1.PolicyTypeAuthBasic, class: phase.ClassAuth, scopes: scopesGR, slot: "auth", config: `{}`},
		{
			typ: v1alpha1.PolicyTypeAuthJWT, class: phase.ClassAuth, scopes: scopesGR, slot: "auth", remote: true,
			config: `{issuers: [{issuer: "https://idp.example", jwksUrl: "https://idp.example/jwks.json", audiences: [api]}]}`,
		},
		{typ: v1alpha1.PolicyTypeAuthzCEL, class: phase.ClassAuthz, scopes: scopesGR, config: `{rule: 'request.method == "GET"'}`},
		{typ: v1alpha1.PolicyTypeAuthzIP, class: phase.ClassAuthz, scopes: scopesGR, config: `{allow: ["10.0.0.0/8"]}`},
		{typ: v1alpha1.PolicyTypeRateLimit, class: phase.ClassAdmission, scopes: scopesGR, config: `{key: source.ip, limits: [{requests: 100, window: 1s}]}`},
		{typ: v1alpha1.PolicyTypeQuota, class: phase.ClassAdmission, scopes: scopesGR, config: `{consumerQuota: q1}`},
		{typ: v1alpha1.PolicyTypeValidationJSONSchema, class: phase.ClassValidation, scopes: scopesR, slot: "validation", body: true, config: `{schema: {type: object}}`},
		{typ: v1alpha1.PolicyTypeCache, class: phase.ClassCache, scopes: scopesR, slot: "cache", cache: true, config: `{}`},
		{
			typ: v1alpha1.PolicyTypeHeaders, class: phase.ClassTransform, scopes: scopesGRU,
			config: `{request: {set: [{name: x-a, value: "1"}]}, response: {set: [{name: x-b, value: "2"}]}}`,
		},
		{typ: v1alpha1.PolicyTypeTransformRequest, class: phase.ClassTransform, scopes: scopesGRU, config: `{remove: [{target: header, name: x-drop}]}`},
		{typ: v1alpha1.PolicyTypeTransformResponse, class: phase.ClassTransform, scopes: scopesGRU, config: `{remove: [{target: header, name: x-drop}]}`},
		{
			typ: v1alpha1.PolicyTypeAuthUpstreamOAuth2, class: phase.ClassUpstreamAuth, scopes: scopesU, slot: "upstream-auth",
			config: `{tokenUrl: "https://idp.example/token", clientId: gw, clientSecret: {secretRef: {provider: file, name: "` + secretToken + `"}}}`,
		},
	}
}

// genPolicy is one generated Policy.
type genPolicy struct {
	name string
	kind policyKind
	// slot is spec.slot; "" leaves the default.
	slot string
	// pinned is overridable: false.
	pinned bool
}

// effectiveSlot is the slot the precedence rules compare: spec.slot, else
// the registry slot, else the name.
func (p *genPolicy) effectiveSlot() string {
	switch {
	case p.slot != "":
		return p.slot
	case p.kind.slot != "":
		return p.kind.slot
	default:
		return p.name
	}
}

// genUpstream is one generated Upstream.
type genUpstream struct {
	name     string
	policies []string
	// ports are the ports of its endpoints on 127.0.0.1, a map list.
	ports []int
}

// genRoute is one generated Route.
type genRoute struct {
	name      string
	upstreams []string
	policies  []string
	exclude   []string
	// methods is spec.match.methods, a set list; nil leaves it out.
	methods []string
}

// genBundle is one generated Bundle.
type genBundle struct {
	policies  []*genPolicy
	gateway   []string
	upstreams []*genUpstream
	routes    []*genRoute
	// timeout is each Route's spec.timeout by name, when set.
	timeout map[string]string
	// labels is a label of each resource, by "Kind/name", when set.
	labels map[string]string
	// requests is the limit of each ratelimit Policy by name, when set.
	requests map[string]int
	// secrets is the secretRef file of each auth.upstream-oauth2 Policy
	// by name, when it is not the Bundle's.
	secrets map[string]string
}

// clone returns a deep copy of g.
func (g *genBundle) clone() *genBundle {
	out := &genBundle{
		gateway: slices.Clone(g.gateway), timeout: maps.Clone(g.timeout), labels: maps.Clone(g.labels),
		requests: maps.Clone(g.requests), secrets: maps.Clone(g.secrets),
	}
	for _, p := range g.policies {
		cp := *p
		out.policies = append(out.policies, &cp)
	}
	for _, u := range g.upstreams {
		out.upstreams = append(out.upstreams, &genUpstream{name: u.name, policies: slices.Clone(u.policies), ports: slices.Clone(u.ports)})
	}
	for _, rt := range g.routes {
		out.routes = append(out.routes, &genRoute{
			name: rt.name, upstreams: slices.Clone(rt.upstreams), policies: slices.Clone(rt.policies),
			exclude: slices.Clone(rt.exclude), methods: slices.Clone(rt.methods),
		})
	}
	return out
}

// Generation modes: a valid Bundle, or one with violations of exactly one
// precedence rule.
const (
	genValid = iota
	// genSlot puts two distinct Policies with one slot in one list:
	// RZ-CFG-018.
	genSlot
	// genPinned excludes or replaces an overridable: false Gateway Policy:
	// RZ-CFG-019.
	genPinned
	// genScope attaches a type at a scope it does not allow: RZ-CFG-020.
	genScope
	numGenModes
)

// genOptions shape a generated Bundle.
type genOptions struct {
	mode int
	// node keeps out the types a Node cannot build without the network.
	node bool
}

// generate draws a Bundle from r.
func generate(r *rand.Rand, o genOptions) *genBundle {
	g := &genBundle{timeout: map[string]string{}, labels: map[string]string{}, requests: map[string]int{}, secrets: map[string]string{}}
	var kinds []policyKind
	for _, k := range policyKinds() {
		if !o.node || !k.remote {
			kinds = append(kinds, k)
		}
	}
	for i := range 4 + r.IntN(9) {
		p := &genPolicy{name: "p" + strconv.Itoa(i), kind: pick(r, kinds)}
		if r.IntN(4) == 0 {
			p.slot = pick(r, []string{"s1", "s2", "auth"})
		}
		p.pinned = r.IntN(4) == 0
		g.policies = append(g.policies, p)
	}
	for i := range 1 + r.IntN(3) {
		g.upstreams = append(g.upstreams, &genUpstream{name: "u" + strconv.Itoa(i), policies: g.draw(r, phase.ScopeUpstream, 3), ports: []int{9001 + i}})
	}
	g.gateway = g.draw(r, phase.ScopeGateway, 5)
	for i := range 1 + r.IntN(3) {
		rt := &genRoute{name: "r" + strconv.Itoa(i), policies: g.draw(r, phase.ScopeRoute, 4)}
		for _, u := range g.upstreams {
			if r.IntN(2) == 0 || len(rt.upstreams) == 0 && u == g.upstreams[len(g.upstreams)-1] {
				rt.upstreams = append(rt.upstreams, u.name)
			}
		}
		for _, name := range g.gateway {
			if r.IntN(4) == 0 {
				rt.exclude = append(rt.exclude, name)
			}
		}
		if r.IntN(3) == 0 {
			g.timeout[rt.name] = pick(r, []string{"1s", "2s", "250ms", "1m"})
		}
		if r.IntN(3) == 0 {
			rt.methods = []string{"GET", "POST"}
		}
		g.routes = append(g.routes, rt)
	}
	g.repair()
	switch o.mode {
	case genSlot:
		g.breakSlot(r)
	case genPinned:
		g.breakPinned(r)
	case genScope:
		g.breakScope(r, kinds)
	}
	return g
}

// policy returns the Policy name.
func (g *genBundle) policy(name string) *genPolicy {
	for _, p := range g.policies {
		if p.name == name {
			return p
		}
	}
	return nil
}

// draw returns up to n distinct Policy names in random order, of types
// allowed at scope.
func (g *genBundle) draw(r *rand.Rand, scope phase.Scope, n int) []string {
	var out []string
	for _, i := range r.Perm(len(g.policies)) {
		p := g.policies[i]
		if len(out) < n && p.kind.scopes&phase.Scopes(scope) != 0 && r.IntN(2) == 0 {
			out = append(out, p.name)
		}
	}
	return out
}

// repair removes every violation of the precedence rules: a later entry
// of a list in the slot of an earlier one, an exclusion of an
// overridable: false Gateway Policy, a Route Policy in the slot of one,
// and a validation Policy wherever a cache Policy is attached at Gateway or
// Route scope (RZ-CFG-038, outside the precedence laws).
func (g *genBundle) repair() {
	cache := slices.ContainsFunc(g.policies, func(p *genPolicy) bool {
		return p.kind.cache && (slices.Contains(g.gateway, p.name) || slices.ContainsFunc(g.routes, func(rt *genRoute) bool { return slices.Contains(rt.policies, p.name) }))
	})
	clean := func(list []string) []string {
		seen := map[string]bool{}
		var out []string
		for _, name := range list {
			p := g.policy(name)
			if seen[p.effectiveSlot()] || cache && p.kind.body {
				continue
			}
			seen[p.effectiveSlot()] = true
			out = append(out, name)
		}
		return out
	}
	g.gateway = clean(g.gateway)
	for _, u := range g.upstreams {
		u.policies = clean(u.policies)
	}
	for _, rt := range g.routes {
		rt.policies = clean(rt.policies)
		rt.exclude = slices.DeleteFunc(rt.exclude, func(name string) bool { return g.policy(name).pinned })
		remaining := g.remaining(rt)
		rt.policies = slices.DeleteFunc(rt.policies, func(name string) bool {
			slot := g.policy(name).effectiveSlot()
			return slices.ContainsFunc(remaining, func(gw string) bool { p := g.policy(gw); return p.pinned && p.effectiveSlot() == slot })
		})
	}
}

// remaining returns the Gateway Policies a Route does not exclude, in
// Gateway list order.
func (g *genBundle) remaining(rt *genRoute) []string {
	var out []string
	for _, name := range g.gateway {
		if !slices.Contains(rt.exclude, name) {
			out = append(out, name)
		}
	}
	return out
}

// addPolicy adds a Policy of kind k with spec.slot slot and returns its
// name.
func (g *genBundle) addPolicy(k policyKind, slot string) string {
	n := len(g.policies)
	for g.policy("x"+strconv.Itoa(n)) != nil {
		n++
	}
	p := &genPolicy{name: "x" + strconv.Itoa(n), kind: k, slot: slot}
	g.policies = append(g.policies, p)
	return p.name
}

// kindAt returns a kind allowed at scope that is neither cache nor body
// (no RZ-CFG-038), with a fixed slot of its own name.
func kindAt(r *rand.Rand, scope phase.Scope) policyKind {
	var ks []policyKind
	for _, k := range policyKinds() {
		if k.scopes&phase.Scopes(scope) != 0 && !k.cache && !k.body && !k.remote && k.slot == "" {
			ks = append(ks, k)
		}
	}
	return pick(r, ks)
}

// breakSlot adds, to a random list, a Policy in the slot of one of its
// entries that is not overridable: false and, in the Gateway list, whose
// slot no Route list holds: RZ-CFG-018 alone.
func (g *genBundle) breakSlot(r *rand.Rand) {
	type list struct {
		scope phase.Scope
		names *[]string
	}
	var lists []list
	lists = append(lists, list{phase.ScopeGateway, &g.gateway})
	for _, rt := range g.routes {
		lists = append(lists, list{phase.ScopeRoute, &rt.policies})
	}
	for _, u := range g.upstreams {
		lists = append(lists, list{phase.ScopeUpstream, &u.policies})
	}
	for _, i := range r.Perm(len(lists)) {
		l := lists[i]
		for _, name := range *l.names {
			p := g.policy(name)
			slot := p.effectiveSlot()
			if p.pinned || l.scope == phase.ScopeGateway && g.routeHoldsSlot(slot) || l.scope == phase.ScopeRoute && g.pinnedSlot(slot) {
				continue
			}
			dup := g.addPolicy(kindAt(r, l.scope), slot)
			*l.names = append(*l.names, dup)
			return
		}
	}
	// No list holds an entry to collide with: start one.
	k := kindAt(r, phase.ScopeUpstream)
	u := g.upstreams[0]
	u.policies = append(u.policies, g.addPolicy(k, "dup"), g.addPolicy(k, "dup"))
}

// routeHoldsSlot reports a Route list entry in slot.
func (g *genBundle) routeHoldsSlot(slot string) bool {
	return slices.ContainsFunc(g.routes, func(rt *genRoute) bool {
		return slices.ContainsFunc(rt.policies, func(name string) bool { return g.policy(name).effectiveSlot() == slot })
	})
}

// pinnedSlot reports an overridable: false Gateway Policy in slot.
func (g *genBundle) pinnedSlot(slot string) bool {
	return slices.ContainsFunc(g.gateway, func(name string) bool { p := g.policy(name); return p.pinned && p.effectiveSlot() == slot })
}

// breakPinned makes a Route exclude or replace an overridable: false
// Gateway Policy: RZ-CFG-019 alone.
func (g *genBundle) breakPinned(r *rand.Rand) {
	if len(g.gateway) == 0 {
		g.gateway = append(g.gateway, g.addPolicy(kindAt(r, phase.ScopeGateway), ""))
	}
	target := g.policy(pick(r, g.gateway))
	target.pinned = true
	rt := pick(r, g.routes)
	rt.exclude = slices.DeleteFunc(rt.exclude, func(name string) bool { return name == target.name })
	if r.IntN(2) == 0 {
		rt.exclude = append(rt.exclude, target.name)
		return
	}
	slot := target.effectiveSlot()
	if slices.ContainsFunc(rt.policies, func(name string) bool { return g.policy(name).effectiveSlot() == slot }) {
		return
	}
	rt.policies = append(rt.policies, g.addPolicy(kindAt(r, phase.ScopeRoute), slot))
}

// breakScope attaches, at a random scope, a new Policy of a type that
// scope does not allow, in a slot of its own: RZ-CFG-020 alone.
func (g *genBundle) breakScope(r *rand.Rand, kinds []policyKind) {
	scope := pick(r, []phase.Scope{phase.ScopeGateway, phase.ScopeRoute, phase.ScopeUpstream})
	var ks []policyKind
	for _, k := range kinds {
		if k.scopes&phase.Scopes(scope) == 0 && !k.cache && !k.body {
			ks = append(ks, k)
		}
	}
	name := g.addPolicy(pick(r, ks), "out-of-scope")
	switch scope {
	case phase.ScopeGateway:
		g.gateway = append(g.gateway, name)
	case phase.ScopeRoute:
		rt := pick(r, g.routes)
		rt.policies = append(rt.policies, name)
	default:
		u := pick(r, g.upstreams)
		u.policies = append(u.policies, name)
	}
}

// files writes the Bundle as source files: the Gateway in ruralz.yaml,
// the rest in one file per kind; secret is the path the secretRef of an
// auth.upstream-oauth2 Policy names.
func (g *genBundle) files(secret string) map[string][]byte {
	refs := func(names []string) string {
		var parts []string
		for _, n := range names {
			parts = append(parts, "{name: "+n+"}")
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	meta := func(kind, name string) string {
		if l, ok := g.labels[kind+"/"+name]; ok {
			return fmt.Sprintf("metadata: {name: %s, labels: {tier: %q}}\n", name, l)
		}
		return "metadata: {name: " + name + "}\n"
	}
	var b strings.Builder
	b.WriteString("apiVersion: ruralz/v1alpha1\nkind: Gateway\n" + meta("Gateway", "edge"))
	b.WriteString("spec:\n  listeners:\n    - {name: http, protocol: http, port: 8080}\n")
	b.WriteString("  policies: " + refs(g.gateway) + "\n")
	files := map[string][]byte{"ruralz.yaml": []byte(b.String())}

	b.Reset()
	b.WriteString("apiVersion: ruralz/v1alpha1\nkind: Consumer\n" + meta("Consumer", "c0"))
	b.WriteString("spec:\n  credentials:\n    apiKeys:\n      - {name: k1, hash: \"sha256:d1bec0f9342e5607570b2636ed0256023b13be7d7d4bb45e1bed3cfee80dc1b6\"}\n")
	b.WriteString("  quotas:\n    - {name: q1, unit: requests, limit: 1000, window: 1h}\n")
	files["consumers.yaml"] = []byte(b.String())

	var docs []string
	for _, p := range g.policies {
		var d strings.Builder
		d.WriteString("apiVersion: ruralz/v1alpha1\nkind: Policy\n" + meta("Policy", p.name) + "spec:\n")
		d.WriteString("  type: " + string(p.kind.typ) + "\n")
		if p.slot != "" {
			d.WriteString("  slot: " + p.slot + "\n")
		}
		if p.pinned {
			d.WriteString("  overridable: false\n")
		}
		cfg := p.kind.config
		if n, ok := g.requests[p.name]; ok && p.kind.typ == v1alpha1.PolicyTypeRateLimit {
			cfg = fmt.Sprintf(`{key: source.ip, limits: [{requests: %d, window: 1s}]}`, n)
		}
		path := secret
		if s, ok := g.secrets[p.name]; ok {
			path = s
		}
		d.WriteString("  config: " + strings.ReplaceAll(cfg, secretToken, path) + "\n")
		docs = append(docs, d.String())
	}
	files["policies.yaml"] = []byte(strings.Join(docs, "---\n"))

	docs = docs[:0]
	for _, u := range g.upstreams {
		var eps []string
		for _, port := range u.ports {
			eps = append(eps, fmt.Sprintf("{address: \"127.0.0.1:%d\"}", port))
		}
		docs = append(docs, "apiVersion: ruralz/v1alpha1\nkind: Upstream\n"+meta("Upstream", u.name)+
			fmt.Sprintf("spec:\n  protocol: http\n  endpoints: [%s]\n  policies: %s\n", strings.Join(eps, ", "), refs(u.policies)))
	}
	files["upstreams.yaml"] = []byte(strings.Join(docs, "---\n"))

	docs = docs[:0]
	for _, rt := range g.routes {
		var d strings.Builder
		d.WriteString("apiVersion: ruralz/v1alpha1\nkind: Route\n" + meta("Route", rt.name) + "spec:\n")
		if rt.methods != nil {
			d.WriteString("  match: {path: {prefix: /" + rt.name + "}, methods: [" + strings.Join(rt.methods, ", ") + "]}\n")
		} else {
			d.WriteString("  match: {path: {prefix: /" + rt.name + "}}\n")
		}
		d.WriteString("  upstreams: " + refs(rt.upstreams) + "\n")
		d.WriteString("  policies: " + refs(rt.policies) + "\n")
		if len(rt.exclude) > 0 {
			d.WriteString("  excludePolicies: " + refs(rt.exclude) + "\n")
		}
		if t, ok := g.timeout[rt.name]; ok {
			d.WriteString("  timeout: " + t + "\n")
		}
		docs = append(docs, d.String())
	}
	files["routes.yaml"] = []byte(strings.Join(docs, "---\n"))
	return files
}
