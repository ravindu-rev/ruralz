// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"errors"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
)

// gatewaySchema returns a minimal rendered schema of the hub apiVersion
// whose Gateway spec holds admin and limits with the given members.
func gatewaySchema(admin, limits string) string {
	return `{
  "type": "object",
  "properties": {"apiVersion": {"const": "ruralz/v1alpha1"}, "kind": {"enum": ["Gateway"]}},
  "allOf": [{"if": {"properties": {"kind": {"const": "Gateway"}}, "required": ["kind"]}, "then": {"$ref": "#/$defs/Gateway"}}],
  "$defs": {
    "Gateway": {"type": "object", "properties": {"kind": {"const": "Gateway"}, "spec": {"$ref": "#/$defs/GatewaySpec"}}},
    "GatewaySpec": {"type": "object", "properties": {
      "admin": {"type": "object", "properties": {` + admin + `}},
      "limits": {"type": "object", "properties": {` + limits + `}}}}
  }
}`
}

// TestGatewayDefaultsFromSchema: New reads admin.port,
// limits.maxCompositionSteps and limits.maxResponseBodyBytes defaults from
// the hub schema, numbers or byte quantities, and refuses a schema that
// lacks one or gives one that is no integer.
func TestGatewayDefaultsFromSchema(t *testing.T) {
	const (
		port  = `"port": {"type": "integer", "default": 9000}`
		steps = `"maxCompositionSteps": {"type": "integer", "default": 4}`
		body  = `"maxResponseBodyBytes": {"type": "string", "default": "2Mi"}`
	)
	tests := []struct {
		name, admin, limits, err string
		want                     schemaDefaults
	}{
		{"all present", port, steps + ", " + body, "", schemaDefaults{adminPort: 9000, maxSteps: 4, maxResponse: 2 << 20}},
		{"no admin port", `"other": {"type": "integer"}`, steps + ", " + body, "no Gateway spec member [admin port]", schemaDefaults{}},
		{"no default", `"port": {"type": "integer"}`, steps + ", " + body, "gives Gateway spec member [admin port] no default", schemaDefaults{}},
		{"fraction", port, `"maxCompositionSteps": {"type": "number", "default": 1.5}, ` + body, "is not an integer", schemaDefaults{}},
		{"bad quantity", port, steps + `, "maxResponseBodyBytes": {"type": "string", "default": "lots"}`, "invalid byte size", schemaDefaults{}},
		{"not a number", port, steps + `, "maxResponseBodyBytes": {"type": "boolean", "default": true}`, "is bool, not a number", schemaDefaults{}},
	}
	for _, tt := range tests {
		idx, err := schemaidx.Load("ruralz/v1alpha1", []byte(gatewaySchema(tt.admin, tt.limits)))
		if err != nil {
			t.Fatalf("%s: Load: %v", tt.name, err)
		}
		set, err := schemaidx.NewSet(idx)
		if err != nil {
			t.Fatal(err)
		}
		v, err := New(Options{Schemas: set, Registry: registry.New()})
		if tt.err == "" {
			if err != nil || v.defaults != tt.want {
				t.Errorf("%s: New = %+v, %v; want defaults %+v", tt.name, v, err, tt.want)
			}
			continue
		}
		if !errors.Is(err, ErrOptions) || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%s: New error %v, want ErrOptions saying %q", tt.name, err, tt.err)
		}
	}
	noGateway := strings.NewReplacer(`{"enum": ["Gateway"]}`, `{"enum": ["Other"]}`, `{"const": "Gateway"}`, `{"const": "Other"}`).
		Replace(gatewaySchema(port, steps+", "+body))
	idx, err := schemaidx.Load("ruralz/v1alpha1", []byte(noGateway))
	if err != nil {
		t.Fatal(err)
	}
	set, err := schemaidx.NewSet(idx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Schemas: set, Registry: registry.New()}); !errors.Is(err, ErrOptions) || !strings.Contains(err.Error(), "has no Gateway") {
		t.Errorf("a hub schema without Gateway: New error %v", err)
	}
}
