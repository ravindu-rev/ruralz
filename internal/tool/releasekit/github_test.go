// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeRepo is the repository the fake GitHub API serves.
const fakeRepo = "ravindu-rev/ruralz"

// fakeGitHub is an in-memory GitHub REST API serving one repository's
// releases, in pages of pageSize items with rel="next" links as GitHub
// does.
type fakeGitHub struct {
	mu       sync.Mutex
	srv      *httptest.Server
	pageSize int
	releases []ghRelease
	// requests counts the requests served.
	requests int
}

func newFakeGitHub(t *testing.T, releases ...ghRelease) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{pageSize: 2, releases: releases}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.Header.Get("Authorization") != "Bearer test-token" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	if r.Method != http.MethodGet || r.URL.Path != "/repos/"+fakeRepo+"/releases" {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	lo := min((page-1)*f.pageSize, len(f.releases))
	hi := min(lo+f.pageSize, len(f.releases))
	if hi < len(f.releases) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		next := url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path, RawQuery: q.Encode()}
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next", <%s>; rel="last"`, next.String(), next.String()))
	}
	writeJSON(w, http.StatusOK, append([]ghRelease{}, f.releases[lo:hi]...))
}

// env is an environment pointing at the fake.
func (f *fakeGitHub) env(extra map[string]string) environ {
	vars := map[string]string{"GITHUB_API_URL": f.srv.URL, "GITHUB_REPOSITORY": fakeRepo, "GH_TOKEN": "test-token"}
	for k, v := range extra {
		vars[k] = v
	}
	return testEnv(vars, f.srv.Client())
}

// sampleReleases are published releases, a draft a failed run left, a
// pre-release and a release under another tag.
func sampleReleases() []ghRelease {
	return []ghRelease{
		{TagName: "v0.1.0"},
		{TagName: "v0.2.0"},
		{TagName: "v0.3.0", Draft: true},
		{TagName: "v0.1.1"},
		{TagName: "v0.4.0", Prerelease: true},
		{TagName: "sdk/go/v0.9.0"},
	}
}

// TestPublishedReleases: every page of the listing, without drafts,
// pre-releases or releases that are no release tag.
func TestPublishedReleases(t *testing.T) {
	f := newFakeGitHub(t, sampleReleases()...)
	g, err := newGitHub(f.srv.URL, fakeRepo, "test-token", f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := publishedReleases(t.Context(), g)
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, v := range got {
		tags = append(tags, v.tag())
	}
	if want := []string{"v0.1.0", "v0.2.0", "v0.1.1"}; !slices.Equal(tags, want) {
		t.Errorf("publishedReleases = %v, want %v", tags, want)
	}
	if f.requests != 3 {
		t.Errorf("%d requests, want the 3 pages", f.requests)
	}
	bad, err := newGitHub(f.srv.URL, fakeRepo, "wrong", f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publishedReleases(t.Context(), bad); err == nil || !strings.Contains(err.Error(), "401 Unauthorized: {\"message\":\"Bad credentials\"}") {
		t.Errorf("wrong credentials: %v", err)
	}
	for _, c := range []struct{ base, repo, token string }{
		{f.srv.URL, "ruralz", "t"}, {"ftp://example.com", fakeRepo, "t"}, {f.srv.URL, fakeRepo, ""},
	} {
		if _, err := newGitHub(c.base, c.repo, c.token, nil); err == nil {
			t.Errorf("newGitHub(%q, %q, %q) accepted bad input", c.base, c.repo, c.token)
		}
	}
}

// TestNextLink: listings follow GitHub's rel="next" links.
func TestNextLink(t *testing.T) {
	h := `<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=9>; rel="last"`
	if got := nextLink(h); got != "https://api.github.com/x?page=2" {
		t.Errorf("nextLink = %q", got)
	}
	if got := nextLink(`<https://api.github.com/x?page=1>; rel="prev"`); got != "" {
		t.Errorf("nextLink without next = %q", got)
	}
}

// TestIsLatest: a release is latest only at or above every published
// release, so a release a re-run publishes after a higher one never
// becomes latest; the first release is.
func TestIsLatest(t *testing.T) {
	published := []releaseVersion{{0, 1, 0}, {0, 2, 0}, {0, 1, 1}}
	cases := []struct {
		name    string
		version releaseVersion
		want    bool
	}{
		{"the next patch", releaseVersion{0, 2, 1}, true},
		{"the next minor", releaseVersion{0, 3, 0}, true},
		{"a re-run of its own publish", releaseVersion{0, 2, 0}, true},
		{"published after a higher release", releaseVersion{0, 1, 2}, false},
	}
	for _, c := range cases {
		if got := isLatest(c.version, published); got != c.want {
			t.Errorf("%s: isLatest(%v) = %t, want %t", c.name, c.version, got, c.want)
		}
	}
	if !isLatest(releaseVersion{0, 1, 0}, nil) {
		t.Error("the first release is not latest")
	}
}

// TestNewestOfLine: X.Y moves only to the newest published release of its
// line, never back to an older patch.
func TestNewestOfLine(t *testing.T) {
	published := []releaseVersion{{0, 1, 0}, {0, 1, 1}, {0, 2, 0}}
	cases := []struct {
		name    string
		version releaseVersion
		want    bool
	}{
		{"the newest patch itself", releaseVersion{0, 1, 1}, true},
		{"a newer patch", releaseVersion{0, 1, 2}, true},
		{"an older patch published later", releaseVersion{0, 1, 0}, false},
		{"a line whose newest is it", releaseVersion{0, 2, 0}, true},
		{"a new line", releaseVersion{0, 3, 0}, true},
	}
	for _, c := range cases {
		if got := newestOfLine(c.version, published); got != c.want {
			t.Errorf("%s: newestOfLine(%v) = %t, want %t", c.name, c.version, got, c.want)
		}
	}
}

// TestRunLatestCommand: latest prints true or false from the published
// releases; without a token it exits 2.
func TestRunLatestCommand(t *testing.T) {
	f := newFakeGitHub(t, sampleReleases()...)
	for version, want := range map[string]string{"0.2.1": "true\n", "0.2.0": "true\n", "0.1.2": "false\n"} {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), []string{"latest", "-version", version}, &stdout, &stderr, f.env(nil)); code != 0 || stdout.String() != want {
			t.Errorf("latest %s: exit %d, stdout %q, stderr %q; want %q", version, code, stdout.String(), stderr.String(), want)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"latest", "-version", "0.2.1", "-api", f.srv.URL, "-repo", fakeRepo}, &stdout, &stderr, f.env(map[string]string{"GH_TOKEN": ""})); code != 2 || !strings.Contains(stderr.String(), "no GitHub token") {
		t.Errorf("latest without a token: exit %d, stderr %q; want 2", code, stderr.String())
	}
	stderr.Reset()
	if code := run(t.Context(), []string{"latest", "-version", "0.2.1"}, &stdout, &stderr, f.env(map[string]string{"GH_TOKEN": "", "GITHUB_TOKEN": "test-token"})); code != 0 {
		t.Errorf("latest with GITHUB_TOKEN: exit %d, stderr %q", code, stderr.String())
	}
}
