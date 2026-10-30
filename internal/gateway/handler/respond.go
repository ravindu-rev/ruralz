// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/exchange"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/problem"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Field names and values the handler writes.
const (
	headerContentType   = "Content-Type"
	headerContentLength = "Content-Length"
	headerCacheControl  = "Cache-Control"
	headerDate          = "Date"
	headerTrailer       = "Trailer"
	noStore             = "no-store"
	eventStream         = "text/event-stream"
)

// commit writes the client response onResponse left (exchange.Reply): the
// header section under the commit locks, then the body: a problem document
// for a generated error, the whole buffered body, or the Upstream stream
// copied as it arrives, then the Upstream's trailers. Every write is bound
// by the connection deadlines, not by the request context.
func (st *state) commit() {
	reply := st.x.Reply()
	st.stream = reply.Stream
	doc := reply.Generated && reply.Code != "" && !reply.Buffered
	if end := st.writeHead(&reply, doc); end != "" || doc {
		st.writeScratch()
		st.gwEnd = st.h.clk.Now()
		return
	}
	switch {
	case !bodyAllowed(st.r.Method, st.status):
	case reply.Buffered:
		if st.writeClient(reply.Body) {
			st.writeTrailers(reply.Stream, reply.Generated || reply.Cached)
		}
	case reply.Stream != nil:
		if st.copyBody(reply.Stream, flushes(st.w.Header())) {
			st.writeTrailers(reply.Stream, reply.Generated || reply.Cached)
		}
	}
	if st.gwEnd.IsZero() {
		st.gwEnd = st.h.clk.Now()
	}
	st.afterBody()
}

// writeHead writes the status and header section of reply under wmu and
// rec.Mu, so no 103 relay interleaves and the ending protocol sees the
// commit. A request the ending protocol ended first gets 503 with the end
// code instead, whose code it returns. doc prepares the problem document
// of a generated error in st.scratch.
func (st *state) writeHead(reply *exchange.Reply, doc bool) (end string) {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	st.rec.Mu.Lock()
	defer st.rec.Mu.Unlock()
	st.rec.Committed, st.committed = true, true
	hdr := st.w.Header()
	if code := retire.Code(st.rec.Ended); code != "" {
		clear(hdr)
		st.problemHead(hdr, code, "")
		return code
	}
	for k, vs := range reply.Header {
		hdr[k] = vs
	}
	status := reply.Status
	st.status, st.generated, st.fromUpstream = status, reply.Generated, !reply.Generated
	if reply.Generated {
		st.code = reply.Code
	}
	now := st.h.clk.Now()
	if reply.Generated || len(hdr[headerDate]) == 0 {
		// An Upstream's Date passes; a response without one, and every
		// generated response, gets the Node's
		// (docs/architecture/07-multi-protocol.md "HTTP intermediary
		// behavior").
		hdr[headerDate] = st.h.date.get(now)
	}
	st.rateLimitFields(hdr, status)
	switch {
	case doc:
		st.problemBody(hdr, reply.Code, reply.Detail, status)
	case bodyAllowed(st.r.Method, status):
		if _, ok := hdr[headerContentType]; !ok {
			// A nil value keeps net/http from sniffing a type the
			// Upstream did not send.
			hdr[headerContentType] = nil
		}
		if reply.Buffered && len(hdr[headerContentLength]) == 0 {
			hdr[headerContentLength] = []string{strconv.Itoa(len(reply.Body))}
		}
		if !reply.Generated && !reply.Cached {
			st.declareTrailers(hdr)
		}
	}
	st.w.WriteHeader(status)
	if isEventStream(hdr) {
		// Gateway-added time of an event stream ends at header commit.
		st.gwEnd = now
	}
	return ""
}

// commitBare commits a problem document for code with no other field, for
// a response no Filter Chain built: pre-routing refusals, an ended
// request, an internal fault. A request the ending protocol ended first
// gets its end code instead. It returns false when a response was already
// committed.
func (st *state) commitBare(code string) bool {
	if !st.headBare(code) {
		return false
	}
	st.writeScratch()
	st.gwEnd = st.h.clk.Now()
	return true
}

// headBare writes the bare problem's header section under the commit
// locks; false when something was committed already.
func (st *state) headBare(code string) bool {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	if st.committed {
		return false
	}
	st.rec.Mu.Lock()
	defer st.rec.Mu.Unlock()
	st.rec.Committed, st.committed = true, true
	if c := retire.Code(st.rec.Ended); c != "" {
		code = c
	}
	hdr := st.w.Header()
	clear(hdr)
	st.problemHead(hdr, code, "")
	return true
}

// problemHead fills hdr for the problem document of code at its status,
// with the rate-limit fields Filters recorded, and writes the status.
func (st *state) problemHead(hdr http.Header, code, detail string) {
	status := st.h.statusOf(code)
	hdr[headerDate] = st.h.date.get(st.h.clk.Now())
	st.rateLimitFields(hdr, status)
	st.problemBody(hdr, code, detail, status)
	st.status, st.code, st.generated, st.fromUpstream = status, code, true, false
	st.w.WriteHeader(status)
}

// problemBody prepares the RFC 9457 document of code in st.scratch and
// sets its Content-Type, Cache-Control and Content-Length
// (docs/architecture/03-data-plane.md "Error response format"). It never
// echoes request content.
func (st *state) problemBody(hdr http.Header, code, detail string, status int) {
	id := ""
	if st.x != nil {
		id = st.x.Exchange().RequestID()
	} else {
		id = hex.EncodeToString(st.decision.TraceID[:])
	}
	st.scratch = problem.Append(st.scratch[:0], problem.Problem{
		Title: problem.Title(code, status), Status: status, Code: code, RequestID: id, Detail: detail,
	})
	hdr[headerContentType] = []string{problem.ContentType}
	hdr[headerCacheControl] = []string{noStore}
	hdr[headerContentLength] = []string{strconv.Itoa(len(st.scratch))}
}

// writeScratch writes the problem document prepared in st.scratch, unless
// the response carries no content (a HEAD request sends the header
// section only).
func (st *state) writeScratch() {
	if bodyAllowed(st.r.Method, st.status) {
		st.writeClient(st.scratch)
	}
}

// writeClient writes b to the client as client I/O, outside gateway-added
// time, and reports whether it was written whole.
func (st *state) writeClient(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	t, clk := st.timer(), st.h.clk
	t.Enter(clk.Now())
	n, err := st.w.Write(b)
	t.Leave(clk.Now())
	st.respBytes += int64(n)
	return err == nil
}

// timer returns the request's gateway-added time measurement, or one that
// measures nothing before the exchange began.
func (st *state) timer() emit.Excluder {
	if st.x != nil {
		return st.x.Timer()
	}
	return noExcluder{}
}

// noExcluder marks nothing.
type noExcluder struct{}

func (noExcluder) Enter(time.Time) {}
func (noExcluder) Leave(time.Time) {}

// copyBody copies an Upstream body to the client as it arrives, through
// the CopyBufferBytes buffer, feeding every Response Cache tee the bytes it
// writes and completing them at the body's end; it flushes after every
// Upstream read when flush is set (no Content-Length, or an event stream).
// It reports whether the whole body was written. A failed Upstream read
// after commit ends the stream with an error: the request aborts so the
// client never takes a cut body as whole, with the read's code or
// RZ-UP-009; a failed client write ends the copy.
func (st *state) copyBody(src io.Reader, flush bool) bool {
	if st.buf == nil {
		st.buf = make([]byte, CopyBufferBytes)
	}
	tees := st.x.Tees()
	t, clk := st.timer(), st.h.clk
	for {
		n, rerr := src.Read(st.buf)
		if n > 0 {
			p := st.buf[:n]
			for _, tee := range tees {
				_, _ = tee.Write(p)
			}
			t.Enter(clk.Now())
			w, werr := st.w.Write(p)
			if werr == nil && flush {
				if ferr := st.rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
					werr = ferr
				}
			}
			t.Leave(clk.Now())
			st.respBytes += int64(w)
			if werr != nil {
				return false
			}
		}
		switch {
		case rerr == nil:
		case errors.Is(rerr, io.EOF):
			for _, tee := range tees {
				tee.Complete()
			}
			return true
		default:
			code, _ := errcode.CodeOf(rerr)
			if code == "" {
				code = codeAfterCommit
			}
			st.code, st.abort = code, true
			return false
		}
	}
}

// afterBody applies what can still end a committed response: the ending
// protocol's end code, and a streamed request body that passed its limit
// after commit, which resets the stream
// (docs/architecture/03-data-plane.md "Streaming").
func (st *state) afterBody() {
	if c := st.endedCode(); c != "" {
		st.code = c
	}
	if st.x.RequestBody().Exceeded() {
		st.code, st.abort = codeBodyTooLarge, true
	}
}

// endedCode returns the end code the ending protocol set, "" while it has
// not ended the request.
func (st *state) endedCode() string {
	st.rec.Mu.Lock()
	defer st.rec.Mu.Unlock()
	return retire.Code(st.rec.Ended)
}

// declareTrailers announces the trailer fields the Upstream declared,
// those IsForwardableTrailer admits, so net/http frames the response for
// them: on HTTP/1.1 chunked, without Content-Length; an HTTP/1.0 client
// cannot receive a trailer section and gets none.
func (st *state) declareTrailers(hdr http.Header) {
	if len(st.upTrailer) == 0 || !st.r.ProtoAtLeast(1, 1) {
		return
	}
	for k := range st.upTrailer {
		if httpfield.IsForwardableTrailer(k) {
			st.trailerKeys = append(st.trailerKeys, k)
		}
	}
	if len(st.trailerKeys) == 0 {
		return
	}
	slices.Sort(st.trailerKeys)
	hdr[headerTrailer] = []string{strings.Join(st.trailerKeys, ", ")}
	if st.r.ProtoMajor == 1 {
		delete(hdr, headerContentLength)
	}
}

// writeTrailers sends the Upstream's trailer fields after the body,
// without hop-by-hop and framing fields (docs/architecture/03-data-plane.md
// "Upstream layer"): the trailer section as the body's end completed it,
// undeclared fields included where the leg body exposes it. A generated or
// cached response has none, and an HTTP/1.0 client cannot receive one.
func (st *state) writeTrailers(src io.Reader, own bool) {
	if own || !st.r.ProtoAtLeast(1, 1) {
		return
	}
	tr := st.upTrailer
	if tb, ok := src.(interface{ Trailer() http.Header }); ok {
		if t := tb.Trailer(); t != nil {
			tr = t
		}
	}
	hdr := st.w.Header()
	for k, vs := range tr {
		if len(vs) == 0 || !httpfield.IsForwardableTrailer(k) {
			continue
		}
		if slices.Contains(st.trailerKeys, k) {
			hdr[k] = vs
		} else {
			hdr[http.TrailerPrefix+k] = vs
		}
	}
}

// HintsToken implements snapshot.EarlyHints: the current request's token.
func (st *state) HintsToken() uint64 {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	return st.gen
}

// RelayHints implements snapshot.EarlyHints: it writes the fields of h,
// without hop-by-hop ones, as a 103 Early Hints response to an HTTP/1.1 or
// later client, while token is the request's and nothing final was
// written, then takes them out of the header map, which net/http keeps
// after a 1xx (docs/architecture/07-multi-protocol.md "HTTP intermediary
// behavior").
func (st *state) RelayHints(token uint64, h http.Header) {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	if st.done || st.committed || token != st.gen || st.w == nil {
		return
	}
	hdr := st.w.Header()
	n := 0
	for k, vs := range h {
		if len(vs) == 0 || httpfield.IsHopByHop(k) {
			continue
		}
		hdr[k] = vs
		n++
	}
	if n == 0 {
		return
	}
	st.w.WriteHeader(http.StatusEarlyHints)
	for k := range h {
		delete(hdr, k)
	}
}

// writeBareProblem writes the problem document of code for a request
// answered without a pinned snapshot: 503 RZ-RT-005 or RZ-RT-016.
func (h *Handler) writeBareProblem(w http.ResponseWriter, r *http.Request, code string, d *emit.Decision) {
	status := h.statusOf(code)
	var buf [256]byte
	doc := problem.Append(buf[:0], problem.Problem{
		Title: problem.Title(code, status), Status: status, Code: code, RequestID: hex.EncodeToString(d.TraceID[:]),
	})
	hdr := w.Header()
	hdr[headerContentType] = []string{problem.ContentType}
	hdr[headerCacheControl] = []string{noStore}
	hdr[headerContentLength] = []string{strconv.Itoa(len(doc))}
	hdr[headerDate] = h.date.get(h.clk.Now())
	w.WriteHeader(status)
	if bodyAllowed(r.Method, status) {
		_, _ = w.Write(doc)
	}
}

// bodyAllowed reports whether a response to method with status carries
// content: never for HEAD, 1xx, 204 or 304.
func bodyAllowed(method string, status int) bool {
	switch {
	case method == http.MethodHead:
		return false
	case status < 200, status == http.StatusNoContent, status == http.StatusNotModified:
		return false
	default:
		return true
	}
}

// flushes reports whether an Upstream body is flushed after every read:
// it has no Content-Length or is an event stream
// (docs/architecture/03-data-plane.md "Streaming").
func flushes(h http.Header) bool {
	return len(h[headerContentLength]) == 0 || isEventStream(h)
}

// isEventStream reports a text/event-stream response.
func isEventStream(h http.Header) bool {
	vs := h[headerContentType]
	if len(vs) == 0 {
		return false
	}
	mt, _, _ := strings.Cut(vs[0], ";")
	return strings.EqualFold(strings.TrimSpace(mt), eventStream)
}
