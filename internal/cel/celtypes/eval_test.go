// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Tests for 03 C requirements 17 to 24 through cel-go: every expr.Vars
// field is reachable from CEL with its documented type and value, nullable
// variables are null when absent, header, query and parameter maps behave as
// specified.

// evalCase is one expression over an activation.
type evalCase struct {
	name     string
	vars     func() *expr.Vars
	src      string
	want     any    // Go value compared with CEL equality; nil with an error
	wantErr  error  // sentinel the evaluation error wraps
	wantText string // text the evaluation error contains (cel-go's own errors)
}

// runEval evaluates every case in one environment.
func runEval(t *testing.T, cases []evalCase) {
	t.Helper()
	env := testEnv(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vars := fullVars()
			if tc.vars != nil {
				vars = tc.vars()
			}
			out, _, err := compile(t, env, tc.src).Eval(NewActivation(vars))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("%s: err = %v (out %v), want %v", tc.src, err, out, tc.wantErr)
				}
				return
			}
			if tc.wantText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantText) {
					t.Fatalf("%s: err = %v (out %v), want %q", tc.src, err, out, tc.wantText)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", tc.src, err)
			}
			assertVal(t, tc.src, out, tc.want)
		})
	}
}

// assertVal compares a CEL value with a Go value under CEL equality.
func assertVal(t *testing.T, src string, got ref.Val, want any) {
	t.Helper()
	w := types.DefaultTypeAdapter.NativeToValue(want)
	if types.Equal(got, w) != types.True {
		t.Errorf("%s = %v (%s), want %v", src, got, got.Type().TypeName(), want)
	}
}

// TestReq17OutputTypes checks the checker's type of every field selection.
func TestReq17OutputTypes(t *testing.T) {
	env := testEnv(t)
	cases := map[string]string{
		`request`:                  TypeRequest,
		`request.method`:           "string",
		`request.scheme`:           "string",
		`request.host`:             "string",
		`request.path`:             "string",
		`request.pathParams`:       "map(string, string)",
		`request.query`:            "map(string, string)",
		`request.headers`:          "map(string, string)",
		`request.body`:             "dyn",
		`source.ip`:                "string",
		`source.port`:              "int",
		`source.tlsVersion`:        "string",
		`source.clientCertSubject`: "string",
		`route.name`:               "string",
		`route.labels`:             "map(string, string)",
		`consumer.name`:            "string",
		`consumer.tier`:            "string",
		`consumer.tags`:            "list(string)",
		`consumer.labels`:          "map(string, string)",
		`consumer.quotas`:          "list(string)",
		`auth.method`:              "string",
		`auth.claims`:              "dyn",
		`response.status`:          "int",
		`response.headers`:         "map(string, string)",
		`response.body`:            "dyn",
		`error.kind`:               "string",
		`upstream.name`:            "string",
		`upstream.endpoint`:        "string",
		`attempt`:                  "int",
		`steps`:                    "map(string, ruralz.Step)",
		`steps.order`:              TypeStep,
		`steps.order.status`:       "int",
		`steps.order.headers`:      "map(string, string)",
		`steps.order.body`:         "dyn",
		`duration`:                 "google.protobuf.Duration",
		`now`:                      "google.protobuf.Timestamp",
		`ai.model`:                 "string",
		`ai.estimatedInputTokens`:  "int",
		`ai.maxOutputTokens`:       "int",
		`ai.stream`:                "bool",
		`consumer == null`:         "bool",
		`response.status >= 400 || duration > duration("1s")`: "bool",
	}
	for src, want := range cases {
		ast, iss := env.Compile(src)
		if iss.Err() != nil {
			t.Errorf("%s: %v", src, iss.Err())
			continue
		}
		if got := ast.OutputType().String(); got != want {
			t.Errorf("%s: type %s, want %s", src, got, want)
		}
	}
	// Undeclared fields are check errors, not runtime errors.
	for _, src := range []string{`request.bogus`, `source.country`, `steps.order.bogus`, `ai.bogus`} {
		if _, iss := env.Compile(src); iss.Err() == nil || !strings.Contains(iss.Err().Error(), "undefined field") {
			t.Errorf("%s: issues %v, want undefined field", src, iss.Err())
		}
	}
}

// TestReq17EveryField evaluates every field of every view with its value.
func TestReq17EveryField(t *testing.T) {
	runEval(t, []evalCase{
		{name: "request.method", src: `request.method`, want: "GET"},
		{name: "request.scheme", src: `request.scheme`, want: "https"},
		{name: "request.host", src: `request.host`, want: "api.example.com"},
		{name: "request.path", src: `request.path`, want: "/orders/42"},
		{name: "request.pathParams", src: `request.pathParams`, want: map[string]string{"orderId": "42", "customer": "acme"}},
		{name: "request.query", src: `request.query`, want: map[string]string{"tag": "a,b", "q": "hello world"}},
		{name: "request.headers", src: `request.headers`, want: map[string]string{"x-tenant": "acme", "accept": "application/json, text/plain", "user-agent": "curl/8.0"}},
		{name: "request.body", src: `request.body.items`, want: []string{"x", "y"}},
		{name: "source.ip", src: `source.ip`, want: "192.0.2.1"},
		{name: "source.port", src: `source.port`, want: 44321},
		{name: "source.tlsVersion", src: `source.tlsVersion`, want: "1.3"},
		{name: "source.clientCertSubject", src: `source.clientCertSubject`, want: "CN=client,O=Acme"},
		{name: "route.name", src: `route.name`, want: "orders"},
		{name: "route.labels", src: `route.labels`, want: map[string]string{"team": "core", "env": "prod"}},
		{name: "consumer.name", src: `consumer.name`, want: "acme"},
		{name: "consumer.tier", src: `consumer.tier`, want: "gold"},
		{name: "consumer.tags", src: `consumer.tags`, want: []string{"beta", "eu"}},
		{name: "consumer.labels", src: `consumer.labels`, want: map[string]string{"org": "acme"}},
		{name: "consumer.quotas", src: `consumer.quotas`, want: []string{"daily-tokens"}},
		{name: "auth.method", src: `auth.method`, want: "jwt"},
		{name: "auth.claims", src: `auth.claims.sub`, want: "u1"},
		{name: "response.status", src: `response.status`, want: 503},
		{name: "response.headers", src: `response.headers`, want: map[string]string{"retry-after": "5"}},
		{name: "response.body", src: `response.body.total`, want: 12.5},
		{name: "error.kind", src: `error.kind`, want: "reset"},
		{name: "upstream.name", src: `upstream.name`, want: "orders-v1"},
		{name: "upstream.endpoint", src: `upstream.endpoint`, want: "10.0.0.7:8080"},
		{name: "attempt", src: `attempt`, want: 2},
		{name: "steps.status", src: `steps.order.status`, want: 200},
		{name: "steps.headers", src: `steps.order.headers["etag"]`, want: `"v1"`},
		{name: "steps.body", src: `steps.order.body.id`, want: 7},
		{name: "duration", src: `duration == duration("1.5s")`, want: true},
		{name: "now", src: `now == timestamp("2026-09-26T10:30:00Z")`, want: true},
		{name: "ai.model", src: `ai.model`, want: "small"},
		{name: "ai.estimatedInputTokens", src: `ai.estimatedInputTokens`, want: 1200},
		{name: "ai.maxOutputTokens", src: `ai.maxOutputTokens`, want: 256},
		{name: "ai.stream", src: `ai.stream`, want: true},
		{name: "type(request)", src: `type(request) == ruralz.Request`, want: true},
		{name: "type(steps.order)", src: `type(steps.order) == ruralz.Step`, want: true},
		{name: "type(response) differs", src: `type(response) == ruralz.Request`, want: false},
		{name: "duration variable and function coexist", src: `response.status >= 400 || duration > duration("1s")`, want: true},
		{name: "example authz-orders", src: `auth.claims.scope.split(" ").exists(s, s == "orders:read")`, want: true},
		{name: "example path expression", src: `"/orders/" + request.pathParams.orderId`, want: "/orders/42"},
		{name: "example quotas", src: `consumer != null && "daily-tokens" in consumer.quotas`, want: true},
		{name: "example retry", src: `error != null ? error.kind in ["connect", "reset"] : response.status == 503`, want: true},
		{name: "example rate key", src: `consumer == null ? source.ip : consumer.name`, want: "acme"},
		{name: "example api key", src: `"x-api-key" in request.headers`, want: false},
		{name: "example body string", src: `string(response.body.total)`, want: "12.5"},
	})
}

// TestReq18Nullability checks that each nullable variable is null when
// absent, that null comparisons type-check and evaluate, that selecting a
// field of null is a runtime error wrapping ErrNull and has() on it is
// false.
func TestReq18Nullability(t *testing.T) {
	absent := func() *expr.Vars { return &expr.Vars{Request: &expr.Request{Method: "GET"}} }
	var cases []evalCase
	for _, v := range []struct{ name, field string }{
		{"consumer", "name"},
		{"auth", "method"},
		{"response", "status"},
		{"error", "kind"},
		{"upstream", "name"},
		{"ai", "model"},
		{"route", "name"},
		{"source", "ip"},
	} {
		cases = append(cases,
			evalCase{name: v.name + " == null", vars: absent, src: v.name + ` == null`, want: true},
			evalCase{name: "null == " + v.name, vars: absent, src: `null == ` + v.name, want: true},
			evalCase{name: v.name + " != null", vars: absent, src: v.name + ` != null`, want: false},
			evalCase{name: v.name + " set != null", src: v.name + ` != null`, want: true},
			evalCase{name: v.name + " select", vars: absent, src: v.name + "." + v.field, wantErr: ErrNull},
			evalCase{name: v.name + " has", vars: absent, src: "has(" + v.name + "." + v.field + ")", want: false},
		)
	}
	cases = append(cases,
		evalCase{name: "anonymous rate key", vars: func() *expr.Vars {
			v := fullVars()
			v.Consumer = nil
			return v
		}, src: `consumer == null ? source.ip : consumer.name`, want: "192.0.2.1"},
		evalCase{name: "retry on connect error", vars: func() *expr.Vars {
			v := fullVars()
			v.Response, v.Error = nil, &expr.AttemptError{Kind: "connect"}
			return v
		}, src: expr.DefaultRetryOn, want: true},
		evalCase{name: "failureWhen with a response", vars: func() *expr.Vars {
			v := fullVars()
			v.Error = nil
			return v
		}, src: expr.DefaultFailureWhen, want: true},
		evalCase{name: "request always a view", vars: absent, src: `request != null`, want: true},
	)
	runEval(t, cases)
}

// TestReq19Headers checks the header maps: lowercased keys, ", " joins,
// case-insensitive lookup, size, in, ascending iteration; Host is hidden in
// request.headers only.
func TestReq19Headers(t *testing.T) {
	withHeaders := func(h http.Header) func() *expr.Vars {
		return func() *expr.Vars {
			v := fullVars()
			v.Request.Header = h
			v.Response.Header = h
			st := &expr.Steps{}
			st.Add("s", expr.Step{Status: 200, Header: h})
			v.Steps = st
			return v
		}
	}
	h := http.Header{
		"A":            {"1", "2"},
		"X-Tenant":     {"acme"},
		"Content-Type": {"application/json"},
	}
	withHost := http.Header{"A": {"1"}, "Host": {"origin.example"}}
	var cases []evalCase
	for _, m := range []string{"request.headers", "response.headers", "steps.s.headers"} {
		cases = append(cases,
			evalCase{name: m + " join", vars: withHeaders(h), src: m + `["a"]`, want: "1, 2"},
			evalCase{name: m + " case-insensitive", vars: withHeaders(h), src: m + `["X-TENANT"]`, want: "acme"},
			evalCase{name: m + " select", vars: withHeaders(h), src: m + `.a`, want: "1, 2"},
			evalCase{name: m + " size", vars: withHeaders(h), src: m + `.size()`, want: 3},
			evalCase{name: m + " in", vars: withHeaders(h), src: `"content-type" in ` + m, want: true},
			evalCase{name: m + " in any case", vars: withHeaders(h), src: `"Content-Type" in ` + m, want: true},
			evalCase{name: m + " absent", vars: withHeaders(h), src: `"x-api-key" in ` + m, want: false},
			evalCase{name: m + " has", vars: withHeaders(h), src: `has(` + m + `.a)`, want: true},
			evalCase{name: m + " sorted keys", vars: withHeaders(h), src: m + `.map(k, k).join(",")`, want: "a,content-type,x-tenant"},
			evalCase{name: m + " sorted filter", vars: withHeaders(h), src: m + `.filter(k, k != "a")`, want: []string{"content-type", "x-tenant"}},
			evalCase{name: m + " missing key", vars: withHeaders(h), src: m + `["nope"]`, wantText: "no such key"},
			evalCase{name: m + " nil header", vars: withHeaders(nil), src: m + `.size()`, want: 0},
			evalCase{name: m + " equality", vars: withHeaders(http.Header{"A": {"1"}}), src: m + ` == {"a": "1"}`, want: true},
		)
	}
	// Host is request.host, so request.headers never shows it; a response
	// or step map shows every field received, Host included.
	cases = append(cases,
		evalCase{name: "request.headers no host", vars: withHeaders(withHost), src: `"host" in request.headers`, want: false},
		evalCase{name: "request.headers no Host any case", vars: withHeaders(withHost), src: `"HOST" in request.headers`, want: false},
		evalCase{name: "request.headers size without host", vars: withHeaders(withHost), src: `request.headers.size()`, want: 1},
		evalCase{name: "request.headers keys without host", vars: withHeaders(withHost), src: `request.headers.map(k, k).join(",")`, want: "a"},
		evalCase{name: "request.headers select host", vars: withHeaders(withHost), src: `request.headers.host`, wantText: "no such key"},
		evalCase{name: "request.headers has host", vars: withHeaders(http.Header{"Host": {"x"}}), src: `has(request.headers)`, want: false},
	)
	for _, m := range []string{"response.headers", "steps.s.headers"} {
		cases = append(cases,
			evalCase{name: m + " host", vars: withHeaders(withHost), src: `"host" in ` + m, want: true},
			evalCase{name: m + " Host any case", vars: withHeaders(withHost), src: m + `["HOST"]`, want: "origin.example"},
			evalCase{name: m + " size with host", vars: withHeaders(withHost), src: m + `.size()`, want: 2},
			evalCase{name: m + " keys with host", vars: withHeaders(withHost), src: m + `.map(k, k).join(",")`, want: "a,host"},
			evalCase{name: m + " has with only host", vars: withHeaders(http.Header{"Host": {"x"}}), src: `has(` + strings.TrimSuffix(m, ".headers") + `.headers)`, want: true},
		)
	}
	cases = append(cases, evalCase{name: "example tenant", src: `request.headers["x-tenant"] == "acme"`, want: true})
	runEval(t, cases)
}

// TestReq20Request checks request fields, pathParams and query parsing.
func TestReq20Request(t *testing.T) {
	withQuery := func(q string, params ...expr.Param) func() *expr.Vars {
		return func() *expr.Vars {
			v := fullVars()
			v.Request.SetRawQuery(q)
			v.Request.PathParams = params
			return v
		}
	}
	runEval(t, []evalCase{
		{name: "query repeated joined", vars: withQuery("a=1&a=2&b="), src: `request.query.a`, want: "1,2"},
		{name: "query empty value", vars: withQuery("a=1&a=2&b="), src: `request.query.b`, want: ""},
		{name: "query percent-decoded", vars: withQuery("q=a%2Fb%20c+d"), src: `request.query.q`, want: "a/b c d"},
		{name: "query key decoded", vars: withQuery("x%2Dy=1"), src: `request.query["x-y"]`, want: "1"},
		{name: "query absent", vars: withQuery(""), src: `request.query.size()`, want: 0},
		{name: "query routing", vars: withQuery("version=2"), src: `"version" in request.query && request.query.version == "2"`, want: true},
		{name: "query sorted", vars: withQuery("z=1&a=2&m=3"), src: `request.query.map(k, k)`, want: []string{"a", "m", "z"}},
		{name: "pathParams empty", vars: withQuery(""), src: `request.pathParams.size()`, want: 0},
		{
			name: "pathParams sorted", vars: withQuery("", expr.Param{Name: "b", Value: "2"}, expr.Param{Name: "a", Value: "1"}),
			src: `request.pathParams.map(k, k + "=" + request.pathParams[k]).join("&")`, want: "a=1&b=2",
		},
		{
			name: "pathParams first wins", vars: withQuery("", expr.Param{Name: "a", Value: "1"}, expr.Param{Name: "a", Value: "2"}),
			src: `request.pathParams.a + string(request.pathParams.size())`, want: "11",
		},
		{name: "pathParams in", vars: withQuery("", expr.Param{Name: "id", Value: "9"}), src: `"id" in request.pathParams`, want: true},
		{name: "has query", vars: withQuery("a=1"), src: `has(request.query.a) && !has(request.query.b)`, want: true},
	})
}

// TestReq22Source checks source.ip forms, port, tlsVersion and the subject.
func TestReq22Source(t *testing.T) {
	withSource := func(s expr.Source) func() *expr.Vars {
		return func() *expr.Vars {
			v := fullVars()
			v.Source = &s
			return v
		}
	}
	runEval(t, []evalCase{
		{name: "ipv4", vars: withSource(expr.Source{IP: netip.MustParseAddr("198.51.100.7")}), src: `source.ip`, want: "198.51.100.7"},
		{name: "ipv4-mapped unmapped", vars: withSource(expr.Source{IP: netip.MustParseAddr("::ffff:198.51.100.7")}), src: `source.ip`, want: "198.51.100.7"},
		{name: "ipv6 RFC 5952", vars: withSource(expr.Source{IP: netip.MustParseAddr("2001:0DB8:0:0:0:0:0:1")}), src: `source.ip`, want: "2001:db8::1"},
		{name: "ipv6 zone dropped", vars: withSource(expr.Source{IP: netip.MustParseAddr("fe80::1%eth0")}), src: `source.ip`, want: "fe80::1"},
		{name: "no address", vars: withSource(expr.Source{}), src: `source.ip`, want: ""},
		{name: "port from header", vars: withSource(expr.Source{IP: netip.MustParseAddr("192.0.2.1")}), src: `source.port`, want: 0},
		{name: "tls 1.2", vars: withSource(expr.Source{TLSVersion: 0x0303}), src: `source.tlsVersion`, want: "1.2"},
		{name: "tls 1.3", vars: withSource(expr.Source{TLSVersion: 0x0304}), src: `source.tlsVersion`, want: "1.3"},
		{name: "cleartext", vars: withSource(expr.Source{}), src: `source.tlsVersion`, want: ""},
		{name: "older tls", vars: withSource(expr.Source{TLSVersion: 0x0301}), src: `source.tlsVersion`, want: ""},
		{name: "no client cert", vars: withSource(expr.Source{}), src: `source.clientCertSubject`, want: ""},
		{name: "has ip", vars: withSource(expr.Source{}), src: `has(source.ip) || has(source.port) || has(source.tlsVersion) || has(source.clientCertSubject)`, want: false},
		{name: "has all", src: `has(source.ip) && has(source.port) && has(source.tlsVersion) && has(source.clientCertSubject)`, want: true},
	})
}

// TestReq23RouteConsumerAuth checks route, consumer and auth, prepared and
// not.
func TestReq23RouteConsumerAuth(t *testing.T) {
	prepared := func() *expr.Vars {
		v := fullVars()
		PrepareRoute(v.Route)
		PrepareConsumer(v.Consumer)
		return v
	}
	apiKey := func() *expr.Vars {
		v := fullVars()
		v.Auth = &expr.Auth{Method: "api-key"}
		return v
	}
	var cases []evalCase
	for _, pc := range []struct {
		name string
		vars func() *expr.Vars
	}{{"lazy", fullVars}, {"prepared", prepared}} {
		cases = append(cases,
			evalCase{name: pc.name + " route labels sorted", vars: pc.vars, src: `route.labels.map(k, k)`, want: []string{"env", "team"}},
			evalCase{name: pc.name + " route label", vars: pc.vars, src: `route.labels.team`, want: "core"},
			evalCase{name: pc.name + " consumer labels", vars: pc.vars, src: `consumer.labels.org`, want: "acme"},
			evalCase{name: pc.name + " consumer labels in", vars: pc.vars, src: `"org" in consumer.labels && !("x" in consumer.labels)`, want: true},
			evalCase{name: pc.name + " gold", vars: pc.vars, src: `consumer != null && consumer.tier == "gold"`, want: true},
			evalCase{name: pc.name + " tags", vars: pc.vars, src: `"eu" in consumer.tags && consumer.tags.size() == 2 && consumer.tags[0] == "beta"`, want: true},
		)
	}
	cases = append(cases,
		evalCase{name: "tier unset", vars: func() *expr.Vars {
			v := fullVars()
			v.Consumer.Tier = ""
			return v
		}, src: `consumer.tier == "" && !has(consumer.tier)`, want: true},
		evalCase{name: "api-key claims empty", vars: apiKey, src: `auth.claims.size() == 0 && !has(auth.claims.sub) && !has(auth.claims)`, want: true},
		evalCase{name: "api-key method", vars: apiKey, src: `auth.method`, want: "api-key"},
		evalCase{name: "jwt claims dyn", src: `auth.claims.scope.startsWith("orders")`, want: true},
		evalCase{name: "tags concat", src: `consumer.tags + ["x"]`, want: []string{"beta", "eu", "x"}},
		evalCase{name: "empty tags concat", vars: func() *expr.Vars {
			v := fullVars()
			v.Consumer.Tags = nil
			return v
		}, src: `consumer.tags + ["x"] == ["x"] && consumer.tags.size() == 0`, want: true},
		evalCase{name: "quotas join", src: `consumer.quotas.join(",")`, want: "daily-tokens"},
		evalCase{name: "tags index out of range", src: `consumer.tags[5]`, wantText: "out of bounds"},
	)
	runEval(t, cases)
}

// TestReq24LegVariables checks response, error, upstream, attempt, steps,
// duration and now.
func TestReq24LegVariables(t *testing.T) {
	noSteps := func() *expr.Vars {
		v := fullVars()
		v.Steps = nil
		return v
	}
	nonUTC := func() *expr.Vars {
		v := fullVars()
		v.Now = time.Date(2026, 9, 26, 12, 30, 0, 0, time.FixedZone("CEST", 2*3600))
		return v
	}
	runEval(t, []evalCase{
		{name: "steps presence", src: `has(steps.order) && "order" in steps`, want: true},
		{name: "steps absence", src: `!has(steps.later) && !("later" in steps)`, want: true},
		{name: "steps missing select", src: `steps.later.status`, wantText: "no such key"},
		{name: "steps size", src: `steps.size()`, want: 1},
		{name: "steps keys", src: `steps.map(k, k)`, want: []string{"order"}},
		{name: "steps index", src: `steps["order"].status == 200`, want: true},
		{name: "nil steps empty", vars: noSteps, src: `steps.size() == 0 && !("order" in steps)`, want: true},
		{name: "step without body", vars: func() *expr.Vars {
			v := fullVars()
			st := &expr.Steps{}
			st.Add("plain", expr.Step{Status: 204})
			v.Steps = st
			return v
		}, src: `steps.plain.body.id`, wantErr: ErrBody},
		{name: "step has body", src: `has(steps.order.body) && has(steps.order.status) && has(steps.order.headers)`, want: true},
		{name: "attempt from 1", src: `attempt >= 1`, want: true},
		{name: "duration ms", src: `duration > duration("1s") && duration < duration("2s")`, want: true},
		{name: "now is UTC", vars: nonUTC, src: `now.getHours()`, want: 10},
		{name: "now fixed", vars: nonUTC, src: `string(now)`, want: "2026-09-26T10:30:00Z"},
		{name: "response headers join", src: `response.headers["retry-after"]`, want: "5"},
		{name: "response has", src: `has(response.status) && has(response.body) && has(response.headers)`, want: true},
		{name: "error has", src: `has(error.kind)`, want: true},
		{name: "upstream has", src: `has(upstream.name) && has(upstream.endpoint)`, want: true},
		{name: "ai has", src: `has(ai.model) && has(ai.stream) && has(ai.estimatedInputTokens) && has(ai.maxOutputTokens)`, want: true},
		{name: "response body unavailable", vars: func() *expr.Vars {
			v := fullVars()
			v.Response.Body = nil
			return v
		}, src: `response.body.total`, wantErr: ErrBody},
		{name: "request body unavailable", vars: func() *expr.Vars {
			v := fullVars()
			v.Request.Body = nil
			return v
		}, src: `has(request.body) || request.body.x == 1`, wantErr: ErrBody},
	})
}

// TestReq6Determinism evaluates iteration-dependent expressions many times:
// every map iterates in ascending key order (03 req 6.3).
func TestReq6Determinism(t *testing.T) {
	env := testEnv(t)
	vars := fullVars()
	vars.Request.Header = http.Header{}
	for _, k := range []string{"Zeta", "Alpha", "Mid", "Beta", "Omega", "Gamma"} {
		vars.Request.Header[k] = []string{strings.ToLower(k)}
	}
	vars.Request.Body = FromNative(map[string]any{"z": "1", "y": "2", "x": "3", "w": "4"})
	src := `request.headers.map(k, k).join(",") + "|" + request.body.map(k, k).join(",") + "|" + request.query.map(k, k).join(",")`
	prg := compile(t, env, src)
	var first ref.Val
	for i := range 100 {
		out, _, err := prg.Eval(NewActivation(vars))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = out
			if want := "alpha,beta,gamma,mid,omega,zeta|w,x,y,z|q,tag"; string(out.(types.String)) != want {
				t.Fatalf("got %v, want %s", out, want)
			}
		} else if out.Equal(first) != types.True {
			t.Fatalf("evaluation %d = %v, first %v", i, out, first)
		}
	}
}

// TestObjectConstructionRejected checks that a Ruralz view cannot be built
// in CEL (the data plane builds views): the construction type-checks, so
// validation rejects it with IsObjectType, and evaluation fails safe.
func TestObjectConstructionRejected(t *testing.T) {
	env := testEnv(t)
	ast, iss := env.Compile(`ruralz.Error{kind: "connect"}`)
	if iss.Err() != nil {
		t.Skipf("construction rejected at check time: %v", iss.Err())
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := prg.Eval(NewActivation(&expr.Vars{})); !errors.Is(err, ErrUnsupported) {
		t.Errorf("construction err = %v, want ErrUnsupported", err)
	}
}

// TestIsObjectType checks the helper validation uses to reject the
// construction of a Ruralz type as RZ-CFG-014: it names every object type
// (with or without cel-go's leading dot) and nothing else, and it finds the
// struct nodes of a checked AST that construct one, as written or fully
// qualified by the checker (03 req 17, 27).
func TestIsObjectType(t *testing.T) {
	for k := range numKinds {
		if name := k.TypeName(); !IsObjectType(name) || !IsObjectType("."+name) {
			t.Errorf("IsObjectType(%s) = false", name)
		}
	}
	for _, name := range []string{
		"", ".", "ruralz", "ruralz.", "ruralz.request", "ruralz.Bogus", "Request",
		"google.protobuf.Duration", "google.protobuf.Struct", "ruralz.Request.headers",
	} {
		if IsObjectType(name) {
			t.Errorf("IsObjectType(%q) = true", name)
		}
	}
	env := testEnv(t)
	tests := []struct {
		src  string
		want int // struct nodes constructing a Ruralz type
	}{
		{`ruralz.Error{kind: "connect"}.kind == "connect"`, 1},
		{`.ruralz.Error{} == .ruralz.Error{}`, 2},
		{`[ruralz.Upstream{name: "a"}].size() == 1`, 1},
		{`google.protobuf.Duration{seconds: 1} == duration("1s")`, 0},
		{`type(request) == ruralz.Request`, 0},
		{`request.method == "GET"`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			checked, iss := env.Compile(tc.src)
			if iss.Err() != nil {
				t.Fatalf("compile: %v", iss.Err())
			}
			got := 0
			celast.PreOrderVisit(checked.NativeRep().Expr(), celast.NewExprVisitor(func(e celast.Expr) {
				if e.Kind() == celast.StructKind && IsObjectType(e.AsStruct().TypeName()) {
					got++
				}
			}))
			if got != tc.want {
				t.Errorf("Ruralz struct nodes = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestEvalWithoutOptimize runs field access through the unoptimized planner
// as well (cel-go plans attributes the same way; this guards regressions).
func TestEvalWithoutOptimize(t *testing.T) {
	env := testEnv(t)
	ast, iss := env.Compile(`request.headers["accept"] + "|" + consumer.name + "|" + string(steps.order.body.id)`)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptTrackState))
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := prg.Eval(NewActivation(fullVars()))
	if err != nil {
		t.Fatal(err)
	}
	assertVal(t, "combined", out, "application/json, text/plain|acme|7")
}

// TestTrackedEvaluation evaluates through cel-go's cost tracker and
// ContextEval with interrupt checks, as tracked programs run (03 req 34,
// 35): the tracker sizes the views through traits.Sizer.
func TestTrackedEvaluation(t *testing.T) {
	env := testEnv(t)
	src := `request.headers.exists(k, k == "accept") && request.body.items.all(i, i != "z") && ` +
		`consumer.tags.size() == 2 && steps.order.headers.size() == 1 && request.query.size() == 2`
	ast, iss := env.Compile(src)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	prg, err := env.Program(ast, cel.CostLimit(1_000_000), cel.InterruptCheckFrequency(100), cel.EvalOptions(cel.OptOptimize, cel.OptTrackCost))
	if err != nil {
		t.Fatal(err)
	}
	out, det, err := prg.ContextEval(t.Context(), NewActivation(fullVars()))
	if err != nil || out != types.True {
		t.Fatalf("ContextEval = %v, %v", out, err)
	}
	if cost := det.ActualCost(); cost == nil || *cost == 0 {
		t.Errorf("actual cost %v", cost)
	}
	// A tight limit stops evaluation with a cost error, not a panic.
	tight, err := env.Program(ast, cel.CostLimit(3))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tight.Eval(NewActivation(fullVars())); err == nil || !strings.Contains(err.Error(), "cost") {
		t.Errorf("tight limit err = %v", err)
	}
}
