// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"fmt"
	"os"
	"slices"
)

// Environment of the chaos jobs (docs/engineering/03-testing-and-quality-strategy.md
// "Scale profiles" and "Result records").
const (
	// EnvScale selects the scale profile: reduced (the default), nightly
	// or target. make chaos sets it from CHAOS_SCALE.
	EnvScale = "RURALZ_CHAOS_SCALE"
	// EnvRecords names the directory that receives the ruralz.chaos.v1
	// records and their interval logs; make chaos passes dist/chaos. Unset,
	// no record is written.
	EnvRecords = "RURALZ_CHAOS_RECORDS"
)

// Profile names.
const (
	Reduced = "reduced"
	Nightly = "nightly"
	Target  = "target"
)

// Kind is what a criterion measures, which decides whether a profile
// gates it.
type Kind uint8

// Criterion kinds.
const (
	// Functional criteria hold on any hardware and run on the
	// controllable-clock build: counts, codes, bounds and controlled-time
	// durations.
	Functional Kind = iota + 1
	// Latency criteria, such as CE-3's p99, are measurements on the
	// release build.
	Latency
	// CPU criteria, such as CE-12's shard script CPU, are measurements on
	// the release build too.
	CPU
)

// String names the kind.
func (k Kind) String() string {
	switch k {
	case Functional:
		return "functional"
	case Latency:
		return "latency"
	case CPU:
		return "cpu"
	default:
		return "unknown"
	}
}

// Experiment identifiers of the Scalability chaos experiments, the System
// overview failure rows and the Verification runbook lab gates the Chaos
// job runs (docs/architecture/11-scalability-and-distributed-state.md
// "Chaos experiments").
const (
	CE1  = "CE-1"
	CE2  = "CE-2"
	CE3  = "CE-3"
	CE4  = "CE-4"
	CE5  = "CE-5"
	CE6  = "CE-6"
	CE12 = "CE-12"
	CE15 = "CE-15"
	CE16 = "CE-16"
	GD2  = "GD-2"
	// RowStateStore is the System overview row "State Store slow or
	// down", and RowUpstream the row "Upstream failing".
	RowStateStore = "state-store-row"
	RowUpstream   = "upstream-row"
	// V1, V3 and V6 are the Verification runbook lab gates the Chaos job
	// runs beside V-4 (CE-2) and V-7 (CE-5).
	V1 = "V-1"
	V3 = "V-3"
	V6 = "V-6"
)

// Profile is one scale profile: where the Chaos job runs, which
// experiments, with how many Nodes and what load, and which criteria
// gate.
type Profile struct {
	// Name is reduced, nightly or target.
	Name string
	// Nodes is the Node count of every run but CE-5's.
	Nodes int
	// CE5Nodes is the Node count of CE-5's runs.
	CE5Nodes int
	// LoadShare is the share of the topology's saturation S a run offers
	// unless its experiment sets a load: half (target).
	LoadShare float64
	// HotRatio divides the rate CE-12 offers its hot key into the hot
	// limit's requests per second: 40, the target's ratio of 1,000,000 to
	// 25,000.
	HotRatio float64

	// hotRate and floodRate, when positive, fix CE-12's offered rate and
	// CE-15's new keys per second (the target profile); otherwise they
	// follow S.
	hotRate, floodRate float64
	gates              []Kind
	runs               []string
}

// Profiles returns the scale profiles in order: reduced, nightly and
// target (docs/engineering/03-testing-and-quality-strategy.md "Scale
// profiles").
func Profiles() []Profile {
	return []Profile{
		{
			// make chaos on a contributor machine: every experiment, drill
			// and row on 3 Nodes, the smallest Cluster Ruralz Gateway
			// availability recommends, gating nothing.
			Name: Reduced, Nodes: 3, CE5Nodes: 3, LoadShare: 0.5, HotRatio: 40,
		},
		{
			// The nightly Chaos job: every functional criterion gates,
			// latency and CPU criteria are recorded.
			Name: Nightly, Nodes: 10, CE5Nodes: 10, LoadShare: 0.5, HotRatio: 40,
			gates: []Kind{Functional},
		},
		{
			// chaos-scale.yml on RH-1: CE-3, CE-5, CE-12, CE-15 and GD-2,
			// every criterion gating.
			Name: Target, Nodes: 10, CE5Nodes: 100, LoadShare: 0.5, HotRatio: 40,
			hotRate: 1_000_000, floodRate: 100_000,
			gates: []Kind{Functional, Latency, CPU},
			runs:  []string{CE3, CE5, CE12, CE15, GD2},
		},
	}
}

// ParseProfile returns the profile named s; "" is reduced, the default of
// make chaos.
func ParseProfile(s string) (Profile, error) {
	if s == "" {
		s = Reduced
	}
	for _, p := range Profiles() {
		if p.Name == s {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("chaos: %s=%q, want %s, %s or %s", EnvScale, s, Reduced, Nightly, Target)
}

// ProfileFromEnv returns the profile EnvScale names.
func ProfileFromEnv() (Profile, error) { return ParseProfile(os.Getenv(EnvScale)) }

// Gates reports whether a failed criterion of kind k fails its run.
func (p Profile) Gates(k Kind) bool { return slices.Contains(p.gates, k) }

// Runs reports whether the profile runs the experiment, drill or row id;
// a variant such as "CE-5/cluster" runs with its experiment, and every
// profile measures its saturation.
func (p Profile) Runs(id string) bool {
	if p.runs == nil || id == Saturation {
		return true
	}
	return slices.Contains(p.runs, Base(id))
}

// HotRate returns the rate CE-12 offers its hot key at saturation s: s,
// or 1,000,000 calls per second at the target profile.
func (p Profile) HotRate(s float64) float64 {
	if p.hotRate > 0 {
		return p.hotRate
	}
	return s
}

// FloodRate returns the new keys per second CE-15 offers at saturation
// s: min(100,000, s), or 100,000 at the target profile.
func (p Profile) FloodRate(s float64) float64 {
	if p.floodRate > 0 {
		return p.floodRate
	}
	return min(100_000, s)
}

// ZoneSplit splits nodes over zones as evenly as possible, earlier zones
// taking the remainder: GD-2 splits 10 Nodes 4, 3 and 3 over three zones.
func ZoneSplit(nodes, zones int) []int {
	if zones < 1 {
		return nil
	}
	out := make([]int, zones)
	for i := range out {
		out[i] = nodes / zones
		if i < nodes%zones {
			out[i]++
		}
	}
	return out
}

// Base returns the experiment of a run name: "CE-5" for "CE-5/cluster".
func Base(id string) string {
	for i := range len(id) {
		if id[i] == '/' {
			return id[:i]
		}
	}
	return id
}
