// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Package e2e is the end-to-end suite of stage 10
// (docs/engineering/03-testing-and-quality-strategy.md "End-to-end
// tests"): it drives the built ruralz and ruralzd on the process harness,
// internal/testkit/topology, in file mode on loopback, with the secret
// leak assertions of every harness run. Until ruralz test run lands
// (Planned (M2)), a Go driver runs the ruralz.test.v1 case files of
// testdata/ (docs/reference/01-cli-and-api-surface.md "Test case format").
//
// The suite covers the M1 scenarios the process harness shows (the REST
// example Bundle served with the Filter Chains ruralz bundle render
// --effective --route prints, ruralz bundle diff against a Node's
// /config/dump, a Zero-Downtime Upgrade under load), the air-gapped start
// in a loopback-only network namespace, and end-to-end cases for each
// documented exit code and output form of every M1 command of the CLI
// command registry (docs/_meta/foundation-pack.md section 9).
package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
)

// buildTimeout bounds the build of the binaries under test.
const buildTimeout = 10 * time.Minute

// built holds the binaries TestMain built, or took from
// RURALZ_TEST_BIN_DIR, for the whole test binary: ruralz, and ruralzd as
// the controllable-clock build.
var built binaries.Set

// builtDir is the directory that holds built; a network namespace child
// takes the same binaries through RURALZ_TEST_BIN_DIR.
var builtDir string

// TestMain builds the binaries the suite runs, once per test binary, with
// the release flags: ruralz, and ruralzd as the controllable-clock build
// (internal/testkit/binaries), or takes the prebuilt ones
// RURALZ_TEST_BIN_DIR names; it removes what it built when the tests end.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "ruralz-e2e-bin-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(dir) }()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	if built, err = binaries.Build(ctx, root, dir, binaries.BuildOptions{TestClock: true}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 2
	}
	builtDir = dir
	if d := os.Getenv(binaries.EnvBinDir); d != "" {
		builtDir = d
	}
	return m.Run()
}
