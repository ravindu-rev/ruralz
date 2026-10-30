// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestDecodeGatewayM1Fields decodes the fields M1 adds to Gateway:
// stateStore.cache (OQ-scalability-and-distributed-state-3 (a)),
// telemetry.otlp.tls with sni, caCertificate, clientCertificate and
// clientKey (OQ-observability-2 (a)), and the zone maps stateStore.zones
// and stateStore.cache.zones, each the connection's own.
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
	    "zones": [{"host": "valkey-0.state.internal", "zone": "eu-west-1a"}, {"host": "10.0.2.15", "zone": "eu-west-1b"}],
	    "cache": {
	      "topology": "cluster",
	      "url": {"secretRef": {"provider": "env", "name": "RURALZ_CACHE_URL"}},
	      "zones": [{"host": "cache.state.internal", "zone": "eu-west-1a"}]
	    }
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
	want := []StateStoreZone{{Host: "valkey-0.state.internal", Zone: "eu-west-1a"}, {Host: "10.0.2.15", Zone: "eu-west-1b"}}
	if z := g.StateStore.Zones; len(z) != len(want) || z[0] != want[0] || z[1] != want[1] {
		t.Fatalf("stateStore.zones = %+v, want %+v", z, want)
	}
	if z := c.Zones; len(z) != 1 || z[0] != (StateStoreZone{Host: "cache.state.internal", Zone: "eu-west-1a"}) {
		t.Fatalf("stateStore.cache.zones = %+v", z)
	}
}

// TestDecodeUpstreamTimeoutFields decodes the dial, TLS handshake, stall
// and ping bounds of an Upstream (docs/architecture/09-traffic-management-and-resilience.md
// "Deadlines" and "Health checking and outlier detection"), and checks
// that the breaker guards are fixed values rather than circuitBreaker
// fields and that telemetry.otlp.tls has no handshakeTimeout.
func TestDecodeUpstreamTimeoutFields(t *testing.T) {
	const doc = `{
	  "protocol": "http",
	  "endpoints": [{"address": "orders:8080"}],
	  "connectTimeout": "250ms",
	  "tls": {"sni": "orders.internal", "handshakeTimeout": "3s"},
	  "healthCheck": {"active": {"path": "/healthz", "interval": "5s"}, "stallTimeout": "1500ms", "pingTimeout": "10s"},
	  "circuitBreaker": {"consecutiveFailures": 7, "maxConnections": 64}
	}`
	var u UpstreamSpec
	if err := json.Unmarshal([]byte(doc), &u); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  *Duration
		want time.Duration
	}{
		{"connectTimeout", u.ConnectTimeout, 250 * time.Millisecond},
		{"tls.handshakeTimeout", u.TLS.HandshakeTimeout, 3 * time.Second},
		{"healthCheck.stallTimeout", u.HealthCheck.StallTimeout, 1500 * time.Millisecond},
		{"healthCheck.pingTimeout", u.HealthCheck.PingTimeout, 10 * time.Second},
	} {
		if c.got == nil || time.Duration(*c.got) != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if cb := u.CircuitBreaker; *cb.ConsecutiveFailures != 7 || *cb.MaxConnections != 64 {
		t.Fatalf("circuitBreaker = %+v", cb)
	}
	if *u.HealthCheck.Active.Path != "/healthz" {
		t.Fatalf("active path = %v", u.HealthCheck.Active.Path)
	}
	strict := func(src string, v any) error {
		dec := json.NewDecoder(strings.NewReader(src))
		dec.DisallowUnknownFields()
		return dec.Decode(v)
	}
	for _, guard := range []string{"minimumLegs", "failureRatio", "halfOpenSuccesses"} {
		var cb CircuitBreaker
		if err := strict(`{"`+guard+`": 1}`, &cb); err == nil {
			t.Errorf("circuitBreaker.%s decoded; the breaker guards are fixed values", guard)
		}
	}
	var otlp OTLPTLS
	if err := strict(`{"sni": "collector.internal", "handshakeTimeout": "2s"}`, &otlp); err == nil {
		t.Error("telemetry.otlp.tls decoded handshakeTimeout, an Upstream tls field only")
	}
}

// TestHeadersConfigPresence covers add and remove lists, and an empty
// literal value that stays distinct from an absent one.
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

// TestReplaceWithoutPath checks that path is optional: a replace entry
// without path rewrites the raw body.
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

// TestAPIKeyHeaderPresence checks that the header field is a pointer, so
// its x-api-key default materializes into present objects only.
func TestAPIKeyHeaderPresence(t *testing.T) {
	var c AuthAPIKeyConfig
	if err := json.Unmarshal([]byte(`{}`), &c); err != nil || c.Header != nil {
		t.Fatalf("absent header = %v, %v", c.Header, err)
	}
	if err := json.Unmarshal([]byte(`{"header": "x-partner-key"}`), &c); err != nil || *c.Header != "x-partner-key" {
		t.Fatalf("header = %v, %v", c.Header, err)
	}
}
