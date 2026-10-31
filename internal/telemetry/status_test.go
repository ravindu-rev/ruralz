// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/logsink"
	"github.com/ravindu-rev/ruralz/internal/telemetry/tracing"
)

// TestEveryReasonAndHopStartsAtZero checks that /metrics shows every
// degraded reason and every cleartext hop at 0 before any Revision.
func TestEveryReasonAndHopStartsAtZero(t *testing.T) {
	r, _ := newRuntime(t, nil)
	set := scrape(t, r)
	for _, reason := range catalog.ReasonNames() {
		if v := value(t, set, catalog.NodeDegradedInfo, "reason", reason); v != 0 {
			t.Errorf("reason %s = %v", reason, v)
		}
	}
	for _, hop := range catalog.HopNames() {
		if v := value(t, set, catalog.SecurityCleartextHops, "hop", hop); v != 0 {
			t.Errorf("hop %s = %v", hop, v)
		}
	}
}

// TestDegradedHoldsWhileAnySourceDoes checks that a reason stays 1 until
// its last source clears, and that raise and clear log once each.
func TestDegradedHoldsWhileAnySourceDoes(t *testing.T) {
	r, out := newRuntime(t, nil)
	jwks := func() float64 {
		return value(t, scrape(t, r), catalog.NodeDegradedInfo, "reason", "jwks_stale")
	}
	r.SetDegraded(catalog.ReasonJWKSStale, "jwt-a", true)
	r.SetDegraded(catalog.ReasonJWKSStale, "jwt-b", true)
	if jwks() != 1 {
		t.Fatal("reason not raised")
	}
	r.SetDegraded(catalog.ReasonJWKSStale, "jwt-a", false)
	if got := value(t, scrapeAfter(t, r), catalog.NodeDegradedInfo, "reason", "jwks_stale"); got != 1 {
		t.Errorf("one source left: %v, want 1", got)
	}
	r.SetDegraded(catalog.ReasonJWKSStale, "jwt-b", false)
	if got := value(t, scrapeAfter(t, r), catalog.NodeDegradedInfo, "reason", "jwks_stale"); got != 0 {
		t.Errorf("no source left: %v, want 0", got)
	}
	flushLogs(t, r)
	var msgs []string
	for _, l := range out.lines(t) {
		if l["reason"] == "jwks_stale" {
			msgs = append(msgs, l["level"].(string)+" "+l["msg"].(string))
		}
	}
	want := []string{"WARN degraded state raised", "INFO degraded state cleared"}
	if !slices.Equal(msgs, want) {
		t.Errorf("transition logs = %q, want %q", msgs, want)
	}
}

// TestCleartextHopsRaiseTheReason checks that any cleartext hop raises
// cleartext_hop and that the hop gauge counts them.
func TestCleartextHopsRaiseTheReason(t *testing.T) {
	r, _ := newRuntime(t, nil)
	r.SetCleartextHops(catalog.HopUpstream, 3)
	set := scrapeAfter(t, r)
	if value(t, set, catalog.SecurityCleartextHops, "hop", "upstream") != 3 ||
		value(t, set, catalog.NodeDegradedInfo, "reason", "cleartext_hop") != 1 {
		t.Error("cleartext upstream hops not reported")
	}
	r.SetCleartextHops(catalog.HopUpstream, 0)
	if value(t, scrapeAfter(t, r), catalog.NodeDegradedInfo, "reason", "cleartext_hop") != 0 {
		t.Error("cleartext_hop not cleared")
	}
}

// TestExportFailingNeedsOneInterval checks telemetry_export_failing: a
// failure run shorter than the interval does not raise it, a longer one
// does, and the signal's next success clears it; failures log at most once
// per interval.
func TestExportFailingNeedsOneInterval(t *testing.T) {
	r, out := newRuntime(t, nil)
	clk, _ := r.clk.(*clocktest.Fake)
	failing := func() float64 {
		return value(t, scrapeAfter(t, r), catalog.NodeDegradedInfo, "reason", "telemetry_export_failing")
	}
	boom := errors.New("unavailable")
	r.health.record(signalSpans, boom)
	clk.Advance(10 * time.Second)
	r.health.record(signalSpans, boom)
	if failing() != 0 {
		t.Fatal("raised within one interval")
	}
	clk.Advance(6 * time.Second)
	r.health.record(signalSpans, boom)
	if failing() != 1 {
		t.Fatal("not raised past one interval")
	}
	r.health.record(signalMetrics, boom)
	r.health.record(signalSpans, nil)
	if failing() != 1 {
		t.Fatal("cleared while metrics still fail")
	}
	r.health.record(signalMetrics, nil)
	if failing() != 0 {
		t.Fatal("not cleared after every signal recovered")
	}
	flushLogs(t, r)
	warns := map[string]int{}
	for _, l := range out.lines(t) {
		if l["msg"] == "OTLP export failed" {
			warns[l["signal"].(string)]++
		}
	}
	// Each scrape advances the clock 2 s, so spans failed at 0 s, 10 s
	// and 18 s: the first and the one past the interval log.
	if warns["spans"] != 2 || warns["metrics"] != 1 {
		t.Errorf("failure logs = %v, want spans 2 and metrics 1", warns)
	}
}

// TestRouteLibraryErrors checks that SDK errors become telemetry WARN
// records instead of stderr output, at most logsink.ErrorLogRate per second.
func TestRouteLibraryErrors(t *testing.T) {
	r, out := newRuntime(t, nil)
	r.RouteLibraryErrors()
	t.Cleanup(func() { otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {})) })
	// The clock stands still, so only the first second's burst passes.
	for range logsink.ErrorLogRate + 3 {
		otel.Handle(errors.New("exporter refused"))
	}
	flushLogs(t, r)
	n := 0
	for _, l := range out.lines(t) {
		if l["msg"] == "OpenTelemetry SDK error" && l["component"] == "telemetry" && l["level"] == "WARN" {
			n++
		}
	}
	if n != logsink.ErrorLogRate {
		t.Errorf("SDK error records = %d, want %d: %v", n, logsink.ErrorLogRate, out.lines(t))
	}
}

// TestSamplingFollowsApply checks the root ratio: the default before any
// Revision, then the applied one.
func TestSamplingFollowsApply(t *testing.T) {
	r, _ := newRuntime(t, nil)
	if r.SamplingRatio() != tracing.DefaultTraceSampling {
		t.Errorf("default ratio = %v", r.SamplingRatio())
	}
	if err := r.Apply(context.Background(), Settings{sampling: 0.5}); err != nil {
		t.Fatal(err)
	}
	if r.SamplingRatio() != 0.5 {
		t.Errorf("applied ratio = %v", r.SamplingRatio())
	}
}
