// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// TestRouteWithoutAuth: a Route whose effective chain runs no Policy of
// Filter class auth is a warning at the Route's start; an inherited
// Gateway auth Policy counts, an excluded one does not, and authz is not
// authentication.
func TestRouteWithoutAuth(t *testing.T) {
	plain := []string{gateway(""), orders()}
	tests := []struct {
		name string
		docs []string
		want []string
	}{
		{
			name: "a Route without Policies",
			docs: append(slices.Clone(plain), route("orders", "")),
			want: []string{`ruralz.yaml:25:1 warning route-without-auth Route/orders: Route "orders" runs no authentication Policy`},
		},
		{
			name: "a Route with an auth Policy",
			docs: append(append(slices.Clone(plain), apiKey()...), route("orders", "  policies: [{name: key}]\n")),
		},
		{
			name: "a Route inheriting the Gateway's auth Policy",
			docs: append([]string{gateway("  policies: [{name: key}]\n"), orders()}, append(apiKey(), route("orders", ""))...),
		},
		{
			name: "a Route excluding the Gateway's auth Policy",
			docs: append([]string{gateway("  policies: [{name: key}]\n"), orders()},
				append(apiKey(), route("orders", "  excludePolicies: [{name: key}]\n"))...),
			want: []string{`ruralz.yaml:44:1 warning route-without-auth Route/orders: Route "orders" runs no authentication Policy`},
		},
		{
			name: "a Route with authorization only",
			docs: append(slices.Clone(plain),
				policy("allow", "authz.cel", "  config: {rule: 'request.method == \"GET\"'}\n"),
				route("orders", "  policies: [{name: allow}]\n")),
			want: []string{`ruralz.yaml:33:1 warning route-without-auth Route/orders: Route "orders" runs no authentication Policy`},
		},
		{
			name: "a Route with an auth-class plugin Policy",
			docs: append(slices.Clone(plain),
				plugin("login", "request.headers.read", ""),
				pluginPolicy("login", "login", "  filterClass: auth\n  config: {}\n"),
				route("orders", "  policies: [{name: login}]\n")),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ofRule(audited(t, tc.docs...), RuleRouteWithoutAuth)
			if !slices.Equal(got, tc.want) {
				t.Errorf("route-without-auth findings = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRouteWithoutAuthIsResourceLevel: the finding names the Route and no
// path, and sits at the Route's start.
func TestRouteWithoutAuthIsResourceLevel(t *testing.T) {
	fs := audited(t, gateway(""), orders(), route("orders", ""))
	i := slices.IndexFunc(fs, func(f Finding) bool { return f.Rule == RuleRouteWithoutAuth })
	if i < 0 {
		t.Fatalf("no route-without-auth finding in %q", lines(fs))
	}
	f := fs[i]
	if f.Severity != diag.SeverityWarning || f.Resource == nil || f.Resource.Kind != "Route" || f.Resource.Name != "orders" ||
		len(f.Path) != 0 || f.File != "ruralz.yaml" || f.Line != 25 || f.Column != 1 {
		t.Errorf("finding = %+v, want a warning on Route/orders at ruralz.yaml:25:1 without a path", f)
	}
}

// TestCleartextHop: http listeners, Upstreams without tls, an http OTLP
// endpoint and an http AIProvider baseUrl are cleartext hops; https and
// TLS ones are not, nor is an ai Upstream, whose AIProviders are the hops.
func TestCleartextHop(t *testing.T) {
	httpGateway := `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: plain
      protocol: http
      port: 8080
  telemetry:
    otlp: {endpoint: "http://otel.internal:4317"}
`
	tlsGateway := gateway("  telemetry:\n    otlp: {endpoint: \"https://otel.internal:4317\"}\n")
	cleartextUpstream := `apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: legacy
spec:
  protocol: http
  endpoints:
    - address: legacy.internal:80
`
	ai := []string{
		`apiVersion: ruralz/v1alpha1
kind: AIProvider
metadata:
  name: local
spec:
  dialect: ollama
  baseUrl: http://ollama.internal:11434
`,
		`apiVersion: ruralz/v1alpha1
kind: AIModel
metadata:
  name: chat
spec:
  strategy: fallback
  candidates:
    - name: primary
      provider: local
      model: llama3
`,
		`apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: llm
spec:
  protocol: ai
  ai: {models: [{name: chat}]}
`,
	}
	tests := []struct {
		name string
		docs []string
		want []string
	}{
		{
			name: "an http listener and OTLP endpoint",
			docs: []string{httpGateway, orders()},
			want: []string{
				`ruralz.yaml:8:17 warning cleartext-hop Gateway/edge spec.listeners[name=plain].protocol: listener "plain" serves cleartext HTTP`,
				`ruralz.yaml:11:22 warning cleartext-hop Gateway/edge spec.telemetry.otlp.endpoint: the OTLP endpoint is cleartext http`,
			},
		},
		{
			name: "an https listener and OTLP endpoint",
			docs: []string{tlsGateway, orders()},
		},
		{
			name: "an Upstream without tls",
			docs: []string{tlsGateway, orders(), cleartextUpstream},
			want: []string{`ruralz.yaml:33:13 warning cleartext-hop Upstream/legacy spec.protocol: Upstream "legacy" connects to its Endpoints without TLS`},
		},
		{
			name: "an ai Upstream and an http AIProvider",
			docs: append([]string{tlsGateway}, ai...),
			want: []string{`ruralz.yaml:24:12 warning cleartext-hop AIProvider/local spec.baseUrl: AIProvider "local" calls a cleartext http baseUrl`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ofRule(audited(t, tc.docs...), RuleCleartextHop)
			if !slices.Equal(got, tc.want) {
				t.Errorf("cleartext-hop findings =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// capabilityFindings returns the rule and message of each Capability
// finding of a Plugin granting caps.
func capabilityFindings(t *testing.T, caps string) []string {
	t.Helper()
	var out []string
	for _, f := range audited(t, gateway(""), orders(), plugin("p", caps, "")) {
		if strings.HasPrefix(f.Rule, "plugin-") {
			out = append(out, f.Rule+" "+f.Path.String()+": "+f.Message)
		}
	}
	return out
}

// TestPluginCapabilities: each granted Capability of the Deny-by-default
// table that the audit flags is one warning at its item; the others are
// none.
func TestPluginCapabilities(t *testing.T) {
	tests := []struct {
		name string
		caps string
		want []string
	}{
		{name: "credentials.read is always flagged", caps: "credentials.read", want: []string{
			`plugin-credential-exposure spec.capabilities[item=credentials.read]: Plugin "p" grants credentials.read: credential exposure`,
		}},
		{name: "request.body.read", caps: "request.body.read", want: []string{
			`plugin-credential-exposure spec.capabilities[item=request.body.read]: Plugin "p" grants request.body.read: credential exposure`,
		}},
		{name: "response.body.read and stream.read", caps: "response.body.read, stream.read", want: []string{
			`plugin-payload-exposure spec.capabilities[item=response.body.read]: Plugin "p" grants response.body.read: payload exposure`,
			`plugin-payload-exposure spec.capabilities[item=stream.read]: Plugin "p" grants stream.read: payload exposure`,
		}},
		{name: "response.send", caps: "response.send", want: []string{
			`plugin-can-deny-traffic spec.capabilities[item=response.send]: Plugin "p" grants response.send: the Plugin can deny traffic`,
		}},
		{name: "state.read and state.write", caps: "state.read, state.write", want: []string{
			`plugin-state-round-trip spec.capabilities[item=state.read]: Plugin "p" grants state.read: each call adds a State Store round trip`,
			`plugin-state-round-trip spec.capabilities[item=state.write]: Plugin "p" grants state.write: each call adds a State Store round trip`,
		}},
		{name: "Capabilities the table does not flag", caps: "request.headers.read, request.headers.write, request.body.write, " +
			"consumer.read, response.headers.write, response.body.write, stream.write, clock.read, random.read, " +
			"request.metadata.read, response.headers.read, context.use, crypto.use"},
		{name: "log.write and metrics.write alone", caps: "log.write, metrics.write"},
		{name: "log.write with credentials.read", caps: "log.write, credentials.read", want: []string{
			`plugin-exfiltration-path spec.capabilities[item=log.write]: Plugin "p" grants log.write with credentials.read: an exfiltration path`,
			`plugin-credential-exposure spec.capabilities[item=credentials.read]: Plugin "p" grants credentials.read: credential exposure`,
		}},
		{name: "metrics.write with state.write", caps: "metrics.write, state.write", want: []string{
			`plugin-exfiltration-path spec.capabilities[item=metrics.write]: Plugin "p" grants metrics.write with state.write: an exfiltration path`,
			`plugin-state-round-trip spec.capabilities[item=state.write]: Plugin "p" grants state.write: each call adds a State Store round trip`,
		}},
		{name: "metrics.write with state.read only", caps: "metrics.write, state.read", want: []string{
			`plugin-state-round-trip spec.capabilities[item=state.read]: Plugin "p" grants state.read: each call adds a State Store round trip`,
		}},
		{name: "log.write names the first High-risk read", caps: "log.write, state.write, request.body.read", want: []string{
			`plugin-exfiltration-path spec.capabilities[item=log.write]: Plugin "p" grants log.write with request.body.read: an exfiltration path`,
			`plugin-state-round-trip spec.capabilities[item=state.write]: Plugin "p" grants state.write: each call adds a State Store round trip`,
			`plugin-credential-exposure spec.capabilities[item=request.body.read]: Plugin "p" grants request.body.read: credential exposure`,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := capabilityFindings(t, tc.caps)
			if !slices.Equal(got, tc.want) {
				t.Errorf("Capability findings =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// TestCapabilityFindingsAreWarnings: every Capability finding is a
// warning at its item's source position.
func TestCapabilityFindingsAreWarnings(t *testing.T) {
	fs := audited(t, gateway(""), orders(), plugin("p", "credentials.read", ""))
	i := slices.IndexFunc(fs, func(f Finding) bool { return f.Rule == RulePluginCredentialExposure })
	if i < 0 {
		t.Fatalf("no finding in %q", lines(fs))
	}
	if f := fs[i]; f.Severity != diag.SeverityWarning || f.Line != 34 || f.Column != 18 {
		t.Errorf("finding = %+v, want a warning at ruralz.yaml:34:18", f)
	}
}

// TestEnforcingOpen: a plugin Policy with failureMode open whose Plugin
// sends responses or writes bodies or streams is an error at its
// failureMode; closed, or a Plugin that only reads, is none.
func TestEnforcingOpen(t *testing.T) {
	tests := []struct {
		name string
		caps string
		mode string
		want bool
	}{
		{name: "response.send set to open", caps: "response.send", mode: "open", want: true},
		{name: "request.body.write set to open", caps: "request.body.write", mode: "open", want: true},
		{name: "response.body.write set to open", caps: "response.body.write", mode: "open", want: true},
		{name: "stream.write set to open", caps: "stream.write", mode: "open", want: true},
		{name: "response.send set to closed", caps: "response.send", mode: "closed"},
		{name: "a reading Plugin set to open", caps: "request.headers.read, request.headers.write", mode: "open"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := audited(t, gateway(""), orders(), plugin("p", tc.caps, ""),
				pluginPolicy("enforce", "p", "  failureMode: "+tc.mode+"\n  config: {}\n"))
			got := ofRule(fs, RulePluginEnforcingOpen)
			want := []string(nil)
			if tc.want {
				want = []string{`ruralz.yaml:43:16 error plugin-enforcing-open Policy/enforce spec.failureMode: Policy "enforce" runs ` +
					`enforcing Plugin "p" with failureMode open, which a fault breaker trip bypasses`}
			}
			if !slices.Equal(got, want) {
				t.Errorf("plugin-enforcing-open findings = %q, want %q", got, want)
			}
		})
	}
}

// poolDocs returns a Bundle whose Route runs one plugin Policy of the
// Plugin p granting caps with limits.memoryBytes memory ("" for none),
// under a Gateway whose limits.maxPluginMemoryBytes is nodeCap ("" for the
// default).
func poolDocs(caps, memory, nodeCap string) []string {
	gw := gateway("")
	if nodeCap != "" {
		gw = gateway("  limits: {maxPluginMemoryBytes: " + nodeCap + "}\n")
	}
	limits := ""
	if memory != "" {
		limits = "  limits: {memoryBytes: " + memory + "}\n"
	}
	return []string{
		gw, orders(), plugin("p", caps, limits),
		pluginPolicy("run", "p", "  config: {}\n"),
		route("orders", "  policies: [{name: run}]\n"),
	}
}

// poolFindings returns the rule and message of each pool finding of docs.
func poolFindings(t *testing.T, docs []string) []string {
	t.Helper()
	var out []string
	for _, f := range audited(t, docs...) {
		switch f.Rule {
		case RulePluginPoolSmall, RulePluginParkedBoundLow, RulePluginWarmReservation:
			out = append(out, f.Severity.String()+" "+f.Rule+" "+f.Resource.Kind+"/"+f.Resource.Name+": "+f.Message)
		default:
		}
	}
	return out
}

// TestStatePoolSizing: a state.* pool holds floor(cap/2 / memoryBytes)
// instances; below 4 is an error, a guaranteed parked bound N - ceil(N/4)
// below 16 a warning, and the bounds themselves are clean. A non-state
// pool is never sized here.
func TestStatePoolSizing(t *testing.T) {
	tests := []struct {
		name           string
		caps           string
		memory, capVal string
		want           []string
	}{
		{name: "N = 3 is below 4", caps: "state.read", memory: "268435457", want: []string{
			`error plugin-pool-small Plugin/p: Plugin "p" gets a state.* pool of 3 instances, below 4`,
		}},
		{name: "N = 4 is not below 4, its parked bound 3 is", caps: "state.read", memory: "256Mi", want: []string{
			`warning plugin-parked-bound-low Plugin/p: Plugin "p" gets a guaranteed parked bound of 3, below 16`,
		}},
		{name: "a 64Mi Plugin parks 12", caps: "state.write", memory: "64Mi", want: []string{
			`warning plugin-parked-bound-low Plugin/p: Plugin "p" gets a guaranteed parked bound of 12, below 16`,
		}},
		{name: "N = 21 parks 15", caps: "state.read", memory: "51130563", want: []string{
			`warning plugin-parked-bound-low Plugin/p: Plugin "p" gets a guaranteed parked bound of 15, below 16`,
		}},
		{name: "N = 22 parks 16", caps: "state.read", memory: "48806446"},
		{name: "the 16Mi default parks 48", caps: "state.read"},
		{name: "a larger Node cap", caps: "state.read", memory: "64Mi", capVal: "4Gi"},
		{name: "a non-state pool", caps: "request.headers.read", memory: "512Mi"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := poolFindings(t, poolDocs(tc.caps, tc.memory, tc.capVal))
			if !slices.Equal(got, tc.want) {
				t.Errorf("pool findings = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPoolsNeedAnAttachedPolicy: a Plugin no chain runs has no pool, so
// it is not sized, however small its pool would be.
func TestPoolsNeedAnAttachedPolicy(t *testing.T) {
	docs := []string{
		gateway(""), orders(), plugin("p", "state.read", "  limits: {memoryBytes: 512Mi}\n"),
		pluginPolicy("idle", "p", "  config: {}\n"), route("orders", ""),
	}
	if got := poolFindings(t, docs); len(got) > 0 {
		t.Errorf("pool findings of an unattached Plugin = %q, want none", got)
	}
}

// TestUpstreamLegPoolsCount: a plugin Policy that runs in an upstream leg
// forms a pool too.
func TestUpstreamLegPoolsCount(t *testing.T) {
	leg := strings.Replace(orders(), "  tls: {}\n", "  tls: {}\n  policies: [{name: leg}]\n", 1)
	p := strings.Replace(plugin("p", "state.read", "  limits: {memoryBytes: 512Mi}\n"),
		"phases: [onRequestHeaders]", "phases: [onUpstreamRequest]", 1)
	docs := []string{gateway(""), leg, p, pluginPolicy("leg", "p", "  config: {}\n"), route("orders", "")}
	want := []string{`error plugin-pool-small Plugin/p: Plugin "p" gets a state.* pool of 2 instances, below 4`}
	if got := poolFindings(t, docs); !slices.Equal(got, want) {
		t.Errorf("pool findings of an upstream-leg Plugin = %q, want %q", got, want)
	}
}

// warmDocs returns a Bundle under a 4Gi Node cap whose Route runs one
// plugin Policy per config of configs, each of the Plugin a with memory
// a, and one of the Plugin b with memory b and config {b: true}. The two
// Plugins have one image digest, so a Policy of a shares b's pool when
// their configs and limits are equal too.
func warmDocs(a, b string, configs ...string) []string {
	docs := []string{
		gateway("  limits: {maxPluginMemoryBytes: 4Gi}\n"), orders(),
		plugin("a", "request.headers.read", "  limits: {memoryBytes: "+a+"}\n"),
		plugin("b", "request.headers.read", "  limits: {memoryBytes: "+b+"}\n"),
		pluginPolicy("b-run", "b", "  config: {b: true}\n"),
	}
	refs := "  policies: [{name: b-run}"
	for i, cfg := range configs {
		name := "a-run-" + string(rune('1'+i))
		docs = append(docs, pluginPolicy(name, "a", "  config: "+cfg+"\n"))
		refs += ", {name: " + name + "}"
	}
	return append(docs, route("orders", refs+"]\n"))
}

// TestWarmReservation: one warm instance per pool; a total of exactly
// half of the Node cap is clean, one byte more is a warning at the
// Gateway; Policies with an equal canonical config share one pool, and so
// do Plugins with one image digest, config and limits. Every Plugin stays
// within its pool's share of the cap, 25% (1Gi) for these non-state
// Plugins, as validation refuses a larger one (RZ-CFG-052), so a total
// above half takes three pools; each sharing case would warn if its
// Policies did not share.
func TestWarmReservation(t *testing.T) {
	warning := `warning plugin-warm-reservation Gateway/edge: warm Plugin reservations of 2147483649 bytes exceed half of the 4294967296-byte Node cap`
	tests := []struct {
		name string
		docs []string
		want []string
	}{
		{name: "exactly half of the cap", docs: warmDocs("512Mi", "1Gi", "{a: 1}", "{a: 2}")},
		{name: "one byte above half of the cap", docs: warmDocs("536870913", "1073741823", "{a: 1}", "{a: 2}"), want: []string{warning}},
		{name: "two configs make two pools", docs: warmDocs("768Mi", "536870913", "{a: 1}", "{a: 2}"), want: []string{warning}},
		{name: "equal canonical configs share one pool", docs: warmDocs("1Gi", "1Gi", "{a: 1, b: [1.0]}", "{b: [1], a: 1.0}")},
		{name: "one digest, config and limits share one pool", docs: warmDocs("1Gi", "1Gi", "{b: true}", "{a: 1}")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := poolFindings(t, tc.docs)
			if !slices.Equal(got, tc.want) {
				t.Errorf("warm reservation findings = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPoolFindingLocations: a pool finding sits at limits.memoryBytes, or
// at the nearest authored ancestor when the Plugin leaves its limits to
// the defaults; the warm reservation sits at the Gateway's
// limits.maxPluginMemoryBytes, or its spec without limits.
func TestPoolFindingLocations(t *testing.T) {
	tests := []struct {
		name string
		docs []string
		rule string
		want string
	}{
		{
			name: "an authored memoryBytes",
			docs: poolDocs("state.read", "512Mi", ""), rule: RulePluginPoolSmall,
			want: `ruralz.yaml:35:25 error plugin-pool-small Plugin/p spec.limits.memoryBytes: Plugin "p" gets a state.* pool of 2 instances, below 4`,
		},
		{
			name: "a defaulted memoryBytes under a small cap",
			docs: poolDocs("state.read", "", "64Mi"), rule: RulePluginPoolSmall,
			want: `ruralz.yaml:32:3 error plugin-pool-small Plugin/p spec.limits.memoryBytes: Plugin "p" gets a state.* pool of 2 instances, below 4`,
		},
		{
			name: "an authored maxPluginMemoryBytes",
			docs: warmDocs("536870913", "1073741823", "{a: 1}", "{a: 2}"), rule: RulePluginWarmReservation,
			want: `ruralz.yaml:15:34 warning plugin-warm-reservation Gateway/edge spec.limits.maxPluginMemoryBytes: ` +
				`warm Plugin reservations of 2147483649 bytes exceed half of the 4294967296-byte Node cap`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ofRule(audited(t, tc.docs...), tc.rule)
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("%s findings = %q, want %q", tc.rule, got, tc.want)
			}
		})
	}
}

// TestDefaultCapWarmReservation: without Gateway limits the default 2Gi
// Node cap applies, and the finding sits at the Gateway's spec. Three
// pools at the 512Mi share of a non-state pool pass half of that cap.
func TestDefaultCapWarmReservation(t *testing.T) {
	docs := []string{
		gateway(""), orders(),
		plugin("a", "request.headers.read", "  limits: {memoryBytes: 512Mi}\n"),
		pluginPolicy("run-1", "a", "  config: {a: 1}\n"),
		pluginPolicy("run-2", "a", "  config: {a: 2}\n"),
		pluginPolicy("run-3", "a", "  config: {a: 3}\n"),
		route("orders", "  policies: [{name: run-1}, {name: run-2}, {name: run-3}]\n"),
	}
	want := []string{`ruralz.yaml:6:3 warning plugin-warm-reservation Gateway/edge spec.limits.maxPluginMemoryBytes: ` +
		`warm Plugin reservations of 1610612736 bytes exceed half of the 2147483648-byte Node cap`}
	if got := ofRule(audited(t, docs...), RulePluginWarmReservation); !slices.Equal(got, want) {
		t.Errorf("warm reservation findings = %q, want %q", got, want)
	}
}

// TestCheckIsSorted: Check returns its findings in Sort order, across
// rules and resources.
func TestCheckIsSorted(t *testing.T) {
	fs := audited(t, gateway(""), orders(), plugin("p", "state.read, credentials.read, log.write", ""),
		route("orders", ""), route("users", ""))
	if len(fs) < 4 {
		t.Fatalf("findings = %q, want at least four", lines(fs))
	}
	sorted := slices.Clone(fs)
	Sort(sorted)
	if !slices.EqualFunc(fs, sorted, func(a, b Finding) bool { return string(a.AppendText(nil)) == string(b.AppendText(nil)) }) {
		t.Errorf("Check order =\n%s\nwant\n%s", strings.Join(lines(fs), "\n"), strings.Join(lines(sorted), "\n"))
	}
}

// TestCheckWithoutBundle: a nil Validated, or one without a Bundle, has
// no findings.
func TestCheckWithoutBundle(t *testing.T) {
	if fs := Check(nil, nil, nil); fs != nil {
		t.Errorf("Check(nil) = %v, want nil", fs)
	}
}

// TestEveryFindingHasItsRuleSeverity: the findings of a Bundle that trips
// every rule each carry the severity Rules gives their rule, and every
// rule of Rules is reported.
func TestEveryFindingHasItsRuleSeverity(t *testing.T) {
	docs := []string{
		`apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: plain
      protocol: http
      port: 8080
  limits: {maxPluginMemoryBytes: 1Gi}
`,
		orders(),
		plugin("p", "state.read, credentials.read, log.write, response.body.read, response.send", "  limits: {memoryBytes: 256Mi}\n"),
		plugin("q", "state.write", "  limits: {memoryBytes: 64Mi}\n"),
		plugin("r", "request.headers.read", "  limits: {memoryBytes: 256Mi}\n"),
		pluginPolicy("open", "p", "  failureMode: open\n  config: {}\n"),
		pluginPolicy("q-run", "q", "  config: {}\n"),
		pluginPolicy("r-run", "r", "  config: {r: 1}\n"),
		route("orders", "  policies: [{name: open}, {name: q-run}, {name: r-run}]\n"),
	}
	fs := audited(t, docs...)
	seen := map[string]bool{}
	for _, f := range fs {
		seen[f.Rule] = true
		if want := severityOf(f.Rule); f.Severity != want {
			t.Errorf("%s has severity %s, want %s", f.AppendText(nil), f.Severity, want)
		}
	}
	for _, r := range Rules() {
		if !seen[r.Name] {
			t.Errorf("no %s finding in\n%s", r.Name, strings.Join(lines(fs), "\n"))
		}
	}
}
