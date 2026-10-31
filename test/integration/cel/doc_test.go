// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package cel is the CEL Node integration suite
// (docs/engineering/03-testing-and-quality-strategy.md "Integration
// tests"): under the integration build tag, in stage 8, it drives the
// controllable-clock ruralzd build of internal/testkit/binaries through the
// process harness of internal/testkit/topology over real sockets. Every M1
// place of docs/architecture/02-configuration-model.md "Allowed places"
// meets each runtime-error kind its input caps leave reachable: an
// evaluation error, the 1,000,000-unit cost limit and the 50 ms
// comprehension stop, the last two through one expression that the 64 KiB
// header cap leaves tracked. Each case asserts the place's "On runtime
// error" behavior and its signal, and the stop is a Node deadline on the
// controllable clock: the answer waits for the clock to pass it. On
// Routes whose 30 ms timeout comes before the 50 ms stop, a match.when and
// an authz.cel rule stop at the Route deadline instead, and
// accessLog.when, which starts past it, stops at its first interrupt
// check.
package cel
