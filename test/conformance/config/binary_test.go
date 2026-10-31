// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !integration

package config

import "testing"

// binaryRunners returns the runners of the built binaries, after the
// in-process ones (suite.runners). The untagged cases run in process only;
// the integration-tagged build of the suite returns the runners of the
// built binaries instead (binary_integration_test.go).
func binaryRunners(t testing.TB, _ *suite) []runner {
	t.Helper()
	return nil
}
