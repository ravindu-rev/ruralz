// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package topology

import "os"

// isStop reports SIGSTOP, which only Unix has.
func isStop(os.Signal) bool { return false }

// isCont reports SIGCONT, which only Unix has.
func isCont(os.Signal) bool { return false }
