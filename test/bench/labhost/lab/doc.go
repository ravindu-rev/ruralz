// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package lab is the benchmark lab's helper, which test/bench/labhost
// builds into one binary that the harness copies to each lab host
// (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Reference hardware"): on RH-1U it serves the mock Upstream, the mock IdP
// and the OTLP sink, on RH-1L it runs the open-loop load generator, on
// RH-1S the fault proxy and supervises Valkey, and on every host it
// answers the pre-run host checks (fixed frequency, thermal-throttle
// counters, a TCP echo for the round trip between hosts). The chaos
// harness's target profile uses it on the same hosts
// (docs/engineering/03-testing-and-quality-strategy.md "Scale profiles"):
// the chaos runs' mock Upstreams, their State Store servers, which the
// run service supervises and signals (Child), and their load, which the
// load service runs and whose records it keeps until no load of the
// session runs (LoadService, Record).
//
// Every service speaks one protocol on its standard input and output, so
// the harness drives it the same way through ssh on the lab hosts and as a
// child process on the loopback profile: the service writes one Ready line
// once it serves, then answers each Request line with one Response line,
// and stops when its standard input ends (the harness closes it, or the
// ssh session dropped) or on SIGTERM, after a final Response line. The
// load generator runs once: it reads one LoadSpec line and writes one
// LoadResult line.
//
// What the services measure is what a run's validity and internal view
// need: the mock records its own latency beyond its configured delay and
// its CPU share, the sink folds every access record's gateway_duration
// into the windows the harness declares (internal/testkit/otlpsink Fold),
// the load generator returns its measured histogram, its 10 s warm-up
// windows, its HdrHistogram V2 interval log and its CPU share, and the
// fault proxy counts what it delayed. Histograms travel in the compressed
// HdrHistogram V2 encoding, base64.
package lab
