// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Leg handles: a leg's memory is pooled and reused, its handle never is,
// so nothing done through a released handle reaches the memory's next
// owner.

// TestStaleLegHandleIsInert checks that a released leg handle, and the
// view and messages taken from it, do nothing once the leg's memory serves
// another leg: a second Release, and every call a stale owner still makes,
// leave the new owner's leg, slots, decisions, records and outgoing request
// untouched, and each owner's records merge into the request once.
func TestStaleLegHandleIsInert(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	ctx := context.Background()
	p := legPolicy()

	first := x.Leg("orders", "a").(*legHandle)
	first.RecordFailure(p, phase.OnUpstreamRequest, v1alpha1.FailureModeOpen)
	staleView := first.Exchange()
	mem := first.l
	first.Release()

	// The pool may hand the same memory to the next leg; issue it so.
	second := f.pool.issue(x, mem, "orders", "b")
	out := &http.Request{Header: http.Header{}, URL: &url.URL{RawQuery: "q=owner"}, Body: http.NoBody}
	second.SetLeg(&filter.Leg{Upstream: "orders", Step: "b", Attempt: 1, Request: out})
	second.Enter(p, phase.OnUpstreamRequest, nil)
	*second.Exchange().PolicyState() = "owner"
	second.SetWhen(p, false)
	span := &recSpan{}
	second.Enter(p, phase.OnUpstreamRequest, span)

	// Everything the first owner still does through its handle and view.
	first.Release()
	first.Enter(p, phase.OnUpstreamResponseBody, nil)
	first.SetLeg(&filter.Leg{Upstream: "orders", Attempt: 9})
	first.SetWhen(p, true)
	first.RecordFailure(p, phase.OnUpstreamRequest, v1alpha1.FailureModeClosed)
	first.RecordShortCircuit(p, phase.OnUpstreamRequest, http.StatusForbidden)
	*staleView.PolicyState() = "stale"
	staleView.Annotate("stale", slog.BoolValue(true))
	staleView.Message().Header().Set("X-Stale", "1")
	staleView.Message().SetRawQuery("q=stale")
	if err := staleView.Message().SetBody(ctx, []byte("stale")); !errors.Is(err, filter.ErrNotAvailable) {
		t.Errorf("stale SetBody() = %v, want ErrNotAvailable", err)
	}
	if err := staleView.Reserve(1); !errors.Is(err, filter.ErrNotAvailable) {
		t.Errorf("stale Reserve() = %v, want ErrNotAvailable", err)
	}
	staleView.Release(1)
	if d, _ := first.When(p); d {
		t.Error("a stale handle reads the new owner's decision")
	}
	if first.Leg("other", "c") != first {
		t.Error("Leg on a stale handle issued a leg")
	}
	if staleView.Path() != "" || staleView.Leg() != nil || staleView.Vars().Request != nil || staleView.RouteMetrics() == nil {
		t.Error("a stale view reads the request")
	}

	if mem.parent != x || mem.step != "b" || mem.attempt == nil || mem.attempt.Attempt != 1 || mem.ph != phase.OnUpstreamRequest {
		t.Fatalf("the new owner's leg changed: parent %p step %q phase %s", mem.parent, mem.step, mem.ph)
	}
	if got := *second.Exchange().PolicyState(); got != "owner" {
		t.Errorf("new owner's slot = %v, want owner", got)
	}
	if d, s := second.When(p); !d || s {
		t.Errorf("new owner's decision = %v, %v, want running", d, s)
	}
	if out.Header.Get("X-Stale") != "" || out.URL.RawQuery != "q=owner" || mem.rewritten {
		t.Error("a stale message wrote into the new owner's outgoing request")
	}
	if span.attr("stale") != "" || mem.rec.nfails != 0 || mem.rec.sc != nil || mem.acct.Charged() != 0 {
		t.Error("a stale handle reached the new owner's span, records or account")
	}
	second.RecordFailure(p, phase.OnUpstreamRequest, v1alpha1.FailureModeOpen)
	second.Release()
	second.Release()
	if fm := x.FailureModes(); len(fm) != 2 || fm[0].Mode != "open" || fm[1].Mode != "open" {
		t.Errorf("FailureModes() = %+v, want each owner's one failure once", fm)
	}
}

// TestLegReleaseRacesReuse checks that releases racing the reuse of leg
// memory each end their own leg once (run under -race): goroutines
// release their legs twice while others take legs from the pool, and every
// leg's record merges into the request exactly once.
func TestLegReleaseRacesReuse(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	p := legPolicy()
	const workers, legs = 8, 50
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range legs {
				h := x.Leg("u", fmt.Sprint("s", w, "-", i))
				h.SetLeg(&filter.Leg{Upstream: "u", Attempt: 1})
				h.Enter(p, phase.OnUpstreamRequest, nil)
				*h.Exchange().PolicyState() = w
				h.RecordFailure(p, phase.OnUpstreamRequest, v1alpha1.FailureModeOpen)
				if got := *h.Exchange().PolicyState(); got != w {
					t.Errorf("worker %d reads slot %v: another owner reached its leg", w, got)
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					h.Release() // a release racing the owner's own
				}()
				h.Release()
				<-done
				h.Release() // and a stale one after the memory is reused
			}
		})
	}
	wg.Wait()
	x.rec.mu.Lock()
	n := x.rec.nfails
	x.rec.mu.Unlock()
	if n != workers*legs {
		t.Errorf("merged %d leg records, want %d: a leg was released twice or not at all", n, workers*legs)
	}
}

// TestPutEndsLiveLegs checks the request's put-back: Put releases every leg
// still live, returning its reservations, so its handle and view do nothing
// afterwards; a Release after the put-back merges nothing into the request,
// even once its memory serves another request; and a leg released before
// Put merged as usual.
func TestPutEndsLiveLegs(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	p := legPolicy()
	ended := x.Leg("orders", "")
	ended.RecordFailure(p, phase.OnUpstreamRequest, v1alpha1.FailureModeOpen)
	ended.Release()
	live := x.Leg("orders", "step").(*legHandle)
	view := live.Exchange()
	if err := view.Reserve(100); err != nil {
		t.Fatal(err)
	}
	live.RecordFailure(p, phase.OnUpstreamRequest, v1alpha1.FailureModeClosed)
	if f.bud.Used() == 0 {
		t.Fatal("the live leg reserved nothing")
	}
	if fm := x.FailureModes(); len(fm) != 1 {
		t.Fatalf("FailureModes() before Put = %+v, want the released leg's", fm)
	}

	f.pool.Put(x)
	if used := f.bud.Used(); used != 0 {
		t.Errorf("budget %d after Put, want the live leg's reservation returned", used)
	}
	if !live.done.Load() || live.l.parent != nil {
		t.Error("Put left the live leg running")
	}
	if err := view.Reserve(1); !errors.Is(err, filter.ErrNotAvailable) {
		t.Errorf("Reserve() after Put = %v, want ErrNotAvailable", err)
	}

	// The same memory serves the next request, as Get would hand it out.
	x.live.Store(true)
	x.Begin(serverRequest(http.MethodGet, "/next", "", nil), start("/next"))
	defer f.pool.Put(x)
	live.Release()
	live.RecordFailure(p, phase.OnUpstreamRequest, v1alpha1.FailureModeClosed)
	if fm := x.FailureModes(); len(fm) != 0 {
		t.Errorf("FailureModes() of the next request = %+v, want none", fm)
	}
	if len(x.legs) != 0 {
		t.Errorf("the next request lists %d legs of the previous one", len(x.legs))
	}
	if h := x.Leg("orders", ""); h.(*legHandle).done.Load() {
		t.Error("the next request issued a released leg")
	}
}

// TestLegAfterPutIsReleased checks that a Leg call on a request already put
// back returns a released handle, which never reaches pooled memory.
func TestLegAfterPutIsReleased(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	f.pool.Put(x)
	h := x.Leg("orders", "").(*legHandle)
	if !h.done.Load() {
		t.Fatal("Leg after Put returned a live handle")
	}
	h.Release()
	if h.Exchange().Path() != "" || h.Budget() == nil {
		t.Error("a released handle reads a request")
	}
}

// TestLegReleaseRacesPut checks a leg Release racing its request's Put (run
// under -race): exactly one of them ends the leg, the reservations come
// back, and nothing writes into the request after it is put back.
func TestLegReleaseRacesPut(t *testing.T) {
	f := newFixture(t)
	p := legPolicy()
	for i := range 50 {
		x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
		h := x.Leg("orders", fmt.Sprint(i))
		if err := h.Exchange().Reserve(10); err != nil {
			t.Fatal(err)
		}
		h.RecordFailure(p, phase.OnUpstreamRequest, v1alpha1.FailureModeOpen)
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.Release()
		}()
		f.pool.Put(x)
		<-done
		if used := f.bud.Used(); used != 0 {
			t.Fatalf("round %d: budget %d after the race, want 0", i, used)
		}
	}
}

// TestLegHandlesAmortized checks the allocation cost of fresh leg handles
// (PB-8): handles come from pooled chunks, one allocation per handleChunk
// legs, so a request with one leg rounds to none.
func TestLegHandlesAmortized(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items under -race; the allocation gate runs without it")
	}
	f := newFixture(t)
	r := serverRequest(http.MethodGet, "/", "", nil)
	st := start("/")
	request := func() {
		x := f.pool.Get()
		x.Begin(r, st)
		for range handleChunk {
			x.Leg("orders", "").Release()
		}
		f.pool.Put(x)
	}
	request() // warm the pools and the request's leg list
	if n := testing.AllocsPerRun(20, request); n > 1 {
		t.Errorf("%v allocations per %d legs, want at most one chunk", n, handleChunk)
	}
}
