// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// schemaDiagnostics validates the JSON Policy document src against the
// embedded rendered schema (stage F) and returns its findings.
func schemaDiagnostics(t *testing.T, src string) diag.List {
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
	file := files.Add(tree.File{Path: "policy.json"})
	docs, ds, err := profile.Parse(t.Context(), []byte(src), file, "policy.json", profile.FormatJSON, profile.Options{})
	if err != nil || len(ds) != 0 || len(docs) != 1 {
		t.Fatalf("profile.Parse = %d documents, %v, %v", len(docs), ds, err)
	}
	res := &tree.Resource{
		ID:         tree.ID{Kind: v1alpha1.KindPolicy, Name: "authz-test"},
		APIVersion: "ruralz/v1alpha1", Root: docs[0].Root, Start: docs[0].Start,
	}
	out, ok := set.Validate(res, files)
	if !ok {
		t.Fatal("ruralz/v1alpha1 is not served")
	}
	return out
}

// TestSchemaRefusesEmptyRule checks that the schema refuses an empty
// config.rule (minLength 1) with RZ-CFG-005 at spec.config.rule, so stage
// F reports it before the Check backstop, and accepts a rule.
func TestSchemaRefusesEmptyRule(t *testing.T) {
	for _, tc := range []struct {
		rule    string
		refused bool
	}{{`""`, true}, {`"consumer != null"`, false}} {
		src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "authz-test"},` +
			`"spec": {"type": "authz.cel", "config": {"rule": ` + tc.rule + `}}}`
		ds := schemaDiagnostics(t, src)
		refused := false
		for _, d := range ds {
			if d.Code == CodeConfig && d.Path.String() == "spec.config.rule" {
				refused = true
			}
		}
		if refused != tc.refused || (!tc.refused && len(ds) != 0) {
			t.Errorf("rule %s: schema diagnostics %v, want refused %v", tc.rule, ds, tc.refused)
		}
	}
}

// TestCheckReportsEmptyRule checks the offline check: an empty
// config.rule is one RZ-CFG-005 error at spec.config.rule, located
// through loc, and a rule, another config type, a nil resource or a nil
// add yield nothing (stage J compiles the rule itself).
func TestCheckReportsEmptyRule(t *testing.T) {
	res := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: "authz-orders"}, Config: &v1alpha1.AuthzCELConfig{}}
	var got []diag.Diagnostic
	loc := func(p diag.Path) diag.Location { return diag.Location{File: "policies.yaml", Line: len(p)} }
	Check(res, loc, func(d diag.Diagnostic) { got = append(got, d) })
	if len(got) != 1 {
		t.Fatalf("Check reported %d diagnostics, want 1: %+v", len(got), got)
	}
	d := got[0]
	if d.Code != CodeConfig || d.Severity != diag.SeverityError || d.Path.String() != "spec.config.rule" || d.Message != "rule is empty" {
		t.Errorf("diagnostic = %s %v at %s %q, want %s at spec.config.rule", d.Code, d.Severity, d.Path, d.Message, CodeConfig)
	}
	if d.Resource == nil || d.Resource.Name != "authz-orders" || d.File != "policies.yaml" || d.Line != 3 {
		t.Errorf("diagnostic resource %+v, location %+v; want the Policy and loc's location", d.Resource, d.Location)
	}
	var n int
	count := func(diag.Diagnostic) { n++ }
	Check(&hub.Resource{Config: &v1alpha1.AuthzCELConfig{Rule: `consumer.tier ==`}}, nil, count)
	Check(&hub.Resource{Config: &v1alpha1.AuthzIPConfig{}}, nil, count)
	Check(&hub.Resource{Config: (*v1alpha1.AuthzCELConfig)(nil)}, nil, count)
	Check(nil, nil, count)
	Check(res, nil, nil)
	if n != 0 {
		t.Errorf("Check reported %d diagnostics for a rule or foreign input, want 0", n)
	}
	got = nil
	Check(res, nil, func(d diag.Diagnostic) { got = append(got, d) })
	if len(got) != 1 || got[0].Location != (diag.Location{}) {
		t.Errorf("Check without loc = %+v, want one diagnostic without a location", got)
	}
}
