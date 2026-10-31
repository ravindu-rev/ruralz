// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"crypto/tls"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestLimitsOf covers the effective limits: each spec.limits field, else
// its schema default, and maxRequestHeaderBytes capped at the 256 KiB
// process ceiling, which marks the limits capped.
func TestLimitsOf(t *testing.T) {
	defaults := snapshot.Limits{
		MaxRequestBodyBytes: 10 << 20, MaxResponseBodyBytes: 10 << 20, MaxBufferedBytes: 512 << 20,
		MaxCompositionSteps: 16, MaxRequestHeaderBytes: 64 << 10,
	}
	b := func(n int64) *v1alpha1.ByteSize { v := v1alpha1.ByteSize(n); return &v }
	steps := int32(4)
	for _, tc := range []struct {
		name string
		in   *v1alpha1.Limits
		want snapshot.Limits
	}{
		{"absent", nil, defaults},
		{"empty", &v1alpha1.Limits{}, defaults},
		{"set", &v1alpha1.Limits{
			MaxRequestBodyBytes: b(1 << 20), MaxResponseBodyBytes: b(2 << 20), MaxBufferedBytes: b(64 << 20),
			MaxCompositionSteps: &steps, MaxRequestHeaderBytes: b(32 << 10),
		}, snapshot.Limits{
			MaxRequestBodyBytes: 1 << 20, MaxResponseBodyBytes: 2 << 20, MaxBufferedBytes: 64 << 20,
			MaxCompositionSteps: 4, MaxRequestHeaderBytes: 32 << 10,
		}},
		{"at the ceiling", &v1alpha1.Limits{MaxRequestHeaderBytes: b(256 << 10)}, func() snapshot.Limits {
			l := defaults
			l.MaxRequestHeaderBytes = 256 << 10
			return l
		}()},
		{"above the ceiling", &v1alpha1.Limits{MaxRequestHeaderBytes: b(1 << 20)}, func() snapshot.Limits {
			l := defaults
			l.MaxRequestHeaderBytes, l.HeaderLimitCapped = 256<<10, true
			return l
		}()},
	} {
		if got := limitsOf(tc.in); got != tc.want {
			t.Errorf("%s: limitsOf = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestDefaultsMatchSchema covers the compiler's defaults against the
// schema's: a Gateway whose limits, admin and telemetry blocks are empty
// gets every default materialized by stage G, and compiles to what a
// Gateway without them compiles to.
func TestDefaultsMatchSchema(t *testing.T) {
	compile := func(extra string) snapshot.Gateway {
		h := newHarness(t)
		return h.mustCompile(h.validate(gateway(extra), upstreamDoc("up", "127.0.0.1:9000", ""), routeDoc("r", "/", "up", "")), nil).Snapshot.Gateway
	}
	absent := compile("")
	materialized := compile("  limits: {}\n  admin: {}\n  telemetry: {}\n  stateStore: {}\n")
	if !gatewayEqual(absent, materialized) {
		t.Errorf("Gateway without blocks = %+v, with empty blocks = %+v; want equal", absent, materialized)
	}
	want := snapshot.Gateway{
		AdminPort: 9901, TraceSampling: 0.01, StateStoreTimeout: 50 * time.Millisecond,
		Limits: snapshot.Limits{MaxRequestBodyBytes: 10 << 20, MaxResponseBodyBytes: 10 << 20, MaxBufferedBytes: 512 << 20, MaxCompositionSteps: 16, MaxRequestHeaderBytes: 64 << 10},
	}
	if !gatewayEqual(absent, want) {
		t.Errorf("Gateway defaults = %+v, want %+v", absent, want)
	}
}

// gatewayEqual compares the comparable settings of two compiled Gateways.
func gatewayEqual(a, b snapshot.Gateway) bool {
	return a.AdminPort == b.AdminPort && slices.Equal(a.TrustedProxies, b.TrustedProxies) && a.Limits == b.Limits &&
		a.TraceSampling == b.TraceSampling && a.OTLPEndpoint == b.OTLPEndpoint && (a.AccessLogWhen == nil) == (b.AccessLogWhen == nil) &&
		a.StateStoreTimeout == b.StateStoreTimeout
}

// TestGatewaySettings covers the Node-wide settings: the admin port, the
// trusted proxies sorted by address and length, the trace sampling ratio,
// the OTLP endpoint, the compiled accessLog.when and the default
// stateStoreTimeout; a trusted proxy that does not parse, which
// validation refuses first, is a finding at its entry, and an
// accessLog.when the Builder refuses one at that field.
func TestGatewaySettings(t *testing.T) {
	const extra = "  admin:\n    port: 9911\n  trustedProxies: [\"192.168.1.1\", \"10.0.0.0/16\", \"10.0.0.0/8\"]\n" +
		"  telemetry:\n    traceSampling: 0.5\n    otlp:\n      endpoint: http://otel.internal:4317\n" +
		"    accessLog:\n      when: response.status >= 400\n  stateStore:\n    timeout: 30ms\n"
	h := newHarness(t)
	res := h.validate(gateway(extra), upstreamDoc("up", "127.0.0.1:9000", ""), routeDoc("r", "/", "up", ""))
	g := h.mustCompile(res, nil).Snapshot.Gateway
	wantProxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("192.168.1.1/32")}
	if g.AdminPort != 9911 || !slices.Equal(g.TrustedProxies, wantProxies) || g.TraceSampling != 0.5 ||
		g.OTLPEndpoint != "http://otel.internal:4317" || g.AccessLogWhen == nil || g.StateStoreTimeout != 30*time.Millisecond {
		t.Errorf("Gateway = %+v, want admin 9911, proxies %v, sampling 0.5, the endpoint, accessLog.when, 30ms", g, wantProxies)
	}

	h = newHarness(t)
	h.cel.refuse("response.status >= 400", "RZ-CFG-015")
	res = h.validate(gateway(extra), upstreamDoc("up", "127.0.0.1:9000", ""), routeDoc("r", "/", "up", ""))
	gw := gatewayOf(t, res.Validated)
	gw.Spec.TrustedProxies = append(gw.Spec.TrustedProxies, "office")
	got := h.rejects(res)
	want := []string{
		"error RZ-CFG-005 Gateway/edge spec.trustedProxies[item=office]:",
		"error RZ-CFG-015 Gateway/edge spec.telemetry.accessLog.when: " + refusedMessage,
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

// TestInputCaps covers the input caps of the snapshot's CEL Builder: the
// effective maxRequestHeaderBytes, the most bytes the client-leg headers
// and transform.request Policies of any one Route add before an
// expression reads the request, 4 × 16 KiB of claims, and Route and
// Consumer caps covering every Route and Consumer, each view prepared for
// CEL once per snapshot; a first compile builds over no previous program
// set.
func TestInputCaps(t *testing.T) {
	h := newHarness(t)
	s := h.mustCompile(h.validate(
		gateway("  limits:\n    maxRequestHeaderBytes: 32Ki\n"+attach("gw-hdr")),
		upstreamDoc("orders", "127.0.0.1:9001", attach("up-hdr")),
		"apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: long-route-name\n  labels:\n    team: storefront\n    tier: gold\nspec:\n"+
			"  match:\n    path:\n      prefix: /a\n  upstreams:\n    - name: orders\n"+attach("hdr", "tr"),
		routeDoc("b", "/b", "orders", ""),
		policyDoc("gw-hdr", "headers", headersConfig, ""),
		policyDoc("hdr", "headers", `{"request":{"set":[{"name":"x-user","valueExpression":"request.method"}],"add":[{"name":"x-b","value":"12345"}]}}`, ""),
		policyDoc("tr", "transform.request", `{"set":[{"target":"query","name":"page","valueExpression":"\"1\""}]}`, ""),
		policyDoc("up-hdr", "headers", `{"request":{"set":[{"name":"x-up","value":"0123456789"}]}}`, ""),
		doc("Consumer", "acme-corporation", "  tier: gold\n  tags: [a, bb]\n  credentials:\n    apiKeys:\n      - name: k1\n        hash: \"sha256:"+strings.Repeat("ab", 32)+"\"\n"),
	), nil).Snapshot
	caps, prev := h.cel.lastCaps(t)
	// gw-hdr: x-a, "1"; hdr: x-user computed, x-b "12345"; tr: page at the
	// query value bound; up-hdr runs on the upstream leg.
	added := int64(3+1+4) + int64(6+httpfield.MaxComputedValueBytes+4) + int64(3+5+4) + int64(4+httpfield.MaxQueryValueBytes+4)
	if caps.RequestHeaderBytes != 32<<10 || caps.AddedHeaderBytes != added || caps.ClaimsBytes != 4*16<<10 {
		t.Errorf("caps = header %d, added %d, claims %d; want %d, %d, %d", caps.RequestHeaderBytes, caps.AddedHeaderBytes, caps.ClaimsBytes, 32<<10, added, 4*16<<10)
	}
	if r := caps.Routes; !r.Known || r.NameBytes != int64(len("long-route-name")) || r.Labels != 2 || r.LabelBytes != int64(len("storefront")) {
		t.Errorf("Route caps = %+v", r)
	}
	if c := caps.Consumers; !c.Known || c.NameBytes != int64(len("acme-corporation")) || c.TierBytes != 4 || c.Tags != 2 || c.TagBytes != 2 {
		t.Errorf("Consumer caps = %+v", c)
	}
	if prev != nil {
		t.Errorf("first compile built over %v, want no previous programs", prev)
	}
	if len(h.cel.consumers) != 1 || h.cel.consumers[0] != s.Consumers["acme-corporation"] {
		t.Errorf("prepared Consumers = %v, want the snapshot's one", h.cel.consumers)
	}
	if len(h.cel.routes) != 2 {
		t.Errorf("prepared %d Route views, want 2", len(h.cel.routes))
	}

	h = newHarness(t)
	h.mustCompile(h.validate(gateway("  limits:\n    maxRequestHeaderBytes: 1Mi\n"), upstreamDoc("up", "127.0.0.1:9000", ""), routeDoc("r", "/", "up", "")), nil)
	if caps, _ := h.cel.lastCaps(t); caps.RequestHeaderBytes != 256<<10 || caps.AddedHeaderBytes != 0 || !caps.Consumers.Known {
		t.Errorf("caps = header %d, added %d, consumers %+v; want the 256 KiB ceiling, 0, known without Consumers", caps.RequestHeaderBytes, caps.AddedHeaderBytes, caps.Consumers)
	}
}

// TestListeners covers the compiled listeners: in name order, each with
// its protocol, port, PROXY protocol, hostnames and metric handles; an
// https listener with its TLS settings: the minimum version (1.3 unless
// set to 1.2) and its certificates in name order as secret references,
// requesting a client certificate when a Route bound to it holds auth.mtls
// in its effective chain.
func TestListeners(t *testing.T) {
	ca, err := pki.NewCA(pki.CAOptions{NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	for _, n := range []string{"RURALZ_SECRET_A_CRT", "RURALZ_SECRET_B_CRT", "RURALZ_SECRET_C_CRT"} {
		h.env.set(n, string(ca.CertPEM))
	}
	for _, n := range []string{"RURALZ_SECRET_A_KEY", "RURALZ_SECRET_B_KEY", "RURALZ_SECRET_C_KEY"} {
		h.env.set(n, string(ca.KeyPEM))
	}
	h.env.set("RURALZ_SECRET_MTLS_CA", string(ca.CertPEM))
	cert := func(name, n string) string {
		return "          - name: " + name + "\n            certificate: {secretRef: {provider: env, name: RURALZ_SECRET_" + n + "_CRT}}\n" +
			"            privateKey: {secretRef: {provider: env, name: RURALZ_SECRET_" + n + "_KEY}}\n"
	}
	s := h.mustCompile(h.validate(
		doc("Gateway", "edge", "  listeners:\n"+
			"    - name: web\n      protocol: http\n      port: 8080\n      proxyProtocol: true\n      hostnames: [api.shop.example]\n"+
			"    - name: legacy\n      protocol: https\n      port: 8443\n      tls:\n        minVersion: \"1.2\"\n        certificates:\n"+cert("b", "B")+cert("a", "A")+
			"    - name: secure\n      protocol: https\n      port: 8444\n      tls:\n        certificates:\n"+cert("c", "C")),
		upstreamDoc("up", "127.0.0.1:9000", ""),
		doc("Route", "mutual", "  match:\n    path:\n      prefix: /m\n  listeners: [secure]\n  upstreams:\n    - name: up\n"+attach("mtls")),
		doc("Route", "open", "  match:\n    path:\n      prefix: /o\n  listeners: [web, legacy]\n  upstreams:\n    - name: up\n"),
		policyDoc("mtls", "auth.mtls", `{"caCertificate":{"secretRef":{"provider":"env","name":"RURALZ_SECRET_MTLS_CA"}}}`, ""),
	), nil).Snapshot
	var got []string
	for _, l := range s.Listeners {
		got = append(got, l.Name)
		if l.Metrics == nil || l.Metrics != s.Metrics.Listener(l.Name) {
			t.Errorf("listener %s: Metrics are not the plan's handles", l.Name)
		}
	}
	if !slices.Equal(got, []string{"legacy", "secure", "web"}) {
		t.Fatalf("Listeners = %v, want name order", got)
	}
	legacy, secure, web := s.Listeners[0], s.Listeners[1], s.Listeners[2]
	if web.Protocol != v1alpha1.ListenerProtocolHTTP || web.Port != 8080 || !web.ProxyProtocol || !slices.Equal(web.Hostnames, []string{"api.shop.example"}) || web.TLS != nil {
		t.Errorf("web = %+v", *web)
	}
	ref := func(n string) secret.Ref {
		return secret.Ref{Provider: v1alpha1.SecretProviderEnv, Name: "RURALZ_SECRET_" + n}
	}
	wantLegacy := snapshot.ListenerTLS{MinVersion: tls.VersionTLS12, Certificates: []snapshot.Certificate{
		{Name: "a", Certificate: ref("A_CRT"), PrivateKey: ref("A_KEY")}, {Name: "b", Certificate: ref("B_CRT"), PrivateKey: ref("B_KEY")},
	}}
	if legacy.TLS == nil || legacy.TLS.MinVersion != wantLegacy.MinVersion || !slices.Equal(legacy.TLS.Certificates, wantLegacy.Certificates) || legacy.TLS.RequestClientCert {
		t.Errorf("legacy TLS = %+v, want %+v", legacy.TLS, wantLegacy)
	}
	if secure.TLS == nil || secure.TLS.MinVersion != tls.VersionTLS13 || !secure.TLS.RequestClientCert || len(secure.TLS.Certificates) != 1 || secure.Port != 8444 {
		t.Errorf("secure TLS = %+v, want TLS 1.3 requesting a client certificate", secure.TLS)
	}
}

// TestWarnings covers the activation warnings: a Revision whose Routes
// reach cache and a limit Policy (ratelimit or quota) without
// stateStore.cache gets one, naming the Gateway; with stateStore.cache, or
// without either reached, none; and each Upstream the balancer budget
// falls back for gets one naming it.
func TestWarnings(t *testing.T) {
	const quotaConsumer = "apiVersion: ruralz/v1alpha1\nkind: Consumer\nmetadata:\n  name: acme\nspec:\n  credentials:\n    apiKeys:\n" +
		"      - name: k1\n        hash: \"sha256:abababababababababababababababababababababababababababababababab\"\n" +
		"  quotas:\n    - name: daily\n      unit: requests\n      limit: 1000\n      window: 24h\n"
	cacheSpec := "  stateStore:\n    driver: redis\n    url: {secretRef: {provider: env, name: RURALZ_SECRET_REDIS}}\n" +
		"    cache:\n      url: {secretRef: {provider: env, name: RURALZ_SECRET_CACHE}}\n"
	for _, tc := range []struct {
		name, spec string
		attached   []string
		want       bool
	}{
		{name: "cache and ratelimit", attached: []string{"cache", "rl"}, want: true},
		{name: "cache and quota", attached: []string{"cache", "quota"}, want: true},
		{name: "cache connection", spec: cacheSpec, attached: []string{"cache", "rl"}},
		{name: "cache alone", attached: []string{"cache"}},
		{name: "ratelimit alone", attached: []string{"rl"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.env.set("RURALZ_SECRET_REDIS", "redis://u:p@main.internal:6379")
			h.env.set("RURALZ_SECRET_CACHE", "redis://u:p@cache.internal:6379")
			c := h.mustCompile(h.validate(
				gateway(tc.spec), upstreamDoc("up", "127.0.0.1:9000", ""),
				routeDoc("r", "/", "up", attach(tc.attached...)),
				policyDoc("cache", "cache", cacheConfig, ""), policyDoc("rl", "ratelimit", rateLimitConfig, ""),
				policyDoc("quota", "quota", `{"consumerQuota":"daily"}`, ""), quotaConsumer,
			), nil)
			if tc.want {
				want := Warning{
					Resource: &diag.ResourceID{Kind: "Gateway", Name: "edge"},
					Message: "the Revision attaches cache and limit Policies (ratelimit, quota or ai.token-budget) without stateStore.cache: " +
						"Response Cache entries share the State Store connection that holds limit keys",
				}
				if len(c.Warnings) != 1 || *c.Warnings[0].Resource != *want.Resource || c.Warnings[0].Message != want.Message {
					t.Errorf("Warnings = %+v, want %+v", c.Warnings, want)
				}
			} else if len(c.Warnings) != 0 {
				t.Errorf("Warnings = %+v, want none", c.Warnings)
			}
		})
	}

	t.Run("balancer budget", func(t *testing.T) {
		h := newHarness(t, func(d *Deps) {
			d.Upstreams = upstream.NewManager(upstream.Options{Clock: d.Clock, Rand: fixedSource{}, Dial: noDial, BalancerBudget: 1})
		})
		h.ups = h.deps.Upstreams
		c := h.mustCompile(h.validate(gateway(""), upstreamDoc("orders", "127.0.0.1:9001", ""), upstreamDoc("audit", "127.0.0.1:9002", ""),
			routeDoc("a", "/a", "orders", ""), routeDoc("b", "/b", "audit", "")), nil)
		var named []string
		for _, w := range c.Warnings {
			if w.Resource == nil || w.Resource.Kind != "Upstream" || w.Message == "" {
				t.Errorf("warning %+v, want one naming an Upstream", w)
				continue
			}
			named = append(named, w.Resource.Name)
		}
		if len(named) == 0 || len(c.Upstreams.Warnings()) != len(named) {
			t.Errorf("warnings name %v, want one per balancer budget warning %v", named, c.Upstreams.Warnings())
		}
	})
}
