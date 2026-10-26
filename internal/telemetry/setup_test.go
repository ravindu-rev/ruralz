// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"os"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestScrubOTELEnvironment checks that every OTEL_* variable is gone after
// the scrub, its value stays in the snapshot, and other variables stay.
func TestScrubOTELEnvironment(t *testing.T) {
	vars := map[string]string{
		"OTEL_RESOURCE_ATTRIBUTES":    "service.name=evil",
		"OTEL_EXPORTER_OTLP_HEADERS":  "authorization=secret",
		"OTEL_TRACES_SAMPLER":         "always_off",
		"OTEL_SERVICE_NAME":           "evil",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector:4317",
		"XOTEL_A":                     "kept",
		"RURALZ_LOG_LEVEL":            "info",
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
	env := ScrubOTELEnvironment()
	for k, v := range vars {
		got, ok := env.Lookup(k)
		if !ok || got != v {
			t.Errorf("Lookup(%s) = %q, %v; want %q", k, got, ok, v)
		}
		_, live := os.LookupEnv(k)
		if want := k[:5] != "OTEL_"; live != want {
			t.Errorf("%s in the environment after the scrub = %v, want %v", k, live, want)
		}
	}
	if _, ok := env.Lookup("RURALZ_NOT_SET"); ok {
		t.Error("Lookup of an unset variable succeeded")
	}
}

// TestResourceHoldsOnlyTheService checks the resource: service.name,
// service.version and service.instance.id under the v1.43.0 schema, and
// nothing else.
func TestResourceHoldsOnlyTheService(t *testing.T) {
	res, err := newResource(ServiceGateway, "1.2.3", testNodeID)
	if err != nil {
		t.Fatal(err)
	}
	if res.SchemaURL() != semconv.SchemaURL {
		t.Errorf("schema = %q", res.SchemaURL())
	}
	want := []attribute.KeyValue{
		semconv.ServiceInstanceID(testNodeID),
		semconv.ServiceName("ruralzd"),
		semconv.ServiceVersion("1.2.3"),
	}
	if got := res.Attributes(); len(got) != len(want) {
		t.Fatalf("attributes = %v, want %v", got, want)
	}
	for _, kv := range want {
		if v, ok := res.Set().Value(kv.Key); !ok || v != kv.Value {
			t.Errorf("%s = %v, want %v", kv.Key, v, kv.Value)
		}
	}
	if _, err := newResource(Service(9), "1", testNodeID); err == nil {
		t.Error("unknown service accepted")
	}
	if _, err := newResource(ServiceGateway, "1", ""); err == nil {
		t.Error("empty instance ID accepted")
	}
	if ServiceControl.Name() != "ruralz-control" {
		t.Errorf("ServiceControl.Name() = %q", ServiceControl.Name())
	}
}

// TestCompileSettingsEndpoint checks the endpoint rules: http or https, a
// host and an optional port defaulting to 4317, nothing else.
func TestCompileSettingsEndpoint(t *testing.T) {
	cases := []struct {
		endpoint  string
		target    string
		cleartext bool
		bad       bool
	}{
		{endpoint: "https://collector:4317", target: "collector:4317"},
		{endpoint: "https://collector", target: "collector:4317"},
		{endpoint: "https://collector/", target: "collector:4317"},
		{endpoint: "http://127.0.0.1:9000", target: "127.0.0.1:9000", cleartext: true},
		{endpoint: "https://[::1]:4317", target: "[::1]:4317"},
		{endpoint: "grpc://collector", bad: true},
		{endpoint: "https://user@collector", bad: true},
		{endpoint: "https://collector/v1/traces", bad: true},
		{endpoint: "https://collector?x=1", bad: true},
		{endpoint: "https://collector#f", bad: true},
		{endpoint: "https://collector:", bad: true},
		{endpoint: "collector:4317", bad: true},
		{endpoint: "https://", bad: true},
	}
	for _, tc := range cases {
		s, err := CompileSettings(&v1alpha1.Telemetry{OTLP: &v1alpha1.OTLP{Endpoint: tc.endpoint}}, nil)
		if tc.bad {
			if code, _ := errcode.CodeOf(err); code != "RZ-CFG-005" {
				t.Errorf("CompileSettings(%q) error = %v, want RZ-CFG-005", tc.endpoint, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("CompileSettings(%q): %v", tc.endpoint, err)
			continue
		}
		if s.target != tc.target || s.cleartext != tc.cleartext || (s.tls == nil) != tc.cleartext {
			t.Errorf("CompileSettings(%q) = target %q, cleartext %v, tls %v", tc.endpoint, s.target, s.cleartext, s.tls != nil)
		}
		s.Close()
	}
}

// TestCompileSettingsSampling checks the root ratio: 0.01 by default,
// clamped to [0, 1].
func TestCompileSettingsSampling(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		t    *v1alpha1.Telemetry
		want float64
	}{
		{nil, 0.01},
		{&v1alpha1.Telemetry{}, 0.01},
		{&v1alpha1.Telemetry{TraceSampling: f(0.25)}, 0.25},
		{&v1alpha1.Telemetry{TraceSampling: f(2)}, 1},
		{&v1alpha1.Telemetry{TraceSampling: f(-1)}, 0},
	}
	for _, tc := range cases {
		s, err := CompileSettings(tc.t, nil)
		if err != nil || s.Sampling() != tc.want || s.target != "" {
			t.Errorf("CompileSettings(%+v) = %v, %v; want %v", tc.t, s.Sampling(), err, tc.want)
		}
	}
}

// TestCompileSettingsTLS checks the collector's TLS client settings: an
// unresolved reference is RZ-CFG-026, a lone client certificate RZ-CFG-005,
// and the key changes with the server name or a reference.
func TestCompileSettingsTLS(t *testing.T) {
	caRef := v1alpha1.SecretRef{Provider: "file", Name: "/etc/ruralz/otel/ca.crt"}
	otlp := func(tls *v1alpha1.UpstreamTLS) *v1alpha1.Telemetry {
		return &v1alpha1.Telemetry{OTLP: &v1alpha1.OTLP{Endpoint: "https://collector:4317", TLS: tls}}
	}
	if _, err := CompileSettings(otlp(&v1alpha1.UpstreamTLS{CACertificate: &v1alpha1.SecretValue{SecretRef: caRef}}), fakeStore{}); err == nil {
		t.Error("unresolved CA accepted")
	} else if code, _ := errcode.CodeOf(err); code != "RZ-CFG-026" {
		t.Errorf("unresolved CA error = %v, want RZ-CFG-026", err)
	}
	cert := &v1alpha1.SecretValue{SecretRef: v1alpha1.SecretRef{Provider: "file", Name: "/etc/ruralz/otel/c.crt"}}
	if _, err := CompileSettings(otlp(&v1alpha1.UpstreamTLS{ClientCertificate: cert}), fakeStore{}); err == nil {
		t.Error("client certificate without key accepted")
	} else if code, _ := errcode.CodeOf(err); code != "RZ-CFG-005" {
		t.Errorf("lone client certificate error = %v, want RZ-CFG-005", err)
	}
	a, err := CompileSettings(otlp(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := CompileSettings(otlp(&v1alpha1.UpstreamTLS{SNI: "otel.internal"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	again, err := CompileSettings(otlp(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if a.key == b.key || a.key != again.key {
		t.Errorf("keys: system roots %q, sni %q, again %q", a.key, b.key, again.key)
	}
}
