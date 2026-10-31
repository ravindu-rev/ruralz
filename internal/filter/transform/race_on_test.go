// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package transform

// raceEnabled reports a -race build: allocation counts are not compared,
// because sync.Pool, which keeps a Filter's scratch, drops items at random
// under the race detector, as cel-go's evaluation pools do.
const raceEnabled = true
