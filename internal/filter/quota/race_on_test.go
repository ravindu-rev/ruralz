// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package quota

// raceEnabled reports a -race build: allocation counts are not compared,
// because sync.Pool drops items at random under the race detector.
const raceEnabled = true
