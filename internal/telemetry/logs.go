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

// logSink is a sink of a log record: the sink label of
// ruralz_telemetry_logs_dropped_total, in catalog order
// (catalog.LogSinks). A record reaches both independently, and each sink
// counts a record it loses once.
type logSink uint8

// Sinks.
const (
	sinkStdout logSink = iota
	sinkOTLP
	numSinks
)

// logDrops counts the OTLP sink's losses per stream, Node-wide: they
// outlive the pipeline that lost them, so a swap never resets a counter.
type logDrops struct {
	queueFull   [numStreams]atomic.Uint64
	exportError [numStreams]atomic.Uint64
}

// logMeta travels in the context from a bridge to the processor: the
// record's stream.
type logMeta struct {
	stream stream
}

// logMetaKey is the context key of logMeta.
type logMetaKey struct{}

// capture receives the SDK record of one access record the export worker
// encodes: OnEmit stores the record in it instead of queueing it.
type capture struct {
	rec sdklog.Record
	ok  bool
}

// captureKey is the context key of a *capture.
type captureKey struct{}

// queuedLog is one record in the processor's queue: an SDK record cloned
// at OnEmit (process logs, through otelslog), or an access record the
// export worker encodes into one when its batch is exported, so the
// request goroutine that queued it does no encoding.
type queuedLog struct {
	rec    sdklog.Record
	access accesslog.Pending
	lazy   bool
	stream stream
	size   int64
}

// logProcessor is the Ruralz sdklog.Processor and the OTLP log queue:
// OnEmit clones a record into a bounded queue without blocking, and
// offerAccess queues an access record as it is, both dropping with a
// queue_full count when the queue is out of records or bytes. One worker
// exports batches of up to logBatch records, or every logInterval on the
// runtime's clock, encoding queued access records first, and counts a
// failed batch as export_error drops of each record's stream.
type logProcessor struct {
	exp    sdklog.Exporter
	clk    clock.Clock
	drops  *logDrops
	report func(error)

	mu     sync.Mutex
	queue  []queuedLog
	bytes  int64
	closed bool
	// logger emits the access records the worker encodes, through this
	// processor (capture), so they carry the provider's resource and scope.
	logger otellog.Logger

	wake  chan struct{}
	flush chan chan struct{}
	stop  chan struct{}
	done  chan struct{}
	// ctx bounds exports; cancel cuts the one in flight at shutdown.
	ctx    context.Context
	cancel context.CancelFunc
	// timer paces the batch interval; it exists before the worker runs, so
	// a clock advanced right after construction still fires it.
	timer clock.Timer
	// scratch is the worker's line buffer for access records.
	scratch []byte
}

// Compile-time check of the contract.
var _ sdklog.Processor = (*logProcessor)(nil)

// newLogProcessor starts the worker of a processor exporting through exp;
// the exporter bounds and retries each export itself.
func newLogProcessor(exp sdklog.Exporter, clk clock.Clock, drops *logDrops, report func(error)) *logProcessor {
	ctx, cancel := context.WithCancel(context.Background())
	p := &logProcessor{
		exp: exp, clk: clk, drops: drops, report: report,
		wake: make(chan struct{}, 1), flush: make(chan chan struct{}),
		stop: make(chan struct{}), done: make(chan struct{}),
		ctx: ctx, cancel: cancel,
		timer: clk.NewTimer(logInterval),
	}
	go p.run()
	return p
}

// setLogger sets the logger the worker emits encoded access records
// through; the pipeline sets it before any bridge offers one.
func (p *logProcessor) setLogger(l otellog.Logger) {
	p.mu.Lock()
	p.logger = l
	p.mu.Unlock()
}

// Enabled reports that every record is processed.
func (p *logProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

// OnEmit queues a clone of r without blocking; a record the worker is
// encoding goes to its capture instead.
func (p *logProcessor) OnEmit(ctx context.Context, r *sdklog.Record) error {
	if c, ok := ctx.Value(captureKey{}).(*capture); ok {
		c.rec, c.ok = r.Clone(), true
		return nil
	}
	st := streamProcess
	if meta, ok := ctx.Value(logMetaKey{}).(logMeta); ok {
		st = meta.stream
	}
	p.enqueue(queuedLog{rec: r.Clone(), stream: st, size: recordSize(r)})
	return nil
}

// offerAccess queues the access record a without encoding it; it is
// released when the worker has exported or dropped it.
func (p *logProcessor) offerAccess(a accesslog.Pending) {
	p.enqueue(queuedLog{access: a, lazy: true, stream: streamAccess, size: a.Size()})
}

// enqueue adds q to the queue, or drops it with a queue_full count.
func (p *logProcessor) enqueue(q queuedLog) {
	p.mu.Lock()
	if p.closed || len(p.queue) >= logQueueRecords || p.bytes+q.size > logQueueBytes {
		p.mu.Unlock()
		p.drops.queueFull[q.stream].Add(1)
		q.releaseAccess()
		return
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
}

// releaseAccess releases the access record a lazy item holds.
func (q *queuedLog) releaseAccess() {
	if q.lazy {
		q.access.Release()
		q.lazy = false
	}
}

// run is the worker: it exports full batches when woken, every queued
// record every logInterval, and on a ForceFlush request.
func (p *logProcessor) run() {
	defer close(p.done)
	defer p.timer.Stop()
	for {
		select {
		case <-p.stop:
			return
		case ack := <-p.flush:
			p.export(p.ctx, false)
			close(ack)
		case <-p.wake:
			p.export(p.ctx, true)
		case <-p.timer.C():
			p.export(p.ctx, false)
			p.timer.Reset(logInterval)
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
		clear(p.queue[:n])
		p.queue = append(p.queue[:0], p.queue[n:]...)
		for _, q := range batch {
			p.bytes -= q.size
		}
		lg := p.logger
		p.mu.Unlock()
		recs := make([]sdklog.Record, 0, n)
		streams := make([]stream, 0, n)
		for i := range batch {
			q := &batch[i]
			if q.lazy {
				rec, ok := p.encodeAccess(lg, q.access) //nolint:contextcheck // the encoding context carries the access record's own span context, never the export's.
				q.releaseAccess()
				if !ok {
					p.drops.exportError[streamAccess].Add(1)
					continue
				}
				q.rec = rec
			}
			recs = append(recs, q.rec)
			streams = append(streams, q.stream)
		}
		if len(recs) == 0 {
			continue
		}
		err := p.exp.Export(ctx, recs)
		if err != nil {
			for _, s := range streams {
				p.drops.exportError[s].Add(1)
			}
		}
		p.report(err)
	}
}

// encodeAccess encodes one access record as an SDK record: the line's
// members become attributes, the request start the timestamp, the
// runtime's clock the observed timestamp, and event.name is
// accesslog.EventName. It emits the record through the provider's logger
// into a capture, so the record carries the provider's resource and
// scope; false when the logger is missing or the provider stopped.
func (p *logProcessor) encodeAccess(lg otellog.Logger, a accesslog.Pending) (sdklog.Record, bool) {
	if lg == nil {
		return sdklog.Record{}, false
	}
	p.scratch = a.AppendLine(p.scratch[:0])
	var rec otellog.Record
	rec.SetTimestamp(a.Start())
	rec.SetObservedTimestamp(p.clk.Now())
	rec.SetEventName(accesslog.EventName)
	rec.SetSeverity(otellog.SeverityInfo)
	rec.SetSeverityText(accesslog.Level)
	rec.SetBody(attribute.StringValue(accesslog.Message))
	if tree, _, err := jsonval.Decode(p.scratch, jsonval.Options{}); err == nil {
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
		rec.SetBody(attribute.StringValue(string(p.scratch)))
	}
	traceID, spanID, sampled := a.Trace()
	c := &capture{}
	ctx := context.WithValue(withMeta(traceID, spanID, sampled, logMeta{stream: streamAccess}), captureKey{}, c)
	lg.Emit(ctx, rec)
	return c.rec, c.ok
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
// shuts the exporter down. The provider calls it once it no longer emits,
// so an access record still queued then can no longer be encoded and
// counts as export_error: the pipeline flushes first. Later calls return
// nil.
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
	for i := range left {
		p.drops.exportError[left[i].stream].Add(1)
		left[i].releaseAccess()
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
// when valid, and the record's stream.
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

// processBridge implements logsink.Bridge over the otelslog handler: each
// record reaches the processor's queue when it is logged.
type processBridge struct {
	h     *otelslog.Handler
	drops *logDrops
}

// Compile-time check of the contract.
var _ logsink.Bridge = (*processBridge)(nil)

// Export hands the record to otelslog with its span context; the
// processor queues it without blocking. A record otelslog refuses counts
// as an export_error loss of the OTLP sink.
func (b *processBridge) Export(e *logsink.Exported) {
	ctx := withMeta(e.TraceID, e.SpanID, false, logMeta{stream: streamProcess})
	if err := b.h.Handle(ctx, e.Record); err != nil {
		b.drops.exportError[streamProcess].Add(1)
	}
}

// accessBridge implements accesslog.Bridge over the processor: each record
// joins the OTLP log queue as it is submitted, and the export worker
// encodes it.
type accessBridge struct {
	p *logProcessor
}

// Compile-time check of the contract.
var _ accesslog.Bridge = (*accessBridge)(nil)

// Export queues one access record.
func (b *accessBridge) Export(a accesslog.Pending) { b.p.offerAccess(a) }

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
