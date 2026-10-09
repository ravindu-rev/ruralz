// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockup

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Request is one logged request. Header and Trailer are copies owned by
// the log; Body holds at most Config.MaxLoggedBody bytes.
//
// A leak scan over the log (11 req 39: the canary API key must never
// reach the mock Upstream) is complete only when every request of the
// scanned window was logged whole: Config.LogSize is not negative (a
// negative size disables the log), ClearRequests was not called during
// the window, Stats().LogEvicted is 0 and no Request has BodyTruncated
// set. Size Config.LogSize and Config.MaxLoggedBody for the run, or check
// these conditions.
type Request struct {
	// Seq numbers requests from 1 in arrival order (after the body was
	// read), logged or not.
	Seq uint64
	// Time is when the body had been read.
	Time time.Time
	// Method, Host, URI (the request target as received), Path, RawQuery
	// and Proto describe the request line.
	Method, Host, URI, Path, RawQuery, Proto string
	// Header and Trailer are the request's fields. net/http moves
	// Transfer-Encoding out of Header (see TransferEncoding).
	Header, Trailer http.Header
	// ContentLength is the declared body length, -1 when unknown (a
	// chunked HTTP/1.1 body, or HTTP/2 without content-length);
	// TransferEncoding is the HTTP/1.1 transfer codings, outermost first
	// ("chunked"); Close reports that the client asked to close the
	// connection after this request (HTTP/1.x Connection: close). They
	// show how the client framed the request (04 req 20).
	ContentLength    int64
	TransferEncoding []string
	Close            bool
	// Body is the first Config.MaxLoggedBody bytes of the body;
	// BodyBytes is its full length and BodyTruncated reports that Body
	// is shorter.
	Body          []byte
	BodyBytes     int64
	BodyTruncated bool
	// RemoteAddr is the client's address.
	RemoteAddr string
	// TLS reports a TLS connection, ServerName its SNI and ALPN the
	// negotiated protocol.
	TLS        bool
	ServerName string
	ALPN       string
	// Delay is the delay drawn for this request.
	Delay time.Duration
}

// requestLog is a bounded ring of requests plus a total count.
type requestLog struct {
	mu      sync.Mutex
	ring    []Request
	next    int  // ring index of the next entry
	full    bool // the ring wrapped
	total   uint64
	evicted int64         // entries overwritten by newer ones
	notify  chan struct{} // nil until a waiter asks; closed on the next add
}

func newRequestLog(size int) *requestLog {
	l := &requestLog{}
	if size > 0 {
		l.ring = make([]Request, size)
	}
	return l
}

func (l *requestLog) add(r *http.Request, body []byte, n int64, maxBody int, delay time.Duration, keep bool) {
	var e Request
	if keep {
		e = Request{
			Time:             time.Now(),
			Method:           r.Method,
			Host:             r.Host,
			URI:              r.RequestURI,
			Path:             r.URL.Path,
			RawQuery:         r.URL.RawQuery,
			Proto:            r.Proto,
			Header:           r.Header.Clone(),
			Trailer:          r.Trailer.Clone(),
			ContentLength:    r.ContentLength,
			TransferEncoding: slices.Clone(r.TransferEncoding),
			Close:            r.Close,
			Body:             slices.Clone(body[:min(len(body), max(maxBody, 0))]),
			BodyBytes:        n,
			RemoteAddr:       r.RemoteAddr,
			Delay:            delay,
		}
		e.BodyTruncated = int64(len(e.Body)) < n
		if r.TLS != nil {
			e.TLS, e.ServerName, e.ALPN = true, r.TLS.ServerName, r.TLS.NegotiatedProtocol
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total++
	if keep && len(l.ring) > 0 {
		e.Seq = l.total
		if l.full {
			l.evicted++
		}
		l.ring[l.next] = e
		l.next++
		if l.next == len(l.ring) {
			l.next, l.full = 0, true
		}
	}
	if l.notify != nil {
		close(l.notify)
		l.notify = nil
	}
}

// counts returns the total and the evictions.
func (l *requestLog) counts() (uint64, int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total, l.evicted
}

// changed returns the current total and a channel closed at the next
// request.
func (l *requestLog) changed() (uint64, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.notify == nil {
		l.notify = make(chan struct{})
	}
	return l.total, l.notify
}

func (l *requestLog) snapshot() []Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.full {
		return slices.Clone(l.ring[:l.next])
	}
	return append(slices.Clone(l.ring[l.next:]), l.ring[:l.next]...)
}

// Requests returns the logged requests, oldest first (at most
// Config.LogSize, the newest kept; Stats().LogEvicted counts the older
// ones dropped).
func (s *Server) Requests() []Request { return s.log.snapshot() }

// FindRequests returns the logged requests pred accepts, oldest first.
func (s *Server) FindRequests(pred func(Request) bool) []Request {
	return slices.DeleteFunc(s.log.snapshot(), func(r Request) bool { return !pred(r) })
}

// LastRequest returns the newest logged request.
func (s *Server) LastRequest() (Request, bool) {
	all := s.log.snapshot()
	if len(all) == 0 {
		return Request{}, false
	}
	return all[len(all)-1], true
}

// ClearRequests empties the request log; Seq numbering and the count
// WaitRequests uses keep running.
func (s *Server) ClearRequests() {
	s.log.mu.Lock()
	defer s.log.mu.Unlock()
	clear(s.log.ring)
	s.log.next, s.log.full = 0, false
}

// WaitRequests waits until n requests in total have been received (their
// bodies read), logged or not. It returns an error wrapping ctx's error
// when ctx ends first, or ErrClosed when the Server closed (and its last
// handler returned) with fewer than n requests.
func (s *Server) WaitRequests(ctx context.Context, n uint64) error {
	for {
		total, ch := s.log.changed()
		if total >= n {
			return nil
		}
		select {
		case <-ch:
		case <-s.drained:
			if total, _ = s.log.counts(); total >= n {
				return nil
			}
			return fmt.Errorf("mockup: waiting for %d requests (have %d): %w", n, total, ErrClosed)
		case <-ctx.Done():
			return fmt.Errorf("mockup: waiting for %d requests (have %d): %w", n, total, ctx.Err())
		}
	}
}
