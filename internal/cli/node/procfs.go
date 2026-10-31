// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

// Fields of a /proc/<pid>/stat line (proc(5)), 1-based.
const (
	// statStateField is the process state, the first field after the
	// parenthesized command name.
	statStateField = 3
	// statStartTimeField is the start time in clock ticks since boot,
	// which holder.json records as startTime.
	statStartTimeField = 22
)

// errStat reports a /proc/<pid>/stat line that does not parse.
var errStat = errors.New("malformed /proc stat line")

// procStat is what ruralz node drain reads from a /proc/<pid>/stat line.
type procStat struct {
	// state is field 3: R running, S sleeping, Z zombie, and so on.
	state byte
	// startTime is field 22, clock ticks since boot, so a reused PID
	// never matches the holder's record.
	startTime uint64
}

// exited reports a process that has exited: a zombie awaiting its
// parent's wait (Z) or one being reaped (X, and x on Linux 2.6.33 to
// 3.13). Such a process serves nothing and holds no lock.
func (s procStat) exited() bool { return s.state == 'Z' || s.state == 'X' || s.state == 'x' }

// parseProcStat parses a /proc/<pid>/stat line. The command name, field 2,
// is parenthesized and may hold spaces and parentheses, so fields are
// counted after the last ')'.
func parseProcStat(line []byte) (procStat, error) {
	i := bytes.LastIndexByte(line, ')')
	if i < 0 {
		return procStat{}, fmt.Errorf("%w: no command name", errStat)
	}
	// fields[0] is field 3, the state.
	fields := bytes.Fields(line[i+1:])
	start := statStartTimeField - statStateField
	if len(fields) <= start {
		return procStat{}, fmt.Errorf("%w: %d fields after the command name", errStat, len(fields))
	}
	if len(fields[0]) != 1 {
		return procStat{}, fmt.Errorf("%w: state %q", errStat, fields[0])
	}
	v, err := strconv.ParseUint(string(fields[start]), 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("%w: start time: %w", errStat, err)
	}
	return procStat{state: fields[0][0], startTime: v}, nil
}

// holdsFlock reports whether locks, the content of /proc/locks, shows a
// granted FLOCK WRITE lock of pid on the file whose inode number is ino:
// a line "<n>: FLOCK  ADVISORY  WRITE <pid> <major>:<minor>:<inode> 0
// EOF". A waiter's line, "<n>: -> FLOCK ...", is no lock, an OFD lock
// shows PID -1, and a lock without an inode shows "<none>:0". The device
// is not compared: a file system may report another device number through
// stat than the one /proc/locks shows.
func holdsFlock(locks []byte, pid int, ino uint64) bool {
	wantPID := strconv.Itoa(pid)
	wantIno := strconv.FormatUint(ino, 10)
	for len(locks) > 0 {
		line := locks
		if j := bytes.IndexByte(locks, '\n'); j >= 0 {
			line, locks = locks[:j], locks[j+1:]
		} else {
			locks = nil
		}
		f := bytes.Fields(line)
		if len(f) < 6 || string(f[1]) != "FLOCK" || string(f[3]) != "WRITE" || string(f[4]) != wantPID {
			continue
		}
		if dev := bytes.Split(f[5], []byte{':'}); len(dev) == 3 && string(dev[2]) == wantIno {
			return true
		}
	}
	return false
}
