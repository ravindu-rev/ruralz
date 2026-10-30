// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package revalidate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Lease modes, shortened.
const (
	take    = statestore.LeaseTake
	refresh = statestore.LeaseRefresh
	endW    = statestore.LeaseEndStale
)

// wantModes fails the test unless the lease writes so far had modes want.
func wantModes(t *testing.T, s *testStore, want ...statestore.LeaseMode) {
	t.Helper()
	if got := s.modes(); !slices.Equal(got, want) {
		t.Errorf("lease writes %v, want %v", got, want)
	}
}

// wantEntry fails the test unless k's variant holds entry (found) or is
// gone (entry "").
func wantEntry(h *harness, k statestore.CacheKey, entry string) {
	h.t.Helper()
	got, found := h.lookup(k)
	switch {
	case entry == "" && found:
		h.t.Errorf("variant still stored (%q), want the stale window ended", got)
	case entry != "" && (!found || got != entry):
		h.t.Errorf("variant = %q, found %v; want %q", got, found, entry)
	}
}

// TestRefreshOn304 checks the revalidation of "Response caching": the
// worker takes the partition's lease, replays the saved request with the
// stored validators to the entry's Route and Upstream, and a 304 refreshes
// the entry under the lease, with the freshened header and a TTL of the
// new lifetime plus the larger stale window minus the age.
func TestRefreshOn304(t *testing.T) {
	h := newHarness(t)
	k := testKey(1)
	h.seed(k)
	e := entry(staleHeader())
	var received time.Time
	h.rp.setReply(func(context.Context, *http.Request) (*http.Response, error) {
		h.clk.Advance(time.Second) // the replay's round trip
		received = h.clk.Now()
		r, _ := respond(http.StatusNotModified, http.Header{
			"Etag": {`"v1"`}, "Cache-Control": {"max-age=120, stale-while-revalidate=30"}, "Date": {date(received)},
		})
		return r, nil
	})
	h.run()
	start := h.clk.Now()
	h.enqueue(k, e)
	if o := h.next(); o != OutcomeRefreshed {
		t.Fatalf("outcome %v, want refreshed", o)
	}

	calls := h.rp.replays()
	if len(calls) != 1 {
		t.Fatalf("%d replays, want 1", len(calls))
	}
	c := calls[0]
	wantHeader := http.Header{
		"Accept":            {"application/json"},
		"If-None-Match":     {`"v1"`},
		"If-Modified-Since": {date(t0.Add(-time.Hour))},
	}
	if c.route != testRoute || c.upstream != testUpstream || c.method != http.MethodGet || c.url != testURL || !equalHeader(c.header, wantHeader) {
		t.Errorf("replay %+v, want GET %s on %s/%s with %v", c, testURL, testRoute, testUpstream, wantHeader)
	}

	r := e.lastRefresh()
	merged := staleHeader()
	merged["Cache-Control"] = []string{"max-age=120, stale-while-revalidate=30"}
	merged["Date"] = []string{date(received)}
	wantFresh := Freshness{Lifetime: sec(120), Explicit: true, StaleWhileRevalidate: sec(30)}
	switch {
	case r == nil:
		t.Fatal("Refresh was not called")
	case !equalHeader(r.Header, merged):
		t.Errorf("refreshed header %v, want %v", r.Header, merged)
	case r.Freshness != wantFresh:
		t.Errorf("refreshed freshness %+v, want %+v", r.Freshness, wantFresh)
	case !r.Requested.Equal(start) || !r.Received.Equal(start.Add(time.Second)):
		t.Errorf("requested %v, received %v; want %v and 1 s later", r.Requested, r.Received, start)
	case r.InitialAge != time.Second:
		t.Errorf("initial age %v, want the 1 s response delay", r.InitialAge)
	}

	// The refresh names the entry's generation token and the 304's
	// receipt, from which the store time's receipt delay is measured.
	const body = "refreshed max-age=120, stale-while-revalidate=30"
	want := []recorded{
		{mode: take, token: 1, timeout: policyTimeout, applied: true},
		{
			mode: refresh, token: 1, generation: fixtureToken(), ttl: sec(149), timeout: policyTimeout,
			received: start.Add(time.Second), entry: body, applied: true,
		},
	}
	if got := h.store.recordedWrites(); !slices.Equal(got, want) {
		t.Errorf("lease writes %+v, want %+v", got, want)
	}
	wantEntry(h, k, body)
	if s := h.rv.Stats(); s.Queued != 1 || s.Outcomes[OutcomeRefreshed] != 1 {
		t.Errorf("Stats() = %+v", s)
	}
}

// TestReplayCarriesOnlyStoredValidators checks that the request's own
// preconditions and Range never reach the replay (RFC 9111 section
// 4.3.1), and that a stored response without an entity tag or
// Last-Modified is validated by its Date.
func TestReplayCarriesOnlyStoredValidators(t *testing.T) {
	h := newHarness(t)
	k := testKey(1)
	h.seed(k)
	e := entry(http.Header{"Cache-Control": {"max-age=60, stale-while-revalidate=30"}, "Date": {date(t0)}})
	e.reqHeader = http.Header{
		"Accept": {"*/*"}, "If-None-Match": {`"client"`}, "If-Match": {`"x"`}, "Range": {"bytes=0-9"},
		"If-Unmodified-Since": {date(t0)}, "If-Range": {`"x"`},
	}
	h.run()
	h.enqueue(k, e)
	h.next()
	calls := h.rp.replays()
	want := http.Header{"Accept": {"*/*"}, "If-Modified-Since": {date(t0)}}
	if len(calls) != 1 || !equalHeader(calls[0].header, want) {
		t.Errorf("replays %+v, want one with header %v", calls, want)
	}
}

// TestOneNodeRevalidates checks the lease: while one Node's revalidation
// holds the partition's lease, another Node's job for it does not replay.
func TestOneNodeRevalidates(t *testing.T) {
	h := newHarness(t)
	k := testKey(1)
	h.seed(k)
	g := newGate(t)
	h.rp.setReply(func(ctx context.Context, _ *http.Request) (*http.Response, error) {
		select {
		case <-g.ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		r, _ := respond(http.StatusNotModified, http.Header{"Etag": {`"v1"`}})
		return r, nil
	})
	h.run()
	h.enqueue(k, entry(staleHeader()))
	h.awaitReplay()

	other := newRevalidator(h)
	runOther(t, other)
	if !other.Enqueue(h.job(k, entry(staleHeader()))) {
		t.Fatal("the other Node refused the job")
	}
	if o := h.next(); o != OutcomeLeaseHeld {
		t.Fatalf("other Node's outcome %v, want lease_held", o)
	}
	g.open()
	if o := h.next(); o != OutcomeRefreshed {
		t.Fatalf("outcome %v, want refreshed", o)
	}
	if n := len(h.rp.replays()); n != 1 {
		t.Errorf("%d replays, want 1", n)
	}
}

// runOther runs rv until the test ends.
func runOther(t *testing.T, rv *Revalidator) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("other Node's Run = %v", err)
			}
		case <-time.After(waitLimit):
			t.Error("Run did not return")
		}
	})
}

// TestReplaceOnFullResponse checks that a full response below 500 that
// the request-path store rules accept replaces the entry under the lease,
// as the leg delivered it: its status, header and body, freshness and age
// go to Entry.Replace, the write names the entry's generation token, the
// replay's receipt and the new TTL, and the body is closed.
func TestReplaceOnFullResponse(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusMovedPermanently, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newHarness(t)
			k := testKey(1)
			h.seed(k)
			var b *body
			var requested, received time.Time
			hdr := http.Header{"Etag": {`"v2"`}, "Cache-Control": {"max-age=60, stale-while-revalidate=30"}}
			h.rp.setReply(func(context.Context, *http.Request) (*http.Response, error) {
				requested = h.clk.Now()
				h.clk.Advance(2 * time.Second) // the replay's round trip
				received = h.clk.Now()
				rh := hdr.Clone()
				rh["Date"] = []string{date(received)}
				b = &body{Reader: strings.NewReader("new content")}
				return &http.Response{StatusCode: status, Header: rh, Body: b}, nil
			})
			h.run()
			e := entry(staleHeader())
			h.enqueue(k, e)
			if o := h.next(); o != OutcomeReplaced {
				t.Fatalf("outcome %v, want replaced", o)
			}
			r := e.lastReplace()
			wantFresh := Freshness{Lifetime: sec(60), Explicit: true, StaleWhileRevalidate: sec(30)}
			switch {
			case r == nil:
				t.Fatal("Replace was not called")
			case r.Status != status || string(r.Body) != "new content" || r.Header.Get("Etag") != `"v2"`:
				t.Errorf("replacement %d %v %q", r.Status, r.Header, r.Body)
			case r.Freshness != wantFresh:
				t.Errorf("replacement freshness %+v, want %+v", r.Freshness, wantFresh)
			case !r.Requested.Equal(requested) || !r.Received.Equal(received) || r.InitialAge != 2*time.Second:
				t.Errorf("requested %v, received %v, initial age %v; want %v, %v and the 2 s delay",
					r.Requested, r.Received, r.InitialAge, requested, received)
			}
			replaced := "replaced " + strconv.Itoa(status) + " new content"
			want := []recorded{
				{mode: take, token: 1, timeout: policyTimeout, applied: true},
				{
					mode: refresh, token: 1, generation: fixtureToken(), ttl: sec(88), timeout: policyTimeout,
					received: received, entry: replaced, applied: true,
				},
			}
			if got := h.store.recordedWrites(); !slices.Equal(got, want) {
				t.Errorf("lease writes %+v, want %+v", got, want)
			}
			wantEntry(h, k, replaced)
			if n := b.closed.Load(); n != 1 {
				t.Errorf("body closed %d times, want 1", n)
			}
			if n := h.store.admitted.Load(); n != int64(len(replaced)) {
				t.Errorf("memory rules saw %d bytes, want the %d of the entry", n, len(replaced))
			}
		})
	}
}

// TestEndStaleOnRefusedFullResponse checks that a full response below 500
// the request-path store rules refuse ends the stale window unstored: an
// unstorable status or header, a partial composition response, other Vary
// names, no TTL left, an entry that cannot be encoded, the size cap or
// the memory rules, the last two counting in
// ruralz_cache_store_skipped_total.
func TestEndStaleOnRefusedFullResponse(t *testing.T) {
	storable := http.Header{"Cache-Control": {"max-age=60"}}
	tests := []struct {
		name   string
		status int
		hdr    http.Header
		body   string
		setup  func(h *harness, e *testEntry)
		skip   int // emit.Skip* reason counted, or -1
	}{
		{name: "partial content", status: http.StatusPartialContent, hdr: storable, skip: -1},
		{name: "no explicit freshness", status: http.StatusOK, hdr: http.Header{"Etag": {`"v2"`}}, skip: -1},
		{name: "no-store", status: http.StatusOK, hdr: http.Header{"Cache-Control": {"no-store, max-age=60"}}, skip: -1},
		{name: "Set-Cookie", status: http.StatusOK, hdr: http.Header{"Cache-Control": {"max-age=60"}, "Set-Cookie": {"a=1"}}, skip: -1},
		{name: "partial composition response", status: http.StatusOK, hdr: http.Header{"Cache-Control": {"max-age=60"}, "Ruralz-Partial": {"true"}}, skip: -1},
		{
			name: "Authorization without permission", status: http.StatusOK, hdr: storable, skip: -1,
			setup: func(_ *harness, e *testEntry) { e.authorized = true },
		},
		{
			name: "Vary names change", status: http.StatusOK, skip: -1,
			hdr:   http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Language"}},
			setup: func(_ *harness, e *testEntry) { e.header["Vary"] = []string{"Accept-Encoding"} },
		},
		{name: "no TTL left", status: http.StatusOK, hdr: http.Header{"Cache-Control": {"max-age=0"}}, skip: -1},
		{
			name: "entry encoding fails", status: http.StatusOK, hdr: storable, skip: -1,
			setup: func(_ *harness, e *testEntry) { e.replaceErr = errors.New("bad header") },
		},
		{name: "body past the size cap", status: http.StatusOK, hdr: storable, body: strings.Repeat("x", MaxEntry+1), skip: emit.SkipSizeLimit},
		{
			name: "entry past the size cap", status: http.StatusOK, hdr: storable, skip: emit.SkipSizeLimit,
			setup: func(_ *harness, e *testEntry) { e.replaceLen = MaxEntry + 1 },
		},
		{
			name: "memory rules", status: http.StatusOK, hdr: storable, skip: emit.SkipMemory,
			setup: func(h *harness, _ *testEntry) { h.store.refuseBytes.Store(true) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			k := testKey(1)
			h.seed(k)
			e := entry(staleHeader())
			if tt.setup != nil {
				tt.setup(h, e)
			}
			h.replyFull(tt.status, tt.hdr, tt.body)
			var skipped [emit.NumSkipReasons]emit.Counter
			cs := make([]*counter, emit.NumSkipReasons)
			for i := range skipped {
				cs[i] = &counter{}
				skipped[i] = cs[i]
			}
			j := h.job(k, e)
			j.StoreSkipped = &skipped
			h.run()
			if !h.rv.Enqueue(j) {
				t.Fatal("Enqueue refused the job")
			}
			if o := h.next(); o != OutcomeEnded {
				t.Fatalf("outcome %v, want ended", o)
			}
			wantModes(t, h.store, take, endW)
			wantEntry(h, k, "")
			for i, c := range cs {
				want := uint64(0)
				if i == tt.skip {
					want = 1
				}
				if n := c.n.Load(); n != want {
					t.Errorf("store skips for reason %d = %d, want %d", i, n, want)
				}
			}
		})
	}
}

// TestUnreadableReplacementKeepsEntry checks that a replacing body that
// fails mid-read is no response: the entry stays for stale-if-error.
func TestUnreadableReplacementKeepsEntry(t *testing.T) {
	h := newHarness(t)
	k := testKey(1)
	h.seed(k)
	h.rp.setReply(func(context.Context, *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Cache-Control": {"max-age=60"}},
			Body: io.NopCloser(io.MultiReader(strings.NewReader("par"), iotest.ErrReader(errors.New("connection reset")))),
		}, nil
	})
	h.run()
	h.enqueue(k, entry(staleHeader()))
	if o := h.next(); o != OutcomeKept {
		t.Fatalf("outcome %v, want kept", o)
	}
	wantModes(t, h.store, take)
	wantEntry(h, k, "stale")
}

// TestUnconditionalReplay checks that an entry without validators, such
// as a merged aggregate response, replays unconditionally, without the
// saved request's own preconditions or Range, and that a full response
// then renews it.
func TestUnconditionalReplay(t *testing.T) {
	h := newHarness(t)
	k := testKey(1)
	h.seed(k)
	e := entry(http.Header{"Cache-Control": {"max-age=60, stale-while-revalidate=30"}})
	e.reqHeader = http.Header{"Accept": {"*/*"}, "If-None-Match": {`"client"`}, "Range": {"bytes=0-9"}}
	h.replyFull(http.StatusOK, http.Header{"Cache-Control": {"max-age=60"}}, "merged")
	h.run()
	h.enqueue(k, e)
	if o := h.next(); o != OutcomeReplaced {
		t.Fatalf("outcome %v, want replaced", o)
	}
	calls := h.rp.replays()
	if want := (http.Header{"Accept": {"*/*"}}); len(calls) != 1 || !equalHeader(calls[0].header, want) {
		t.Errorf("replays %+v, want one with header %v", calls, want)
	}
	wantEntry(h, k, "replaced 200 merged")
}

// TestCompositionJob checks that a composition entry's job, which names no
// Upstream, is accepted and replays the whole Route.
func TestCompositionJob(t *testing.T) {
	h := newHarness(t)
	k := testKey(1)
	h.seed(k)
	h.reply304(http.Header{"Etag": {`"v1"`}})
	j := h.job(k, entry(staleHeader()))
	j.Upstream = ""
	h.run()
	if !h.rv.Enqueue(j) {
		t.Fatal("Enqueue refused a composition job")
	}
	if o := h.next(); o != OutcomeRefreshed {
		t.Fatalf("outcome %v, want refreshed", o)
	}
	if calls := h.rp.replays(); len(calls) != 1 || calls[0].route != testRoute || calls[0].upstream != "" {
		t.Errorf("replays %+v, want one of Route %s without an Upstream", calls, testRoute)
	}
}

// TestLeaseWritesNeedTheEntryToken checks that the refresh, replacement
// and end-stale writes apply only while the URI's generation key holds
// the token the entry was written under: after an invalidation bumps it
// during the replay, none of them applies.
func TestLeaseWritesNeedTheEntryToken(t *testing.T) {
	tests := []struct {
		name   string
		status int
		hdr    http.Header
		mode   statestore.LeaseMode
	}{
		{name: "refresh", status: http.StatusNotModified, hdr: http.Header{"Etag": {`"v1"`}}, mode: refresh},
		{name: "replacement", status: http.StatusOK, hdr: http.Header{"Cache-Control": {"max-age=60"}}, mode: refresh},
		{name: "end-stale", status: http.StatusOK, hdr: http.Header{"Cache-Control": {"no-store"}}, mode: endW},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			k := testKey(1)
			h.seed(k)
			h.rp.setReply(func(context.Context, *http.Request) (*http.Response, error) {
				h.bump(k)
				return &http.Response{StatusCode: tt.status, Header: tt.hdr.Clone(), Body: http.NoBody}, nil
			})
			h.run()
			h.enqueue(k, entry(staleHeader()))
			if o := h.next(); o != OutcomeWriteFailed {
				t.Fatalf("outcome %v, want write_failed", o)
			}
			w := h.store.recordedWrites()
			if len(w) != 2 || w[1].mode != tt.mode || w[1].generation != fixtureToken() || w[1].applied {
				t.Errorf("lease writes %+v, want an unapplied %v naming the entry's token", w, tt.mode)
			}
			wantEntry(h, k, "stale")
		})
	}
}

// TestKeepOnError checks that a 5xx or a replay without a response leaves
// the entry as it is, so stale-if-error can still serve it.
func TestKeepOnError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		err    error
	}{
		{name: "500", status: http.StatusInternalServerError},
		{name: "503", status: http.StatusServiceUnavailable},
		{name: "504", status: http.StatusGatewayTimeout},
		{name: "replay error", err: errors.New("connect: connection refused")},
		{name: "Route or Upstream gone", err: filter.ErrNotAvailable},
		{name: "neither response nor error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			k := testKey(1)
			h.seed(k)
			var b *body
			h.rp.setReply(func(context.Context, *http.Request) (*http.Response, error) {
				if tt.status == 0 {
					return nil, tt.err
				}
				var r *http.Response
				r, b = respond(tt.status, nil)
				return r, nil
			})
			h.run()
			h.enqueue(k, entry(staleHeader()))
			if o := h.next(); o != OutcomeKept {
				t.Fatalf("outcome %v, want kept", o)
			}
			wantModes(t, h.store, take)
			wantEntry(h, k, "stale")
			if b != nil && b.closed.Load() != 1 {
				t.Errorf("body closed %d times, want 1", b.closed.Load())
			}
		})
	}
}

// TestEndStaleOn304 checks the 304s that end the stale window instead of
// refreshing: validators that do not select the stored response, a result
// the Storable rule refuses, changed Vary names, no TTL left, or an entry
// the Filter cannot encode.
func TestEndStaleOn304(t *testing.T) {
	tests := []struct {
		name  string
		setup func(e *testEntry)
		nm    http.Header
	}{
		{name: "other entity tag", nm: http.Header{"Etag": {`"v2"`}}},
		{name: "Set-Cookie", nm: http.Header{"Etag": {`"v1"`}, "Set-Cookie": {"session=1"}}},
		{name: "no-store", nm: http.Header{"Cache-Control": {"no-store"}}},
		{name: "private", nm: http.Header{"Cache-Control": {"private, max-age=60"}}},
		{
			name: "Authorization without permission",
			setup: func(e *testEntry) {
				e.authorized = true
				e.header["Cache-Control"] = []string{"public, max-age=60, stale-while-revalidate=30"}
			},
			nm: http.Header{"Cache-Control": {"max-age=60"}},
		},
		{
			name:  "Vary names change",
			setup: func(e *testEntry) { e.header["Vary"] = []string{"Accept-Encoding"} },
			nm:    http.Header{"Vary": {"Accept-Encoding, Accept-Language"}},
		},
		{name: "no TTL left", nm: http.Header{"Cache-Control": {"max-age=0"}}},
		{
			name:  "entry encoding fails",
			setup: func(e *testEntry) { e.refreshErr = errors.New("entry too large") },
			nm:    http.Header{"Etag": {`"v1"`}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			k := testKey(1)
			h.seed(k)
			e := entry(staleHeader())
			if tt.setup != nil {
				tt.setup(e)
			}
			h.reply304(tt.nm)
			h.run()
			h.enqueue(k, e)
			if o := h.next(); o != OutcomeEnded {
				t.Fatalf("outcome %v, want ended", o)
			}
			wantModes(t, h.store, take, endW)
			wantEntry(h, k, "")
		})
	}
}

// TestEndStaleWithoutReplay checks that a saved request the entry cannot
// give ends the stale window under the lease without a replay.
func TestEndStaleWithoutReplay(t *testing.T) {
	tests := []struct {
		name string
		e    *testEntry
	}{
		{name: "unreadable saved request", e: &testEntry{header: staleHeader(), token: fixtureToken(), reqErr: errors.New("corrupt entry")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			k := testKey(1)
			h.seed(k)
			h.run()
			h.enqueue(k, tt.e)
			if o := h.next(); o != OutcomeEnded {
				t.Fatalf("outcome %v, want ended", o)
			}
			if n := len(h.rp.replays()); n != 0 {
				t.Errorf("%d replays, want none", n)
			}
			wantModes(t, h.store, take, endW)
			wantEntry(h, k, "")
		})
	}
}

// TestLeaseHeldSkipsReplay checks that a job whose partition lease another
// holder has does nothing more.
func TestLeaseHeldSkipsReplay(t *testing.T) {
	h := newHarness(t)
	k := testKey(1)
	h.seed(k)
	h.takeLease(k)
	h.run()
	h.enqueue(k, entry(staleHeader()))
	if o := h.next(); o != OutcomeLeaseHeld {
		t.Fatalf("outcome %v, want lease_held", o)
	}
	if n := len(h.rp.replays()); n != 0 {
		t.Errorf("%d replays, want none", n)
	}
	wantModes(t, h.store, take)
	wantEntry(h, k, "stale")
}

// TestLeaseFailureSkipsReplay checks that a failed lease take, such as an
// open State client breaker, ends the job without a replay.
func TestLeaseFailureSkipsReplay(t *testing.T) {
	h := newHarness(t)
	k := testKey(1)
	h.seed(k)
	h.store.setIntercept(func(w *statestore.Write) bool {
		w.Err = statestore.ErrBreakerOpen
		return true
	})
	h.run()
	h.enqueue(k, entry(staleHeader()))
	if o := h.next(); o != OutcomeLeaseFailed {
		t.Fatalf("outcome %v, want lease_failed", o)
	}
	if n := len(h.rp.replays()); n != 0 {
		t.Errorf("%d replays, want none", n)
	}
	wantEntry(h, k, "stale")
}

// TestWriteFailed checks the refresh, replacement and end-stale writes
// that do not apply: a lease that no longer holds the job's token, or a
// failed call.
func TestWriteFailed(t *testing.T) {
	tests := []struct {
		name   string
		status int
		hdr    http.Header
		mode   statestore.LeaseMode
		answer func(w *statestore.Write)
	}{
		{name: "lease lost before the refresh", status: http.StatusNotModified, mode: refresh, answer: func(w *statestore.Write) { w.Applied = false }},
		{name: "refresh write fails", status: http.StatusNotModified, mode: refresh, answer: func(w *statestore.Write) { w.Err = statestore.ErrFailed }},
		{
			name: "lease lost before the replacement", status: http.StatusOK, hdr: http.Header{"Cache-Control": {"max-age=60"}},
			mode: refresh, answer: func(w *statestore.Write) { w.Applied = false },
		},
		{name: "end-stale write times out", status: http.StatusOK, mode: endW, answer: func(w *statestore.Write) { w.Err = statestore.ErrTimeout }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			k := testKey(1)
			h.seed(k)
			h.store.setIntercept(func(w *statestore.Write) bool {
				if w.Lease.Mode != tt.mode {
					return false
				}
				tt.answer(w)
				return true
			})
			hdr := http.Header{"Etag": {`"v1"`}}
			for k, vs := range tt.hdr {
				hdr[k] = vs
			}
			h.rp.setReply(func(context.Context, *http.Request) (*http.Response, error) {
				r, _ := respond(tt.status, hdr.Clone())
				return r, nil
			})
			h.run()
			h.enqueue(k, entry(staleHeader()))
			if o := h.next(); o != OutcomeWriteFailed {
				t.Fatalf("outcome %v, want write_failed", o)
			}
			wantEntry(h, k, "stale")
		})
	}
}

// TestDeadline checks the 5 s deadline (target): it cancels a replay still
// running, and no write is attempted after it, so nothing lands once the
// lease may have passed to another Node.
func TestDeadline(t *testing.T) {
	t.Run("replay canceled", func(t *testing.T) {
		h := newHarness(t)
		k := testKey(1)
		h.seed(k)
		cause := make(chan error, 1)
		h.rp.setReply(func(ctx context.Context, _ *http.Request) (*http.Response, error) {
			<-ctx.Done()
			cause <- context.Cause(ctx)
			return nil, ctx.Err()
		})
		h.run()
		h.enqueue(k, entry(staleHeader()))
		h.awaitReplay()
		h.clk.Advance(Deadline)
		if o := h.next(); o != OutcomeKept {
			t.Fatalf("outcome %v, want kept", o)
		}
		if err := <-cause; !errors.Is(err, errDeadline) {
			t.Errorf("replay canceled by %v, want the deadline", err)
		}
		wantModes(t, h.store, take)
		wantEntry(h, k, "stale")
	})
	t.Run("304 after the deadline", func(t *testing.T) {
		h := newHarness(t)
		k := testKey(1)
		h.seed(k)
		h.rp.setReply(func(ctx context.Context, _ *http.Request) (*http.Response, error) {
			<-ctx.Done()
			r, _ := respond(http.StatusNotModified, http.Header{"Etag": {`"v1"`}})
			return r, nil
		})
		h.run()
		h.enqueue(k, entry(staleHeader()))
		h.awaitReplay()
		h.clk.Advance(Deadline)
		if o := h.next(); o != OutcomeWriteFailed {
			t.Fatalf("outcome %v, want write_failed", o)
		}
		w := h.store.recordedWrites()
		if len(w) != 2 || w[1].mode != refresh || w[1].timeout != 0 || w[1].err != statestore.ErrNotAttempted {
			t.Errorf("lease writes %+v, want an unattempted refresh last", w)
		}
		wantEntry(h, k, "stale")
	})
}

// TestLeaseWriteTimeout checks each lease write's timeout: the Policy's
// stateStoreTimeout, cut to the time left before the deadline.
func TestLeaseWriteTimeout(t *testing.T) {
	h := newHarness(t)
	k := testKey(1)
	h.seed(k)
	h.rp.setReply(func(context.Context, *http.Request) (*http.Response, error) {
		h.clk.Advance(Deadline - 20*time.Millisecond)
		r, _ := respond(http.StatusNotModified, http.Header{"Etag": {`"v1"`}})
		return r, nil
	})
	h.run()
	h.enqueue(k, entry(staleHeader()))
	if o := h.next(); o != OutcomeRefreshed {
		t.Fatalf("outcome %v, want refreshed", o)
	}
	w := h.store.recordedWrites()
	if len(w) != 2 || w[0].timeout != policyTimeout || w[1].timeout != 20*time.Millisecond {
		t.Errorf("lease writes %+v, want timeouts 50ms then 20ms", w)
	}
}

// TestLeaseTokens checks that the take and the refresh carry the job's one
// token, never 0.
func TestLeaseTokens(t *testing.T) {
	h := newHarness(t)
	h.rnd.vals = []uint64{0, 42}
	k := testKey(1)
	h.seed(k)
	h.reply304(http.Header{"Etag": {`"v1"`}})
	h.run()
	h.enqueue(k, entry(staleHeader()))
	if o := h.next(); o != OutcomeRefreshed {
		t.Fatalf("outcome %v, want refreshed", o)
	}
	w := h.store.recordedWrites()
	if len(w) != 2 || w[0].token != 42 || w[1].token != 42 {
		t.Errorf("lease writes %+v, want token 42 on both", w)
	}
}

// TestEnqueueDropsWhenFull checks the 256-entry queue (target): Enqueue
// never blocks and drops past it.
func TestEnqueueDropsWhenFull(t *testing.T) {
	h := newHarness(t)
	e := entry(staleHeader())
	for i := range QueueSize {
		h.enqueue(testKey(i), e)
	}
	if h.rv.Enqueue(h.job(testKey(QueueSize), e)) {
		t.Fatal("Enqueue accepted a job past the full queue")
	}
	if s := h.rv.Stats(); s.Queued != QueueSize || s.Dropped != 1 {
		t.Errorf("Stats() = %+v, want %d queued and 1 dropped", s, QueueSize)
	}
}

// TestEnqueueDeduplicates checks that a Node keeps one job per partition
// queued or running, and none for Deadline after its lease attempt.
func TestEnqueueDeduplicates(t *testing.T) {
	h := newHarness(t)
	k := testKey(1)
	e := entry(staleHeader())
	h.takeLease(k)
	h.enqueue(k, e)
	if h.rv.Enqueue(h.job(k, e)) {
		t.Error("Enqueue accepted a second job for a queued partition")
	}
	h.run()
	if o := h.next(); o != OutcomeLeaseHeld {
		t.Fatalf("outcome %v, want lease_held", o)
	}
	if h.rv.Enqueue(h.job(k, e)) {
		t.Error("Enqueue accepted a job right after the lease attempt")
	}
	h.clk.Advance(Deadline - time.Nanosecond)
	if h.rv.Enqueue(h.job(k, e)) {
		t.Error("Enqueue accepted a job before the hold ended")
	}
	h.clk.Advance(time.Nanosecond)
	if !h.rv.Enqueue(h.job(k, e)) {
		t.Error("Enqueue refused a job after the hold ended")
	}
	h.next()
	if s := h.rv.Stats(); s.Queued != 2 || s.Duplicate != 3 {
		t.Errorf("Stats() = %+v, want 2 queued and 3 duplicates", s)
	}
}

// TestPending checks Pending: true while a job for the partition is
// queued or running and for Deadline after its lease attempt, exactly
// when Enqueue would refuse another as a duplicate.
func TestPending(t *testing.T) {
	h := newHarness(t)
	k := testKey(1)
	if h.rv.Pending(k) {
		t.Fatal("an unknown partition is pending")
	}
	h.takeLease(k)
	h.enqueue(k, entry(staleHeader()))
	if !h.rv.Pending(k) || h.rv.Pending(testKey(2)) {
		t.Fatal("Pending does not follow the queued partition")
	}
	h.run()
	h.next()
	h.clk.Advance(Deadline - time.Nanosecond)
	if !h.rv.Pending(k) {
		t.Error("the partition is not pending during its hold")
	}
	h.clk.Advance(time.Nanosecond)
	if h.rv.Pending(k) {
		t.Error("the partition is pending after its hold")
	}
}

// TestHeldBound checks MaxHeld (target): past it a new partition is
// dropped until a hold ends.
func TestHeldBound(t *testing.T) {
	h := newHarness(t)
	h.rv.maxHeld = 2
	e := entry(staleHeader())
	for i := range 2 {
		h.takeLease(testKey(i))
		h.enqueue(testKey(i), e)
	}
	if h.rv.Enqueue(h.job(testKey(2), e)) {
		t.Error("Enqueue accepted a third partition while two were queued")
	}
	h.run()
	h.next()
	h.next()
	if h.rv.Enqueue(h.job(testKey(2), e)) {
		t.Error("Enqueue accepted a third partition while two were held")
	}
	h.clk.Advance(Deadline)
	h.takeLease(testKey(2))
	if !h.rv.Enqueue(h.job(testKey(2), e)) {
		t.Error("Enqueue refused a partition after the holds ended")
	}
	h.next()
	if s := h.rv.Stats(); s.Dropped != 2 {
		t.Errorf("Stats() = %+v, want 2 dropped", s)
	}
}

// TestQueueWait checks MaxQueueWait (target): a job that waited longer
// is dropped unrun and frees its partition; one that waited exactly as
// long still runs.
func TestQueueWait(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		h := newHarness(t)
		k := testKey(1)
		h.enqueue(k, entry(staleHeader()))
		h.clk.Advance(MaxQueueWait + time.Nanosecond)
		h.run()
		if o := h.next(); o != OutcomeExpired {
			t.Fatalf("outcome %v, want expired", o)
		}
		if w := h.store.recordedWrites(); len(w) != 0 {
			t.Errorf("lease writes %+v, want none", w)
		}
		h.takeLease(k)
		if !h.rv.Enqueue(h.job(k, entry(staleHeader()))) {
			t.Error("Enqueue refused the partition of an expired job")
		}
		h.next()
	})
	t.Run("on time", func(t *testing.T) {
		h := newHarness(t)
		k := testKey(1)
		h.enqueue(k, entry(staleHeader()))
		h.clk.Advance(MaxQueueWait)
		h.takeLease(k)
		h.run()
		if o := h.next(); o != OutcomeLeaseHeld {
			t.Fatalf("outcome %v, want lease_held", o)
		}
	})
}

// TestWorkersRunConcurrently checks the four workers (target): four
// replays run at once while a fifth job waits in the queue.
func TestWorkersRunConcurrently(t *testing.T) {
	h := newHarness(t)
	g := newGate(t)
	h.rp.setReply(func(ctx context.Context, _ *http.Request) (*http.Response, error) {
		select {
		case <-g.ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		r, _ := respond(http.StatusInternalServerError, nil)
		return r, nil
	})
	for i := range Workers + 1 {
		h.seed(testKey(i))
	}
	for i := range Workers + 1 {
		h.enqueue(testKey(i), entry(staleHeader()))
	}
	h.run()
	for range Workers {
		h.awaitReplay()
	}
	if n := len(h.rv.queue); n != 1 {
		t.Errorf("%d jobs queued while %d replays run, want 1", n, Workers)
	}
	g.open()
	for range Workers + 1 {
		if o := h.next(); o != OutcomeKept {
			t.Errorf("outcome %v, want kept", o)
		}
	}
}

// TestRunLifecycle checks Run: jobs still queued when it stops end
// canceled and free their partitions, Enqueue drops while stopped, a
// second concurrent Run fails and a stopped Revalidator runs again.
func TestRunLifecycle(t *testing.T) {
	h := newHarness(t)
	e := entry(staleHeader())
	for i := range 3 {
		h.enqueue(testKey(i), e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.rv.Run(ctx); err != nil {
		t.Fatalf("Run = %v", err)
	}
	for range 3 {
		if o := h.next(); o != OutcomeCanceled {
			t.Errorf("outcome %v, want canceled", o)
		}
	}
	if h.rv.Enqueue(h.job(testKey(0), e)) {
		t.Error("Enqueue accepted a job while stopped")
	}
	if s := h.rv.Stats(); s.Dropped != 1 || s.Outcomes[OutcomeCanceled] != 3 {
		t.Errorf("Stats() = %+v", s)
	}

	h.run()
	waitRunning(t, h.rv)
	h.takeLease(testKey(0))
	h.enqueue(testKey(0), e)
	if o := h.next(); o != OutcomeLeaseHeld {
		t.Errorf("outcome after a restart %v, want lease_held", o)
	}
	// The job finished, so the restarted Run is active. A second Run gets
	// a canceled context, so a wrong success returns instead of blocking.
	ctx2, cancel2 := context.WithCancel(t.Context())
	cancel2()
	if err := h.rv.Run(ctx2); !errors.Is(err, ErrRunning) {
		t.Errorf("second Run = %v, want ErrRunning", err)
	}
}

// waitRunning waits until a Run of rv is active: a stopped Revalidator
// drops jobs until its next Run starts.
func waitRunning(t *testing.T, rv *Revalidator) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for rv.state.Load() != stateRunning {
		if time.Now().After(deadline) {
			t.Fatal("Run did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestEnqueueRejectsInvalid checks that a job missing what it needs is
// refused uncounted; a zero Timeout would leave every write unattempted.
// A job without an Upstream is a composition entry's (TestCompositionJob).
func TestEnqueueRejectsInvalid(t *testing.T) {
	h := newHarness(t)
	tests := map[string]func(j *Job){
		"no store":    func(j *Job) { j.Store = nil },
		"no replayer": func(j *Job) { j.Replayer = nil },
		"no entry":    func(j *Job) { j.Entry = nil },
		"no timeout":  func(j *Job) { j.Timeout = 0 },
		"no route":    func(j *Job) { j.Route = "" },
	}
	for name, mutate := range tests {
		j := h.job(testKey(1), entry(staleHeader()))
		mutate(&j)
		if h.rv.Enqueue(j) {
			t.Errorf("Enqueue accepted a job with %s", name)
		}
	}
	if s := h.rv.Stats(); s != (Stats{}) {
		t.Errorf("Stats() = %+v, want zero", s)
	}
}

// TestName checks the Component name.
func TestName(t *testing.T) {
	var c filter.Component = New(Options{})
	if c.Name() != ComponentName {
		t.Errorf("Name() = %q", c.Name())
	}
}

// TestStrings checks the names of uses and outcomes.
func TestStrings(t *testing.T) {
	for u, want := range map[Use]string{UseFresh: "fresh", UseStale: "stale", UseValidate: "validate", 0: "unknown"} {
		if got := u.String(); got != want {
			t.Errorf("Use(%d).String() = %q, want %q", u, got, want)
		}
	}
	names := map[Outcome]string{
		OutcomeRefreshed: "refreshed", OutcomeEnded: "ended", OutcomeKept: "kept", OutcomeLeaseHeld: "lease_held",
		OutcomeLeaseFailed: "lease_failed", OutcomeWriteFailed: "write_failed", OutcomeExpired: "expired",
		OutcomeCanceled: "canceled", OutcomeReplaced: "replaced", 0: "unknown", NumOutcomes: "unknown",
	}
	for o, want := range names {
		if got := o.String(); got != want {
			t.Errorf("Outcome(%d).String() = %q, want %q", o, got, want)
		}
	}
}
