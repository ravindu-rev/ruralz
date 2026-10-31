// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package cel

// raceEnabled reports a -race build: allocation counts are not compared,
// because cel-go's evaluation pools drop items at random under the race
// detector.
const raceEnabled = true
