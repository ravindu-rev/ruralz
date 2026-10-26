// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// TestNewValidates checks the required options.
func TestNewValidates(t *testing.T) {
	cases := map[string]Options{
		"no stdout":  {Service: ServiceGateway, NodeID: testNodeID},
		"no node":    {Service: ServiceGateway, Stdout: io.Discard},
		"no service": {NodeID: testNodeID, Stdout: io.Discard},
	}
	for name, o := range cases {
		if r, err := New(o); err == nil {
			_ = r.Shutdown(context.Background())
			t.Errorf("%s: New succeeded", name)
		}
	}
}

// TestProcessLogsCarryTheFraming checks a process line: component, node_id,
// the active Revision once set, and redaction of a secret value.
func TestProcessLogsCarryTheFraming(t *testing.T) {
	r, out := newRuntime(t, nil)
	r.Logger("loader").Info("before activation")
	r.SetActiveRevision("rev-0123456789ab")
	r.Logger("loader").Info("activated", slog.Any("token", secret.NewValue([]byte("s3cret"))))
	r.Logger("loader").Debug("below the level")
	flushLogs(t, r)
	lines := out.lines(t)
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want 2", lines)
	}
	for _, l := range lines {
		if l["component"] != "loader" || l["node_id"] != testNodeID {
			t.Errorf("line %v lacks component or node_id", l)
		}
	}
	if _, ok := lines[0]["revision"]; ok {
		t.Errorf("revision before activation: %v", lines[0])
	}
	if lines[1]["revision"] != "rev-0123456789ab" || lines[1]["token"] != "[REDACTED]" {
		t.Errorf("activated line = %v", lines[1])
	}
}

// TestLogCountersReachMetrics checks that the stream counters feed
// ruralz_telemetry_logs_total at collection.
func TestLogCountersReachMetrics(t *testing.T) {
	r, _ := newRuntime(t, nil)
	for range 3 {
		r.Logger("x").Warn("w")
	}
	rec := r.AccessLog().Acquire()
	rec.Method, rec.Path, rec.Status = "GET", "/", 200
	r.AccessLog().Submit(rec)
	set := scrapeAfter(t, r)
	if v := value(t, set, catalog.TelemetryLogsTotal, "stream", "process"); v < 3 {
		t.Errorf("process records = %v, want at least 3", v)
	}
	if v := value(t, set, catalog.TelemetryLogsTotal, "stream", "access"); v != 1 {
		t.Errorf("access records = %v, want 1", v)
	}
	if v := value(t, set, catalog.TelemetryLogsDroppedTotal, "stream", "process", "reason", "queue_full"); v != 0 {
		t.Errorf("process queue_full = %v", v)
	}
}

// TestRuntimeImplementsTheContracts checks the handles the data plane
// takes.
func TestRuntimeImplementsTheContracts(t *testing.T) {
	r, _ := newRuntime(t, nil)
	var (
		_ emit.Meter      = r.Meter()
		_ emit.Tracer     = r.Tracer()
		_ emit.AccessLog  = r.AccessLog()
		_ emit.NodeStatus = r
	)
	if r.ErrorLog("listener") == nil {
		t.Error("no ErrorLog handler")
	}
}

// TestShutdownIsFinal checks that Shutdown is idempotent and that Apply
// afterwards is refused.
func TestShutdownIsFinal(t *testing.T) {
	r, _ := newRuntime(t, nil)
	ctx := context.Background()
	if err := r.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := r.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
	if err := r.Apply(ctx, Settings{sampling: 0.5}); !errors.Is(err, ErrClosed) {
		t.Errorf("Apply after Shutdown = %v, want ErrClosed", err)
	}
}
