// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package pluginhost is the WASM Plugin host of Ruralz Gateway and of
// `ruralz plugin test` (docs/architecture/05-wasm-plugin-system.md;
// ADR-0004, ADR-0005), and the Filter of the plugin Policy type: wazero in
// compiler mode, one compilation cache per artifact digest shared by a
// runtime per limits.memoryBytes, compiled code in memory only; Plugin ABI
// v1 (ruralz.plugin.v1) Host Functions and the WASI shim behind the
// Capability gate; per-call deadlines and Host Function call budgets in
// place of fuel; instance pools a pool manager fills off the request path,
// within the Node Plugin memory cap; fault breakers; and the hot-swap
// lifecycle that warms pools before a snapshot publishes and closes them,
// with their compiled code, when no live snapshot holds them. It is the
// only importer of wazero.
package pluginhost
