// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package nodedir

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

// statStartTimeField is the 1-based field of /proc/<pid>/stat holding the
// start time in clock ticks since boot (proc(5)).
const statStartTimeField = 22

// errStat reports a /proc/<pid>/stat line that does not parse.
var errStat = errors.New("nodedir: malformed /proc stat line")

// parseStatStartTime returns field 22 of a /proc/<pid>/stat line. The
// command name (field 2) is parenthesized and may hold spaces and
// parentheses, so fields are counted after the last ')'.
func parseStatStartTime(line []byte) (uint64, error) {
	i := bytes.LastIndexByte(line, ')')
	if i < 0 {
		return 0, fmt.Errorf("%w: no command name", errStat)
	}
	// Field 3 (state) is the first after the command name.
	fields := bytes.Fields(line[i+1:])
	idx := statStartTimeField - 3
	if len(fields) <= idx {
		return 0, fmt.Errorf("%w: %d fields after the command name", errStat, len(fields))
	}
	v, err := strconv.ParseUint(string(fields[idx]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: start time: %w", errStat, err)
	}
	return v, nil
}
