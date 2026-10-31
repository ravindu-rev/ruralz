// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package header

// raceEnabled reports a -race build: allocation counts that include a CEL
// evaluation are not compared, because cel-go's evaluation pools drop
// items at random under the race detector.
const raceEnabled = true
