// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/ext"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// allVars is every variable with a declared type.
const allVars = expr.VarsBase | expr.VarResponse | expr.VarError | expr.VarUpstream |
	expr.VarAttempt | expr.VarSteps | expr.VarDuration | expr.VarAI

// testEnv returns a cel-go environment declaring every variable with the
// Provider, the standard library and the strings and encoders extensions
// (the 03 req 4 surface).
func testEnv(tb testing.TB) *cel.Env {
	tb.Helper()
	p, err := NewProvider()
	if err != nil {
		tb.Fatalf("NewProvider: %v", err)
	}
	opts := []cel.EnvOption{
		cel.CustomTypeProvider(p),
		cel.CustomTypeAdapter(p),
		ext.Strings(ext.StringsVersion(5)),
		ext.Encoders(ext.EncodersVersion(1)),
	}
	for _, v := range Variables(allVars) {
		opts = append(opts, cel.Variable(v.Name, v.Type))
	}
	env, err := cel.NewEnv(opts...)
	if err != nil {
		tb.Fatalf("NewEnv: %v", err)
	}
	return env
}

// compile type-checks src and plans an optimized program.
func compile(tb testing.TB, env *cel.Env, src string) cel.Program {
	tb.Helper()
	ast, iss := env.Compile(src)
	if iss.Err() != nil {
		tb.Fatalf("compile %q: %v", src, iss.Err())
	}
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptOptimize))
	if err != nil {
		tb.Fatalf("program %q: %v", src, err)
	}
	return prg
}

// jsonNum is a decoded JSON number literal.
func jsonNum(s string) json.Number { return json.Number(s) }

// fullVars returns an activation with every variable set.
func fullVars() *expr.Vars {
	req := &expr.Request{
		Method: "GET",
		Scheme: "https",
		Host:   "api.example.com",
		Path:   "/orders/42",
		PathParams: []expr.Param{
			{Name: "orderId", Value: "42"},
			{Name: "customer", Value: "acme"},
		},
		Header: http.Header{
			"X-Tenant":   {"acme"},
			"Accept":     {"application/json", "text/plain"},
			"User-Agent": {"curl/8.0"},
		},
		Body: FromNative(map[string]any{"b": jsonNum("1"), "a": jsonNum("2"), "items": []any{"x", "y"}}),
	}
	req.SetRawQuery("tag=a&tag=b&q=hello%20world")
	steps := &expr.Steps{}
	steps.Add("order", expr.Step{Status: 200, Header: http.Header{"Etag": {`"v1"`}}, Body: FromNative(map[string]any{"id": jsonNum("7")})})
	return &expr.Vars{
		Request: req,
		Source: &expr.Source{
			IP: netip.MustParseAddr("::ffff:192.0.2.1"), Port: 44321, TLSVersion: 0x0304,
			ClientCertSubject: "CN=client,O=Acme",
		},
		Route: &expr.Route{Name: "orders", Labels: map[string]string{"team": "core", "env": "prod"}},
		Consumer: &expr.Consumer{
			Name: "acme", Tier: "gold", Tags: []string{"beta", "eu"},
			Labels: map[string]string{"org": "acme"}, Quotas: []string{"daily-tokens"},
		},
		Auth:     &expr.Auth{Method: "jwt", Claims: FromNative(map[string]any{"scope": "orders:read orders:write", "sub": "u1"})},
		Response: &expr.Response{Status: 503, Header: http.Header{"Retry-After": {"5"}}, Body: FromNative(map[string]any{"total": jsonNum("12.5")})},
		Error:    &expr.AttemptError{Kind: "reset"},
		Upstream: &expr.Upstream{Name: "orders-v1", Endpoint: "10.0.0.7:8080"},
		Attempt:  2,
		Steps:    steps,
		AI:       &expr.AI{Model: "small", EstimatedInputTokens: 1200, MaxOutputTokens: 256, Stream: true},
		Now:      time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC),
		Duration: 1500 * time.Millisecond,
	}
}
