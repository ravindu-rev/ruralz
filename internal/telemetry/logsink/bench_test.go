// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"io"
	"log/slog"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// BenchmarkProcessLogDisabled: a debug record at info (09 req 64, 70: no
// cost, 0 allocations).
func BenchmarkProcessLogDisabled(b *testing.B) {
	s, err := New(Options{Stdout: io.Discard})
	if err != nil {
		b.Fatal(err)
	}
	log := s.Logger("gateway")
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		log.LogAttrs(ctx, slog.LevelDebug, "request", slog.String("route", "orders"))
	}
}

// BenchmarkProcessLogQueue: an enabled record queued by the caller; the
// queue is drained outside the timer's view every 1,024 records.
func BenchmarkProcessLogQueue(b *testing.B) {
	s, err := New(Options{Stdout: io.Discard, QueueRecords: 2048})
	if err != nil {
		b.Fatal(err)
	}
	log := s.Logger("gateway")
	ctx := b.Context()
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		log.LogAttrs(ctx, slog.LevelWarn, "upstream degraded",
			slog.String(catalog.KeyUpstream, "orders"), slog.String(catalog.KeyCode, "RZ-UP-001"))
		if i++; i%1024 == 0 {
			b.StopTimer()
			s.pump(nil)
			b.StartTimer()
		}
	}
}

// BenchmarkProcessLogEncode: the worker's encoding and write of one record.
func BenchmarkProcessLogEncode(b *testing.B) {
	s, err := New(Options{Stdout: io.Discard, NodeID: "01J9Z8Q4W6X3V5T7R2N0M1K8H4"})
	if err != nil {
		b.Fatal(err)
	}
	s.SetRevision("rev-4f2a9c01b7de")
	log := s.Logger("gateway")
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		log.LogAttrs(ctx, slog.LevelWarn, "upstream degraded",
			slog.String(catalog.KeyUpstream, "orders"), slog.String(catalog.KeyCode, "RZ-UP-001"), slog.Int("attempt", 2))
		b.StartTimer()
		s.pump(nil)
	}
}
