// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package retire

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Tests for spec 04 req 53 (ending protocol: records registered at pin
// time, one bounded goroutine per request at grace end, root context
// canceled, deadlines past or 5 s ahead under the per-request mutex,
// marked for 503 RZ-RT-014) and req 63 (the Drain deadline reuses it with
// RZ-RT-016); req 38 (commit under the mutex); test plan item 12 (ending
// before commit is 503 RZ-RT-014, after commit the connection closes or
// the stream resets, observed by an httptest client).

// toEnding publishes two more snapshots (b, c) over the pinned snapshot a
// with K = 1 and advances past grace, so a becomes ending.
func toEnding(t *testing.T, x *harness) (cleanup func()) {
	t.Helper()
	b, c := newSnap("b-"+t.Name(), nil), newSnap("c-"+t.Name(), nil)
	x.publish(b)
	pb := x.pin(1)
	x.publish(c)
	x.clock.Advance(DefaultGrace)
	return func() { x.unpin(pb) }
}

// TestReq53UncommittedDeadlines: before commit the read deadline moves to
// the past, the write deadline 5 s ahead, Ended is set and the root
// context canceled once.
func TestReq53UncommittedDeadlines(t *testing.T) {
	x := newHarness(t, Config{K: 1}, true)
	a := newSnap("a", nil)
	x.publish(a)
	w := &deadlineWriter{}
	var cancels atomic.Int32
	rec := &snapshot.PinnedRequest{Cancel: func() { cancels.Add(1) }, RC: http.NewResponseController(w)}
	s := x.h.Pin(0, rec)
	cleanup := toEnding(t, x)
	defer cleanup()
	waitFor(t, "cancel", func() bool { return cancels.Load() == 1 })
	if ended(rec) != snapshot.EndGrace {
		t.Fatal("request not marked EndGrace")
	}
	read, write := w.deadlines()
	now := x.clock.Now()
	if len(read) != 1 || !read[0].Before(now) || !read[0].Before(time.Now()) {
		t.Fatalf("read deadlines = %v, want one in the past", read)
	}
	if len(write) != 1 || !write[0].Equal(now.Add(DefaultWriteSlack)) {
		t.Fatalf("write deadlines = %v, want now+5s (%v)", write, now.Add(DefaultWriteSlack))
	}
	x.h.Unpin(s, 0, rec)
	x.waitState(a, freed)
	if cancels.Load() != 1 {
		t.Fatalf("cancel called %d times, want 1", cancels.Load())
	}
}

// TestReq53CommittedDeadlines: after commit both deadlines move to the
// past (stream reset or connection closed).
func TestReq53CommittedDeadlines(t *testing.T) {
	x := newHarness(t, Config{K: 1}, true)
	x.publish(newSnap("a", nil))
	w := &deadlineWriter{}
	rec := &snapshot.PinnedRequest{Cancel: func() {}, RC: http.NewResponseController(w), Committed: true}
	s := x.h.Pin(0, rec)
	cleanup := toEnding(t, x)
	defer cleanup()
	waitFor(t, "ended", func() bool { return ended(rec) == snapshot.EndGrace })
	read, write := w.deadlines()
	if len(read) != 1 || len(write) != 1 || !read[0].Before(time.Now()) || !write[0].Before(time.Now()) {
		t.Fatalf("deadlines = %v / %v, want both in the past", read, write)
	}
	x.h.Unpin(s, 0, rec)
}

// TestReq53EndingWithoutController ends a request that has no
// ResponseController (a synthetic replay exchange): it is marked and
// canceled; a writer without deadline support is tolerated.
func TestReq53EndingWithoutController(t *testing.T) {
	x := newHarness(t, Config{K: 1}, true)
	x.publish(newSnap("a", nil))
	plain := x.pin(0)
	w := &deadlineWriter{notSupported: true}
	rec := &snapshot.PinnedRequest{RC: http.NewResponseController(w)}
	s := x.h.Pin(1, rec)
	cleanup := toEnding(t, x)
	defer cleanup()
	waitFor(t, "both ended", func() bool {
		return ended(plain.rec) == snapshot.EndGrace && ended(rec) == snapshot.EndGrace
	})
	if plain.cancels.Load() != 1 {
		t.Fatal("root context not canceled")
	}
	waitFor(t, "ended counter", func() bool { return x.ended.n.Load() == 2 })
	x.unpin(plain)
	x.h.Unpin(s, 1, rec)
}

// TestReq53PanickingControllerIsContained logs a panic from a connection
// whose handler broke the Unpin contract instead of crashing the Node.
func TestReq53PanickingControllerIsContained(t *testing.T) {
	logs := &logRecords{}
	x := newHarness(t, Config{K: 1, Logger: slog.New(logs)}, true)
	x.publish(newSnap("a", nil))
	var cancels atomic.Int32
	rec := &snapshot.PinnedRequest{Cancel: func() { cancels.Add(1) }, RC: http.NewResponseController(&deadlineWriter{panicOnRead: true})}
	s := x.h.Pin(0, rec)
	cleanup := toEnding(t, x)
	defer cleanup()
	waitFor(t, "cancel after panic", func() bool { return cancels.Load() == 1 })
	waitFor(t, "panic logged", func() bool { return logs.has("ending callback panicked") })
	x.h.Unpin(s, 0, rec)
}

// TestReq63EndAllDrain ends every pinned request of every live snapshot,
// the active one included, with RZ-RT-016; the grace counter is untouched
// and a second EndAll ends nothing new.
func TestReq63EndAllDrain(t *testing.T) {
	x := newHarness(t, Config{}, true)
	x.publish(newSnap("a", nil))
	pa := x.pin(0)
	x.publish(newSnap("b", nil))
	pb1, pb2 := x.pin(1), x.pin(2)
	n, err := x.h.EndAll(context.Background(), snapshot.EndDrain)
	if err != nil || n != 3 {
		t.Fatalf("EndAll = %d, %v; want 3, nil", n, err)
	}
	for _, p := range []pinned{pa, pb1, pb2} {
		if ended(p.rec) != snapshot.EndDrain || p.cancels.Load() != 1 {
			t.Fatalf("request not ended with EndDrain (ended %d, cancels %d)", ended(p.rec), p.cancels.Load())
		}
	}
	if x.ended.n.Load() != 0 {
		t.Fatal("a Drain ending counted as a retirement ending")
	}
	if n, err := x.h.EndAll(context.Background(), snapshot.EndDrain); err != nil || n != 0 {
		t.Fatalf("repeated EndAll = %d, %v; want 0, nil", n, err)
	}
	for _, p := range []pinned{pa, pb1, pb2} {
		if ended(p.rec) != snapshot.EndDrain || p.cancels.Load() != 1 {
			t.Fatal("a repeated EndAll changed or re-canceled an ended request")
		}
		x.unpin(p)
	}
}

func TestReq63EndAllErrors(t *testing.T) {
	x := newHarness(t, Config{}, false)
	if _, err := x.h.EndAll(context.Background(), snapshot.EndNone); !errors.Is(err, ErrEndReason) {
		t.Fatalf("EndAll(EndNone) = %v, want ErrEndReason", err)
	}
	if _, err := x.h.EndAll(context.Background(), snapshot.EndGrace); !errors.Is(err, ErrEndReason) {
		t.Fatalf("EndAll(EndGrace) = %v, want ErrEndReason", err)
	}
	x.publish(newSnap("a", nil))
	p := x.pin(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := x.h.EndAll(ctx, snapshot.EndDrain)
	if !errors.Is(err, context.Canceled) || n != 0 {
		t.Fatalf("EndAll with a canceled context = %d, %v; want 0, Canceled", n, err)
	}
	if ended(p.rec) != snapshot.EndNone {
		t.Fatal("a canceled EndAll ended a request")
	}
	// The latch holds although nothing was walked: a request that pins now
	// is ended at once.
	q := x.pin(1)
	if ended(q.rec) != snapshot.EndDrain || q.cancels.Load() != 1 {
		t.Fatalf("pin after a canceled EndAll: ended %d, cancels %d; want EndDrain once", ended(q.rec), q.cancels.Load())
	}
	x.unpin(q)
	x.unpin(p)
}

// TestReq53EndingGoroutinesAreBounded holds every request mutex so the
// callbacks block: no more than MaxEnding run at once, and all complete
// once the mutexes are released.
func TestReq53EndingGoroutinesAreBounded(t *testing.T) {
	const maxEnding, requests = 2, 10
	x := newHarness(t, Config{MaxEnding: maxEnding}, false)
	x.publish(newSnap("a", nil))
	var ps []pinned
	for i := range requests {
		p := x.pin(emit.Stripe(i))
		p.rec.Mu.Lock()
		ps = append(ps, p)
	}
	done := make(chan int, 1)
	go func() {
		n, _ := x.h.EndAll(context.Background(), snapshot.EndDrain)
		done <- n
	}()
	activeCount := func() int {
		x.h.ws.mu.Lock()
		defer x.h.ws.mu.Unlock()
		return len(x.h.ws.active)
	}
	waitFor(t, "callbacks blocked", func() bool { return activeCount() == maxEnding })
	time.Sleep(10 * time.Millisecond)
	if n := activeCount(); n != maxEnding {
		t.Fatalf("%d callbacks active, want at most %d", n, maxEnding)
	}
	for _, p := range ps {
		p.rec.Mu.Unlock()
	}
	select {
	case n := <-done:
		if n != requests {
			t.Fatalf("EndAll ended %d, want %d", n, requests)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("EndAll did not finish")
	}
	for _, p := range ps {
		x.unpin(p)
	}
}

// TestReq53UnpinWaitsForActiveCallback: a handler that unpins while its
// callback runs waits for it, so the callback never touches the record
// or the connection after the handler returned.
func TestReq53UnpinWaitsForActiveCallback(t *testing.T) {
	x := newHarness(t, Config{}, false)
	x.publish(newSnap("a", nil))
	p := x.pin(0)
	p.rec.Mu.Lock() // the handler is mid-commit
	endDone := make(chan struct{})
	go func() {
		defer close(endDone)
		_, _ = x.h.EndAll(context.Background(), snapshot.EndDrain)
	}()
	waitFor(t, "callback claimed", func() bool {
		x.h.ws.mu.Lock()
		defer x.h.ws.mu.Unlock()
		_, ok := x.h.ws.active[p.rec]
		return ok
	})
	unpinned := make(chan struct{})
	go func() {
		x.unpin(p)
		close(unpinned)
	}()
	select {
	case <-unpinned:
		t.Fatal("Unpin returned while the request's callback was running")
	case <-time.After(20 * time.Millisecond):
	}
	p.rec.Committed = true
	p.rec.Mu.Unlock() // commit done; the callback proceeds
	select {
	case <-unpinned:
	case <-time.After(10 * time.Second):
		t.Fatal("Unpin did not return after the callback finished")
	}
	if ended(p.rec) != snapshot.EndDrain {
		t.Fatal("Unpin returned before the callback marked the request")
	}
	<-endDone
}

// TestReq53UnpinForgetsPendingRecord: a record Unpin detached before its
// callback claimed it is left alone.
func TestReq53UnpinForgetsPendingRecord(t *testing.T) {
	var w walkSet
	w.cond.L = &w.mu
	r := &snapshot.PinnedRequest{}
	walk := w.begin()
	if !w.visit(r, walk) {
		t.Fatal("first visit refused")
	}
	if w.visit(r, w.begin()) {
		t.Fatal("second visit of a pending record accepted")
	}
	w.detach(r)
	if w.claim(r, walk) {
		t.Fatal("claim after detach succeeded")
	}
	if !w.visit(r, walk) || !w.claim(r, walk) {
		t.Fatal("visit and claim after detach failed")
	}
	if w.visit(r, w.begin()) {
		t.Fatal("visit of an active record accepted")
	}
	w.done(r)
	w.detach(r)
	if !w.visit(r, walk) {
		t.Fatal("visit after done refused")
	}
	w.forget(r, walk)
	if w.claim(r, walk) {
		t.Fatal("claim after forget succeeded")
	}
}

// TestReq53StaleClaimOfAReusedRecord drives the overlap of a grace walk and
// the Drain walk on one pooled record: walk 1 visits r, the handler
// unpins r (detach) before walk 1's callback claims it, r is reused and
// visited by walk 2. Walk 1's stale callback must not end the new request
// with RZ-RT-014; walk 2's callback ends it with RZ-RT-016.
func TestReq53StaleClaimOfAReusedRecord(t *testing.T) {
	x := newHarness(t, Config{}, false)
	var cancels atomic.Int32
	r := &snapshot.PinnedRequest{Cancel: func() { cancels.Add(1) }}
	w := &x.h.ws
	grace, drain := w.begin(), w.begin()
	if grace == drain || grace == 0 {
		t.Fatalf("walk tags %d and %d, want distinct and non-zero", grace, drain)
	}
	if !w.visit(r, grace) {
		t.Fatal("grace visit refused")
	}
	w.detach(r) // Unpin before the grace callback ran
	if !w.visit(r, drain) {
		t.Fatal("drain visit of the reused record refused")
	}
	w.forget(r, grace) // a stale forget leaves the drain walk's entry
	if x.h.endOne(r, snapshot.EndGrace, grace) {
		t.Fatal("the stale grace callback ended the reused request")
	}
	if ended(r) != snapshot.EndNone || cancels.Load() != 0 {
		t.Fatal("the stale grace callback touched the reused request")
	}
	if !x.h.endOne(r, snapshot.EndDrain, drain) {
		t.Fatal("the drain callback did not end the request")
	}
	if ended(r) != snapshot.EndDrain || cancels.Load() != 1 {
		t.Fatalf("request ended %d with %d cancels, want EndDrain once", ended(r), cancels.Load())
	}
	if x.ended.n.Load() != 0 {
		t.Fatal("a Drain ending counted as a retirement ending")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) != 0 || len(w.active) != 0 {
		t.Fatalf("walk set keeps %d pending and %d active records", len(w.pending), len(w.active))
	}
}

// TestReq63PinAfterEndAllIsEnded: once EndAll ran, a request that pins on
// a connection that is still open is ended inside Pin with EndDrain
// (deadlines moved, context canceled), so Close does not wait for new
// work.
func TestReq63PinAfterEndAllIsEnded(t *testing.T) {
	x := newHarness(t, Config{}, true)
	x.publish(newSnap("a", nil))
	if n, err := x.h.EndAll(context.Background(), snapshot.EndDrain); err != nil || n != 0 {
		t.Fatalf("EndAll = %d, %v; want 0, nil", n, err)
	}
	if n, err := x.h.EndAll(context.Background(), snapshot.EndDrain); err != nil || n != 0 {
		t.Fatalf("repeated EndAll = %d, %v; want 0, nil", n, err)
	}
	w := &deadlineWriter{}
	var cancels atomic.Int32
	rec := &snapshot.PinnedRequest{Cancel: func() { cancels.Add(1) }, RC: http.NewResponseController(w)}
	s := x.h.Pin(2, rec)
	if s == nil {
		t.Fatal("Pin after EndAll returned nil")
	}
	if ended(rec) != snapshot.EndDrain || cancels.Load() != 1 {
		t.Fatalf("pin after EndAll: ended %d, cancels %d; want EndDrain once", ended(rec), cancels.Load())
	}
	read, write := w.deadlines()
	if len(read) != 1 || len(write) != 1 || !read[0].Before(time.Now()) {
		t.Fatalf("deadlines = %v / %v, want the uncommitted ending deadlines", read, write)
	}
	if x.ended.n.Load() != 0 {
		t.Fatal("a Drain ending counted as a retirement ending")
	}
	closed := make(chan error, 1)
	go func() { closed <- x.h.Close(context.Background()) }()
	x.h.Unpin(s, 2, rec)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the ended request unpinned")
	}
}

// TestReq63EndAllRejectsGrace: grace endings come only from the retirer,
// so EndAll(EndGrace) is refused and sets no latch: a request pinned
// afterwards is not ended and nothing is counted in
// ruralz_snapshot_retirement_ended_total.
func TestReq63EndAllRejectsGrace(t *testing.T) {
	x := newHarness(t, Config{}, false)
	x.publish(newSnap("a", nil))
	if n, err := x.h.EndAll(context.Background(), snapshot.EndGrace); !errors.Is(err, ErrEndReason) || n != 0 {
		t.Fatalf("EndAll(EndGrace) = %d, %v; want 0, ErrEndReason", n, err)
	}
	p := x.pin(1)
	if ended(p.rec) != snapshot.EndNone || p.cancels.Load() != 0 || x.ended.n.Load() != 0 {
		t.Fatalf("ended %d, cancels %d, counter %d; want EndNone, 0, 0", ended(p.rec), p.cancels.Load(), x.ended.n.Load())
	}
	x.unpin(p)
}

func TestReq53Codes(t *testing.T) {
	tests := []struct {
		r    snapshot.EndReason
		code string
		name string
	}{
		{snapshot.EndNone, "", "none"},
		{snapshot.EndGrace, "RZ-RT-014", "grace"},
		{snapshot.EndDrain, "RZ-RT-016", "drain"},
	}
	for _, tt := range tests {
		if got := Code(tt.r); got != tt.code {
			t.Errorf("Code(%d) = %q, want %q", tt.r, got, tt.code)
		}
		if got := reasonName(tt.r); got != tt.name {
			t.Errorf("reasonName(%d) = %q, want %q", tt.r, got, tt.name)
		}
		err := EndError(tt.r)
		if tt.code == "" {
			if err != nil {
				t.Errorf("EndError(%d) = %v, want nil", tt.r, err)
			}
			continue
		}
		if c, ok := errcode.CodeOf(err); !ok || c != tt.code || !errors.Is(err, ErrEnded) {
			t.Errorf("EndError(%d) = %v, want code %s wrapping ErrEnded", tt.r, err, tt.code)
		}
		if _, ok := errcode.Lookup(tt.code); !ok {
			t.Errorf("%s is not registered", tt.code)
		}
	}
}

// endingServer serves a handler that pins like the request handler: it
// registers a record, then either blocks before commit (/wait) or commits
// a partial streamed response first (/committed), and commits under the
// record's mutex, writing 503 with the end code when the request was
// ended (spec 04 req 38).
type endingServer struct {
	h      *Holder
	pinned chan struct{}
}

func (e *endingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	rec := &snapshot.PinnedRequest{Cancel: cancel, RC: http.NewResponseController(w)}
	s := e.h.Pin(0, rec)
	if s == nil {
		http.Error(w, "no snapshot", http.StatusServiceUnavailable)
		return
	}
	defer e.h.Unpin(s, 0, rec)
	if r.URL.Path == "/committed" {
		rec.Mu.Lock()
		rec.Committed = true
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "partial")
		_ = rec.RC.Flush()
		rec.Mu.Unlock()
		e.pinned <- struct{}{}
		<-ctx.Done()
		_, _ = io.WriteString(w, strings.Repeat("x", 64<<10))
		return
	}
	e.pinned <- struct{}{}
	<-ctx.Done()
	rec.Mu.Lock()
	defer rec.Mu.Unlock()
	rec.Committed = true
	if code := Code(rec.Ended); code != "" {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, code)
		return
	}
	w.WriteHeader(http.StatusInternalServerError)
}

// TestReq53EndingOverHTTP runs the ending protocol against real HTTP/1.1
// and HTTP/2 connections: before commit the client gets 503 RZ-RT-014,
// after commit the connection closes (HTTP/1.1) or the stream resets
// (HTTP/2) and the body read fails.
func TestReq53EndingOverHTTP(t *testing.T) {
	for _, proto := range []string{"http1", "http2"} {
		for _, path := range []string{"/wait", "/committed"} {
			t.Run(proto+path, func(t *testing.T) {
				// Deadlines are absolute times net/http compares with the
				// wall clock, so the fake clock starts now.
				fake := clocktest.New(time.Now())
				x := newHarness(t, Config{K: 1, Clock: fake}, true)
				x.clock = fake
				srv := &endingServer{h: x.h, pinned: make(chan struct{}, 1)}
				ts := httptest.NewUnstartedServer(srv)
				if proto == "http2" {
					ts.EnableHTTP2 = true
					ts.StartTLS()
				} else {
					ts.Start()
				}
				defer ts.Close()
				client := ts.Client()

				x.publish(newSnap("a", nil))
				type result struct {
					status  int
					body    string
					readErr error
					proto   int
				}
				res := make(chan result, 1)
				go func() {
					req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+path, nil)
					resp, err := client.Do(req)
					if err != nil {
						res <- result{readErr: err}
						return
					}
					defer func() { _ = resp.Body.Close() }()
					b, rerr := io.ReadAll(resp.Body)
					res <- result{status: resp.StatusCode, body: string(b), readErr: rerr, proto: resp.ProtoMajor}
				}()
				select {
				case <-srv.pinned:
				case <-time.After(10 * time.Second):
					t.Fatal("request never pinned")
				}
				cleanup := toEnding(t, x)
				defer cleanup()

				var got result
				select {
				case got = <-res:
				case <-time.After(10 * time.Second):
					t.Fatal("client never finished")
				}
				wantProto := 1
				if proto == "http2" {
					wantProto = 2
				}
				if path == "/wait" {
					if got.readErr != nil || got.status != http.StatusServiceUnavailable || got.body != "RZ-RT-014" {
						t.Fatalf("uncommitted ending = %d %q (%v), want 503 RZ-RT-014", got.status, got.body, got.readErr)
					}
					if got.proto != wantProto {
						t.Fatalf("protocol = HTTP/%d, want HTTP/%d", got.proto, wantProto)
					}
				} else if got.status != http.StatusOK || got.readErr == nil {
					t.Fatalf("committed ending = %d, read error %v; want 200 and a failed body read", got.status, got.readErr)
				}
				waitFor(t, "ended counter", func() bool { return x.ended.n.Load() == 1 })
			})
		}
	}
}

// TestReq53PinAfterEndingKeepsWorking: requests keep pinning the active
// snapshot while an ending walk runs on a retired one.
func TestReq53PinAfterEndingKeepsWorking(t *testing.T) {
	x := newHarness(t, Config{K: 1}, true)
	x.publish(newSnap("a", nil))
	held := x.pin(0)
	held.rec.Mu.Lock() // keep the walk running
	cleanup := toEnding(t, x)
	defer cleanup()
	waitFor(t, "walk running", func() bool { return x.h.walking.Load() != 0 })
	var wg sync.WaitGroup
	for i := range emit.Stripe(8) {
		wg.Go(func() {
			for range 100 {
				p := x.pin(i)
				if ended(p.rec) != snapshot.EndNone {
					t.Error("a request on the active snapshot was ended")
				}
				x.unpin(p)
			}
		})
	}
	wg.Wait()
	held.rec.Mu.Unlock()
	waitFor(t, "held ended", func() bool { return ended(held.rec) == snapshot.EndGrace })
	x.unpin(held)
}
