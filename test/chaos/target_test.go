// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
)

// inventoryEnv returns a getenv naming the committed inventory of
// profile, or none for "".
func inventoryEnv(t *testing.T, profile string) func(string) string {
	t.Helper()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	return func(k string) string {
		if k != harness.EnvProfile || profile == "" {
			return ""
		}
		return filepath.Join(root, "deploy", "benchhost", profile+".json")
	}
}

// TestTargetHosts: the target profile runs on the RH-1 hosts of the
// benchmark harness's inventory, which make chaos-scale names; no
// inventory, or that of another profile, is refused.
func TestTargetHosts(t *testing.T) {
	p, err := TargetHosts(inventoryEnv(t, harness.ProfileRH1))
	if err != nil {
		t.Fatalf("TargetHosts(rh-1.json) = %v", err)
	}
	if p.Name != harness.ProfileRH1 || !p.Runner(harness.RoleLoad).Remote() {
		t.Errorf("TargetHosts(rh-1.json) = %+v, want the RH-1 hosts", p)
	}
	for _, c := range []struct{ profile, want string }{
		{"", harness.EnvProfile + " names no inventory"},
		{harness.ProfileRH3, "not on the rh-3 inventory"},
		{harness.ProfileLoopback, "not on the loopback inventory"},
		{"missing", "the target profile's hosts"},
	} {
		if _, err := TargetHosts(inventoryEnv(t, c.profile)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("TargetHosts(%q) = %v, want an error naming %q", c.profile, err, c.want)
		}
	}
}

// TestPlacementFor: the reduced and nightly profiles run on the harness's
// own host; the target profile places its runs on the RH-1 inventory,
// which it needs, rather than stopping before its first run.
func TestPlacementFor(t *testing.T) {
	var placed []*harness.Profile
	place := func(p *harness.Profile) (*Placement, error) {
		placed = append(placed, p)
		return &Placement{Profile: p}, nil
	}
	for _, name := range []string{Reduced, Nightly} {
		p, err := ParseProfile(name)
		if err != nil {
			t.Fatal(err)
		}
		if pl, err := placementFor(p, inventoryEnv(t, harness.ProfileRH1), place); pl != nil || err != nil {
			t.Errorf("placementFor(%s) = %v, %v; want the harness's own host", name, pl, err)
		}
	}
	if len(placed) != 0 {
		t.Fatalf("placed %d inventories for the reduced and nightly profiles", len(placed))
	}
	target, err := ParseProfile(Target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placementFor(target, inventoryEnv(t, ""), place); err == nil || !strings.Contains(err.Error(), "names no inventory") {
		t.Errorf("placementFor(target) without an inventory = %v, want the missing inventory", err)
	}
	pl, err := placementFor(target, inventoryEnv(t, harness.ProfileRH1), place)
	if err != nil || pl == nil || len(placed) != 1 || placed[0].Name != harness.ProfileRH1 {
		t.Fatalf("placementFor(target) on rh-1.json = %+v, %v (placed %d); want its placement on the RH-1 hosts", pl, err, len(placed))
	}
	failing := func(*harness.Profile) (*Placement, error) { return nil, errors.New("no ssh") }
	if _, err := placementFor(target, inventoryEnv(t, harness.ProfileRH1), failing); err == nil || !strings.Contains(err.Error(), "no ssh") {
		t.Errorf("placementFor with a failing placement = %v", err)
	}
}
