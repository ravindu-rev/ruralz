// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"crypto/sha256"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// TestMissStoresThenHits checks the main path of "Response caching": a GET
// miss is forwarded and records miss with fwd=uri-miss; its storable leg
// response starts a tee of the room MaxEntry leaves the body, the member
// says stored, and Finish queues one store that creates the URI's
// generation key with a fresh token; the next GET is a hit with Age, its
// member hit with the seconds of freshness left.
func TestMissStoresThenHits(t *testing.T) {
	h := newHarness(t)
	const body = `{"items":[1,2,3]}`
	miss := h.do(h.get(nil), leg{status: http.StatusOK, header: fresh60(t0), body: body})
	if !miss.forwarded || miss.cached {
		t.Fatalf("miss: forwarded %v, cached %v", miss.forwarded, miss.cached)
	}
	if got := cacheStatus(miss.header); !slices.Equal(got, []string{"ruralz;fwd=uri-miss;stored"}) {
		t.Errorf("miss Cache-Status %q", got)
	}
	if miss.x.CacheResult != "miss" {
		t.Errorf("miss cache field %q", miss.x.CacheResult)
	}
	ws := h.queue.recorded()
	if len(ws) != 1 || ws[0].class != statestore.ClassStore || ws[0].w.Kind != statestore.OpCacheSet {
		t.Fatalf("queued %+v, want one store", ws)
	}
	w := ws[0].w
	c := w.Cache
	switch {
	case w.Timeout != testTimeout:
		t.Errorf("store timeout %v", w.Timeout)
	case !c.Create || c.Generation.IsZero() || c.Token == 0:
		t.Errorf("store creates %v under token %x, fill token %d; want a fresh token and a create", c.Create, c.Generation, c.Token)
	case c.Names != "" || c.Variant != statestore.Digest(sha256.Sum256(nil)):
		t.Errorf("store names %q, variant %x; want none", c.Names, c.Variant)
	case c.TTL != time.Minute || !c.Received.Equal(t0):
		t.Errorf("store TTL %v, received %v", c.TTL, c.Received)
	case c.Key.URI != URIDigest("https", "shop.example", "shop.example", testPath, "page=2") || c.Key.Partition != PartitionDigest(testRoute, ""):
		t.Error("store key is not U and P of the request")
	}
	if prefix := len(c.Entry) - len(body); miss.x.TeeLimit != int64(MaxEntry-prefix) {
		t.Errorf("tee limit %d, want MaxEntry less the %d-byte prefix", miss.x.TeeLimit, prefix)
	}
	var e entry
	if err := decodeEntry(&e, c.Entry); err != nil {
		t.Fatal(err)
	}
	switch {
	case e.token != c.Generation || e.status != http.StatusOK || string(e.body) != body:
		t.Errorf("stored entry %+v", e)
	case e.route != testRoute || e.upstream != testUpstream || e.method != http.MethodGet || e.target != "https://shop.example/v1/catalog?page=2":
		t.Errorf("stored entry names %q %q %q %q", e.route, e.upstream, e.method, e.target)
	case e.header.Get("Content-Length") != "" || e.header.Get("Date") != date(t0) || e.flags != 0:
		t.Errorf("stored header %v, flags %d", e.header, e.flags)
	}

	h.clk.Advance(10 * time.Second)
	hit := h.do(h.get(nil), leg{})
	if hit.forwarded || !hit.cached || hit.status != http.StatusOK || string(hit.body) != body {
		t.Fatalf("hit: %+v", hit)
	}
	switch {
	case hit.header.Get("Age") != "10":
		t.Errorf("hit Age %q, want 10", hit.header.Get("Age"))
	case hit.header.Get("Content-Length") != strconv.Itoa(len(body)) || hit.header.Get("Etag") != `"v1"`:
		t.Errorf("hit header %v", hit.header)
	case !slices.Equal(cacheStatus(hit.header), []string{"ruralz;hit;ttl=50"}):
		t.Errorf("hit Cache-Status %q", cacheStatus(hit.header))
	case hit.x.CacheResult != "hit":
		t.Errorf("hit cache field %q", hit.x.CacheResult)
	}
	if got, want := h.count(), results(emit.CacheMiss, 1, emit.CacheHit, 1); got != want {
		t.Errorf("results %v, want %v", got, want)
	}
	if hit.x.Budget.Reserved != 0 {
		t.Errorf("a finished hit still holds %d bytes of the buffer budget", hit.x.Budget.Reserved)
	}
}

// TestCacheStatusAppended checks that the member is appended to any
// Cache-Status the stored response already carries.
func TestCacheStatusAppended(t *testing.T) {
	h := newHarness(t)
	hdr := fresh60(t0)
	hdr["Cache-Status"] = []string{"origin-cdn; hit"}
	h.prime(nil, hdr, "x")
	hit := h.do(h.get(nil), leg{})
	if got := cacheStatus(hit.header); !slices.Equal(got, []string{"origin-cdn; hit", "ruralz;hit;ttl=60"}) {
		t.Errorf("Cache-Status %q", got)
	}
}

// TestHeadServedFromGetEntry checks that a HEAD is served from the GET's
// entry, with the GET's Content-Length, and never stores.
func TestHeadServedFromGetEntry(t *testing.T) {
	h := newHarness(t)
	h.prime(nil, fresh60(t0), "0123456789")
	head := h.do(h.request(http.MethodHead, "page=2", nil), leg{})
	if !head.cached || head.status != http.StatusOK || head.header.Get("Content-Length") != "10" {
		t.Errorf("HEAD hit %+v", head)
	}
	missHead := h.do(h.request(http.MethodHead, "page=3", nil), leg{status: http.StatusOK, header: fresh60(t0)})
	if !missHead.forwarded || missHead.x.TeeLimit != 0 {
		t.Errorf("HEAD miss: forwarded %v, tee limit %d; want forwarded and no tee", missHead.forwarded, missHead.x.TeeLimit)
	}
	if got := cacheStatus(missHead.header); !slices.Equal(got, []string{"ruralz;fwd=uri-miss"}) {
		t.Errorf("HEAD miss Cache-Status %q", got)
	}
}

// TestClientValidation checks "Client validation": a GET or HEAD whose
// If-None-Match lists the stored entity tag or "*", or whose
// If-Modified-Since is no earlier than the stored Last-Modified, gets a
// 304 with the stored validators and caching fields, Age and the member.
func TestClientValidation(t *testing.T) {
	tests := []struct {
		name string
		req  http.Header
		want int
	}{
		{"matching If-None-Match", http.Header{"If-None-Match": {`W/"x", "v1"`}}, http.StatusNotModified},
		{"star", http.Header{"If-None-Match": {"*"}}, http.StatusNotModified},
		{"other entity tag", http.Header{"If-None-Match": {`"v2"`}}, http.StatusOK},
		{"If-Modified-Since later", http.Header{"If-Modified-Since": {date(t0)}}, http.StatusNotModified},
		{"If-Modified-Since earlier", http.Header{"If-Modified-Since": {date(t0.Add(-2 * time.Hour))}}, http.StatusOK},
		{"If-None-Match decides alone", http.Header{"If-None-Match": {`"v2"`}, "If-Modified-Since": {date(t0)}}, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.prime(nil, fresh60(t0), "body")
			o := h.do(h.get(tt.req), leg{})
			if o.status != tt.want || !o.cached {
				t.Fatalf("status %d, cached %v; want %d", o.status, o.cached, tt.want)
			}
			if tt.want == http.StatusNotModified {
				if o.body != nil || o.header.Get("Etag") != `"v1"` || o.header.Get("Cache-Control") != "max-age=60" ||
					o.header.Get("Age") != "0" || o.header.Get("Content-Type") != "" || o.header.Get("Last-Modified") != "" {
					t.Errorf("304 %v %q", o.header, o.body)
				}
				if !slices.Equal(cacheStatus(o.header), []string{"ruralz;hit;ttl=60"}) {
					t.Errorf("304 Cache-Status %q", cacheStatus(o.header))
				}
			}
			if got := h.count(); got[emit.CacheHit] != 1 {
				t.Errorf("results %v, want the 304 counted as a hit", got)
			}
		})
	}
}

// TestClientValidationOnlyFor2xx checks that preconditions are ignored for
// a stored response that is not 2xx (RFC 9110 section 13.2.1).
func TestClientValidationOnlyFor2xx(t *testing.T) {
	h := newHarness(t)
	h.do(h.get(nil), leg{status: http.StatusNotFound, header: fresh60(t0), body: "gone"})
	o := h.do(h.get(http.Header{"If-None-Match": {`"v1"`}}), leg{})
	if o.status != http.StatusNotFound || string(o.body) != "gone" {
		t.Errorf("stored 404 under a matching If-None-Match: %d %q", o.status, o.body)
	}
}

// TestRange checks the Range row: one satisfiable bytes range of a stored
// 200 is a 206 with Content-Length, Content-Range and Accept-Ranges; an
// unsatisfiable one 416 RZ-RT-026 with Content-Range bytes */length and
// the member; several ranges, another unit, an invalid Range or a failed
// If-Range the whole 200; a HEAD ignores Range.
func TestRange(t *testing.T) {
	const body = "0123456789"
	lm := t0.Add(-time.Hour)
	tests := []struct {
		name         string
		method       string
		req          http.Header
		status       int
		part, crange string
	}{
		{"first bytes", http.MethodGet, http.Header{"Range": {"bytes=0-3"}}, http.StatusPartialContent, "0123", "bytes 0-3/10"},
		{"open range", http.MethodGet, http.Header{"Range": {"bytes=7-"}}, http.StatusPartialContent, "789", "bytes 7-9/10"},
		{"suffix", http.MethodGet, http.Header{"Range": {"bytes=-2"}}, http.StatusPartialContent, "89", "bytes 8-9/10"},
		{"suffix past the length", http.MethodGet, http.Header{"Range": {"bytes=-50"}}, http.StatusPartialContent, body, "bytes 0-9/10"},
		{"last past the length", http.MethodGet, http.Header{"Range": {"bytes=5-99"}}, http.StatusPartialContent, "56789", "bytes 5-9/10"},
		{"unit in other case", http.MethodGet, http.Header{"Range": {"Bytes=1-1"}}, http.StatusPartialContent, "1", "bytes 1-1/10"},
		{"several ranges", http.MethodGet, http.Header{"Range": {"bytes=0-1,4-5"}}, http.StatusOK, body, ""},
		{"other unit", http.MethodGet, http.Header{"Range": {"items=0-1"}}, http.StatusOK, body, ""},
		{"first after last", http.MethodGet, http.Header{"Range": {"bytes=5-1"}}, http.StatusOK, body, ""},
		{"not a number", http.MethodGet, http.Header{"Range": {"bytes=a-1"}}, http.StatusOK, body, ""},
		{"two Range lines", http.MethodGet, http.Header{"Range": {"bytes=0-1", "bytes=2-3"}}, http.StatusOK, body, ""},
		{"unsatisfiable", http.MethodGet, http.Header{"Range": {"bytes=10-"}}, http.StatusRequestedRangeNotSatisfiable, "", "bytes */10"},
		{"zero suffix", http.MethodGet, http.Header{"Range": {"bytes=-0"}}, http.StatusRequestedRangeNotSatisfiable, "", "bytes */10"},
		{"If-Range strong match", http.MethodGet, http.Header{"Range": {"bytes=0-0"}, "If-Range": {`"v1"`}}, http.StatusPartialContent, "0", "bytes 0-0/10"},
		{"If-Range other tag", http.MethodGet, http.Header{"Range": {"bytes=0-0"}, "If-Range": {`"v2"`}}, http.StatusOK, body, ""},
		{"If-Range weak tag", http.MethodGet, http.Header{"Range": {"bytes=0-0"}, "If-Range": {`W/"v1"`}}, http.StatusOK, body, ""},
		{"If-Range date", http.MethodGet, http.Header{"Range": {"bytes=0-0"}, "If-Range": {date(lm)}}, http.StatusPartialContent, "0", "bytes 0-0/10"},
		{"If-Range other date", http.MethodGet, http.Header{"Range": {"bytes=0-0"}, "If-Range": {date(lm.Add(time.Second))}}, http.StatusOK, body, ""},
		{"HEAD ignores Range", http.MethodHead, http.Header{"Range": {"bytes=0-3"}}, http.StatusOK, body, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.prime(nil, fresh60(t0), body)
			o := h.do(h.request(tt.method, "page=2", tt.req), leg{})
			if o.status != tt.status || o.forwarded {
				t.Fatalf("status %d (forwarded %v), want %d", o.status, o.forwarded, tt.status)
			}
			switch tt.status {
			case http.StatusPartialContent:
				if string(o.body) != tt.part || o.header.Get("Content-Range") != tt.crange ||
					o.header.Get("Accept-Ranges") != "bytes" || o.header.Get("Content-Length") != strconv.Itoa(len(tt.part)) || !o.cached {
					t.Errorf("206 %q %v", o.body, o.header)
				}
			case http.StatusRequestedRangeNotSatisfiable:
				if o.cached || o.result.Response.Code != CodeRange || o.header.Get("Content-Range") != tt.crange {
					t.Errorf("416 %+v", o.result.Response)
				}
				if !slices.Equal(cacheStatus(o.header), []string{"ruralz;hit;ttl=60"}) {
					t.Errorf("416 Cache-Status %q", cacheStatus(o.header))
				}
			default:
				if string(o.body) != tt.part || o.header.Get("Content-Range") != "" {
					t.Errorf("200 %q %v", o.body, o.header)
				}
			}
			if got := h.count(); got[emit.CacheHit] != 1 {
				t.Errorf("results %v, want a hit", got)
			}
		})
	}
}

// TestIfRangeDateNeedsStrongLastModified checks that an If-Range date
// matches only a Last-Modified at least 1 s before the stored Date.
func TestIfRangeDateNeedsStrongLastModified(t *testing.T) {
	h := newHarness(t)
	hdr := fresh60(t0)
	hdr["Last-Modified"] = []string{date(t0)}
	h.prime(nil, hdr, "0123456789")
	o := h.do(h.get(http.Header{"Range": {"bytes=0-0"}, "If-Range": {date(t0)}}), leg{})
	if o.status != http.StatusOK {
		t.Errorf("status %d, want the whole 200 for a weak Last-Modified", o.status)
	}
}

// TestHitReservesBudget checks that a hit reserves its size from the
// buffer budget, holds it until Finish, and bypasses when it is refused.
func TestHitReservesBudget(t *testing.T) {
	h := newHarness(t)
	h.prime(nil, fresh60(t0), "0123456789")
	if r := h.f.Handle(t.Context(), phase.OnRequestBody, h.get(nil)); r.Outcome != filter.Continue {
		t.Fatal("a Phase other than onRequestHeaders and onResponse did something")
	}
	held := h.get(nil)
	o := h.do(held, leg{})
	if !o.cached || held.Budget.Reserved != 0 {
		t.Fatalf("hit cached %v, %d bytes still reserved after Finish", o.cached, held.Budget.Reserved)
	}
	refused := h.get(nil)
	refused.Budget.Exhausted = true
	o = h.do(refused, leg{status: http.StatusOK, header: fresh60(t0), body: "new"})
	if !o.forwarded || o.cached || o.x.CacheResult != "bypass" {
		t.Errorf("refused reservation: forwarded %v, cached %v, result %q; want a bypass", o.forwarded, o.cached, o.x.CacheResult)
	}
	if got := cacheStatus(o.header); !slices.Equal(got, []string{"ruralz;fwd=bypass"}) {
		t.Errorf("bypass Cache-Status %q", got)
	}
	if n := len(h.queue.recorded()); n != 1 {
		t.Errorf("a bypassed response was stored (%d writes)", n)
	}
}
