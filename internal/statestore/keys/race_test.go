// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package keys

// raceEnabled skips allocation checks that sync.Pool defeats under -race.
const raceEnabled = true
