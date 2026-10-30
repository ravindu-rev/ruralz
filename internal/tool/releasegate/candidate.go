// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// candidateWindow is how long a candidate stays published before its
// vX.Y.0 (target) (docs/engineering/04-release-versioning-and-compatibility.md,
// "Release cadence and support windows").
const candidateWindow = 14 * 24 * time.Hour

// releaseWorkflow is the workflow file of release runs.
const releaseWorkflow = "release.yml"

// workflowRun is one run of a workflow.
type workflowRun struct {
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// runsPage is one page of a workflow's runs.
type runsPage struct {
	Runs []workflowRun `json:"workflow_runs"`
}

// peel resolves a tag ref to the commit it names, through annotated tags.
func peel(ctx context.Context, g *github, obj gitObject) (string, error) {
	for range 4 {
		if obj.Type != "tag" {
			if obj.Type != "commit" {
				return "", fmt.Errorf("tag names a %s", obj.Type)
			}
			return obj.SHA, nil
		}
		var t gitTag
		if err := g.get(ctx, g.repoPath("/git/tags/%s", obj.SHA), &t); err != nil {
			return "", err
		}
		obj = t.Object
	}
	return "", errors.New("tag chain too deep")
}

// tagCommit resolves a tag name to its commit; found is false when the tag
// does not exist.
func tagCommit(ctx context.Context, g *github, tag string) (commit string, found bool, err error) {
	var ref gitRef
	if err := g.get(ctx, g.repoPath("/git/ref/tags/%s", url.PathEscape(tag)), &ref); err != nil {
		if errors.Is(err, errNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	c, err := peel(ctx, g, ref.Object)
	return c, err == nil, err
}

// candidateFindings is the "Release candidate" gate: a vX.Y.0 release needs
// a vX.Y.0-rc.N tag on the same commit whose release run passed and whose
// pre-release was published at least 14 days before now; a patch and a
// candidate need none (applicable is false).
func candidateFindings(ctx context.Context, g *github, v version, commit string, now time.Time) (applicable bool, findings []string, err error) {
	if !v.needsCandidate() {
		return false, nil, nil
	}
	prefix := v.base().tag() + "-rc."
	refs, err := list[gitRef](ctx, g, g.repoPath("/git/matching-refs/tags/%s", url.PathEscape(prefix)))
	if err != nil {
		return true, nil, err
	}
	var problems []string
	candidates := 0
	for _, ref := range refs {
		name := strings.TrimPrefix(ref.Ref, "refs/tags/")
		c, err := parseTag(name)
		if err != nil || c.RC == 0 || c.base() != v.base() {
			continue
		}
		at, err := peel(ctx, g, ref.Object)
		if err != nil {
			return true, nil, err
		}
		if at != commit {
			continue
		}
		candidates++
		why, err := candidateProblems(ctx, g, name, commit, now)
		if err != nil {
			return true, nil, err
		}
		if len(why) == 0 {
			return true, nil, nil
		}
		problems = append(problems, why...)
	}
	if candidates == 0 {
		return true, []string{fmt.Sprintf("no %sN tag names commit %s: tag a candidate on it first", prefix, commit)}, nil
	}
	return true, problems, nil
}

// candidateProblems checks one candidate on the release commit: a passed
// release run of its tag, and a pre-release published at least 14 days
// before now.
func candidateProblems(ctx context.Context, g *github, tag, commit string, now time.Time) ([]string, error) {
	var out []string
	runs, err := listIn(ctx, g, g.repoPath("/actions/workflows/%s/runs?event=push&head_sha=%s", releaseWorkflow, commit),
		func(p *runsPage) []workflowRun { return p.Runs })
	if err != nil {
		return nil, err
	}
	passed := false
	for _, r := range runs {
		if r.HeadBranch == tag && r.HeadSHA == commit && r.Status == "completed" && r.Conclusion == "success" {
			passed = true
		}
	}
	if !passed {
		out = append(out, fmt.Sprintf("%s: no passed %s run", tag, releaseWorkflow))
	}
	var rel release
	switch err := g.get(ctx, g.repoPath("/releases/tags/%s", url.PathEscape(tag)), &rel); {
	case errors.Is(err, errNotFound):
		out = append(out, fmt.Sprintf("%s: not published", tag))
		return out, nil
	case err != nil:
		return nil, err
	}
	if rel.Draft || !rel.Prerelease {
		out = append(out, fmt.Sprintf("%s: not published as a pre-release", tag))
		return out, nil
	}
	published, err := time.Parse(time.RFC3339, rel.PublishedAt)
	if err != nil {
		return nil, fmt.Errorf("%s: published_at %q: %w", tag, rel.PublishedAt, err)
	}
	if age := now.Sub(published); age < candidateWindow {
		out = append(out, fmt.Sprintf("%s: published %s, %s ago; a candidate stays published at least 14 days (target) before its release",
			tag, published.UTC().Format(time.RFC3339), age.Truncate(time.Minute)))
	}
	return out, nil
}
