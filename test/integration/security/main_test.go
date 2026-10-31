// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

// Package security is the security Node integration suite
// (docs/engineering/03-testing-and-quality-strategy.md "Integration
// tests", the security row; docs/architecture/08-security-and-identity.md).
// Every case drives the controllable-clock build of ruralzd from
// internal/testkit/binaries over real sockets on the process harness
// (internal/testkit/topology): Node timers, TTLs, refresh intervals and
// backoffs move only when a case advances the harness's clock, and the
// harness waits for the mocks and for the Nodes' reactions with condition
// polls under real-time deadlines, never with bare sleeps.
//
// The cases: JWKS rotation in controlled time (jwks_test.go); auth.mtls
// and its CA rotation (mtls_test.go); the admin port, a bearer token from
// a non-loopback peer refused without TLS inside a network namespace
// included (admin_test.go, netns_test.go); secret-to-destination binding
// (binding_test.go); the State Store entry MAC with keyed and key-less
// Nodes on one State Store (mac_test.go); Secrets rules 5 and 6
// (secrets_test.go); listener and Upstream certificate rotation by watch
// (rotation_test.go); the auth.basic throttle (basic_test.go); and the
// secret leak and redaction test over every surface the harness scans
// (leak_test.go). Every value a case supplies carries a harness canary,
// and the teardown of every topology fails the case when a canary leaks.
//
// A case that needs the controllable clock skips when
// RURALZ_TEST_BIN_DIR names a release build.
package security

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// bins is the ruralzd under test, built once by TestMain; digest is the
// configuration pipeline's Revision digest of a one-file rendered Bundle.
// Test-only package state: TestMain sets both before any test runs.
var (
	bins   binaries.Set
	digest func([]byte) (string, error)
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

// runMain builds the controllable-clock ruralzd, or takes the build
// RURALZ_TEST_BIN_DIR names, and runs the tests.
func runMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "rz-security-bin-")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "security:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "security:", err)
		return 1
	}
	bins, err = binaries.Build(context.Background(), root, dir, binaries.BuildOptions{
		TestClock: true, Commands: []string{binaries.Ruralzd},
	})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "security: building ruralzd:", err)
		return 1
	}
	if digest, err = revisionDigest(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "security:", err)
		return 1
	}
	return m.Run()
}

// binDir is the directory holding the ruralzd under test, which a network
// namespace child takes through RURALZ_TEST_BIN_DIR instead of building
// again.
func binDir() string { return filepath.Dir(bins.Ruralzd) }

// revisionDigest returns the Revision digest the configuration pipeline
// computes for a one-file Bundle, as a file-mode Node loads it without
// ${VAR} values. A Bundle the pipeline rejects, which a case swaps in to
// see the Node reject it too, has no Revision: its test render is named by
// the SHA-256 of its bytes instead, which no Node ever reports.
func revisionDigest() (func([]byte) (string, error), error) {
	celc, err := cel.New()
	if err != nil {
		return nil, err
	}
	p, err := pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: celc, Now: time.Now})
	if err != nil {
		return nil, err
	}
	return func(rendered []byte) (string, error) {
		res, err := p.Run(context.Background(), pipeline.Request{
			Source:    loader.Source{FS: fstest.MapFS{"ruralz.yaml": &fstest.MapFile{Data: rendered}}, Path: "ruralz.yaml"},
			Variables: subst.Map{},
		})
		if err != nil {
			return "", err
		}
		if res.Validated == nil {
			sum := sha256.Sum256(rendered)
			return "sha256:" + hex.EncodeToString(sum[:]), nil
		}
		return res.Validated.Revision.Digest.String(), nil
	}, nil
}

// newTopology returns a topology over the controllable-clock ruralzd,
// skipping t against a release build.
func newTopology(t *testing.T, c topology.Config) *topology.Topology {
	t.Helper()
	if !bins.TestClock {
		t.Skip("needs the controllable-clock ruralzd build; " + binaries.EnvBinDir + " names a release build")
	}
	c.Binary, c.TestClock, c.Digest = bins.Ruralzd, true, digest
	return topology.New(t, c)
}
