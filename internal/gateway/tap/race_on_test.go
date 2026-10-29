// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package tap

// raceEnabled reports a -race build: sync.Pool then drops items at random,
// so allocation counts are meaningless.
const raceEnabled = true
