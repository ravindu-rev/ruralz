// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !race

package schemaview

// raceEnabled reports a test binary built with -race (see race_test.go).
const raceEnabled = false
