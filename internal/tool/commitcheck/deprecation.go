// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// deprecatedToken is the footer that announces a deprecation
// (docs/engineering/02-repository-layout-and-conventions.md, "Conventional
// Commits"), one footer per deprecated surface:
//
//	Deprecated: <surface>; removed in <MAJOR>.<MINOR>.0
//
// <surface> names what is deprecated as users meet it, such as a CLI flag,
// a Bundle field, an apiVersion or a metric; MAJOR.MINOR.0 is the first
// release without it. The release notes list each one.
const deprecatedToken = "Deprecated"

// deprecationWindow is the shortest deprecation window of the release
// policy, in minor releases: a deprecation shipped in minor N is removed
// no earlier than minor N+2, as CLI flags and verbs, renamed metrics and
// ruralz/v1alpha1 changes require
// (docs/engineering/04-release-versioning-and-compatibility.md, "Versioned
// surfaces" and "apiVersion lifecycle and deprecation windows"). Longer
// windows and removals reserved to a major release are the reviewers'.
const deprecationWindow = 2

// footerToken matches a footer line: "<token>: <value>", where the token
// is BREAKING CHANGE or a word with hyphens.
func footerToken() *regexp.Regexp {
	return regexp.MustCompile(`^(BREAKING CHANGE|[A-Za-z][A-Za-z0-9-]*): (.*)$`)
}

// footerValues returns the values of the footers named token in a commit
// body. A value continues over the following lines until a blank line or
// the next footer, joined with single spaces.
func footerValues(body, token string) []string {
	var out []string
	var cur *strings.Builder
	flush := func() {
		if cur != nil {
			out = append(out, strings.TrimSpace(cur.String()))
			cur = nil
		}
	}
	re := footerToken()
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		if m := re.FindStringSubmatch(line); m != nil {
			flush()
			if m[1] == token {
				cur = &strings.Builder{}
				cur.WriteString(m[2])
			}
			continue
		}
		if line == "" {
			flush()
			continue
		}
		if cur != nil {
			cur.WriteString(" " + strings.TrimSpace(line))
		}
	}
	flush()
	return out
}

// miscasedDeprecated finds footers whose token is Deprecated in another
// case, which the release notes would not read.
func miscasedDeprecated(body string) []string {
	var out []string
	re := regexp.MustCompile(`(?i)^(deprecated):`)
	for _, line := range strings.Split(body, "\n") {
		if m := re.FindStringSubmatch(line); m != nil && m[1] != deprecatedToken {
			out = append(out, m[1])
		}
	}
	return out
}

// deprecation is a parsed Deprecated footer.
type deprecation struct {
	surface      string
	major, minor int
}

// parseDeprecated parses "<surface>; removed in MAJOR.MINOR.0". The
// surface is not empty and holds no semicolon; the removing release is a
// minor or major one, so its patch is 0.
func parseDeprecated(value string) (deprecation, error) {
	m := regexp.MustCompile(`^([^;]*[^;\s]); removed in (0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.0$`).FindStringSubmatch(value)
	if m == nil || strings.TrimSpace(m[1]) != m[1] {
		return deprecation{}, fmt.Errorf("want %q", "<surface>; removed in MAJOR.MINOR.0")
	}
	major, errMajor := strconv.Atoi(m[2])
	minor, errMinor := strconv.Atoi(m[3])
	if err := errors.Join(errMajor, errMinor); err != nil {
		return deprecation{}, err
	}
	return deprecation{surface: m[1], major: major, minor: minor}, nil
}

// releaseTagFunc returns the newest release tag (vX.Y.Z, never a release
// candidate's) reachable from a commit, or "" before the first release.
type releaseTagFunc func(ctx context.Context, dir, hash string) (string, error)

// gitReleaseTag is the releaseTagFunc that asks git.
func gitReleaseTag(ctx context.Context, dir, hash string) (string, error) {
	//nolint:gosec // The commit comes from git log; arguments are passed without a shell.
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "describe", "--tags", "--abbrev=0", "--match", "v[0-9]*", "--exclude", "v*-*", hash)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := stderr.String()
		if strings.Contains(msg, "No names found") || strings.Contains(msg, "No tags can describe") {
			return "", nil
		}
		return "", fmt.Errorf("git describe %s: %w: %s", hash, err, strings.TrimSpace(msg))
	}
	return strings.TrimSpace(string(out)), nil
}

// shippingMinor is the minor release that ships a commit's deprecation:
// the next minor after its newest release tag, or 0.1 before the first
// release (0.1.0). Deprecations never ship in a patch release.
func shippingMinor(tag string) (major, minor int, err error) {
	if tag == "" {
		return 0, 1, nil
	}
	m := regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).FindStringSubmatch(tag)
	if m == nil {
		return 0, 0, fmt.Errorf("release tag %q is not vMAJOR.MINOR.PATCH", tag)
	}
	major, errMajor := strconv.Atoi(m[1])
	minor, errMinor := strconv.Atoi(m[2])
	return major, minor + 1, errors.Join(errMajor, errMinor)
}

// checkDeprecations validates the Deprecated footers of each commit: the
// token's case, the "<surface>; removed in MAJOR.MINOR.0" form, and a
// removing release at least deprecationWindow minors after the minor that
// ships the commit. It returns one problem per bad footer.
func checkDeprecations(ctx context.Context, dir string, commits []commit, tagOf releaseTagFunc) ([]string, error) {
	var problems []string
	for _, c := range commits {
		short := c.hash
		if len(short) > 12 {
			short = short[:12]
		}
		where := fmt.Sprintf("%s %q", short, c.subject)
		for _, token := range miscasedDeprecated(c.body) {
			problems = append(problems, fmt.Sprintf("%s: footer token %q: write it as %q", where, token, deprecatedToken))
		}
		values := footerValues(c.body, deprecatedToken)
		if len(values) == 0 {
			continue
		}
		tag, err := tagOf(ctx, dir, c.hash)
		if err != nil {
			return nil, err
		}
		shipMajor, shipMinor, err := shippingMinor(tag)
		if err != nil {
			return nil, err
		}
		for _, v := range values {
			d, err := parseDeprecated(v)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: Deprecated footer %q: %v", where, v, err))
				continue
			}
			earliest := shipMinor + deprecationWindow
			if d.major < shipMajor || d.major == shipMajor && d.minor < earliest {
				problems = append(problems, fmt.Sprintf("%s: Deprecated footer %q: it ships in %d.%d.0, so it is removed in %d.%d.0 or later (%d minor releases)",
					where, v, shipMajor, shipMinor, shipMajor, earliest, deprecationWindow))
			}
		}
	}
	return problems, nil
}
