// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package scenarios holds the M1 benchmark scenarios of
// docs/architecture/12-performance-budgets-and-benchmarking.md that the
// harness's open-loop catalog does not run by itself: the Bundles under
// this directory, one directory per Revision, with the host profiles'
// Environments in environments.yaml, and the drivers of O1, O2, C1, C2,
// R1, G1, the constants runs, PB-10's telemetry-off S2 and the GOMEMLIMIT
// run, which the Latency job registers in harness.Night.Drivers and the
// functional variants run at reduced scale (test/bench/functional).
//
// Each Bundle directory holds the scenario's base in ruralz.yaml and one
// overlay per host profile, overlays/loopback, overlays/rh-1 and
// overlays/rh-3, with the profile's lab addresses, its TLS file paths
// under the profile's fixed secret root and, for the State Store
// scenarios, stateStore.tls and stateStore.zones; the overlays use no
// ${VAR}, so a profile's Revision digests stay equal across runs, which
// the package's tests pin. O1, C1, C2 and the GOMEMLIMIT run load S3's
// Bundle. The functional variants of O1 and O2 have Bundles of their own,
// o1-functional and o2-functional, with loopback overlays only: their
// longer timeouts let a longer injected delay at a lower rate reach the
// same plateaus on one host.
//
// A driver runs at a Scale: Full is the Latency job's, on RH-1, judged on
// every criterion of its scenario; Functional is the nightly Functional
// job's reduced scale on the loopback profile, judged only on the criteria
// that hold on any hardware, the others recorded as skipped. Every driver
// writes ruralz.bench.v1 records through the session's Recorder, and its
// criteria become the records' budget results.
package scenarios
