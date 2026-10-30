// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/otlptranslator"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/telemetry/aggregate"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// /metrics bounds (docs/architecture/10-observability.md "Observability
// principles").
const (
	// scrapeSlots is how many scrapes encode at once.
	scrapeSlots = 4
	// scrapeWait is the longest a scrape waits for a slot before 503.
	scrapeWait = 10 * time.Second
	// scrapeWrite cuts off a scrape still writing.
	scrapeWrite = 10 * time.Second
	// scrapeBuffer is the size of a scrape's pooled write buffer.
	scrapeBuffer = 64 << 10
)

// streamCounts are the cumulative counts of one log stream: the records
// produced, and the records each sink lost by reason.
type streamCounts struct {
	produced               uint64
	queueFull, exportError [numSinks]uint64
}

// streamStats feeds ruralz_telemetry_logs_total and
// ruralz_telemetry_logs_dropped_total from the streams' and sinks' own
// counters: before each collection it adds what each counted since the
// previous one. A record a sink loses counts once under that sink, so one
// record lost by stdout and by OTLP counts once under each.
type streamStats struct {
	read func() [numStreams]streamCounts

	produced               [numStreams]emit.Counter
	queueFull, exportError [numStreams][numSinks]emit.Counter

	mu   sync.Mutex
	last [numStreams]streamCounts
}

// newStreamStats resolves the counters of both streams and both sinks.
func newStreamStats(reg *aggregate.Registry, read func() [numStreams]streamCounts) (*streamStats, error) {
	s := &streamStats{read: read}
	streams := [numStreams]string{streamAccess: "access", streamProcess: "process"}
	sinks := catalog.LogSinks()
	for i, name := range streams {
		var err error
		if s.produced[i], err = reg.Counter(catalog.TelemetryLogsTotal, name); err != nil {
			return nil, fmt.Errorf("telemetry: %w", err)
		}
		for k := range numSinks {
			if s.queueFull[i][k], err = reg.Counter(catalog.TelemetryLogsDroppedTotal, name, sinks[k], "queue_full"); err != nil {
				return nil, fmt.Errorf("telemetry: %w", err)
			}
			if s.exportError[i][k], err = reg.Counter(catalog.TelemetryLogsDroppedTotal, name, sinks[k], "export_error"); err != nil {
				return nil, fmt.Errorf("telemetry: %w", err)
			}
		}
	}
	return s, nil
}

// sync adds the counts since the previous call.
func (s *streamStats) sync() {
	now := s.read()
	s.mu.Lock()
	defer s.mu.Unlock()
	add := func(c emit.Counter, cur, prev uint64) {
		if cur > prev {
			c.Add(0, cur-prev)
		}
	}
	for i := range numStreams {
		add(s.produced[i], now[i].produced, s.last[i].produced)
		for k := range numSinks {
			add(s.queueFull[i][k], now[i].queueFull[k], s.last[i].queueFull[k])
			add(s.exportError[i][k], now[i].exportError[k], s.last[i].exportError[k])
		}
		s.last[i] = now[i]
	}
}

// syncProducer is the sdkmetric.Producer both metric exports read: it
// brings the stream counters up to date, then collects the registry.
// record, when set, receives each result, so the /metrics handler can
// release it after Gather.
type syncProducer struct {
	inner  *aggregate.Producer
	stats  *streamStats
	record func([]metricdata.ScopeMetrics)
}

// Compile-time check of the contract.
var _ sdkmetric.Producer = (*syncProducer)(nil)

// Produce implements sdkmetric.Producer.
func (p *syncProducer) Produce(ctx context.Context) ([]metricdata.ScopeMetrics, error) {
	p.stats.sync()
	sm, err := p.inner.Produce(ctx)
	if err == nil && p.record != nil {
		p.record(sm)
	}
	return sm, err
}

// releasingExporter wraps the OTLP metric exporter: it bounds and retries
// each export (exportRetry), then releases the collection to the
// producer's pool and reports the result.
type releasingExporter struct {
	sdkmetric.Exporter
	inner  *aggregate.Producer
	report func(error)
	retry  exportRetry
}

// Export implements sdkmetric.Exporter.
func (e *releasingExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := e.retry.run(ctx, func(ctx context.Context) error { return e.Exporter.Export(ctx, rm) })
	e.inner.ReleaseResource(rm)
	e.report(err)
	return err
}

// metricsHandler serves /metrics: the OpenTelemetry Prometheus exporter on
// a private registry, fed by the shared producer, so /metrics and OTLP
// expose one name set (ADR-0018). Scrapes share one Gather for up to the
// producer's reuse window; at most scrapeSlots encode at once.
type metricsHandler struct {
	reg   *prometheus.Registry
	mp    *sdkmetric.MeterProvider
	inner *aggregate.Producer
	clk   clock.Clock
	slots chan struct{}
	bufs  sync.Pool

	// gmu serializes Gather and guards the cached result: a function
	// encoding the gathered families, which concurrent scrapes share
	// read-only.
	gmu      sync.Mutex
	cached   func(expfmt.Encoder) error
	cachedAt time.Time

	// pmu guards produced: the collections of the Gather in progress,
	// which the Prometheus collector may make on another goroutine.
	pmu      sync.Mutex
	produced [][]metricdata.ScopeMetrics
}

// newMetricsHandler builds the exporter and its meter provider.
func newMetricsHandler(inner *aggregate.Producer, stats *streamStats, res *resource.Resource, clk clock.Clock) (*metricsHandler, error) {
	h := &metricsHandler{
		reg:   prometheus.NewRegistry(),
		inner: inner,
		clk:   clk,
		slots: make(chan struct{}, scrapeSlots),
	}
	h.bufs.New = func() any { return bufio.NewWriterSize(nil, scrapeBuffer) }
	prod := &syncProducer{inner: inner, stats: stats, record: h.recordProduced}
	exp, err := otelprom.New(
		otelprom.WithRegisterer(h.reg),
		otelprom.WithProducer(prod),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithoutSuffixes),
		otelprom.WithoutScopeInfo(),
		otelprom.WithoutTargetInfo(),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: prometheus exporter: %w", err)
	}
	h.mp = sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp), sdkmetric.WithResource(res))
	return h, nil
}

// recordProduced keeps a collection of the Gather in progress.
func (h *metricsHandler) recordProduced(sm []metricdata.ScopeMetrics) {
	h.pmu.Lock()
	h.produced = append(h.produced, sm)
	h.pmu.Unlock()
}

// gather returns the encoder of a Gather at most the reuse window old,
// releasing the collections the Gather read once it has copied them.
func (h *metricsHandler) gather() (func(expfmt.Encoder) error, error) {
	h.gmu.Lock()
	defer h.gmu.Unlock()
	if h.cached != nil && h.clk.Since(h.cachedAt) < aggregate.ReuseWindow {
		return h.cached, nil
	}
	mfs, err := h.reg.Gather()
	h.pmu.Lock()
	produced := h.produced
	h.produced = nil
	h.pmu.Unlock()
	for _, sm := range produced {
		h.inner.Release(sm)
	}
	if err != nil {
		return nil, err
	}
	h.cached = func(enc expfmt.Encoder) error {
		for _, mf := range mfs {
			if err := enc.Encode(mf); err != nil {
				return err
			}
		}
		return nil
	}
	h.cachedAt = h.clk.Now()
	return h.cached, nil
}

// acquire takes a scrape slot, waiting up to scrapeWait on the runtime's
// clock, so the wait is a Node timer the controllable clock drives
// (docs/engineering/03-testing-and-quality-strategy.md "Hermetic rule
// scope"); false when the wait ends or the scrape goes away first.
func (h *metricsHandler) acquire(ctx context.Context) bool {
	select {
	case h.slots <- struct{}{}:
		return true
	default:
	}
	t := h.clk.NewTimer(scrapeWait)
	defer t.Stop()
	select {
	case h.slots <- struct{}{}:
		return true
	case <-t.C():
		return false
	case <-ctx.Done():
		return false
	}
}

// ServeHTTP implements http.Handler.
func (h *metricsHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.acquire(req.Context()) {
		http.Error(w, "too many concurrent scrapes", http.StatusServiceUnavailable)
		return
	}
	defer func() { <-h.slots }()
	encode, err := h.gather()
	if err != nil {
		http.Error(w, "metric collection failed", http.StatusInternalServerError)
		return
	}
	// The deadline is a socket deadline, so it follows the real clock.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(scrapeWrite))
	// Text and OpenMetrics only: OpenMetrics carries the exemplars.
	format := expfmt.NegotiateAccept(req.Header, expfmt.NewFormat(expfmt.TypeOpenMetrics), expfmt.NewFormat(expfmt.TypeTextPlain))
	w.Header().Set("Content-Type", string(format))
	if req.Method == http.MethodHead {
		return
	}
	bw, _ := h.bufs.Get().(*bufio.Writer)
	bw.Reset(w)
	defer func() {
		bw.Reset(nil)
		h.bufs.Put(bw)
	}()
	enc := expfmt.NewEncoder(bw, format)
	if err := encode(enc); err != nil {
		return
	}
	if c, ok := enc.(expfmt.Closer); ok {
		if err := c.Close(); err != nil {
			return
		}
	}
	_ = bw.Flush()
}

// shutdown releases the exporter's meter provider.
func (h *metricsHandler) shutdown(ctx context.Context) error { return h.mp.Shutdown(ctx) }
