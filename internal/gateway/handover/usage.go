// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handover

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
)

// MemorySharePercent is the share of the cgroup's memory.max, in percent,
// under which a new process sets its soft memory limit while the holder
// runs, minus the holder's resident set (target).
const MemorySharePercent = 90

// Meter reads the holder's usage of the Node-wide ceilings for accepted
// and usage messages. A nil reader reports 0.
type Meter struct {
	// Conns are the client connections open (admission.ConnLimiter.Open).
	Conns interface{ Open() int64 }
	// Units are the in-flight units held (admission.Units.InUse).
	Units interface{ InUse() int64 }
	// Budget is the buffered bytes reserved (body.Budget.Used).
	Budget interface{ Used() int64 }
	// PluginMemory returns the Plugin memory held, Planned (M2).
	PluginMemory func() int64
	// RSS returns the process's resident set size; nil reads
	// /proc/self/statm, 0 where that does not exist.
	RSS func() (int64, error)
}

// Usage returns the usage now; an error reading the resident set leaves
// RSSBytes 0.
func (m Meter) Usage() (Usage, error) {
	var u Usage
	if m.Conns != nil {
		u.Connections = m.Conns.Open()
	}
	if m.Units != nil {
		u.InflightUnits = m.Units.InUse()
	}
	if m.Budget != nil {
		u.BufferedBytes = m.Budget.Used()
	}
	if m.PluginMemory != nil {
		u.PluginMemoryBytes = m.PluginMemory()
	}
	rss := m.RSS
	if rss == nil {
		rss = residentBytes
	}
	n, err := rss()
	if err != nil {
		return u, err
	}
	u.RSSBytes = n
	return u, nil
}

// Ceilings are the new process's Node-wide ceilings, each lowered by the
// holder's reported usage from accepted until the holder is gone, and its
// soft memory limit (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
// "In-place handover"). A nil Ceiling is left alone.
type Ceilings struct {
	// Conns is the client connection ceiling (admission.ConnLimiter).
	Conns Ceiling
	// Units is the in-flight unit ceiling (admission.Units).
	Units Ceiling
	// Budget is limits.maxBufferedBytes (body.Budget).
	Budget Ceiling
	// PluginMemory is limits.maxPluginMemoryBytes, Planned (M2).
	PluginMemory Ceiling
	// SetMemoryLimit sets the soft memory limit and returns the previous
	// one, a negative value only reading it; nil means
	// debug.SetMemoryLimit.
	SetMemoryLimit func(int64) int64
	// MemoryMax returns memory.max of the process's cgroup, false when it
	// sets none; nil reads it through /proc/self/cgroup and
	// /sys/fs/cgroup (cgroup v2).
	MemoryMax func() (int64, bool, error)
}

// shared applies the holder's usage to Ceilings. It is safe for
// concurrent use.
type shared struct {
	c Ceilings

	mu sync.Mutex
	// own is the soft memory limit before the first usage report, which
	// restore sets again; valid while saved.
	own   int64
	saved bool
	// applied is set from the first usage report until restore.
	applied bool
}

// newShared returns the ceilings of c, with the defaults filled in.
func newShared(c Ceilings) *shared {
	if c.SetMemoryLimit == nil {
		c.SetMemoryLimit = debug.SetMemoryLimit
	}
	if c.MemoryMax == nil {
		c.MemoryMax = cgroupMemoryMax
	}
	return &shared{c: c}
}

// apply lowers each ceiling by u and sets the soft memory limit: the
// share of the cgroup's memory.max, or without a cgroup limit the
// process's own limit (GOMEMLIMIT), minus the holder's resident set; with
// neither it keeps the limit. A memory.max that cannot be read counts as
// none and is returned.
func (s *shared) apply(u Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = true
	lower(s.c.Conns, u.Connections)
	lower(s.c.Units, u.InflightUnits)
	lower(s.c.Budget, u.BufferedBytes)
	lower(s.c.PluginMemory, u.PluginMemoryBytes)
	if !s.saved {
		s.own, s.saved = s.c.SetMemoryLimit(-1), true
	}
	limit, ok, err := s.memoryLimit(max(0, u.RSSBytes))
	if ok {
		s.c.SetMemoryLimit(limit)
	}
	return err
}

// memoryLimit returns the soft memory limit for the holder's resident set
// rss, and false to keep the limit.
func (s *shared) memoryLimit(rss int64) (int64, bool, error) {
	cgroupMax, has, err := s.c.MemoryMax()
	switch {
	case err == nil && has:
		return max(0, share(cgroupMax)-rss), true, nil
	case s.own != math.MaxInt64:
		return max(0, s.own-rss), true, err
	default:
		return 0, false, err
	}
}

// share returns MemorySharePercent of n, rounded down, without
// overflowing.
func share(n int64) int64 {
	return n/100*MemorySharePercent + n%100*MemorySharePercent/100
}

// restore sets every ceiling back to its full value and the soft memory
// limit back to the process's own, once the holder is gone.
func (s *shared) restore() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.applied {
		return
	}
	s.applied = false
	lower(s.c.Conns, 0)
	lower(s.c.Units, 0)
	lower(s.c.Budget, 0)
	lower(s.c.PluginMemory, 0)
	if s.saved {
		s.c.SetMemoryLimit(s.own)
		s.saved = false
	}
}

// lower sets c's external usage to n, never below 0.
func lower(c Ceiling, n int64) {
	if c != nil {
		c.SetExternal(max(0, n))
	}
}

// parseStatm returns the resident set of a /proc/<pid>/statm line in
// bytes: its second field counts pages (proc(5)).
func parseStatm(b []byte, pageSize int) (int64, error) {
	f := bytes.Fields(b)
	if len(f) < 2 {
		return 0, fmt.Errorf("handover: statm has %d fields, want at least 2", len(f))
	}
	pages, err := strconv.ParseInt(string(f[1]), 10, 64)
	if err != nil || pages < 0 || pages > math.MaxInt64/int64(max(1, pageSize)) {
		return 0, fmt.Errorf("handover: statm resident %q is not a page count", f[1])
	}
	return pages * int64(pageSize), nil
}

// memoryMaxAt returns memory.max of the cgroup v2 that the
// /proc/<pid>/cgroup file at procCgroup names, under the cgroup root
// (/sys/fs/cgroup); false when the cgroup sets none ("max"), the process
// is in no cgroup v2, or the file does not exist.
func memoryMaxAt(procCgroup, root string) (int64, bool, error) {
	data, err := os.ReadFile(procCgroup) //nolint:gosec // G304: /proc/self/cgroup, or a test's file.
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("handover: %w", err)
	}
	path, ok := unifiedPath(data)
	if !ok {
		return 0, false, nil
	}
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path), "memory.max")) //nolint:gosec // G304: the kernel's cgroup path under the cgroup root.
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("handover: %w", err)
	}
	return parseMemoryMax(b)
}

// unifiedPath returns the cgroup v2 path of a /proc/<pid>/cgroup file:
// the path of its "0::" line.
func unifiedPath(data []byte) (string, bool) {
	for l := range strings.SplitSeq(string(data), "\n") {
		if path, ok := strings.CutPrefix(l, "0::"); ok && strings.HasPrefix(path, "/") {
			return path, true
		}
	}
	return "", false
}

// parseMemoryMax parses a memory.max file: a byte count, or "max" for
// none.
func parseMemoryMax(b []byte) (int64, bool, error) {
	s := string(bytes.TrimSpace(b))
	if s == "max" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false, fmt.Errorf("handover: memory.max %q is not a byte count", s)
	}
	return n, true, nil
}
