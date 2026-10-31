// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the registered fields at stage F: the quota config, closed,
// and the Consumer's quotas[].calendar beside window.

// schemaDiagnostics validates the JSON document src, a resource of kind
// named name, against the embedded rendered schema (stage F) and returns
// its findings.
func schemaDiagnostics(t *testing.T, kind v1alpha1.Kind, name, src string) diag.List {
	t.Helper()
	idx, err := schemaidx.Embedded()
	if err != nil {
		t.Fatalf("schemaidx.Embedded: %v", err)
	}
	set, err := schemaview.Embedded(idx)
	if err != nil {
		t.Fatalf("schemaview.Embedded: %v", err)
	}
	files := &tree.FileTable{}
	file := files.Add(tree.File{Path: "doc.json"})
	docs, ds, err := profile.Parse(t.Context(), []byte(src), file, "doc.json", profile.FormatJSON, profile.Options{})
	if err != nil || len(ds) != 0 || len(docs) != 1 {
		t.Fatalf("profile.Parse = %d documents, %v, %v", len(docs), ds, err)
	}
	res := &tree.Resource{
		ID:         tree.ID{Kind: kind, Name: name},
		APIVersion: "ruralz/v1alpha1", Root: docs[0].Root, Start: docs[0].Start,
	}
	out, ok := set.Validate(res, files)
	if !ok {
		t.Fatal("ruralz/v1alpha1 is not served")
	}
	return out
}

// codeUnknown is RZ-CFG-006, an unknown field.
const codeUnknown = "RZ-CFG-006"

// wantSchema fails unless the findings ds are code alone, or none for an
// empty code.
func wantSchema(t *testing.T, what string, ds diag.List, code string) {
	t.Helper()
	if (len(ds) > 0) != (code != "") {
		t.Errorf("%s: schema diagnostics %v, want code %q", what, ds, code)
	}
	for _, d := range ds {
		if d.Code != code {
			t.Errorf("%s: code %s, want %s", what, d.Code, code)
		}
	}
}

// TestSchemaClosedConfig checks the quota config at stage F: its
// registered fields pass, cost is a CEL string, an empty key or cost is
// refused (RZ-CFG-005), as an empty expression in any CEL field is, and an
// unknown member is refused (RZ-CFG-006).
func TestSchemaClosedConfig(t *testing.T) {
	for _, tc := range []struct {
		name, config, code string
	}{
		{"every field", `{"consumerQuota": "daily", "key": "consumer.name", "cost": "request.method == \"POST\" ? 5 : 1"}`, ""},
		{"unknown member", `{"consumerQuota": "daily", "units": 2}`, codeUnknown},
		{"cost not a string", `{"consumerQuota": "daily", "cost": 2}`, CodeConfig},
		{"empty key", `{"consumerQuota": "daily", "key": ""}`, CodeConfig},
		{"empty cost", `{"consumerQuota": "daily", "cost": ""}`, CodeConfig},
		{"no consumerQuota", `{"cost": "1"}`, CodeConfig},
	} {
		src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "q"},` +
			`"spec": {"type": "quota", "config": ` + tc.config + `}}`
		wantSchema(t, tc.name, schemaDiagnostics(t, v1alpha1.KindPolicy, "q", src), tc.code)
	}
}

// TestSchemaQuotaCalendar checks a Consumer quota at stage F: exactly one
// of window and calendar, and month the one calendar period.
func TestSchemaQuotaCalendar(t *testing.T) {
	for _, tc := range []struct {
		name, quota, code string
	}{
		{"window", `, "window": "24h"`, ""},
		{"calendar month", `, "calendar": "month"`, ""},
		{"both", `, "window": "24h", "calendar": "month"`, CodeConfig},
		{"neither", ``, CodeConfig},
		{"another period", `, "calendar": "year"`, CodeConfig},
	} {
		src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Consumer", "metadata": {"name": "acme"}, "spec": {` +
			`"credentials": {"apiKeys": [{"name": "primary", "hash": "sha256:d1bec0f9342e5607570b2636ed0256023b13be7d7d4bb45e1bed3cfee80dc1b6"}]},` +
			`"quotas": [{"name": "q", "unit": "requests", "limit": 5` + tc.quota + `}]}}`
		wantSchema(t, tc.name, schemaDiagnostics(t, v1alpha1.KindConsumer, "acme", src), tc.code)
	}
}
