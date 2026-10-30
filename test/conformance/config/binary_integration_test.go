// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package config

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

// builtBinaries holds the binaries TestMain built, or took from
// RURALZ_TEST_BIN_DIR, for the whole test binary.
var builtBinaries binaries.Set

// TestMain builds the binaries the suite runs, once per test binary, with
// the release flags: ruralz, and ruralzd as the controllable-clock build
// (internal/testkit/binaries), or takes the prebuilt ones
// RURALZ_TEST_BIN_DIR names; it removes what it built when the tests end.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "ruralz-conformance-bin-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "conformance: %v\n", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(dir) }()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "conformance: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	if builtBinaries, err = binaries.Build(ctx, root, dir, binaries.BuildOptions{TestClock: true}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "conformance: %v\n", err)
		return 2
	}
	return m.Run()
}

// binaryRunners returns the runners of the built binaries, after the
// in-process ones (suite.runners): every case of the suite runs through
// them too.
func binaryRunners(t testing.TB, _ *suite) []runner {
	t.Helper()
	return []runner{ruralzBinary{path: builtBinaries.Ruralz}}
}
