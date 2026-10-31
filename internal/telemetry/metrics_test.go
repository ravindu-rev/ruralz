// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/telemetry/aggregate"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/telemetry/tracing"
)

// openMetrics is the Accept header of an OpenMetrics scraper.
const openMetrics = "application/openmetrics-text;version=1.0.0;escaping=underscores"

// TestMetricsNegotiatesTheFormat checks text by default and OpenMetrics,
// ended by # EOF, when asked.
func TestMetricsNegotiatesTheFormat(t *testing.T) {
	r, _ := newRuntime(t, nil)
	body, ctype := scrapeText(t, r, "")
	if !strings.HasPrefix(ctype, "text/plain") || strings.Contains(body, "# EOF") {
		t.Errorf("default scrape: content type %q, EOF %v", ctype, strings.Contains(body, "# EOF"))
	}
	if !strings.Contains(body, catalog.NodeDegradedInfo) {
		t.Errorf("text scrape lacks %s", catalog.NodeDegradedInfo)
	}
	body, ctype = scrapeText(t, r, openMetrics)
	if !strings.HasPrefix(ctype, "application/openmetrics-text") || !strings.HasSuffix(body, "# EOF\n") {
		t.Errorf("OpenMetrics scrape: content type %q, tail %q", ctype, body[max(0, len(body)-20):])
	}
}

// TestMetricsServesOnlyGetAndHead checks the methods: HEAD sends headers
// only, any other method is 405 with Allow.
func TestMetricsServesOnlyGetAndHead(t *testing.T) {
	r, _ := newRuntime(t, nil)
	rec := httptest.NewRecorder()
	r.MetricsHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/metrics", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") == "" {
		t.Errorf("HEAD = %d, %d bytes, type %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Type"))
	}
	rec = httptest.NewRecorder()
	r.MetricsHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST = %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

// TestFifthScrapeWaitsForASlot checks the scrape bound: with every slot
// taken a scrape waits, gets 503 once its wait ends, and proceeds as soon
// as a slot frees.
func TestFifthScrapeWaitsForASlot(t *testing.T) {
	r, _ := newRuntime(t, nil)
	h := r.metrics
	for range scrapeSlots {
		h.slots <- struct{}{}
	}
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(ended, http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("busy scrape = %d, want 503", rec.Code)
	}
	done := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
		done <- rec.Code
	}()
	<-h.slots
	if code := <-done; code != http.StatusOK {
		t.Errorf("scrape after a slot freed = %d, want 200", code)
	}
	for range scrapeSlots - 1 {
		<-h.slots
	}
}

// TestExemplarsOnlyInOpenMetrics checks that a sampled observation shows
// as an exemplar in OpenMetrics and never in the text format.
func TestExemplarsOnlyInOpenMetrics(t *testing.T) {
	r, _ := newRuntime(t, nil)
	plan, err := r.Meter().Admit(emit.Shape{Listeners: []string{"http"}, Routes: []string{"orders"}})
	if err != nil {
		t.Fatal(err)
	}
	r.Meter().Bind(plan)
	trace := [16]byte{0xab, 1}
	plan.Route("orders").Duration.RecordExemplar(0, 1_500_000, emit.Exemplar{TraceID: trace, SpanID: [8]byte{0xcd, 2}, Time: r.clk.Now()})
	r.clk.(*clocktest.Fake).Advance(2 * aggregate.ReuseWindow)
	text, _ := scrapeText(t, r, "")
	om, _ := scrapeText(t, r, openMetrics)
	if strings.Contains(text, "trace_id") {
		t.Error("text exposition carries an exemplar")
	}
	if want := `trace_id="` + tracing.FormatTraceID(trace) + `"`; !strings.Contains(om, want) {
		t.Errorf("OpenMetrics lacks the exemplar %s", want)
	}
}

// TestScrapeReleasesItsCollections checks that a scrape hands the
// collections it read back to the producer.
func TestScrapeReleasesItsCollections(t *testing.T) {
	r, _ := newRuntime(t, nil)
	scrape(t, r)
	r.metrics.pmu.Lock()
	left := len(r.metrics.produced)
	r.metrics.pmu.Unlock()
	if left != 0 {
		t.Errorf("%d collections not released", left)
	}
}
