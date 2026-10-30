// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Sysctls are the kernel settings every record names (PBB "Reference
// hardware" settings baseline and "Reproducibility").
func Sysctls() []string {
	return []string{
		"net.ipv4.ip_local_port_range", "net.core.somaxconn", "net.ipv4.tcp_max_syn_backlog",
		"net.core.netdev_max_backlog", "net.ipv4.tcp_tw_reuse", "vm.swappiness",
		"kernel.sched_autogroup_enabled", "vm.overcommit_memory",
	}
}

// CPUFacts is one CPU's frequency and throttle state.
type CPUFacts struct {
	CPU      int    `json:"cpu"`
	Governor string `json:"governor"`
	// MinKHz, MaxKHz and CurKHz are scaling_min_freq, scaling_max_freq
	// and scaling_cur_freq.
	MinKHz int64 `json:"minKHz"`
	MaxKHz int64 `json:"maxKHz"`
	CurKHz int64 `json:"curKHz"`
	// CoreThrottle and PackageThrottle are the thermal throttle counters.
	CoreThrottle    int64 `json:"coreThrottle"`
	PackageThrottle int64 `json:"packageThrottle"`
}

// HostFacts is a host's fingerprint and the state the pre-run host check
// reads.
type HostFacts struct {
	Hostname          string            `json:"hostname"`
	Kernel            string            `json:"kernel"`
	Microarchitecture string            `json:"microarchitecture"`
	SMT               string            `json:"smt"`
	THP               string            `json:"thp"`
	Cpuset            string            `json:"cpuset"`
	Sysctls           map[string]string `json:"sysctls"`
	// NIC holds each interface's speed and MTU, and the qdisc-free
	// settings sysfs shows.
	NIC  map[string]string `json:"nic"`
	CPUs []CPUFacts        `json:"cpus"`
}

// ReadHostFacts reads the facts under root ("/" on a host; a fake tree in
// tests). Missing files leave their facts empty.
func ReadHostFacts(root string) HostFacts {
	read := func(parts ...string) string {
		b, err := os.ReadFile(filepath.Join(append([]string{root}, parts...)...)) //nolint:gosec // G304: sysfs and procfs paths under the given root
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	f := HostFacts{
		Hostname: read("proc", "sys", "kernel", "hostname"),
		Kernel:   read("proc", "sys", "kernel", "osrelease"),
		SMT:      read("sys", "devices", "system", "cpu", "smt", "active"),
		THP:      selected(read("sys", "kernel", "mm", "transparent_hugepage", "enabled")),
		Sysctls:  map[string]string{},
		NIC:      map[string]string{},
	}
	f.Microarchitecture = cpuModel(read("proc", "cpuinfo"))
	f.Cpuset = statusField(read("proc", "self", "status"), "Cpus_allowed_list")
	for _, k := range Sysctls() {
		if v := read(append([]string{"proc", "sys"}, strings.Split(k, ".")...)...); v != "" {
			f.Sysctls[k] = strings.Join(strings.Fields(v), " ")
		}
	}
	if ents, err := os.ReadDir(filepath.Join(root, "sys", "class", "net")); err == nil {
		for _, e := range ents {
			name := e.Name()
			if name == "lo" {
				continue
			}
			speed, mtu := read("sys", "class", "net", name, "speed"), read("sys", "class", "net", name, "mtu")
			f.NIC[name] = "speed=" + speed + " mtu=" + mtu
		}
	}
	cpuDir := filepath.Join(root, "sys", "devices", "system", "cpu")
	if ents, err := os.ReadDir(cpuDir); err == nil {
		for _, e := range ents {
			n, ok := strings.CutPrefix(e.Name(), "cpu")
			id, err := strconv.Atoi(n)
			if !ok || err != nil {
				continue
			}
			num := func(parts ...string) int64 {
				v, _ := strconv.ParseInt(read(append([]string{"sys", "devices", "system", "cpu", e.Name()}, parts...)...), 10, 64)
				return v
			}
			f.CPUs = append(f.CPUs, CPUFacts{
				CPU:             id,
				Governor:        read("sys", "devices", "system", "cpu", e.Name(), "cpufreq", "scaling_governor"),
				MinKHz:          num("cpufreq", "scaling_min_freq"),
				MaxKHz:          num("cpufreq", "scaling_max_freq"),
				CurKHz:          num("cpufreq", "scaling_cur_freq"),
				CoreThrottle:    num("thermal_throttle", "core_throttle_count"),
				PackageThrottle: num("thermal_throttle", "package_throttle_count"),
			})
		}
	}
	slices.SortFunc(f.CPUs, func(a, b CPUFacts) int { return a.CPU - b.CPU })
	return f
}

// selected returns the bracketed choice of a sysfs choice list such as
// "always [madvise] never".
func selected(v string) string {
	if i, j := strings.IndexByte(v, '['), strings.IndexByte(v, ']'); i >= 0 && j > i {
		return v[i+1 : j]
	}
	return v
}

// cpuModel returns the first "model name" of /proc/cpuinfo with its family
// and model numbers.
func cpuModel(cpuinfo string) string {
	var name, family, model string
	sc := bufio.NewScanner(strings.NewReader(cpuinfo))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case k == "model name" && name == "":
			name = v
		case k == "cpu family" && family == "":
			family = v
		case k == "model" && model == "":
			model = v
		}
	}
	if name == "" {
		return ""
	}
	return name + " (family " + family + ", model " + model + ")"
}

// statusField returns a field of /proc/<pid>/status.
func statusField(status, key string) string {
	for line := range bytes.SplitSeq([]byte(status), []byte("\n")) {
		if k, v, ok := strings.Cut(string(line), ":"); ok && k == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ParseCPUList parses a cpuset list such as "0-3,8,10-11".
func ParseCPUList(s string) ([]int, bool) {
	var out []int
	for part := range strings.SplitSeq(strings.TrimSpace(s), ",") {
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 {
			return nil, false
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil, false
			}
		}
		for c := a; c <= b; c++ {
			out = append(out, c)
		}
	}
	return out, len(out) > 0
}
