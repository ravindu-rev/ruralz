// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ip

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// policy returns a decoded authz.ip Policy resource named "office".
func policy(cfg any) *hub.Resource {
	return &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: "office"}, Config: cfg}
}

// TestCheckReportsMalformedEntries checks that the offline check reports
// every entry of either list that is neither a CIDR nor an IP address as
// RZ-CFG-005 at its set item under spec.config, located through loc, so
// validation refuses exactly what Build refuses.
func TestCheckReportsMalformedEntries(t *testing.T) {
	cfg := &v1alpha1.AuthzIPConfig{
		Allow: []string{"10.0.0.0/8", "10.0.0.0/33", "2001:db8::1"},
		Deny:  []string{"fe80::1%eth0", "192.0.2.0/24", "office-net"},
	}
	var got []diag.Diagnostic
	loc := func(p diag.Path) diag.Location { return diag.Location{File: "policies.yaml", Line: len(p)} }
	Check(policy(cfg), loc, func(d diag.Diagnostic) { got = append(got, d) })
	want := []string{
		`spec.config.allow[item=10.0.0.0/33]`,
		`spec.config.deny[item="fe80::1%eth0"]`,
		`spec.config.deny[item=office-net]`,
	}
	if len(got) != len(want) {
		t.Fatalf("Check reported %d diagnostics, want %d: %+v", len(got), len(want), got)
	}
	for i, d := range got {
		if d.Code != CodeConfig || d.Severity != diag.SeverityError || d.Path.String() != want[i] {
			t.Errorf("diagnostic %d = %s %v at %s, want %s error at %s", i, d.Code, d.Severity, d.Path, CodeConfig, want[i])
		}
		if d.Resource == nil || d.Resource.Kind != string(v1alpha1.KindPolicy) || d.Resource.Name != "office" {
			t.Errorf("diagnostic %d resource = %+v, want Policy office", i, d.Resource)
		}
		if d.File != "policies.yaml" || d.Line != 4 {
			t.Errorf("diagnostic %d location = %+v, want the one loc returned", i, d.Location)
		}
	}
	for _, d := range got {
		if _, err := (Factory{}).Build(t.Context(), buildEnvFor(d.Path)); err == nil {
			t.Errorf("Build accepts the entry Check refuses at %s", d.Path)
		}
	}
}

// TestCheckReportsEmptyLists: config.allow and config.deny both empty,
// absent or written as empty lists, which the schema's "one of allow or
// deny" admits, is RZ-CFG-005 at spec.config, exactly the configs Build
// refuses; one non-empty list passes both.
func TestCheckReportsEmptyLists(t *testing.T) {
	tests := []struct {
		name  string
		cfg   *v1alpha1.AuthzIPConfig
		empty bool
	}{
		{"both absent", &v1alpha1.AuthzIPConfig{}, true},
		{"both written empty", &v1alpha1.AuthzIPConfig{Allow: []string{}, Deny: []string{}}, true},
		{"allow written empty", &v1alpha1.AuthzIPConfig{Allow: []string{}}, true},
		{"deny beside an empty allow", &v1alpha1.AuthzIPConfig{Allow: []string{}, Deny: []string{"192.0.2.0/24"}}, false},
		{"allow only", &v1alpha1.AuthzIPConfig{Allow: []string{"10.0.0.0/8"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []diag.Diagnostic
			Check(policy(tt.cfg), nil, func(d diag.Diagnostic) { got = append(got, d) })
			_, err := (Factory{}).Build(t.Context(), filter.BuildEnv{Config: tt.cfg})
			if !tt.empty {
				if len(got) != 0 || err != nil {
					t.Fatalf("Check = %+v, Build error %v, want neither", got, err)
				}
				return
			}
			if len(got) != 1 || got[0].Code != CodeConfig || got[0].Path.String() != "spec.config" ||
				got[0].Message != "config.allow and config.deny are both empty" {
				t.Fatalf("Check = %+v, want one RZ-CFG-005 at spec.config", got)
			}
			if err == nil {
				t.Error("Build accepts the config Check refuses")
			}
		})
	}
}

// buildEnvFor returns a BuildEnv whose config holds only the entry at p.
func buildEnvFor(p diag.Path) filter.BuildEnv {
	entry, _ := p[len(p)-1].Item.(string)
	cfg := &v1alpha1.AuthzIPConfig{Allow: []string{entry}}
	if p[2].Name == fieldDeny {
		cfg = &v1alpha1.AuthzIPConfig{Deny: []string{entry}}
	}
	return filter.BuildEnv{Config: cfg}
}

// TestCheckIgnoresOtherResources checks that a valid config, a resource
// that is not a decoded authz.ip config, a nil resource or a nil add
// yields no diagnostic and no panic.
func TestCheckIgnoresOtherResources(t *testing.T) {
	var n int
	add := func(diag.Diagnostic) { n++ }
	Check(policy(&v1alpha1.AuthzIPConfig{Allow: []string{"10.0.0.0/8"}, Deny: []string{"::1"}}), nil, add)
	Check(policy(&v1alpha1.CORSConfig{AllowOrigins: []string{"nope"}}), nil, add)
	Check(policy((*v1alpha1.AuthzIPConfig)(nil)), nil, add)
	Check(nil, nil, add)
	Check(policy(&v1alpha1.AuthzIPConfig{Allow: []string{"bad"}}), nil, nil)
	if n != 0 {
		t.Errorf("Check reported %d diagnostics, want 0", n)
	}
}
