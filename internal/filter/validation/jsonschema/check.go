// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

var _ hub.PolicyCheck = Check

// configSchemaPath is spec.config.schema, the prefix of every finding.
func configSchemaPath() diag.Path {
	return diag.Path{diag.Field("spec"), diag.Field("config"), diag.Field("schema")}
}

// Check is the offline check of a validation.json-schema Policy (a
// hub.PolicyCheck run at stage H by ruralz, ruralzd and, from M2,
// ruralz-control): it compiles config.schema exactly as the Node does and
// reports each finding as RZ-CFG-005 at a key-aware path under
// spec.config.schema, an object member as a field and an array element as
// an index (07 req 75). It reads no file or network. A resource whose
// Config is not a decoded ValidationJSONSchemaConfig, or whose schema is
// absent (stage F reports the missing required member), yields nothing.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.ValidationJSONSchemaConfig)
	if !ok || cfg == nil || len(cfg.Schema) == 0 {
		return
	}
	_, doc, issues := compileRaw(cfg.Schema, refusingLoader{})
	for _, is := range issues {
		p := schemaPath(configSchemaPath(), doc, is.Pointer)
		var l diag.Location
		if loc != nil {
			l = loc(p)
		}
		add(diag.Diagnostic{
			Code:     CodeConfig,
			Severity: diag.SeverityError,
			Location: l,
			Resource: &diag.ResourceID{Kind: string(r.Kind), Name: r.Name},
			Path:     p,
			Message:  is.Message,
		})
	}
}
