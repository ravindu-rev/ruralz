// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter/cache/revalidate"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// waitLimit bounds every wait for an event a test caused; reaching it
// fails the test instead of hanging it. No assertion depends on timing.
const waitLimit = 10 * time.Second

// header returns a response header with Cache-Control cc, the fixture
// validators and Date now.
func header(cc string, now time.Time) http.Header {
	h := fresh60(now)
	h["Cache-Control"] = []string{cc}
	return h
}

// TestStaleWhileRevalidate checks that a stale entry inside
// stale-while-revalidate serves at once, recorded stale with a negative
// ttl, and queues one background revalidation per partition.
func TestStaleWhileRevalidate(t *testing.T) {
	h := newHarness(t)
	h.prime(nil, header("max-age=60, stale-while-revalidate=30", t0), "old")
	h.clk.Advance(70 * time.Second)
	o := h.do(h.get(nil), leg{})
	if !o.cached || o.forwarded || string(o.body) != "old" || o.header.Get("Age") != "70" {
		t.Fatalf("stale hit %+v", o)
	}
	if got := cacheStatus(o.header); !slices.Equal(got, []string{"ruralz;hit;ttl=-10"}) {
		t.Errorf("stale Cache-Status %q", got)
	}
	if o.x.CacheResult != "stale" {
		t.Errorf("cache field %q, want stale", o.x.CacheResult)
	}
	h.do(h.get(nil), leg{})
	if s := h.rv.Stats(); s.Queued != 1 || s.Duplicate != 0 {
		t.Errorf("revalidator Stats() = %+v, want one job and no duplicate offered", s)
	}
	if got, want := h.count(), results(emit.CacheMiss, 1, emit.CacheStale, 2); got != want {
		t.Errorf("results %v, want %v", got, want)
	}
}

// signalStore signals each applied lease refresh or end-stale write.
type signalStore struct {
	*testStore
	done chan statestore.LeaseMode
}

// Write implements statestore.Store.
func (s *signalStore) Write(ctx context.Context, batch []*statestore.Write) {
	s.testStore.Write(ctx, batch)
	for _, w := range batch {
		if w.Lease.Mode == statestore.LeaseRefresh || w.Lease.Mode == statestore.LeaseEndStale {
			s.done <- w.Lease.Mode
		}
	}
}

// replayed is one Replay call.
type replayed struct {
	route, upstream, url string
	header               http.Header
}

// TestBackgroundRevalidation checks the job a stale hit queues, run by the
// Node's workers over the entry the Filter stored: the replay of the saved
// request with the stored validators to the entry's Route and Upstream,
// then a 304 refresh or a full response's replacement under the lease,
// which the next lookup serves.
func TestBackgroundRevalidation(t *testing.T) {
	tests := []struct {
		name   string
		status int
		hdr    func(now time.Time) http.Header
		body   string
		want   string
		age    string
	}{
		{"304 refreshes", http.StatusNotModified, func(now time.Time) http.Header {
			return http.Header{"Etag": {`"v1"`}, "Cache-Control": {"max-age=120"}, "Date": {date(now)}}
		}, "", "old", "0"},
		{"full response replaces", http.StatusOK, func(now time.Time) http.Header {
			return http.Header{"Etag": {`"v2"`}, "Cache-Control": {"max-age=120"}, "Date": {date(now)}}
		}, "new", "new", "0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			sig := &signalStore{testStore: h.store, done: make(chan statestore.LeaseMode, 4)}
			var mu sync.Mutex
			var calls []replayed
			h.replay = filtertest.ReplayFunc(func(_ context.Context, route, upstream string, req *http.Request) (*http.Response, error) {
				mu.Lock()
				calls = append(calls, replayed{route: route, upstream: upstream, url: req.URL.String(), header: req.Header.Clone()})
				mu.Unlock()
				return &http.Response{StatusCode: tt.status, Header: tt.hdr(h.clk.Now()), Body: io.NopCloser(strings.NewReader(tt.body))}, nil
			})
			env := h.env()
			env.CacheStore = sig
			flt, err := h.fac.Build(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			h.f = flt.(*Filter)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- h.rv.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Errorf("Run = %v", err)
				}
			})

			h.prime(http.Header{"Accept": {"application/json"}}, header("max-age=60, stale-while-revalidate=30", t0), "old")
			h.clk.Advance(70 * time.Second)
			if o := h.do(h.get(nil), leg{}); o.x.CacheResult != "stale" {
				t.Fatalf("result %q, want stale", o.x.CacheResult)
			}
			select {
			case <-sig.done:
			case <-time.After(waitLimit):
				t.Fatal("the revalidation wrote nothing")
			}
			mu.Lock()
			got := slices.Clone(calls)
			mu.Unlock()
			if len(got) != 1 || got[0].route != testRoute || got[0].upstream != testUpstream ||
				got[0].url != "https://shop.example/v1/catalog?page=2" || got[0].header.Get("If-None-Match") != `"v1"` ||
				got[0].header.Get("Accept") != "application/json" {
				t.Fatalf("replays %+v", got)
			}
			o := h.do(h.get(nil), leg{})
			if !o.cached || o.x.CacheResult != "hit" || string(o.body) != tt.want || o.header.Get("Age") != tt.age {
				t.Errorf("after the revalidation: result %q, body %q, Age %q", o.x.CacheResult, o.body, o.header.Get("Age"))
			}
		})
	}
}

// TestCredentialedRevalidatesOnRequestPath checks that a credentialed
// entry is never revalidated in the background: stale inside
// stale-while-revalidate, it validates on the request path with the
// client's own request and the stored validators.
func TestCredentialedRevalidatesOnRequestPath(t *testing.T) {
	h := newHarness(t)
	h.prime(http.Header{"Authorization": {"Bearer t"}}, header("public, max-age=60, stale-while-revalidate=30", t0), "old")
	h.clk.Advance(70 * time.Second)
	x := h.get(http.Header{"Authorization": {"Bearer t"}})
	o := h.do(x, leg{status: http.StatusNotModified, header: http.Header{"Etag": {`"v1"`}, "Date": {date(h.clk.Now())}}})
	if !o.forwarded || x.Hdr.Get("If-None-Match") != `"v1"` || x.Hdr.Get("Authorization") != "Bearer t" {
		t.Fatalf("forwarded %v with header %v", o.forwarded, x.Hdr)
	}
	if !o.cached || string(o.body) != "old" || !slices.Equal(cacheStatus(o.header), []string{"ruralz;fwd=stale;fwd-status=304"}) {
		t.Errorf("refreshed answer %d %q %q", o.status, o.body, cacheStatus(o.header))
	}
	if s := h.rv.Stats(); s.Queued != 0 {
		t.Errorf("a credentialed entry queued %d background revalidations", s.Queued)
	}
}

// TestNoReplayerRevalidatesOnRequestPath checks that without a Replayer a
// stale entry validates on the request path instead.
func TestNoReplayerRevalidatesOnRequestPath(t *testing.T) {
	h := newHarness(t, withoutReplayer())
	h.prime(nil, header("max-age=60, stale-while-revalidate=30", t0), "old")
	h.clk.Advance(70 * time.Second)
	if o := h.do(h.get(nil), leg{status: http.StatusOK, header: header("max-age=60", h.clk.Now()), body: "new"}); !o.forwarded {
		t.Error("a stale entry without a Replayer was served stale")
	}
}

// TestValidationRefreshes checks the request-path validation of a stale
// entry: the forward carries the stored validators in place of the
// client's own preconditions and Range; a selecting 304 serves the stored
// response it freshened, recorded miss with fwd=stale and fwd-status 304,
// and queues its refresh under the lookup's token.
func TestValidationRefreshes(t *testing.T) {
	h := newHarness(t)
	h.prime(nil, header("max-age=60, stale-if-error=300", t0), "old")
	h.clk.Advance(2 * time.Minute)
	now := h.clk.Now()
	x := h.get(http.Header{"If-None-Match": {`"client"`}, "Range": {"bytes=0-0"}, "If-Match": {"*"}})
	o := h.do(x, leg{status: http.StatusNotModified, header: http.Header{"Etag": {`"v1"`}, "Cache-Control": {"max-age=60"}, "Date": {date(now)}}})
	want := http.Header{"If-None-Match": {`"v1"`}, "If-Modified-Since": {date(t0.Add(-time.Hour))}}
	if !o.forwarded || !equalHeader(x.Hdr, want) {
		t.Fatalf("forwarded %v with header %v, want %v", o.forwarded, x.Hdr, want)
	}
	// The client's own Range applies to the refreshed response.
	if !o.cached || o.status != http.StatusPartialContent || string(o.body) != "o" || o.header.Get("Age") != "0" {
		t.Errorf("refreshed answer %d %q Age %q", o.status, o.body, o.header.Get("Age"))
	}
	if got := cacheStatus(o.header); !slices.Equal(got, []string{"ruralz;fwd=stale;fwd-status=304"}) {
		t.Errorf("Cache-Status %q", got)
	}
	if o.x.CacheResult != "miss" {
		t.Errorf("cache field %q, want miss", o.x.CacheResult)
	}
	ws := h.queue.recorded()
	if len(ws) != 2 {
		t.Fatalf("%d writes, want the store and the refresh", len(ws))
	}
	prime, re := ws[0].w.Cache, ws[1].w.Cache
	if re.Create || re.Generation != prime.Generation || re.Variant != prime.Variant || re.TTL != time.Minute || !re.Received.Equal(now) {
		t.Errorf("refresh %+v, want the stored token and variant, TTL 1m", re)
	}
	var e entry
	if err := decodeEntry(&e, re.Entry); err != nil || e.header.Get("Date") != date(now) || string(e.body) != "old" {
		t.Errorf("refreshed entry %+v (%v)", e, err)
	}
	if hit := h.do(h.get(nil), leg{}); hit.x.CacheResult != "hit" {
		t.Errorf("after the refresh: result %q", hit.x.CacheResult)
	}
}

// TestValidationAppliesClientConditions checks that the response a 304
// refreshed answers the client's own preconditions and Range, which the
// forward did not carry.
func TestValidationAppliesClientConditions(t *testing.T) {
	tests := []struct {
		name   string
		req    http.Header
		status int
		body   string
	}{
		{"client copy validated", http.Header{"If-None-Match": {`"v1"`}}, http.StatusNotModified, ""},
		{"range", http.Header{"Range": {"bytes=2-3"}}, http.StatusPartialContent, "23"},
		{"unsatisfiable range gets the whole response", http.Header{"Range": {"bytes=50-"}}, http.StatusOK, "0123456789"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.prime(nil, header("max-age=60, stale-if-error=300", t0), "0123456789")
			h.clk.Advance(2 * time.Minute)
			nm := leg{status: http.StatusNotModified, header: http.Header{"Etag": {`"v1"`}, "Date": {date(h.clk.Now())}}}
			o := h.do(h.get(tt.req), nm)
			if !o.forwarded || !o.cached || o.status != tt.status || string(o.body) != tt.body {
				t.Errorf("forwarded %v, cached %v: %d %q; want %d %q", o.forwarded, o.cached, o.status, o.body, tt.status, tt.body)
			}
		})
	}
}

// TestValidationNonSelecting304 checks that a 304 whose validators do not
// select the stored response passes through unstored.
func TestValidationNonSelecting304(t *testing.T) {
	h := newHarness(t)
	h.prime(nil, header("max-age=60, stale-if-error=300", t0), "old")
	h.clk.Advance(2 * time.Minute)
	o := h.do(h.get(nil), leg{status: http.StatusNotModified, header: http.Header{"Etag": {`"v9"`}}})
	if o.cached || o.status != http.StatusNotModified {
		t.Errorf("non-selecting 304: status %d, cached %v", o.status, o.cached)
	}
	if n := len(h.queue.recorded()); n != 1 {
		t.Errorf("%d writes, want none beyond the store", n)
	}
}

// TestValidationFullResponseStores checks that a full response to a
// validation is stored under the lookup's token, replacing the variant.
func TestValidationFullResponseStores(t *testing.T) {
	h := newHarness(t)
	h.prime(nil, header("max-age=60, stale-if-error=300", t0), "old")
	h.clk.Advance(2 * time.Minute)
	o := h.do(h.get(nil), leg{status: http.StatusOK, header: header("max-age=60", h.clk.Now()), body: "new"})
	if !o.forwarded || o.cached || string(o.body) != "new" {
		t.Fatalf("full response %+v", o)
	}
	if got := cacheStatus(o.header); !slices.Equal(got, []string{"ruralz;fwd=stale;stored"}) {
		t.Errorf("Cache-Status %q", got)
	}
	ws := h.queue.recorded()
	if len(ws) != 2 || ws[1].w.Cache.Create || ws[1].w.Cache.Generation != ws[0].w.Cache.Generation {
		t.Fatalf("writes %+v, want a store under the stored token", ws)
	}
	if hit := h.do(h.get(nil), leg{}); string(hit.body) != "new" {
		t.Errorf("after the store: %q", hit.body)
	}
}

// TestStaleIfError checks stale-if-error: after a validation, a 500, 502,
// 503 or 504 from the Upstream, or a Node-generated failure of the leg,
// is replaced by the stored response while the response's or the
// request's window covers its age, recorded stale_error with fwd-status
// and the negative ttl; past it the error passes through. An entry lives
// only for its freshness plus its larger stale window, so a validation
// past stale-if-error needs a request directive forcing it.
func TestStaleIfError(t *testing.T) {
	tests := []struct {
		name    string
		cc      string
		req     http.Header
		advance time.Duration
		l       leg
		served  bool
		member  string
	}{
		{
			"503 from the Upstream", "max-age=60, stale-if-error=300", nil, 2 * time.Minute,
			leg{status: http.StatusServiceUnavailable, body: "down"},
			true, "ruralz;fwd=stale;fwd-status=503;ttl=-60",
		},
		{
			"Node-generated 502", "max-age=60, stale-if-error=300", nil, 2 * time.Minute,
			leg{failed: http.StatusBadGateway},
			true, "ruralz;fwd=stale;fwd-status=502;ttl=-60",
		},
		{
			"500", "max-age=60, stale-if-error=300", nil, 2 * time.Minute,
			leg{status: http.StatusInternalServerError},
			true, "ruralz;fwd=stale;fwd-status=500;ttl=-60",
		},
		{
			"past the window", "max-age=60, stale-while-revalidate=300, stale-if-error=30",
			http.Header{"Cache-Control": {"no-cache"}},
			2 * time.Minute,
			leg{status: http.StatusServiceUnavailable},
			false, "ruralz;fwd=request",
		},
		{
			"request widens the window", "max-age=60, stale-while-revalidate=300",
			http.Header{"Cache-Control": {"max-age=10, stale-if-error=600"}},
			2 * time.Minute,
			leg{status: http.StatusGatewayTimeout},
			true, "ruralz;fwd=request;fwd-status=504;ttl=-60",
		},
		{
			"a fresh entry serves on error", "max-age=60",
			http.Header{"Cache-Control": {"no-cache"}},
			10 * time.Second,
			leg{status: http.StatusBadGateway},
			true, "ruralz;fwd=request;fwd-status=502",
		},
		{
			"404 is no error", "max-age=60, stale-if-error=300", nil, 2 * time.Minute,
			leg{status: http.StatusNotFound, header: http.Header{"Cache-Control": {"no-store"}}},
			false, "ruralz;fwd=stale",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.prime(nil, header(tt.cc, t0), "old")
			h.clk.Advance(tt.advance)
			o := h.do(h.get(tt.req), tt.l)
			if !o.forwarded {
				t.Fatal("the stale entry was not validated")
			}
			if o.cached != tt.served || (tt.served && (string(o.body) != "old" || o.status != http.StatusOK)) {
				t.Errorf("served %v: %d %q", o.cached, o.status, o.body)
			}
			if got := cacheStatus(o.header); !slices.Equal(got, []string{tt.member}) {
				t.Errorf("Cache-Status %q, want %q", got, tt.member)
			}
			want := "miss"
			if tt.served {
				want = "stale_error"
			}
			if o.x.CacheResult != want {
				t.Errorf("cache field %q, want %q", o.x.CacheResult, want)
			}
		})
	}
}

// TestRequestForcesValidation checks "Request directives": no-cache and
// max-age=0 validate a fresh entry, recorded with fwd=request; a larger
// max-age bounds the age and min-fresh the freshness left; max-stale
// accepts staleness up to its value.
func TestRequestForcesValidation(t *testing.T) {
	tests := []struct {
		name    string
		cc      string
		req     string
		advance time.Duration
		member  string
	}{
		{"no-cache", "max-age=60", "no-cache", 0, "ruralz;fwd=request;fwd-status=304"},
		{"max-age=0", "max-age=60", "max-age=0", 0, "ruralz;fwd=request;fwd-status=304"},
		{"max-age below the age", "max-age=60", "max-age=5", 10 * time.Second, "ruralz;fwd=request;fwd-status=304"},
		{"min-fresh", "max-age=60", "min-fresh=55", 10 * time.Second, "ruralz;fwd=request;fwd-status=304"},
		{"max-stale accepts", "max-age=60, stale-if-error=300", "max-stale=100", 90 * time.Second, "ruralz;hit;ttl=-30"},
		{"max-stale too small", "max-age=60, stale-if-error=300", "max-stale=10", 90 * time.Second, "ruralz;fwd=stale;fwd-status=304"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.prime(nil, header(tt.cc, t0), "old")
			h.clk.Advance(tt.advance)
			o := h.do(h.get(http.Header{"Cache-Control": {tt.req}}), leg{status: http.StatusNotModified, header: http.Header{"Etag": {`"v1"`}}})
			if got := cacheStatus(o.header); !slices.Equal(got, []string{tt.member}) {
				t.Errorf("Cache-Status %q, want %q", got, tt.member)
			}
			if string(o.body) != "old" {
				t.Errorf("body %q", o.body)
			}
		})
	}
}

// TestOnlyIfCached checks that only-if-cached is answered from a stored
// response or with 504 RZ-RT-018, never forwarded.
func TestOnlyIfCached(t *testing.T) {
	h := newHarness(t)
	oic := http.Header{"Cache-Control": {"only-if-cached"}}
	o := h.do(h.get(oic.Clone()), leg{})
	if o.forwarded || o.status != http.StatusGatewayTimeout || o.result.Response.Code != CodeOnlyIfCached {
		t.Fatalf("miss: %+v", o.result)
	}
	if got := cacheStatus(o.header); !slices.Equal(got, []string{"ruralz"}) || o.x.CacheResult != "miss" {
		t.Errorf("504 Cache-Status %q, result %q", got, o.x.CacheResult)
	}
	h.prime(nil, fresh60(t0), "stored")
	if o := h.do(h.get(oic.Clone()), leg{}); !o.cached || string(o.body) != "stored" {
		t.Errorf("fresh entry under only-if-cached: %+v", o)
	}
	h.clk.Advance(2 * time.Minute)
	if o := h.do(h.get(oic.Clone()), leg{}); o.forwarded || o.status != http.StatusGatewayTimeout {
		t.Errorf("stale entry under only-if-cached: forwarded %v, status %d", o.forwarded, o.status)
	}
	nostore := http.Header{"Cache-Control": {"only-if-cached, no-store"}}
	if o := h.do(h.get(nostore), leg{}); o.forwarded || o.status != http.StatusGatewayTimeout || o.x.CacheResult != "bypass" {
		t.Errorf("bypass under only-if-cached: forwarded %v, status %d, result %q", o.forwarded, o.status, o.x.CacheResult)
	}
}

// equalHeader compares headers ignoring the order of keys.
func equalHeader(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

// TestJobEntry checks the background revalidation's view of an entry: the
// saved request in absolute form with its own header, the stored header,
// the flags and token, and the refreshed and replacing encodings.
func TestJobEntry(t *testing.T) {
	e := goldenEntry()
	j := &jobEntry{e: e}
	r, err := j.Request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != http.MethodGet || r.URL.String() != e.target || r.Host != "shop.example" || r.Header.Get("Accept") != "application/json" {
		t.Errorf("saved request %s %s host %q %v", r.Method, r.URL, r.Host, r.Header)
	}
	r.Header.Set("Accept", "x")
	if e.reqHeader.Get("Accept") != "application/json" {
		t.Error("the saved request's header is shared")
	}
	if !j.Authorized() || j.Token() != e.token || j.Header().Get("Etag") != `"v1"` {
		t.Error("flags, token or header lost")
	}
	b, err := j.Refresh(&revalidate.Refreshed{Header: http.Header{"Cache-Control": {"max-age=9"}}, InitialAge: 2 * time.Second})
	var re entry
	if err != nil || decodeEntry(&re, b) != nil || re.header.Get("Cache-Control") != "max-age=9" || re.initialAge != 2*time.Second ||
		string(re.body) != "hello" || re.token != e.token {
		t.Errorf("refreshed entry %+v (%v)", re, err)
	}
	b, err = j.Replace(&revalidate.Replacement{Status: 203, Header: http.Header{"Content-Length": {"3"}}, Body: []byte("new"), Received: t0})
	if err != nil || decodeEntry(&re, b) != nil || re.status != 203 || string(re.body) != "new" ||
		re.header.Get("Content-Length") != "" || re.header.Get("Date") != date(t0) || re.target != e.target {
		t.Errorf("replacing entry %+v (%v)", re, err)
	}
	big := &revalidate.Refreshed{Header: http.Header{"X-Big": {strings.Repeat("x", MaxEntry)}}}
	if _, err := j.Refresh(big); err == nil {
		t.Error("a refreshed entry past MaxEntry encoded")
	}
}
