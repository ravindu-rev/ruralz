// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/telemetry/accesslog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/logsink"
)

// OTLP log queue and batching (docs/architecture/10-observability.md
// "Telemetry pipeline").
const (
	// logQueueRecords bounds the records queued for OTLP.
	logQueueRecords = 8192
	// logQueueBytes bounds their estimated size.
	logQueueBytes = 4 << 20
	// logBatch is the most records one export carries.
	logBatch = 512
	// logInterval is the longest a queued record waits for its batch.
	logInterval = time.Second
)

// stream is a log stream: the stream label of ruralz_telemetry_logs_*,
// in catalog order.
type stream uint8

// Streams.
const (
	streamAccess stream = iota
	streamProcess
	numStreams
)

// logDrops counts the OTLP-side losses per stream, Node-wide: they outlive
// the pipeline that lost them, so a swap never resets a counter.
type logDrops struct {
	queueFull   [numStreams]atomic.Uint64
	exportError [numStreams]atomic.Uint64
}

// logMeta travels in the context from a bridge to the processor: the
// record's stream, and whether stdout already lost and counted it.
type logMeta struct {
	stream  stream
	counted bool
}

// logMetaKey is the context key of logMeta.
type logMetaKey struct{}

// queuedLog is one record in the processor's queue.
type queuedLog struct {
	rec  sdklog.Record
	meta logMeta
	size int64
}

// logProcessor is the Ruralz sdklog.Processor: OnEmit clones the record
// into a bounded queue without blocking, dropping with a queue_full count
// when the queue is out of records or bytes; one worker exports batches of
// up to logBatch records, or every logInterval, and counts a failed batch
// as export_error drops. A record stdout already lost is exported but
// never counted again.
type logProcessor struct {
	exp     sdklog.Exporter
	clk     clock.Clock
	drops   *logDrops
	timeout time.Duration
	report  func(error)

	mu     sync.Mutex
	queue  []queuedLog
	bytes  int64
	closed bool

	wake  chan struct{}
	flush chan chan struct{}
	stop  chan struct{}
	done  chan struct{}
	// ctx bounds exports; cancel cuts the one in flight at shutdown.
	ctx    context.Context
	cancel context.CancelFunc
}

// Compile-time check of the contract.
var _ sdklog.Processor = (*logProcessor)(nil)

// newLogProcessor starts the worker of a processor exporting through exp.
func newLogProcessor(exp sdklog.Exporter, clk clock.Clock, drops *logDrops, timeout time.Duration, report func(error)) *logProcessor {
	ctx, cancel := context.WithCancel(context.Background())
	p := &logProcessor{
		exp: exp, clk: clk, drops: drops, timeout: timeout, report: report,
		wake: make(chan struct{}, 1), flush: make(chan chan struct{}),
		stop: make(chan struct{}), done: make(chan struct{}),
		ctx: ctx, cancel: cancel,
	}
	go p.run()
	return p
}

// Enabled reports that every record is processed.
func (p *logProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

// OnEmit queues a clone of r without blocking.
func (p *logProcessor) OnEmit(ctx context.Context, r *sdklog.Record) error {
	meta, _ := ctx.Value(logMetaKey{}).(logMeta)
	q := queuedLog{rec: r.Clone(), meta: meta, size: recordSize(r)}
	p.mu.Lock()
	if p.closed || len(p.queue) >= logQueueRecords || p.bytes+q.size > logQueueBytes {
		p.mu.Unlock()
		p.drop(meta, &p.drops.queueFull)
		return nil
	}
	p.queue = append(p.queue, q)
	p.bytes += q.size
	full := len(p.queue) >= logBatch
	p.mu.Unlock()
	if full {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// drop counts one lost record on counter c, unless stdout already did.
func (p *logProcessor) drop(meta logMeta, c *[numStreams]atomic.Uint64) {
	if !meta.counted && meta.stream < numStreams {
		c[meta.stream].Add(1)
	}
}

// run is the worker: it exports full batches when woken, every queued
// record every logInterval, and on a ForceFlush request.
func (p *logProcessor) run() {
	defer close(p.done)
	t := p.clk.NewTimer(logInterval)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case ack := <-p.flush:
			p.export(p.ctx, false)
			close(ack)
		case <-p.wake:
			p.export(p.ctx, true)
		case <-t.C():
			p.export(p.ctx, false)
			t.Reset(logInterval)
		}
	}
}

// export sends the queue in batches: only full batches when fullOnly, else
// everything queued now. It stops early when ctx ends, leaving the rest
// queued.
func (p *logProcessor) export(ctx context.Context, fullOnly bool) {
	for ctx.Err() == nil {
		p.mu.Lock()
		n := min(len(p.queue), logBatch)
		if n == 0 || (fullOnly && n < logBatch) {
			p.mu.Unlock()
			return
		}
		batch := make([]queuedLog, n)
		copy(batch, p.queue)
		p.queue = append(p.queue[:0], p.queue[n:]...)
		for _, q := range batch {
			p.bytes -= q.size
		}
		p.mu.Unlock()
		recs := make([]sdklog.Record, n)
		for i, q := range batch {
			recs[i] = q.rec
		}
		ectx, cancel := context.WithTimeout(ctx, p.timeout)
		err := p.exp.Export(ectx, recs)
		cancel()
		if err != nil {
			for _, q := range batch {
				p.drop(q.meta, &p.drops.exportError)
			}
		}
		p.report(err)
	}
}

// ForceFlush exports every record queued when it is called, bounded by ctx.
func (p *logProcessor) ForceFlush(ctx context.Context) error {
	ack := make(chan struct{})
	select {
	case p.flush <- ack:
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown stops accepting records, exports what is queued within ctx
// (records left when ctx ends count as export_error), stops the worker and
// shuts the exporter down. Later calls return nil.
func (p *logProcessor) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	close(p.stop)
	// A shutdown that ends early cuts the export in flight.
	stopCut := context.AfterFunc(ctx, p.cancel)
	defer stopCut()
	<-p.done
	p.export(ctx, false)
	p.mu.Lock()
	left := p.queue
	p.queue, p.bytes = nil, 0
	p.mu.Unlock()
	for _, q := range left {
		p.drop(q.meta, &p.drops.exportError)
	}
	p.cancel()
	return errors.Join(ctx.Err(), p.exp.Shutdown(ctx))
}

// recordSize estimates the bytes a queued record holds.
func recordSize(r *sdklog.Record) int64 {
	size := int64(128) + valueSize(r.Body()) + int64(len(r.EventName()))
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		size += int64(len(kv.Key)) + valueSize(kv.Value)
		return true
	})
	return size
}

// valueSize estimates the bytes of a log value.
func valueSize(v attribute.Value) int64 {
	switch v.Type() {
	case attribute.STRING:
		return 16 + int64(len(v.AsString()))
	case attribute.BYTESLICE:
		return 24 + int64(len(v.AsByteSlice()))
	case attribute.STRINGSLICE:
		n := int64(24)
		for _, s := range v.AsStringSlice() {
			n += 16 + int64(len(s))
		}
		return n
	case attribute.SLICE:
		n := int64(24)
		for _, e := range v.AsSlice() {
			n += valueSize(e)
		}
		return n
	case attribute.MAP:
		n := int64(24)
		for _, kv := range v.AsMap() {
			n += 16 + int64(len(kv.Key)) + valueSize(kv.Value)
		}
		return n
	case attribute.BOOLSLICE:
		return 24 + int64(len(v.AsBoolSlice()))
	case attribute.INT64SLICE:
		return 24 + 8*int64(len(v.AsInt64Slice()))
	case attribute.FLOAT64SLICE:
		return 24 + 8*int64(len(v.AsFloat64Slice()))
	case attribute.EMPTY, attribute.BOOL, attribute.INT64, attribute.FLOAT64:
		return 16
	default:
		return 16
	}
}

// withMeta returns ctx carrying the span context of traceID and spanID,
// when valid, and the record's stream and drop state.
func withMeta(traceID [16]byte, spanID [8]byte, sampled bool, meta logMeta) context.Context {
	ctx := context.Background()
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID})
	if sampled {
		sc = sc.WithTraceFlags(trace.FlagsSampled)
	}
	if sc.IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, sc)
	}
	return context.WithValue(ctx, logMetaKey{}, meta)
}

// processBridge implements logsink.Bridge over the otelslog handler.
type processBridge struct {
	h *otelslog.Handler
}

// Compile-time check of the contract.
var _ logsink.Bridge = (*processBridge)(nil)

// Export hands the record to otelslog with its span context; the
// processor queues it without blocking.
func (b *processBridge) Export(e *logsink.Exported) error {
	ctx := withMeta(e.TraceID, e.SpanID, false, logMeta{stream: streamProcess, counted: e.Dropped})
	return b.h.Handle(ctx, e.Record)
}

// accessBridge implements accesslog.Bridge over an OpenTelemetry logger:
// the line's members become attributes, the request start the timestamp,
// and event.name is accesslog.EventName.
type accessBridge struct {
	l otellog.Logger
}

// Compile-time check of the contract.
var _ accesslog.Bridge = (*accessBridge)(nil)

// Export emits one access record.
func (b *accessBridge) Export(e *accesslog.Exported) error {
	var rec otellog.Record
	rec.SetTimestamp(e.Start)
	rec.SetObservedTimestamp(e.Observed)
	rec.SetEventName(accesslog.EventName)
	rec.SetSeverity(otellog.SeverityInfo)
	rec.SetSeverityText(accesslog.Level)
	rec.SetBody(attribute.StringValue(accesslog.Message))
	if tree, _, err := jsonval.Decode(e.Line, jsonval.Options{}); err == nil {
		if obj, ok := tree.(*jsonval.Object); ok {
			for name, v := range obj.All() {
				switch name {
				case "time", "level", "msg":
					// Carried by the timestamp, severity and body.
				default:
					rec.AddAttributes(attribute.KeyValue{Key: attribute.Key(name), Value: logValue(v)})
				}
			}
		}
	} else {
		rec.SetBody(attribute.StringValue(string(e.Line)))
	}
	ctx := withMeta(e.TraceID, e.SpanID, e.Sampled, logMeta{stream: streamAccess, counted: e.Dropped})
	b.l.Emit(ctx, rec)
	return nil
}

// logValue converts a decoded JSON value; null is the empty value.
func logValue(v any) attribute.Value {
	switch x := v.(type) {
	case string:
		return attribute.StringValue(x)
	case bool:
		return attribute.BoolValue(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return attribute.Int64Value(i)
		}
		if f, err := x.Float64(); err == nil {
			return attribute.Float64Value(f)
		}
		return attribute.StringValue(string(x))
	case []any:
		vs := make([]attribute.Value, len(x))
		for i, e := range x {
			vs[i] = logValue(e)
		}
		return attribute.SliceValue(vs...)
	case *jsonval.Object:
		kvs := make([]attribute.KeyValue, 0, x.Len())
		for name, e := range x.All() {
			kvs = append(kvs, attribute.KeyValue{Key: attribute.Key(name), Value: logValue(e)})
		}
		return attribute.MapValue(kvs...)
	default:
		return attribute.Value{}
	}
}
