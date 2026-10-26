// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package telemetry is the telemetry runtime of a Ruralz binary
// (docs/architecture/10-observability.md, ADR-0018): it assembles the
// metric registry (internal/telemetry/aggregate), the tracer
// (internal/telemetry/tracing), the process and access logs
// (internal/telemetry/logsink and internal/telemetry/accesslog) and the
// degraded-state gauges into one Runtime, and owns their exports: the
// /metrics handler and the OTLP/gRPC pipelines.
//
// # Process setup
//
// ScrubOTELEnvironment snapshots the environment and removes every OTEL_*
// variable before any OpenTelemetry constructor runs, so no SDK option comes
// from the environment; the snapshot still serves ${VAR} substitution. The
// resource holds service.name (the binary), service.version and
// service.instance.id (node.id) under the semantic conventions v1.43.0
// schema, and never the Revision. New starts the one goroutine that owns
// stdout; it opens no connection.
//
// # Export
//
// Apply installs the telemetry settings of the active Revision. With an
// otlp.endpoint, spans, metrics and logs go over one gRPC connection: https
// through the collector's TLS client settings (telemetry.otlp.tls), http as
// a reported cleartext telemetry hop. Spans and logs pass bounded queues
// that never block and count queue_full and export_error drops (spans: the
// tracing processor; logs: 8,192 records and 4 MiB, batches of 512 or every
// second); metrics are pushed every 15 s from the same collection /metrics
// reads. One export attempt is bounded by 10 s and retried from 1 s to 5 s
// apart for up to 10 s; a signal's exports failing continuously for longer
// than one metric interval raise telemetry_export_failing until every
// failing signal has exported again. Without an endpoint nothing is pushed
// and no span is created. A
// changed endpoint or TLS setting builds the new connection off the request
// path, swaps it in and retires the old one within 5 s.
//
// # /metrics
//
// MetricsHandler serves the OpenTelemetry Prometheus exporter over a
// private registry fed by the same producer as OTLP, so both expose one
// name set: text or OpenMetrics with exemplars, at most 4 scrapes encoding
// at once; a fifth waits up to 10 s, then gets 503, and a scrape still
// writing after 10 s is cut off.
//
// # Library errors
//
// RouteLibraryErrors sends the OpenTelemetry SDK's error handler to the
// telemetry logger instead of stderr. It changes a library global, so a
// binary calls it once at start; New never does.
//
// # Concurrency
//
// A Runtime is safe for concurrent use. Request goroutines only use the
// handles it hands out, which never block. Its goroutines (the stdout
// worker, the log exporter worker and the retirement of an old pipeline)
// are canceled and awaited by Shutdown, which flushes logs, spans and a
// final metric export within its context.
package telemetry
