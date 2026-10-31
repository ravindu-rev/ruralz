// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package nodedir

import "testing"

// checkProcIdentity: without /proc the start time and PID namespace stay
// empty.
func checkProcIdentity(t *testing.T, h Holder) {
	t.Helper()
	if h.StartTime != 0 || h.PIDNamespace != "" {
		t.Errorf("NewHolder = %+v, want no /proc members", h)
	}
}

// verifyHolderProcess has nothing to verify without /proc.
func verifyHolderProcess(*testing.T, *Dir, Holder) {}
