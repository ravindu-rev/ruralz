// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !race

package overlay

// raceEnabled reports a test binary built with -race. The race detector
// slows the scale, memory and diagnostic budget tests about ten times, so
// they run at a smaller size then (raceScaled), while the build without it
// runs them at full size (review finding: under -race, beside other
// packages, the package came close to the default go test timeout).
const raceEnabled = false
