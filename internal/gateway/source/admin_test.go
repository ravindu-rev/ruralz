// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package source

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/gateway/admin"
)

// sharedPipeline is the real configuration pipeline the tests share:
// compiling the embedded schema views takes a while.
var sharedPipeline = sync.OnceValues(func() (*pipeline.Pipeline, error) {
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	return pipeline.New(pipeline.Options{Checks: hub.Checks{}, CEL: c, Workers: 1})
})

// gatewayDoc is a Gateway with one listener and the spec lines extra.
func gatewayDoc(extra string) string {
	return "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\nspec:\n  listeners:\n    - name: http\n      protocol: http\n      port: 8080\n" + extra
}

// TestAdminPort: the admin port of the Bundle a Node boots is its
// Gateway's spec.admin.port when the Gateway parses, the default 9901
// when it names none, whatever else of the Bundle fails; nothing when the
// source cannot be read or holds no usable Gateway.
func TestAdminPort(t *testing.T) {
	p, err := sharedPipeline()
	if err != nil {
		t.Fatal(err)
	}
	route := "---\napiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: broken\nspec:\n  match:\n    path:\n      prefix: /\n  upstreams:\n    - name: missing\n"
	tests := []struct {
		name string
		yaml string
		vars subst.Map
		port int
		ok   bool
	}{
		{"named", gatewayDoc("  admin:\n    port: 19901\n"), nil, 19901, true},
		{"default", gatewayDoc(""), nil, 9901, true},
		{"admin without port", gatewayDoc("  admin: {}\n"), nil, 9901, true},
		{"substituted", gatewayDoc("  admin:\n    port: ${ADMIN_PORT}\n"), subst.Map{"ADMIN_PORT": "29901"}, 29901, true},
		{"invalid rest of the Bundle", gatewayDoc("  admin:\n    port: 19902\n") + route, nil, 19902, true},
		{"string port", gatewayDoc("  admin:\n    port: nine\n"), nil, 0, false},
		{"port out of range", gatewayDoc("  admin:\n    port: 70000\n"), nil, 0, false},
		{"admin not a map", gatewayDoc("  admin: 9\n"), nil, 0, false},
		{"no Gateway", "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: u\nspec:\n  protocol: http\n", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ruralz.yaml"), []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			w, err := New(Config{Path: dir, Pipeline: &fakePipe{real: p}, Variables: tt.vars})
			if err != nil {
				t.Fatal(err)
			}
			port, ok := w.AdminPort(t.Context())
			if port != tt.port || ok != tt.ok {
				t.Fatalf("AdminPort = %d, %v; want %d, %v", port, ok, tt.port, tt.ok)
			}
		})
	}
	t.Run("missing source", func(t *testing.T) {
		w, err := New(Config{Path: filepath.Join(t.TempDir(), "absent"), Pipeline: &fakePipe{real: p}})
		if err != nil {
			t.Fatal(err)
		}
		if port, ok := w.AdminPort(t.Context()); ok {
			t.Fatalf("AdminPort = %d, true for a missing source", port)
		}
	})
}

func TestDefaultAdminPort(t *testing.T) {
	if defaultAdminPort != admin.DefaultPort {
		t.Fatalf("defaultAdminPort = %d, admin.DefaultPort %d", defaultAdminPort, admin.DefaultPort)
	}
}
