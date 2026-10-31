// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package harness is the macro benchmark harness of
// docs/architecture/12-performance-budgets-and-benchmarking.md: it runs
// the open-loop scenarios, the rate ladders, the soak and the component
// benchmarks on a host profile, judges each run's validity, compares head
// with baseline runs, checks the budgets and writes ruralz.bench.v1
// records (internal/testkit/benchrecord).
//
// A host profile (Profile) names the lab hosts: RH-1 runs ruralzd and the
// harness, RH-1L the load tools, RH-1U the mock Upstream, the mock IdP and
// the OTLP sink, RH-1S the State Store and the fault proxy. The harness
// builds test/bench/labhost for each run, copies it to the hosts and
// starts each service in a systemd-run scope on its cpuset, driving it
// over its standard input and output through ssh (Runner, Proc); the
// loopback profile runs the same services as child processes of the
// harness, so every code path but ssh and the scopes runs on any Linux
// host. Each run makes a fresh PKI (internal/testkit/pki) and starts the
// release build of ruralzd (internal/testkit/binaries) with RURALZ_ZONE
// lab-a, the profile's GOMAXPROCS, SSL_CERT_FILE naming the run's CA and
// the Revision the head renders from the scenario's Bundle.
//
// A run (Run) offers its rate open-loop: loadgen for every scenario whose
// requests rotate keys or tokens, oha for S1 and its ladder, vegeta for
// S1's cross-check. The warm-up is 60 s, extended in 30 s steps up to
// 180 s until three consecutive 10 s windows agree within 5% at p99; the
// measured phase follows. The run reads the external view from the load
// tool and the internal view from the OTLP sink, which folds each access
// record's gateway_duration into the run's window, and the Node's runtime
// counters over the window for GC CPU share and alloc/op. A run is valid
// only when its achieved rate is within 0.5% of the offered rate, the
// generator and the mock stay below 70% CPU, the mock's p99 beyond its
// delay is at most 200 µs and no telemetry record dropped (targets).
//
// A comparison (Compare) interleaves five head and five baseline runs,
// A, B, A, B; it fails when the median-of-runs p99, external or internal,
// is more than 5% above the baseline's with four of five pairs slower, and
// is inconclusive when the baseline's p99 values vary by more than 2%
// (coefficient of variation) or a side lacks five valid runs. The nightly
// plan (Plan) runs S1 and S2 every night, the component benchmarks and one
// slot of the rotation, re-queues inconclusive items and keeps its state
// and each scenario's baseline on the bench-records branch.
package harness
