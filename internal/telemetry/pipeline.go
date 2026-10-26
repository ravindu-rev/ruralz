// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/ravindu-rev/ruralz/internal/telemetry/tracing"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// Export bounds shared by the three signals
// (docs/architecture/10-observability.md "Telemetry pipeline").
const (
	// exportTimeout bounds one export attempt.
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

// exportBounds bound one export attempt and the exporters' retry; a
// retryElapsed of 0 disables retry.
type exportBounds struct {
	timeout, retryInitial, retryMax, retryElapsed time.Duration
}

// defaultExportBounds returns the documented bounds.
func defaultExportBounds() exportBounds {
	return exportBounds{timeout: exportTimeout, retryInitial: retryInitial, retryMax: retryMax, retryElapsed: retryElapsed}
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
	b := r.export
	retry := b.retryElapsed > 0
	spans, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithGRPCConn(conn),
		otlptracegrpc.WithTimeout(b.timeout),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: retry, InitialInterval: b.retryInitial, MaxInterval: b.retryMax, MaxElapsedTime: b.retryElapsed}),
	)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("telemetry: OTLP span exporter: %w", err), conn.Close())
	}
	p.spans = spans
	metrics, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithGRPCConn(conn),
		otlpmetricgrpc.WithTimeout(b.timeout),
		otlpmetricgrpc.WithRetry(otlpmetricgrpc.RetryConfig{Enabled: retry, InitialInterval: b.retryInitial, MaxInterval: b.retryMax, MaxElapsedTime: b.retryElapsed}),
	)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("telemetry: OTLP metric exporter: %w", err), spans.Shutdown(ctx), conn.Close())
	}
	reader := sdkmetric.NewPeriodicReader(
		&releasingExporter{Exporter: metrics, inner: r.meter.Producer(), report: report(signalMetrics)},
		sdkmetric.WithInterval(r.metricInterval),
		sdkmetric.WithTimeout(b.timeout),
		sdkmetric.WithProducer(&syncProducer{inner: r.meter.Producer(), stats: r.streams}),
	)
	p.mp = sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(r.res))
	logs, err := otlploggrpc.New(ctx,
		otlploggrpc.WithGRPCConn(conn),
		otlploggrpc.WithTimeout(b.timeout),
		otlploggrpc.WithRetry(otlploggrpc.RetryConfig{Enabled: retry, InitialInterval: b.retryInitial, MaxInterval: b.retryMax, MaxElapsedTime: b.retryElapsed}),
	)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("telemetry: OTLP log exporter: %w", err), spans.Shutdown(ctx), p.mp.Shutdown(ctx), conn.Close())
	}
	proc := newLogProcessor(logs, r.clk, &r.drops, b.timeout, report(signalLogs))
	p.lp = sdklog.NewLoggerProvider(sdklog.WithResource(r.res), sdklog.WithProcessor(proc))
	p.process = &processBridge{h: otelslog.NewHandler(tracing.ScopeName, otelslog.WithLoggerProvider(p.lp), otelslog.WithVersion(r.version))}
	p.access = &accessBridge{l: p.lp.Logger(tracing.ScopeName)}
	return p, nil
}

// shutdown makes the final metric export, flushes the queued logs, then
// closes the connection and stops following the TLS secrets. The span
// exporter is shut down by its owner first.
func (p *pipeline) shutdown(ctx context.Context) error {
	p.current.Store(false)
	err := errors.Join(p.mp.Shutdown(ctx), p.lp.Shutdown(ctx), p.conn.Close())
	if p.tls != nil {
		p.tls.Close()
	}
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
