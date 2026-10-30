// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// goIn runs the go command in dir with extra environment and returns its
// trimmed output.
func goIn(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", args...) //nolint:gosec // The test's own go commands.
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// TestReleaseFromRepository runs the stage 12 tooling on this repository
// in the order of make release-dry-run without Docker: release-flag
// binaries of three artifacts (one of each executable format), depgate's
// notices, package, an SBOM per archive as Syft writes one, notes,
// checksums and the audit, which finds nothing. It proves depgate and
// releasekit agree on the notices and that the real deploy/systemd/ files
// ship.
func TestReleaseFromRepository(t *testing.T) {
	if testing.Short() {
		t.Skip("builds release binaries")
	}
	if os.Getenv("GOEXPERIMENT") != "" {
		t.Skip("GOEXPERIMENT is set (the floor job); release binaries never set one")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, err := filepath.Abs(repoRoot())
	if err != nil {
		t.Fatal(err)
	}
	commit, err := resolveCommit(t.Context(), runGit, root, "HEAD")
	if err != nil {
		t.Skipf("not a Git checkout: %v", err)
	}
	const version = "0.0.0-dev"
	base := t.TempDir()
	dist, notices, out := filepath.Join(base, "dist"), filepath.Join(base, "dist", "notices"), filepath.Join(base, "release")
	arts := []artifact{
		{"ruralzd", "linux", "amd64", supportProduction},
		{"ruralz", "darwin", "arm64", supportProduction},
		{"ruralz", "windows", "amd64", supportProduction},
	}
	ldflags := "-s -w -X " + modulePath + "/internal/buildinfo.version=" + version +
		" -X " + modulePath + "/internal/buildinfo.commit=" + commit +
		" -X " + modulePath + "/internal/buildinfo.flavor=default"
	for _, a := range arts {
		goIn(t, root, []string{"CGO_ENABLED=0", "GOOS=" + a.OS, "GOARCH=" + a.Arch},
			"build", "-trimpath", "-ldflags", ldflags, "-o", filepath.Join(dist, a.distDir(), a.executable()), "./cmd/"+a.Binary)
	}
	goIn(t, root, nil, "run", "./internal/tool/depgate", "-C", root, "-notices", notices)
	toolchain := goIn(t, root, nil, "env", "GOVERSION")

	// A development dry run without Docker records no image.
	m, err := runPackage(t.Context(), packageOptions{root: root, dist: dist, notices: notices, out: out, version: version, commit: commit, bench: filepath.Join(base, "bench")},
		packageDeps{git: runGit, facts: readBuildFacts, toolchain: toolchain, artifacts: arts})
	if err != nil {
		t.Fatal(err)
	}
	for i, info := range m.Archives {
		facts, err := readBuildFacts(readBack(t, filepath.Join(dist, arts[i].distDir(), arts[i].executable())))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(out, info.SBOM), sbomFor(facts.Info.Deps))
	}
	if err := runNotes(t.Context(), notesOptions{root: root, version: version, from: rootRef, to: commit, manifest: filepath.Join(out, manifestName), out: filepath.Join(out, notesName)}, runGit); err != nil {
		t.Fatal(err)
	}
	if findings, err := runChecksums(out); err != nil || len(findings) > 0 {
		t.Fatalf("checksums: %q %v", findings, err)
	}
	findings, err := runVerify(verifyOptions{root: root, manifest: filepath.Join(out, manifestName)},
		verifyDeps{facts: readBuildFacts, toolchain: toolchain, artifacts: arts})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
	var systemd []string
	for _, e := range m.Archives[0].Entries {
		if strings.HasPrefix(e.Path, "deploy/systemd/") && e.Type == entryFile {
			systemd = append(systemd, e.Path+" "+e.Mode)
		}
	}
	want := "deploy/systemd/99-ruralzd.conf 0644 deploy/systemd/ruralzd-handover 0755 deploy/systemd/ruralzd.env 0644 deploy/systemd/ruralzd.service 0644 deploy/systemd/ruralzd.sysusers 0644"
	if strings.Join(systemd, " ") != want {
		t.Errorf("linux ruralzd archive ships %q, want %q", systemd, want)
	}
}
