// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// ErrHostCheck is the pre-run host check's failure: the job stops with an
// "RH-1 host check failed" annotation and writes no records for the
// scenario (PBB "Reproducibility").
var ErrHostCheck = errors.New("RH-1 host check failed")

// Host check bounds (PBB "Reference hardware" settings baseline).
const (
	// MinFixedKHz is the fixed frequency's floor: 3.0 GHz (target).
	MinFixedKHz = 3_000_000
	// roundTrips is the round trips measured per host pair.
	roundTrips = 10000
	// floorDuration is the direct-to-mock floor check's length.
	floorDuration = 10 * time.Second
)

// CheckFacts compares a host's facts with its previous check: every CPU of
// cpus (all CPUs when empty) runs the performance governor at a fixed
// frequency of 3.0 GHz or more, and no thermal-throttle counter moved.
func CheckFacts(prev *lab.HostFacts, cur lab.HostFacts, cpus []int) []string {
	var reasons []string
	want := map[int]bool{}
	for _, c := range cpus {
		want[c] = true
	}
	before := map[int]lab.CPUFacts{}
	if prev != nil {
		for _, c := range prev.CPUs {
			before[c.CPU] = c
		}
	}
	seen := 0
	for _, c := range cur.CPUs {
		if len(want) > 0 && !want[c.CPU] {
			continue
		}
		seen++
		switch {
		case c.Governor != "performance":
			reasons = append(reasons, fmt.Sprintf("cpu%d governor %q, want performance", c.CPU, c.Governor))
		case c.MinKHz != c.MaxKHz:
			reasons = append(reasons, fmt.Sprintf("cpu%d frequency not fixed: %d to %d kHz", c.CPU, c.MinKHz, c.MaxKHz))
		case c.MinKHz < MinFixedKHz:
			reasons = append(reasons, fmt.Sprintf("cpu%d fixed at %d kHz, below %d", c.CPU, c.MinKHz, MinFixedKHz))
		}
		if b, ok := before[c.CPU]; ok && (b.CoreThrottle != c.CoreThrottle || b.PackageThrottle != c.PackageThrottle) {
			reasons = append(reasons, fmt.Sprintf("cpu%d thermal throttle counters moved: core %d to %d, package %d to %d",
				c.CPU, b.CoreThrottle, c.CoreThrottle, b.PackageThrottle, c.PackageThrottle))
		}
	}
	if seen == 0 {
		reasons = append(reasons, "no CPU frequency facts (cpufreq missing)")
	}
	return reasons
}

// HostChecker runs the pre-run host checks before every scenario and
// keeps each host's previous facts.
type HostChecker struct {
	s    *Session
	prev map[Role]*lab.HostFacts
}

// NewHostChecker returns the session's checker.
func NewHostChecker(s *Session) *HostChecker {
	return &HostChecker{s: s, prev: map[Role]*lab.HostFacts{}}
}

// Check runs the checks for scenario sc: each host's fixed frequency and
// throttle counters, the round trip between the hosts, and the
// direct-to-mock floor check. A profile without checks passes.
func (h *HostChecker) Check(ctx context.Context, sc Scenario) error {
	p := h.s.Profile
	if !p.Checks {
		return nil
	}
	var reasons []string
	for _, role := range Roles() {
		r := p.Runner(role)
		out, err := r.Output(ctx, []string{h.bin(role), "facts"}, nil)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s facts: %v", role, err))
			continue
		}
		var f lab.HostFacts
		if err := json.Unmarshal(out, &f); err != nil {
			reasons = append(reasons, fmt.Sprintf("%s facts: %v", role, err))
			continue
		}
		cpus, _ := proc.ParseCPUList(p.Hosts[role].CPUs)
		for _, why := range CheckFacts(h.prev[role], f, cpus) {
			reasons = append(reasons, string(role)+": "+why)
		}
		h.prev[role] = &f
	}
	rt, err := h.roundTrips(ctx)
	if err != nil {
		reasons = append(reasons, err.Error())
	}
	reasons = append(reasons, rt...)
	if why, err := h.floor(ctx, sc); err != nil {
		reasons = append(reasons, "floor check: "+err.Error())
	} else if why != "" {
		reasons = append(reasons, "floor check: "+why)
	}
	if len(reasons) > 0 {
		return fmt.Errorf("%w: %v", ErrHostCheck, reasons)
	}
	return nil
}

func (h *HostChecker) bin(role Role) string {
	return h.s.Profile.Hosts[role].Dir + "/bin/labhost"
}

// roundTrips starts an echo service on every host and measures the round
// trip p99 from RH-1 to each other host and from RH-1L to RH-1.
func (h *HostChecker) roundTrips(ctx context.Context) ([]string, error) {
	p := h.s.Profile
	bound := time.Duration(p.roundTripBound()) * time.Microsecond
	echoes := map[Role]*Proc{}
	defer func() {
		for _, e := range echoes {
			_, _ = e.Stop()
		}
	}()
	for _, role := range Roles() {
		addr := net.JoinHostPort(p.Hosts[role].Address, strconv.Itoa(p.Services.EchoPort))
		e, err := p.Runner(role).Start(ctx, procEcho, []string{h.bin(role), "echo", "-listen", addr}, p.Hosts[role].AuxCPUs)
		if err != nil {
			return nil, err
		}
		echoes[role] = e
	}
	pairs := [][2]Role{{RoleGateway, RoleLoad}, {RoleGateway, RoleUpstream}, {RoleGateway, RoleStateStore}, {RoleLoad, RoleGateway}}
	var reasons []string
	for _, pr := range pairs {
		from, to := pr[0], pr[1]
		addr := net.JoinHostPort(p.Hosts[to].Address, strconv.Itoa(p.Services.EchoPort))
		out, err := p.Runner(from).Output(ctx, []string{h.bin(from), "roundtrip", "-addr", addr, "-n", strconv.Itoa(roundTrips)}, nil)
		if err != nil {
			return reasons, err
		}
		var doc struct {
			Histogram string `json:"histogram"`
		}
		if err := json.Unmarshal(out, &doc); err != nil {
			return reasons, err
		}
		hist, err := lab.DecodeHistogram(doc.Histogram)
		if err != nil {
			return reasons, err
		}
		if v := hist.ValueAtPercentile(99); v > bound {
			reasons = append(reasons, fmt.Sprintf("round trip %s to %s p99 %s, above %s", from, to, v, bound))
		}
	}
	return reasons, nil
}

// floor runs the direct-to-mock floor check: the scenario's rate from
// RH-1L straight to the mock, valid by loadgen's rules. Its percentiles
// are reported, never subtracted.
func (h *HostChecker) floor(ctx context.Context, sc Scenario) (string, error) {
	p := h.s.Profile
	rp, err := NewRunPKI(h.s.Work+"/floor/pki", p)
	if err != nil {
		return "", err
	}
	if err := rp.Distribute(ctx, p); err != nil {
		return "", err
	}
	l, err := NewLab(ctx, p, h.s.Labhost, rp, h.s.Work+"/floor")
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Close() }()
	if err := l.StartUpstream(ctx, sc.Workload); err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(p.Services.Mock)
	if err != nil {
		return "", err
	}
	w := sc.Workload
	w.Tool, w.Protocol, w.Tokens, w.Streams = ToolLoadgen, "http1", 0, 0
	w.Port, _ = strconv.Atoi(port)
	spec, err := l.LoadSpec(w, host, 0, floorDuration, false)
	if err != nil {
		return "", err
	}
	res, err := l.RunLoad(ctx, spec)
	if err != nil {
		return "", err
	}
	if !res.Valid {
		return res.Invalid, nil
	}
	return "", nil
}
