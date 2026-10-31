// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
)

// checkedToolchain is the pipeline with the offline checks of every Policy
// type, as every binary builds it, and its CEL compiler.
var checkedToolchain = sync.OnceValues(func() (*toolchain, error) {
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	p, err := pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: c, Workers: 1})
	if err != nil {
		return nil, err
	}
	return &toolchain{pipe: p, cel: c}, nil
})

// zeroReader is an io.Reader of zero bytes: a fixed throttle key.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestBuiltinTables covers the compiler over the real Filter tables
// (internal/filter/builtin), as ruralzd injects them, and the offline
// checks: a Revision attaching the served Policy types that need no fetch
// builds every attachment with the BuildEnv the compiler gives, its limit
// Filters consumptive, and a Route without a cache Policy that overlaps a
// cache Route gets that Route's Response Cache Filter, an Invalidator, as
// its invalidation entry; Discard closes every Filter.
func TestBuiltinTables(t *testing.T) {
	tc, err := checkedToolchain()
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(d *Deps) {
		table, err := builtin.New(builtin.Options{Clock: d.Clock, Entropy: zeroReader{}, Rand: fixedSource{}})
		if err != nil {
			t.Fatal(err)
		}
		d.Filters, d.Pipeline, d.CEL = table, tc.pipe, tc.cel
	})
	h.tc = tc
	c := h.mustCompile(h.validate(
		gateway(attach("cors")),
		upstreamDoc("orders", "127.0.0.1:9001", attach("up-hdr")),
		routeDoc("shop", "/shop", "orders", attach("key", "ip", "rl", "quota", "cache", "hdr", "resp")),
		routeDoc("writer", "/shop/", "orders", attach("hdr")),
		routeDoc("checkout", "/checkout", "orders", attach("rule", "schema", "body")),
		policyDoc("cors", "cors", corsConfig, ""),
		policyDoc("key", "auth.api-key", `{}`, ""),
		policyDoc("ip", "authz.ip", `{"allow":["10.0.0.0/8"]}`, ""),
		policyDoc("rl", "ratelimit", `{"key":"source.ip","limits":[{"requests":100,"window":"1s"}]}`, ""),
		policyDoc("quota", "quota", `{"consumerQuota":"daily"}`, ""),
		policyDoc("cache", "cache", cacheConfig, ""),
		policyDoc("hdr", "headers", headersConfig, ""),
		policyDoc("resp", "transform.response", `{"set":[{"target":"header","name":"x-r","valueExpression":"\"1\""}]}`, ""),
		policyDoc("rule", "authz.cel", `{"rule":"request.body.amount < 100"}`, ""),
		policyDoc("schema", "validation.json-schema", `{"schema":{"type":"object"}}`, ""),
		policyDoc("body", "transform.request", `{"body":"request.body"}`, ""),
		policyDoc("up-hdr", "headers", headersConfig, ""),
		"apiVersion: ruralz/v1alpha1\nkind: Consumer\nmetadata:\n  name: acme\nspec:\n  credentials:\n    apiKeys:\n"+
			"      - name: k1\n        hash: \"sha256:abababababababababababababababababababababababababababababababab\"\n"+
			"  quotas:\n    - name: daily\n      unit: requests\n      limit: 1000\n      window: 24h\n",
	), nil)
	routes := map[string]*snapshot.Route{}
	for _, rt := range c.Snapshot.Routes {
		routes[rt.Name] = rt
	}
	var cacheFilter filter.Filter
	for _, p := range routes["shop"].Chain.Policies {
		if p.Filter == nil {
			t.Errorf("shop: %s has no Filter", p.Name)
		}
		switch p.Name {
		case "rl", "quota":
			if p.Consumptive == nil {
				t.Errorf("shop: %s is not consumptive", p.Name)
			}
		case "cache":
			cacheFilter = p.Filter
		}
	}
	entry := entries(routes["writer"])
	if len(entry) != 1 || entry[0].Filter != cacheFilter {
		t.Fatalf("writer invalidation entries = %v, want shop's cache Filter", names(entry))
	}
	if _, ok := entry[0].Filter.(filter.Invalidator); !ok {
		t.Errorf("the Response Cache Filter %T is no Invalidator", entry[0].Filter)
	}
	if body := routes["checkout"].Body; !body.RequestGate || body.ResponseTee {
		t.Errorf("checkout Body = %+v, want the request gated, no tee", body)
	}
	if body := routes["shop"].Body; body.RequestGate || !body.ResponseTee {
		t.Errorf("shop Body = %+v, want the response teed, the request not gated", body)
	}
	c.Discard()
}
