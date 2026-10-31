// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeTree writes files under dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// symlinkOrSkip creates a symbolic link, skipping the test where the OS
// refuses it (Windows without the privilege).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links are not available: %v", err)
	}
}

// TestOSDirectory covers a Bundle read from the operating system: the same
// discovery and diagnostics as from an fs.FS, with slash-separated
// root-relative paths on every OS.
func TestOSDirectory(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		RootFile:                   gateway,
		"routes/orders.yaml":       route("orders"),
		"upstreams/orders.yaml":    upstream("orders"),
		"overlays/prod/scale.yaml": "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: orders\nspec:\n  timeout: 9s\n",
		IgnoreFile:                 "drafts/\n",
		"drafts/broken.yaml":       "{",
		"routes/bad.yaml":          "kind: Route\nmetadata:\n  name: bad\n",
	})
	envFile := filepath.Join(t.TempDir(), "envs.yaml")
	writeTree(t, filepath.Dir(envFile), map[string]string{"envs.yaml": environment("prod", " {}")})
	r, err := newLoader(t, Options{}).Load(t.Context(), Request{
		Source: Source{Path: dir}, Environments: Source{Path: envFile}, Environment: "prod",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantTexts(t, "OS directory", r.Diagnostics, "routes/bad.yaml:1:1 error RZ-CFG-007 Route/bad apiVersion: apiVersion is required")
	if got := ids(r.Resources); !slices.Equal(got, []string{"Gateway/edge", "Route/orders", "Upstream/orders"}) {
		t.Errorf("resources = %q", got)
	}
	if r.Overlay != "prod" || at(t, find(t, r.Resources, "Upstream/orders").Root, "spec.timeout").Text != "9s" {
		t.Errorf("the overlay did not apply: %q", r.Overlay)
	}
}

// TestOSFileSource covers a one-file Bundle named by an OS path:
// diagnostics name it as given, slash-separated.
func TestOSFileSource(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"out/rendered.yaml": docs(gateway, upstream("orders"), upstream("orders"))})
	p := filepath.Join(dir, "out", "rendered.yaml")
	r, err := newLoader(t, Options{}).Load(t.Context(), Request{Source: Source{Path: p}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].File != filepath.ToSlash(p) || r.Diagnostics[0].Code != CodeDuplicate {
		t.Errorf("diagnostics = %q, want RZ-CFG-008 in %s", texts(r.Diagnostics), filepath.ToSlash(p))
	}
}

// TestOSRootThroughSymlink covers a Bundle root path that is itself a
// symbolic link, which is how a watched directory is replaced atomically:
// the link is followed once and the load reads its target.
func TestOSRootThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"v1/" + RootFile: gateway})
	link := filepath.Join(dir, "current")
	symlinkOrSkip(t, filepath.Join(dir, "v1"), link)
	r := load(t, newLoader(t, Options{}), os.DirFS(link))
	wantClean(t, "through a link", r.Diagnostics)
	r2, err := newLoader(t, Options{}).Load(t.Context(), Request{Source: Source{Path: link}})
	if err != nil {
		t.Fatal(err)
	}
	wantClean(t, "OS path through a link", r2.Diagnostics)
	if len(r2.Resources) != 1 {
		t.Errorf("resources = %q", ids(r2.Resources))
	}
}

// TestOSSymlinkInside covers a symbolic link inside a Bundle read from the
// operating system: RZ-CFG-001, its target never read, even one outside
// the root.
func TestOSSymlinkInside(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeTree(t, dir, map[string]string{RootFile: gateway})
	writeTree(t, outside, map[string]string{"u.yaml": upstream("outside")})
	symlinkOrSkip(t, filepath.Join(outside, "u.yaml"), filepath.Join(dir, "u.yaml"))
	r, err := newLoader(t, Options{}).Load(t.Context(), Request{Source: Source{Path: dir}})
	if err != nil {
		t.Fatal(err)
	}
	if got := codes(r.Diagnostics); !slices.Equal(got, []string{CodeLimit}) || r.Diagnostics[0].File != "u.yaml" {
		t.Errorf("diagnostics = %q", texts(r.Diagnostics))
	}
	if slices.Contains(ids(r.Resources), "Upstream/outside") {
		t.Error("the link's target was read")
	}
}
