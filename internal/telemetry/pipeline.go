// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/telemetry/tracing"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// Export bounds shared by the three signals
// (docs/architecture/10-observability.md "Telemetry pipeline").
const (
	// exportTimeout bounds one export with its retries.
	exportTimeout = 10 * time.Second
	// retryInitial, retryMax and retryElapsed are the exporters' retry:
	// from 1 s up to 5 s apart, giving up after 10 s.
	retryInitial = time.Second
	retryMax     = 5 * time.Second
	retryElapsed = 10 * time.Second
	// metricInterval is the OTLP metric push interval, also the bound of
	// telemetry_export_failing.
	metricInterval = 15 * time.Second
	// retireTimeout bounds the shutdown of a replaced pipeline.
	retireTimeout = 5 * time.Second
)

// exportBounds bound one export and its retries; a retryElapsed of 0
// disables retry.
type exportBounds struct {
	timeout, retryInitial, retryMax, retryElapsed time.Duration
}

// defaultExportBounds returns the documented bounds.
func defaultExportBounds() exportBounds {
	return exportBounds{timeout: exportTimeout, retryInitial: retryInitial, retryMax: retryMax, retryElapsed: retryElapsed}
}

// exportRetry runs exports under the documented bounds on the runtime's
// clock, so they are Node timers the controllable clock drives
// (docs/engineering/03-testing-and-quality-strategy.md "Hermetic rule
// scope"): an export and its retries end within timeout of its start, a
// retry waits from retryInitial, doubling up to retryMax, and starts only
// within retryElapsed of the start, and only a retryable gRPC status is
// retried. The SDK exporters run with their own timeout and retry off.
type exportRetry struct {
	clk clock.Clock
	b   exportBounds
}

// run calls export until it succeeds, fails for good, or the bounds or
// ctx end it; it returns the last error.
func (x exportRetry) run(ctx context.Context, export func(context.Context) error) error {
	start := x.clk.Now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if x.b.timeout > 0 {
		t := x.clk.AfterFunc(x.b.timeout, cancel)
		defer t.Stop()
	}
	wait := x.b.retryInitial
	for {
		err := export(ctx)
		if err == nil || x.b.retryElapsed <= 0 || ctx.Err() != nil || !retryable(err) {
			return err
		}
		if x.clk.Since(start)+wait > x.b.retryElapsed {
			return err
		}
		if x.clk.Sleep(ctx, wait) != nil {
			return err
		}
		wait = min(2*wait, x.b.retryMax)
	}
}

// retryable reports whether an export error is worth another attempt: the
// gRPC codes the OTLP exporters retry, ResourceExhausted aside, which they
// retry only with a server delay this runtime does not read.
func retryable(err error) bool {
	s, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch s.Code() {
	case codes.Canceled, codes.DeadlineExceeded, codes.Aborted, codes.OutOfRange, codes.Unavailable, codes.DataLoss:
		return true
	default:
		return false
	}
}

// retryingSpans bounds and retries every span export (exportRetry).
type retryingSpans struct {
	sdktrace.SpanExporter
	retry exportRetry
}

// ExportSpans implements sdktrace.SpanExporter.
func (e *retryingSpans) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	return e.retry.run(ctx, func(ctx context.Context) error { return e.SpanExporter.ExportSpans(ctx, spans) })
}

// retryingLogs bounds and retries every log export (exportRetry).
type retryingLogs struct {
	sdklog.Exporter
	retry exportRetry
}

// Export implements sdklog.Exporter.
func (e *retryingLogs) Export(ctx context.Context, recs []sdklog.Record) error {
	return e.retry.run(ctx, func(ctx context.Context) error { return e.Exporter.Export(ctx, recs) })
}

// pipeline is the OTLP export of one endpoint setting: one gRPC connection
// shared by the span, metric and log exporters. The span exporter belongs
// to the tracer once installed; the rest belongs to the pipeline.
type pipeline struct {
	key       string
	cleartext bool
	conn      *grpc.ClientConn
	tls       *tlsconf.ClientConfig
	spans     sdktrace.SpanExporter
	mp        *sdkmetric.MeterProvider
	lp        *sdklog.LoggerProvider
	process   *processBridge
	access    *accessBridge
	// current is cleared when the pipeline is replaced, so the failures
	// of its final exports do not raise telemetry_export_failing.
	current atomic.Bool
	// push sends the shared collection to the collector.
	push *metricPusher
}

// newPipeline builds the pipeline of s, which must have an endpoint; it
// adopts s's TLS configuration. Building dials nothing: the connection
// opens on the first export.
func (r *Runtime) newPipeline(ctx context.Context, s Settings) (*pipeline, error) {
	creds := insecure.NewCredentials()
	if s.tls != nil {
		creds = &rotatingTLS{cfg: s.tls}
	}
	conn, err := grpc.NewClient(s.target, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("telemetry: OTLP connection: %w", err)
	}
	p := &pipeline{key: s.key, cleartext: s.cleartext, conn: conn, tls: s.tls}
	p.current.Store(true)
	report := func(sig signal) func(error) {
		return func(err error) {
			if p.current.Load() {
				r.health.record(sig, err)
			}
		}
	}
	retry := exportRetry{clk: r.clk, b: r.export}
	off := otlptracegrpc.RetryConfig{Enabled: false}
	spans, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithGRPCConn(conn), otlptracegrpc.WithTimeout(0), otlptracegrpc.WithRetry(off),
	)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("telemetry: OTLP span exporter: %w", err), conn.Close())
	}
	p.spans = &retryingSpans{SpanExporter: spans, retry: retry}
	metrics, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithGRPCConn(conn), otlpmetricgrpc.WithTimeout(0),
		otlpmetricgrpc.WithRetry(otlpmetricgrpc.RetryConfig{Enabled: false}),
	)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("telemetry: OTLP metric exporter: %w", err), spans.Shutdown(ctx), conn.Close())
	}
	reader := sdkmetric.NewManualReader(sdkmetric.WithProducer(&syncProducer{inner: r.meter.Producer(), stats: r.streams}))
	p.mp = sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(r.res))
	p.push = newMetricPusher(reader,
		&releasingExporter{Exporter: metrics, inner: r.meter.Producer(), report: report(signalMetrics), retry: retry},
		r.clk, r.metricInterval)
	logs, err := otlploggrpc.New(ctx,
		otlploggrpc.WithGRPCConn(conn), otlploggrpc.WithTimeout(0),
		otlploggrpc.WithRetry(otlploggrpc.RetryConfig{Enabled: false}),
	)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("telemetry: OTLP log exporter: %w", err), spans.Shutdown(ctx), p.push.shutdown(ctx), p.mp.Shutdown(ctx), conn.Close())
	}
	proc := newLogProcessor(&retryingLogs{Exporter: logs, retry: retry}, r.clk, &r.drops, report(signalLogs))
	p.lp = sdklog.NewLoggerProvider(sdklog.WithResource(r.res), sdklog.WithProcessor(proc))
	proc.setLogger(p.lp.Logger(tracing.ScopeName, otellog.WithInstrumentationVersion(r.version)))
	p.process = &processBridge{
		h:     otelslog.NewHandler(tracing.ScopeName, otelslog.WithLoggerProvider(p.lp), otelslog.WithVersion(r.version)),
		drops: &r.drops,
	}
	p.access = &accessBridge{p: proc}
	return p, nil
}

// shutdown makes the final metric export, flushes the queued logs, then
// closes the connection and stops following the TLS secrets. The span
// exporter is shut down by its owner first. The logs are flushed before
// the provider shuts down, since a queued access record is encoded through
// the provider, which emits nothing once it stops.
func (p *pipeline) shutdown(ctx context.Context) error {
	p.current.Store(false)
	err := errors.Join(p.push.shutdown(ctx), p.mp.Shutdown(ctx), p.lp.ForceFlush(ctx), p.lp.Shutdown(ctx), p.conn.Close())
	if p.tls != nil {
		p.tls.Close()
	}
	return err
}

// metricPusher pushes the collection /metrics reads to the OTLP metric
// exporter every interval on the runtime's clock, so the push is a Node
// timer the controllable clock drives, and makes a final push at
// shutdown. Its goroutine is stopped and awaited by shutdown.
type metricPusher struct {
	reader   *sdkmetric.ManualReader
	exp      *releasingExporter
	clk      clock.Clock
	interval time.Duration
	timer    clock.Timer

	mu     sync.Mutex // serializes pushes: the timer's and ForceFlush's
	stop   chan struct{}
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

// newMetricPusher starts the push loop. The timer exists before the loop
// runs, so a clock advanced right after construction still fires it.
func newMetricPusher(reader *sdkmetric.ManualReader, exp *releasingExporter, clk clock.Clock, interval time.Duration) *metricPusher {
	ctx, cancel := context.WithCancel(context.Background())
	m := &metricPusher{
		reader: reader, exp: exp, clk: clk, interval: interval, timer: clk.NewTimer(interval),
		stop: make(chan struct{}), done: make(chan struct{}), ctx: ctx, cancel: cancel,
	}
	go m.run()
	return m
}

// run pushes every interval until stopped. The timer is re-armed before
// each push, so pushes keep one interval apart from fire to fire, however
// long a push takes.
func (m *metricPusher) run() {
	defer close(m.done)
	defer m.timer.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-m.timer.C():
			m.timer.Reset(m.interval)
			_ = m.pushNow(m.ctx)
		}
	}
}

// pushNow collects once and exports the collection within ctx.
func (m *metricPusher) pushNow(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rm metricdata.ResourceMetrics
	if err := m.reader.Collect(ctx, &rm); err != nil {
		return fmt.Errorf("telemetry: metric collection: %w", err)
	}
	return m.exp.Export(ctx, &rm)
}

// shutdown stops the loop, cutting a push in flight when ctx ends, then
// makes the final push within ctx and shuts the exporter down. Later
// calls return nil.
func (m *metricPusher) shutdown(ctx context.Context) error {
	var err error
	m.once.Do(func() {
		close(m.stop)
		stopCut := context.AfterFunc(ctx, m.cancel)
		<-m.done
		stopCut()
		m.cancel()
		err = errors.Join(m.pushNow(ctx), m.exp.Shutdown(ctx))
	})
	return err
}

// rotatingTLS is the gRPC transport credentials of an https collector:
// each handshake takes the current configuration of the collector's TLS
// client settings, so a rotated CA bundle reaches the next connection
// without rebuilding the exporters (a rotated client certificate already
// reaches every handshake through GetClientCertificate).
type rotatingTLS struct {
	cfg *tlsconf.ClientConfig
}

// Compile-time check of the contract.
var _ credentials.TransportCredentials = (*rotatingTLS)(nil)

// ClientHandshake runs the TLS handshake with the current configuration.
func (c *rotatingTLS) ClientHandshake(ctx context.Context, authority string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	host, _, err := net.SplitHostPort(authority)
	if err != nil {
		host = authority
	}
	return credentials.NewTLS(c.cfg.ForHost(host)).ClientHandshake(ctx, authority, raw)
}

// ServerHandshake is not supported: these credentials dial only.
func (c *rotatingTLS) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("telemetry: OTLP credentials are client-only")
}

// Info returns the TLS protocol information.
func (c *rotatingTLS) Info() credentials.ProtocolInfo {
	return credentials.NewTLS(c.cfg.Current()).Info()
}

// Clone returns credentials sharing the same configuration.
func (c *rotatingTLS) Clone() credentials.TransportCredentials { return &rotatingTLS{cfg: c.cfg} }

// OverrideServerName implements the deprecated method; the server name
// comes from the TLS settings.
func (c *rotatingTLS) OverrideServerName(string) error { return nil }
