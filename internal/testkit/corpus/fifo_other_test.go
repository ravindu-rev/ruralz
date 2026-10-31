// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package corpus

import "testing"

// mkfifo is a no-op where named pipes do not exist.
func mkfifo(t *testing.T, _ string) { t.Helper() }
