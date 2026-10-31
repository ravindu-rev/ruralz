// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestFooterValues: a footer value continues over the next lines until a
// blank line or another footer; other tokens are skipped.
func TestFooterValues(t *testing.T) {
	body := "Body text.\n\n" +
		"Deprecated: the --flat flag of ruralz bundle render;\n" +
		"removed in 0.4.0\n" +
		"Refs: OQ-cli-and-api-surface-6\n" +
		"Deprecated: Route.spec.legacy; removed in 0.5.0\n" +
		"Signed-off-by: Jane Doe <jane@example.com>\n\n" +
		"Deprecated: after a blank line; removed in 0.6.0\r\n"
	got := footerValues(body, deprecatedToken)
	want := []string{
		"the --flat flag of ruralz bundle render; removed in 0.4.0",
		"Route.spec.legacy; removed in 0.5.0",
		"after a blank line; removed in 0.6.0",
	}
	if !slices.Equal(got, want) {
		t.Errorf("footerValues = %q, want %q", got, want)
	}
	if got := footerValues("Not a footer line\nDeprecated:missing space\n", deprecatedToken); len(got) != 0 {
		t.Errorf("non-footers matched: %q", got)
	}
}

func TestParseDeprecated(t *testing.T) {
	good := map[string]deprecation{
		"the --flat flag of ruralz bundle render; removed in 0.4.0": {"the --flat flag of ruralz bundle render", 0, 4},
		"ruralz_gateway_old_total; removed in 1.0.0":                {"ruralz_gateway_old_total", 1, 0},
		"x; removed in 12.34.0":                                     {"x", 12, 34},
	}
	for v, want := range good {
		got, err := parseDeprecated(v)
		if err != nil || got != want {
			t.Errorf("parseDeprecated(%q) = %+v, %v; want %+v", v, got, err, want)
		}
	}
	for _, v := range []string{
		"", "--flat", "--flat removed in 0.4.0", "--flat; removed in 0.4", "--flat; removed in 0.4.1",
		"--flat; removed in v0.4.0", "--flat; removed in 0.04.0", "; removed in 0.4.0", " --flat; removed in 0.4.0",
		"a; b; removed in 0.4.0", "--flat;removed in 0.4.0", "--flat; removed in 0.4.0.",
	} {
		if _, err := parseDeprecated(v); err == nil {
			t.Errorf("parseDeprecated(%q) = nil error", v)
		}
	}
}

func TestShippingMinor(t *testing.T) {
	cases := map[string][2]int{"": {0, 1}, "v0.1.0": {0, 2}, "v0.2.3": {0, 3}, "v1.9.0": {1, 10}}
	for tag, want := range cases {
		major, minor, err := shippingMinor(tag)
		if err != nil || major != want[0] || minor != want[1] {
			t.Errorf("shippingMinor(%q) = %d.%d, %v; want %d.%d", tag, major, minor, err, want[0], want[1])
		}
	}
	if _, _, err := shippingMinor("v1.0"); err == nil {
		t.Error("a malformed tag is accepted")
	}
}

// TestCheckDeprecations: the form, the token's case and the removal window
// of two minor releases after the minor that ships the commit.
func TestCheckDeprecations(t *testing.T) {
	tags := map[string]string{"a1": "", "b2": "v0.2.1", "c3": "v0.2.1", "d4": "v1.4.0", "e5": "", "f6": "", "g7": "v0.1.0"}
	tagOf := func(_ context.Context, _, hash string) (string, error) { return tags[hash], nil }
	commits := []commit{
		{hash: "a1", subject: "feat(cli): deprecate --flat", body: "Deprecated: --flat; removed in 0.3.0\n"},
		{hash: "b2", subject: "feat(cli): too early", body: "Deprecated: --flat; removed in 0.4.0\n"},
		{hash: "c3", subject: "feat(cli): on time", body: "Deprecated: --flat; removed in 0.5.0\nDeprecated: --tree; removed in 1.0.0\n"},
		{hash: "d4", subject: "feat(config): older major", body: "Deprecated: Route.spec.x; removed in 0.9.0\n"},
		{hash: "e5", subject: "feat(cli): malformed", body: "Deprecated: --flat\n"},
		{hash: "f6", subject: "feat(cli): lower case", body: "deprecated: --flat; removed in 0.3.0\n"},
		{hash: "g7", subject: "fix(cli): no footer", body: "Refs: OQ-cli-and-api-surface-6\n"},
	}
	problems, err := checkDeprecations(t.Context(), ".", commits, tagOf)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`b2 "feat(cli): too early": Deprecated footer "--flat; removed in 0.4.0": it ships in 0.3.0, so it is removed in 0.5.0 or later`,
		`d4 "feat(config): older major": Deprecated footer "Route.spec.x; removed in 0.9.0": it ships in 1.5.0, so it is removed in 1.7.0 or later`,
		`e5 "feat(cli): malformed": Deprecated footer "--flat": want "<surface>; removed in MAJOR.MINOR.0"`,
		`f6 "feat(cli): lower case": footer token "deprecated": write it as "Deprecated"`,
	}
	if len(problems) != len(want) {
		t.Fatalf("problems = %q\nwant %q", problems, want)
	}
	for i, w := range want {
		if !strings.HasPrefix(problems[i], w) {
			t.Errorf("problems[%d] = %q\nwant prefix %q", i, problems[i], w)
		}
	}
	failing := func(context.Context, string, string) (string, error) { return "", errors.New("describe failed") }
	if _, err := checkDeprecations(t.Context(), ".", commits[:1], failing); err == nil {
		t.Error("a failing tag lookup is not an error")
	}
}

// TestGitReleaseTag: the newest vX.Y.Z tag reachable from a commit, never
// a release candidate's, or none before the first release.
func TestGitReleaseTag(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
	if tag, err := gitReleaseTag(t.Context(), dir, "HEAD"); err != nil || tag != "" {
		t.Errorf("no tags: %q, %v", tag, err)
	}
	git(t, dir, "tag", "v0.1.0")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "feat: next")
	git(t, dir, "tag", "v0.2.0-rc.1")
	if tag, err := gitReleaseTag(t.Context(), dir, "HEAD"); err != nil || tag != "v0.1.0" {
		t.Errorf("after a candidate: %q, %v; want v0.1.0", tag, err)
	}
	git(t, dir, "tag", "v0.2.0")
	if tag, err := gitReleaseTag(t.Context(), dir, "HEAD"); err != nil || tag != "v0.2.0" {
		t.Errorf("at a release: %q, %v; want v0.2.0", tag, err)
	}
	if _, err := gitReleaseTag(t.Context(), dir, "no-such-commit"); err == nil {
		t.Error("an unknown commit is not an error")
	}
}

// TestRunDeprecations runs the subcommand on a repository: a footer within
// the window passes, one too early fails, and usage or git errors exit 2.
func TestRunDeprecations(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "feat: base")
	git(t, dir, "tag", "v0.1.0")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "feat(cli): deprecate --flat\n\nDeprecated: --flat of ruralz bundle render; removed in 0.4.0")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "feat(cli): deprecate --tree\n\nDeprecated: --tree of ruralz bundle render; removed in 0.3.0")
	if code := run(t.Context(), []string{"deprecations", "-C", dir, "-range", "HEAD~2..HEAD~1"}); code != 0 {
		t.Errorf("footer within the window: exit %d, want 0", code)
	}
	if code := run(t.Context(), []string{"deprecations", "-C", dir, "-range", "HEAD~1..HEAD"}); code != 1 {
		t.Errorf("footer before the window ends: exit %d, want 1", code)
	}
	if code := run(t.Context(), []string{"deprecations", "-C", dir}); code != 2 {
		t.Errorf("missing -range: exit %d, want 2", code)
	}
	if code := run(t.Context(), []string{"deprecations", "-C", filepath.Join(dir, "missing"), "-range", "HEAD~1..HEAD"}); code != 2 {
		t.Errorf("missing repository: exit %d, want 2", code)
	}
}
