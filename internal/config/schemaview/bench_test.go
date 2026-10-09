// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// benchGateway is a valid Gateway of typical size.
const benchGateway = `{
  "apiVersion": "ruralz/v1alpha1",
  "kind": "Gateway",
  "metadata": {"name": "edge", "labels": {"team": "platform"}},
  "spec": {
    "listeners": [
      {"name": "http", "port": 8080, "protocol": "http"},
      {"name": "https", "port": 8443, "protocol": "https", "tls": {"certificates": [
        {"name": "shop", "certificate": {"secretRef": {"provider": "file", "name": "/etc/ruralz/shop.crt"}},
         "privateKey": {"secretRef": {"provider": "file", "name": "/etc/ruralz/shop.key"}}}]}}
    ],
    "admin": {"port": 9901},
    "limits": {"maxRequestBodyBytes": "10Mi", "maxResponseBodyBytes": 10485760},
    "stateStore": {"driver": "redis", "url": {"secretRef": {"provider": "env", "name": "RURALZ_STATE_STORE_URL"}}, "timeout": "50ms"},
    "policies": [{"name": "cors"}, {"name": "ratelimit-global"}]
  }
}`

// benchRoute is a Route with three findings.
const benchRoute = `{
  "apiVersion": "ruralz/v1alpha1",
  "kind": "Route",
  "metadata": {"name": "orders"},
  "spec": {
    "match": {"path": {"exact": "/orders", "prefix": "/orders/"}, "methods": ["GET", "GET"]},
    "upstreams": [{"name": "orders", "weight": 1}],
    "timout": "2s"
  }
}`

// BenchmarkCompile measures compiling the embedded rendered view, done
// once per process (01 req 32).
func BenchmarkCompile(b *testing.B) {
	idx := testView(b).Index()
	data := schema.RenderedV1alpha1()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Compile(data, idx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkValidate measures stage F per resource, part of the budget of
// validating 10,000 resources in under 2 s (01 req 54, target).
func BenchmarkValidate(b *testing.B) {
	v := testView(b)
	for _, bc := range []struct {
		name, src string
		findings  int
	}{
		{"valid-gateway", benchGateway, 0},
		{"invalid-route", benchRoute, 3},
	} {
		b.Run(bc.name, func(b *testing.B) {
			files := &tree.FileTable{}
			res := mustResource(b, files, "r.json", bc.src)
			if got := v.Validate(res, files); len(got) != bc.findings {
				b.Fatalf("Validate() = %s, want %d findings", texts(got), bc.findings)
			}
			b.ReportAllocs()
			for b.Loop() {
				v.Validate(res, files)
			}
		})
	}
}
