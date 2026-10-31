// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package profile

// raceEnabled reports a test binary built with -race, whose race detector
// slows the parse past the time bounds of the hostile inputs.
const raceEnabled = true
