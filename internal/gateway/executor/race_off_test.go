// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !race

package executor

// raceEnabled reports a -race build (see race_on_test.go).
const raceEnabled = false
