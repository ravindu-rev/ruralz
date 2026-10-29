// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !race

package cel

// raceEnabled reports a -race build (see race_test.go).
const raceEnabled = false
