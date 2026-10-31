// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tap

import (
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Limits of docs/architecture/03-data-plane.md "Admin endpoints" (target
// values).
const (
	// MaxSubscribers is the number of /tap subscribers at once.
	MaxSubscribers = 4
	// BufferBytes is the buffer each subscriber holds.
	BufferBytes = 1 << 20
	// DefaultSample is the sampling rate without ?sample=.
	DefaultSample = 1.0
)

// Encode buffer sizes. A buffer that grew past maxPooledLine while
// encoding a large event is left to the garbage collector instead of
// being pooled, so one large event does not keep memory alive.
const (
	initialLine   = 2 << 10
	maxPooledLine = 64 << 10
)

// Config configures a Hub. Zero values select the defaults.
type Config struct {
	// Dropped is ruralz_tap_events_dropped_total
	// (emit.NodeMetrics.TapEventsDropped): one per event a subscriber
	// loses. nil counts nothing.
	Dropped emit.Counter
	// Rand draws the sampling decisions. It must be safe for concurrent
	// use; nil selects the math/rand/v2 global source.
	Rand rand.Source
	// RequestID returns the requestId of the hub's problem documents, the
	// admin server's function (admin.RequestID); nil leaves it empty.
	RequestID func(*http.Request) string
	// Clock dates the write deadlines; nil selects clock.Real().
	Clock clock.Clock
}

// Hub fans finished exchanges out to the /tap subscribers. Active and
// Publish are the request path; ServeHTTP runs one subscription per
// request. A Hub is safe for concurrent use; a nil *Hub has no
// subscribers, and the zero Hub is New(Config{}).
type Hub struct {
	// active is true while at least one subscriber exists: the one atomic
	// load an exchange pays without subscribers.
	active atomic.Bool
	// subs is the current subscriber list, replaced on every change and
	// never mutated in place.
	subs atomic.Pointer[[]*subscriber]

	dropped   emit.Counter
	src       rand.Source
	requestID func(*http.Request) string
	clk       clock.Clock
	lines     sync.Pool // *[]byte encode buffers

	mu sync.Mutex // serializes subscribe and unsubscribe
}

// New returns a Hub without subscribers.
func New(cfg Config) *Hub {
	return &Hub{dropped: cfg.Dropped, src: cfg.Rand, requestID: cfg.RequestID, clk: cfg.Clock}
}

// now returns the current time of the hub's clock.
func (h *Hub) now() time.Time {
	if h.clk == nil {
		return clock.Real().Now()
	}
	return h.clk.Now()
}

// Active reports whether any subscriber exists. It is one atomic load;
// the request handler calls it before it fills an Event.
func (h *Hub) Active() bool { return h != nil && h.active.Load() }

// Subscribers returns the number of current subscribers.
func (h *Hub) Subscribers() int {
	if h == nil {
		return 0
	}
	if p := h.subs.Load(); p != nil {
		return len(*p)
	}
	return 0
}

// Publish offers ev to every subscriber. Each subscriber samples it at
// its own rate; when at least one takes it, ev is encoded once and the
// line copied into the takers' buffers. A taker whose buffer has no room
// loses the line, counted on stripe s. Publish never blocks on a
// subscriber and keeps no reference to ev.
func (h *Hub) Publish(s emit.Stripe, ev *Event) {
	if h == nil {
		return
	}
	p := h.subs.Load()
	if p == nil {
		return
	}
	// The list never holds more than MaxSubscribers (subscribe), so takers
	// has room for every subscriber.
	var takers [MaxSubscribers]*subscriber
	n := 0
	for _, sub := range *p {
		if n < len(takers) && (sub.all || h.draw() < sub.threshold) {
			takers[n] = sub
			n++
		}
	}
	if n == 0 {
		return
	}
	bp, _ := h.lines.Get().(*[]byte)
	if bp == nil {
		b := make([]byte, 0, initialLine)
		bp = &b
	}
	line := appendLine((*bp)[:0], ev)
	for _, sub := range takers[:n] {
		if sub.offer(line) == lost && h.dropped != nil {
			h.dropped.Add(s, 1)
		}
	}
	if cap(line) <= maxPooledLine {
		*bp = line[:0]
		h.lines.Put(bp)
	}
}

// draw returns one uniform 64-bit value for a sampling decision.
func (h *Hub) draw() uint64 {
	if h.src == nil {
		return rand.Uint64() //nolint:gosec // G404: sampling needs a uniform draw, not an unpredictable one.
	}
	return h.src.Uint64()
}

// subscribe adds a subscriber sampling at rate; nil when MaxSubscribers
// already subscribe.
func (h *Hub) subscribe(rate float64) *subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()
	var cur []*subscriber
	if p := h.subs.Load(); p != nil {
		cur = *p
	}
	if len(cur) >= MaxSubscribers {
		return nil
	}
	s := newSubscriber(rate)
	next := make([]*subscriber, 0, len(cur)+1)
	next = append(append(next, cur...), s)
	h.subs.Store(&next)
	h.active.Store(true)
	return s
}

// unsubscribe removes s and closes it, so a Publish still holding the old
// list skips it.
func (h *Hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	var cur []*subscriber
	if p := h.subs.Load(); p != nil {
		cur = *p
	}
	next := make([]*subscriber, 0, len(cur))
	for _, x := range cur {
		if x != s {
			next = append(next, x)
		}
	}
	h.subs.Store(&next)
	h.active.Store(len(next) > 0)
	h.mu.Unlock()
	s.close()
}
