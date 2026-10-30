// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestNotStored checks the Storable row: a leg response is stored only
// with explicit freshness and TTL left, a final status other than 206 and
// 304, no no-store, no-cache, private, Vary * or Set-Cookie, under
// Authorization only with public, s-maxage or must-revalidate, and never
// as a partial composition response; a Node-generated failure has no leg
// response. None starts a tee or says stored.
func TestNotStored(t *testing.T) {
	cc := func(v string) http.Header { return header(v, t0) }
	tests := []struct {
		name string
		req  http.Header
		l    leg
	}{
		{"no-store", nil, leg{status: http.StatusOK, header: cc("max-age=60, no-store")}},
		{"no-cache", nil, leg{status: http.StatusOK, header: cc("max-age=60, no-cache")}},
		{"private", nil, leg{status: http.StatusOK, header: cc("private, max-age=60")}},
		{"no explicit freshness", nil, leg{status: http.StatusOK, header: cc("public")}},
		{"no TTL left", nil, leg{status: http.StatusOK, header: cc("max-age=0")}},
		{"206", nil, leg{status: http.StatusPartialContent, header: cc("max-age=60")}},
		{"304", nil, leg{status: http.StatusNotModified, header: cc("max-age=60")}},
		{"Vary star", nil, leg{status: http.StatusOK, header: func() http.Header { h := cc("max-age=60"); h["Vary"] = []string{"*"}; return h }()}},
		{"Set-Cookie", nil, leg{status: http.StatusOK, header: func() http.Header { h := cc("max-age=60"); h["Set-Cookie"] = []string{"a=b"}; return h }()}},
		{"partial composition", nil, leg{status: http.StatusOK, header: func() http.Header {
			h := cc("max-age=60")
			h["Ruralz-Partial"] = []string{"true"}
			return h
		}()}},
		{"Authorization without permission", http.Header{"Authorization": {"Bearer t"}}, leg{status: http.StatusOK, header: cc("max-age=60")}},
		{"Node-generated failure", nil, leg{failed: http.StatusBadGateway}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			o := h.do(h.get(tt.req), tt.l)
			if !o.forwarded || o.x.TeeLimit != 0 {
				t.Errorf("forwarded %v, tee limit %d; want no tee", o.forwarded, o.x.TeeLimit)
			}
			if n := len(h.queue.recorded()); n != 0 {
				t.Errorf("%d writes, want none", n)
			}
			if got := cacheStatus(o.header); !slices.Equal(got, []string{"ruralz;fwd=uri-miss"}) {
				t.Errorf("Cache-Status %q", got)
			}
		})
	}
}

// TestStoredWithAuthorization checks that a response to a request with
// Authorization is stored under public, s-maxage or must-revalidate, as a
// credentialed and authorized entry.
func TestStoredWithAuthorization(t *testing.T) {
	for _, v := range []string{"public, max-age=60", "s-maxage=60", "max-age=60, must-revalidate"} {
		h := newHarness(t)
		h.prime(http.Header{"Authorization": {"Bearer t"}}, header(v, t0), "x")
		var e entry
		if err := decodeEntry(&e, h.queue.recorded()[0].w.Cache.Entry); err != nil || e.flags != flagCredentialed|flagAuthorized {
			t.Errorf("%s: flags %d (%v), want credentialed and authorized", v, e.flags, err)
		}
	}
}

// TestSavedRequest checks what a store saves of the request: its target in
// absolute form with the query the request Phases left, and its header
// without hop-by-hop fields, credential fields (Authorization,
// Proxy-Authorization, Cookie and every auth.api-key header of the
// Revision), preconditions or Range; a request with any credential stores
// a credentialed entry.
func TestSavedRequest(t *testing.T) {
	h := newHarness(t)
	shopKey := "X-Shop-Key"
	b := hub.NewBundle([]*hub.Resource{{
		ID:     hub.ID{Kind: v1alpha1.KindPolicy, Name: "keys"},
		Object: &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "keys"}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeAuthAPIKey}},
		Config: &v1alpha1.AuthAPIKeyConfig{Header: &shopKey},
	}, {
		ID:     hub.ID{Kind: v1alpha1.KindPolicy, Name: "default-keys"},
		Object: &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "default-keys"}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeAuthAPIKey}},
		Config: &v1alpha1.AuthAPIKeyConfig{},
	}})
	h.f = h.build(withBundle(b))
	req := http.Header{
		"Accept": {"application/json"}, "Cookie": {"s=1"}, "Proxy-Authorization": {"Basic x"},
		"X-Shop-Key": {"k1"}, "X-Api-Key": {"k2"}, "Connection": {"keep-alive"}, "Te": {"gzip"},
		"If-None-Match": {`"c"`}, "Range": {"bytes=0-1"},
	}
	x := h.get(req)
	x.VarsV.Request.SetRawQuery("page=2&rewritten=1")
	o := h.do(x, leg{status: http.StatusOK, header: header("max-age=60", t0), body: "x"})
	if !o.forwarded {
		t.Fatal("not forwarded")
	}
	ws := h.queue.recorded()
	if len(ws) != 1 {
		t.Fatalf("%d writes", len(ws))
	}
	var e entry
	if err := decodeEntry(&e, ws[0].w.Cache.Entry); err != nil {
		t.Fatal(err)
	}
	if want := (http.Header{"Accept": {"application/json"}}); !equalHeader(e.reqHeader, want) {
		t.Errorf("saved header %v, want %v", e.reqHeader, want)
	}
	if e.target != "https://shop.example/v1/catalog?page=2&rewritten=1" || e.flags != flagCredentialed {
		t.Errorf("target %q, flags %d", e.target, e.flags)
	}
}

// TestCredentialsFromIdentity checks that a request auth.api-key or
// auth.basic authenticated, which removed its credential field, stores a
// credentialed entry, authorized for auth.basic.
func TestCredentialsFromIdentity(t *testing.T) {
	for method, want := range map[string]uint64{
		filter.MethodAPIKey: flagCredentialed,
		filter.MethodBasic:  flagCredentialed | flagAuthorized,
		filter.MethodJWT:    0,
	} {
		h := newHarness(t)
		x := h.get(nil)
		x.Ident = &filter.Identity{Method: method, Principal: "acme"}
		cc := "max-age=60"
		if want&flagAuthorized != 0 {
			cc = "public, max-age=60"
		}
		h.do(x, leg{status: http.StatusOK, header: header(cc, t0), body: "x"})
		ws := h.queue.recorded()
		var e entry
		if len(ws) != 1 || decodeEntry(&e, ws[0].w.Cache.Entry) != nil || e.flags != want {
			t.Errorf("%s: flags %d, want %d", method, e.flags, want)
		}
	}
}

// TestSizeLimit checks the Size row: encoded header blocks and saved
// request alone past 128 KiB count size_limit and start no tee; a tee the
// data plane refuses or stops stores nothing and counts nothing here.
func TestSizeLimit(t *testing.T) {
	h := newHarness(t)
	big := http.Header{"X-Big": {strings.Repeat("x", MaxEntry)}}
	o := h.do(h.get(big), leg{status: http.StatusOK, header: header("max-age=60", t0), body: "x"})
	if o.x.TeeLimit != 0 || h.skips[emit.SkipSizeLimit].n.Load() != 1 || len(h.queue.recorded()) != 0 {
		t.Errorf("tee limit %d, size_limit %d, writes %d; want no tee, 1, none",
			o.x.TeeLimit, h.skips[emit.SkipSizeLimit].n.Load(), len(h.queue.recorded()))
	}
	if got := cacheStatus(o.header); !slices.Equal(got, []string{"ruralz;fwd=uri-miss"}) {
		t.Errorf("Cache-Status %q", got)
	}
	for _, l := range []leg{
		{status: http.StatusOK, header: header("max-age=60", t0), body: "x", noTee: true},
		{status: http.StatusOK, header: header("max-age=60", t0), body: "x", incomplete: true},
	} {
		h := newHarness(t)
		h.do(h.get(nil), l)
		if n := len(h.queue.recorded()); n != 0 || h.skips[emit.SkipSizeLimit].n.Load() != 0 {
			t.Errorf("tee refused %v, stopped %v: %d writes, size_limit %d", l.noTee, l.incomplete, n, h.skips[emit.SkipSizeLimit].n.Load())
		}
	}
}

// TestMemoryRules checks that a store the memory rules refuse counts
// memory and is not queued, and that a store creating the generation key
// counts its bytes.
func TestMemoryRules(t *testing.T) {
	h := newHarness(t)
	h.store.refuse = true
	h.do(h.get(nil), leg{status: http.StatusOK, header: header("max-age=60", t0), body: "x"})
	if n := len(h.queue.recorded()); n != 0 || h.skips[emit.SkipMemory].n.Load() != 1 {
		t.Errorf("%d writes, memory %d; want none and 1", n, h.skips[emit.SkipMemory].n.Load())
	}
	h.store.refuse = false
	h.prime(nil, header("max-age=60", t0), "x")
	w := h.queue.recorded()[0].w
	if got := h.store.admits[len(h.store.admits)-1]; !w.Cache.Create || got != len(w.Cache.Entry)+generationKeyBytes {
		t.Errorf("memory rules asked for %d bytes, want the %d-byte entry and the generation key", got, len(w.Cache.Entry))
	}
}

// TestQueueDrop checks that a store the post-commit queue drops leaves
// nothing behind.
func TestQueueDrop(t *testing.T) {
	h := newHarness(t)
	h.queue.drop = true
	o := h.do(h.get(nil), leg{status: http.StatusOK, header: header("max-age=60", t0), body: "x"})
	if !o.forwarded || len(h.queue.recorded()) != 0 {
		t.Fatal("a dropped store was written")
	}
	h.queue.drop = false
	if o := h.do(h.get(nil), leg{status: http.StatusOK, header: header("max-age=60", t0), body: "x"}); !o.forwarded {
		t.Error("a dropped store hit")
	}
}

// TestStoreRecordsReceipt checks that a store names the response's
// receipt, from which the driver measures the receipt delay t:<V>
// subtracts, and the initial age of a slow response.
func TestStoreRecordsReceipt(t *testing.T) {
	h := newHarness(t)
	x := h.get(nil)
	ctx := t.Context()
	h.f.Handle(ctx, phase.OnRequestHeaders, x)
	h.clk.Advance(3 * time.Second) // the response takes 3 s
	var o outcome
	h.respond(ctx, x, leg{status: http.StatusOK, header: header("max-age=60", t0), body: "x"}, &o)
	h.clk.Advance(time.Second) // Finish runs 1 s later
	h.f.Finish(ctx, x)
	ws := h.queue.recorded()
	if len(ws) != 1 {
		t.Fatalf("%d writes", len(ws))
	}
	c := ws[0].w.Cache
	var e entry
	if err := decodeEntry(&e, c.Entry); err != nil {
		t.Fatal(err)
	}
	if !c.Received.Equal(t0.Add(3*time.Second)) || e.initialAge != 3*time.Second || c.TTL != 57*time.Second {
		t.Errorf("received %v, initial age %v, TTL %v; want t0+3s, 3s, 57s", c.Received, e.initialAge, c.TTL)
	}
}

// TestBypass checks that the cache stays out of a request with a body,
// no-store, a config.key that fails to evaluate, or an authenticated
// request without a principal: no lookup, no store, fwd=bypass.
func TestBypass(t *testing.T) {
	tests := []struct {
		name  string
		opts  []option
		setup func(x *httpExchange)
	}{
		{"Content-Length", nil, func(x *httpExchange) { x.ReceivedV.ContentLength = 5 }},
		{"Transfer-Encoding", nil, func(x *httpExchange) { x.ReceivedV.ContentLength = -1 }},
		{"no-store", nil, func(x *httpExchange) { x.Hdr.Set("Cache-Control", "no-store") }},
		{"config.key fails", []option{withKey(`request.headers["x-tier"]`)}, func(*httpExchange) {}},
		{"no principal", nil, func(x *httpExchange) { x.Ident = &filter.Identity{Method: filter.MethodJWT} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.opts...)
			x := h.get(nil)
			tt.setup(x)
			o := h.do(x, leg{status: http.StatusOK, header: header("max-age=60", t0), body: "x"})
			if !o.forwarded || h.store.readCount() != 0 || len(h.queue.recorded()) != 0 {
				t.Errorf("forwarded %v, reads %d, writes %d; want a bypass", o.forwarded, h.store.readCount(), len(h.queue.recorded()))
			}
			if got := cacheStatus(o.header); !slices.Equal(got, []string{"ruralz;fwd=bypass"}) || o.x.CacheResult != "bypass" {
				t.Errorf("Cache-Status %q, result %q", got, o.x.CacheResult)
			}
			if got := h.count(); got != results(emit.CacheBypass, 1) {
				t.Errorf("results %v", got)
			}
		})
	}
}

// httpExchange shortens the Exchange type in tables.
type httpExchange = filtertest.Exchange

// TestPartitions checks the Partition row: config.key's result, else the
// principal of an authenticated request, else one shared partition; each
// Route has its own.
func TestPartitions(t *testing.T) {
	h := newHarness(t, withKey(`request.headers["x-tier"]`))
	h.prime(http.Header{"X-Tier": {"gold"}}, header("max-age=60", t0), "gold")
	if o := h.do(h.get(http.Header{"X-Tier": {"gold"}}), leg{}); string(o.body) != "gold" {
		t.Errorf("same key: %q", o.body)
	}
	if o := h.do(h.get(http.Header{"X-Tier": {"silver"}}), leg{status: http.StatusOK, header: header("max-age=60", t0), body: "silver"}); !o.forwarded {
		t.Error("another key shared the partition")
	}

	p := newHarness(t)
	acme := p.get(nil)
	acme.Ident = &filter.Identity{Method: filter.MethodJWT, Principal: "acme"}
	p.do(acme, leg{status: http.StatusOK, header: header("max-age=60", t0), body: "acme"})
	other := p.get(nil)
	other.Ident = &filter.Identity{Method: filter.MethodJWT, Principal: "globex"}
	if o := p.do(other, leg{status: http.StatusOK, header: header("max-age=60", t0), body: "globex"}); !o.forwarded {
		t.Error("another principal shared the partition")
	}
	anon := p.do(p.get(nil), leg{status: http.StatusOK, header: header("max-age=60", t0), body: "anon"})
	if !anon.forwarded {
		t.Error("an anonymous request shared a principal's partition")
	}
	again := p.get(nil)
	again.Ident = &filter.Identity{Method: filter.MethodJWT, Principal: "acme"}
	if o := p.do(again, leg{}); string(o.body) != "acme" {
		t.Errorf("principal partition: %q", o.body)
	}
	route := p.get(nil)
	route.RouteV.Name = "other-route"
	if o := p.do(route, leg{status: http.StatusOK, header: header("max-age=60", t0), body: "r"}); !o.forwarded {
		t.Error("another Route shared the partition")
	}
}

// TestVaryNames checks that a store learns the response's Vary names for
// the URI, so later lookups compute V under them: the same selected
// values hit, others miss with vary-miss, and a Node that learned no
// names misses once and learns the partition's.
func TestVaryNames(t *testing.T) {
	h := newHarness(t)
	vh := header("max-age=60", t0)
	vh["Vary"] = []string{"Accept-Encoding"}
	h.prime(http.Header{"Accept-Encoding": {"gzip"}}, vh, "gzip")
	w := h.queue.recorded()[0].w.Cache
	if w.Names != "accept-encoding" || w.Variant != Variant(http.Header{"Accept-Encoding": {"gzip"}}, "accept-encoding") {
		t.Errorf("store names %q, variant not under them", w.Names)
	}
	if o := h.do(h.get(http.Header{"Accept-Encoding": {"gzip"}}), leg{}); string(o.body) != "gzip" {
		t.Errorf("same selected value: %q", o.body)
	}
	o := h.do(h.get(http.Header{"Accept-Encoding": {"br"}}), leg{status: http.StatusOK, header: vh, body: "br"})
	if got := cacheStatus(o.header); !slices.Equal(got, []string{"ruralz;fwd=vary-miss;stored"}) {
		t.Errorf("other value Cache-Status %q", got)
	}
	// A second Factory has learned nothing: its first lookup computes V
	// without names, misses and learns them.
	fac, err := NewFactory(h.rv, Options{Clock: h.clk, Rand: &seqSource{}})
	if err != nil {
		t.Fatal(err)
	}
	h.fac = fac
	h.f = h.build()
	o = h.do(h.get(http.Header{"Accept-Encoding": {"gzip"}}), leg{status: http.StatusOK, header: vh, body: "gzip"})
	if got := cacheStatus(o.header); !slices.Equal(got, []string{"ruralz;fwd=vary-miss;stored"}) {
		t.Errorf("unlearned Node Cache-Status %q", got)
	}
	if o := h.do(h.get(http.Header{"Accept-Encoding": {"gzip"}}), leg{}); string(o.body) != "gzip" || !o.cached {
		t.Errorf("after learning: %q", o.body)
	}
}

// TestInvalidation checks the Invalidation row: a 2xx or 3xx leg response
// to an unsafe method bumps the URI's generation key to a fresh token in
// the invalidation class, so the stored entry misses with uri-miss and the
// next store writes under the new token; safe methods and other statuses
// bump nothing.
func TestInvalidation(t *testing.T) {
	h := newHarness(t)
	h.prime(nil, header("max-age=60", t0), "old")
	prime := h.queue.recorded()[0].w.Cache
	for _, tc := range []struct {
		method string
		status int
	}{
		{http.MethodGet, http.StatusOK}, {http.MethodHead, http.StatusOK}, {http.MethodOptions, http.StatusNoContent},
		{http.MethodTrace, http.StatusOK}, {http.MethodPost, http.StatusNotFound}, {http.MethodDelete, http.StatusInternalServerError},
		{http.MethodPut, 0},
	} {
		x := h.request(tc.method, "page=2", nil)
		x.FinalV = filter.Final{Status: tc.status, LegStatus: tc.status}
		h.f.Invalidate(t.Context(), x)
	}
	if n := len(h.queue.recorded()); n != 1 {
		t.Fatalf("%d writes after safe methods and failures, want none beyond the store", n)
	}
	post := h.request(http.MethodPost, "page=2", nil)
	post.FinalV = filter.Final{Status: 0, LegStatus: http.StatusCreated}
	h.f.Invalidate(t.Context(), post)
	ws := h.queue.recorded()
	if len(ws) != 2 || ws[1].class != statestore.ClassInvalidate || ws[1].w.Kind != statestore.OpCacheInvalidate ||
		ws[1].w.Invalidate != prime.Key.URI || ws[1].w.Bump.IsZero() || ws[1].w.Bump == prime.Generation || ws[1].w.Timeout != testTimeout {
		t.Fatalf("invalidation %+v", ws[len(ws)-1].w)
	}
	// Past the first store's 1 s fill lease.
	h.clk.Advance(2 * time.Second)
	o := h.do(h.get(nil), leg{status: http.StatusOK, header: header("max-age=60", h.clk.Now()), body: "new"})
	if got := cacheStatus(o.header); !slices.Equal(got, []string{"ruralz;fwd=uri-miss;stored"}) {
		t.Errorf("after the bump Cache-Status %q", got)
	}
	if w := h.queue.recorded()[2].w.Cache; w.Create || w.Generation != ws[1].w.Bump {
		t.Errorf("store after the bump: create %v under %x, want the bumped token %x", w.Create, w.Generation, ws[1].w.Bump)
	}
	if o := h.do(h.get(nil), leg{}); string(o.body) != "new" {
		t.Errorf("after the new store: %q", o.body)
	}
}

// TestInvalidationDropsHotToken checks that the invalidating Node forgets
// the URI's held token at once, so its own next lookup reads the new one.
func TestInvalidationDropsHotToken(t *testing.T) {
	h := newHarness(t)
	h.prime(nil, header("max-age=60", t0), "old")
	h.do(h.get(nil), leg{})
	h.do(h.get(nil), leg{})
	post := h.request(http.MethodPost, "page=2", nil)
	post.FinalV = filter.Final{LegStatus: http.StatusNoContent}
	h.f.Invalidate(t.Context(), post)
	if o := h.do(h.get(nil), leg{status: http.StatusOK, header: header("max-age=60", t0), body: "new"}); !o.forwarded {
		t.Error("the invalidating Node served the old generation from its hot-entry layer")
	}
}
