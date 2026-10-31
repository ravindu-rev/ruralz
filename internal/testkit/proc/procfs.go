// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ClockTicks is Linux USER_HZ, the unit of the CPU times in
// /proc/<pid>/stat (fixed at 100 by the kernel ABI).
const ClockTicks = 100

// SampleProc reads /proc/<pid>/stat and /proc/<pid>/status under root
// ("/proc" on Linux; a fake tree in tests).
func SampleProc(root string, pid int) (Usage, error) {
	dir := filepath.Join(root, strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(dir, "stat")) //nolint:gosec // G304: a /proc path built from a PID
	if err != nil {
		return Usage{}, fmt.Errorf("proc: sample %d: %w", pid, err)
	}
	status, err := os.ReadFile(filepath.Join(dir, "status")) //nolint:gosec // G304: a /proc path built from a PID
	if err != nil {
		return Usage{}, fmt.Errorf("proc: sample %d: %w", pid, err)
	}
	now := time.Now()
	user, sys, _, err := ParseStat(stat)
	if err != nil {
		return Usage{}, err
	}
	rss, threads, err := ParseStatus(status)
	if err != nil {
		return Usage{}, err
	}
	tick := time.Second / ClockTicks
	return Usage{
		UserCPU:  time.Duration(user) * tick,
		SysCPU:   time.Duration(sys) * tick,
		RSSBytes: rss,
		Threads:  threads,
		Time:     now,
	}, nil
}

// ParseStat parses /proc/<pid>/stat and returns utime and stime in clock
// ticks and num_threads. The command name (field 2) may hold spaces and
// parentheses, so fields are counted after its last ')'.
func ParseStat(data []byte) (utime, stime int64, threads int, err error) {
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return 0, 0, 0, errors.New("proc: stat: no command field")
	}
	f := strings.Fields(string(data[i+1:]))
	// f[0] is field 3 (state); utime is field 14, stime 15, num_threads 20.
	if len(f) < 18 {
		return 0, 0, 0, fmt.Errorf("proc: stat: %d fields after the command, want at least 18", len(f))
	}
	if utime, err = strconv.ParseInt(f[11], 10, 64); err != nil {
		return 0, 0, 0, fmt.Errorf("proc: stat utime: %w", err)
	}
	if stime, err = strconv.ParseInt(f[12], 10, 64); err != nil {
		return 0, 0, 0, fmt.Errorf("proc: stat stime: %w", err)
	}
	if threads, err = strconv.Atoi(f[17]); err != nil {
		return 0, 0, 0, fmt.Errorf("proc: stat num_threads: %w", err)
	}
	return utime, stime, threads, nil
}

// ParseStatus parses /proc/<pid>/status and returns VmRSS in bytes (0
// when absent, as for a zombie) and Threads.
func ParseStatus(data []byte) (rssBytes int64, threads int, err error) {
	seenThreads := false
	for line := range strings.SplitSeq(string(data), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "VmRSS":
			num, unit, _ := strings.Cut(val, " ")
			kb, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
			if err != nil || strings.TrimSpace(unit) != "kB" {
				return 0, 0, fmt.Errorf("proc: status VmRSS %q", val)
			}
			rssBytes = kb * 1024
		case "Threads":
			n, err := strconv.Atoi(val)
			if err != nil {
				return 0, 0, fmt.Errorf("proc: status Threads %q", val)
			}
			threads, seenThreads = n, true
		}
	}
	if !seenThreads {
		return 0, 0, errors.New("proc: status: no Threads line")
	}
	return rssBytes, threads, nil
}
