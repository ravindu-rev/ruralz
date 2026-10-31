// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package schemaview

// raceEnabled reports a test binary built with -race, whose race detector
// slows the scale documents past the package test timeout under a full
// parallel run; the run without it checks them.
const raceEnabled = true
