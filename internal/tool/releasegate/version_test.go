// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"testing"
)

// TestParseTag: release tags are vX.Y.Z or vX.Y.Z-rc.N without leading
// zeros; anything else stops the release run.
func TestParseTag(t *testing.T) {
	good := map[string]version{
		"v0.1.0":      {0, 1, 0, 0},
		"v0.1.0-rc.1": {0, 1, 0, 1},
		"v1.12.3":     {1, 12, 3, 0},
		"v2.0.0-rc.10": {
			2, 0, 0, 10,
		},
	}
	for tag, want := range good {
		v, err := parseTag(tag)
		if err != nil || v != want || v.tag() != tag {
			t.Errorf("parseTag(%q) = %+v, %v; want %+v", tag, v, err, want)
		}
	}
	for _, tag := range []string{"0.1.0", "v0.1", "v01.1.0", "v0.1.0-rc.0", "v0.1.0-beta.1", "v0.1.0-rc.1.2", "v0.1.0+meta", "v0.1.0-rc1", "refs/tags/v0.1.0", ""} {
		if _, err := parseTag(tag); err == nil {
			t.Errorf("parseTag(%q) accepted a tag that is no release tag", tag)
		}
	}
}

// TestVersionRules: kinds, the candidate rule (vX.Y.0 only), the
// unskippable Benchmarks gate of 0.1.0 and its candidates, and the image
// tags (X.Y.Z and X.Y, or X.Y.Z-rc.N, never latest).
func TestVersionRules(t *testing.T) {
	cases := []struct {
		tag            string
		kind           string
		needsCandidate bool
		benchFixed     bool
		tags           []string
	}{
		{"v0.1.0", kindRelease, true, true, []string{"0.1.0", "0.1"}},
		{"v0.1.0-rc.2", kindCandidate, false, true, []string{"0.1.0-rc.2"}},
		{"v0.1.1", kindRelease, false, false, []string{"0.1.1", "0.1"}},
		{"v0.2.0", kindRelease, true, false, []string{"0.2.0", "0.2"}},
		{"v1.0.0-rc.1", kindCandidate, false, false, []string{"1.0.0-rc.1"}},
	}
	for _, c := range cases {
		v, err := parseTag(c.tag)
		if err != nil {
			t.Fatal(err)
		}
		if v.kind() != c.kind || v.needsCandidate() != c.needsCandidate || v.benchmarksUnskippable() != c.benchFixed || !slices.Equal(v.imageTags(), c.tags) {
			t.Errorf("%s: kind %s, needsCandidate %t, benchmarksUnskippable %t, image tags %q; want %s, %t, %t, %q",
				c.tag, v.kind(), v.needsCandidate(), v.benchmarksUnskippable(), v.imageTags(), c.kind, c.needsCandidate, c.benchFixed, c.tags)
		}
	}
}

// TestPreviousRelease: the newest published release below the X.Y.Z,
// never a candidate or another name.
func TestPreviousRelease(t *testing.T) {
	published := []string{"v0.1.0", "v0.1.1", "v0.2.0-rc.1", "v0.2.0", "v0.1.2", "nightly", "v0.3.0-rc.1"}
	cases := []struct {
		tag, want string
	}{
		{"v0.1.0-rc.1", ""},
		{"v0.1.0", ""},
		{"v0.1.3", "v0.1.2"},
		{"v0.2.0", "v0.1.2"},
		{"v0.2.0-rc.3", "v0.1.2"},
		{"v0.3.0", "v0.2.0"},
		{"v1.0.0", "v0.2.0"},
	}
	for _, c := range cases {
		v, _ := parseTag(c.tag)
		p, ok := previousRelease(v, published)
		got := ""
		if ok {
			got = p.tag()
		}
		if got != c.want {
			t.Errorf("previousRelease(%s) = %q, want %q", c.tag, got, c.want)
		}
	}
}

// TestIsLatest: a release is latest only above every published release, so
// an older line's patch never becomes latest; a candidate never is.
func TestIsLatest(t *testing.T) {
	published := []string{"v0.1.0", "v0.2.0", "v0.3.0-rc.1"}
	cases := map[string]bool{
		"v0.2.1":      true,
		"v0.3.0":      true,
		"v0.2.0":      true, // a re-run of its own publish
		"v0.1.1":      false,
		"v0.3.0-rc.2": false,
	}
	for tag, want := range cases {
		v, _ := parseTag(tag)
		if got := isLatest(v, published); got != want {
			t.Errorf("isLatest(%s) = %t, want %t", tag, got, want)
		}
	}
	v, _ := parseTag("v0.1.0")
	if !isLatest(v, nil) {
		t.Error("the first release is not latest")
	}
}

// TestNewestOfLines: the newest release of each of the three newest lines,
// N, N-1 and N-2; older lines are unsupported and candidates never count.
func TestNewestOfLines(t *testing.T) {
	published := []string{"v0.1.0", "v0.1.4", "v0.2.0", "v0.2.1", "v0.3.0", "v1.0.0", "v1.0.1", "v1.1.0-rc.1", "v0.10.0"}
	var got []string
	for _, v := range newestOfLines(published) {
		got = append(got, v.tag())
	}
	if want := []string{"v1.0.1", "v0.10.0", "v0.3.0"}; !slices.Equal(got, want) {
		t.Errorf("newestOfLines = %q, want %q", got, want)
	}
	if got := newestOfLines([]string{"v0.1.0-rc.1"}); len(got) != 0 {
		t.Errorf("newestOfLines of a candidate only = %v, want none", got)
	}
}
