// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"reflect"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the offline checks: the schema's closed config (stage F) and
// Check's RZ-CFG-005 rules (stage H).

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
		ID:         tree.ID{Kind: v1alpha1.KindPolicy, Name: "rl"},
		APIVersion: "ruralz/v1alpha1", Root: docs[0].Root, Start: docs[0].Start,
	}
	out, ok := set.Validate(res, files)
	if !ok {
		t.Fatal("ruralz/v1alpha1 is not served")
	}
	return out
}

// TestSchemaClosedConfig checks the registered config at stage F: the
// fields of the Configuration model pass, an unknown member of config or
// of a limit is refused, responseHeaders takes ietf or none, and cost and
// when are CEL strings.
func TestSchemaClosedConfig(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		refused      bool
	}{
		{"every field", `{"key": "consumer.name", "cost": "2", "responseHeaders": "none", "localOnly": false,
			"limits": [{"requests": 10, "window": "1s", "burst": 5, "perNodeCeiling": 3, "when": "consumer != null"}]}`, false},
		{"unknown config member", `{"limits": [{"requests": 10, "window": "1s"}], "leases": true}`, true},
		{"unknown limit member", `{"limits": [{"requests": 10, "window": "1s", "jitter": "1s"}]}`, true},
		{"responseHeaders value", `{"limits": [{"requests": 10, "window": "1s"}], "responseHeaders": "draft"}`, true},
		{"cost not a string", `{"limits": [{"requests": 10, "window": "1s"}], "cost": 2}`, true},
		{"when not a string", `{"limits": [{"requests": 10, "window": "1s", "when": true}]}`, true},
	} {
		src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "rl"},` +
			`"spec": {"type": "ratelimit", "config": ` + tc.config + `}}`
		ds := schemaDiagnostics(t, src)
		if refused := len(ds) > 0; refused != tc.refused {
			t.Errorf("%s: schema diagnostics %v, want refused %v", tc.name, ds, tc.refused)
		}
	}
}

// TestCheckReportsConfigRules checks each RZ-CFG-005 rule at its path,
// located through loc, and that a valid config, another config type, a
// nil resource or a nil add yield nothing.
func TestCheckReportsConfigRules(t *testing.T) {
	nine := make([]v1alpha1.RateLimit, 9)
	for i := range nine {
		nine[i] = limit(int64(i+1), time.Second)
	}
	withCeil := limit(5, time.Second)
	withCeil.PerNodeCeiling = ptr(int64(0))
	withBurst := limit(5, time.Second)
	withBurst.Burst = ptr(int64(-1))
	for _, tc := range []struct {
		name   string
		limits []v1alpha1.RateLimit
		paths  []string
	}{
		{"valid", []v1alpha1.RateLimit{limit(10, time.Second), limit(5000, time.Minute)}, nil},
		{"nine limits", nine, []string{"spec.config.limits[8]"}},
		{"requests 0", []v1alpha1.RateLimit{limit(0, time.Second)}, []string{"spec.config.limits[0].requests"}},
		{"window below 1ms", []v1alpha1.RateLimit{limit(1, 500*time.Microsecond)}, []string{"spec.config.limits[0].window"}},
		{"window not whole ms", []v1alpha1.RateLimit{limit(1, 1001*time.Microsecond)}, []string{"spec.config.limits[0].window"}},
		{"interval below 1µs", []v1alpha1.RateLimit{limit(1001, time.Millisecond)}, []string{"spec.config.limits[0].window"}},
		{"interval of 1µs", []v1alpha1.RateLimit{limit(1000, time.Millisecond)}, nil},
		{"ceiling below 1", []v1alpha1.RateLimit{withCeil}, []string{"spec.config.limits[0].perNodeCeiling"}},
		{"burst below 0", []v1alpha1.RateLimit{withBurst}, []string{"spec.config.limits[0].burst"}},
		{"equal limits", []v1alpha1.RateLimit{limit(5, time.Second), limit(6, time.Second), limit(5, time.Second)}, []string{"spec.config.limits[2]"}},
		{"none", nil, []string{"spec.config.limits"}},
	} {
		res := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: "rl"}, Config: &v1alpha1.RateLimitConfig{Limits: tc.limits}}
		var got []string
		loc := func(p diag.Path) diag.Location { return diag.Location{File: "policies.yaml", Line: len(p)} }
		Check(res, loc, func(d diag.Diagnostic) {
			if d.Code != CodeConfig || d.Severity != diag.SeverityError || d.Resource == nil || d.Resource.Name != "rl" || d.File != "policies.yaml" || d.Message == "" {
				t.Errorf("%s: diagnostic %+v", tc.name, d)
			}
			got = append(got, d.Path.String())
		})
		if !reflect.DeepEqual(got, tc.paths) {
			t.Errorf("%s: Check reported %v, want %v", tc.name, got, tc.paths)
		}
	}
	n := 0
	count := func(diag.Diagnostic) { n++ }
	Check(&hub.Resource{Config: &v1alpha1.QuotaConfig{}}, nil, count)
	Check(&hub.Resource{Config: (*v1alpha1.RateLimitConfig)(nil)}, nil, count)
	Check(nil, nil, count)
	Check(&hub.Resource{Config: &v1alpha1.RateLimitConfig{}}, nil, nil)
	if n != 0 {
		t.Errorf("Check reported %d diagnostics for foreign input, want 0", n)
	}
}
