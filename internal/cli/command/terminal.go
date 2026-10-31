// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import "io"

// Colors reports whether human output written to w is colored
// (docs/reference/01-cli-and-api-surface.md "Output formats and exit
// codes"): NO_COLOR is unset or empty, TERM is not dumb, and terminal
// reports that w is a terminal that takes ANSI color; a nil terminal is
// IsTerminal. A file or a pipe never is, and callers never color JSON.
func Colors(sio IO, w io.Writer, terminal func(io.Writer) bool) bool {
	if sio.LookupEnv != nil {
		if v, ok := sio.LookupEnv("NO_COLOR"); ok && v != "" {
			return false
		}
		if v, _ := sio.LookupEnv("TERM"); v == "dumb" {
			return false
		}
	}
	if terminal == nil {
		terminal = IsTerminal
	}
	return terminal(w)
}
