// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !race

package logsink

import "testing"

// skipUnderRace skips allocation counts, which the race detector changes.
func skipUnderRace(t *testing.T) { t.Helper() }
