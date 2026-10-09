// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package logsink is the process-log half of the Ruralz logging pipeline
// (docs/architecture/10-observability.md "Process logs"; ADR-0018 row
// Logs; spec 09 requirements 61 to 65, spec 04 requirement 79).
//
// A Sink owns stdout. Its Handler is the only slog.Handler of ruralzd: it
// checks the fixed level (RURALZ_LOG_LEVEL), clones the record with the
// handler's derived state and the context's trace IDs, resolves and
// redacts values that could leak or change (secret values, credential
// headers, URLs with query strings, header maps), and queues it without
// blocking into a queue bounded to 4,096 records and 1 MiB. A full queue
// drops the record and counts it (ruralz_telemetry_logs_dropped_total
// stream process, reason queue_full). No request goroutine ever writes.
//
// Redaction and copying cover these value types, directly or through one
// pointer: LogValuer (resolved at once), http.Header and
// map[string][]string (copied, credential headers [REDACTED]),
// url.Values (a parsed query, written as [REDACTED]), *http.Request
// (method, host and path only), url.URL and *url.URL (no user
// information, query or fragment), []byte (copied), and errors and
// []error holding a *url.Error (text with the URL redacted). Any other
// value is queued by reference and encoded with encoding/json on the
// worker, as given: a reference value (slice, map, pointer, struct
// holding one) must not change once logged, and a struct that holds a
// header map, URL or error is not redacted, so log those fields directly.
// An error whose own text quotes a *url.Error's text again (with %q, say)
// escapes the URL twice; that spelling is not found and not redacted.
//
// One worker goroutine (Run) drains the process queue and, when present,
// a second Stream (the access log of internal/telemetry/accesslog), encodes
// each record as one JSON line into a 64 KiB batch, and writes the batch
// to stdout when it fills or the queues drain. After the write it hands
// each record once to the OTLP Bridge, if one is set. A failed write
// counts export_error for the stream that lost the line; a record is
// counted as dropped at most once. Close counts what an abandoned worker
// still holds: queued records as queue_full, and the lines of the batch
// it is writing as export_error.
//
// Process lines carry, in order: time (RFC 3339 UTC with microseconds),
// level, msg, component, node_id, revision (absent before the first
// activation), trace_id and span_id (when the context carries a span),
// code and error, then the handler's and the record's attributes.
package logsink

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Default bounds (proposed in 09 req 61 and 62).
const (
	// DefaultQueueRecords bounds the process-log queue in records.
	DefaultQueueRecords = 4096
	// DefaultQueueBytes bounds the process-log queue in estimated bytes.
	DefaultQueueBytes = 1 << 20
	// DefaultBatchBytes is the stdout batch size, written when full or
	// when the queues drain.
	DefaultBatchBytes = 64 << 10
	// ErrorLogRate is the records per second the http.Server.ErrorLog
	// bridge passes (04 req 79, proposed).
	ErrorLogRate = 10
)

// entryOverhead approximates the fixed bytes of a queued record.
const entryOverhead = 320

// Stream is a second record queue drained by the Sink's worker, so one
// goroutine owns stdout for both log streams (ADR-0018 row Logs).
// internal/telemetry/accesslog.Writer implements it. Next, Flushed and
// Pending are called from the worker only. The Stream counts its own
// losses; when its owner gives up on a worker blocked in a write, the
// Stream counts the lines Next returned that Flushed has not settled.
type Stream interface {
	// Wake returns a channel that receives after records are queued.
	Wake() <-chan struct{}
	// Next appends the oldest queued record to dst as one JSON line with
	// its newline; ok is false when nothing is queued.
	Next(dst []byte) (line []byte, ok bool)
	// Flushed reports the stdout write of every line Next returned since
	// the previous call; the last lost of them were not written.
	Flushed(lost int)
	// Pending returns the number of queued records.
	Pending() int
}

// Bridge exports process records over OTLP logs; internal/telemetry
// implements it over the otelslog handler, building the span context from
// TraceID and SpanID. Export runs on the worker after the stdout write,
// once per record, and must not block; an error counts export_error
// unless the record was already counted as dropped.
type Bridge interface {
	Export(r *Exported) error
}

// Exported is one record handed to the Bridge, valid until Export returns.
type Exported struct {
	// Record has the time, level and message, then component, revision,
	// code and error, then the handler's and the record's attributes with
	// groups applied, all resolved and redacted. node_id is omitted (the
	// OTLP resource carries service.instance.id), and the trace IDs are
	// separate.
	Record slog.Record
	// TraceID and SpanID are the logging context's span, when HasTrace.
	TraceID [16]byte
	SpanID  [8]byte
	// HasTrace reports a valid span context.
	HasTrace bool
	// Dropped is true when the stdout write already lost and counted the
	// record; the Bridge must not count it again (09 req 65).
	Dropped bool
}

// Options configure a Sink.
type Options struct {
	// Stdout receives the JSON lines; required.
	Stdout io.Writer
	// Level is the fixed process level (ParseLevel).
	Level slog.Level
	// NodeID is the ULID written as node_id; empty omits it.
	NodeID string
	// Clock paces the rate-limited handlers; nil is clock.Real().
	Clock clock.Clock
	// TraceContext returns the trace and span IDs of ctx's valid span
	// context (injected by internal/telemetry); nil logs no trace IDs.
	TraceContext func(ctx context.Context) (traceID [16]byte, spanID [8]byte, ok bool)
	// ReplaceAttr is an extra redaction hook (internal/redact), called on
	// the worker for every non-group attribute with its groups.
	ReplaceAttr func(groups []string, a slog.Attr) slog.Attr
	// Access is the access-log stream the worker also drains; optional.
	Access Stream
	// QueueRecords, QueueBytes and BatchBytes override the defaults when
	// positive (tests).
	QueueRecords int
	QueueBytes   int64
	BatchBytes   int
}

// Stats are the counters of one log stream (09 req 65): Produced feeds
// ruralz_telemetry_logs_total, QueueFull and ExportError feed
// ruralz_telemetry_logs_dropped_total by reason.
type Stats struct {
	// Produced counts records handled (at or above the level).
	Produced uint64
	// QueueFull counts records dropped because a queue was full or closed.
	QueueFull uint64
	// ExportError counts records lost by a failed stdout write or Bridge.
	ExportError uint64
}

// ErrRunning is returned by Run when the worker already runs.
var ErrRunning = errors.New("logsink: worker already running")

// ErrNoStdout is returned by New without a Stdout writer.
var ErrNoStdout = errors.New("logsink: Options.Stdout is required")

type bridgeBox struct{ b Bridge }

// mark ends one line of the current batch.
type mark struct {
	end    int
	access bool
}

// Sink owns the process-log queue and the stdout worker.
type Sink struct {
	out          io.Writer
	level        slog.Level
	nodeID       string
	clock        clock.Clock
	traceContext func(context.Context) ([16]byte, [8]byte, bool)
	replace      func([]string, slog.Attr) slog.Attr
	access       Stream
	batchMax     int
	maxBytes     int64

	q        chan *entry
	bytes    atomic.Int64
	pool     sync.Pool
	revision atomic.Pointer[string]
	creds    atomic.Pointer[credSet]
	bridge   atomic.Pointer[bridgeBox]
	closed   atomic.Bool
	running  atomic.Bool
	// unflushed counts the process lines encoded into the batch and not
	// yet settled by flush; Close claims them when it gives up on a worker
	// blocked in Write.
	unflushed atomic.Int64

	produced    atomic.Uint64
	queueFull   atomic.Uint64
	exportError atomic.Uint64

	// Worker-owned.
	batch       []byte
	marks       []mark
	pending     []*entry
	accessLines int
}

// entry is one queued process record.
type entry struct {
	st       *state
	rec      slog.Record
	revision string
	traceID  [16]byte
	spanID   [8]byte
	hasTrace bool
	size     int64
}

// New returns a Sink; start its worker with Run.
func New(o Options) (*Sink, error) {
	if o.Stdout == nil {
		return nil, ErrNoStdout
	}
	s := &Sink{
		out:          o.Stdout,
		level:        o.Level,
		nodeID:       o.NodeID,
		clock:        o.Clock,
		traceContext: o.TraceContext,
		replace:      o.ReplaceAttr,
		access:       o.Access,
		batchMax:     o.BatchBytes,
		maxBytes:     o.QueueBytes,
	}
	if s.clock == nil {
		s.clock = clock.Real()
	}
	if s.batchMax <= 0 {
		s.batchMax = DefaultBatchBytes
	}
	if s.maxBytes <= 0 {
		s.maxBytes = DefaultQueueBytes
	}
	records := o.QueueRecords
	if records <= 0 {
		records = DefaultQueueRecords
	}
	s.q = make(chan *entry, records)
	s.pool.New = func() any { return new(entry) }
	s.creds.Store(newCredSet(nil))
	s.batch = make([]byte, 0, s.batchMax+s.batchMax/4)
	return s, nil
}

// Level returns the fixed process level.
func (s *Sink) Level() slog.Level { return s.level }

// Handler returns a handler whose records carry component; an empty
// component omits the member.
func (s *Sink) Handler(component string) *Handler {
	st := &state{component: component, levels: []level{{}}}
	st.encode(s)
	return &Handler{s: s, st: st}
}

// Logger returns a logger for component (the telemetry Runtime's logger
// factory hands these out).
func (s *Sink) Logger(component string) *slog.Logger { return slog.New(s.Handler(component)) }

// ErrorLog returns the handler http.Server.ErrorLog is bridged to
// (slog.NewLogLogger(h, slog.LevelWarn)): component's handler limited to
// ErrorLogRate records per second (04 req 79).
func (s *Sink) ErrorLog(component string) slog.Handler {
	return RateLimit(s.Handler(component), s.clock, ErrorLogRate)
}

// SetRevision sets the active Revision display ("rev-<12 hex>") written
// as revision on later records; empty removes it.
func (s *Sink) SetRevision(display string) {
	if display == "" {
		s.revision.Store(nil)
		return
	}
	s.revision.Store(&display)
}

// SetCredentialHeaders sets the header names redacted besides the
// built-in credential headers: every auth.api-key Policy's header of the
// active Revision and the headers set by upstream-auth Filters (06 req 92).
func (s *Sink) SetCredentialHeaders(names []string) { s.creds.Store(newCredSet(names)) }

// SetBridge sets the OTLP bridge; nil stops OTLP export of process logs.
func (s *Sink) SetBridge(b Bridge) {
	if b == nil {
		s.bridge.Store(nil)
		return
	}
	s.bridge.Store(&bridgeBox{b: b})
}

// Stats returns the process stream counters.
func (s *Sink) Stats() Stats {
	return Stats{Produced: s.produced.Load(), QueueFull: s.queueFull.Load(), ExportError: s.exportError.Load()}
}

func (s *Sink) reserve(n int64) bool {
	if s.bytes.Add(n) > s.maxBytes {
		s.bytes.Add(-n)
		return false
	}
	return true
}

// resize changes a reservation of old bytes to n; when the growth does not
// fit the budget it releases the whole reservation and returns false.
func (s *Sink) resize(old, n int64) bool {
	if d := n - old; d > 0 && !s.reserve(d) {
		s.bytes.Add(-old)
		return false
	}
	if n < old {
		s.bytes.Add(n - old)
	}
	return true
}

func (s *Sink) getEntry() *entry {
	e, ok := s.pool.Get().(*entry)
	if !ok {
		e = new(entry)
	}
	return e
}

// release returns e's bytes to the budget and e to the pool.
func (s *Sink) release(e *entry) {
	s.bytes.Add(-e.size)
	*e = entry{}
	s.pool.Put(e)
}

// Run is the stdout worker: it drains the process queue and the access
// Stream until ctx is done, then encodes what is still queued, writes it
// and returns nil. Its owner (the telemetry Runtime) waits for it; a write
// blocked on stdout cannot be interrupted, so the owner gives up after its
// Drain deadline and calls Close (09 section 9 item 20).
func (s *Sink) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer s.running.Store(false)
	var wake <-chan struct{}
	if s.access != nil {
		wake = s.access.Wake()
	}
	done := ctx.Done()
	for {
		select {
		case <-done:
			s.final()
			return nil
		case e := <-s.q:
			s.encode(e)
			s.pump(done)
		case <-wake:
			s.pump(done)
		}
	}
}

// pump encodes every queued record of both streams, alternating, then
// writes the batch; it stops early once done is closed.
func (s *Sink) pump(done <-chan struct{}) {
	for {
		progressed := false
		select {
		case e := <-s.q:
			s.encode(e)
			progressed = true
		default:
		}
		if s.access != nil && s.nextAccess() {
			progressed = true
		}
		if !progressed {
			break
		}
		select {
		case <-done:
			s.flush()
			return
		default:
		}
	}
	s.flush()
}

// final encodes the records queued when Run was canceled (bounded by the
// queue lengths at that moment) and writes them.
func (s *Sink) final() {
drain:
	for n := len(s.q); n > 0; n-- {
		select {
		case e := <-s.q:
			s.encode(e)
		default:
			break drain
		}
	}
	if s.access != nil {
		for n := s.access.Pending(); n > 0; n-- {
			if !s.nextAccess() {
				break
			}
		}
	}
	s.flush()
}

// encode appends one process record to the batch.
func (s *Sink) encode(e *entry) {
	s.batch = s.appendRecord(s.batch, e)
	s.marks = append(s.marks, mark{end: len(s.batch)})
	s.pending = append(s.pending, e)
	s.unflushed.Add(1)
	if len(s.batch) >= s.batchMax {
		s.flush()
	}
}

// nextAccess appends one access line to the batch; false when none.
func (s *Sink) nextAccess() bool {
	b, ok := s.access.Next(s.batch)
	if !ok {
		return false
	}
	s.batch = b
	s.marks = append(s.marks, mark{end: len(s.batch), access: true})
	s.accessLines++
	if len(s.batch) >= s.batchMax {
		s.flush()
	}
	return true
}

// flush writes the batch, attributes lines lost to a failed write to
// their stream, hands the process records to the Bridge and releases them.
func (s *Sink) flush() {
	lostProcess, lostAccess := 0, 0
	if len(s.batch) > 0 {
		n, err := s.out.Write(s.batch)
		if err != nil || n < len(s.batch) {
			for i := len(s.marks) - 1; i >= 0 && s.marks[i].end > n; i-- {
				if s.marks[i].access {
					lostAccess++
				} else {
					lostProcess++
				}
			}
		}
	}
	s.finishProcess(lostProcess)
	if s.accessLines > 0 {
		s.access.Flushed(lostAccess)
		s.accessLines = 0
	}
	s.batch = s.batch[:0]
	s.marks = s.marks[:0]
	if cap(s.batch) > 4*s.batchMax {
		s.batch = make([]byte, 0, s.batchMax+s.batchMax/4)
	}
}

// finishProcess counts the lost process lines, exports the records to the
// Bridge and releases them. The last lost records were not written; the
// first ones Close claimed (see unflushed) were already counted, so they
// are neither counted again nor counted by the Bridge.
func (s *Sink) finishProcess(lost int) {
	n := len(s.pending)
	claimed := max(n-int(s.unflushed.Swap(0)), 0)
	box := s.bridge.Load()
	for i, e := range s.pending {
		byClose := i < claimed
		dropped := byClose || i >= n-lost
		if dropped && !byClose {
			s.exportError.Add(1)
		}
		if box != nil {
			ex := s.exported(e, dropped)
			if err := box.b.Export(&ex); err != nil && !dropped {
				s.exportError.Add(1)
			}
		}
		s.release(e)
	}
	clear(s.pending)
	s.pending = s.pending[:0]
}

// Close stops the Sink: later records are dropped and counted, the records
// still queued are counted as dropped (queue_full), and the process lines
// of a batch an abandoned worker is still writing count export_error, so
// records lost at exit are counted once (09 section 9 item 20). Call it
// after Run returned, or after giving up on a Run blocked on stdout; a
// batch that such a worker writes later is not counted again. It is safe
// to call more than once.
func (s *Sink) Close() {
	s.closed.Store(true)
	s.dropQueued()
	if n := s.unflushed.Swap(0); n > 0 {
		s.exportError.Add(uint64(n))
	}
}

// dropQueued counts every queued record as dropped (queue_full) and
// releases it.
func (s *Sink) dropQueued() {
	for {
		select {
		case e := <-s.q:
			s.queueFull.Add(1)
			s.release(e)
		default:
			return
		}
	}
}
