// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package binaries

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuildRepository builds the real cmd/ruralzd and cmd/ruralz with the
// release flags and an injected version, then an overlay variant, and
// checks that `ruralz version` reports each as injected (11 test plan item
// 12; 11 req 32, 45, 47).
func TestBuildRepository(t *testing.T) {
	t.Setenv(EnvBinDir, "")
	root, err := RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	set, err := Build(ctx, root, t.TempDir(), BuildOptions{Version: "0.1.0-e2e.a", Commit: "0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(set.Ruralzd); err != nil {
		t.Fatal(err)
	}
	out := version(ctx, t, set.Ruralz)
	if !strings.Contains(out, "version:     0.1.0-e2e.a\n") || !strings.Contains(out, "commit:      0123456789abcdef\n") {
		t.Fatalf("ruralz version:\n%s", out)
	}

	const served = `"ruralz/v1alpha1"`
	src, err := os.ReadFile(filepath.Join(root, "internal", "buildinfo", "buildinfo.go")) //nolint:gosec // G304: a fixed repository file
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), served) {
		t.Fatalf("internal/buildinfo/buildinfo.go no longer holds %s; update this test", served)
	}
	repl := filepath.Join(t.TempDir(), "buildinfo.go")
	variantSrc := strings.Replace(string(src), served, `"ruralz/v1alpha1-overlay"`, 1)
	if err := os.WriteFile(repl, []byte(variantSrc), 0o600); err != nil { //nolint:gosec // G703: a file in the test's temporary directory
		t.Fatal(err)
	}
	variant, err := Build(ctx, root, t.TempDir(), BuildOptions{
		Version:  "0.1.0-e2e.b",
		Overlay:  map[string]string{"internal/buildinfo/buildinfo.go": repl},
		Commands: []string{Ruralz},
	})
	if err != nil {
		t.Fatal(err)
	}
	out = version(ctx, t, variant.Ruralz)
	if !strings.Contains(out, "version:     0.1.0-e2e.b\n") || !strings.Contains(out, "ruralz/v1alpha1-overlay") {
		t.Fatalf("overlay ruralz version:\n%s", out)
	}
}

func version(ctx context.Context, t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s version: %v\n%s", bin, err, out)
	}
	return string(out)
}
