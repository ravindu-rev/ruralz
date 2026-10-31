// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Issue labels the gates read: open nightly-failure issues block the next
// release, and a fuzz-crasher issue stays open until its fix lands.
const (
	labelNightly = "nightly-failure"
	labelCrasher = "fuzz-crasher"
)

// skipPrefix starts a skip label, skip-gate:<gate>, and a skip reason
// comment, skip-gate:<gate>: <reason>.
const skipPrefix = "skip-gate:"

// ghLabel is an issue label.
type ghLabel struct {
	Name string `json:"name"`
}

// issue is a GitHub issue; pull requests also list as issues.
type issue struct {
	Number int       `json:"number"`
	Title  string    `json:"title"`
	State  string    `json:"state"`
	Body   string    `json:"body"`
	Labels []ghLabel `json:"labels"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
	PullRequest *struct{} `json:"pull_request"`
}

// hasLabel reports whether the issue carries the label now.
func (i *issue) hasLabel(name string) bool {
	return slices.ContainsFunc(i.Labels, func(l ghLabel) bool { return l.Name == name })
}

// issueEvent is one event of an issue's timeline of label changes.
type issueEvent struct {
	Event string   `json:"event"`
	Label *ghLabel `json:"label"`
	Actor struct {
		Login string `json:"login"`
	} `json:"actor"`
}

// issueComment is one comment of an issue.
type issueComment struct {
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// labeledIssues lists the issues (not pull requests) carrying label in
// state open or closed.
func labeledIssues(ctx context.Context, g *github, label, state string) ([]issue, error) {
	is, err := list[issue](ctx, g, g.repoPath("/issues?state=%s&labels=%s", state, url.QueryEscape(label)))
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(is, func(i issue) bool { return i.PullRequest != nil }), nil
}

// trackingAuthor opens every tracking issue: the release workflow's
// GITHUB_TOKEN acts as this bot, so an issue someone else titled with a
// tag is never taken for its tracking issue.
const trackingAuthor = "github-actions[bot]"

// findTrackingIssue returns the issue titled with the tag that the release
// workflow opened, open or closed, or nil; issues list newest first. The
// listing is filtered here, by title and author, rather than by the API's
// creator parameter, so a bot login the API might not resolve can never
// hide the tracking issue and open a second one.
func findTrackingIssue(ctx context.Context, g *github, title string) (*issue, error) {
	is, err := list[issue](ctx, g, g.repoPath("/issues?state=all&sort=created&direction=desc"))
	if err != nil {
		return nil, err
	}
	for i := range is {
		if is[i].PullRequest == nil && is[i].Title == title && is[i].User.Login == trackingAuthor {
			return &is[i], nil
		}
	}
	return nil, nil
}

// skip is a gate a maintainer skipped: the ruralz.skippedgates.v1 entry.
type skip struct {
	Gate   string `json:"gate"`
	By     string `json:"by"`
	Role   string `json:"role"`
	Reason string `json:"reason"`
}

// roleOf returns a login's repository role (admin, maintain, write, triage,
// read or none), as scripts/ci-approvals.sh reads it.
func roleOf(ctx context.Context, g *github, login string) (string, error) {
	var p struct {
		Permission string `json:"permission"`
		RoleName   string `json:"role_name"`
	}
	err := g.get(ctx, g.repoPath("/collaborators/%s/permission", url.PathEscape(login)), &p)
	switch {
	case errors.Is(err, errNotFound):
		return "none", nil
	case err != nil:
		return "", err
	case p.RoleName != "":
		return p.RoleName, nil
	default:
		return p.Permission, nil
	}
}

// readSkips reads the skips recorded on the tracking issue
// (docs/engineering/03-testing-and-quality-strategy.md, "Release gates"):
// a skip counts only when the issue carries skip-gate:<gate>, its latest
// application was by a maintainer (repository role admin or maintain), and
// that maintainer commented skip-gate:<gate>: <reason> with a reason; the
// latest such comment holds the reason. notes says why a recorded skip
// does not count.
func readSkips(ctx context.Context, g *github, iss *issue) (skips map[string]skip, notes []string, err error) {
	skips = map[string]skip{}
	if iss == nil {
		return skips, nil, nil
	}
	var wanted []string
	for _, gt := range gates() {
		if iss.hasLabel(skipPrefix + gt) {
			wanted = append(wanted, gt)
		}
	}
	if len(wanted) == 0 {
		return skips, nil, nil
	}
	events, err := list[issueEvent](ctx, g, g.repoPath("/issues/%d/events", iss.Number))
	if err != nil {
		return nil, nil, err
	}
	comments, err := list[issueComment](ctx, g, g.repoPath("/issues/%d/comments", iss.Number))
	if err != nil {
		return nil, nil, err
	}
	roles := map[string]string{}
	for _, gt := range wanted {
		label := skipPrefix + gt
		actor := ""
		for _, e := range events {
			if e.Event == "labeled" && e.Label != nil && e.Label.Name == label {
				actor = e.Actor.Login
			}
		}
		if actor == "" {
			notes = append(notes, fmt.Sprintf("%s: no labeled event names who applied %s", gt, label))
			continue
		}
		role, ok := roles[actor]
		if !ok {
			if role, err = roleOf(ctx, g, actor); err != nil {
				return nil, nil, err
			}
			roles[actor] = role
		}
		if role != "admin" && role != "maintain" {
			notes = append(notes, fmt.Sprintf("%s: %s was applied by @%s, whose role %s is not admin or maintain", gt, label, actor, role))
			continue
		}
		reason, found := "", false
		for _, c := range comments {
			if c.User.Login != actor {
				continue
			}
			if r, ok := strings.CutPrefix(strings.TrimSpace(c.Body), label+":"); ok {
				reason, found = strings.TrimSpace(r), true
			}
		}
		switch {
		case !found:
			notes = append(notes, fmt.Sprintf("%s: @%s applied %s but wrote no %s: <reason> comment", gt, actor, label, label))
		case reason == "":
			notes = append(notes, fmt.Sprintf("%s: @%s's %s: comment gives no reason", gt, actor, label))
		default:
			skips[gt] = skip{Gate: gt, By: actor, Role: role, Reason: reason}
		}
	}
	return skips, notes, nil
}

// skipLabelColor is the color of the skip-gate labels.
const skipLabelColor = "FBCA04"

// ensureTrackingIssue returns the tracking issue, opening it when absent,
// and creates the skip-gate labels a maintainer applies.
func ensureTrackingIssue(ctx context.Context, g *github, title, body string) (*issue, error) {
	iss, err := findTrackingIssue(ctx, g, title)
	if err != nil || iss != nil {
		return iss, err
	}
	for _, gt := range gates() {
		lbl := map[string]string{
			"name": skipPrefix + gt, "color": skipLabelColor,
			"description": "A maintainer skips the " + gt + " release gate, with a reason comment",
		}
		// 422 means the label exists already.
		if _, err := g.do(ctx, http.MethodPost, g.repoPath("/labels"), lbl, nil); err != nil && statusCode(err) != http.StatusUnprocessableEntity {
			return nil, err
		}
	}
	var created issue
	if _, err := g.do(ctx, http.MethodPost, g.repoPath("/issues"), map[string]string{"title": title, "body": body}, &created); err != nil {
		return nil, err
	}
	return &created, nil
}

// updateTrackingIssue replaces the tracking issue's body with the latest
// outcome.
func updateTrackingIssue(ctx context.Context, g *github, iss *issue, body string) error {
	_, err := g.do(ctx, http.MethodPatch, g.repoPath("/issues/%d", iss.Number), map[string]string{"body": body}, nil)
	return err
}

// crasherSeed returns the seed file a fuzz-crasher issue body names
// (nightly.yml writes "Commit the input as `<file>`").
func crasherSeed(body string) (string, bool) {
	m := regexp.MustCompile("Commit the input as `([^`]+)`").FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	p := m[1]
	if p == "" || path.IsAbs(p) || strings.Contains(p, `\`) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}
