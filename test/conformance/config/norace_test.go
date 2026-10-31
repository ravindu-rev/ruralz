// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !race

package config

// raceEnabled reports a test binary built with -race, whose race detector
// multiplies the memory and time of the hostile loads.
const raceEnabled = false
