// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package statestore is the State Store Node integration suite
// (docs/engineering/03-testing-and-quality-strategy.md "Integration
// tests", the statestore row): the controllable-clock ruralzd build
// serving real requests over loopback through the process harness
// (internal/testkit/topology), against every kind
// internal/statestore/statestoretest/redisserver selects, counting the
// commands the Node sends through the counting RESP proxy or by INFO
// commandstats deltas. It covers the round trip of
// docs/adr/0008-rate-limiting-local-bucket-and-gcra.md for every M1 State
// Store Policy type, the required State Store scenarios and the State
// client behaviors a Node shows
// (docs/architecture/11-scalability-and-distributed-state.md "State client
// and RZ-STS error codes").
package statestore

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
)

// buildTimeout bounds the build of the ruralzd under test.
const buildTimeout = 10 * time.Minute

// The ruralzd under test, built once per test binary on first use, so a
// run whose every case skips builds nothing.
var (
	binDir    string
	buildOnce sync.Once
	built     binaries.Set
	buildErr  error
)

// TestMain owns the directory the ruralzd under test is built into and
// removes it when the tests end.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "ruralz-statestore-bin-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "statestore: %v\n", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(dir) }()
	binDir = dir
	return m.Run()
}

// ruralzd returns the ruralzd under test: the controllable-clock build of
// this commit, with the release flags, or the build RURALZ_TEST_BIN_DIR
// names. The suite asserts timeouts, TTLs and refresh intervals in
// controlled time, so it skips against a build without the controllable
// clock (docs/engineering/03-testing-and-quality-strategy.md "Hermetic
// rule scope").
func ruralzd(t *testing.T) binaries.Set {
	t.Helper()
	buildOnce.Do(func() {
		root, err := binaries.RepoRoot(".")
		if err != nil {
			buildErr = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
		defer cancel()
		built, buildErr = binaries.Build(ctx, root, binDir, binaries.BuildOptions{TestClock: true, Commands: []string{binaries.Ruralzd}})
	})
	if buildErr != nil {
		t.Fatalf("statestore: build ruralzd: %v", buildErr)
	}
	if !built.TestClock {
		t.Skipf("statestore: %s names a ruralzd without the controllable clock; the suite runs in controlled time", binaries.EnvBinDir)
	}
	return built
}
