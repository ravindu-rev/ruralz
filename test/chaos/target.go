// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"fmt"

	"github.com/ravindu-rev/ruralz/test/bench/harness"
)

// TargetHosts returns the RH-1 inventory the target profile runs on: the
// benchmark harness's inventory that harness.EnvProfile names, which make
// chaos-scale sets from BENCH_PROFILE (deploy/benchhost/rh-1.json in
// chaos-scale.yml), read and validated by the harness's parser. Only the
// RH-1 hosts, with their cpusets, run it: target results gated on another
// placement would not be the target's
// (docs/engineering/03-testing-and-quality-strategy.md "Scale profiles").
func TargetHosts(getenv func(string) string) (*harness.Profile, error) {
	path := getenv(harness.EnvProfile)
	if path == "" {
		return nil, fmt.Errorf("chaos: the %s profile runs on the four RH-1 hosts: %s names no inventory (make chaos-scale BENCH_PROFILE=deploy/benchhost/rh-1.json)", Target, harness.EnvProfile)
	}
	p, err := harness.ReadProfile(path)
	if err != nil {
		return nil, fmt.Errorf("chaos: the %s profile's hosts: %w", Target, err)
	}
	if p.Name != harness.ProfileRH1 || !p.Scope {
		return nil, fmt.Errorf("chaos: the %s profile runs on the RH-1 hosts with their cpusets, not on the %s inventory %s", Target, p.Name, path)
	}
	return p, nil
}
