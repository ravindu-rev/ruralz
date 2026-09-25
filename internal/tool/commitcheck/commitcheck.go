// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// commit is what the DCO check reads from one commit.
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
		f := strings.Split(rec, sep)
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

// checkDCO requires a Signed-off-by trailer whose name and email match the
// commit author. Emails compare case-insensitively.
func checkDCO(commits []commit) []string {
	var problems []string
	for _, c := range commits {
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

// componentScopes are the scopes of feat, fix, perf and refactor.
func componentScopes() []string {
	return []string{
		"gateway", "control", "cli", "config", "filter", "plugin", "ai", "statestore",
		"controlstream", "console", "sdk", "api", "deploy",
	}
}

// scopesFor returns the allowed scopes of a type; nil means any scope.
func scopesFor(typ string) (scopes []string, known bool) {
	switch typ {
	case "feat", "fix", "perf", "refactor":
		return componentScopes(), true
	case "test":
		return append(componentScopes(), "e2e", "conformance"), true
	case "build", "ci":
		return []string{"deps", "tools", "workflows"}, true
	case "docs", "chore", "revert":
		return nil, true
	default:
		return nil, false
	}
}

// checkTitle validates "<type>(<scope>)!: <subject>". The scope is optional;
// docs takes a document slug and chore and revert any scope.
func checkTitle(title string) error {
	m := regexp.MustCompile(`^([a-z]+)(?:\(([a-z0-9][a-z0-9-]*)\))?(!)?: (.+)$`).FindStringSubmatch(title)
	if m == nil {
		return errors.New("want <type>(<scope>): <subject>, such as \"fix(statestore): skip calls after the deadline\"")
	}
	typ, scope, subject := m[1], m[2], m[4]
	scopes, known := scopesFor(typ)
	if !known {
		return fmt.Errorf("unknown type %q; use feat, fix, perf, refactor, test, docs, build, ci, chore or revert", typ)
	}
	if scope != "" && scopes != nil && !slices.Contains(scopes, scope) {
		return fmt.Errorf("scope %q is not allowed for %s; use one of %s", scope, typ, strings.Join(scopes, ", "))
	}
	first := []rune(subject)[0]
	if unicode.IsUpper(first) || unicode.IsSpace(first) {
		return errors.New("the subject starts with a lower-case word")
	}
	if strings.HasSuffix(subject, ".") {
		return errors.New("the subject has no trailing period")
	}
	return nil
}
