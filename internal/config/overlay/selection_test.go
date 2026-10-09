// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// TestValidName covers the spec.overlay rule of 01 req 21 that keeps the
// selection of 01 req 23 inside overlays/.
func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"prod": true, "staging-eu.1": true, "A_b": true, "0": true,
		"": false, "../x": false, "a/b": false, ".hidden": false, "-x": false, "_x": false, "a b": false, "é": false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestSelection covers 01 req 23: one overlay at most, overlays/<name>/;
// an absent directory is an empty overlay when spec.overlay was defaulted
// and RZ-CFG-009 at spec.overlay when it was set (risk 18).
func TestSelection(t *testing.T) {
	s := Selection{
		Name: "prod", Explicit: true, Environment: "production",
		Location: diag.Location{File: "control/environments.yaml", Line: 7, Column: 14},
	}
	if g := s.Dir(); g != "overlays/prod/" {
		t.Errorf("Dir = %q", g)
	}
	for path, want := range map[string]bool{
		"overlays/prod/a.yaml": true, "overlays/prod/x/b.yml": true,
		"overlays/production/a.yaml": false, "overlays/prod": false, "prod/a.yaml": false,
	} {
		if got := s.Contains(path); got != want {
			t.Errorf("Contains(%q) = %v, want %v", path, got, want)
		}
	}
	if (Selection{Name: ".."}).Contains("overlays/../ruralz.yaml") {
		t.Error("an invalid name selected a path")
	}
	got := s.Absent([]string{"staging", "prd"})
	want := []string{`control/environments.yaml:7:14 error RZ-CFG-009 Environment/production spec.overlay: overlay directory "overlays/prod/" does not exist (did you mean "prd"?)`}
	if g := texts(got); !slices.Equal(g, want) {
		t.Errorf("Absent =\n%q\nwant\n%q", g, want)
	}
	if got := (Selection{Name: "prod", Explicit: true}).Absent(nil); len(got) != 1 || got[0].Resource != nil || got[0].Hint != "" {
		t.Errorf("Absent without Environment = %+v", got)
	}
	if got := (Selection{Name: "staging"}).Absent([]string{"prod"}); got != nil {
		t.Errorf("defaulted overlay: Absent = %q, want none", texts(got))
	}
}
