// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"log/slog"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// signal is an OTLP signal.
type signal uint8

// Signals.
const (
	signalSpans signal = iota
	signalMetrics
	signalLogs
	numSignals
)

// String returns the signal name used in logs.
func (s signal) String() string {
	switch s {
	case signalSpans:
		return "spans"
	case signalMetrics:
		return "metrics"
	case signalLogs:
		return "logs"
	default:
		return ""
	}
}

// healthSource is the source of telemetry_export_failing.
const healthSource = "otlp_export"

// exportHealth raises telemetry_export_failing while one signal's exports
// have failed continuously for longer than the metric interval, and clears
// it when every failing signal has exported again
// (docs/architecture/10-observability.md "Degraded states"). An export
// failure logs at WARN at most once per signal per interval.
type exportHealth struct {
	clk      clock.Clock
	interval time.Duration
	status   emit.NodeStatus
	log      *slog.Logger

	mu       sync.Mutex
	since    [numSignals]time.Time // first failure of the current run; zero when healthy
	lastWarn [numSignals]time.Time
	raised   bool
}

// record notes one export attempt of sig.
func (h *exportHealth) record(sig signal, err error) {
	now := h.clk.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		h.since[sig] = time.Time{}
	} else {
		if h.since[sig].IsZero() {
			h.since[sig] = now
		}
		if h.lastWarn[sig].IsZero() || now.Sub(h.lastWarn[sig]) >= h.interval {
			h.lastWarn[sig] = now
			h.log.Warn("OTLP export failed", slog.String("signal", sig.String()), slog.Any("error", err))
		}
	}
	h.update(now)
}

// reset forgets every failure: the pipeline they belonged to is gone.
func (h *exportHealth) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.since = [numSignals]time.Time{}
	h.update(h.clk.Now())
}

// update raises or clears the reason from the failure runs.
func (h *exportHealth) update(now time.Time) {
	failing := false
	for _, s := range h.since {
		if !s.IsZero() && now.Sub(s) > h.interval {
			failing = true
		}
	}
	healthy := true
	for _, s := range h.since {
		if !s.IsZero() {
			healthy = false
		}
	}
	switch {
	case failing && !h.raised:
		h.raised = true
		h.status.SetDegraded(catalog.ReasonTelemetryExportFailing, healthSource, true)
	case healthy && h.raised:
		h.raised = false
		h.status.SetDegraded(catalog.ReasonTelemetryExportFailing, healthSource, false)
	}
}
