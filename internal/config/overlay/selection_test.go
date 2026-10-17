// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"slices"
	"strconv"
	"strings"
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

// TestAbsentHintOrder: the RZ-CFG-009 hint never depends on the order of
// the existing overlay names (01 req 50 identical output; review finding
// on hint order). diag.Nearest compares at most 64 length-compatible
// names, so with "prd" listed after 70 others it would go unseen.
func TestAbsentHintOrder(t *testing.T) {
	var names []string
	for i := range 70 {
		names = append(names, "q"+strconv.Itoa(100+i))
	}
	s := Selection{Name: "prod", Explicit: true}
	want := texts(s.Absent(append([]string{"prd"}, names...)))
	if len(want) != 1 || !strings.Contains(want[0], `did you mean "prd"?`) {
		t.Fatalf("Absent = %q, want the prd hint", want)
	}
	for name, order := range map[string][]string{
		"listed last":       append(slices.Clone(names), "prd"),
		"listed in reverse": reversed(append(slices.Clone(names), "prd")),
		"with duplicates":   append(append(slices.Clone(names), names...), "prd", "prd"),
	} {
		if got := texts(s.Absent(order)); !slices.Equal(got, want) {
			t.Errorf("%s: Absent = %q, want %q", name, got, want)
		}
	}
	if !slices.Equal(names[:2], []string{"q100", "q101"}) {
		t.Errorf("Absent changed the caller's slice: %q", names[:2])
	}
}

func reversed(s []string) []string {
	slices.Reverse(s)
	return s
}
