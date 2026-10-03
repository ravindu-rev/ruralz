// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nodedir

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// checkProcIdentity checks NewHolder against /proc/self (spec 10
// requirement 95): startTime is field 22 of the stat line, pidNamespace
// the ns/pid link.
func checkProcIdentity(t *testing.T, h Holder) {
	t.Helper()
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Skipf("no /proc: %v", err)
	}
	want, err := parseStatStartTime(data)
	if err != nil {
		t.Fatal(err)
	}
	if h.StartTime != want || want == 0 {
		t.Errorf("StartTime = %d, want %d", h.StartTime, want)
	}
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Logf("ns/pid unreadable: %v", err)
		return
	}
	if h.PIDNamespace != ns || !strings.HasPrefix(ns, "pid:[") {
		t.Errorf("PIDNamespace = %q, want %q", h.PIDNamespace, ns)
	}
}

// verifyHolderProcess performs the checks ruralz node drain makes (spec
// 10 requirement 96, steps 2, 4 and 5) against a live helper: same PID
// namespace, matching start time, and a BSD FLOCK WRITE line in
// /proc/locks for its PID and the lock file's inode (never an OFD lock,
// whose PID reads -1).
func verifyHolderProcess(t *testing.T, d *Dir, h Holder) {
	t.Helper()
	if ns, err := os.Readlink("/proc/self/ns/pid"); err == nil && h.PIDNamespace != ns {
		t.Errorf("pidNamespace = %q, want %q", h.PIDNamespace, ns)
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", h.PID))
	if err != nil {
		t.Fatalf("holder process stat: %v", err)
	}
	if st, err := parseStatStartTime(stat); err != nil || st != h.StartTime {
		t.Errorf("stat start time = %d, %v; holder.json says %d", st, err, h.StartTime)
	}
	fi, err := os.Stat(d.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	ino := fi.Sys().(*syscall.Stat_t).Ino
	locks, err := os.ReadFile("/proc/locks")
	if err != nil {
		t.Skipf("no /proc/locks: %v", err)
	}
	if !hasFlockLine(locks, h.PID, ino) {
		t.Errorf("/proc/locks has no FLOCK WRITE line for pid %d inode %d:\n%s", h.PID, ino, locks)
	}
}

// hasFlockLine reports whether /proc/locks holds a granted (not "->")
// FLOCK WRITE lock of pid on inode ino.
func hasFlockLine(locks []byte, pid int, ino uint64) bool {
	sc := bufio.NewScanner(bytes.NewReader(locks))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 6 || f[1] == "->" || f[1] != "FLOCK" || f[3] != "WRITE" || f[4] != strconv.Itoa(pid) {
			continue
		}
		parts := strings.Split(f[5], ":")
		if len(parts) == 3 && parts[2] == strconv.FormatUint(ino, 10) {
			return true
		}
	}
	return false
}

// TestHasFlockLine covers the /proc/locks reader used above.
func TestHasFlockLine(t *testing.T) {
	locks := []byte("1: FLOCK  ADVISORY  WRITE 42 00:2e:1001 0 EOF\n" +
		"1: -> FLOCK  ADVISORY  WRITE 43 00:2e:1002 0 EOF\n" +
		"2: OFDLCK ADVISORY  WRITE -1 00:2e:1003 0 EOF\n" +
		"3: FLOCK  ADVISORY  READ 44 fd:01:1004 0 EOF\n")
	for _, tc := range []struct {
		pid  int
		ino  uint64
		want bool
	}{{42, 1001, true}, {42, 1002, false}, {43, 1002, false}, {-1, 1003, false}, {44, 1004, false}} {
		if got := hasFlockLine(locks, tc.pid, tc.ino); got != tc.want {
			t.Errorf("hasFlockLine(%d, %d) = %v, want %v", tc.pid, tc.ino, got, tc.want)
		}
	}
}
