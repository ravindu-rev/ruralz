// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package bundle

import (
	"io"
	"os"
)

// isTerminal reports whether w is a terminal: an open file that is a
// character device, as a terminal is and a pipe or a regular file never
// is. Whether it takes ANSI color is TERM's to say (invocation.colors).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
