// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Package functional is the nightly Functional job's suite
// (docs/architecture/12-performance-budgets-and-benchmarking.md "Suite
// layers"; docs/engineering/03-testing-and-quality-strategy.md
// "Benchmarks and regression gates"): every M1 scenario and constants run
// at reduced scale on the loopback profile, with the release build of
// ruralzd and loadgen in real time, asserting only the criteria that hold
// on any hardware: O1's accounting and its 20,000 in-flight plateau, O2's
// 8,192-call plateau and skip counting, C1 and C2 holding every
// connection, R1's activations, G1's connection counts, the pre-routing
// writes, the body flood, K = 2 under activations and the queue bounds.
// The drivers record the hardware-bound criteria as skipped, and the
// runs' numbers are never budget results. The loopback profile's fixed
// ports let one run at a time use the host, so the tests run one after
// another (make nightly NIGHTLY_JOB=functional passes -p 1); a host whose
// open-file limit is below a run's connections fails that run before it
// starts.
package functional

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
	"github.com/ravindu-rev/ruralz/test/bench/scenarios"
)

// suite is the package's one build and session, made by the first test
// that needs it and shared by the rest.
var suite struct {
	once sync.Once
	env  scenarios.Env
	err  error
	work string
}

// TestMain removes the shared work directory once every test ran.
func TestMain(m *testing.M) {
	code := m.Run()
	if suite.work != "" {
		_ = os.RemoveAll(suite.work)
	}
	os.Exit(code)
}

// env returns the shared loopback Env: the release ruralzd and ruralz of
// this tree and a session writing its records into the work directory,
// or RURALZ_BENCH_RECORDS when set.
func env(t *testing.T) scenarios.Env {
	t.Helper()
	suite.once.Do(func() {
		root, err := binaries.RepoRoot(".")
		if err != nil {
			suite.err = err
			return
		}
		if suite.work, err = os.MkdirTemp("", "ruralz-bench-functional-"); err != nil {
			suite.err = err
			return
		}
		records := os.Getenv(harness.EnvRecords)
		if records == "" {
			records = filepath.Join(suite.work, "records")
		}
		// The build outlives the first test, so it runs without a test's
		// context.
		suite.env, suite.err = scenarios.Loopback(context.Background(), root, suite.work, records)
	})
	if suite.err != nil {
		t.Fatal(suite.err)
	}
	return suite.env
}

// check fails the test on a driver error and on every criterion that
// failed; an invalid run, whose validity rules hold only on the reference
// hardware, is logged.
func check(t *testing.T, r *scenarios.Result, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Failures() {
		t.Error(f)
	}
	for _, b := range r.Budgets {
		if b.Verdict != benchrecord.BudgetFail {
			t.Logf("%s: %s (%s)", b.ID, b.Verdict, b.Result)
		}
	}
	for _, inv := range r.Invalid {
		t.Logf("invalid on this host, not judged: %s", inv)
	}
	if len(r.Records) == 0 && len(r.Budgets) == 0 {
		t.Error("the run wrote no record")
	}
}

// catalogRun runs catalog scenario id at the functional scale.
func catalogRun(t *testing.T, id string) {
	t.Helper()
	sc, ok := harness.Find(harness.Catalog(), id)
	if !ok {
		t.Fatalf("no catalog scenario %s", id)
	}
	s := scenarios.Functional()
	r, err := scenarios.RunScenario(t.Context(), env(t), s, scenarios.AtScale(sc, s.Rate))
	check(t, r, err)
}

// TestS1, TestS2, TestS3, TestS5 and TestS5x run the catalog scenarios at
// the functional rate: each serves every offered request.
func TestS1(t *testing.T)  { catalogRun(t, "S1") }
func TestS2(t *testing.T)  { catalogRun(t, "S2") }
func TestS3(t *testing.T)  { catalogRun(t, "S3") }
func TestS5(t *testing.T)  { catalogRun(t, "S5") }
func TestS5x(t *testing.T) { catalogRun(t, "S5x") }

// driver runs one driver at the functional scale.
func driver(t *testing.T, run func(context.Context, scenarios.Env, scenarios.Scale) (*scenarios.Result, error)) {
	t.Helper()
	r, err := run(t.Context(), env(t), scenarios.Functional())
	check(t, r, err)
}

// TestO1 checks O1's admitted plus RZ-RT-005 accounting, zero RZ-UP-006
// and the 20,000 in-flight plateau.
func TestO1(t *testing.T) { driver(t, scenarios.RunO1) }

// TestO2 checks O2's plateau at 8,192 State Store calls in flight, reached
// with a longer injected delay at a lower rate, its skip counting, the
// goroutine bound and the shard's connection count.
func TestO2(t *testing.T) { driver(t, scenarios.RunO2) }

// TestC1 checks C1's cleartext and TLS runs holding every idle connection.
func TestC1(t *testing.T) { driver(t, scenarios.RunC1) }

// TestC2 checks C2's handshake burst holding every new connection.
func TestC2(t *testing.T) { driver(t, scenarios.RunC2) }

// TestR1 checks the reload ladder's activations, its Last-Known-Good start
// and no failed request across Hot Reloads under load.
func TestR1(t *testing.T) { driver(t, scenarios.RunR1) }

// TestG1 checks G1's connection counts per emulated Node.
func TestG1(t *testing.T) { driver(t, scenarios.RunG1) }

// TestConstants checks the constants runs: the pre-routing writes, the
// body flood, K = 2 under activations, the queue bounds at both trace
// caps, the Quota denials, the cache key flood, the ring-hash ladder and
// the slow-header flood.
func TestConstants(t *testing.T) { driver(t, scenarios.RunConstants) }

// TestPB10 runs PB-10's telemetry-off S2; PB-10 itself holds only on RH-1.
func TestPB10(t *testing.T) { driver(t, scenarios.RunPB10) }

// TestGOMEMLIMIT checks the GOMEMLIMIT run holding its idle TLS
// connections through S2's load.
func TestGOMEMLIMIT(t *testing.T) { driver(t, scenarios.RunGOMEMLIMIT) }
