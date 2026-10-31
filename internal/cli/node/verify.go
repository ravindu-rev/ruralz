// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
)

// readHolder reads the lock holder's record, holder.json under root, and
// checks its ruralz.holder.v1 format, a positive PID and a node.id
// (nodedir.ReadHolderAt). No record, or one it cannot read, exits 2.
func readHolder(root string) (nodedir.Holder, error) {
	h, err := nodedir.ReadHolderAt(root)
	switch {
	case err == nil:
		return h, nil
	case errors.Is(err, nodedir.ErrNoHolder):
		return nodedir.Holder{}, command.NoResultf("no lock holder recorded in %s", root)
	default:
		return nodedir.Holder{}, command.NoResultf("unreadable holder record in %s: %s", root, holderReason(err))
	}
}

// holderReason returns what nodedir.ReadHolderAt found wrong with a
// record: its message after "unreadable holder record", without the
// package prefixes, so the CLI prints one plain reason.
func holderReason(err error) string {
	msg := err.Error()
	if _, after, ok := strings.Cut(msg, nodedir.ErrHolderInvalid.Error()+": "); ok {
		msg = after
	}
	return strings.TrimPrefix(msg, "nodedir: ")
}

// verifyHolder checks, in order, that the holder runs in this process's PID
// namespace, that a live process has its PID and start time, and that the
// process holds a granted FLOCK WRITE lock on lockPath in /proc/locks
// (docs/reference/01-cli-and-api-surface.md, "Rollout, promotion and Node
// verbs"). The caller opened its handle on the PID first, so a process
// that passes these checks is the one the handle signals. Any other answer
// exits 2 before a signal: a record that names another PID namespace, or
// none, as a holder without /proc writes it; a stale record, whose PID no
// process has, or another process has, or whose process has exited or
// holds no lock; or a /proc read that fails.
func verifyHolder(hs host, h nodedir.Holder, lockPath string) error {
	own, err := hs.pidNamespace()
	switch {
	case err != nil:
		return unverified(h, err)
	case h.PIDNamespace == "":
		return unverified(h, errors.New("its record names no PID namespace"))
	case h.PIDNamespace != own:
		return command.NoResultf("the lock holder (pid %d) runs in another PID namespace (%s, not this process's %s)",
			h.PID, h.PIDNamespace, own)
	}
	line, err := hs.procStat(h.PID)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return stale("no process has pid %d", h.PID)
	case err != nil:
		return unverified(h, err)
	}
	st, err := parseProcStat(line)
	switch {
	case err != nil:
		return unverified(h, fmt.Errorf("/proc/%d/stat: %w", h.PID, err))
	case st.startTime != h.StartTime:
		return stale("pid %d belongs to another process (start time %d, recorded %d)", h.PID, st.startTime, h.StartTime)
	case st.exited():
		return stale("pid %d has exited", h.PID)
	}
	ino, err := hs.inode(lockPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return stale("%s does not exist", lockPath)
	case err != nil:
		return unverified(h, err)
	}
	locks, err := hs.locks()
	if err != nil {
		return unverified(h, err)
	}
	if !holdsFlock(locks, h.PID, ino) {
		return stale("pid %d holds no FLOCK WRITE lock on %s", h.PID, lockPath)
	}
	return nil
}

// processGone reports whether the signaled holder is gone: no process has
// its PID, another process has it (another start time), or it has exited.
// A /proc read or line that fails proves nothing, so the poll goes on and
// --timeout bounds it.
func processGone(hs host, h nodedir.Holder) bool {
	line, err := hs.procStat(h.PID)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	st, err := parseProcStat(line)
	if err != nil {
		return false
	}
	return st.startTime != h.StartTime || st.exited()
}

// stale is the exit 2 of a holder record that names no live lock holder.
func stale(format string, args ...any) error {
	return command.NoResultf("stale holder record: "+format, args...)
}

// unverified is the exit 2 of a check that could not be made.
func unverified(h nodedir.Holder, err error) error {
	return command.NoResultf("cannot verify the lock holder (pid %d): %w", h.PID, err)
}

// lockPath returns the path of the lock file in the data directory root.
func lockPath(root string) string { return filepath.Join(root, filepath.FromSlash(nodedir.LockFile)) }
