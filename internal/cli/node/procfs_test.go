// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"errors"
	"testing"
)

// TestParseProcStat reads field 3 (state) and field 22 (start time),
// counting fields after the last ')' of the command name.
func TestParseProcStat(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		want       procStat
	}{
		{"plain", statLine(4242, "ruralzd", 'S', 1234567), procStat{state: 'S', startTime: 1234567}},
		{"zombie", statLine(1, "init", 'Z', 3), procStat{state: 'Z', startTime: 3}},
		{"spaces and parentheses in the name", statLine(7, "a) Z 1 (b", 'R', 99), procStat{state: 'R', startTime: 99}},
		{"empty name", statLine(7, "", 'I', 0), procStat{state: 'I', startTime: 0}},
		{"no trailing newline", "9 (x) S 1 9 9 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 42", procStat{state: 'S', startTime: 42}},
	} {
		got, err := parseProcStat([]byte(tc.line))
		if err != nil || got != tc.want {
			t.Errorf("parseProcStat(%s) = %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ name, line string }{
		{"empty", ""},
		{"no command name", "4242 ruralzd S 1"},
		{"too few fields", "9 (x) S 1 9 9 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0"},
		{"start time not a number", "9 (x) S 1 9 9 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 4x2"},
		{"negative start time", "9 (x) S 1 9 9 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 -42"},
		{"state of two letters", "9 (x) SS 1 9 9 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 42"},
	} {
		if got, err := parseProcStat([]byte(tc.line)); !errors.Is(err, errStat) {
			t.Errorf("parseProcStat(%s) = %+v, %v; want errStat", tc.name, got, err)
		}
	}
}

// TestExited: zombie and dead states have exited; the others run.
func TestExited(t *testing.T) {
	for _, s := range []byte("RSDTtWPIK") {
		if (procStat{state: s}).exited() {
			t.Errorf("state %c counts as exited", s)
		}
	}
	for _, s := range []byte("ZXx") {
		if !(procStat{state: s}).exited() {
			t.Errorf("state %c does not count as exited", s)
		}
	}
}

// TestHoldsFlock: only a granted FLOCK WRITE line of the PID on the inode
// counts; the device is not compared.
func TestHoldsFlock(t *testing.T) {
	const locks = "1: POSIX  ADVISORY  WRITE 42 08:01:7864448 0 EOF\n" +
		"2: FLOCK  ADVISORY  WRITE 42 00:2e:1001 0 EOF\n" +
		"2: -> FLOCK  ADVISORY  WRITE 43 00:2e:1001 0 EOF\n" +
		"3: OFDLCK ADVISORY  WRITE -1 00:2e:1003 0 EOF\n" +
		"4: FLOCK  ADVISORY  READ  44 fd:01:1004 0 EOF\n" +
		"5: FLOCK  ADVISORY  WRITE 45 103:02:1005 0 EOF\n" +
		"6: LEASE  ACTIVE    READ  46 00:2e:1006 0 EOF\n" +
		"7: FLOCK  ADVISORY  WRITE 47 <none>:0 0 EOF\n" +
		"8: FLOCK  ADVISORY  WRITE 48 00:2e:1008 0 EOF"
	for _, tc := range []struct {
		name string
		pid  int
		ino  uint64
		want bool
	}{
		{"granted", 42, 1001, true},
		{"POSIX lock", 42, 7864448, false},
		{"waiter", 43, 1001, false},
		{"OFD lock", -1, 1003, false},
		{"read lock", 44, 1004, false},
		{"another device", 45, 1005, true},
		{"lease", 46, 1006, false},
		{"no inode", 47, 0, false},
		{"last line without newline", 48, 1008, true},
		{"other inode", 42, 1002, false},
		{"other PID", 49, 1001, false},
		{"PID prefix", 4, 1001, false},
	} {
		if got := holdsFlock([]byte(locks), tc.pid, tc.ino); got != tc.want {
			t.Errorf("holdsFlock(%s: pid %d, inode %d) = %v, want %v", tc.name, tc.pid, tc.ino, got, tc.want)
		}
	}
	if holdsFlock(nil, 42, 1001) {
		t.Error("holdsFlock of empty /proc/locks = true")
	}
}
