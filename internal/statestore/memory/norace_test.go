// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !race

package memory

// raceEnabled skips the latency budget, which instrumentation defeats.
const raceEnabled = false
