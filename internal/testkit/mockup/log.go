// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockup

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
)

// Request is one logged request. Header and Trailer are copies owned by
// the log; Body holds at most Config.MaxLoggedBody bytes.
//
// A leak scan over the log (the canary API key must never reach the mock
// Upstream) is complete only when every request of the scanned window was
// logged whole: Config.LogSize is not negative (a negative size disables
// the log), Stats().LogEvicted is 0 and no Request has BodyTruncated set.
// Size Config.LogSize and Config.MaxLoggedBody for the run, or check
// these conditions.
type Request struct {
	// Method, Host, URI (the request target as received), Path and Proto
	// describe the request line.
	Method, Host, URI, Path, Proto string
	// Header and Trailer are the request's fields. net/http moves
	// Transfer-Encoding out of Header.
	Header, Trailer http.Header
	// Body is the first Config.MaxLoggedBody bytes of the body, and
	// BodyTruncated reports that the body was longer.
	Body          []byte
	BodyTruncated bool
	// RemoteAddr is the client's address.
	RemoteAddr string
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

// add counts one request, the n bytes of whose body body holds the first,
// and, with keep, logs it.
func (l *requestLog) add(r *http.Request, body []byte, n int64, maxBody int, keep bool) {
	var e Request
	if keep {
		e = Request{
			Method:     r.Method,
			Host:       r.Host,
			URI:        r.RequestURI,
			Path:       r.URL.Path,
			Proto:      r.Proto,
			Header:     r.Header.Clone(),
			Trailer:    r.Trailer.Clone(),
			Body:       slices.Clone(body[:min(len(body), max(maxBody, 0))]),
			RemoteAddr: r.RemoteAddr,
		}
		e.BodyTruncated = int64(len(e.Body)) < n
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total++
	if keep && len(l.ring) > 0 {
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
