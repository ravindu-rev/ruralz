// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"io"
	"os"
	"syscall"
)

// enableVirtualTerminalProcessing is the console mode flag under which a
// Windows console interprets ANSI escape sequences.
const enableVirtualTerminalProcessing = 0x0004

// isTerminal reports whether w is a console with virtual terminal
// processing, the only Windows console that takes ANSI color: an open file
// whose handle has a console mode with that flag. A pipe or a regular file
// has no console mode.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || f == nil {
		return false
	}
	var mode uint32
	if err := syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode); err != nil {
		return false
	}
	return mode&enableVirtualTerminalProcessing != 0
}
