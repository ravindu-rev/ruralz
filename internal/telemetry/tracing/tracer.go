// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// ScopeName is the instrumentation scope of every Ruralz span.
const ScopeName = "github.com/ravindu-rev/ruralz"

// Span limits of spec 09 req 22 (proposed there): they bound the size of a
// queued span and so the 8 MiB span queue. A span cuts every string value
// to at most MaxAttributeValueLen bytes, at a UTF-8 rune boundary, before
// the SDK sees it, because the SDK's AttributeValueLengthLimit counts
// characters, not bytes.
const (
	MaxSpanAttributes     = 32
	MaxAttributeValueLen  = 256
	MaxSpanEvents         = 8
	MaxSpanLinks          = 0
	MaxAttributesPerEvent = 32
)

// SpanLimits returns the raw span limits installed with
// sdktrace.WithRawSpanLimits (spec 09 req 22).
func SpanLimits() sdktrace.SpanLimits {
	return sdktrace.SpanLimits{
		AttributeValueLengthLimit:   MaxAttributeValueLen,
		AttributeCountLimit:         MaxSpanAttributes,
		EventCountLimit:             MaxSpanEvents,
		LinkCountLimit:              MaxSpanLinks,
		AttributePerEventCountLimit: MaxAttributesPerEvent,
		AttributePerLinkCountLimit:  0,
	}
}

// Counters are the Node-wide tracing counters, from the telemetry
// registry; a nil counter counts nothing.
type Counters struct {
	// Spans counts ruralz_telemetry_spans_total.
	Spans emit.Counter
	// DroppedQueueFull counts ruralz_telemetry_spans_dropped_total{reason="queue_full"}.
	DroppedQueueFull emit.Counter
	// DroppedExportError counts ruralz_telemetry_spans_dropped_total{reason="export_error"}.
	DroppedExportError emit.Counter
	// UnsampledRoot counts ruralz_telemetry_traces_unsampled_total{reason="rate_cap_root"}.
	UnsampledRoot emit.Counter
	// UnsampledParent counts ruralz_telemetry_traces_unsampled_total{reason="rate_cap_parent"}.
	UnsampledParent emit.Counter
}

// Options configure a Tracer. Zero values take the defaults; the caps,
// queue and batch values are fixed in M1 (OQ-observability-10 (a)) and
// set here only by tests.
type Options struct {
	// Clock drives the caps and the batch interval; nil means clock.Real().
	Clock clock.Clock
	// Resource is the resource the tracer, meter and logger providers
	// share (spec 09 reqs 11 and 13); nil means an empty resource, never
	// resource.Default().
	Resource *resource.Resource
	// Counters are the tracing counters.
	Counters Counters
	// OnExport observes every export attempt (ProcessorOptions.OnExport).
	OnExport func(spans int, err error)
	// RootPerSecond caps sampled root decisions (default 1,000); the burst
	// equals the rate.
	RootPerSecond int
	// ParentPerSecond caps sampled decisions from a client's flag
	// (default 500); the burst equals the rate.
	ParentPerSecond int
	// QueueSize, BatchSize, BatchInterval and ExportTimeout configure the
	// span processor (ProcessorOptions).
	QueueSize     int
	BatchSize     int
	BatchInterval time.Duration
	ExportTimeout time.Duration
}

// ErrOptions reports invalid Options.
var ErrOptions = errors.New("tracing: invalid options")

// Tracer implements emit.Tracer: the per-request sampling decision, the
// Ruralz span model and propagation to Upstream legs. It is safe for
// concurrent use; its caps survive Hot Reloads because one Tracer lives
// as long as the Node.
type Tracer struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
	proc     *Processor
	src      source
	clock    clock.Clock
	epoch    time.Time
	root     *Limiter
	parent   *Limiter
	enabled  atomic.Bool

	unsampledRoot, unsampledParent emit.Counter

	serverOpts, internalOpts, clientOpts []trace.SpanStartOption
}

var _ emit.Tracer = (*Tracer)(nil)

// New returns a Tracer with no exporter: decisions and propagation work at
// once, spans start once SetExporter installs one. Shutdown releases it.
func New(o Options) (*Tracer, error) {
	switch {
	case o.RootPerSecond < 0, o.ParentPerSecond < 0, o.QueueSize < 0, o.BatchSize < 0,
		o.BatchInterval < 0, o.ExportTimeout < 0:
		return nil, fmt.Errorf("%w: negative cap, size or duration", ErrOptions)
	}
	c := o.Clock
	if c == nil {
		c = clock.Real()
	}
	res := o.Resource
	if res == nil {
		res = resource.Empty()
	}
	rootRate := positive(o.RootPerSecond, DefaultRootPerSecond)
	parentRate := positive(o.ParentPerSecond, DefaultParentPerSecond)
	t := &Tracer{
		clock:           c,
		epoch:           c.Now(),
		root:            NewLimiter(rootRate, rootRate),
		parent:          NewLimiter(parentRate, parentRate),
		unsampledRoot:   counterOrNoop(o.Counters.UnsampledRoot),
		unsampledParent: counterOrNoop(o.Counters.UnsampledParent),
		serverOpts:      []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindServer)},
		internalOpts:    []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindInternal)},
		clientOpts:      []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindClient)},
	}
	t.proc = NewProcessor(ProcessorOptions{
		Clock:              c,
		QueueSize:          o.QueueSize,
		BatchSize:          o.BatchSize,
		BatchInterval:      o.BatchInterval,
		ExportTimeout:      o.ExportTimeout,
		Spans:              o.Counters.Spans,
		DroppedQueueFull:   o.Counters.DroppedQueueFull,
		DroppedExportError: o.Counters.DroppedExportError,
		OnExport:           o.OnExport,
	})
	t.provider = sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithIDGenerator(&IDGenerator{src: &t.src}),
		sdktrace.WithRawSpanLimits(SpanLimits()),
		sdktrace.WithSpanProcessor(t.proc),
	)
	t.tracer = t.provider.Tracer(ScopeName, trace.WithSchemaURL(semconv.SchemaURL))
	return t, nil
}

// SetExporter installs e, or none when nil, and returns the previous
// exporter for the caller to shut down (spec 09 req 24: an endpoint or TLS
// change swaps exporters off the request path). Spans are created only
// while an exporter is set (req 15); decisions and propagation never
// depend on it.
func (t *Tracer) SetExporter(e sdktrace.SpanExporter) sdktrace.SpanExporter {
	old := t.proc.SetExporter(e)
	t.enabled.Store(e != nil)
	return old
}

// SpansEnabled reports whether sampled requests create spans.
func (t *Tracer) SpansEnabled() bool { return t.enabled.Load() }

// ForceFlush exports every ended span now, bounded by ctx.
func (t *Tracer) ForceFlush(ctx context.Context) error { return t.provider.ForceFlush(ctx) }

// Shutdown stops span creation, flushes the queued spans within ctx (spec
// 09 req 25), stops the processor's worker and shuts the current exporter
// down. When ctx is already done the worker still stops and every queued
// span is counted as dropped (Processor.Shutdown).
func (t *Tracer) Shutdown(ctx context.Context) error {
	t.enabled.Store(false)
	// The processor first: the SDK's TracerProvider.Shutdown returns
	// ctx.Err() before it reaches a processor when ctx is already done,
	// which would leave the worker and the exporter running.
	perr := t.proc.Shutdown(ctx)
	// Processor.Shutdown is idempotent, so the provider's call to it
	// returns at once; a context without cancellation lets the provider
	// mark itself shut down even when ctx is done.
	if err := errors.Join(perr, t.provider.Shutdown(context.WithoutCancel(ctx))); err != nil {
		return fmt.Errorf("tracing: shutdown: %w", err)
	}
	return nil
}

// Decide makes the request's one sampling decision (spec 09 reqs 27 to
// 30) and fills d: a valid traceparent parents the server span, its
// tracestate is kept when valid, and its sampled flag decides within the
// parent cap; otherwise a new trace ID is drawn and ratio decides on it
// within the root cap. A decision over its cap is unsampled and counted on
// a stripe chosen by the random server span ID, so a flood of forced
// flags spreads over the stripes. The server span ID is always drawn. It
// allocates nothing except the combined value when a request carries
// several tracestate field lines that are valid together.
func (t *Tracer) Decide(h http.Header, ratio float64, d *emit.Decision) {
	*d = emit.Decision{}
	var flags byte
	if vs := h[HeaderTraceparent]; len(vs) == 1 {
		if tid, pid, fl, ok := ParseTraceparent(vs[0]); ok {
			d.TraceID, d.ParentSpanID, d.Remote, flags = tid, pid, true, fl
			d.TraceState = tracestate(h)
		}
	}
	if d.Remote {
		t.src.spanID(&d.ServerSpanID)
		if flags&FlagSampled == 0 {
			return
		}
		if t.parent.Allow(t.since()) {
			d.Sampled = true
		} else {
			t.unsampledParent.Add(stripeOf(d.ServerSpanID[0]), 1)
		}
		return
	}
	t.src.ids(&d.TraceID, &d.ServerSpanID)
	if !RatioSampled(d.TraceID, ratio) {
		return
	}
	if t.root.Allow(t.since()) {
		d.Sampled = true
	} else {
		t.unsampledRoot.Add(stripeOf(d.ServerSpanID[0]), 1)
	}
}

// since is the cap clock: the time since the Tracer was built.
func (t *Tracer) since() time.Duration { return t.clock.Now().Sub(t.epoch) }

// StartServer starts the SERVER span of a sampled request while spans are
// enabled, with the IDs of d and the attributes of spec 09 req 33, named
// "<method>" until ruralz.route is set; otherwise it returns ctx and a
// no-op span whose SpanID is d's server span ID, allocating nothing.
func (t *Tracer) StartServer(ctx context.Context, d *emit.Decision, method string, a emit.ServerAttrs) (context.Context, emit.Span) {
	if d == nil {
		return ctx, noopSpan{}
	}
	if !d.Sampled || !t.enabled.Load() {
		return ctx, (*noopServer)(d)
	}
	pctx := trace.ContextWithSpan(ctx, &serverParent{d: d})
	name, original := normalizeMethod(method)
	sctx, sdk := t.tracer.Start(pctx, name, t.serverOpts...) //nolint:spancheck // returned as emit.Span; the caller ends it
	s := newSpan(kindServer, sdk, d.ServerSpanID)
	s.method = name
	s.serverStart(original, &a)
	return sctx, s //nolint:spancheck // the caller ends the returned span
}

// StartRouteMatch starts ruralz.route.match (INTERNAL, spec 09 req 34)
// under the span in ctx; the caller sets ruralz.route to the matched Route
// or UnmatchedRoute.
func (t *Tracer) StartRouteMatch(ctx context.Context) (context.Context, emit.Span) {
	if !trace.SpanFromContext(ctx).IsRecording() {
		return ctx, noopSpan{}
	}
	return t.start(ctx, catalog.SpanRouteMatch, kindMatch, t.internalOpts)
}

// StartFilter starts ruralz.filter.<name> (INTERNAL, spec 09 req 35) under
// the span in ctx, which is the leg span for upstream-scoped Policies;
// spanName is precomputed per snapshot (catalog.FilterSpanName). onLog
// creates no span.
func (t *Tracer) StartFilter(ctx context.Context, spanName string, a emit.FilterAttrs) (context.Context, emit.Span) {
	if a.Phase.String() == onLogPhase {
		return ctx, noopSpan{}
	}
	return t.startPolicy(ctx, spanName, &a)
}

// StartChunk starts the span of one Policy's onChunk subscription on a
// stream (spec 09 req 37). It is the M3 extension point: M1 serves no
// stream and has no caller.
func (t *Tracer) StartChunk(ctx context.Context, spanName string, a emit.FilterAttrs) (context.Context, emit.Span) {
	return t.startPolicy(ctx, spanName, &a)
}

// startPolicy starts an INTERNAL Policy span under the span in ctx.
func (t *Tracer) startPolicy(ctx context.Context, spanName string, a *emit.FilterAttrs) (context.Context, emit.Span) {
	if !trace.SpanFromContext(ctx).IsRecording() {
		return ctx, noopSpan{}
	}
	sctx, s := t.start(ctx, spanName, kindFilter, t.internalOpts)
	s.filterStart(a)
	return sctx, s
}

// StartUpstream starts ruralz.upstream.<name> (CLIENT, spec 09 req 36) for
// one leg attempt under the span in ctx; spanName is precomputed per
// snapshot (catalog.UpstreamSpanName). Inject with the returned context
// names this span as the Upstream's parent.
func (t *Tracer) StartUpstream(ctx context.Context, spanName string, a emit.UpstreamAttrs) (context.Context, emit.Span) {
	if !trace.SpanFromContext(ctx).IsRecording() {
		return ctx, noopSpan{}
	}
	sctx, s := t.start(ctx, spanName, kindUpstream, t.clientOpts)
	s.upstreamStart(&a)
	return sctx, s
}

// start starts a child span of the recording span in ctx.
func (t *Tracer) start(ctx context.Context, name string, k spanKind, opts []trace.SpanStartOption) (context.Context, *span) {
	sctx, sdk := t.tracer.Start(ctx, name, opts...)          //nolint:spancheck // returned as emit.Span; the caller ends it
	return sctx, newSpan(k, sdk, sdk.SpanContext().SpanID()) //nolint:spancheck // the caller ends the returned span
}

// Inject replaces the client's traceparent and tracestate on an outgoing
// Upstream leg (spec 09 req 31): traceparent carries d's trace ID, the
// span in ctx as parent (the attempt's ruralz.upstream.<name> span when
// spans exist) or else d's server span ID, and flags 01 when sampled or
// 00; tracestate is d's validated value, set only when non-empty.
// Baggage is never added. A nil h is left alone: it holds no client
// values and cannot take new ones. It allocates the new traceparent value
// and its slice (the 2 allocations of the unsampled budget, spec 09 req
// 38).
func (t *Tracer) Inject(ctx context.Context, d *emit.Decision, h http.Header) {
	if h == nil {
		return
	}
	delete(h, KeyTraceparent)
	delete(h, KeyTracestate)
	if d == nil {
		delete(h, HeaderTraceparent)
		delete(h, HeaderTracestate)
		return
	}
	var buf [TraceparentLen]byte
	h[HeaderTraceparent] = []string{string(AppendTraceparent(buf[:0], d.TraceID, parentID(ctx, d), d.Sampled))}
	switch vs := h[HeaderTracestate]; {
	case d.TraceState == "":
		delete(h, HeaderTracestate)
	case len(vs) == 1 && vs[0] == d.TraceState:
		// The outgoing header already holds exactly this value.
	default:
		h[HeaderTracestate] = []string{d.TraceState}
	}
}

// Carrier is the header abstraction of the non-HTTP legs (gRPC metadata in
// M3; Kafka, NATS and MQTT 5 headers in M4, spec 09 section 8); keys are
// KeyTraceparent and KeyTracestate. Get serves the M3 counterpart of
// Decide that extracts from a carrier (a DecideCarrier); Set and Delete
// serve InjectCarrier.
type Carrier interface {
	// Get returns the value of key, "" when absent.
	Get(key string) string
	// Set replaces the value of key.
	Set(key, value string)
	// Delete removes key.
	Delete(key string)
}

// InjectCarrier is Inject for a non-HTTP leg; a nil c is left alone. M1
// has no caller.
func (t *Tracer) InjectCarrier(ctx context.Context, d *emit.Decision, c Carrier) {
	if c == nil {
		return
	}
	c.Delete(KeyTraceparent)
	c.Delete(KeyTracestate)
	if d == nil {
		return
	}
	var buf [TraceparentLen]byte
	c.Set(KeyTraceparent, string(AppendTraceparent(buf[:0], d.TraceID, parentID(ctx, d), d.Sampled)))
	if d.TraceState != "" {
		c.Set(KeyTracestate, d.TraceState)
	}
}

// parentID returns the ID of the span in ctx when it is a local span of
// d's trace, else d's server span ID.
func parentID(ctx context.Context, d *emit.Decision) [8]byte {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() && !sc.IsRemote() && sc.TraceID() == d.TraceID {
		return sc.SpanID()
	}
	return d.ServerSpanID
}
