// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseReleaseTag: a release tag is vX.Y.Z without leading zeros;
// anything else, a pre-release, build metadata or a Go PDK's tag
// included, is no release tag.
func TestParseReleaseTag(t *testing.T) {
	good := map[string]releaseVersion{
		"v0.1.0":   {0, 1, 0},
		"v1.12.3":  {1, 12, 3},
		"v10.0.20": {10, 0, 20},
	}
	for tag, want := range good {
		v, ok := parseReleaseTag(tag)
		if !ok || v != want || v.tag() != tag || "v"+v.String() != tag {
			t.Errorf("parseReleaseTag(%q) = %+v, %t; want %+v", tag, v, ok, want)
		}
	}
	for _, tag := range []string{"0.1.0", "v0.1", "v01.1.0", "v0.01.0", "v0.1.0-rc.1", "v0.1.0+meta", "refs/tags/v0.1.0", "sdk/go/v0.1.0", "v1234567890.0.0", ""} {
		if v, ok := parseReleaseTag(tag); ok {
			t.Errorf("parseReleaseTag(%q) = %+v, want no release tag", tag, v)
		}
	}
	if got := (releaseVersion{1, 12, 3}).line(); got != "1.12" {
		t.Errorf("line = %q, want 1.12", got)
	}
}

// TestCommitChange: each rule of the version computation for one commit.
func TestCommitChange(t *testing.T) {
	cases := []struct {
		name, subject, body string
		want                change
	}{
		{"feat", "feat: add the version command", "", changeMinor},
		{"feat with a scope", "feat(cli): add the version command", "", changeMinor},
		{"fix", "fix(gateway): keep the Host header", "", changePatch},
		{"perf", "perf(gateway): cut an allocation", "", changePatch},
		{"breaking feat", "feat(config)!: drop the old field", "", changeBreaking},
		{"breaking fix", "fix(cli)!: exit 1 on findings", "", changeBreaking},
		{"breaking title of a type that releases nothing alone", "chore!: drop Go 1.25", "", changeBreaking},
		{"BREAKING CHANGE footer", "refactor(cli): split the output", "Text.\n\nBREAKING CHANGE: the CLI exit code 4 is gone\n", changeBreaking},
		{"BREAKING-CHANGE footer", "docs(cli-and-api-surface): rename the flag", "BREAKING-CHANGE: --old is now --new\n", changeBreaking},
		{"BREAKING CHANGE footer on a non-Conventional title", "Rename the flag", "BREAKING CHANGE: --old is now --new\n", changeBreaking},
		{"Dependabot's build(deps) of a Go module", "build(deps): bump golang.org/x/net from 0.61.0 to 0.62.0", "Bumps golang.org/x/net.\n\n---\nupdated-dependencies:\n- dependency-name: golang.org/x/net\n", changePatch},
		{"Dependabot's build(deps) of the image's base", "build(deps): bump golang from 1.27.2-bookworm to 1.27.3-bookworm in /deploy/container", "", changePatch},
		{"a toolchain update by hand", "build(deps): move the toolchain to go1.27.4", "", changePatch},
		{"breaking dependency update", "build(deps)!: require Go 1.27", "", changeBreaking},
		{"docs", "docs(data-plane): explain the Drain", "", changeNone},
		{"test", "test(gateway): cover the Drain", "", changeNone},
		{"ci", "ci(workflows): pin the runner", "", changeNone},
		{"build of another scope", "build(tools): add releasekit", "", changeNone},
		{"build without a scope", "build: tidy the Makefile", "", changeNone},
		{"Dependabot's ci(deps) of actions", "ci(deps): bump the actions group with 2 updates", "", changeNone},
		{"chore", "chore: tidy the agent instructions", "", changeNone},
		{"refactor", "refactor(gateway): move the dialer", "", changeNone},
		{"revert of a feature", "revert: feat(cli): add the version command", "This reverts commit 0123456.\n", changeNone},
		{"GitHub's revert title", `Revert "feat(cli): add the version command"`, "", changeNone},
		{"no Conventional title", "Update README.md", "", changeNone},
		{"upper-case type", "Feat: add the version command", "", changeNone},
		{"no space after the colon", "feat:add the version command", "", changeNone},
		{"plural footer token", "docs(data-plane): explain", "BREAKING CHANGES: not the token\n", changeNone},
		{"empty BREAKING CHANGE footer", "docs(data-plane): explain", "BREAKING CHANGE: \n", changeNone},
		{"Deprecated footer", "docs(cli-and-api-surface): deprecate --flat", "Deprecated: --flat; removed in 0.4.0\n", changeNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := commitChange(commit{Hash: fixtureCommit, Subject: c.subject, Body: c.body}); got != c.want {
				t.Errorf("commitChange(%q) = %v, want %v", c.subject, got, c.want)
			}
		})
	}
}

// TestBump: fix, perf and build(deps) move the patch, feat the minor, and
// a breaking change the minor while the major is 0 and the major from
// 1.0.0.
func TestBump(t *testing.T) {
	cases := []struct {
		name string
		from releaseVersion
		c    change
		want releaseVersion
	}{
		{"patch", releaseVersion{0, 1, 0}, changePatch, releaseVersion{0, 1, 1}},
		{"minor resets the patch", releaseVersion{0, 1, 3}, changeMinor, releaseVersion{0, 2, 0}},
		{"minor past nine", releaseVersion{0, 9, 4}, changeMinor, releaseVersion{0, 10, 0}},
		{"breaking while the major is 0", releaseVersion{0, 1, 3}, changeBreaking, releaseVersion{0, 2, 0}},
		{"patch from 1.0.0", releaseVersion{1, 2, 3}, changePatch, releaseVersion{1, 2, 4}},
		{"minor from 1.0.0", releaseVersion{1, 2, 3}, changeMinor, releaseVersion{1, 3, 0}},
		{"breaking from 1.0.0", releaseVersion{1, 2, 3}, changeBreaking, releaseVersion{2, 0, 0}},
		{"none", releaseVersion{1, 2, 3}, changeNone, releaseVersion{1, 2, 3}},
	}
	for _, c := range cases {
		if got := c.from.bump(c.c); got != c.want {
			t.Errorf("%s: %v.bump(%v) = %v, want %v", c.name, c.from, c.c, got, c.want)
		}
	}
}

// TestPlanVersion: without a Release-As footer, the version the commits
// since the previous release make: the highest change wins, the first
// release is 0.1.0 whatever its change, and commits that release nothing
// give no release.
func TestPlanVersion(t *testing.T) {
	c := func(hash, subject, body string) commit { return commit{Hash: hash, Subject: subject, Body: body} }
	feat := c("f1", "feat(cli): add the version command", "")
	fix := c("x1", "fix(gateway): keep the Host header", "")
	perf := c("p1", "perf(gateway): cut an allocation", "")
	docs := c("d1", "docs(data-plane): explain the Drain", "")
	deps := c("b1", "build(deps): bump golang.org/x/net from 0.61.0 to 0.62.0", "")
	actions := c("a1", "ci(deps): bump the actions group with 2 updates", "")
	bang := c("k1", "feat(config)!: drop the old field", "")
	footer := c("k2", "refactor(cli): split the output", "BREAKING CHANGE: exit code 4 is gone\n")
	v := func(major, minor, patch int) *releaseVersion { return &releaseVersion{major, minor, patch} }
	cases := []struct {
		name    string
		prev    *releaseVersion
		commits []commit
		want    string // empty: no release
		top     change
		first   string
	}{
		{"first release, a feature", nil, []commit{docs, feat}, "0.1.0", changeMinor, "f1"},
		{"first release, a fix", nil, []commit{fix}, "0.1.0", changePatch, "x1"},
		{"first release, a dependency update", nil, []commit{docs, deps}, "0.1.0", changePatch, "b1"},
		{"first release, a breaking change", nil, []commit{bang, fix}, "0.1.0", changeBreaking, "k1"},
		{"first release, nothing releases", nil, []commit{docs, actions}, "", changeNone, ""},
		{"no commits", v(0, 1, 0), nil, "", changeNone, ""},
		{"Dependabot's build(deps) releases a patch", v(0, 1, 0), []commit{deps, deps}, "0.1.1", changePatch, "b1"},
		{"Dependabot's ci(deps) releases nothing", v(0, 1, 0), []commit{actions}, "", changeNone, ""},
		{"only types that release nothing", v(0, 1, 0), []commit{
			docs, c("t1", "test: cover it", ""), c("c1", "ci(workflows): pin", ""), c("h1", "chore: tidy", ""),
			c("r1", "refactor: move it", ""), c("v1", "revert: feat: add it", ""), c("u1", "build(tools): pin syft", ""),
		}, "", changeNone, ""},
		{"fix and perf", v(0, 1, 0), []commit{fix, perf}, "0.1.1", changePatch, "x1"},
		{"a feature wins over fixes and dependency updates", v(0, 1, 4), []commit{fix, deps, feat, perf}, "0.2.0", changeMinor, "f1"},
		{"the oldest commit of the highest change", v(0, 1, 4), []commit{fix, feat, c("f2", "feat: second", "")}, "0.2.0", changeMinor, "f1"},
		{"breaking title while the major is 0", v(0, 3, 2), []commit{fix, bang}, "0.4.0", changeBreaking, "k1"},
		{"breaking footer while the major is 0", v(0, 3, 2), []commit{footer, feat}, "0.4.0", changeBreaking, "k2"},
		{"breaking change from 1.0.0", v(1, 4, 2), []commit{feat, footer}, "2.0.0", changeBreaking, "k2"},
		{"feature from 1.0.0", v(1, 4, 2), []commit{feat, deps}, "1.5.0", changeMinor, "f1"},
		{"fix from 1.0.0", v(1, 4, 2), []commit{docs, fix}, "1.4.3", changePatch, "x1"},
		{"dependency update from 1.0.0", v(1, 4, 2), []commit{actions, deps}, "1.4.3", changePatch, "b1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := planVersion(tc.prev, tc.commits)
			got := ""
			if p.ok {
				got = p.next.String()
			}
			if got != tc.want || p.top != tc.top || p.first.Hash != tc.first || p.footer != nil || len(p.refused) != 0 {
				t.Errorf("planVersion = %q, %v, first %q, footer %v, refused %q; want %q, %v, first %q, no footer",
					got, p.top, p.first.Hash, p.footer, p.refused, tc.want, tc.top, tc.first)
			}
		})
	}
}

// TestPlanVersionReleaseAs: a Release-As: X.Y.Z footer above the version
// the commits make raises the next version to X.Y.Z, alone or beside
// releasing commits, the highest of several counting; one that is no
// version X.Y.Z, or not above the previous release, is refused unless a
// later commit's valid footer replaces it.
func TestPlanVersionReleaseAs(t *testing.T) {
	c := func(hash, subject, body string) commit { return commit{Hash: hash, Subject: subject, Body: body} }
	as := func(hash, version string) commit {
		return c(hash, "chore: release "+version, "Cut the release.\n\nRelease-As: "+version+"\nSigned-off-by: A <a@example.com>\n")
	}
	feat := c("f1", "feat(cli): add the version command", "")
	fix := c("x1", "fix(gateway): keep the Host header", "")
	bang := c("k1", "feat(config)!: drop the old field", "")
	v := func(major, minor, patch int) *releaseVersion { return &releaseVersion{major, minor, patch} }
	cases := []struct {
		name    string
		prev    *releaseVersion
		commits []commit
		want    string // empty: no release
		footer  string // the commit of the honored footer
		refused []string
	}{
		{"footer alone", v(0, 4, 2), []commit{as("r1", "1.0.0")}, "1.0.0", "r1", nil},
		{"footer plus feat", v(0, 4, 2), []commit{feat, as("r1", "1.0.0")}, "1.0.0", "r1", nil},
		{"footer plus a breaking change", v(0, 4, 2), []commit{bang, as("r1", "1.0.0")}, "1.0.0", "r1", nil},
		{"footer on a releasing commit", v(0, 4, 2), []commit{c("r1", "feat(cli): stable output", "Release-As: 1.0.0\n")}, "1.0.0", "r1", nil},
		{"footer token in another case", v(0, 4, 2), []commit{c("r1", "chore: release", "release-as: 1.0.0\n")}, "1.0.0", "r1", nil},
		{"two footers, the higher first", v(0, 4, 2), []commit{as("r1", "2.0.0"), as("r2", "1.0.0")}, "2.0.0", "r1", nil},
		{"two footers, the higher last", v(0, 4, 2), []commit{as("r1", "1.0.0"), fix, as("r2", "1.1.0")}, "1.1.0", "r2", nil},
		{"two footers in one commit", v(0, 4, 2), []commit{c("r1", "chore: release", "Release-As: 1.0.0\nRelease-As: 1.2.0\n")}, "1.2.0", "r1", nil},
		{"first release with a footer", nil, []commit{feat, as("r1", "1.0.0")}, "1.0.0", "r1", nil},
		{"first release, a footer below 0.1.0", nil, []commit{as("r1", "0.0.1")}, "0.0.1", "r1", nil},
		{
			"equal to the previous release", v(1, 0, 0),
			[]commit{fix, as("r1", "1.0.0")},
			"", "",
			[]string{`r1 "chore: release 1.0.0": Release-As 1.0.0 is not above the previous release v1.0.0`},
		},
		{
			"lower than the previous release", v(1, 0, 0),
			[]commit{as("r1", "0.9.0")},
			"", "",
			[]string{`r1 "chore: release 0.9.0": Release-As 0.9.0 is not above the previous release v1.0.0`},
		},
		{
			"malformed: no patch", v(0, 4, 2),
			[]commit{as("r1", "1.0")},
			"", "",
			[]string{`r1 "chore: release 1.0": its Release-As footer "1.0" is no version X.Y.Z`},
		},
		{"malformed: a leading v", v(0, 4, 2), []commit{as("r1", "v1.0.0")}, "", "", []string{`its Release-As footer "v1.0.0" is no version X.Y.Z`}},
		{"malformed: a pre-release", v(0, 4, 2), []commit{as("r1", "1.0.0-rc.1")}, "", "", []string{`its Release-As footer "1.0.0-rc.1" is no version X.Y.Z`}},
		{"malformed: a leading zero", v(0, 4, 2), []commit{as("r1", "1.00.0")}, "", "", []string{`its Release-As footer "1.00.0" is no version X.Y.Z`}},
		{
			"malformed: text after the version", v(0, 4, 2),
			[]commit{c("r1", "chore: release", "Release-As: 1.0.0\nfor the stable API\n")},
			"", "",
			[]string{`its Release-As footer "1.0.0 for the stable API" is no version X.Y.Z`},
		},
		{"a refused footer replaced by a later valid one", v(0, 4, 2), []commit{as("r1", "1.0"), fix, as("r2", "1.0.0")}, "1.0.0", "r2", nil},
		{"a lower footer replaced by a later valid one", v(1, 0, 0), []commit{as("r1", "0.9.0"), as("r2", "1.1.0")}, "1.1.0", "r2", nil},
		{
			"a refused footer after the valid one", v(0, 4, 2),
			[]commit{as("r1", "1.0.0"), as("r2", "0.1.0")},
			"", "",
			[]string{`r2 "chore: release 0.1.0": Release-As 0.1.0 is not above the previous release v0.4.2`},
		},
		{
			"every refused footer after the last valid one", v(0, 4, 2),
			[]commit{as("r1", "0.1"), as("r2", "x")},
			"", "",
			[]string{`r1 "chore: release 0.1": its Release-As footer "0.1"`, `r2 "chore: release x": its Release-As footer "x"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := planVersion(tc.prev, tc.commits)
			if len(tc.refused) > 0 {
				if len(p.refused) != len(tc.refused) {
					t.Fatalf("refused %q, want %d findings like %q", p.refused, len(tc.refused), tc.refused)
				}
				for i, want := range tc.refused {
					if !strings.Contains(p.refused[i], want) || !strings.Contains(p.refused[i], "a later commit's valid Release-As footer replaces it") {
						t.Errorf("refused[%d] = %q, want it to contain %q", i, p.refused[i], want)
					}
				}
				return
			}
			got, footer := "", ""
			if p.ok {
				got = p.next.String()
			}
			if p.footer != nil {
				footer = p.footer.commit.Hash
			}
			if got != tc.want || footer != tc.footer || len(p.refused) != 0 {
				t.Errorf("planVersion = %q by the footer of %q, refused %q; want %q by the footer of %q", got, footer, p.refused, tc.want, tc.footer)
			}
			if want := "the Release-As footer of " + tc.footer; !strings.Contains(p.reason("v0.4.2"), want) {
				t.Errorf("reason %q lacks %q", p.reason("v0.4.2"), want)
			}
		})
	}
}

// TestPlanVersionReleaseAsNeverLowers: a Release-As footer at or below the
// version the commits' changes make sets nothing, so a feature never ships
// as a patch and a breaking change never as less than its bump; the reason
// names the footer the commits outgrew.
func TestPlanVersionReleaseAsNeverLowers(t *testing.T) {
	c := func(hash, subject, body string) commit { return commit{Hash: hash, Subject: subject, Body: body} }
	as := func(hash, version string) commit {
		return c(hash, "chore: release "+version, "Release-As: "+version+"\n")
	}
	feat := c("f1", "feat(cli): add the version command", "")
	fix := c("x1", "fix(gateway): keep the Host header", "")
	bang := c("k1", "feat(config)!: drop the old field", "")
	v := func(major, minor, patch int) *releaseVersion { return &releaseVersion{major, minor, patch} }
	cases := []struct {
		name     string
		prev     *releaseVersion
		commits  []commit
		want     string
		outgrown string
		value    string
	}{
		{"a feature above a patch footer", v(0, 4, 2), []commit{as("r1", "0.4.3"), feat}, "0.5.0", "r1", "0.4.3"},
		{"a fix equal to the footer", v(0, 4, 2), []commit{fix, as("r1", "0.4.3")}, "0.4.3", "r1", "0.4.3"},
		{"a breaking change from 1.0.0 above a minor footer", v(1, 4, 2), []commit{bang, as("r1", "1.5.0")}, "2.0.0", "r1", "1.5.0"},
		{"the first release above a lower footer", nil, []commit{feat, as("r1", "0.0.1")}, "0.1.0", "r1", "0.0.1"},
		{"the highest of two footers below", v(0, 4, 2), []commit{as("r1", "0.4.4"), as("r2", "0.4.3"), feat}, "0.5.0", "r1", "0.4.4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := planVersion(tc.prev, tc.commits)
			got, outgrown := "", ""
			if p.ok {
				got = p.next.String()
			}
			if p.outgrown != nil {
				outgrown = p.outgrown.commit.Hash
			}
			if got != tc.want || p.footer != nil || outgrown != tc.outgrown || len(p.refused) != 0 {
				t.Fatalf("planVersion = %q, footer %v, outgrown %q, refused %q; want %q, no footer, outgrown %q",
					got, p.footer, outgrown, p.refused, tc.want, tc.outgrown)
			}
			want := "; the Release-As " + tc.value + " footer of " + tc.outgrown + " "
			if r := p.reason("v0.4.2"); !strings.Contains(r, want) || !strings.HasPrefix(r, "release "+tc.want+": a ") {
				t.Errorf("reason %q, want the change's release naming %q", r, want)
			}
		})
	}
}

// repo is a scratch Git repository on branch main.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "--quiet", "--initial-branch=main")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	return gitIn(r.t, r.dir, args...)
}

// commit adds an empty commit with message and returns it.
func (r *repo) commit(message string) string {
	r.t.Helper()
	r.git("commit", "--quiet", "--allow-empty", "-m", message)
	return r.git("rev-parse", "HEAD")
}

// next runs the version decision for ref.
func (r *repo) next(ref string) (nextResult, error) {
	r.t.Helper()
	return runNext(r.t.Context(), nextOptions{root: r.dir, to: ref}, runGit)
}

// wantNext checks the decision for ref.
func (r *repo) wantNext(name, ref, version, previous string, tagged bool, reason string) {
	r.t.Helper()
	got, err := r.next(ref)
	if err != nil {
		r.t.Fatalf("%s: %v", name, err)
	}
	tag := ""
	if version != "" {
		tag = "v" + version
	}
	if got.Version != version || got.Tag != tag || got.Previous != previous || got.Tagged != tagged || !strings.Contains(got.Reason, reason) {
		r.t.Errorf("%s: %+v; want version %q, previous %q, tagged %t and a reason containing %q", name, got, version, previous, tagged, reason)
	}
}

// TestRunNextOnRepository drives the version decision through a history
// of pushes to main: nothing before a releasing commit, 0.1.0 for the
// first release without a tag, a resume on a commit its tag names, nothing
// for Dependabot's ci(deps) or a merge commit's title, a patch for its
// build(deps), the patch, minor and, from 1.0.0, major rules against the
// previous release tag, which annotated and lightweight tags give alike
// and other tags never.
func TestRunNextOnRepository(t *testing.T) {
	const dependabot = "dependabot[bot] <49699333+dependabot[bot]@users.noreply.github.com>"
	const nothing = "nothing to release: no feat, fix, perf, build(deps), breaking change or Release-As footer since "
	r := newRepo(t)
	r.commit("docs: start the design")
	r.wantNext("no releasing commit", "HEAD", "", "", false, nothing+"the first commit")

	first := r.commit("feat(cli): add the version command")
	r.git("tag", "sdk/go/v0.1.0")
	r.git("tag", "-a", "-m", "not a release", "v0.0.9-rc.1")
	r.wantNext("first release", "HEAD", "0.1.0", "", false, `release 0.1.0: a feature change since the first commit, first in `+short(first)+` "feat(cli): add the version command"`)

	r.git("tag", "-a", "-m", "Ruralz 0.1.0", "v0.1.0")
	r.wantNext("a re-run after the tag job", "HEAD", "0.1.0", "", true, "v0.1.0 names "+short(first)+" already: its release resumes")

	r.git("commit", "--quiet", "--allow-empty", "--author", dependabot, "-m", "ci(deps): bump the actions group with 2 updates")
	r.wantNext("Dependabot's action updates", "HEAD", "", "v0.1.0", false, nothing+"v0.1.0")

	r.git("commit", "--quiet", "--allow-empty", "--author", dependabot,
		"-m", "build(deps): bump golang.org/x/net from 0.61.0 to 0.62.0\n\nBumps golang.org/x/net.")
	deps := r.git("rev-parse", "HEAD")
	r.wantNext("Dependabot's module update", "HEAD", "0.1.1", "v0.1.0", false,
		`release 0.1.1: a fix, performance or dependency change since v0.1.0, first in `+short(deps)+` "build(deps): bump golang.org/x/net from 0.61.0 to 0.62.0"`)

	r.commit("fix(gateway): keep the Host header")
	r.wantNext("a fix", "HEAD", "0.1.1", "v0.1.0", false, "release 0.1.1: a fix, performance or dependency change since v0.1.0")
	r.git("tag", "v0.1.1") // lightweight

	r.git("checkout", "--quiet", "-b", "topic")
	r.commit("docs(data-plane): explain the Drain")
	r.git("checkout", "--quiet", "main")
	r.git("merge", "--no-ff", "--quiet", "-m", "feat(cli): the merge commit's title", "topic")
	r.wantNext("a merge commit", "HEAD", "", "v0.1.1", false, nothing+"v0.1.1")

	r.commit("feat(config)!: drop the old field")
	r.wantNext("breaking while the major is 0", "HEAD", "0.2.0", "v0.1.1", false, "release 0.2.0: a breaking change since v0.1.1")

	r.git("tag", "-a", "-m", "Ruralz 1.0.0", "v1.0.0")
	r.commit("feat(cli): add the plan command")
	r.wantNext("feature from 1.0.0", "HEAD", "1.1.0", "v1.0.0", false, "release 1.1.0")
	r.commit("refactor(cli): split the output\n\nBREAKING CHANGE: exit code 4 is gone")
	r.wantNext("breaking footer from 1.0.0", "HEAD", "2.0.0", "v1.0.0", false, "release 2.0.0: a breaking change since v1.0.0")
}

// TestRunNextReleaseAsOnRepository: a Release-As footer of a non-merge
// commit cuts the version it names, such as 1.0.0, a merge commit's footer
// counts for nothing, and a footer not above the previous release stops
// the run (exit 1) until a later commit's valid footer replaces it.
func TestRunNextReleaseAsOnRepository(t *testing.T) {
	r := newRepo(t)
	r.commit("feat(cli): add the version command")
	r.git("tag", "-a", "-m", "Ruralz 0.1.0", "v0.1.0")

	r.git("checkout", "--quiet", "-b", "topic")
	r.commit("docs(data-plane): explain the Drain")
	r.git("checkout", "--quiet", "main")
	r.git("merge", "--no-ff", "--quiet", "-m", "Merge pull request #7 from ravindu-rev/topic", "-m", "Release-As: 9.0.0", "topic")
	r.wantNext("a merge commit's footer", "HEAD", "", "v0.1.0", false, "nothing to release")

	r.commit("feat(cli): add the plan command")
	cut := r.commit("chore: cut 1.0.0\n\nThe CLI and the Bundle schema are stable.\n\nRelease-As: 1.0.0")
	r.wantNext("Release-As", "HEAD", "1.0.0", "v0.1.0", false, `release 1.0.0: the Release-As footer of `+short(cut)+` "chore: cut 1.0.0" sets it`)
	r.git("tag", "-a", "-m", "Ruralz 1.0.0", "v1.0.0")
	r.wantNext("a re-run of the 1.0.0 run", "HEAD", "1.0.0", "v0.1.0", true, "its release resumes")

	low := r.commit("chore: release 0.9.0\n\nRelease-As: 0.9.0")
	_, err := r.next("HEAD")
	var fe *findingsError
	if !errors.As(err, &fe) || !hasFinding(fe.findings, short(low)+` "chore: release 0.9.0": Release-As 0.9.0 is not above the previous release v1.0.0`) {
		t.Fatalf("a footer below the previous release: %v, want a finding", err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"next", "-C", r.dir}, &stdout, &stderr, testEnv(nil, nil)); code != 1 || !strings.Contains(stderr.String(), "is not above the previous release v1.0.0") {
		t.Errorf("next on a refused footer: exit %d, stderr %q; want 1", code, stderr.String())
	}

	r.commit("chore: release 1.1.0\n\nRelease-As: 1.1.0")
	r.wantNext("a later valid footer", "HEAD", "1.1.0", "v1.0.0", false, "the Release-As footer of")
}

// TestRunNextReleasedLater: a commit that a later release contains, such as
// a run re-run after a newer push released its commits, releases nothing;
// its own tag still resumes its release.
func TestRunNextReleasedLater(t *testing.T) {
	r := newRepo(t)
	a := r.commit("feat(cli): add the version command")
	b := r.commit("fix(gateway): keep the Host header")
	r.git("tag", "-a", "-m", "Ruralz 0.1.0", "v0.1.0", b)
	r.wantNext("an earlier push", a, "", "", false, "nothing to release: v0.1.0, of a later commit, contains "+short(a))

	// A run that tagged a before failing resumes even after a later
	// release.
	r.git("tag", "-d", "v0.1.0")
	r.git("tag", "-a", "-m", "Ruralz 0.1.0", "v0.1.0", a)
	r.git("tag", "-a", "-m", "Ruralz 0.1.1", "v0.1.1", b)
	r.wantNext("resume under a later release", a, "0.1.0", "", true, "its release resumes")
}

// TestRunNextFindings: tags that contradict the decision are findings, so a
// release tag is never created twice or for another version.
func TestRunNextFindings(t *testing.T) {
	r := newRepo(t)
	base := r.commit("feat(cli): add the version command")
	r.git("tag", "v0.1.0")
	r.git("checkout", "--quiet", "-b", "side")
	side := r.commit("fix: a commit main never got")
	r.git("tag", "-a", "-m", "elsewhere", "v0.1.1", side)
	r.git("checkout", "--quiet", "main")
	head := r.commit("fix(gateway): keep the Host header")

	var fe *findingsError
	_, err := r.next("HEAD")
	if !errors.As(err, &fe) || !hasFinding(fe.findings, "v0.1.1 is due for "+short(head)+", but it names "+short(side)+", which neither contains it nor follows it") {
		t.Errorf("the version's tag on another line: %v, want a finding", err)
	}

	r.git("tag", "-a", "-m", "wrong", "v0.5.0", head)
	if _, err := r.next("HEAD"); !errors.As(err, &fe) || !hasFinding(fe.findings, "v0.5.0 names "+short(head)+", but the commits since v0.1.0 make v0.1.1") {
		t.Errorf("a tag of another version: %v, want a finding", err)
	}
	r.git("tag", "v0.1.2", head)
	if _, err := r.next("HEAD"); !errors.As(err, &fe) || !hasFinding(fe.findings, "carries the release tags v0.1.2, v0.5.0") {
		t.Errorf("two release tags on one commit: %v, want a finding", err)
	}

	docs := r.commit("docs(data-plane): explain the Drain")
	r.git("tag", "v0.6.0", docs)
	if _, err := r.next(docs); !errors.As(err, &fe) || !hasFinding(fe.findings, "but the commits since v0.5.0 make no release") {
		t.Errorf("a release tag on a commit that releases nothing: %v, want a finding", err)
	}

	r.git("tag", "v0.7.0", r.git("rev-parse", base+"^{tree}"))
	if _, err := r.next(base); err == nil || errors.As(err, &fe) || !strings.Contains(err.Error(), "release tag v0.7.0 names no commit") {
		t.Errorf("a release tag of a tree: %v, want an error", err)
	}
}

// TestRunNextErrors: a shallow clone, which hides commits and tags, a bad
// ref and failing git are errors.
func TestRunNextErrors(t *testing.T) {
	shallow := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) > 1 && args[1] == "--is-shallow-repository":
			return []byte("true\n"), nil
		case len(args) > 0 && args[0] == "rev-parse":
			return []byte(fixtureCommit + "\n"), nil
		}
		return nil, errors.New("unexpected git call")
	}
	if _, err := runNext(t.Context(), nextOptions{root: ".", to: "HEAD"}, shallow); !errors.Is(err, errShallow) {
		t.Errorf("a shallow clone: %v, want %v", err, errShallow)
	}
	if _, err := runNext(t.Context(), nextOptions{root: ".", to: "--all"}, shallow); err == nil {
		t.Error("an option as the ref: no error")
	}
	failing := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("git failed") }
	if _, err := runNext(t.Context(), nextOptions{root: ".", to: "HEAD"}, failing); err == nil {
		t.Error("failing git: no error")
	}
}

// TestRunNextCommand: next prints the decision and appends the step
// outputs, or prints them without -outputs; tags that contradict it exit 1
// and a directory that is no repository exits 2.
func TestRunNextCommand(t *testing.T) {
	r := newRepo(t)
	r.commit("feat(cli): add the version command")
	env := testEnv(nil, nil)
	outputs := filepath.Join(t.TempDir(), "github_output")
	writeFile(t, outputs, "earlier=step\n")
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"next", "-C", r.dir, "-outputs", outputs}, &stdout, &stderr, env); code != 0 || !strings.Contains(stdout.String(), "releasekit: release 0.1.0") {
		t.Fatalf("next: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if got, want := string(readBack(t, outputs)), "earlier=step\nversion=0.1.0\ntag=v0.1.0\nprevious=\ntagged=false\n"; got != want {
		t.Errorf("outputs %q, want %q", got, want)
	}

	r.git("tag", "v0.1.0")
	r.commit("docs(data-plane): explain the Drain")
	stdout.Reset()
	if code := run(t.Context(), []string{"next", "-C", r.dir}, &stdout, &stderr, env); code != 0 ||
		!strings.HasSuffix(stdout.String(), "\nversion=\ntag=\nprevious=v0.1.0\ntagged=false\n") || !strings.Contains(stdout.String(), "nothing to release") {
		t.Errorf("next without -outputs: exit %d, stdout %q", code, stdout.String())
	}

	r.git("tag", "v0.2.0")
	stderr.Reset()
	if code := run(t.Context(), []string{"next", "-C", r.dir}, &stdout, &stderr, env); code != 1 || !strings.Contains(stderr.String(), "releasekit next: 1 finding(s)") {
		t.Errorf("next on a contradicting tag: exit %d, stderr %q; want 1", code, stderr.String())
	}
	stderr.Reset()
	if code := run(t.Context(), []string{"next", "-C", filepath.Join(t.TempDir(), "absent")}, &stdout, &stderr, env); code != 2 || !strings.Contains(stderr.String(), "releasekit next:") {
		t.Errorf("next outside a repository: exit %d, stderr %q; want 2", code, stderr.String())
	}
}
