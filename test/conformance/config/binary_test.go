// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

// binaryRunners returns the runners of the built binaries, after the
// in-process ones (suite.runners). The untagged cases run in process only.
// The integration-tagged cases of the ruralzd wiring and CLI launcher
// steps return here a runner for the built ruralz and one for the
// controllable-clock ruralzd build from internal/testkit/binaries, or for
// the binaries RURALZ_TEST_BIN_DIR names; every case of the suite then
// runs through them too.
func binaryRunners(t testing.TB, _ *suite) []runner {
	t.Helper()
	return nil
}
