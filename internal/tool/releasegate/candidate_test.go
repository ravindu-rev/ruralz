// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"
)

// TestCandidateFindings: vX.Y.0 needs a vX.Y.0-rc.N tag on the same commit
// whose release run passed and whose pre-release was published at least
// 14 days earlier; a patch and a candidate need none.
func TestCandidateFindings(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	setup := func(t *testing.T) *fakeGitHub {
		t.Helper()
		f := newFakeGitHub(t)
		blob, _ := sshKeyPair(1)
		sig := sshSignature(blob, "git")
		// rc.1 on another commit; rc.2 on the release commit, published 15
		// days ago with a passed run; an unrelated tag of another version.
		f.addTag("v0.2.0-rc.1", commitB, sig, true)
		f.addTag("v0.2.0-rc.2", commitA, sig, true)
		f.addTag("v0.2.1-rc.1", commitA, sig, true)
		f.addRelease("v0.2.0-rc.1", true, now.Add(-30*24*time.Hour))
		f.addRelease("v0.2.0-rc.2", true, now.Add(-15*24*time.Hour))
		f.runs = []workflowRun{
			{HeadBranch: "v0.2.0-rc.1", HeadSHA: commitB, Status: "completed", Conclusion: "success"},
			{HeadBranch: "v0.2.0-rc.2", HeadSHA: commitA, Status: "completed", Conclusion: "failure"},
			{HeadBranch: "v0.2.0-rc.2", HeadSHA: commitA, Status: "completed", Conclusion: "success"},
		}
		return f
	}
	check := func(t *testing.T, f *fakeGitHub, tag, commit string, at time.Time) (bool, []string) {
		t.Helper()
		v, _ := parseTag(tag)
		applies, findings, err := candidateFindings(t.Context(), f.client(t), v, commit, at)
		if err != nil {
			t.Fatal(err)
		}
		return applies, findings
	}

	f := setup(t)
	if applies, got := check(t, f, "v0.2.0", commitA, now); !applies || len(got) != 0 {
		t.Errorf("a candidate on the commit 15 days after publishing: applies %t, findings %q", applies, got)
	}
	for _, tag := range []string{"v0.2.1", "v0.3.0-rc.1"} {
		if applies, got := check(t, f, tag, commitA, now); applies || got != nil {
			t.Errorf("%s needs no candidate: applies %t, findings %q", tag, applies, got)
		}
	}

	cases := []struct {
		name   string
		change func(f *fakeGitHub)
		tag    string
		commit string
		at     time.Time
		want   string
	}{
		{"window", nil, "v0.2.0", commitA, now.Add(-2 * 24 * time.Hour), "a candidate stays published at least 14 days"},
		{"no candidate on the commit", nil, "v0.2.0", commitC, now, "no v0.2.0-rc.N tag names commit " + commitC},
		{"no passed run", func(f *fakeGitHub) { f.runs = f.runs[:2] }, "v0.2.0", commitA, now, "v0.2.0-rc.2: no passed release.yml run"},
		{"not published", func(f *fakeGitHub) { f.releases = f.releases[:1] }, "v0.2.0", commitA, now, "v0.2.0-rc.2: not published"},
		{"published as a release", func(f *fakeGitHub) { f.releases[1].Prerelease = false }, "v0.2.0", commitA, now, "not published as a pre-release"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			if c.change != nil {
				c.change(f)
			}
			applies, got := check(t, f, c.tag, c.commit, c.at)
			if !applies || !strings.Contains(strings.Join(got, "\n"), c.want) {
				t.Errorf("applies %t, findings %q; want %q", applies, got, c.want)
			}
		})
	}
}
