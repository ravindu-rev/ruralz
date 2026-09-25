// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckTitle(t *testing.T) {
	good := []string{
		"feat(gateway): add the admin listener",
		"fix(statestore): skip remaining calls once the request deadline expires",
		"feat(api)!: rename the config field",
		"docs(data-plane): clarify body buffering",
		"build(deps): bump example.com/mod from 1.0.0 to 1.0.1",
		"ci(workflows): add pr-fast",
		"chore: tidy",
		"test(e2e): cover hot reload",
		"refactor: split the loader",
		"revert(anything): undo a change",
	}
	for _, title := range good {
		if err := checkTitle(title); err != nil {
			t.Errorf("checkTitle(%q) = %v", title, err)
		}
	}
	bad := []string{
		"",
		"add a feature",
		"feature(gateway): add",
		"feat(Gateway): add",
		"feat(tools): add",
		"build(gateway): add",
		"feat(gateway): Add the listener",
		"feat(gateway): add the listener.",
		"feat(gateway):add",
		"test(tools): add",
	}
	for _, title := range bad {
		if err := checkTitle(title); err == nil {
			t.Errorf("checkTitle(%q) = nil, want an error", title)
		}
	}
}

func TestCheckDCO(t *testing.T) {
	commits := []commit{
		{hash: "a1", authorName: "Jane Doe", authorEmail: "jane@example.com", subject: "ok", signoffs: []string{"Jane Doe <Jane@Example.com>"}},
		{hash: "b2", authorName: "Jane Doe", authorEmail: "jane@example.com", subject: "missing"},
		{hash: "c3", authorName: "Jane Doe", authorEmail: "jane@example.com", subject: "other person", signoffs: []string{"John Roe <john@example.com>"}},
		{hash: "d4", authorName: "Jane Doe", authorEmail: "jane@example.com", subject: "two", signoffs: []string{"John Roe <john@example.com>", "Jane Doe <jane@example.com>"}},
	}
	problems := checkDCO(commits)
	if len(problems) != 2 || !strings.Contains(problems[0], "b2") || !strings.Contains(problems[1], "c3") {
		t.Fatalf("problems = %q", problems)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // Test runs git with fixed arguments in a temporary repository.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Jane Doe", "GIT_AUTHOR_EMAIL=jane@example.com",
		"GIT_COMMITTER_NAME=Jane Doe", "GIT_COMMITTER_EMAIL=jane@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestGitCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, dir, "commit", "-q", "--allow-empty", "-s", "-m", "feat: signed")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "feat: unsigned\n\nCo-Authored-By: Someone <s@example.com>")
	commits, err := gitCommits(t.Context(), dir, "HEAD~2..HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 || commits[0].subject != "feat: signed" || len(commits[0].signoffs) != 1 {
		t.Fatalf("commits = %+v", commits)
	}
	problems := checkDCO(commits)
	if len(problems) != 1 || !strings.Contains(problems[0], "feat: unsigned") {
		t.Fatalf("problems = %q", problems)
	}
	if code := run(t.Context(), []string{"dco", "-C", dir, "-range", "HEAD~2..HEAD~1"}); code != 0 {
		t.Errorf("dco on signed commit = %d", code)
	}
	if code := run(t.Context(), []string{"dco", "-C", dir, "-range", "HEAD~1..HEAD"}); code != 1 {
		t.Errorf("dco on unsigned commit = %d", code)
	}
	if code := run(t.Context(), []string{"dco", "-C", filepath.Join(dir, "missing"), "-range", "HEAD~1..HEAD"}); code != 2 {
		t.Errorf("dco on missing repo = %d", code)
	}
}

func TestRunUsage(t *testing.T) {
	if run(t.Context(), nil) != 2 || run(t.Context(), []string{"nope"}) != 2 || run(t.Context(), []string{"dco"}) != 2 {
		t.Error("usage errors must exit 2")
	}
	if run(t.Context(), []string{"title", "-title", "feat(cli): add version"}) != 0 {
		t.Error("valid title rejected")
	}
	if run(t.Context(), []string{"title", "-title", "Add version"}) != 1 {
		t.Error("invalid title accepted")
	}
	t.Setenv("PR_TITLE", "")
	if run(t.Context(), []string{"title"}) != 2 {
		t.Error("missing title must exit 2")
	}
	t.Setenv("PR_TITLE", "docs(roadmap-and-milestones): mark M0 done")
	if run(t.Context(), []string{"title"}) != 0 {
		t.Error("PR_TITLE not read")
	}
}
