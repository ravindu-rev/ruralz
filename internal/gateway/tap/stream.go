// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tap

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/problem"
)

// ContentType is the media type of the /tap stream.
const ContentType = "application/x-ndjson"

// Codes of the problem documents ServeHTTP writes.
const (
	// CodeLimit is a subscription refused because MaxSubscribers already
	// subscribe: 503.
	CodeLimit = "RZ-RT-019"
	// CodeInvalidSample is a ?sample= value outside (0, 1] or not a
	// number: 400.
	CodeInvalidSample = "RZ-RT-022"
)

// WriteTimeout is the deadline of every write to a subscriber, flushes
// included (target value of docs/architecture/03-data-plane.md "Admin
// endpoints"). A subscriber that misses it is dropped, freeing its slot.
const WriteTimeout = 10 * time.Second

// sampleParam is the query parameter of the sampling rate.
const sampleParam = "sample"

// ServeHTTP serves one /tap subscription (GET; the admin server routes
// and authenticates). It answers 400 RZ-RT-022 for an invalid ?sample=
// and 503 RZ-RT-019 when MaxSubscribers already subscribe; otherwise it
// commits a 200 application/x-ndjson response at once and streams lines
// until the request context ends or a write fails or misses its deadline.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rate, ok := sampleRate(r.URL.Query())
	if !ok {
		problem.Write(w, problem.New(CodeInvalidSample, h.rid(r)), nil)
		return
	}
	s := h.subscribe(rate)
	if s == nil {
		problem.Write(w, problem.New(CodeLimit, h.rid(r)), nil)
		return
	}
	defer h.unsubscribe(s)
	hdr := w.Header()
	hdr.Set("Content-Type", ContentType)
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	out := &sink{w: w, rc: http.NewResponseController(w), now: h.now}
	if err := out.flush(); err != nil {
		return
	}
	s.stream(r.Context(), out)
}

// stream writes the subscriber's buffer to out until ctx ends or a write
// fails, flushing after each drained batch.
func (s *subscriber) stream(ctx context.Context, out *sink) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		for {
			b := s.pending()
			if len(b) == 0 {
				break
			}
			if err := out.write(b); err != nil {
				return
			}
			s.consume(len(b))
		}
		if err := out.flush(); err != nil {
			return
		}
	}
}

// sink is a subscriber's response: every write and flush runs under its
// own WriteTimeout deadline, and no deadline is left between batches, so
// an idle subscription is never cut. A response that cannot take a
// deadline ends the subscription: no write goes without one.
type sink struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	now func() time.Time
}

// write writes b before its deadline.
func (o *sink) write(b []byte) error {
	if err := o.rc.SetWriteDeadline(o.now().Add(WriteTimeout)); err != nil {
		return err
	}
	_, err := o.w.Write(b) //nolint:gosec // G705: JSON-escaped NDJSON served as nosniff application/x-ndjson, never HTML.
	return err
}

// flush sends what is buffered before its deadline, then clears the
// deadline for the idle time until the next batch.
func (o *sink) flush() error {
	if err := o.rc.SetWriteDeadline(o.now().Add(WriteTimeout)); err != nil {
		return err
	}
	if err := o.rc.Flush(); err != nil {
		return err
	}
	return o.rc.SetWriteDeadline(time.Time{})
}

// rid returns the requestId of r's problem document.
func (h *Hub) rid(r *http.Request) string {
	if h.requestID == nil {
		return ""
	}
	return h.requestID(r)
}

// sampleRate returns the ?sample= rate of q, DefaultSample without one; ok
// is false for a value that is not a number in (0, 1].
func sampleRate(q url.Values) (rate float64, ok bool) {
	if !q.Has(sampleParam) {
		return DefaultSample, true
	}
	v, err := strconv.ParseFloat(q.Get(sampleParam), 64)
	if err != nil || math.IsNaN(v) || v <= 0 || v > 1 {
		return 0, false
	}
	return v, true
}
