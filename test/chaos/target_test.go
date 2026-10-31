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

// TestCheckProfile: the reduced and nightly profiles run on the harness's
// own host; the target profile needs its RH-1 inventory and then stops at
// ErrTargetPlacement instead of gating target results on a placement it
// does not have.
func TestCheckProfile(t *testing.T) {
	for _, name := range []string{Reduced, Nightly} {
		p, err := ParseProfile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkProfile(p, inventoryEnv(t, "")); err != nil {
			t.Errorf("checkProfile(%s) = %v, want nil", name, err)
		}
	}
	target, err := ParseProfile(Target)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkProfile(target, inventoryEnv(t, "")); err == nil || errors.Is(err, ErrTargetPlacement) {
		t.Errorf("checkProfile(target) without an inventory = %v, want the missing inventory", err)
	}
	if err := checkProfile(target, inventoryEnv(t, harness.ProfileRH1)); !errors.Is(err, ErrTargetPlacement) {
		t.Errorf("checkProfile(target) on rh-1.json = %v, want ErrTargetPlacement", err)
	}
}
