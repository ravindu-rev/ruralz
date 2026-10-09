// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package retire

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// RZ codes of the ending protocol (spec 04 reqs 52, 53 and 63): 503 before
// commit, a reset stream or closed connection after it.
const (
	CodeGraceEnded   = "RZ-RT-014"
	CodeDrainReached = "RZ-RT-016"
)

// ErrEnded is the cause EndError wraps.
var ErrEnded = errors.New("retire: request ended by the ending protocol")

// ErrEndReason rejects EndAll with any reason but snapshot.EndDrain: grace
// endings come only from the retirer.
var ErrEndReason = errors.New("retire: EndAll takes only the drain end reason")

// Code returns the RZ code of an end reason: RZ-RT-014 for EndGrace,
// RZ-RT-016 for EndDrain and "" for EndNone.
func Code(r snapshot.EndReason) string {
	switch r {
	case snapshot.EndGrace:
		return CodeGraceEnded
	case snapshot.EndDrain:
		return CodeDrainReached
	default:
		return ""
	}
}

// EndError returns the error the handler reports for a request the ending
// protocol ended (errcode.CodeOf gives Code(r)), or nil for EndNone.
func EndError(r snapshot.EndReason) error {
	c := Code(r)
	if c == "" {
		return nil
	}
	return errcode.Wrap(c, ErrEnded)
}

// reasonName is the log value of an end reason.
func reasonName(r snapshot.EndReason) string {
	switch r {
	case snapshot.EndGrace:
		return "grace"
	case snapshot.EndDrain:
		return "drain"
	default:
		return "none"
	}
}

// pastDeadline is a deadline already in the past on any clock: it resets
// an HTTP/2 stream or times out an HTTP/1.1 connection at once.
func pastDeadline() time.Time { return time.Unix(1, 0) }

// EndAll runs the ending protocol with reason on every live snapshot, the
// active one included: the Drain deadline ends all remaining work with
// RZ-RT-016 (spec 04 req 63). It blocks until every started callback has
// finished and returns how many requests it ended; when ctx ends first it
// stops starting callbacks and returns ctx's error with the count so far.
// It takes snapshot.EndDrain only and returns ErrEndReason for any other
// reason.
//
// The end is sticky: from the first EndAll on, every request that pins
// (connections stay open until Close) is ended inside Pin with RZ-RT-016,
// so no work starts after the Drain deadline. The latch is set before ctx
// is checked, so it holds even when ctx is already done and nothing was
// walked. The supervisor calls EndAll, then Close (package documentation).
func (h *Holder) EndAll(ctx context.Context, reason snapshot.EndReason) (int, error) {
	if reason != snapshot.EndDrain {
		return 0, ErrEndReason
	}
	// Set before the live list is read: a Pin that misses the flag added
	// its record before the walk below locks that stripe, so one of the two
	// ends it.
	h.drained.Store(true)
	h.mu.Lock()
	snaps := make([]*snapshot.Snapshot, 0, len(h.live))
	for _, t := range h.live {
		snaps = append(snaps, t.s)
	}
	h.mu.Unlock()
	n := 0
	for _, s := range snaps {
		n += h.endPinned(ctx, s, reason)
	}
	h.wake()
	h.log.InfoContext(ctx, "pinned requests ended", "ended", n, catalog.KeyReason, reasonName(reason))
	if err := ctx.Err(); err != nil {
		return n, fmt.Errorf("retire: end all: %w", err)
	}
	return n, nil
}

// endPinned is the ending protocol over one snapshot (spec 04 req 53): it
// walks the per-stripe request lists and runs each request's callback on
// its own goroutine, at most MaxEnding at once, and waits for them. It
// counts grace endings in ruralz_snapshot_retirement_ended_total.
//
// A record visited here stays pending, tagged with this walk, until its
// callback claims it; Unpin forgets a pending record and waits for a
// claimed one, so a callback acts only on a request that was pinned when
// claimed and never after its handler returned. The tag makes a claim
// fail when the record was unpinned, reused and visited by another walk
// in between, so a stale callback never ends the new request with this
// walk's reason.
func (h *Holder) endPinned(ctx context.Context, s *snapshot.Snapshot, reason snapshot.EndReason) int {
	h.walking.Add(1)
	defer h.walking.Add(-1)
	walk := h.ws.begin()
	var wg sync.WaitGroup
	var ended atomic.Int64
	s.Pins.Each(func(r *snapshot.PinnedRequest) {
		if ctx.Err() != nil || !h.ws.visit(r, walk) {
			return
		}
		select {
		case h.sem <- struct{}{}:
		case <-ctx.Done():
			h.ws.forget(r, walk)
			return
		}
		wg.Go(func() {
			defer func() { <-h.sem }()
			if h.endOne(r, reason, walk) {
				ended.Add(1)
			}
		})
	})
	wg.Wait()
	n := ended.Load()
	if reason == snapshot.EndGrace && n > 0 {
		h.endedC.Add(0, uint64(n))
	}
	if n > 0 {
		h.log.InfoContext(ctx, "snapshot requests ended", catalog.KeyRevision, s.Revision.Digest.Short(),
			"ended", n, catalog.KeyReason, reasonName(reason))
	}
	return int(n)
}

// endOne is one request's ending callback for walk: under rec.Mu it moves
// the connection deadlines and marks the request ended, then it cancels
// the request's root context. Before commit the read deadline moves to the
// past and the write deadline WriteSlack ahead, so the handler can write
// 503 RZ-RT-014 (RZ-RT-016); after commit both move to the past, which
// resets the HTTP/2 stream or closes the HTTP/1.1 connection. A request
// already ended is left alone.
//
// Spec 04 req 53 lists the cancel first; architecture R-73 (3.3 step 9)
// adopts this order instead. Marking before canceling means a handler woken by
// the cancellation always sees Ended when it commits under rec.Mu (req
// 38), so it writes the 503 with the end code instead of its own
// cancellation error. Both steps run before the callback returns, so the
// client cannot tell the orders apart otherwise.
func (h *Holder) endOne(r *snapshot.PinnedRequest, reason snapshot.EndReason, walk uint64) bool {
	if !h.ws.claim(r, walk) {
		return false
	}
	defer h.ws.done(r)
	cancel, ok := h.mark(r, reason)
	if cancel != nil {
		cancel()
	}
	return ok
}

// endAtPin ends a request that pinned after EndAll, on the request's own
// goroutine inside Pin (no walk can hold rec yet, and a walk that visits
// it later finds it ended).
func (h *Holder) endAtPin(r *snapshot.PinnedRequest) {
	cancel, _ := h.mark(r, snapshot.EndDrain)
	if cancel != nil {
		cancel()
	}
}

// mark sets the deadlines and Ended under r.Mu and returns the request's
// cancel function; a request already ended gets neither again. A panic
// from the connection (a handler that broke the Unpin contract) is logged
// instead of crashing the Node.
func (h *Holder) mark(r *snapshot.PinnedRequest, reason snapshot.EndReason) (cancel context.CancelFunc, ended bool) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	if r.Ended != snapshot.EndNone {
		return nil, false
	}
	cancel = r.Cancel
	defer func() {
		if v := recover(); v != nil {
			h.log.Error("ending callback panicked", catalog.KeyError, fmt.Sprint(v))
			ended = false
		}
	}()
	r.Ended = reason
	if rc := r.RC; rc != nil {
		// Errors mean the writer supports no deadlines (http.ErrNotSupported);
		// the canceled context still ends the request.
		if r.Committed {
			_ = rc.SetReadDeadline(pastDeadline())
			_ = rc.SetWriteDeadline(pastDeadline())
		} else {
			_ = rc.SetReadDeadline(pastDeadline())
			_ = rc.SetWriteDeadline(h.clock.Now().Add(h.writeSlack))
		}
	}
	return cancel, true
}

// walkSet tracks the records ending walks visited: pending, tagged with
// the visiting walk, until that walk's callback claims them, then active
// while it runs. cond signals the end of an active callback.
type walkSet struct {
	mu      sync.Mutex
	cond    sync.Cond
	seq     uint64
	pending map[*snapshot.PinnedRequest]uint64
	active  map[*snapshot.PinnedRequest]struct{}
}

// begin returns a new walk's tag (never 0).
func (w *walkSet) begin() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	return w.seq
}

// visit records r as pending for walk; false when a walk already holds it.
func (w *walkSet) visit(r *snapshot.PinnedRequest, walk uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.pending[r]; ok {
		return false
	}
	if _, ok := w.active[r]; ok {
		return false
	}
	if w.pending == nil {
		w.pending = map[*snapshot.PinnedRequest]uint64{}
	}
	w.pending[r] = walk
	return true
}

// forget drops r when it is still pending for walk, whose callback was
// never started.
func (w *walkSet) forget(r *snapshot.PinnedRequest, walk uint64) {
	w.mu.Lock()
	if w.pending[r] == walk {
		delete(w.pending, r)
	}
	w.mu.Unlock()
}

// claim moves r, pending for walk, to active; false when Unpin forgot it
// or another walk visited it since.
func (w *walkSet) claim(r *snapshot.PinnedRequest, walk uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if v, ok := w.pending[r]; !ok || v != walk {
		return false
	}
	delete(w.pending, r)
	if w.active == nil {
		w.active = map[*snapshot.PinnedRequest]struct{}{}
	}
	w.active[r] = struct{}{}
	return true
}

// done ends r's active callback.
func (w *walkSet) done(r *snapshot.PinnedRequest) {
	w.mu.Lock()
	delete(w.active, r)
	w.cond.Broadcast()
	w.mu.Unlock()
}

// detach is Unpin's side: a pending r is forgotten, an active one waited
// for.
func (w *walkSet) detach(r *snapshot.PinnedRequest) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.pending, r)
	for {
		if _, ok := w.active[r]; !ok {
			return
		}
		w.cond.Wait()
	}
}
