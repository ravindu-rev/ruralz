// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/redact"
	"github.com/ravindu-rev/ruralz/internal/telemetry/accesslog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/aggregate"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/telemetry/logsink"
	"github.com/ravindu-rev/ruralz/internal/telemetry/tracing"
)

// componentTelemetry is the component of the runtime's own log records.
const componentTelemetry = "telemetry"

// ErrClosed is returned by Apply after Shutdown.
var ErrClosed = errors.New("telemetry: runtime shut down")

// Options configure a Runtime.
type Options struct {
	// Service is the binary.
	Service Service
	// NodeID is the service.instance.id and the node_id of every log line
	// (the ULID of internal/nodedir).
	NodeID string
	// Version is the service.version (buildinfo.Get().Version).
	Version string
	// Stdout receives the process and access log lines; required.
	Stdout io.Writer
	// Level is the process log level (logsink.ParseLevel of
	// RURALZ_LOG_LEVEL).
	Level slog.Level
	// Clock is the time source; nil is clock.Real().
	Clock clock.Clock
	// Stripes is the metric stripe count; 0 is min(GOMAXPROCS, 8).
	Stripes int
	// MetricInterval overrides the 15 s OTLP metric push interval, and
	// with it the telemetry_export_failing bound, when positive (tests).
	MetricInterval time.Duration
}

// Runtime is the telemetry of one process: the metric registry, the
// tracer, the process and access logs, the degraded-state gauges, the
// /metrics handler and the OTLP export of the active Revision.
type Runtime struct {
	clk            clock.Clock
	res            *resource.Resource
	version        string
	metricInterval time.Duration
	// export bounds the OTLP exports of the pipelines built after it is
	// set; tests shorten it.
	export exportBounds

	meter   *aggregate.Registry
	sink    *logsink.Sink
	access  *accesslog.Writer
	tracer  *tracing.Tracer
	status  *nodeStatus
	health  *exportHealth
	streams *streamStats
	drops   logDrops
	metrics *metricsHandler
	log     *slog.Logger

	// ratio holds the float64 bits of the root sampling ratio.
	ratio atomic.Uint64

	// worker is the stdout worker: stopWorker cancels it, workerDone
	// closes when it returns.
	stopWorker context.CancelFunc
	workerDone chan struct{}

	mu     sync.Mutex
	pipe   *pipeline
	closed bool
	// retiring tracks the retirements of replaced pipelines; cancels holds
	// their cancel functions by number, so Shutdown can cut them short.
	retiring   sync.WaitGroup
	cancels    map[uint64]context.CancelFunc
	nextCancel uint64
}

// Compile-time check of the contract.
var _ emit.NodeStatus = (*Runtime)(nil)

// New builds the runtime of a process and starts its stdout worker. It
// opens no connection: OTLP export starts with the first Apply that sets
// an endpoint.
func New(o Options) (*Runtime, error) {
	if o.Stdout == nil {
		return nil, logsink.ErrNoStdout
	}
	res, err := newResource(o.Service, o.Version, o.NodeID)
	if err != nil {
		return nil, err
	}
	r := &Runtime{
		clk:            o.Clock,
		res:            res,
		version:        o.Version,
		metricInterval: metricInterval,
		export:         defaultExportBounds(),
		workerDone:     make(chan struct{}),
		cancels:        map[uint64]context.CancelFunc{},
	}
	if r.clk == nil {
		r.clk = clock.Real()
	}
	if o.MetricInterval > 0 {
		r.metricInterval = o.MetricInterval
	}
	r.ratio.Store(math.Float64bits(tracing.DefaultTraceSampling))
	r.access = accesslog.New(accesslog.Options{NodeID: o.NodeID})
	r.sink, err = logsink.New(logsink.Options{
		Stdout:       o.Stdout,
		Level:        o.Level,
		NodeID:       o.NodeID,
		Clock:        r.clk,
		TraceContext: traceContext,
		ReplaceAttr:  redact.Default().ReplaceAttr,
		Access:       r.access,
	})
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}
	r.log = r.sink.Logger(componentTelemetry)
	if err := r.build(o); err != nil {
		r.sink.Close()
		r.access.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.stopWorker = cancel
	go func() {
		defer close(r.workerDone)
		_ = r.sink.Run(ctx)
	}()
	return r, nil
}

// build creates the registry and what feeds it.
func (r *Runtime) build(o Options) error {
	codes := errcode.All()
	ids := make([]string, len(codes))
	for i, c := range codes {
		ids[i] = c.ID
	}
	var err error
	r.meter, err = aggregate.New(aggregate.Options{
		Clock: r.clk, Stripes: o.Stripes, Codes: ids, Logger: r.log, ScopeVersion: o.Version,
	})
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	if r.status, err = newNodeStatus(r.meter, r.log); err != nil {
		return err
	}
	r.health = &exportHealth{clk: r.clk, interval: r.metricInterval, status: r.status, log: r.log}
	r.streams, err = newStreamStats(r.meter, r.streamCounts)
	if err != nil {
		return err
	}
	counters, err := tracingCounters(r.meter)
	if err != nil {
		return err
	}
	r.tracer, err = tracing.New(tracing.Options{
		Clock:         r.clk,
		Resource:      r.res,
		Counters:      counters,
		OnExport:      func(_ int, err error) { r.health.record(signalSpans, err) },
		ExportTimeout: exportTimeout,
	})
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	r.metrics, err = newMetricsHandler(r.meter.Producer(), r.streams, r.res, r.clk)
	return err
}

// tracingCounters resolves the tracing counters in the registry.
func tracingCounters(reg *aggregate.Registry) (tracing.Counters, error) {
	var c tracing.Counters
	var errs []error
	get := func(name string, values ...string) emit.Counter {
		h, err := reg.Counter(name, values...)
		errs = append(errs, err)
		return h
	}
	c.Spans = get(catalog.TelemetrySpansTotal)
	c.DroppedQueueFull = get(catalog.TelemetrySpansDroppedTotal, "queue_full")
	c.DroppedExportError = get(catalog.TelemetrySpansDroppedTotal, "export_error")
	c.UnsampledRoot = get(catalog.TelemetryTracesUnsampledTotal, "rate_cap_root")
	c.UnsampledParent = get(catalog.TelemetryTracesUnsampledTotal, "rate_cap_parent")
	if err := errors.Join(errs...); err != nil {
		return tracing.Counters{}, fmt.Errorf("telemetry: %w", err)
	}
	return c, nil
}

// streamCounts reads both streams' counters: stdout's losses from the
// streams, the OTLP sink's from the log processors.
func (r *Runtime) streamCounts() [numStreams]streamCounts {
	var out [numStreams]streamCounts
	acc, proc := r.access.Stats(), r.sink.Stats()
	out[streamAccess].produced = acc.Produced
	out[streamAccess].queueFull[sinkStdout] = acc.QueueFull
	out[streamAccess].exportError[sinkStdout] = acc.ExportError
	out[streamProcess].produced = proc.Produced
	out[streamProcess].queueFull[sinkStdout] = proc.QueueFull
	out[streamProcess].exportError[sinkStdout] = proc.ExportError
	for s := range numStreams {
		out[s].queueFull[sinkOTLP] = r.drops.queueFull[s].Load()
		out[s].exportError[sinkOTLP] = r.drops.exportError[s].Load()
	}
	return out
}

// traceContext gives the process logs the span context of ctx.
func traceContext(ctx context.Context) (traceID [16]byte, spanID [8]byte, ok bool) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return traceID, spanID, false
	}
	return sc.TraceID(), sc.SpanID(), true
}

// Logger returns a process logger whose records carry component.
func (r *Runtime) Logger(component string) *slog.Logger { return r.sink.Logger(component) }

// ErrorLog returns the handler http.Server.ErrorLog is bridged to, rate
// limited (logsink.Sink.ErrorLog).
func (r *Runtime) ErrorLog(component string) slog.Handler { return r.sink.ErrorLog(component) }

// Meter returns the metric registry (emit.Meter).
func (r *Runtime) Meter() *aggregate.Registry { return r.meter }

// Tracer returns the tracer (emit.Tracer).
func (r *Runtime) Tracer() *tracing.Tracer { return r.tracer }

// AccessLog returns the access log (emit.AccessLog).
func (r *Runtime) AccessLog() *accesslog.Writer { return r.access }

// SamplingRatio returns the root sampling ratio of the active Revision,
// the ratio argument of Tracer.Decide.
func (r *Runtime) SamplingRatio() float64 { return math.Float64frombits(r.ratio.Load()) }

// SetDegraded raises or clears reason rs for source (emit.NodeStatus).
func (r *Runtime) SetDegraded(rs catalog.Reason, source string, on bool) {
	r.status.SetDegraded(rs, source, on)
}

// SetCleartextHops sets the count of cleartext hops of kind h
// (emit.NodeStatus).
func (r *Runtime) SetCleartextHops(h catalog.Hop, n int) { r.status.SetCleartextHops(h, n) }

// SetActiveRevision sets the Revision display ("rev-<12 hex>") later
// process records carry; empty removes it.
func (r *Runtime) SetActiveRevision(display string) { r.sink.SetRevision(display) }

// SetCredentialHeaders sets the header names the process logs redact
// besides the built-in credential headers (logsink.Sink.SetCredentialHeaders).
func (r *Runtime) SetCredentialHeaders(names []string) { r.sink.SetCredentialHeaders(names) }

// MetricsHandler returns the /metrics handler the admin server mounts.
func (r *Runtime) MetricsHandler() http.Handler { return r.metrics }

// RouteLibraryErrors makes the OpenTelemetry SDK report its errors as
// telemetry WARN records, at most logsink.ErrorLogRate per second, instead
// of writing them to stderr. It changes a library global, so a binary
// calls it once at start.
func (r *Runtime) RouteLibraryErrors() {
	l := slog.New(logsink.RateLimit(r.sink.Handler(componentTelemetry), r.clk, logsink.ErrorLogRate))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		l.Warn("OpenTelemetry SDK error", slog.Any("error", err))
	}))
}

// Apply installs the telemetry settings of the activated Revision: the
// sampling ratio at once, and for a changed endpoint or TLS setting a new
// OTLP pipeline swapped in off the request path, with the old one retired
// within 5 s. Unchanged settings keep the running pipeline. Apply takes
// ownership of s; after Shutdown it returns ErrClosed.
func (r *Runtime) Apply(ctx context.Context, s Settings) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		s.Close()
		return ErrClosed
	}
	r.ratio.Store(math.Float64bits(s.sampling))
	cur := r.pipe
	if cur != nil && cur.key == s.key {
		s.Close()
		return nil
	}
	if cur == nil && s.target == "" {
		s.Close()
		return nil
	}
	var next *pipeline
	if s.target != "" {
		var err error
		if next, err = r.newPipeline(ctx, s); err != nil {
			s.Close()
			return err
		}
	} else {
		s.Close()
	}
	var old sdktrace.SpanExporter
	if next != nil {
		old = r.tracer.SetExporter(next.spans)
		r.sink.SetBridge(next.process)
		r.access.SetBridge(next.access)
	} else {
		old = r.tracer.SetExporter(nil)
		r.sink.SetBridge(nil)
		r.access.SetBridge(nil)
	}
	r.pipe = next
	hops := 0
	if next != nil && next.cleartext {
		hops = 1
	}
	r.status.SetCleartextHops(catalog.HopTelemetry, hops)
	r.health.reset()
	if cur != nil {
		cur.current.Store(false)
		// The retirement outlives this activation, so it keeps ctx's values
		// but not its cancellation, and ends within retireTimeout on the
		// runtime's clock or when Shutdown cuts it short.
		rctx, cancelCtx := context.WithCancel(context.WithoutCancel(ctx))
		bound := r.clk.AfterFunc(retireTimeout, cancelCtx)
		cancel := func() {
			bound.Stop()
			cancelCtx()
		}
		id := r.nextCancel
		r.nextCancel++
		r.cancels[id] = cancel
		r.retiring.Go(func() {
			r.retire(rctx, cur, old)
			r.mu.Lock()
			delete(r.cancels, id)
			r.mu.Unlock()
			cancel()
		})
	}
	return nil
}

// retire shuts a replaced pipeline and its span exporter down within ctx.
func (r *Runtime) retire(ctx context.Context, p *pipeline, spans sdktrace.SpanExporter) {
	var errs []error
	if spans != nil {
		errs = append(errs, spans.Shutdown(ctx))
	}
	errs = append(errs, p.shutdown(ctx))
	if err := errors.Join(errs...); err != nil {
		r.log.WarnContext(ctx, "retiring the previous OTLP pipeline", slog.Any("error", err))
	}
}

// cancelRetirements cuts every running retirement short.
func (r *Runtime) cancelRetirements() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, cancel := range r.cancels {
		cancel()
	}
}

// Shutdown flushes and stops every signal within ctx (5 s at Drain): the
// stdout worker writes what is queued and hands it to OTLP, the tracer
// exports its spans, the pipeline makes its final metric export and
// flushes its logs, and every goroutine ends. Records still queued when
// ctx ends are dropped and counted. Later calls return nil.
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	pipe := r.pipe
	r.pipe = nil
	r.mu.Unlock()

	var errs []error
	r.stopWorker()
	select {
	case <-r.workerDone:
	case <-ctx.Done():
		// A write blocked on stdout cannot be interrupted; give up on it.
		errs = append(errs, fmt.Errorf("telemetry: stdout worker: %w", ctx.Err()))
	}
	r.sink.Close()
	r.access.Close()
	errs = append(errs, r.tracer.Shutdown(ctx))
	if pipe != nil {
		errs = append(errs, pipe.shutdown(ctx))
	}
	// A retirement still running when ctx ends is cut short, so the wait
	// below ends with it.
	stop := context.AfterFunc(ctx, r.cancelRetirements)
	r.retiring.Wait()
	stop()
	errs = append(errs, r.metrics.shutdown(ctx))
	return errors.Join(errs...)
}
