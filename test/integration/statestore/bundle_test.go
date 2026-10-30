// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package statestore

import (
	"fmt"
	"strings"
)

// bundle builds the rendered test Bundles: one Gateway with the web
// listener and the admin port, which the harness moves to free ports, the
// mock Upstream "backend" and the documents a test adds.
type bundle struct {
	// stateStore is the Gateway's stateStore block, YAML indented by two
	// spaces under spec; "" leaves it out.
	stateStore string
	// upstream is the mock Upstream's address.
	upstream string
	// upstreamSpec holds extra lines of the Upstream's spec, indented
	// under spec.
	upstreamSpec []string
	// docs are the further documents: Policies, Routes, Consumers.
	docs []string
}

// render returns the Bundle as one multi-document YAML file.
func (b bundle) render() []byte {
	var s strings.Builder
	s.WriteString("apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\nspec:\n  admin:\n    port: 9901\n" +
		"  listeners:\n    - name: web\n      protocol: http\n      port: 8080\n")
	if b.stateStore != "" {
		s.WriteString(b.stateStore)
	}
	fmt.Fprintf(&s, "---\napiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: backend\nspec:\n  protocol: http\n"+
		"  endpoints:\n    - address: %q\n", b.upstream)
	for _, l := range b.upstreamSpec {
		s.WriteString("  " + l + "\n")
	}
	for _, d := range b.docs {
		s.WriteString("---\n")
		s.WriteString(d)
	}
	return []byte(s.String())
}

// redisStore returns a stateStore block of driver redis with topology
// topo, its url read from the file secret of ref ("file:<path>"), and
// extra lines indented under stateStore.
func redisStore(topo, ref string, extra ...string) string {
	var s strings.Builder
	fmt.Fprintf(&s, "  stateStore:\n    driver: redis\n    topology: %s\n    url: %s\n", topo, secretRef(ref))
	for _, l := range extra {
		s.WriteString("    " + l + "\n")
	}
	return s.String()
}

// cacheBlock returns the lines of a stateStore.cache connection of
// topology topo with its url read from ref, for redisStore's extra.
func cacheBlock(topo, ref string) []string {
	return []string{"cache:", "  topology: " + topo, "  url: " + secretRef(ref)}
}

// secretRef returns the inline SecretValue of a "provider:name" reference.
func secretRef(ref string) string {
	provider, name, _ := strings.Cut(ref, ":")
	return fmt.Sprintf("{secretRef: {provider: %s, name: %s}}", provider, name)
}

// route returns a Route matching the path prefix to backend with the
// Policies named.
func route(name, prefix string, policies ...string) string {
	var s strings.Builder
	fmt.Fprintf(&s, "apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: %s\nspec:\n  match:\n    path:\n      prefix: %s\n", name, prefix)
	if len(policies) > 0 {
		s.WriteString("  policies:\n")
		for _, p := range policies {
			fmt.Fprintf(&s, "    - name: %s\n", p)
		}
	}
	s.WriteString("  upstreams:\n    - name: backend\n")
	return s.String()
}

// policy returns a Policy of type typ whose spec carries the extra lines,
// indented under spec.
func policy(name, typ string, spec ...string) string {
	var s strings.Builder
	fmt.Fprintf(&s, "apiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata:\n  name: %s\nspec:\n  type: %s\n", name, typ)
	for _, l := range spec {
		s.WriteString("  " + l + "\n")
	}
	return s.String()
}

// rateLimit returns a ratelimit Policy keyed by the x-client request
// header with one limit of requests per window, burst and failureMode
// given ("" keeps the defaults).
func rateLimit(name string, requests int, window, burst, failureMode string) string {
	limit := fmt.Sprintf("{requests: %d, window: %s", requests, window)
	if burst != "" {
		limit += ", burst: " + burst
	}
	limit += "}"
	spec := []string{"config:", `  key: 'request.headers["x-client"]'`, "  limits: [" + limit + "]"}
	if failureMode != "" {
		spec = append(spec, "failureMode: "+failureMode)
	}
	return policy(name, "ratelimit", spec...)
}

// consumer returns the Consumer the test API key identifies, with the
// requests quota "q" of limit per window.
func consumer(limit int, window string) string {
	return fmt.Sprintf("apiVersion: ruralz/v1alpha1\nkind: Consumer\nmetadata:\n  name: partner\nspec:\n  credentials:\n    apiKeys:\n"+
		"      - name: primary\n        hash: %q\n  quotas:\n    - {name: q, unit: requests, limit: %d, window: %s}\n", apiKeyHash, limit, window)
}

// apiKeyPolicy returns the auth.api-key Policy reading x-api-key.
func apiKeyPolicy() string {
	return policy("apikey", "auth.api-key", "config: {header: x-api-key}")
}
