// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package accesslog is the access-log half of the Ruralz logging pipeline
// (docs/architecture/10-observability.md "Access logs"; spec 09
// requirements 66 to 68, spec 04 requirement 79). Writer implements
// emit.AccessLog.
//
// The request handler (internal/gateway/handler) evaluates
// Gateway.spec.telemetry.accessLog.when itself, with the onLog variables,
// and writes the entry on a runtime error (R-48); this package only pools,
// truncates, queues and encodes. On the request goroutine, Submit copies
// every field, truncated, into a pooled entry whose arena it owns, so a
// queued record never references request memory; the query string is
// never copied, and the submitted record, which returns to the pool,
// keeps no caller slice. A record's string values take at most 4 KiB once
// JSON-escaped, host and user_agent at most 256 bytes and path 1,024;
// cut fields are named in truncated. The queue is bounded twice: 8,192
// records, and a 4 MiB byte budget from which each record reserves its
// actual size (the bytes copied into its arena plus a fixed overhead for
// its other members) until it is written; either limit drops the record
// with reason queue_full. A typical record reserves under 512 bytes, so
// the record bound is reached first. The budget counts record bytes, not
// the pooled entry and arena class holding them, so the queue's heap can
// reach about 2.3 times the budget (about 9.4 MiB at the defaults, with
// every record just over 512 bytes). Submit never blocks and allocates nothing
// at steady state; a record with failure modes costs the caller its
// FailureModes slice unless the caller reuses one of its own.
//
// The stdout worker of internal/telemetry/logsink drains the queue through
// the logsink.Stream methods (Wake, Next, Flushed, Pending), so one
// goroutine owns stdout for both streams. Each line is slog-shaped
// ("level":"INFO","msg":"access") with the members of 09 req 68 in a fixed
// order.
package accesslog

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Default queue bounds (09 req 67; targets).
const (
	// DefaultQueueRecords bounds the queue in records.
	DefaultQueueRecords = 8192
	// DefaultQueueBytes is the byte budget queued records reserve.
	DefaultQueueBytes = 4 << 20
)

// Bridge exports access records over OTLP logs (internal/telemetry maps
// the line's members to attributes, the start time to the LogRecord
// timestamp and sets event.name to EventName). Export runs on the stdout
// worker after the stdout write, once per record, and must not block; an
// error counts export_error unless the record was already counted.
type Bridge interface {
	Export(r *Exported) error
}

// Exported is one record handed to the Bridge, valid until Export returns.
type Exported struct {
	// Start is the request start (LogRecord timestamp); Observed is the
	// emit time (observed timestamp).
	Start, Observed time.Time
	// TraceID, SpanID and Sampled are the server span context.
	TraceID [16]byte
	SpanID  [8]byte
	Sampled bool
	// Line is the record as one JSON object without the newline.
	Line []byte
	// Dropped is true when the stdout write already lost and counted the
	// record; the Bridge must not count it again (09 req 65).
	Dropped bool
}

// Options configure a Writer.
type Options struct {
	// NodeID is the ULID written as node_id.
	NodeID string
	// Clock stamps the observed time of bridged records; nil is clock.Real().
	Clock clock.Clock
	// QueueRecords and QueueBytes override the defaults when positive.
	QueueRecords int
	QueueBytes   int64
}

// Stats are the access stream counters (09 req 65): Produced feeds
// ruralz_telemetry_logs_total{stream="access"}, QueueFull and ExportError
// feed ruralz_telemetry_logs_dropped_total by reason.
type Stats struct {
	// Produced counts submitted records.
	Produced uint64
	// QueueFull counts records dropped because the queue was full, out of
	// bytes or closed.
	QueueFull uint64
	// ExportError counts records lost by a failed stdout write or Bridge.
	ExportError uint64
}

type bridgeBox struct{ b Bridge }

// Writer is the bounded, non-blocking access log (emit.AccessLog).
type Writer struct {
	node     []byte
	clock    clock.Clock
	maxBytes int64

	q       chan *entry
	wake    chan struct{}
	bytes   atomic.Int64
	closed  atomic.Bool
	records sync.Pool
	entries [numClasses]sync.Pool
	bridge  atomic.Pointer[bridgeBox]
	// unflushed counts the lines Next handed out that Flushed has not
	// settled; Close claims them when it gives up on a blocked worker.
	unflushed atomic.Int64

	produced    atomic.Uint64
	queueFull   atomic.Uint64
	exportError atomic.Uint64

	// Worker-owned (logsink's stdout worker).
	pending []*entry
	scratch []byte
}

var _ emit.AccessLog = (*Writer)(nil)

// New returns a Writer. It starts no goroutine: the logsink worker drains
// it (logsink.Options.Access).
func New(o Options) *Writer {
	w := &Writer{node: nodeMember(o.NodeID), clock: o.Clock, maxBytes: o.QueueBytes}
	if w.clock == nil {
		w.clock = clock.Real()
	}
	if w.maxBytes <= 0 {
		w.maxBytes = DefaultQueueBytes
	}
	n := o.QueueRecords
	if n <= 0 {
		n = DefaultQueueRecords
	}
	w.q = make(chan *entry, n)
	w.wake = make(chan struct{}, 1)
	w.records.New = func() any { return new(emit.AccessRecord) }
	for c := range numClasses {
		size := classSize(c)
		class := uint8(c)
		w.entries[c].New = func() any { return &entry{class: class, arena: make([]byte, 0, size)} }
	}
	return w
}

// Acquire returns a pooled, reset record (emit.AccessLog). Its
// FailureModes is nil: append to it or assign a slice; Submit copies the
// entries and never retains the slice.
func (w *Writer) Acquire() *emit.AccessRecord {
	r, ok := w.records.Get().(*emit.AccessRecord)
	if !ok {
		r = &emit.AccessRecord{}
	}
	return r
}

// Submit copies r, truncated, into a pooled entry and queues it without
// blocking; r returns to the pool at once (emit.AccessLog). A closed
// Writer, a full queue or an exhausted byte budget drops the record with
// reason queue_full. The record reserves its actual size (09 req 67):
// entryOverhead plus the bytes its strings need, trued down to the bytes
// copied once fill applied the escaped-size budget.
func (w *Writer) Submit(r *emit.AccessRecord) {
	w.produced.Add(1)
	if w.closed.Load() || len(w.q) == cap(w.q) {
		w.queueFull.Add(1)
		w.putRecord(r)
		return
	}
	n := need(r)
	size := int64(entryOverhead + n)
	if !w.reserve(size) {
		w.queueFull.Add(1)
		w.putRecord(r)
		return
	}
	e := w.getEntry(classFor(n))
	e.fill(r)
	if d := len(e.arena) - n; d < 0 {
		w.bytes.Add(int64(d))
		size += int64(d)
	}
	e.size = size
	w.putRecord(r)
	select {
	case w.q <- e:
		// A Close that ran between the closed check and the send has
		// already drained the queue; drain again so e is counted.
		if w.closed.Load() {
			w.dropQueued()
			return
		}
		select {
		case w.wake <- struct{}{}:
		default:
		}
	default:
		w.queueFull.Add(1)
		w.release(e)
	}
}

// putRecord resets r and returns it to the pool. The record keeps no
// slice: FailureModes may be the caller's (09 req 67).
func (w *Writer) putRecord(r *emit.AccessRecord) {
	*r = emit.AccessRecord{}
	w.records.Put(r)
}

func (w *Writer) reserve(n int64) bool {
	if w.bytes.Add(n) > w.maxBytes {
		w.bytes.Add(-n)
		return false
	}
	return true
}

func (w *Writer) getEntry(class int) *entry {
	e, ok := w.entries[class].Get().(*entry)
	if !ok {
		e = &entry{class: uint8(class), arena: make([]byte, 0, classSize(class))} //nolint:gosec // G115: class < numClasses.
	}
	return e
}

// release returns e's reservation to the budget and e to its pool.
func (w *Writer) release(e *entry) {
	w.bytes.Add(-e.size)
	e.reset()
	w.entries[e.class].Put(e)
}

// Stats returns the access stream counters.
func (w *Writer) Stats() Stats {
	return Stats{Produced: w.produced.Load(), QueueFull: w.queueFull.Load(), ExportError: w.exportError.Load()}
}

// SetBridge sets the OTLP bridge; nil stops OTLP export of access records.
func (w *Writer) SetBridge(b Bridge) {
	if b == nil {
		w.bridge.Store(nil)
		return
	}
	w.bridge.Store(&bridgeBox{b: b})
}

// Close stops the Writer: later records are dropped and counted, the
// records still queued are counted as dropped (queue_full), and the lines
// Next handed to a worker that has not settled them with Flushed (a batch
// an abandoned worker is still writing) count export_error, so records
// lost at exit are counted once (09 section 9 item 20). Call it after the
// stdout worker returned, or after giving up on a worker blocked on
// stdout; lines such a worker settles later are not counted again. It is
// safe to call more than once.
func (w *Writer) Close() {
	w.closed.Store(true)
	w.dropQueued()
	if n := w.unflushed.Swap(0); n > 0 {
		w.exportError.Add(uint64(n))
	}
}

// dropQueued counts every queued record as dropped (queue_full) and
// releases it.
func (w *Writer) dropQueued() {
	for {
		select {
		case e := <-w.q:
			w.queueFull.Add(1)
			w.release(e)
		default:
			return
		}
	}
}

// Wake returns the channel signaled after a record is queued
// (logsink.Stream).
func (w *Writer) Wake() <-chan struct{} { return w.wake }

// Pending returns the number of queued records (logsink.Stream).
func (w *Writer) Pending() int { return len(w.q) }

// Next appends the oldest queued record to dst as one JSON line with its
// newline (logsink.Stream); ok is false when nothing is queued. Worker
// only.
func (w *Writer) Next(dst []byte) ([]byte, bool) {
	select {
	case e := <-w.q:
		dst = e.appendLine(dst, w.node)
		dst = append(dst, '\n')
		w.pending = append(w.pending, e)
		w.unflushed.Add(1)
		return dst, true
	default:
		return dst, false
	}
}

// Flushed settles the lines Next returned since the previous call
// (logsink.Stream): the last lost were not written and count
// export_error; every record then goes to the Bridge, if set, and its
// entry and reservation are released. The first lines Close already
// claimed (see unflushed) are not counted again and reach the Bridge
// flagged Dropped. Worker only.
func (w *Writer) Flushed(lost int) {
	box := w.bridge.Load()
	n := len(w.pending)
	claimed := max(n-int(w.unflushed.Swap(0)), 0)
	for i, e := range w.pending {
		byClose := i < claimed
		dropped := byClose || i >= n-lost
		if dropped && !byClose {
			w.exportError.Add(1)
		}
		if box != nil {
			w.scratch = e.appendLine(w.scratch[:0], w.node)
			ex := Exported{
				Start: e.start, Observed: w.clock.Now(),
				TraceID: e.traceID, SpanID: e.spanID, Sampled: e.sampled,
				Line: w.scratch, Dropped: dropped,
			}
			if err := box.b.Export(&ex); err != nil && !dropped {
				w.exportError.Add(1)
			}
		}
		w.release(e)
	}
	clear(w.pending)
	w.pending = w.pending[:0]
}

// AppendJSON appends the access line of r (without newline) to dst, with
// the truncation rules of Submit; for off-path uses such as tests and the
// /tap view. r is not modified.
func AppendJSON(dst []byte, r *emit.AccessRecord, nodeID string) []byte {
	e := &entry{arena: make([]byte, 0, need(r))}
	e.fill(r)
	return e.appendLine(dst, nodeMember(nodeID))
}
