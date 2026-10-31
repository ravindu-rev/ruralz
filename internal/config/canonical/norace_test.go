// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !race

package canonical

// raceEnabled reports a test binary built with -race, whose race detector
// slows the decodes of the memory tests tenfold and changes their
// allocations; the run without it measures them.
const raceEnabled = false
