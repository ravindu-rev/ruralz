// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"
)

// TestDecodeGatewayM1Fields decodes the fields M1 adds to Gateway:
// stateStore.cache (OQ-scalability-and-distributed-state-3 (a)) and
// telemetry.otlp.tls with the UpstreamTLS shape (OQ-observability-2 (a)).
func TestDecodeGatewayM1Fields(t *testing.T) {
	const doc = `{
	  "listeners": [{"name": "http", "protocol": "http", "port": 8080}],
	  "telemetry": {
	    "otlp": {
	      "endpoint": "https://otel-collector:4317",
	      "tls": {
	        "sni": "collector.internal",
	        "caCertificate": {"secretRef": {"provider": "file", "name": "/etc/ruralz/otlp/ca.pem"}},
	        "clientCertificate": {"secretRef": {"provider": "file", "name": "/etc/ruralz/otlp/tls.crt"}},
	        "clientKey": {"secretRef": {"provider": "file", "name": "/etc/ruralz/otlp/tls.key"}}
	      }
	    },
	    "traceSampling": 0
	  },
	  "stateStore": {
	    "driver": "redis",
	    "url": {"secretRef": {"provider": "env", "name": "RURALZ_STATE_STORE_URL"}},
	    "timeout": "50ms",
	    "cache": {"topology": "cluster", "url": {"secretRef": {"provider": "env", "name": "RURALZ_CACHE_URL"}}}
	  }
	}`
	var g GatewaySpec
	if err := json.Unmarshal([]byte(doc), &g); err != nil {
		t.Fatal(err)
	}
	tlsCfg := g.Telemetry.OTLP.TLS
	if tlsCfg == nil || tlsCfg.SNI != "collector.internal" || tlsCfg.ClientKey.SecretRef.Name != "/etc/ruralz/otlp/tls.key" {
		t.Fatalf("otlp.tls = %+v", tlsCfg)
	}
	// An explicit 0 survives decoding (pointer), distinct from the 0.01 default.
	if g.Telemetry.TraceSampling == nil || *g.Telemetry.TraceSampling != 0 {
		t.Fatalf("traceSampling = %v", g.Telemetry.TraceSampling)
	}
	c := g.StateStore.Cache
	if c == nil || *c.Topology != StateStoreTopologyCluster || c.URL.SecretRef.Name != "RURALZ_CACHE_URL" {
		t.Fatalf("stateStore.cache = %+v", c)
	}
	if time.Duration(*g.StateStore.Timeout) != 50*time.Millisecond {
		t.Fatalf("timeout = %v", *g.StateStore.Timeout)
	}
}

// TestDecodeCircuitBreaker decodes the OQ-traffic-management-and-resilience-5
// (a) guard fields and the active health check defaults' fields.
func TestDecodeCircuitBreaker(t *testing.T) {
	const doc = `{
	  "protocol": "http",
	  "endpoints": [{"address": "orders:8080"}],
	  "healthCheck": {"active": {"path": "/healthz", "interval": "5s"}},
	  "circuitBreaker": {"minimumLegs": 40, "failureRatio": 0.25, "halfOpenSuccesses": 1, "maxConnections": 64}
	}`
	var u UpstreamSpec
	if err := json.Unmarshal([]byte(doc), &u); err != nil {
		t.Fatal(err)
	}
	cb := u.CircuitBreaker
	if *cb.MinimumLegs != 40 || *cb.FailureRatio != 0.25 || *cb.HalfOpenSuccesses != 1 || *cb.MaxConnections != 64 {
		t.Fatalf("circuitBreaker = %+v", cb)
	}
	if *u.HealthCheck.Active.Path != "/healthz" {
		t.Fatalf("active path = %v", u.HealthCheck.Active.Path)
	}
}

// TestHeadersConfigPresence covers 07 reqs 20 and 23: add and remove lists,
// and an empty literal value that stays distinct from an absent one.
func TestHeadersConfigPresence(t *testing.T) {
	const doc = `{
	  "request": {
	    "set": [{"name": "x-empty", "value": ""}],
	    "add": [{"name": "x-trace", "valueExpression": "request.id"}],
	    "remove": ["x-forwarded-for"]
	  },
	  "response": {"remove": ["server"], "add": [{"name": "vary", "value": "accept"}]}
	}`
	var h HeadersConfig
	if err := json.Unmarshal([]byte(doc), &h); err != nil {
		t.Fatal(err)
	}
	if v := h.Request.Set[0].Value; v == nil || *v != "" {
		t.Fatalf("empty value lost: %v", v)
	}
	if h.Request.Add[0].Value != nil || h.Request.Add[0].ValueExpression != "request.id" {
		t.Fatalf("add = %+v", h.Request.Add[0])
	}
	if h.Request.Remove[0] != "x-forwarded-for" || h.Response.Remove[0] != "server" || *h.Response.Add[0].Value != "accept" {
		t.Fatalf("headers = %+v %+v", h.Request, h.Response)
	}
	out, err := json.Marshal(h.Request.Set[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"name":"x-empty","value":""}` {
		t.Fatalf("empty value dropped on encode: %s", out)
	}
}

// TestReplaceWithoutPath covers 07 risk 3: a replace entry without path
// rewrites the raw body, so path is optional.
func TestReplaceWithoutPath(t *testing.T) {
	var r Replace
	if err := json.Unmarshal([]byte(`{"pattern": "secret", "replacement": "***"}`), &r); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"pattern":"secret","replacement":"***"}` {
		t.Fatalf("encoded %s", out)
	}
}

// TestAPIKeyHeaderPresence covers 06 req 15: the header field is a pointer
// so its x-api-key default materializes into present objects only.
func TestAPIKeyHeaderPresence(t *testing.T) {
	var c AuthAPIKeyConfig
	if err := json.Unmarshal([]byte(`{}`), &c); err != nil || c.Header != nil {
		t.Fatalf("absent header = %v, %v", c.Header, err)
	}
	if err := json.Unmarshal([]byte(`{"header": "x-partner-key"}`), &c); err != nil || *c.Header != "x-partner-key" {
		t.Fatalf("header = %v, %v", c.Header, err)
	}
}
