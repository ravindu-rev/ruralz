// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package jsonschema

// raceEnabled reports a -race build: timing bounds are relaxed and
// allocation counts are not compared, since sync.Pool then drops items at
// random.
const raceEnabled = true
