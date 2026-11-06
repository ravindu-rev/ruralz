// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// maxResponseBytes bounds one GitHub API response body.
const maxResponseBytes = 32 << 20

// maxPages bounds a paginated listing: 100 pages of 100 items.
const maxPages = 100

// github is a minimal GitHub REST API client: JSON listings with a token,
// following rel="next" links.
type github struct {
	// base is the API root, such as https://api.github.com.
	base string
	// repo is "owner/name".
	repo  string
	token string
	http  *http.Client
}

// newGitHub validates the repository name and API root.
func newGitHub(base, repo, token string, client *http.Client) (*github, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`).MatchString(repo) {
		return nil, fmt.Errorf("repository %q is not owner/name", repo)
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("API root %q is not an http(s) URL", base)
	}
	if token == "" {
		return nil, errors.New("no GitHub token: set GH_TOKEN or GITHUB_TOKEN")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &github{base: strings.TrimRight(base, "/"), repo: repo, token: token, http: client}, nil
}

// getPage sends one GET and decodes its JSON body into out. It returns the
// response's next-page URL, if any.
func (g *github) getPage(ctx context.Context, target string, out any) (string, error) {
	u := target
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		u = g.base + target
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", target, err)
	}
	if len(data) > maxResponseBytes {
		return "", fmt.Errorf("GET %s: response exceeds %d bytes", target, maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", fmt.Errorf("GET %s: %d %s: %s", target, resp.StatusCode, http.StatusText(resp.StatusCode), msg)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return "", fmt.Errorf("GET %s: %w", target, err)
	}
	return nextLink(resp.Header.Get("Link")), nil
}

// nextLink returns the rel="next" URL of a Link header.
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		target := strings.TrimSpace(segs[0])
		for _, s := range segs[1:] {
			if strings.TrimSpace(s) == `rel="next"` && strings.HasPrefix(target, "<") && strings.HasSuffix(target, ">") {
				return target[1 : len(target)-1]
			}
		}
	}
	return ""
}

// listAll collects every page of a listing whose pages are JSON arrays.
func listAll[T any](ctx context.Context, g *github, path string) ([]T, error) {
	var all []T
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	next := path + sep + "per_page=100"
	for page := 0; next != ""; page++ {
		if page == maxPages {
			return nil, fmt.Errorf("GET %s: more than %d pages", path, maxPages)
		}
		var items []T
		n, err := g.getPage(ctx, next, &items)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		next = n
	}
	return all, nil
}

// ghRelease is the part of a GitHub release the publish job reads.
type ghRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// publishedReleases lists the versions of the repository's published
// releases: those under a release tag vX.Y.Z that are neither a draft,
// such as one a failed run left, nor a pre-release.
func publishedReleases(ctx context.Context, g *github) ([]releaseVersion, error) {
	rs, err := listAll[ghRelease](ctx, g, "/repos/"+g.repo+"/releases")
	if err != nil {
		return nil, err
	}
	var out []releaseVersion
	for _, r := range rs {
		if v, ok := parseReleaseTag(r.TagName); ok && !r.Draft && !r.Prerelease {
			out = append(out, v)
		}
	}
	return out, nil
}

// isLatest reports whether v is at or above every published release, the
// only case in which the publish job marks its GitHub release latest: a
// release that a failed run publishes on a re-run, after a higher one, never
// becomes latest.
func isLatest(v releaseVersion, published []releaseVersion) bool {
	for _, p := range published {
		if compareVersions(p, v) > 0 {
			return false
		}
	}
	return true
}

// newestOfLine reports whether v is at or above every published release of
// its line X.Y, the only case in which the image tag X.Y moves to v's image.
func newestOfLine(v releaseVersion, published []releaseVersion) bool {
	for _, p := range published {
		if p.line() == v.line() && compareVersions(p, v) > 0 {
			return false
		}
	}
	return true
}
