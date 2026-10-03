// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Processor defaults (spec 09 req 20: queue 8,192 spans, target; batch and
// interval proposed there; exporter timeout 10 s from req 16).
const (
	DefaultQueueSize     = 8192
	DefaultBatchSize     = 512
	DefaultBatchInterval = time.Second
	DefaultExportTimeout = 10 * time.Second
)

// ErrClosed reports a Processor that was shut down.
var ErrClosed = errors.New("tracing: processor shut down")

// ProcessorOptions configure a Processor. Zero values take the defaults.
type ProcessorOptions struct {
	// Clock schedules the batch interval; nil means clock.Real().
	Clock clock.Clock
	// QueueSize bounds the queue of ended spans.
	QueueSize int
	// BatchSize is the most spans one export carries.
	BatchSize int
	// BatchInterval is the longest a queued span waits for its batch.
	BatchInterval time.Duration
	// ExportTimeout bounds one export.
	ExportTimeout time.Duration
	// Spans counts ruralz_telemetry_spans_total.
	Spans emit.Counter
	// DroppedQueueFull counts ruralz_telemetry_spans_dropped_total{reason="queue_full"}.
	DroppedQueueFull emit.Counter
	// DroppedExportError counts ruralz_telemetry_spans_dropped_total{reason="export_error"}.
	DroppedExportError emit.Counter
	// OnExport, when set, is called on the worker after every export
	// attempt through an exporter with the batch length and the result,
	// so the Runtime can raise and clear telemetry_export_failing and log
	// failures at most once per interval (spec 09 req 23). It must not
	// block.
	OnExport func(spans int, err error)
}

// Processor is the Ruralz sdktrace.SpanProcessor of spec 09 req 20 (not
// BatchSpanProcessor, which has no drop counters): OnEnd counts the span
// and enqueues it without blocking into a bounded queue, dropping with a
// queue_full count when full; one worker goroutine, owned by the
// Processor and stopped by Shutdown, exports batches of up to BatchSize
// spans or every BatchInterval through the current exporter and counts a
// failed batch as export_error drops. The exporter is swapped atomically
// by SetExporter (Hot Reload, spec 09 req 24).
type Processor struct {
	queue    chan sdktrace.ReadOnlySpan
	exporter atomic.Pointer[exporterRef]
	closed   atomic.Bool
	// inflight counts OnEnd calls that passed the closed check (enter),
	// so Shutdown can wait for them and count a span one queued after
	// the worker's last drain.
	inflight atomic.Int64

	spans, dropFull, dropExport emit.Counter
	onExport                    func(int, error)

	batchSize int
	interval  time.Duration
	timeout   time.Duration
	timer     clock.Timer

	kick     chan struct{}
	flushes  chan flushRequest
	stop     chan flushRequest
	done     chan struct{}
	cancel   context.CancelFunc
	stopOnce sync.Once
}

var _ sdktrace.SpanProcessor = (*Processor)(nil)

// exporterRef boxes an exporter for the atomic pointer.
type exporterRef struct{ e sdktrace.SpanExporter }

// flushRequest asks the worker to export everything queued within ctx.
type flushRequest struct {
	// ctx bounds this request only; the worker drops it when done.
	ctx  context.Context
	done chan error
}

// NewProcessor returns a Processor with no exporter and starts its worker.
// Shutdown stops the worker.
func NewProcessor(o ProcessorOptions) *Processor {
	c := o.Clock
	if c == nil {
		c = clock.Real()
	}
	p := &Processor{
		queue:      make(chan sdktrace.ReadOnlySpan, positive(o.QueueSize, DefaultQueueSize)),
		spans:      counterOrNoop(o.Spans),
		dropFull:   counterOrNoop(o.DroppedQueueFull),
		dropExport: counterOrNoop(o.DroppedExportError),
		onExport:   o.OnExport,
		batchSize:  positive(o.BatchSize, DefaultBatchSize),
		interval:   positiveDuration(o.BatchInterval, DefaultBatchInterval),
		timeout:    positiveDuration(o.ExportTimeout, DefaultExportTimeout),
		kick:       make(chan struct{}, 1),
		flushes:    make(chan flushRequest),
		stop:       make(chan flushRequest, 1),
		done:       make(chan struct{}),
	}
	// The timer exists before the worker runs, so a fake clock advanced
	// right after construction still fires it.
	p.timer = c.NewTimer(p.interval)
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go p.run(ctx)
	return p
}

// SetExporter installs e (nil: none) and returns the previous exporter,
// which the caller shuts down once it is no longer needed; a batch in
// flight may still use it until its export returns.
func (p *Processor) SetExporter(e sdktrace.SpanExporter) sdktrace.SpanExporter {
	var ref *exporterRef
	if e != nil {
		ref = &exporterRef{e: e}
	}
	old := p.exporter.Swap(ref)
	if old == nil {
		return nil
	}
	return old.e
}

// Exporter returns the current exporter, nil when none.
func (p *Processor) Exporter() sdktrace.SpanExporter {
	if ref := p.exporter.Load(); ref != nil {
		return ref.e
	}
	return nil
}

// OnStart does nothing.
func (*Processor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

// OnEnd counts s and enqueues it without blocking; a full queue, or a
// Processor shut down, drops s with a queue_full count. The counters are
// striped by the span ID, so concurrent requests rarely share a stripe.
func (p *Processor) OnEnd(s sdktrace.ReadOnlySpan) {
	sid := s.SpanContext().SpanID()
	st := stripeOf(sid[7])
	p.spans.Add(st, 1)
	if !p.enter() {
		p.dropFull.Add(st, 1)
		return
	}
	defer p.inflight.Add(-1)
	select {
	case p.queue <- s:
		if len(p.queue) >= p.batchSize {
			select {
			case p.kick <- struct{}{}:
			default:
			}
		}
	default:
		p.dropFull.Add(st, 1)
	}
}

// enter announces an OnEnd that may enqueue and reports true, or reports
// false once the Processor is shut down. The check after the increment
// makes Shutdown, which sets closed before it waits for inflight to reach
// zero, see every call that may enqueue; calls that start after the close
// never touch inflight, so that wait ends.
func (p *Processor) enter() bool {
	if p.closed.Load() {
		return false
	}
	p.inflight.Add(1)
	if p.closed.Load() {
		p.inflight.Add(-1)
		return false
	}
	return true
}

// ForceFlush exports every span queued when it is called, bounded by ctx.
func (p *Processor) ForceFlush(ctx context.Context) error {
	if p.closed.Load() {
		return ErrClosed
	}
	req := flushRequest{ctx: ctx, done: make(chan error, 1)}
	select {
	case p.flushes <- req:
	case <-p.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown stops accepting spans, exports what is queued within ctx
// (spans left when ctx ends are dropped and counted as export_error, spec
// 09 req 25), stops the worker and shuts the current exporter down. When
// ctx is already done it still stops the worker, counts every queued span
// and shuts the exporter down; it waits for an export in flight to see
// the cancellation, which a sdktrace.SpanExporter must honor. When it
// returns, every span counted in spans_total was exported or counted as
// dropped. Later calls return nil.
func (p *Processor) Shutdown(ctx context.Context) error {
	err := ErrClosed
	p.stopOnce.Do(func() {
		err = p.shutdown(ctx)
	})
	if errors.Is(err, ErrClosed) {
		return nil
	}
	return err
}

func (p *Processor) shutdown(ctx context.Context) error {
	p.closed.Store(true)
	req := flushRequest{ctx: ctx, done: make(chan error, 1)}
	p.stop <- req
	var err error
	select {
	case <-p.done:
		err = <-req.done
	case <-ctx.Done():
		// Abort an export still running past ctx; the worker's final
		// drain then sees ctx done, counts what is left and exits.
		p.cancel()
		<-p.done
		err = ctx.Err()
	}
	p.cancel()
	// An OnEnd that passed the closed check before it was set may queue
	// its span after the worker's last drain: wait for those calls (they
	// never block), then count what they queued.
	for p.inflight.Load() > 0 {
		runtime.Gosched()
	}
	p.dropExport.Add(0, p.discardQueued())
	if ref := p.exporter.Swap(nil); ref != nil {
		err = errors.Join(err, ref.e.Shutdown(ctx))
	}
	return err
}

// run is the export worker. It sleeps until the batch interval passes or
// OnEnd reports a full batch queued, so ending a span never wakes it.
func (p *Processor) run(ctx context.Context) {
	defer close(p.done)
	defer p.timer.Stop()
	batch := make([]sdktrace.ReadOnlySpan, 0, p.batchSize)
	for {
		select {
		case <-p.kick:
			_ = p.drain(ctx, &batch, drainFull)
		case <-p.timer.C():
			_ = p.drain(ctx, &batch, drainAll)
			p.timer.Reset(p.interval)
		case r := <-p.flushes:
			r.done <- p.drain(r.ctx, &batch, drainAll) //nolint:contextcheck // a flush is bounded by its caller's context
		case r := <-p.stop:
			r.done <- p.drain(r.ctx, &batch, drainFinal) //nolint:contextcheck // Shutdown is bounded by its caller's context
			return
		}
	}
}

// drainMode selects what drain exports.
type drainMode uint8

const (
	// drainFull exports full batches while at least one is queued.
	drainFull drainMode = iota
	// drainAll exports the spans queued when drain starts; spans left when
	// ctx ends stay queued for the next batch.
	drainAll
	// drainFinal (Shutdown) also exports spans queued meanwhile and drops
	// the spans left when ctx ends as export_error (spec 09 req 25).
	drainFinal
)

// drain exports queued spans in batches of up to batchSize within ctx.
// Only this worker receives from the queue while it runs, so pending
// spans are there to receive.
func (p *Processor) drain(ctx context.Context, batch *[]sdktrace.ReadOnlySpan, mode drainMode) error {
	pending := len(p.queue)
	for {
		if mode == drainFull && pending < p.batchSize {
			return nil
		}
		if err := ctx.Err(); err != nil {
			if mode == drainFinal {
				p.dropExport.Add(0, p.discardQueued())
			}
			return err
		}
		for pending > 0 && len(*batch) < p.batchSize {
			*batch = append(*batch, <-p.queue)
			pending--
		}
		if len(*batch) == 0 {
			if more := len(p.queue); mode == drainFinal && more > 0 {
				pending = more
				continue
			}
			return nil
		}
		*batch = p.export(ctx, *batch)
		if mode == drainFull {
			pending = len(p.queue)
		}
	}
}

// discardQueued empties the queue and returns how many spans it held.
func (p *Processor) discardQueued() uint64 {
	var n uint64
	for {
		select {
		case <-p.queue:
			n++
		default:
			return n
		}
	}
}

// export sends batch through the current exporter, counts a failed or
// exporter-less batch as export_error drops, and returns batch emptied
// for reuse.
func (p *Processor) export(ctx context.Context, batch []sdktrace.ReadOnlySpan) []sdktrace.ReadOnlySpan {
	ref := p.exporter.Load()
	if ref == nil {
		p.dropExport.Add(0, uint64(len(batch)))
	} else {
		ectx, cancel := context.WithTimeout(ctx, p.timeout)
		err := ref.e.ExportSpans(ectx, batch)
		cancel()
		if err != nil {
			p.dropExport.Add(0, uint64(len(batch)))
		}
		if p.onExport != nil {
			p.onExport(len(batch), err)
		}
	}
	clear(batch)
	return batch[:0]
}

// stripeOf maps a random ID byte to one of the eight counter stripes.
func stripeOf(b byte) emit.Stripe { return emit.Stripe(b & 7) }

// noopCounter stands in for a counter the caller did not provide.
type noopCounter struct{}

func (noopCounter) Add(emit.Stripe, uint64) {}

func counterOrNoop(c emit.Counter) emit.Counter {
	if c == nil {
		return noopCounter{}
	}
	return c
}

func positive(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func positiveDuration(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}
