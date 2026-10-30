// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package telemetry is the telemetry Node integration suite
// (docs/engineering/03-testing-and-quality-strategy.md "Integration
// tests"): under the integration build tag, in stage 8, it drives the
// controllable-clock ruralzd build of internal/testkit/binaries through the
// process harness of internal/testkit/topology over real sockets, with
// the in-process collector of internal/testkit/otlpsink. It covers a Hot
// Reload that keeps the counters and the OTLP connection and a changed
// endpoint or telemetry.otlp.tls that swaps it within 5 s, a blocked
// stdout under load, the 1,000-reload test of
// docs/architecture/10-observability.md "Cardinality budget", the
// collector modes, the metric push interval, the scrape bound and the
// cleartext hop.
package telemetry
