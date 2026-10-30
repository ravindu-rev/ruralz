// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build t5 && linux

package t5

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// capNetBindService is CAP_NET_BIND_SERVICE's bit number
// (capabilities(7)).
const capNetBindService = 10

// proc identifies one process: its PID and its start time, so a PID the
// kernel reused names another process.
type proc struct {
	PID   int
	Start uint64
}

// String formats p for messages.
func (p proc) String() string { return fmt.Sprintf("process %d", p.PID) }

// statFields returns the fields of /proc/<pid>/stat content after the
// command name, which may hold spaces and parentheses: fields[0] is the
// state, field 3 of proc(5).
func statFields(stat []byte) ([]string, error) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return nil, errors.New("no command name in /proc/<pid>/stat")
	}
	return strings.Fields(string(stat[i+1:])), nil
}

// parseStat returns the state and the start time, fields 3 and 22 of
// proc(5), of /proc/<pid>/stat content.
func parseStat(stat []byte) (state string, start uint64, err error) {
	f, err := statFields(stat)
	if err != nil {
		return "", 0, err
	}
	// fields[0] is field 3, so field 22 is fields[19].
	if len(f) < 20 {
		return "", 0, fmt.Errorf("/proc/<pid>/stat has %d fields after the command name, want 20 or more", len(f))
	}
	start, err = strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("/proc/<pid>/stat start time %q: %w", f[19], err)
	}
	return f[0], start, nil
}

// lookupProc returns the live process pid; ok is false when none runs
// with it or it is a zombie, whose exit only its parent has yet to reap.
func lookupProc(pid int) (p proc, ok bool) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")) //nolint:gosec // G304: a procfs path built from a PID
	if err != nil {
		return proc{}, false
	}
	state, start, err := parseStat(b)
	if err != nil || state == "Z" || state == "X" {
		return proc{}, false
	}
	return proc{PID: pid, Start: start}, true
}

// alive reports whether p still runs: its PID names a live process with
// its start time.
func (p proc) alive() bool {
	q, ok := lookupProc(p.PID)
	return ok && q.Start == p.Start
}

// kill sends sig to p when it still runs.
func (p proc) kill(sig syscall.Signal) {
	if p.alive() {
		_ = syscall.Kill(p.PID, sig)
	}
}

// parseStatus returns the "Key:\tvalue" lines of /proc/<pid>/status
// content.
func parseStatus(status []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

// procStatus returns /proc/<pid>/status.
func procStatus(pid int) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status")) //nolint:gosec // G304: a procfs path built from a PID
	if err != nil {
		return nil, err
	}
	return parseStatus(b), nil
}

// capMask parses a capability set of /proc/<pid>/status, 16 hexadecimal
// digits.
func capMask(hex string) (uint64, error) {
	v, err := strconv.ParseUint(hex, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("capability set %q: %w", hex, err)
	}
	return v, nil
}

// capProblems returns the capability sets of /proc/<pid>/status that hold
// anything but CAP_NET_BIND_SERVICE, and the effective, permitted,
// bounding and ambient sets that lack it: the T5 unit grants it through
// AmbientCapabilities= and bounds the process to it with
// CapabilityBoundingSet=, so the Node holds it and nothing else.
func capProblems(status map[string]string) []string {
	const only = uint64(1) << capNetBindService
	var problems []string
	for _, set := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		v, err := capMask(status[set])
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", set, err))
		case v&^only != 0:
			problems = append(problems, fmt.Sprintf("%s = %016x holds capabilities besides CAP_NET_BIND_SERVICE", set, v))
		case v != only && set != "CapInh":
			problems = append(problems, fmt.Sprintf("%s = %016x lacks CAP_NET_BIND_SERVICE", set, v))
		}
	}
	return problems
}

// controlGroupProcs returns the PIDs in the cgroup systemd names in a
// unit's ControlGroup= property, read from cgroup v2 or, on a hybrid
// host, the unified or name=systemd hierarchy.
func controlGroupProcs(cgroup string) ([]int, error) {
	var errs []error
	for _, base := range []string{"/sys/fs/cgroup", "/sys/fs/cgroup/unified", "/sys/fs/cgroup/systemd"} {
		b, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(cgroup), "cgroup.procs")) //nolint:gosec // G304: a cgroupfs path systemd reports
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var pids []int
		for f := range strings.FieldsSeq(string(b)) {
			pid, err := strconv.Atoi(f)
			if err != nil {
				return nil, fmt.Errorf("cgroup.procs of %s: %w", cgroup, err)
			}
			pids = append(pids, pid)
		}
		return pids, nil
	}
	return nil, fmt.Errorf("no cgroup.procs for %s: %w", cgroup, errors.Join(errs...))
}
