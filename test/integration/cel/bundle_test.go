// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package cel

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// doc is one YAML document of kind and name with spec lines indented two
// spaces.
func doc(kind, name, spec string) string {
	return "---\napiVersion: ruralz/v1alpha1\nkind: " + kind + "\nmetadata:\n  name: " + name + "\nspec:\n" + spec
}

// routeDoc is a Route matching prefix, with match.when when set, to
// upstream through policies.
func routeDoc(name, prefix, when, upstream string, policies ...string) string {
	s := "  match:\n    path:\n      prefix: " + prefix + "\n"
	if when != "" {
		s += "    when: " + quoted(when) + "\n"
	}
	s += "  upstreams:\n    - name: " + upstream + "\n"
	if len(policies) > 0 {
		s += "  policies:\n"
		for _, p := range policies {
			s += "    - name: " + p + "\n"
		}
	}
	return doc("Route", name, s)
}

// compositionDoc is a sequential composition Route matching prefix with
// steps (YAML lines indented six spaces, each step starting "- ").
func compositionDoc(name, prefix, steps string) string {
	return doc("Route", name, "  match:\n    path:\n      prefix: "+prefix+"\n"+
		"  composition:\n    mode: sequential\n    steps:\n"+steps)
}

// policyDoc is a Policy of type typ with failureMode mode ("" for the
// type's default), when ("" for none) and config lines indented four
// spaces.
func policyDoc(name, typ, mode, when, config string) string {
	s := "  type: " + typ + "\n"
	if mode != "" {
		s += "  failureMode: " + mode + "\n"
	}
	if when != "" {
		s += "  when: " + quoted(when) + "\n"
	}
	return doc("Policy", name, s+"  config:\n"+config)
}

// upstreamDoc is an http Upstream with one static Endpoint and extra spec
// lines.
func upstreamDoc(name, address, extra string) string {
	return doc("Upstream", name, "  protocol: http\n  endpoints:\n    - address: \""+address+"\"\n"+extra)
}

// responseHeader is a headers config setting the response header name to
// value.
func responseHeader(name, value string) string {
	return "    response:\n      set:\n        - name: " + name + "\n          value: \"" + value + "\"\n"
}

// modes are the failureMode variants of a Policy field.
func modes() []string { return []string{"closed", "open"} }

// bundle is the Revision of every place: the main mock answers the
// Upstreams u, u-hash and u-breaker, the retry mock u-retry.
func bundle(main, retry string) string {
	var b strings.Builder
	b.WriteString("apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\nspec:\n" +
		"  admin:\n    port: 9901\n" +
		"  listeners:\n    - name: web\n      protocol: http\n      port: 8080\n" +
		"  telemetry:\n    traceSampling: 0\n    accessLog:\n      when: " +
		quoted(`!("`+logGate+`" in request.headers) || `+boolExpr(logPrefix)) + "\n")
	b.WriteString(upstreamDoc("u", main, ""))
	b.WriteString(upstreamDoc("u-hash", main, "  loadBalancing:\n    algorithm: ring-hash\n    hashKey: "+quoted(stringExpr())+"\n"))
	b.WriteString(upstreamDoc("u-retry", retry, "  retries:\n    attempts: 1\n    retryOn: "+quoted(boolExpr(celPrefix))+"\n"))
	b.WriteString(upstreamDoc("u-breaker", main, "  circuitBreaker:\n    failureWhen: "+quoted(boolExpr(celPrefix))+"\n"))
	b.WriteString(doc("Consumer", "suite", "  credentials:\n    apiKeys:\n      - name: primary\n        hash: \""+keyHash()+"\"\n"+
		"  quotas:\n    - name: calls\n      unit: requests\n      limit: 1000000\n      window: 1h\n"))

	// Route.spec.match.when, with a fallback Route the guarded one must
	// not fall through to.
	b.WriteString(policyDoc("tag-guarded", "headers", "", "", responseHeader("x-route", "guarded")))
	b.WriteString(policyDoc("tag-fallback", "headers", "", "", responseHeader("x-route", "fallback")))
	b.WriteString(routeDoc("match-guarded", "/match/guarded", boolExpr(celPrefix), "u", "tag-guarded"))
	b.WriteString(routeDoc("match-fallback", "/match", "", "u", "tag-fallback"))

	// Composition steps.
	b.WriteString(compositionDoc("step-path", "/step-path",
		"      - name: only\n        upstream: u\n        pathExpression: "+quoted(pathExpr())+"\n"))
	b.WriteString(compositionDoc("step-path-optional", "/step-path-optional",
		"      - name: first\n        upstream: u\n        optional: true\n        pathExpression: "+quoted(pathExpr())+"\n"+
			"      - name: last\n        upstream: u\n        path: /c/last\n"))
	b.WriteString(compositionDoc("step-when", "/step-when",
		"      - name: first\n        upstream: u\n        path: /c/first\n"+
			"      - name: second\n        upstream: u\n        path: /c/second\n        when: "+quoted(boolExpr(celPrefix))+"\n"))
	b.WriteString(compositionDoc("step-when-optional", "/step-when-optional",
		"      - name: first\n        upstream: u\n        path: /c/first\n"+
			"      - name: second\n        upstream: u\n        path: /c/second\n        optional: true\n        when: "+quoted(boolExpr(celPrefix))+"\n"))

	// authz.cel config.rule.
	b.WriteString(policyDoc("authz-rule", "authz.cel", "", "", "    rule: "+quoted(boolExpr(celPrefix))+"\n"))
	b.WriteString(routeDoc("authz", "/authz", "", "u", "authz-rule"))

	// The cache config.key.
	b.WriteString(policyDoc("cache-key", "cache", "", "", "    key: "+quoted(stringExpr())+"\n"))
	b.WriteString(routeDoc("cache", "/cache", "", "u", "cache-key"))

	b.WriteString(policyDoc("apikey", "auth.api-key", "", "", "    header: x-api-key\n"))
	limit := "    limits:\n      - requests: 100000\n        window: 1m\n"
	for _, m := range modes() {
		// Policy.spec.when.
		b.WriteString(policyDoc("when-"+m, "headers", m, boolExpr(celPrefix), responseHeader("x-ran", "1")))
		b.WriteString(routeDoc("policy-when-"+m, "/policy-when/"+m, "", "u", "when-"+m))
		// ratelimit config.key, config.cost and config.limits[].when.
		b.WriteString(policyDoc("rl-key-"+m, "ratelimit", m, "", "    key: "+quoted(stringExpr())+"\n"+limit))
		b.WriteString(routeDoc("rl-key-"+m, "/rl-key/"+m, "", "u", "rl-key-"+m))
		b.WriteString(policyDoc("rl-cost-"+m, "ratelimit", m, "", "    cost: "+quoted(intExpr())+"\n"+limit))
		b.WriteString(routeDoc("rl-cost-"+m, "/rl-cost/"+m, "", "u", "rl-cost-"+m))
		b.WriteString(policyDoc("rl-when-"+m, "ratelimit", m, "",
			"    limits:\n      - requests: 100000\n        window: 1m\n        when: "+quoted(boolExpr(celPrefix))+"\n"))
		b.WriteString(routeDoc("rl-when-"+m, "/rl-when/"+m, "", "u", "rl-when-"+m))
		// quota config.key and config.cost, behind auth.api-key.
		b.WriteString(policyDoc("quota-key-"+m, "quota", m, "", "    consumerQuota: calls\n    key: "+quoted(stringExpr())+"\n"))
		b.WriteString(routeDoc("quota-key-"+m, "/quota-key/"+m, "", "u", "apikey", "quota-key-"+m))
		b.WriteString(policyDoc("quota-cost-"+m, "quota", m, "", "    consumerQuota: calls\n    cost: "+quoted(intExpr())+"\n"))
		b.WriteString(routeDoc("quota-cost-"+m, "/quota-cost/"+m, "", "u", "apikey", "quota-cost-"+m))
		// headers valueExpression, request and response.
		b.WriteString(policyDoc("hdr-req-"+m, "headers", m, "",
			"    request:\n      set:\n        - name: x-computed\n          valueExpression: "+quoted(stringExpr())+"\n"))
		b.WriteString(routeDoc("hdr-req-"+m, "/hdr-req/"+m, "", "u", "hdr-req-"+m))
		b.WriteString(policyDoc("hdr-resp-"+m, "headers", m, "",
			"    response:\n      set:\n        - name: x-computed\n          valueExpression: "+quoted(stringExpr())+"\n"))
		b.WriteString(routeDoc("hdr-resp-"+m, "/hdr-resp/"+m, "", "u", "hdr-resp-"+m))
		// transform.request and transform.response config.body and
		// config.set[].valueExpression.
		b.WriteString(policyDoc("trq-body-"+m, "transform.request", m, "", "    body: "+quoted(dynExpr())+"\n"))
		b.WriteString(routeDoc("trq-body-"+m, "/trq-body/"+m, "", "u", "trq-body-"+m))
		b.WriteString(policyDoc("trq-set-"+m, "transform.request", m, "",
			"    set:\n      - target: header\n        name: x-t\n        valueExpression: "+quoted(stringExpr())+"\n"))
		b.WriteString(routeDoc("trq-set-"+m, "/trq-set/"+m, "", "u", "trq-set-"+m))
		b.WriteString(policyDoc("trs-body-"+m, "transform.response", m, "", "    body: "+quoted(dynExpr())+"\n"))
		b.WriteString(routeDoc("trs-body-"+m, "/trs-body/"+m, "", "u", "trs-body-"+m))
		b.WriteString(policyDoc("trs-set-"+m, "transform.response", m, "",
			"    set:\n      - target: header\n        name: x-t\n        valueExpression: "+quoted(stringExpr())+"\n"))
		b.WriteString(routeDoc("trs-set-"+m, "/trs-set/"+m, "", "u", "trs-set-"+m))
	}

	// The Upstream fields and accessLog.when.
	b.WriteString(routeDoc("hash", "/hash", "", "u-hash"))
	b.WriteString(routeDoc("retry", "/retry", "", "u-retry"))
	b.WriteString(routeDoc("breaker", "/breaker", "", "u-breaker"))
	b.WriteString(routeDoc("log", "/log", "", "u"))

	// Routes whose deadline comes before the 50 ms stop: their match.when
	// and authz.cel rule stop at it.
	timeout := "  timeout: " + routeDeadline.String() + "\n"
	b.WriteString(routeDoc("deadline-match", "/deadline/match", boolExpr(celPrefix), "u") + timeout)
	b.WriteString(policyDoc("authz-deadline", "authz.cel", "", "", "    rule: "+quoted(boolExpr(celPrefix))+"\n"))
	b.WriteString(routeDoc("deadline-authz", "/deadline/authz", "", "u", "authz-deadline") + timeout)
	return b.String()
}

// failures reads ruralz_filter_failures_total of policy.
func failures(policy string) func(promtext.Set) float64 {
	return func(set promtext.Set) float64 {
		return set.Sum(catalog.FilterFailuresTotal, promtext.L("policy", policy))
	}
}

// upstreamCEL reads ruralz_upstream_cel_errors_total of an Upstream field.
func upstreamCEL(upstream, field string) func(promtext.Set) float64 {
	return func(set promtext.Set) float64 {
		return set.Sum(catalog.UpstreamCELErrorsTotal, promtext.L("upstream", upstream, "field", field))
	}
}

// executorRecord is the debug record of a Filter that could not decide,
// naming the error.
const executorRecord = "filter could not decide"

// is returns a check of an answer's status, code and header values ("" for
// an absent header), with a body that holds body when set.
func is(status int, code string, headers map[string]string, body string) func(t *testing.T, a answer) {
	return func(t *testing.T, a answer) {
		t.Helper()
		if a.status != status || a.code != code {
			t.Errorf("answer %v, want %d %s", a, status, code)
		}
		for name, want := range headers {
			if got := a.header.Get(name); got != want {
				t.Errorf("answer %v: %s = %q, want %q", a, name, got, want)
			}
		}
		if body != "" && !strings.Contains(string(a.body), body) {
			t.Errorf("answer %v, want a body holding %s", a, body)
		}
	}
}

// ok is a 200 answer without a problem document.
func ok(headers map[string]string, body string) func(t *testing.T, a answer) {
	return is(http.StatusOK, "", headers, body)
}
