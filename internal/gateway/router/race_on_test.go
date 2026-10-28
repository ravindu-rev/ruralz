// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package router

// raceEnabled reports a -race build: sync.Pool, which regexp uses for its
// machines, then drops items at random, so allocation counts are
// meaningless (the standard library skips its AllocsPerRun tests the same
// way).
const raceEnabled = true
