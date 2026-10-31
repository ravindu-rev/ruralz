// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handover

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// fakeCeiling records the external usage set on a ceiling.
type fakeCeiling struct {
	mu  sync.Mutex
	set []int64
}

func (c *fakeCeiling) SetExternal(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set = append(c.set, n)
}

// last returns the last value set, or -1.
func (c *fakeCeiling) last() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.set) == 0 {
		return -1
	}
	return c.set[len(c.set)-1]
}

// fakeMemory is a soft memory limit and a cgroup memory.max.
type fakeMemory struct {
	mu        sync.Mutex
	limit     int64 // the current limit
	sets      []int64
	cgroupMax int64
	hasCgroup bool
	cgroupErr error
}

func (m *fakeMemory) SetMemoryLimit(n int64) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.limit
	if n >= 0 {
		m.limit = n
		m.sets = append(m.sets, n)
	}
	return prev
}

func (m *fakeMemory) MemoryMax() (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cgroupMax, m.hasCgroup, m.cgroupErr
}

func (m *fakeMemory) current() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limit
}

func (m *fakeMemory) setCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sets)
}

type constReader int64

func (c constReader) Open() int64  { return int64(c) }
func (c constReader) InUse() int64 { return int64(c) }
func (c constReader) Used() int64  { return int64(c) }

// TestMeterUsage checks the usage the holder reports: each reader's value,
// 0 for a missing one, and RSSBytes 0 with the error when the resident set
// is unreadable.
func TestMeterUsage(t *testing.T) {
	m := Meter{
		Conns:        constReader(3),
		Units:        constReader(4),
		Budget:       constReader(5),
		PluginMemory: func() int64 { return 6 },
		RSS:          func() (int64, error) { return 7, nil },
	}
	u, err := m.Usage()
	if want := (Usage{Connections: 3, InflightUnits: 4, BufferedBytes: 5, PluginMemoryBytes: 6, RSSBytes: 7}); err != nil || u != want {
		t.Errorf("Usage() = %+v, %v, want %+v", u, err, want)
	}
	boom := errors.New("boom")
	u, err = Meter{RSS: func() (int64, error) { return 9, boom }}.Usage()
	if !errors.Is(err, boom) || u != (Usage{}) {
		t.Errorf("Usage() with an unreadable resident set = %+v, %v, want zero usage and the error", u, err)
	}
}

// TestSharedCeilings checks the new process's shared ceilings: each
// ceiling lowered by the holder's usage, never below 0; the soft memory
// limit at 90% of the cgroup's memory.max minus the holder's resident
// set, else GOMEMLIMIT minus it, else unchanged, never below 0; and on
// restore every ceiling and the process's own limit back.
func TestSharedCeilings(t *testing.T) {
	const gib = 1 << 30
	tests := []struct {
		name      string
		own       int64 // the process's own limit; math.MaxInt64 means none
		cgroupMax int64
		hasCgroup bool
		cgroupErr error
		rss       int64
		want      int64 // -1: the limit is left alone
	}{
		{name: "cgroup limit", own: math.MaxInt64, cgroupMax: 10 * gib, hasCgroup: true, rss: 2 * gib, want: 9*gib - 2*gib},
		{name: "cgroup limit beside GOMEMLIMIT", own: 4 * gib, cgroupMax: 10 * gib, hasCgroup: true, rss: 2 * gib, want: 9*gib - 2*gib},
		{name: "GOMEMLIMIT", own: 8 * gib, rss: 3 * gib, want: 5 * gib},
		{name: "unreadable cgroup counts as none", own: 8 * gib, cgroupErr: errors.New("boom"), rss: 3 * gib, want: 5 * gib},
		{name: "neither", own: math.MaxInt64, rss: 3 * gib, want: -1},
		{name: "resident set above the share", own: math.MaxInt64, cgroupMax: gib, hasCgroup: true, rss: 2 * gib, want: 0},
		{name: "negative resident set", own: 8 * gib, rss: -5, want: 8 * gib},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem := &fakeMemory{limit: tt.own, cgroupMax: tt.cgroupMax, hasCgroup: tt.hasCgroup, cgroupErr: tt.cgroupErr}
			conns, units, budget, plugins := &fakeCeiling{}, &fakeCeiling{}, &fakeCeiling{}, &fakeCeiling{}
			s := newShared(Ceilings{Conns: conns, Units: units, Budget: budget, PluginMemory: plugins, SetMemoryLimit: mem.SetMemoryLimit, MemoryMax: mem.MemoryMax})
			err := s.apply(Usage{Connections: 10, InflightUnits: 20, BufferedBytes: 30, PluginMemoryBytes: -1, RSSBytes: tt.rss})
			if !errors.Is(err, tt.cgroupErr) {
				t.Errorf("apply error = %v, want %v", err, tt.cgroupErr)
			}
			for name, c := range map[string]*fakeCeiling{"connections": conns, "units": units, "budget": budget, "plugin memory": plugins} {
				want := map[string]int64{"connections": 10, "units": 20, "budget": 30, "plugin memory": 0}[name]
				if got := c.last(); got != want {
					t.Errorf("%s external = %d, want %d", name, got, want)
				}
			}
			switch got := mem.current(); {
			case tt.want < 0 && mem.setCount() != 0:
				t.Errorf("memory limit set to %d, want it left alone", got)
			case tt.want >= 0 && got != tt.want:
				t.Errorf("memory limit = %d, want %d", got, tt.want)
			}
			s.restore()
			for name, c := range map[string]*fakeCeiling{"connections": conns, "units": units, "budget": budget, "plugin memory": plugins} {
				if got := c.last(); got != 0 {
					t.Errorf("%s external after restore = %d, want 0", name, got)
				}
			}
			if got := mem.current(); got != tt.own {
				t.Errorf("memory limit after restore = %d, want the own %d", got, tt.own)
			}
		})
	}
}

// TestSharedRestoreWithoutUsage checks that restore changes nothing
// before a usage report, and that a second report keeps the process's
// own limit from before the first.
func TestSharedRestoreWithoutUsage(t *testing.T) {
	mem := &fakeMemory{limit: 1000}
	conns := &fakeCeiling{}
	s := newShared(Ceilings{Conns: conns, SetMemoryLimit: mem.SetMemoryLimit, MemoryMax: mem.MemoryMax})
	s.restore()
	if conns.last() != -1 || mem.setCount() != 0 {
		t.Fatalf("restore before any usage changed something: ceiling %v, limits %v", conns.set, mem.sets)
	}
	_ = s.apply(Usage{RSSBytes: 100})
	_ = s.apply(Usage{RSSBytes: 300})
	if got := mem.current(); got != 700 {
		t.Errorf("limit after two reports = %d, want 1000 - 300", got)
	}
	s.restore()
	if got := mem.current(); got != 1000 {
		t.Errorf("limit after restore = %d, want 1000", got)
	}
	if want := []int64{900, 700, 1000}; !slices.Equal(mem.sets, want) {
		t.Errorf("limits set = %v, want %v", mem.sets, want)
	}
}

// TestParseStatm checks the resident set read from /proc/self/statm.
func TestParseStatm(t *testing.T) {
	if got, err := parseStatm([]byte("1000 250 30 4 0 60 0\n"), 4096); err != nil || got != 250*4096 {
		t.Errorf("parseStatm = %d, %v, want %d", got, err, 250*4096)
	}
	for _, bad := range []string{"", "1000", "1000 x", "1000 -1"} {
		if _, err := parseStatm([]byte(bad), 4096); err == nil {
			t.Errorf("parseStatm(%q) succeeded", bad)
		}
	}
}

// TestMemoryMaxAt checks the cgroup v2 memory.max read: the "0::" path
// of /proc/self/cgroup under the cgroup root, "max" as none, no limit
// for a process in no cgroup v2 or without the files, and an error for
// an unparsable value.
func TestMemoryMaxAt(t *testing.T) {
	root := t.TempDir()
	unit := filepath.Join(root, "system.slice", "ruralzd.service")
	if err := os.MkdirAll(unit, 0o750); err != nil {
		t.Fatal(err)
	}
	write := func(path, s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	proc := filepath.Join(root, "cgroup")
	tests := []struct {
		name    string
		proc    string // "" leaves no /proc file
		max     string // "" leaves no memory.max
		want    int64
		has     bool
		wantErr bool
	}{
		{name: "limit", proc: "0::/system.slice/ruralzd.service\n", max: "1073741824\n", want: 1 << 30, has: true},
		{name: "max", proc: "0::/system.slice/ruralzd.service\n", max: "max\n"},
		{name: "hybrid hierarchy", proc: "12:memory:/x\n0::/system.slice/ruralzd.service\n", max: "2048\n", want: 2048, has: true},
		{name: "cgroup v1 only", proc: "4:memory:/system.slice/ruralzd.service\n", max: "2048\n"},
		{name: "no memory.max", proc: "0::/system.slice/ruralzd.service\n"},
		{name: "no /proc file"},
		{name: "unparsable", proc: "0::/system.slice/ruralzd.service\n", max: "lots\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Remove(proc)
			_ = os.Remove(filepath.Join(unit, "memory.max"))
			if tt.proc != "" {
				write(proc, tt.proc)
			}
			if tt.max != "" {
				write(filepath.Join(unit, "memory.max"), tt.max)
			}
			got, has, err := memoryMaxAt(proc, root)
			if (err != nil) != tt.wantErr || got != tt.want || has != tt.has {
				t.Errorf("memoryMaxAt = %d, %t, %v, want %d, %t, error %t", got, has, err, tt.want, tt.has, tt.wantErr)
			}
		})
	}
}
