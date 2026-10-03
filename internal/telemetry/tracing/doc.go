// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package tracing implements emit.Tracer and emit.Span for ruralzd on the
// OpenTelemetry trace SDK (spec 09 sections 2.4 and 2.5,
// docs/architecture/10-observability.md "Tracing").
//
// Every request gets exactly one sampling decision (Tracer.Decide): the
// W3C traceparent of the client, when valid, parents the server span and
// its sampled flag decides, within a per-Node parent cap of 500 per
// second; otherwise a new trace starts and the Revision's traceSampling
// ratio decides deterministically on the trace ID, within a per-Node root
// cap of 1,000 per second. Both caps are lock-free GCRA buckets on one
// atomic word each; a decision over its cap becomes unsampled and counts
// in ruralz_telemetry_traces_unsampled_total, and the request is never
// dropped. The trace ID and the server span ID are chosen by Decide on
// crypto/rand even for unsampled requests, because the trace ID is the
// requestId of every problem document and every Upstream leg receives a
// traceparent.
//
// Spans exist only for sampled requests while an OTLP exporter is set
// (Tracer.SetExporter). The server span's IDs are the ones Decide chose:
// the SDK's IDGenerator returns them from the context StartServer builds.
// Unsampled requests start no span and allocate nothing; the no-op spans
// are values that cost a pointer copy. Span names come from the catalog
// and are precomputed at snapshot compile time; attributes follow
// semantic conventions v1.43 minus url.full, url.query and every header
// attribute.
//
// Ended spans pass a bounded Processor (8,192 spans) that never blocks the
// request goroutine: a full queue drops and counts queue_full; one worker
// exports batches of up to 512 spans or every second and counts failed
// batches as export_error.
package tracing
