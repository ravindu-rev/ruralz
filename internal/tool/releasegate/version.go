// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
)

// Tag kinds: release and candidate tags start release.yml; any other tag
// stops it.
const (
	kindRelease   = "release"
	kindCandidate = "candidate"
)

// version is a release tag's version: X.Y.Z, or X.Y.Z-rc.N for a release
// candidate (N from 1).
type version struct {
	Major, Minor, Patch int
	// RC is the candidate number; 0 for a release.
	RC int
}

// parseTag parses vX.Y.Z or vX.Y.Z-rc.N without leading zeros.
func parseTag(tag string) (version, error) {
	m := regexp.MustCompile(`^v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(?:-rc\.([1-9][0-9]{0,8}))?$`).FindStringSubmatch(tag)
	if m == nil {
		return version{}, fmt.Errorf("tag %q is neither vX.Y.Z nor vX.Y.Z-rc.N", tag)
	}
	var v version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		v.RC, _ = strconv.Atoi(m[4])
	}
	return v, nil
}

// kind is release or candidate.
func (v version) kind() string {
	if v.RC > 0 {
		return kindCandidate
	}
	return kindRelease
}

// String is the version without the leading v.
func (v version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.RC > 0 {
		s += fmt.Sprintf("-rc.%d", v.RC)
	}
	return s
}

// tag is the version's tag name.
func (v version) tag() string { return "v" + v.String() }

// base is X.Y.Z without the candidate suffix.
func (v version) base() version { return version{Major: v.Major, Minor: v.Minor, Patch: v.Patch} }

// line is the version's release line, X.Y.
func (v version) line() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// compareBase orders versions by X.Y.Z only.
func compareBase(a, b version) int {
	return cmp.Or(cmp.Compare(a.Major, b.Major), cmp.Compare(a.Minor, b.Minor), cmp.Compare(a.Patch, b.Patch))
}

// needsCandidate reports whether a release must follow a candidate on the
// same commit: vX.Y.0 does, a patch vX.Y.Z (Z above 0) and a candidate do
// not (docs/engineering/04-release-versioning-and-compatibility.md,
// "Release cadence and support windows").
func (v version) needsCandidate() bool { return v.RC == 0 && v.Patch == 0 }

// imageTags are the tags of the version's images: X.Y.Z and X.Y for a
// release, X.Y.Z-rc.N for a candidate, never latest. The first is
// immutable; X.Y moves to the newest patch of its line.
func (v version) imageTags() []string {
	if v.RC > 0 {
		return []string{v.String()}
	}
	return []string{v.String(), v.line()}
}

// releases parses the published release tags, dropping candidates and
// other names.
func releases(published []string) []version {
	var out []version
	for _, t := range published {
		if p, err := parseTag(t); err == nil && p.RC == 0 {
			out = append(out, p)
		}
	}
	return out
}

// previousRelease returns the newest published release below v's X.Y.Z
// among tags, and whether one exists; candidates and other names never
// count.
func previousRelease(v version, published []string) (version, bool) {
	var best version
	found := false
	for _, p := range releases(published) {
		if compareBase(p, v) >= 0 {
			continue
		}
		if !found || compareBase(p, best) > 0 {
			best, found = p, true
		}
	}
	return best, found
}

// isLatest reports whether the release v is above every other published
// release, the only case in which GitHub marks it latest, so an older
// line's patch never becomes latest.
func isLatest(v version, published []string) bool {
	if v.kind() != kindRelease {
		return false
	}
	for _, p := range releases(published) {
		if compareBase(p, v) > 0 {
			return false
		}
	}
	return true
}

// supportedLineCount is how many release lines are supported: N with full
// support, N-1 with bug fixes and N-2 with critical and high security fixes
// until N+3 ships (docs/engineering/04-release-versioning-and-compatibility.md,
// "Release cadence and support windows").
const supportedLineCount = 3

// newestOfLines returns the newest published release of each of the
// supportedLineCount newest lines, newest line first.
func newestOfLines(published []string) []version {
	newest := map[string]version{}
	for _, p := range releases(published) {
		if cur, ok := newest[p.line()]; !ok || compareBase(p, cur) > 0 {
			newest[p.line()] = p
		}
	}
	var out []version
	for _, v := range newest {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b version) int { return compareBase(b, a) })
	if len(out) > supportedLineCount {
		out = out[:supportedLineCount]
	}
	return out
}
