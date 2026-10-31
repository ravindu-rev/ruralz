// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRunUsage: a missing or unknown command, a missing required flag, an
// unknown flag, an extra argument, no token or a bad run ID is a usage
// error (exit 2).
func TestRunUsage(t *testing.T) {
	f := newFakeGitHub(t)
	env := f.env(time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), nil)
	noToken := env
	noToken.getenv = func(k string) string {
		if k == "GH_TOKEN" {
			return ""
		}
		return env.getenv(k)
	}
	cases := []struct {
		args []string
		env  environ
		want string
	}{
		{nil, env, "usage: releasegate tag|gates|latest|lines|image"},
		{[]string{"publish"}, env, `unknown command "publish"`},
		{[]string{"tag", "-tag", "v0.1.0"}, env, "releasegate tag: -commit is required"},
		{[]string{"gates", "-tag", "v0.1.0", "-commit", commitA}, env, "releasegate gates: -run is required"},
		{[]string{"gates", "-tag", "v0.1.0", "-commit", commitA, "-run", "x"}, env, `-run "x" is not a workflow run ID`},
		{[]string{"latest"}, env, "releasegate latest: -tag is required"},
		{[]string{"latest", "-tag", "v1"}, env, "neither vX.Y.Z nor vX.Y.Z-rc.N"},
		{[]string{"image", "-tag", "v0.1.0"}, env, "releasegate image: -digest is required"},
		{[]string{"lines", "extra"}, env, "unexpected arguments"},
		{[]string{"lines", "-bogus"}, env, "flag provided but not defined: -bogus"},
		{[]string{"lines"}, noToken, "no GitHub token"},
		{[]string{"lines", "-repo", "not a repo"}, env, "is not owner/name"},
	}
	for _, c := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), c.args, &stdout, &stderr, c.env); code != 2 || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("run(%q) = %d, %q; want 2 and %q", c.args, code, stderr.String(), c.want)
		}
	}
}

// TestRunCommands: tag, latest and lines through run and the environment:
// a passed tag check exits 0 with its outputs, a failed one 1; latest and
// lines print their answers.
func TestRunCommands(t *testing.T) {
	f, janeBlob, _ := tagFixture(t)
	f.addTag("v0.2.0", commitA, sshSignature(janeBlob, "git"), true)
	f.addRelease("v0.0.9", false, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env := f.env(time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), nil)
	outputs := filepath.Join(t.TempDir(), "github_output")

	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"tag", "-tag", "v0.2.0", "-commit", commitA, "-outputs", outputs}, &stdout, &stderr, env); code != 0 {
		t.Fatalf("tag: exit %d, %s", code, stderr.String())
	}
	data := readBack(t, outputs)
	if !strings.Contains(string(data), "version=0.2.0\nkind=release\nprerelease=false\nprevious=v0.1.1\n") {
		t.Errorf("outputs %q", data)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(t.Context(), []string{"tag", "-tag", "v0.9.0", "-commit", commitA}, &stdout, &stderr, env); code != 1 || !strings.Contains(stderr.String(), "does not exist") {
		t.Errorf("absent tag: exit %d, %s", code, stderr.String())
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"latest", "-tag", "v0.1.2"}, "true\n"},
		{[]string{"latest", "-tag", "v0.0.10"}, "false\n"},
		{[]string{"lines"}, `["v0.1.1","v0.0.9"]` + "\n"},
	} {
		stdout.Reset()
		if code := run(t.Context(), c.args, &stdout, &stderr, env); code != 0 || stdout.String() != c.want {
			t.Errorf("run(%q) = %d, %q; want %q", c.args, code, stdout.String(), c.want)
		}
	}
}

// TestGateRegistryMatchesDocs: the gates releasegate decides are exactly
// the rows of both Release gates tables of the testing document that apply
// from M0 and M1, in table order.
func TestGateRegistryMatchesDocs(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "engineering", "03-testing-and-quality-strategy.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(data), "\n## Release gates\n")
	if !ok {
		t.Fatal("no Release gates section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	var decided, evidence []string
	table := 0
	for _, line := range strings.Split(section, "\n") {
		switch {
		case strings.HasPrefix(line, "| Gate |"):
			table++
		case strings.HasPrefix(line, "|---"):
		case strings.HasPrefix(line, "| "):
			cells := strings.Split(line, " | ")
			gate := strings.TrimPrefix(cells[0], "| ")
			if table == 1 && regexp.MustCompile(`^Planned \(M[01]\)`).MatchString(strings.TrimSuffix(cells[len(cells)-1], " |")) {
				decided = append(decided, gate)
			}
			if table == 2 {
				evidence = append(evidence, gate)
			}
		}
	}
	if !slices.Equal(decided, gates()) {
		t.Errorf("M0 and M1 gates of the first table %q, want gates() %q", decided, gates())
	}
	if !slices.Equal(evidence, gates()) {
		t.Errorf("gates of the evidence table %q, want gates() %q", evidence, gates())
	}
}
