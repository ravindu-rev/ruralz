// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// commit is what the check reads from one commit.
type commit struct {
	hash, authorName, authorEmail, subject string
	signoffs                               []string
}

// gitCommits lists the non-merge commits of rng, oldest first.
func gitCommits(ctx context.Context, dir, rng string) ([]commit, error) {
	const sep, end = "\x1f", "\x1e"
	format := strings.Join([]string{"%H", "%an", "%ae", "%s", "%(trailers:key=Signed-off-by,valueonly,separator=%x1d)"}, sep) + end
	//nolint:gosec // The range comes from CI and is passed as one argument, never through a shell.
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "log", "--no-merges", "--reverse", "--format="+format, rng)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("git log %s: %s", rng, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var commits []commit
	for _, rec := range strings.Split(string(out), end) {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, sep, 5)
		if len(f) != 5 {
			return nil, fmt.Errorf("unexpected git log record %q", rec)
		}
		c := commit{hash: f[0], authorName: f[1], authorEmail: f[2], subject: f[3]}
		for _, s := range strings.Split(strings.TrimSpace(f[4]), "\x1d") {
			if s = strings.TrimSpace(s); s != "" {
				c.signoffs = append(c.signoffs, s)
			}
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// signoffExempt reports whether author is a bot that cannot add a
// Signed-off-by trailer: Dependabot has no sign-off option (doc 02, "DCO
// sign-off"). Both name and email must match exactly.
func signoffExempt(name, email string) bool {
	return name == "dependabot[bot]" && email == "49699333+dependabot[bot]@users.noreply.github.com"
}

// checkDCO requires a Signed-off-by trailer whose name and email match the
// commit author. Emails compare case-insensitively.
func checkDCO(commits []commit) []string {
	var problems []string
	for _, c := range commits {
		if signoffExempt(c.authorName, c.authorEmail) {
			continue
		}
		want := c.authorName + " <" + c.authorEmail + ">"
		ok := slices.ContainsFunc(c.signoffs, func(s string) bool {
			name, email, found := strings.Cut(s, " <")
			return found && name == c.authorName && strings.EqualFold(strings.TrimSuffix(email, ">"), c.authorEmail)
		})
		if !ok {
			short := c.hash
			if len(short) > 12 {
				short = short[:12]
			}
			problems = append(problems, fmt.Sprintf("%s %q: missing \"Signed-off-by: %s\"", short, c.subject, want))
		}
	}
	return problems
}
