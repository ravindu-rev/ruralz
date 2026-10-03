// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"bytes"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// golden compares got with testdata/name, or rewrites it with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a fixed testdata path.
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("output differs from %s:\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

// Test plan 6.4 item 35, WP-11 done-when: process log line golden bytes.
func TestProcessGolden_Req63(t *testing.T) {
	s, out := newSink(t, Options{Level: slog.LevelDebug, NodeID: "01J9Z8Q4W6X3V5T7R2N0M1K8H4", TraceContext: traceFromContext})
	at := time.Date(2026, 9, 26, 7, 46, 12, 345678901, time.UTC)
	ctx := withTrace(t.Context(), testIDs())
	emit := func(h slog.Handler, l slog.Level, msg string, as ...slog.Attr) {
		r := slog.NewRecord(at, l, msg, 0)
		r.AddAttrs(as...)
		if err := h.Handle(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	gw := s.Handler("gateway")
	emit(gw, slog.LevelInfo, "listening", slog.String(catalog.KeyListener, "public"), slog.String("address", "0.0.0.0:8443"))
	s.SetRevision("rev-4f2a9c01b7de")
	emit(gw, slog.LevelWarn, "upstream degraded", slog.String(catalog.KeyUpstream, "orders"),
		slog.String(catalog.KeyReason, "discovery_stale"), slog.Duration("age", 90*time.Second))
	emit(s.Handler("loader").WithAttrs([]slog.Attr{slog.String(catalog.KeyFile, "gateway.yaml")}), slog.LevelError,
		"activation failed", slog.String(catalog.KeyCode, "RZ-CFG-026"),
		slog.Any(catalog.KeyError, errors.New(`secretRef env:DB_URL: variable "DB_URL" is not set`)),
		slog.Any("value", secret.NewValue([]byte("hunter2"))), slog.Int(catalog.KeyLine, 12))
	emit(s.Handler("admin").WithGroup("request"), slog.LevelDebug, "tap subscriber",
		slog.Any("headers", http.Header{"Authorization": {"Bearer t"}, "Accept": {"application/x-ndjson"}}),
		slog.String("note", "line\nbreak \"quoted\" \x00 \xff"))
	drain(s)
	golden(t, "process.golden", []byte(out.String()))
}
