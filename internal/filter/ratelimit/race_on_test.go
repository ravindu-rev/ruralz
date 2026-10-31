// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package ratelimit

// raceEnabled reports a -race build: allocation counts are not compared,
// because sync.Pool and cel-go's pools drop items at random under the race
// detector.
const raceEnabled = true
