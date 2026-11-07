// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package binaries

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestBuildRepository builds the real cmd/ruralzd and cmd/ruralz with the
// release flags and checks that `ruralz version` reports the build
// metadata Build sets.
func TestBuildRepository(t *testing.T) {
	t.Setenv(EnvBinDir, "")
	root, err := RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	set, err := Build(ctx, root, t.TempDir(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(set.Ruralzd); err != nil {
		t.Fatal(err)
	}
	out := version(ctx, t, set.Ruralz)
	if !strings.Contains(out, "version:      "+DefaultVersion+"\n") || !strings.Contains(out, "commit:       "+DefaultCommit+"\n") {
		t.Fatalf("ruralz version:\n%s", out)
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
