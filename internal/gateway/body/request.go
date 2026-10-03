// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package body

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// errStale is returned by a reader opened before the body was superseded,
// reopened or reset: it never touches the client stream or recycled bytes.
var errStale = errors.New("body: request body reader is stale")

// Request modes.
const (
	modeEmpty uint8 = iota
	modeGated
	modeStream
)

// Request is the client request body as the Upstream layer sees it; it
// implements snapshot.RequestBody (architecture R-41) over the gate or the
// client stream:
//
//   - empty (declared 0, nil or http.NoBody): replayable; Open returns
//     http.NoBody;
//   - gated (read whole within limits.maxRequestBodyBytes, possibly
//     rewritten by a transform): replayable; every Open is an independent
//     reader from the first byte, so parallel composition steps each get
//     the body (spec 05 req 54); Buffered returns the bytes for GetBody;
//   - streamed: replayable only while no byte has been read and no read is
//     in progress (spec 05 req 31); a second Open after that is
//     snapshot.ErrNotReplayable; the stream is cut at its limit with
//     ErrTooLarge.
//
// Closing an opened reader never closes the client stream. A Request lives
// in the handler's pooled per-request state: Prepare (or SetGated,
// SetStream) at the start, Reset at the end. Readers still held by a
// transport after Reset, or after a later Open replaced them, fail with an
// error and never read another request's bytes. Prepare, the setters,
// Replace and Reset are called by the request's owner, one at a time; the
// accessors and Open are safe from any goroutine at any time (composition
// steps opening the body while the ending protocol ends the request): they
// read under the lock those methods write under.
type Request struct {
	mu   sync.RWMutex // guards the fields below against stale readers
	gen  uint64       // bumped by Reset, Replace and Set*; never reset
	mode uint8

	// Gated: cur is the body (the gate's bytes, or a rewritten body whose
	// reservation acct holds); gate owns the gate reservation until the
	// body is replaced.
	cur  []byte
	gate *Buffer
	acct Account

	// Streamed: st is allocated per request so a read blocked in a
	// transport goroutine never races with the next request using this
	// Request.
	st       *stream
	declared int64

	bud    *Budget
	stripe emit.Stripe

	// own is the gate Prepare reads into, so a gated request body costs no
	// allocation; it is never handed out.
	own Buffer
}

var _ snapshot.RequestBody = (*Request)(nil)

// stream is one client stream shared by the readers Open returns.
type stream struct {
	mu      sync.Mutex
	lim     Limited
	cur     uint64 // id of the current reader
	reading bool   // a Read is in progress
	read    int64  // bytes returned
	eof     bool
	over    bool // the body passed its limit
	dead    bool // the request ended (Reset)
}

// Prepare sets q up for one client request body, following spec 04 req 34
// and spec 05 req 31: declared is the request's Content-Length (-1 when
// unknown). A declared length over limit is ErrTooLarge before body is read
// (no 100 Continue is sent); a declared length of 0 or a nil or
// http.NoBody body is empty; with gate the whole body is read now
// (ReadGate: ErrTooLarge, ErrBudget, a read error or ctx's error, with q
// left empty); otherwise the body streams, cut at limit. Code maps the
// errors with SideRequest. bud also backs Replace; the buffered-bytes
// gauge records on the request's stripe s.
func (q *Request) Prepare(ctx context.Context, body io.Reader, declared int64, gate bool, limit int64, bud *Budget, s emit.Stripe) error {
	q.mu.Lock()
	q.clearLocked()
	q.bud, q.stripe = bud, s
	q.mu.Unlock()
	if err := CheckDeclared(declared, limit); err != nil {
		return err
	}
	if declared == 0 || body == nil || body == http.NoBody {
		return nil
	}
	if !gate {
		q.SetStream(body, declared, limit)
		return nil
	}
	// q.own is released (empty) after clearLocked, and no reader reaches
	// it, so it is filled outside the lock.
	if err := q.own.fill(ctx, body, declared, limit, bud, s); err != nil {
		return err
	}
	q.SetGated(&q.own)
	return nil
}

// SetGated makes g the body; q owns g and releases it on Reset or when the
// body is replaced. Without a budget from Prepare, q takes g's budget and
// stripe for Replace.
func (q *Request) SetGated(g *Buffer) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.gate != g {
		q.clearLocked()
	} else {
		q.gen++ // re-setting the current gate keeps it, readers turn stale
	}
	q.mode, q.gate, q.cur = modeGated, g, g.Bytes()
	if q.bud == nil {
		q.bud, q.stripe = g.bud, g.stripe
	}
}

// SetStream makes body the streamed body, cut at limit bytes; declared is
// its Content-Length (-1 when unknown).
func (q *Request) SetStream(body io.Reader, declared, limit int64) {
	st := &stream{}
	st.lim.Reset(body, limit)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.clearLocked()
	q.mode, q.st, q.declared = modeStream, st, declared
}

// Replace replaces a gated body with b, a body rewritten by a transform
// (spec 04 req 48, spec 07 req 71): b is reserved from the budget like a
// decoded value (ErrBudget, 503 RZ-RT-004, leaves the body unchanged), then
// the superseded body's reservation is returned. Readers opened before are
// stale afterwards. q takes b; the caller must not modify it. Replacing a
// streamed or empty body makes it gated.
//
// b may be, or share memory with, the current gated body (a Filter passing
// back the slice it got from Message.Body, unchanged, truncated, emptied or
// edited in place): the released gate's array goes back to the budget's
// pool, where the next gate on the Node overwrites it, so such a b is
// copied first and q keeps only the copy. An empty b (nil, or a zero-length
// slice wherever it points, whose capacity may reach into the gate) is
// replaced by a fresh empty slice with no capacity.
func (q *Request) Replace(b []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	var next Account
	next.Bind(q.bud, Gate, q.stripe)
	if err := next.Reserve(int64(len(b))); err != nil {
		return err
	}
	switch {
	case len(b) == 0:
		b = []byte{}
	case q.gate != nil && overlaps(b, q.gate.b):
		b = bytes.Clone(b)
	}
	q.clearLocked()
	q.mode, q.cur, q.acct = modeGated, b, next
	return nil
}

// overlaps reports whether b shares memory with the backing array of a,
// from its first element to its capacity. reflect.Value.Pointer gives the
// element addresses without package unsafe; both slices are live for the
// comparison and the heap does not move.
func overlaps(b, a []byte) bool {
	if len(b) == 0 || cap(a) == 0 {
		return false
	}
	pb := reflect.ValueOf(b).Pointer()
	pa := reflect.ValueOf(a).Pointer()
	return pb < pa+uintptr(cap(a)) && pa < pb+uintptr(len(b))
}

// Reset returns every reservation, detaches the client stream and makes
// every reader opened so far stale; q is then empty and ready for reuse.
func (q *Request) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.clearLocked()
	q.bud, q.stripe = nil, 0
}

// clearLocked empties q and invalidates its readers.
func (q *Request) clearLocked() {
	q.gen++
	if q.gate != nil {
		q.gate.Release()
		q.gate = nil
	}
	q.acct.ReleaseAll()
	q.acct = Account{}
	if q.st != nil {
		q.st.mu.Lock()
		q.st.dead = true
		q.st.mu.Unlock()
		q.st = nil
	}
	q.mode, q.cur, q.declared = modeEmpty, nil, 0
}

// Gated returns the gated (or rewritten) body and true, or nil and false
// when the body is empty or streamed. The bytes are valid until Reset or
// Replace. The slice's capacity is its length, so appending to it never
// writes into the gate's spare capacity.
func (q *Request) Gated() ([]byte, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.mode != modeGated {
		return nil, false
	}
	return slices.Clip(q.cur), true
}

// Streamed reports whether the body is streamed.
func (q *Request) Streamed() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.mode == modeStream
}

// BytesRead returns the body bytes read from the client: the gated length,
// or the bytes a streamed body returned so far
// (ruralz_http_request_body_bytes).
func (q *Request) BytesRead() int64 {
	q.mu.RLock()
	defer q.mu.RUnlock()
	switch q.mode {
	case modeGated:
		return int64(len(q.cur))
	case modeStream:
		q.st.mu.Lock()
		defer q.st.mu.Unlock()
		return q.st.read
	default:
		return 0
	}
}

// Exceeded reports whether a streamed body passed its limit: before commit
// the client gets 413 RZ-RT-003; after commit the leg is aborted (spec 04
// req 37).
func (q *Request) Exceeded() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.mode != modeStream {
		return false
	}
	q.st.mu.Lock()
	defer q.st.mu.Unlock()
	return q.st.over
}

// ContentLength implements snapshot.RequestBody: the gated length, the
// declared length of a streamed body (-1 when unknown) or 0.
func (q *Request) ContentLength() int64 {
	q.mu.RLock()
	defer q.mu.RUnlock()
	switch q.mode {
	case modeGated:
		return int64(len(q.cur))
	case modeStream:
		return q.declared
	default:
		return 0
	}
}

// Replayable implements snapshot.RequestBody: empty and gated bodies always
// are; a streamed body is while no byte has been read from it and no read
// is in progress.
func (q *Request) Replayable() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.mode != modeStream {
		return true
	}
	st := q.st
	st.mu.Lock()
	defer st.mu.Unlock()
	return !st.dead && !st.reading && st.read == 0
}

// Open implements snapshot.RequestBody: a reader from the first byte for
// one attempt or step. An empty body (a streamed one found empty included)
// is http.NoBody; a gated body gets an independent reader per call; a
// streamed body gets a reader over the client stream while it is
// replayable, which makes every earlier reader stale, and
// snapshot.ErrNotReplayable afterwards. Closing a reader never closes the
// client stream.
func (q *Request) Open() (io.ReadCloser, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	switch q.mode {
	case modeGated:
		if len(q.cur) == 0 {
			return http.NoBody, nil
		}
		return &gateReader{q: q, gen: q.gen}, nil
	case modeStream:
		st := q.st
		st.mu.Lock()
		defer st.mu.Unlock()
		switch {
		case st.dead:
			return nil, errStale
		case st.eof && st.read == 0:
			return http.NoBody, nil
		case st.reading || st.read > 0:
			return nil, snapshot.ErrNotReplayable
		}
		st.cur++
		return &streamReader{st: st, id: st.cur}, nil
	default:
		return http.NoBody, nil
	}
}

// Buffered implements snapshot.RequestBody: the gated bytes (non-nil, empty
// for an empty gated body), or nil for an empty or streamed body. The
// Upstream layer sets http.Request.GetBody only when it is non-nil and
// should build it on Open, whose readers turn stale when the request ends;
// the bytes are valid until Reset or Replace. As with Gated, the slice's
// capacity is its length.
func (q *Request) Buffered() []byte {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.mode != modeGated {
		return nil
	}
	if q.cur == nil {
		return []byte{}
	}
	return slices.Clip(q.cur)
}

// gateReader reads a gated body from the first byte.
type gateReader struct {
	q      *Request
	gen    uint64
	off    int
	closed atomic.Bool
}

// Read copies the next bytes; errStale once the body was replaced or reset.
func (r *gateReader) Read(p []byte) (int, error) {
	if r.closed.Load() {
		return 0, errStale
	}
	q := r.q
	q.mu.RLock()
	defer q.mu.RUnlock()
	if r.gen != q.gen {
		return 0, errStale
	}
	if r.off >= len(q.cur) {
		return 0, io.EOF
	}
	n := copy(p, q.cur[r.off:])
	r.off += n
	return n, nil
}

// Close ends this reader only.
func (r *gateReader) Close() error {
	r.closed.Store(true)
	return nil
}

// streamReader reads the client stream while it is the current reader.
type streamReader struct {
	st     *stream
	id     uint64
	closed atomic.Bool
}

// Read reads from the client stream, cut at the limit; errStale once a
// later Open replaced this reader or the request ended.
func (r *streamReader) Read(p []byte) (int, error) {
	st := r.st
	st.mu.Lock()
	if r.closed.Load() || st.dead || r.id != st.cur || st.reading {
		st.mu.Unlock()
		return 0, errStale
	}
	if st.eof {
		st.mu.Unlock()
		return 0, io.EOF
	}
	st.reading = true
	st.mu.Unlock()

	n, err := st.lim.Read(p)

	st.mu.Lock()
	st.reading = false
	st.read += int64(n)
	switch {
	case errors.Is(err, io.EOF):
		st.eof = true
	case errors.Is(err, ErrTooLarge):
		st.over = true
	}
	st.mu.Unlock()
	return n, err
}

// Close ends this reader only; the client stream stays open.
func (r *streamReader) Close() error {
	r.closed.Store(true)
	return nil
}
