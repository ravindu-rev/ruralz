// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package celtypes

// raceEnabled reports a -race build: sync.Pool drops items at random under
// the race detector, so cel-go's pooled frames make allocation counts of a
// whole evaluation noisy.
const raceEnabled = true
