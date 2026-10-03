// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build race

package accesslog

import "testing"

// skipUnderRace skips allocation counts: under -race, sync.Pool drops a
// share of the objects put back, so pooled paths allocate.
func skipUnderRace(t *testing.T) {
	t.Helper()
	t.Skip("allocation counts are not meaningful under -race")
}
