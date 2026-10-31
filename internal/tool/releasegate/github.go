// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
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

// errNotFound reports a 404 from the GitHub API.
var errNotFound = errors.New("not found")

// apiError is a GitHub API response outside 2xx.
type apiError struct {
	method, target string
	code           int
	msg            string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s: %d %s: %s", e.method, e.target, e.code, http.StatusText(e.code), e.msg)
}

// Is makes a 404 match errNotFound.
func (e *apiError) Is(target error) bool {
	return target == errNotFound && e.code == http.StatusNotFound
}

// statusCode returns the HTTP status of an API error, or 0.
func statusCode(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.code
	}
	return 0
}

// github is a minimal GitHub REST API client: JSON requests with a token,
// following rel="next" links for listings.
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

// repoPath is a path below /repos/<owner>/<name>.
func (g *github) repoPath(format string, args ...any) string {
	return "/repos/" + g.repo + fmt.Sprintf(format, args...)
}

// do sends one request and decodes a JSON response into out (unless nil).
// It returns the response's next-page URL, if any.
func (g *github) do(ctx context.Context, method, target string, body, out any) (string, error) {
	u := target
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		u = g.base + target
	}
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd) //nolint:gosec // G704: the API root newGitHub checked, from -api or GITHUB_API_URL, which the workflow sets.
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req) //nolint:gosec // G704: see NewRequestWithContext above.
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", method, target, err)
	}
	if len(data) > maxResponseBytes {
		return "", fmt.Errorf("%s %s: response exceeds %d bytes", method, target, maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", &apiError{method: method, target: target, code: resp.StatusCode, msg: msg}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return "", fmt.Errorf("%s %s: %w", method, target, err)
		}
	}
	return nextLink(resp.Header.Get("Link")), nil
}

// get decodes one GET response.
func (g *github) get(ctx context.Context, path string, out any) error {
	_, err := g.do(ctx, http.MethodGet, path, nil, out)
	return err
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

// list collects every page of a listing whose pages are JSON arrays.
func list[T any](ctx context.Context, g *github, path string) ([]T, error) {
	var all []T
	next := withPerPage(path)
	for page := 0; next != ""; page++ {
		if page == maxPages {
			return nil, fmt.Errorf("GET %s: more than %d pages", path, maxPages)
		}
		var items []T
		n, err := g.do(ctx, http.MethodGet, next, nil, &items)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		next = n
	}
	return all, nil
}

// listIn collects every page of a listing whose pages are objects holding
// the items in one member, such as {"jobs": [...]}; items picks them.
func listIn[P, T any](ctx context.Context, g *github, path string, items func(*P) []T) ([]T, error) {
	var all []T
	next := withPerPage(path)
	for page := 0; next != ""; page++ {
		if page == maxPages {
			return nil, fmt.Errorf("GET %s: more than %d pages", path, maxPages)
		}
		var p P
		n, err := g.do(ctx, http.MethodGet, next, nil, &p)
		if err != nil {
			return nil, err
		}
		all = append(all, items(&p)...)
		next = n
	}
	return all, nil
}

// withPerPage asks for 100 items per page.
func withPerPage(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "per_page=100"
}
