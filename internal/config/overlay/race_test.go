// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package overlay

// raceEnabled reports a test binary built with -race. The race detector
// slows the scale, memory and diagnostic budget tests about ten times, so
// they run at a smaller size then (raceScaled), which keeps the package
// well inside the default go test timeout beside other packages, while
// the build without it runs them at full size.
const raceEnabled = true
