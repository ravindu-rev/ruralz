// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package pluginhost

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"
)

// measureCode returns the bytes of the process's anonymous executable
// mappings, the native code wazero maps for compiled Plugins (the Go
// runtime maps no anonymous executable memory, and the vDSO carries a
// name): the measured side of the compiled-code limit (the wazero study).
func measureCode() (int64, bool) {
	b, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return 0, false
	}
	var n int64
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 5 || len(f[1]) < 3 || f[1][2] != 'x' || f[4] != "0" {
			continue
		}
		lo, hi, ok := strings.Cut(f[0], "-")
		if !ok {
			continue
		}
		s, err1 := strconv.ParseUint(lo, 16, 64)
		e, err2 := strconv.ParseUint(hi, 16, 64)
		if err1 != nil || err2 != nil || e < s {
			continue
		}
		n += int64(e - s) //nolint:gosec // G115: a mapping's size fits int64.
	}
	return n, true
}
