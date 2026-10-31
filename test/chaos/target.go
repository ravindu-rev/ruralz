// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"errors"
	"fmt"

	"github.com/ravindu-rev/ruralz/test/bench/harness"
)

// ErrTargetPlacement stops the target profile before it starts anything:
// its runs place the State Store on RH-1S, the mock Upstreams and the OTLP
// sink on RH-1U and the load on RH-1L of the RH-1 inventory, RH-1L
// returning every request's record once the load ends
// (docs/engineering/03-testing-and-quality-strategy.md "Scale profiles"),
// a placement Planned (M1) that the chaos harness does not make: it runs
// everything on its own host. Running them all on RH-1 would record and
// gate target results on a placement the profile does not have.
var ErrTargetPlacement = errors.New("chaos: the target profile's State Store, mocks and load on RH-1S, RH-1U and RH-1L are Planned (M1) " +
	`(docs/engineering/03-testing-and-quality-strategy.md "Scale profiles")`)

// TargetHosts returns the RH-1 inventory the target profile runs on: the
// benchmark harness's inventory that harness.EnvProfile names, which make
// chaos-scale sets from BENCH_PROFILE (deploy/benchhost/rh-1.json in
// chaos-scale.yml), read and validated by the harness's parser.
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

// checkProfile checks that the harness can run profile p: the target
// profile needs its RH-1 inventory, then fails with ErrTargetPlacement.
func checkProfile(p Profile, getenv func(string) string) error {
	if p.Name != Target {
		return nil
	}
	if _, err := TargetHosts(getenv); err != nil {
		return err
	}
	return ErrTargetPlacement
}
