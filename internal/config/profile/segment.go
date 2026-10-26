// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import "strings"

// markerLine reports whether line is a document marker: "---" or "..." at
// column 1 followed by a space, a tab or the end of the line
// (c-forbidden). Such a line ends any scalar, and "...x" or "---x" is
// content.
func markerLine(line, marker string) bool {
	if !strings.HasPrefix(line, marker) {
		return false
	}
	rest := line[len(marker):]
	return rest == "" || rest[0] == ' ' || rest[0] == '\t'
}
