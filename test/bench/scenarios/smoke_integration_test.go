// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package scenarios

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
)

// TestSmoke is the pr-full scenario smoke run (PBB "Suite layers"): every
// M1 scenario, S1, S2, S3, S5, S5x, O1, O2, C1, C2, R1 and G1, on the
// loopback profile at 100 rps for 5 s with the release build in real time.
// Each must start, serve and write valid ruralz.bench.v1 records; no
// numeric criterion is judged. The 5-minute total is a target the log
// reports, never an assertion.
func TestSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	records := filepath.Join(work, "records")
	e, err := Loopback(ctx, root, work, records)
	if err != nil {
		t.Fatal(err)
	}
	s := Smoke()
	start := time.Now()
	for _, sc := range harness.Catalog() {
		t.Run(sc.ID, func(t *testing.T) {
			if _, err := RunScenario(ctx, e, s, AtScale(sc, s.Rate)); err != nil {
				t.Fatalf("%s did not start or serve: %v", sc.ID, err)
			}
		})
	}
	for _, d := range []struct {
		id  string
		run func(context.Context, Env, Scale) (*Result, error)
	}{
		{ItemO1, RunO1}, {ItemO2, RunO2}, {ItemC1, RunC1}, {ItemC2, RunC2}, {ItemR1, RunR1}, {ItemG1, RunG1},
	} {
		t.Run(d.id, func(t *testing.T) {
			r, err := d.run(ctx, e, s)
			if err != nil {
				t.Fatalf("%s did not start or serve: %v", d.id, err)
			}
			if len(r.Records) == 0 {
				t.Errorf("%s wrote no record", d.id)
			}
			// No numeric criterion is judged at 100 rps; the log keeps
			// what each criterion found.
			for _, f := range r.Failures() {
				t.Logf("%s (not judged): %s", d.id, f)
			}
		})
	}
	problems, n, err := CheckRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
	if n == 0 {
		t.Error("the smoke run wrote no record")
	}
	t.Logf("smoke run: %d records in %s (target 5 minutes)", n, time.Since(start).Round(time.Second))
}
