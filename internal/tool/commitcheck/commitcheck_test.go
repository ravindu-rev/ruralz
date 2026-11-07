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

func TestCheckDCO(t *testing.T) {
	commits := []commit{
		{hash: "a1", authorName: "Jane Doe", authorEmail: "jane@example.com", subject: "ok", signoffs: []string{"Jane Doe <Jane@Example.com>"}},
		{hash: "b2", authorName: "Jane Doe", authorEmail: "jane@example.com", subject: "missing"},
		{hash: "c3", authorName: "Jane Doe", authorEmail: "jane@example.com", subject: "other person", signoffs: []string{"John Roe <john@example.com>"}},
		{hash: "d4", authorName: "Jane Doe", authorEmail: "jane@example.com", subject: "two", signoffs: []string{"John Roe <john@example.com>", "Jane Doe <jane@example.com>"}},
		{hash: "e5", authorName: "dependabot[bot]", authorEmail: "49699333+dependabot[bot]@users.noreply.github.com", subject: "dependabot"},
		{hash: "f6", authorName: "dependabot[bot]", authorEmail: "jane@example.com", subject: "bot name only"},
		{hash: "g7", authorName: "Jane Doe", authorEmail: "49699333+dependabot[bot]@users.noreply.github.com", subject: "bot email only"},
		{hash: "h8", authorName: "dependabot[bot]", authorEmail: "49699333+Dependabot[bot]@users.noreply.github.com", subject: "bot email case"},
	}
	problems := checkDCO(commits)
	want := []string{"b2", "c3", "f6", "g7", "h8"}
	if len(problems) != len(want) {
		t.Fatalf("problems = %q, want one each for %q", problems, want)
	}
	for i, hash := range want {
		if !strings.Contains(problems[i], hash) {
			t.Errorf("problems[%d] = %q, want commit %s", i, problems[i], hash)
		}
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
	// A trailer other than Signed-off-by is no sign-off.
	if commits[1].subject != "feat: unsigned" || len(commits[1].signoffs) != 0 {
		t.Errorf("commits[1] = %+v, want the unsigned commit without sign-offs", commits[1])
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

// TestDCORangeSkipsMergesAndDependabot checks a range as the PR pipeline
// passes it: GitHub's merge commit carries no sign-off and is skipped, and
// so is a Dependabot commit, which cannot sign off; the signed commits
// pass.
func TestDCORangeSkipsMergesAndDependabot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "build(deps): Bump x from 1.0.0 to 1.0.1",
		"--author", "dependabot[bot] <49699333+dependabot[bot]@users.noreply.github.com>")
	git(t, dir, "checkout", "-q", "-b", "side", "HEAD~1")
	git(t, dir, "commit", "-q", "--allow-empty", "-s", "-m", "fix: side change")
	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "Merge branch 'side'", "side")
	commits, err := gitCommits(t.Context(), dir, "HEAD~2..HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 {
		t.Fatalf("commits = %+v, want the Dependabot and side commits without the merge", commits)
	}
	if code := run(t.Context(), []string{"dco", "-C", dir, "-range", "HEAD~2..HEAD"}); code != 0 {
		t.Errorf("dco on a signed commit, a Dependabot commit and a merge = %d, want 0", code)
	}
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "fix: unsigned")
	if code := run(t.Context(), []string{"dco", "-C", dir, "-range", "HEAD~3..HEAD"}); code != 1 {
		t.Errorf("dco on a range holding an unsigned commit = %d, want 1", code)
	}
}

// TestRunUsage checks the usage errors, the commit checks other than the
// DCO sign-off among them: commitcheck has no title, deprecations or
// golden command.
func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"nope"},
		{"title", "-range", "HEAD~1..HEAD"},
		{"title", "-title", "feat(cli): add version"},
		{"deprecations", "-range", "HEAD~1..HEAD"},
		{"golden", "-range", "HEAD~1..HEAD"},
		{"dco"},
		{"dco", "-range"},
		{"dco", "-unknown", "x", "-range", "HEAD~1..HEAD"},
	} {
		if code := run(t.Context(), args); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
	}
}
