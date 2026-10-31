// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestNewestRelease: the highest vMAJOR.MINOR.PATCH by number, never a
// release candidate's or another name.
func TestNewestRelease(t *testing.T) {
	for _, tc := range []struct {
		tags []string
		want string
	}{
		{nil, ""},
		{[]string{"", "v1", "v1.2", "release-0.1", "v0.1.0-rc.1", "v01.0.0", "v1.0.0.0", "1.0.0"}, ""},
		{[]string{"v0.1.0"}, "v0.1.0"},
		{[]string{"v0.9.0", "v0.10.0", "v0.2.5"}, "v0.10.0"},
		{[]string{"v1.0.0", "v0.12.3", "v2.0.0-rc.1"}, "v1.0.0"},
		{[]string{"v0.2.1", "v0.2.10", "v0.2.9 "}, "v0.2.10"},
	} {
		if got := newestRelease(tc.tags); got != tc.want {
			t.Errorf("newestRelease(%q) = %q, want %q", tc.tags, got, tc.want)
		}
	}
}

// TestSplitRange: BASE..HEAD and BASE...HEAD, both revisions required.
func TestSplitRange(t *testing.T) {
	for rng, want := range map[string][2]string{
		"origin/main..HEAD": {"origin/main", "HEAD"},
		"a1b2...c3d4":       {"a1b2", "c3d4"},
		"HEAD~2..HEAD~1":    {"HEAD~2", "HEAD~1"},
	} {
		base, head, err := splitRange(rng)
		if err != nil || base != want[0] || head != want[1] {
			t.Errorf("splitRange(%q) = %q, %q, %v; want %q", rng, base, head, err, want)
		}
	}
	for _, rng := range []string{"", "HEAD", "..HEAD", "main..", "a..b..c", "..."} {
		if _, _, err := splitRange(rng); err == nil {
			t.Errorf("splitRange(%q) = nil error", rng)
		}
	}
}

// TestCheckGolden: modifying, deleting or retyping a held file is a
// finding; adding a file and changing a file no release holds are not.
func TestCheckGolden(t *testing.T) {
	held := map[string]bool{
		corpusDir + "/golden/shop-bundle/level-0/expected.json": true,
		corpusDir + "/golden/shop-bundle/bundle/ruralz.yaml":    true,
		corpusDir + "/negative/RZ-CFG-005-port/expected.txt":    true,
	}
	changes := []corpusChange{
		{'M', corpusDir + "/golden/shop-bundle/level-0/expected.json"},
		{'D', corpusDir + "/golden/shop-bundle/bundle/ruralz.yaml"},
		{'T', corpusDir + "/negative/RZ-CFG-005-port/expected.txt"},
		{'A', corpusDir + "/golden/shop-bundle/level-1/expected.json"},
		{'M', corpusDir + "/golden/shop-bundle-1/level-0/expected.json"},
		{'D', corpusDir + "/negative/RZ-CFG-006-new/expected.txt"},
	}
	want := []string{
		corpusDir + "/golden/shop-bundle/level-0/expected.json: the range changes it, but release v0.1.0 holds it",
		corpusDir + "/golden/shop-bundle/bundle/ruralz.yaml: the range deletes it, but release v0.1.0 holds it",
		corpusDir + "/negative/RZ-CFG-005-port/expected.txt: the range changes the type of it, but release v0.1.0 holds it",
	}
	if got := checkGolden("v0.1.0", held, changes); !slices.Equal(got, want) {
		t.Errorf("checkGolden =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := checkGolden("", map[string]bool{}, changes); len(got) != 0 {
		t.Errorf("before the first release: %q, want no finding", got)
	}
}

// writeRepoFile writes a file of the test repository.
func writeRepoFile(t *testing.T, dir, name, data string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRunGolden runs the subcommand on a repository: before the first
// release nothing is held; after it a range that adds files or changes
// files outside the release passes, one that changes, deletes or renames a
// held file fails, a release candidate holds nothing, the newest release
// counts even when main cannot reach it, and usage or git errors exit 2.
func TestRunGolden(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	entry := corpusDir + "/golden/shop-bundle/"
	git(t, dir, "init", "-q", "-b", "main")
	writeRepoFile(t, dir, entry+"bundle/ruralz.yaml", "kind: Gateway\n")
	writeRepoFile(t, dir, entry+"level-0/expected.json", "{}\n")
	writeRepoFile(t, dir, "README.md", "readme\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "feat: corpus")

	// branch commits the change on a new branch from main and returns the
	// range of the pull request it would be.
	branch := func(name string, change func()) string {
		t.Helper()
		git(t, dir, "checkout", "-q", "-b", name, "main")
		change()
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-q", "--allow-empty", "-m", "test: "+name)
		git(t, dir, "checkout", "-q", "main")
		return "main.." + name
	}
	modify := branch("modify", func() { writeRepoFile(t, dir, entry+"level-0/expected.json", "{\"level\": 0}\n") })
	if code := run(t.Context(), []string{"golden", "-C", dir, "-range", modify}); code != 0 {
		t.Errorf("before the first release: exit %d, want 0", code)
	}

	git(t, dir, "tag", "v0.1.0")
	cases := map[string]int{
		modify: 1,
		branch("delete", func() {
			if err := os.Remove(filepath.Join(dir, filepath.FromSlash(entry+"bundle/ruralz.yaml"))); err != nil {
				t.Fatal(err)
			}
		}): 1,
		branch("rename", func() { git(t, dir, "mv", entry+"bundle/ruralz.yaml", entry+"bundle/gateway.yaml") }): 1,
		branch("add", func() {
			writeRepoFile(t, dir, entry+"level-1/expected.json", "{}\n")
			writeRepoFile(t, dir, corpusDir+"/golden/shop-bundle-1/bundle/ruralz.yaml", "kind: Gateway\n")
		}): 0,
		branch("outside", func() { writeRepoFile(t, dir, "README.md", "changed\n") }): 0,
	}
	for _, rng := range slices.Sorted(maps.Keys(cases)) {
		if code := run(t.Context(), []string{"golden", "-C", dir, "-range", rng}); code != cases[rng] {
			t.Errorf("range %s: exit %d, want %d", rng, code, cases[rng])
		}
	}

	// A file added after v0.1.0 is free until a release holds it; a
	// candidate's tag holds nothing.
	branch("added", func() { writeRepoFile(t, dir, corpusDir+"/golden/quickstart/bundle/ruralz.yaml", "kind: Gateway\n") })
	git(t, dir, "merge", "-q", "--ff-only", "added")
	reworked := branch("rework", func() { writeRepoFile(t, dir, corpusDir+"/golden/quickstart/bundle/ruralz.yaml", "kind: Route\n") })
	git(t, dir, "tag", "v0.2.0-rc.1", "added")
	if code := run(t.Context(), []string{"golden", "-C", dir, "-range", reworked}); code != 0 {
		t.Errorf("a file only a release candidate holds: exit %d, want 0", code)
	}
	// A patch release tagged on another branch is the newest release even
	// though main cannot reach it.
	git(t, dir, "checkout", "-q", "-b", "release-0.2", "added")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "fix: patch")
	git(t, dir, "tag", "v0.2.1")
	git(t, dir, "checkout", "-q", "main")
	if code := run(t.Context(), []string{"golden", "-C", dir, "-range", reworked}); code != 1 {
		t.Errorf("a file the newest release holds on another branch: exit %d, want 1", code)
	}

	for name, args := range map[string][]string{
		"missing -range":     {"golden", "-C", dir},
		"malformed range":    {"golden", "-C", dir, "-range", "main"},
		"unknown revision":   {"golden", "-C", dir, "-range", "main..no-such-branch"},
		"missing repository": {"golden", "-C", filepath.Join(dir, "missing"), "-range", "main..add"},
	} {
		if code := run(t.Context(), args); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}
