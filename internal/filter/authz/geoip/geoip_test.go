// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package geoip

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// checkText runs Check on cfg and returns the text form of its findings,
// each located at line 4 of policies/geo.yaml through loc.
func checkText(t *testing.T, cfg any) []string {
	t.Helper()
	r := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: "geo"}, Config: cfg}
	var out []string
	loc := func(p diag.Path) diag.Location {
		if p.String() != "spec.config" {
			t.Errorf("loc(%s), want spec.config", p)
		}
		return diag.Location{File: "policies/geo.yaml", Line: 4, Column: 3}
	}
	Check(r, loc, func(d diag.Diagnostic) { out = append(out, string(d.AppendText(nil))) })
	return out
}

// TestCheckBothEmpty: config.allow and config.deny both empty, whether
// absent or written as empty lists, is RZ-CFG-005 at spec.config (the
// configuration model's "checked offline in every binary from M1"); one
// non-empty list is enough.
func TestCheckBothEmpty(t *testing.T) {
	const finding = `policies/geo.yaml:4:3 error RZ-CFG-005 Policy/geo spec.config: config.allow and config.deny are both empty (list the ISO 3166-1 alpha-2 country codes to allow or to deny)`
	tests := []struct {
		name string
		cfg  *v1alpha1.AuthzGeoIPConfig
		want []string
	}{
		{"both absent", &v1alpha1.AuthzGeoIPConfig{}, []string{finding}},
		{"both written empty", &v1alpha1.AuthzGeoIPConfig{Allow: []v1alpha1.CountryCode{}, Deny: []v1alpha1.CountryCode{}}, []string{finding}},
		{"allow empty, deny absent", &v1alpha1.AuthzGeoIPConfig{Allow: []v1alpha1.CountryCode{}}, []string{finding}},
		{"allow only", &v1alpha1.AuthzGeoIPConfig{Allow: []v1alpha1.CountryCode{"DE"}}, nil},
		{"deny only", &v1alpha1.AuthzGeoIPConfig{Deny: []v1alpha1.CountryCode{"KP"}}, nil},
		{"deny beside an empty allow", &v1alpha1.AuthzGeoIPConfig{Allow: []v1alpha1.CountryCode{}, Deny: []v1alpha1.CountryCode{"KP"}}, nil},
		{"both", &v1alpha1.AuthzGeoIPConfig{Allow: []v1alpha1.CountryCode{"DE", "FR"}, Deny: []v1alpha1.CountryCode{"KP"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkText(t, tt.cfg)
			if len(got) != len(tt.want) {
				t.Fatalf("Check = %q, want %q", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Check = %q, want %q", got[i], tt.want[i])
				}
			}
		})
	}
}

// TestCheckHint: the finding tells the author what to write.
func TestCheckHint(t *testing.T) {
	var got []diag.Diagnostic
	Check(&hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: "geo"}, Config: &v1alpha1.AuthzGeoIPConfig{}}, nil,
		func(d diag.Diagnostic) { got = append(got, d) })
	if len(got) != 1 || got[0].Hint == "" || got[0].Location != (diag.Location{}) || got[0].Severity != diag.SeverityError {
		t.Errorf("Check = %+v, want one located-nowhere error with a hint", got)
	}
}

// TestCheckIgnoresOtherResources: no finding for a resource without a
// decoded authz.geoip config, and no call without add.
func TestCheckIgnoresOtherResources(t *testing.T) {
	add := func(d diag.Diagnostic) { t.Errorf("unexpected diagnostic %v", d) }
	Check(nil, nil, add)
	Check(&hub.Resource{}, nil, add)
	Check(&hub.Resource{Config: &v1alpha1.AuthzIPConfig{}}, nil, add)
	Check(&hub.Resource{Config: (*v1alpha1.AuthzGeoIPConfig)(nil)}, nil, add)
	Check(&hub.Resource{Config: &v1alpha1.AuthzGeoIPConfig{}}, nil, nil)
}
