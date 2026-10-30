// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunUsage: a missing or unknown command, a missing required flag, an
// unknown flag or an extra argument is a usage error (exit 2).
func TestRunUsage(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{nil, "usage: releasekit image|package|sbom|notes|checksums|verify"},
		{[]string{"publish"}, `unknown command "publish"`},
		{[]string{"image"}, "releasekit image: -dir is required"},
		{[]string{"sbom", "-manifest", "m"}, "releasekit sbom: -scans is required"},
		{[]string{"package", "-dist", "d"}, "releasekit package: -notices is required"},
		{[]string{"package", "-dist", "d", "-notices", "n", "-out", "o", "-version", "0.1.0"}, "-commit is required"},
		{[]string{"checksums"}, "releasekit checksums: -dir is required"},
		{[]string{"notes", "-version", "0.1.0", "-from", "root", "-to", "HEAD"}, "releasekit notes: -out is required"},
		{[]string{"verify"}, "releasekit verify: -manifest is required"},
		{[]string{"verify", "-manifest", "m", "extra"}, "unexpected arguments"},
		{[]string{"verify", "-bogus"}, "flag provided but not defined: -bogus"},
	}
	for _, c := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), c.args, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("run(%q) = %d, %q; want 2 and %q", c.args, code, stderr.String(), c.want)
		}
	}
}

// TestRunExitCodes: findings exit 1, input errors 2, success 0, through
// the real build information reader and git.
func TestRunExitCodes(t *testing.T) {
	fx := newFixture(t, "0.1.0")
	fx.release(t)
	manifestPath := filepath.Join(fx.out, manifestName)
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		// The fixture's binaries are no Go binaries, which the audit finds.
		{"verify findings", []string{"verify", "-C", fx.root, "-manifest", manifestPath}, 1, "not a Go binary"},
		{"verify without a manifest", []string{"verify", "-C", fx.root, "-manifest", filepath.Join(fx.out, "absent.json")}, 2, "releasekit verify:"},
		{"checksums", []string{"checksums", "-dir", fx.out}, 0, ""},
		{"checksums without a manifest", []string{"checksums", "-dir", t.TempDir()}, 2, "releasekit checksums:"},
		{"notes with a bad version", []string{"notes", "-version", "v1", "-from", "root", "-to", "HEAD", "-out", filepath.Join(t.TempDir(), "n.md")}, 2, "leading v"},
	}
	for _, c := range cases {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), c.args, &stdout, &stderr)
		if code != c.code || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%s: exit %d, stderr %q; want %d and %q", c.name, code, stderr.String(), c.code, c.want)
		}
	}

	// image prints the layout's index digest; sbom writes the image SBOM.
	digest := fx.writeImage(t, nil)
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"image", "-dir", fx.image}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != digest {
		t.Errorf("image: exit %d, stdout %q, stderr %q; want 0 and %s", code, stdout.String(), stderr.String(), digest)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(t.Context(), []string{"image", "-dir", t.TempDir()}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "image layout") {
		t.Errorf("image of an empty directory: exit %d, stderr %q; want 1", code, stderr.String())
	}
	m, err := readManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	scans := fx.writeScans(t, m)
	if code := run(t.Context(), []string{"sbom", "-manifest", manifestPath, "-scans", scans}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "ruralzd_0.1.0_image.cdx.json") {
		t.Errorf("sbom: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(t.Context(), []string{"sbom", "-manifest", manifestPath, "-scans", t.TempDir()}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "no scan of the linux/amd64 image") {
		t.Errorf("sbom without scans: exit %d, stderr %q; want 1", code, stderr.String())
	}

	// package through git: the fixture root as a repository, whose commit
	// time dates the archives; the fake binaries are findings.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	pf := newFixture(t, "0.1.0")
	gitIn(t, pf.root, "init", "--quiet", "--initial-branch=main")
	gitIn(t, pf.root, "add", ".")
	gitIn(t, pf.root, "commit", "--quiet", "-m", "feat: fixture")
	commit := gitIn(t, pf.root, "rev-parse", "HEAD")
	stdout.Reset()
	stderr.Reset()
	code := run(t.Context(), []string{
		"package", "-C", pf.root, "-dist", pf.dist, "-notices", pf.notices, "-out", pf.out, "-version", "0.1.0", "-commit", commit,
		"-image", pf.image, "-bench", pf.bench,
	}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "ruralzd_linux_amd64: not a Go binary") {
		t.Errorf("package: exit %d, stderr %q; want 1 and the findings", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = run(t.Context(), []string{"package", "-C", pf.root, "-dist", pf.dist, "-notices", pf.notices, "-out", filepath.Join(t.TempDir(), "out"), "-version", "0.1.0", "-commit", commit}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "the release 0.1.0 has no image") {
		t.Errorf("package of a release without an image: exit %d, stderr %q; want 1", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	notes := filepath.Join(t.TempDir(), "NOTES.md")
	if code := run(t.Context(), []string{"notes", "-C", pf.root, "-version", "0.1.0", "-from", "root", "-to", "HEAD", "-out", notes}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "wrote the release notes") {
		t.Errorf("notes: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}
