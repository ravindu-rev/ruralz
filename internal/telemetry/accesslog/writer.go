// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package accesslog is the access-log half of the Ruralz logging pipeline
// (docs/architecture/10-observability.md "Access logs"). Writer
// implements emit.AccessLog.
//
// The request handler (internal/gateway/handler) evaluates
// Gateway.spec.telemetry.accessLog.when itself, with the onLog variables, and
// writes the entry on a runtime error; this package only pools, truncates,
// queues and encodes. On the request goroutine, Submit copies every field,
// truncated, into a pooled entry whose arena it owns, so a queued record never
// references request memory; the query string is never copied, and the
// submitted record, which returns to the pool, keeps no caller slice. A
// record's string values take at most 4 KiB once JSON-escaped, host and
// user_agent at most 256 bytes and path 1,024; cut fields are named in
// truncated. The stdout queue is bounded twice: 8,192 records, and a 4 MiB
// byte budget from which each record reserves its actual size (the bytes
// copied into its arena plus a fixed overhead for its other members) until it
// is written; either limit drops the record from stdout with reason
// queue_full. A typical record reserves under 512 bytes, so the record bound
// is reached first. The budget counts record bytes, not the pooled entry and
// arena class holding them, so the queue's heap can reach about 2.3 times the
// budget (about 9.4 MiB at the defaults, with every record just over 512
// bytes). Submit never blocks and allocates nothing at steady state; a record
// with failure modes costs the caller its FailureModes slice unless the caller
// reuses one of its own.
//
// The stdout worker of internal/telemetry/logsink drains the queue through
// the logsink.Stream methods (Wake, Next, Flushed, Pending), so one
// goroutine owns stdout for both streams. Each line is slog-shaped
// ("level":"INFO","msg":"access") with the members in a fixed order.
//
// The OTLP Bridge, when one is set, takes each record when it is queued, as
// a Pending handle on the same pooled entry, whether or not the stdout queue
// has room for it, so a blocked stdout never stops OTLP log export. Taking a
// record costs the request goroutine one reference count: the Bridge reads
// and encodes the record off the request path and releases it, and the entry
// returns to its pool once stdout and the Bridge are both done with it. Each
// sink counts a record it loses once (ruralz_telemetry_logs_dropped_total
// with sink stdout or otlp): Stats holds the stdout sink's losses, and the
// Bridge counts its own.
package accesslog

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Default queue bounds (targets).
const (
	// DefaultQueueRecords bounds the queue in records.
	DefaultQueueRecords = 8192
	// DefaultQueueBytes is the byte budget queued records reserve.
	DefaultQueueBytes = 4 << 20
)

// Bridge exports access records over OTLP logs (internal/telemetry maps
// the line's members to attributes, the start time to the LogRecord
// timestamp and sets event.name to EventName). Export runs when the record
// is queued, on the request goroutine that submitted it, once per record
// whether or not stdout has room for it. It must not block or do work in
// proportion to the record: it keeps p and calls p.Release exactly once,
// at once or after reading the record on a goroutine of its own. The
// Bridge counts the records it loses itself
// (ruralz_telemetry_logs_dropped_total with sink otlp).
type Bridge interface {
	Export(p Pending)
}

// Pending is one access record handed to the Bridge. The record is
// immutable until Release, and its methods are safe to call from any
// goroutine before Release; none may be called after it.
type Pending struct {
	w *Writer
	e *entry
}

// Start returns the request start, the LogRecord timestamp.
func (p Pending) Start() time.Time { return p.e.start }

// Trace returns the server span context of the request.
func (p Pending) Trace() (traceID [16]byte, spanID [8]byte, sampled bool) {
	return p.e.traceID, p.e.spanID, p.e.sampled
}

// AppendLine appends the record to dst as one JSON object, without the
// newline, exactly as stdout writes it.
func (p Pending) AppendLine(dst []byte) []byte { return p.e.appendLine(dst, p.w.node) }

// Size returns the bytes the record holds: the bytes copied into its arena
// plus the fixed overhead of its other members, the actual size it
// reserves from the stdout budget.
func (p Pending) Size() int64 { return int64(entryOverhead + len(p.e.arena)) }

// Release gives the record back; the entry returns to its pool once stdout
// is done with it too.
func (p Pending) Release() { p.w.deref(p.e) }

// Options configure a Writer.
type Options struct {
	// NodeID is the ULID written as node_id.
	NodeID string
	// QueueRecords and QueueBytes override the defaults when positive.
	QueueRecords int
	QueueBytes   int64
}

// Stats are the access stream counters on stdout: Produced feeds
// ruralz_telemetry_logs_total{stream="access"}, QueueFull and ExportError
// feed ruralz_telemetry_logs_dropped_total with sink stdout, by reason.
// The Bridge counts the OTLP sink's losses.
type Stats struct {
	// Produced counts submitted records.
	Produced uint64
	// QueueFull counts records stdout lost because the queue was full, out
	// of bytes or closed.
	QueueFull uint64
	// ExportError counts records stdout lost to a failed write.
	ExportError uint64
}

type bridgeBox struct{ b Bridge }

// Writer is the bounded, non-blocking access log (emit.AccessLog).
type Writer struct {
	node     []byte
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
}

var _ emit.AccessLog = (*Writer)(nil)

// New returns a Writer. It starts no goroutine: the logsink worker drains
// it (logsink.Options.Access).
func New(o Options) *Writer {
	w := &Writer{node: nodeMember(o.NodeID), maxBytes: o.QueueBytes}
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

// Submit copies r, truncated, into a pooled entry, hands the entry to the
// Bridge when one is set and queues it for stdout, without blocking; r
// returns to the pool at once (emit.AccessLog). A closed Writer, a full
// queue or an exhausted byte budget drops the record from stdout with
// reason queue_full, which never keeps it from the Bridge. The record
// reserves its actual size: entryOverhead plus the bytes its strings need,
// trued down to the bytes copied once fill applied the escaped-size
// budget.
func (w *Writer) Submit(r *emit.AccessRecord) {
	w.produced.Add(1)
	box := w.bridge.Load()
	n := need(r)
	size := int64(entryOverhead + n)
	stdout := !w.closed.Load() && len(w.q) < cap(w.q) && w.reserve(size)
	if !stdout {
		w.queueFull.Add(1)
		if box == nil {
			w.putRecord(r)
			return
		}
	}
	e := w.getEntry(classFor(n))
	e.fill(r)
	w.putRecord(r)
	var refs int32
	if stdout {
		if d := len(e.arena) - n; d < 0 {
			w.bytes.Add(int64(d))
			size += int64(d)
		}
		e.size = size
		refs++
	}
	if box != nil {
		refs++
	}
	e.refs.Store(refs)
	if box != nil {
		box.b.Export(Pending{w: w, e: e})
	}
	if !stdout {
		return
	}
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
// slice: FailureModes may be the caller's.
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

// release ends stdout's hold on e: its reservation returns to the budget
// and its reference is dropped.
func (w *Writer) release(e *entry) {
	w.bytes.Add(-e.size)
	w.deref(e)
}

// deref drops one reference to e; the last one returns e to its pool.
func (w *Writer) deref(e *entry) {
	if e.refs.Add(-1) == 0 {
		e.reset()
		w.entries[e.class].Put(e)
	}
}

// Stats returns the access stream counters.
func (w *Writer) Stats() Stats {
	return Stats{Produced: w.produced.Load(), QueueFull: w.queueFull.Load(), ExportError: w.exportError.Load()}
}

// SetBridge sets the OTLP bridge, which takes every record submitted after
// the call; nil stops OTLP export of access records.
func (w *Writer) SetBridge(b Bridge) {
	if b == nil {
		w.bridge.Store(nil)
		return
	}
	w.bridge.Store(&bridgeBox{b: b})
}

// Close stops the Writer for stdout: later records are dropped from stdout
// and counted, the records still queued are counted as dropped
// (queue_full), and the lines Next handed to a worker that has not settled
// them with Flushed (a batch an abandoned worker is still writing) count
// export_error, so records lost at exit are counted once. A Bridge still
// set keeps taking records. Call it after the stdout worker returned, or
// after giving up on a worker blocked on stdout; lines such a worker
// settles later are not counted again. It is safe to call more than once.
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
// (logsink.Stream): the last lost were not written and count export_error,
// and every entry's reservation is released. The first lines Close already
// claimed (see unflushed) are not counted again. Worker only.
func (w *Writer) Flushed(lost int) {
	n := len(w.pending)
	claimed := max(n-int(w.unflushed.Swap(0)), 0)
	for i, e := range w.pending {
		if i >= claimed && i >= n-lost {
			w.exportError.Add(1)
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
