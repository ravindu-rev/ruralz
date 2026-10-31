// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// follow starts x's onRequestHeaders on its own goroutine, as a concurrent
// miss, and returns once it waits for its leader; the channel yields its
// Result.
func (h *harness) follow(x *filtertest.Exchange) <-chan filter.Result {
	h.t.Helper()
	joined := make(chan struct{}, 1)
	h.fac.onWait = func() { joined <- struct{}{} }
	out := make(chan filter.Result, 1)
	go func() { out <- h.f.Handle(context.Background(), phase.OnRequestHeaders, x) }()
	select {
	case <-joined:
	case r := <-out:
		h.t.Fatalf("the request did not follow the flight: %+v", r)
	case <-time.After(waitLimit):
		h.t.Fatal("the follower never waited")
	}
	return out
}

// result waits for a follower's Result.
func (h *harness) result(out <-chan filter.Result) filter.Result {
	h.t.Helper()
	select {
	case r := <-out:
		return r
	case <-time.After(waitLimit):
		h.t.Fatal("the follower never returned")
		return filter.Result{}
	}
}

// finish runs a served follower's onResponse and Finish.
func (h *harness) finish(x *filtertest.Exchange, r filter.Result) outcome {
	o := outcome{x: x, result: r}
	if r.Outcome == filter.Respond {
		resp := r.Response
		x.Msg = &filtertest.Message{Hdr: resp.Header, Code: resp.Status, Raw: resp.Body}
		h.f.Handle(context.Background(), phase.OnResponse, x)
		o.status, o.header, o.body, o.cached = resp.Status, resp.Header, resp.Body, resp.Cached
	}
	h.f.Finish(context.Background(), x)
	return o
}

// TestCoalescedReuse checks miss coalescing: a concurrent GET, HEAD or
// Range miss of the key follows the GET leader's flight and reuses its
// storable response like a hit, through its own response chain, recorded
// coalesced with the collapsed parameter.
func TestCoalescedReuse(t *testing.T) {
	tests := []struct {
		name   string
		method string
		req    http.Header
		status int
		body   string
	}{
		{"GET", http.MethodGet, nil, http.StatusOK, "0123456789"},
		{"HEAD", http.MethodHead, nil, http.StatusOK, "0123456789"},
		{"Range", http.MethodGet, http.Header{"Range": {"bytes=0-1"}}, http.StatusPartialContent, "01"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			lead := h.get(nil)
			if r := h.f.Handle(ctx, phase.OnRequestHeaders, lead); r.Outcome != filter.Continue {
				t.Fatalf("leader %+v", r)
			}
			fx := h.request(tt.method, "page=2", tt.req)
			out := h.follow(fx)
			var lo outcome
			h.respond(ctx, lead, leg{status: http.StatusOK, header: header("max-age=60", t0), body: "0123456789"}, &lo)
			h.f.Finish(ctx, lead)
			o := h.finish(fx, h.result(out))
			if !o.cached || o.status != tt.status || string(o.body) != tt.body {
				t.Fatalf("follower %d %q cached %v", o.status, o.body, o.cached)
			}
			if got := cacheStatus(o.header); !slices.Equal(got, []string{"ruralz;fwd=uri-miss;collapsed"}) || fx.CacheResult != "coalesced" {
				t.Errorf("follower Cache-Status %q, result %q", got, fx.CacheResult)
			}
			if o.header.Get("Age") != "0" {
				t.Errorf("follower Age %q", o.header.Get("Age"))
			}
			if got, want := h.count(), results(emit.CacheMiss, 1, emit.CacheCoalesced, 1); got != want {
				t.Errorf("results %v, want %v", got, want)
			}
			if n := len(h.queue.recorded()); n != 1 {
				t.Errorf("%d stores, want the leader's only", n)
			}
		})
	}
}

// TestCoalescedFetchAlone checks the followers that fetch alone: other
// Vary-selected values than the leader's, or a leader that stores
// nothing.
func TestCoalescedFetchAlone(t *testing.T) {
	vary := header("max-age=60", t0)
	vary["Vary"] = []string{"Accept-Encoding"}
	tests := []struct {
		name string
		req  http.Header
		l    leg
	}{
		{"other selected values", http.Header{"Accept-Encoding": {"br"}}, leg{status: http.StatusOK, header: vary, body: "gzip"}},
		{"leader not storable", nil, leg{status: http.StatusOK, header: header("no-store", t0), body: "x"}},
		{"leader failed", nil, leg{failed: http.StatusBadGateway}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			lead := h.get(http.Header{"Accept-Encoding": {"gzip"}})
			h.f.Handle(ctx, phase.OnRequestHeaders, lead)
			fx := h.get(tt.req)
			out := h.follow(fx)
			var lo outcome
			h.respond(ctx, lead, tt.l, &lo)
			h.f.Finish(ctx, lead)
			r := h.result(out)
			if r.Outcome != filter.Continue {
				t.Fatalf("follower %+v, want a fetch of its own", r)
			}
			var fo outcome
			h.respond(ctx, fx, leg{status: http.StatusOK, header: header("no-store", t0)}, &fo)
			h.f.Finish(ctx, fx)
			if fx.CacheResult != "miss" {
				t.Errorf("result %q, want miss", fx.CacheResult)
			}
		})
	}
}

// TestCoalescingRoles checks who takes part in a flight: a HEAD or Range
// miss never leads one, and a request asking for revalidation neither
// leads nor follows.
func TestCoalescingRoles(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, x := range []*filtertest.Exchange{
		h.request(http.MethodHead, "page=2", nil),
		h.get(http.Header{"Range": {"bytes=0-1"}}),
		h.get(http.Header{"Cache-Control": {"no-cache"}}),
	} {
		if r := h.f.Handle(ctx, phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
			t.Fatalf("miss %+v", r)
		}
		if n := h.fac.flights.Len(); n != 0 {
			t.Errorf("%s %v led a flight", x.MethodV, x.Hdr)
		}
		h.f.Finish(ctx, x)
	}
	lead := h.get(nil)
	h.f.Handle(ctx, phase.OnRequestHeaders, lead)
	nc := h.get(http.Header{"Cache-Control": {"no-cache"}})
	if r := h.f.Handle(ctx, phase.OnRequestHeaders, nc); r.Outcome != filter.Continue {
		t.Fatalf("no-cache miss %+v", r)
	}
	h.f.Finish(ctx, nc)
	h.f.Finish(ctx, lead)
	if n := h.fac.flights.Len(); n != 0 {
		t.Errorf("%d flights left after the leader's Finish", n)
	}
}

// TestLookupFailure checks a failed lookup: the RZ-STS failure under the
// Policy's failureMode, recorded as a bypass.
func TestLookupFailure(t *testing.T) {
	h := newHarness(t)
	h.store.readErr = statestore.ErrTimeout
	x := h.get(nil)
	r := h.f.Handle(context.Background(), phase.OnRequestHeaders, x)
	if r.Outcome != filter.CannotDecide || r.Code != "RZ-STS-001" || !errors.Is(r.Err, statestore.ErrTimeout) {
		t.Fatalf("Result %+v, want cannot decide with RZ-STS-001", r)
	}
	h.f.Finish(context.Background(), x)
	if x.CacheResult != "bypass" || h.count() != results(emit.CacheBypass, 1) {
		t.Errorf("result %q, counts %v; want one bypass", x.CacheResult, h.count())
	}
}

// TestHotLayerSkipsReads checks rule 1 on the request path: once a URI's
// token and entry were looked up twice within 1 s, lookups skip the State
// Store until the hold ends.
func TestHotLayerSkipsReads(t *testing.T) {
	h := newHarness(t)
	h.prime(nil, header("max-age=60", t0), "x")
	base := h.store.readCount()
	for range 4 {
		if o := h.do(h.get(nil), leg{}); string(o.body) != "x" {
			t.Fatalf("hit %q", o.body)
		}
	}
	if n := h.store.readCount() - base; n != 1 {
		t.Errorf("%d reads for 4 lookups within 1 s, want 1 (the priming miss read once already)", n)
	}
	h.clk.Advance(time.Second)
	h.do(h.get(nil), leg{})
	if n := h.store.readCount() - base; n != 2 {
		t.Errorf("%d reads after the hold ended, want 2", n)
	}
}

// TestBuild checks Build's refusals: a config of another type
// (RZ-CFG-005), another scope or Phase set (RZ-CFG-020), a config.key that
// does not compile (RZ-CFG-014), no State Store or queue; and its
// fallback to the main State Store and queue.
func TestBuild(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name   string
		change func(env *filter.BuildEnv)
		code   string
		err    error
	}{
		{"other config", func(env *filter.BuildEnv) { env.Config = &v1alpha1.CORSConfig{} }, CodeConfig, nil},
		{"Gateway scope", func(env *filter.BuildEnv) { env.Scope = phase.ScopeGateway }, CodeScope, nil},
		{"no onResponse", func(env *filter.BuildEnv) { env.Phases = phase.Of(phase.OnRequestHeaders) }, CodeScope, nil},
		{"bad config.key", func(env *filter.BuildEnv) { env.Config = &v1alpha1.CacheConfig{Key: "request.nope +"} }, CodeCEL, nil},
		{"config.key without CEL", func(env *filter.BuildEnv) {
			env.Config, env.CEL = &v1alpha1.CacheConfig{Key: "route.name"}, nil
		}, "", ErrNoCEL},
		{"no store", func(env *filter.BuildEnv) { env.CacheStore = nil }, "", ErrNoStore},
		{"no queue", func(env *filter.BuildEnv) { env.CacheEnqueuer = nil }, "", ErrNoQueue},
	}
	for _, tt := range tests {
		env := h.env()
		tt.change(&env)
		_, err := h.fac.Build(context.Background(), env)
		if code, _ := errcode.CodeOf(err); tt.code != "" && code != tt.code {
			t.Errorf("%s: Build = %v, want %s", tt.name, err, tt.code)
		}
		if tt.err != nil && !errors.Is(err, tt.err) {
			t.Errorf("%s: Build = %v, want %v", tt.name, err, tt.err)
		}
	}
	env := h.env()
	env.CacheStore, env.CacheEnqueuer, env.StateStore, env.Enqueuer = nil, nil, h.store, h.queue
	flt, err := h.fac.Build(context.Background(), env)
	if err != nil || flt.(*Filter).store != h.store || flt.(*Filter).queue != h.queue {
		t.Errorf("fallback Build = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.fac.Build(ctx, h.env()); !errors.Is(err, context.Canceled) {
		t.Errorf("Build with an ended context = %v", err)
	}
	if _, err := NewFactory(nil, Options{}); !errors.Is(err, ErrNoRevalidator) {
		t.Errorf("NewFactory(nil) = %v", err)
	}
}

// TestNewToken checks that generation tokens are drawn fresh and never
// zero.
func TestNewToken(t *testing.T) {
	h := newHarness(t)
	a, b := h.fac.newToken(), h.fac.newToken()
	if a.IsZero() || b.IsZero() || a == b {
		t.Errorf("tokens %x and %x", a, b)
	}
}
