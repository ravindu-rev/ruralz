// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// compositionRoute is the Route c whose sequential composition calls
// Upstream audit.
const compositionRoute = "apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: c\nspec:\n  match:\n    path:\n      prefix: /c\n" +
	"  composition:\n    mode: sequential\n    steps:\n      - name: s1\n        upstream: audit\n        path: /x\n"

// built is one Build: its Policy and scope.
type builtAt struct {
	policy string
	scope  phase.Scope
}

// TestBuildsReachedAttachmentsOnce covers what step 10 builds: a Filter for
// each Policy some Route's effective chain runs, client and upstream legs
// alike, once per attachment (Policy, scope, Phase set) however many
// Routes share it; nothing for a Policy no chain runs, an Upstream-scope
// Policy of an Upstream no Route names among them; and the Upstream
// runtimes and metric label sets of the Upstreams the Routes name in
// upstreams or a composition step only.
func TestBuildsReachedAttachmentsOnce(t *testing.T) {
	h := newHarness(t)
	c := h.mustCompile(h.validate(
		gateway(attach("gw-cors")),
		upstreamDoc("orders", "127.0.0.1:9001", attach("up-hdr")),
		upstreamDoc("audit", "127.0.0.1:9002", attach("audit-hdr")),
		upstreamDoc("idle", "127.0.0.1:9003", attach("idle-hdr")),
		routeDoc("a", "/a", "orders", attach("hdr", "rl")),
		routeDoc("b", "/b", "orders", attach("hdr")),
		compositionRoute,
		policyDoc("gw-cors", "cors", corsConfig, ""),
		policyDoc("hdr", "headers", headersConfig, ""),
		policyDoc("rl", "ratelimit", rateLimitConfig, ""),
		policyDoc("up-hdr", "headers", headersConfig, ""),
		policyDoc("audit-hdr", "headers", headersConfig, ""),
		policyDoc("idle-hdr", "headers", headersConfig, ""),
		policyDoc("unused", "headers", headersConfig, ""),
	), nil)
	var got []builtAt
	for _, env := range h.builds() {
		got = append(got, builtAt{env.Policy.Metadata.Name, env.Scope})
	}
	want := []builtAt{
		{"audit-hdr", phase.ScopeUpstream},
		{"gw-cors", phase.ScopeGateway},
		{"hdr", phase.ScopeRoute},
		{"rl", phase.ScopeRoute},
		{"up-hdr", phase.ScopeUpstream},
	}
	if !slices.Equal(got, want) {
		t.Errorf("builds = %v, want %v", got, want)
	}
	if got := h.runtimes(); !slices.Equal(got, []string{"audit", "orders"}) {
		t.Errorf("Upstream runtimes = %v, want [audit orders]", got)
	}
	if _, ok := c.Upstreams.Upstream("idle"); ok {
		t.Error("the Upstream set holds idle, which no Route names")
	}
	s := h.meter.lastShape(t)
	if !slices.Equal(s.Upstreams, []string{"audit", "orders"}) || !slices.Equal(s.Routes, []string{"a", "b", "c"}) || !slices.Equal(s.Listeners, []string{"http"}) {
		t.Errorf("shape: Upstreams %v, Routes %v, Listeners %v", s.Upstreams, s.Routes, s.Listeners)
	}
	var policies []string
	for _, p := range s.Policies {
		policies = append(policies, p.Name)
	}
	if !slices.Equal(policies, []string{"audit-hdr", "gw-cors", "hdr", "rl", "up-hdr"}) {
		t.Errorf("shape Policies = %v", policies)
	}
}

// TestMetricShape covers the admission shape: each reached Policy once,
// with its type, every scope and Phase it runs in and whether it is
// attached at Gateway scope, and the Routes whose chain holds a cache
// Policy as CachedRoutes.
func TestMetricShape(t *testing.T) {
	h := newHarness(t)
	h.mustCompile(h.validate(
		gateway(attach("gw-hdr")),
		upstreamDoc("orders", "127.0.0.1:9001", attach("shared")),
		routeDoc("a", "/a", "orders", attach("shared", "cache")),
		routeDoc("b", "/b", "orders", ""),
		policyDoc("gw-hdr", "headers", headersConfig, ""),
		policyDoc("shared", "headers", `{"request":{"set":[{"name":"x-a","value":"1"}]},"response":{"set":[{"name":"x-b","value":"2"}]}}`, ""),
		policyDoc("cache", "cache", cacheConfig, ""),
	), nil)
	s := h.meter.lastShape(t)
	byName := map[string]emit.PolicyShape{}
	for _, p := range s.Policies {
		byName[p.Name] = p
	}
	envs := h.buildsOf("shared")
	if len(envs) != 2 {
		t.Fatalf("shared built %d times, want 2", len(envs))
	}
	shared := byName["shared"]
	if shared.Type != "headers" || shared.Scopes != phase.Scopes(phase.ScopeRoute)|phase.Scopes(phase.ScopeUpstream) ||
		shared.Phases != envs[0].Phases|envs[1].Phases || shared.Gateway {
		t.Errorf("shared shape = %+v, want headers at Route and Upstream scope, the union of its Phases, not Gateway", shared)
	}
	if gw := byName["gw-hdr"]; !gw.Gateway || gw.Scopes != phase.Scopes(phase.ScopeGateway) {
		t.Errorf("gw-hdr shape = %+v, want Gateway scope, striped", gw)
	}
	if !slices.Equal(s.CachedRoutes, []string{"a"}) {
		t.Errorf("CachedRoutes = %v, want [a]", s.CachedRoutes)
	}
}

// TestBuildEnv covers the BuildEnv of each attachment: the typed Policy
// and config, the attachment's scope and Phase set, the Revision and its
// compiled Consumers, the snapshot's one CEL Builder with the sites stage
// J checked, the resolved secrets, both State Store connections with
// their queues, the effective stateStoreTimeout (spec.stateStoreTimeout,
// else the Gateway's stateStore.timeout), the effective limits, the
// Policy's metric handles, the Node's status, clock, logger and replayer,
// one Shared memo per compile, and no Previous on a first compile.
func TestBuildEnv(t *testing.T) {
	h := newHarness(t)
	h.env.set("RURALZ_SECRET_REDIS", "redis://u:p@main.internal:6379")
	h.env.set("RURALZ_SECRET_CACHE", "redis://u:p@cache.internal:6379")
	res := h.validate(
		gateway("  limits:\n    maxRequestBodyBytes: 1Mi\n    maxResponseBodyBytes: 2Mi\n    maxBufferedBytes: 64Mi\n"+
			"  stateStore:\n    driver: redis\n    url: {secretRef: {provider: env, name: RURALZ_SECRET_REDIS}}\n    timeout: 20ms\n"+
			"    cache:\n      url: {secretRef: {provider: env, name: RURALZ_SECRET_CACHE}}\n"),
		upstreamDoc("orders", "127.0.0.1:9001", ""),
		routeDoc("a", "/a", "orders", attach("rl", "cache")),
		policyDoc("rl", "ratelimit", rateLimitConfig, "  stateStoreTimeout: 30ms\n"),
		policyDoc("cache", "cache", cacheConfig, ""),
		consumerDoc,
	)
	c := h.mustCompile(res, nil)
	s := c.Snapshot
	envs := h.builds()
	if len(envs) != 2 {
		t.Fatalf("%d builds, want cache and rl", len(envs))
	}
	timeouts := map[string]time.Duration{"cache": 20 * time.Millisecond, "rl": 30 * time.Millisecond}
	for _, env := range envs {
		name := env.Policy.Metadata.Name
		r, _ := res.Validated.Bundle.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: name})
		pol, _ := hub.Object[v1alpha1.Policy](r)
		if env.Policy != pol || env.Config != r.Config || env.Bundle != res.Validated.Bundle {
			t.Errorf("%s: Policy, Config or Bundle is not the Revision's", name)
		}
		var sp *snapshotPolicy
		for _, p := range s.Routes[0].Chain.Policies {
			if p.Name == name {
				sp = &snapshotPolicy{scope: p.Scope, phases: p.Phases, timeout: p.StateStoreTimeout, metrics: p.Metrics}
			}
		}
		if sp == nil || env.Scope != sp.scope || env.Phases != sp.phases || env.Scope != phase.ScopeRoute {
			t.Errorf("%s: scope %v, Phases %v; want the chain's Route attachment %+v", name, env.Scope, env.Phases, sp)
			continue
		}
		if env.Consumers["acme"] == nil || env.Consumers["acme"] != s.Consumers["acme"] {
			t.Errorf("%s: Consumers are not the snapshot's", name)
		}
		if env.CEL == nil || env.CEL != envs[0].CEL || env.Shared == nil || env.Shared != envs[0].Shared {
			t.Errorf("%s: CEL Builder or Shared memo is not the compile's one", name)
		}
		for _, place := range []expr.PlaceID{expr.PlacePolicyWhen, expr.PlaceRateLimitKey, expr.PlaceCacheKey} {
			want := h.tc.pipe.PolicySite(r, place, nil, env.Scope, env.Phases)
			if got := env.Site(place); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: Site(%s) = %+v, want stage J's %+v", name, place, got, want)
			}
		}
		if env.Secrets != c.Secrets {
			t.Errorf("%s: Secrets are not the snapshot's table", name)
		}
		if env.StateStore != s.StateStore.Store() || env.Enqueuer != s.StateStore.Enqueuer() ||
			env.CacheStore != s.CacheStore.Store() || env.CacheEnqueuer != s.CacheStore.Enqueuer() || env.StateStore == env.CacheStore {
			t.Errorf("%s: State Store connections are not the snapshot's two", name)
		}
		if env.StateStoreTimeout != timeouts[name] || sp.timeout != timeouts[name] {
			t.Errorf("%s: stateStoreTimeout %v, chain %v; want %v", name, env.StateStoreTimeout, sp.timeout, timeouts[name])
		}
		if want := (filter.Limits{MaxRequestBodyBytes: 1 << 20, MaxResponseBodyBytes: 2 << 20, MaxBufferedBytes: 64 << 20}); env.Limits != want {
			t.Errorf("%s: Limits %+v, want %+v", name, env.Limits, want)
		}
		if env.Metrics == nil || env.Metrics != s.Metrics.Policy(name) || sp.metrics != env.Metrics {
			t.Errorf("%s: Metrics are not the plan's handles", name)
		}
		if env.Status != h.deps.Status || env.Clock != h.clk || env.Logger != h.deps.Logger || env.Replayer != h.deps.Replayer {
			t.Errorf("%s: Status, Clock, Logger or Replayer is not the Node's", name)
		}
		if env.Previous != nil {
			t.Errorf("%s: Previous %v on a first compile", name, env.Previous)
		}
	}
	if len(h.cel.caps) != 1 {
		t.Errorf("%d Builders, want one per compile", len(h.cel.caps))
	}
	h.mustCompile(res, nil)
	if envs := h.builds(); envs[len(envs)-1].Shared == envs[0].Shared {
		t.Error("a second compile shares the first one's Shared memo")
	}
}

// snapshotPolicy is what TestBuildEnv compares of a compiled Policy.
type snapshotPolicy struct {
	scope   phase.Scope
	phases  phase.Set
	timeout time.Duration
	metrics *emit.PolicyMetrics
}

// TestFilterRefusals covers Factory refusals: each Build error with a
// registered RZ code is a finding at the Policy's spec.config, its message
// the cause without the code; every refusal is reported, sorted, and a
// Policy refused alike at two attachments is one finding.
func TestFilterRefusals(t *testing.T) {
	h := newHarness(t)
	h.behave(v1alpha1.PolicyTypeHeaders, "hdr", behavior{err: errcode.Errorf("RZ-CFG-005", "the header is refused")})
	h.behave(v1alpha1.PolicyTypeRateLimit, "rl", behavior{err: errcode.Errorf("RZ-CFG-037", "the limit is refused")})
	got := h.rejects(h.validate(
		gateway(""),
		upstreamDoc("orders", "127.0.0.1:9001", attach("hdr")),
		routeDoc("a", "/a", "orders", attach("hdr", "rl", "ok")),
		policyDoc("rl", "ratelimit", rateLimitConfig, ""),
		policyDoc("hdr", "headers", headersConfig, ""),
		policyDoc("ok", "headers", headersConfig, ""),
	))
	want := []string{
		"ruralz.yaml:43:11 error RZ-CFG-037 Policy/rl spec.config: the limit is refused",
		"ruralz.yaml:51:11 error RZ-CFG-005 Policy/hdr spec.config: the header is refused",
	}
	if !slices.Equal(got, want) {
		t.Errorf("findings\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := len(h.buildsOf("hdr")); n != 2 {
		t.Errorf("hdr built %d times, want once per attachment, 2", n)
	}
}

// TestFilterFaults covers what a Build cannot be refused with: an error
// without an RZ code, an unregistered code, another area's code, a Build
// returning neither a Filter nor an error, and a served Policy type
// without a Factory are faults of the release, the compile's error with
// no findings.
func TestFilterFaults(t *testing.T) {
	plain := errors.New("plain failure")
	for _, tc := range []struct {
		name string
		b    behavior
		drop bool
		want string
	}{
		{name: "uncoded", b: behavior{err: plain}, want: "Policy/hdr spec.config: plain failure"},
		{name: "unregistered code", b: behavior{err: errcode.Errorf("RZ-CFG-"+"999", "made up")}, want: "made up"},
		{name: "runtime code", b: behavior{err: errcode.Errorf("RZ-RT-001", "a request code")}, want: "a request code"},
		{name: "no Filter", b: behavior{none: true}, want: "the headers Factory built no Filter for Policy \"hdr\""},
		{name: "no Factory", drop: true, want: "no Factory for the served Policy type \"headers\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.drop {
				h = newHarness(t, func(d *Deps) {
					fs := map[v1alpha1.PolicyType]filter.Factory{}
					for typ, f := range h.facs {
						if typ != v1alpha1.PolicyTypeHeaders {
							fs[typ] = f
						}
					}
					d.Filters = filter.NewRegistry(fs)
				})
			}
			h.behave(v1alpha1.PolicyTypeHeaders, "hdr", tc.b)
			c, ds, err := h.compile(h.validate(
				gateway(""), upstreamDoc("orders", "127.0.0.1:9001", ""), routeDoc("a", "/a", "orders", attach("hdr", "rl")),
				policyDoc("hdr", "headers", headersConfig, ""), policyDoc("rl", "ratelimit", rateLimitConfig, ""),
			), nil)
			if c != nil || len(ds) != 0 || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Compile = %v, %s, %v; want a fault naming %q", c, text(ds), err, tc.want)
			}
			if tc.b.err != nil && !errors.Is(err, tc.b.err) {
				t.Errorf("fault %v does not wrap the Build error", err)
			}
			assertAllClosed(t, h)
		})
	}
}

// TestPolicyWhen covers spec.when: it compiles once per attachment at the
// attachment's site into the compiled Policy's When; a when the snapshot's
// Builder refuses is a finding at spec.when with the first Issue's code
// and message, RZ-CFG-014 for an Issue without a registered code or a
// refusal without an Issue, and its Policy builds no Filter.
func TestPolicyWhen(t *testing.T) {
	h := newHarness(t)
	c := h.mustCompile(h.validate(
		gateway(""), upstreamDoc("orders", "127.0.0.1:9001", ""),
		routeDoc("a", "/a", "orders", attach("hdr")), routeDoc("b", "/b", "orders", attach("hdr")),
		policyDoc("hdr", "headers", headersConfig, "  when: request.method == \"GET\"\n"),
	), nil)
	wa, wb := c.Snapshot.Routes[0].Chain.Policies[0].When, c.Snapshot.Routes[1].Chain.Policies[0].When
	if wa == nil || wa != wb {
		t.Errorf("When = %v and %v, want one program shared by both Routes", wa, wb)
	}

	for _, tc := range []struct{ name, code, want string }{
		{"cost", "RZ-CFG-015", "error RZ-CFG-015 Policy/hdr spec.when: " + refusedMessage},
		{"unregistered", "bogus", "error RZ-CFG-014 Policy/hdr spec.when: " + refusedMessage},
		{"no Issue", "", "error RZ-CFG-014 Policy/hdr spec.when: CEL did not compile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cel.refuse(`request.method == "POST"`, tc.code)
			got := h.rejects(h.validate(
				gateway(""), upstreamDoc("orders", "127.0.0.1:9001", ""), routeDoc("a", "/a", "orders", attach("hdr")),
				policyDoc("hdr", "headers", headersConfig, "  when: request.method == \"POST\"\n"),
			))
			if len(got) != 1 || !strings.HasSuffix(got[0], tc.want) || !strings.HasPrefix(got[0], "ruralz.yaml:40:9 ") {
				t.Errorf("findings = %q, want %q at the when", got, tc.want)
			}
			if n := len(h.buildsOf("hdr")); n != 0 {
				t.Errorf("hdr built %d times after its when was refused", n)
			}
		})
	}
}
