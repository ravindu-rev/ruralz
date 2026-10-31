// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestAggregateMerges checks the aggregate response
// (docs/architecture/03-data-plane.md "Merge rules"): status 200 with
// Content-Type: application/json and Content-Length; a step with group
// merges under it and one without member by member at the top level;
// members are written in step list order, then in each body's order, a
// later step winning a key at the key's first position; numbers keep their
// text and nothing is HTML-escaped.
func TestAggregateMerges(t *testing.T) {
	f := newFixture(t, "orders", "stock", "prices")
	f.leg("orders").fn = answer(func() *snapshot.UpstreamResponse {
		return jsonResp(200, `{"id":"o1","total":1.50,"note":"<b>&</b>"}`)
	})
	f.leg("stock").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(200, `{"count":7}`) })
	f.leg("prices").fn = answer(func() *snapshot.UpstreamResponse {
		return jsonResp(200, `{"total":2e3,"currency":"EUR"}`)
	})
	stock := stepTo("stock", "stock", "/stock")
	stock.Group = "stock"
	comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("orders", "orders", "/o"), stock, stepTo("prices", "prices", "/p")))
	resp, err := f.forward(comp)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"o1","total":2e3,"note":"<b>&</b>","stock":{"count":7},"currency":"EUR"}`
	if got := read(t, resp); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
	if resp.Status != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Content-Length") != strconv.Itoa(len(want)) || resp.Partial {
		t.Errorf("response = %d %v partial %v", resp.Status, resp.Header, resp.Partial)
	}
	if resp.Header.Get("Ruralz-Partial") != "" {
		t.Errorf("Ruralz-Partial = %q on a complete response", resp.Header.Get("Ruralz-Partial"))
	}
}

// TestAggregateRunsInParallel checks that aggregate steps run at once, one
// goroutine each: every leg is called before any returns.
func TestAggregateRunsInParallel(t *testing.T) {
	f := newFixture(t, "a", "b", "c")
	var started atomic.Int32
	release := make(chan struct{})
	for _, name := range []string{"a", "b", "c"} {
		f.leg(name).fn = func(ctx context.Context, _ upstream.Target) (*snapshot.UpstreamResponse, error) {
			if started.Add(1) == 3 {
				close(release)
			}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return jsonResp(200, `{}`), nil
		}
	}
	comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("a", "a", "/a"), stepTo("b", "b", "/b"), stepTo("c", "c", "/c")))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	resp, err := comp.Forward(ctx, f.outbound(exchange(http.MethodGet, "/v1", ""), ""))
	if err != nil {
		t.Fatalf("Forward = %v: the steps did not run at once", err)
	}
	read(t, resp)
}

// TestAggregateUnits checks the in-flight units of an aggregate: one per
// step, taken before the first step starts and returned when each step
// ends; a ceiling without room for them is 503 RZ-RT-005 before any step
// runs.
func TestAggregateUnits(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.units = admission.NewUnits(3)
	f.e.units = f.units
	// Both steps wait for each other, so the sample sees every unit the
	// aggregate holds.
	var held atomic.Int64
	var started atomic.Int32
	both := make(chan struct{})
	for _, n := range []string{"a", "b"} {
		f.leg(n).fn = func(context.Context, upstream.Target) (*snapshot.UpstreamResponse, error) {
			if started.Add(1) == 2 {
				held.Store(f.units.InUse())
				close(both)
			}
			<-both
			return jsonResp(200, `{}`), nil
		}
	}
	comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("a", "a", "/a"), stepTo("b", "b", "/b")))
	resp, err := f.forward(comp)
	if err != nil {
		t.Fatal(err)
	}
	read(t, resp)
	if got := held.Load(); got != 2 {
		t.Errorf("units held while a step ran = %d, want one per step", got)
	}
	if got := f.units.InUse(); got != 0 {
		t.Errorf("units held after Forward = %d, want 0", got)
	}
	if !f.units.TryAcquire(2) {
		t.Fatal("TryAcquire(2) failed")
	}
	_, err = f.forward(comp)
	if codeOf(err) != admission.CodeFull || stepOf(err) != "" {
		t.Errorf("Forward with a full ceiling = %v, want RZ-RT-005", err)
	}
	if n := len(f.leg("b").list()); n != 1 {
		t.Errorf("leg b ran %d times, want only the first request's", n)
	}
}

// TestAggregateFirstFailureDecides checks that a failed step that is not
// optional cancels its siblings through their shared context, and that
// the failure those cancellations cause reports nothing.
func TestAggregateFirstFailureDecides(t *testing.T) {
	f := newFixture(t, "slow", "bad")
	canceled := make(chan struct{})
	f.leg("slow").fn = func(ctx context.Context, _ upstream.Target) (*snapshot.UpstreamResponse, error) {
		<-ctx.Done()
		close(canceled)
		return nil, &upstream.LegError{Upstream: "slow", Attempts: 1, Err: codedErr("RZ-UP-003")}
	}
	f.leg("bad").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(500, `{}`) })
	comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("slow", "slow", "/s"), stepTo("bad", "bad", "/b")))
	resp, err := f.forward(comp)
	if resp != nil || codeOf(err) != CodeStepStatus || stepOf(err) != "bad" {
		t.Errorf("Forward = %v, %v; want RZ-UP-011 from step bad", resp, err)
	}
	select {
	case <-canceled:
	default:
		t.Error("the sibling was not canceled")
	}
}

// TestAggregateTiesGoToListOrder checks that of two failures at the same
// time, the earlier step in list order decides, whichever finished first.
func TestAggregateTiesGoToListOrder(t *testing.T) {
	for range 20 {
		f := newFixture(t, "a", "b")
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse {
			return &snapshot.UpstreamResponse{Status: 401, Generated: &filter.Response{Status: 401, Code: "RZ-AUTH-020"}}
		})
		f.leg("b").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(503, `{}`) })
		comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("a", "a", "/a"), stepTo("b", "b", "/b")))
		resp, err := f.forward(comp)
		if err != nil || resp == nil || resp.Generated == nil || resp.Generated.Code != "RZ-AUTH-020" || resp.Status != 401 {
			t.Fatalf("Forward = %+v, %v; want step a's generated 401 RZ-AUTH-020", resp, err)
		}
	}
}

// TestAggregateEarlierInTimeDecides checks that the first step to fail in
// time decides over a later failure of an earlier step in list order.
func TestAggregateEarlierInTimeDecides(t *testing.T) {
	f := newFixture(t, "a", "b")
	// a answers only once b's failure canceled it, after the clock moved:
	// its 500 is a failure of its own, later in time.
	f.leg("a").fn = func(ctx context.Context, _ upstream.Target) (*snapshot.UpstreamResponse, error) {
		<-ctx.Done()
		f.clk.Advance(time.Second)
		return jsonResp(500, `{}`), nil
	}
	f.leg("b").fn = func(context.Context, upstream.Target) (*snapshot.UpstreamResponse, error) {
		return nil, &upstream.LegError{Upstream: "b", Attempts: 1, Err: codedErr("RZ-UP-001")}
	}
	comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("a", "a", "/a"), stepTo("b", "b", "/b")))
	_, err := f.forward(comp)
	if codeOf(err) != "RZ-UP-001" || stepOf(err) != "b" {
		t.Errorf("Forward = %v, want step b's RZ-UP-001, which failed first", err)
	}
}

// TestAggregateOptional checks failed optional steps: each is left out and
// the response carries ruralz-partial: true; when every step failed
// optionally the answer is 200 {} with ruralz-partial: true and
// Cache-Control: no-store.
func TestAggregateOptional(t *testing.T) {
	t.Run("one left out", func(t *testing.T) {
		f := newFixture(t, "a", "b")
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(200, `{"a":1}`) })
		f.leg("b").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(502, `{}`) })
		comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("a", "a", "/a"), optional(stepTo("b", "b", "/b"))))
		resp, err := f.forward(comp)
		if err != nil {
			t.Fatal(err)
		}
		if got := read(t, resp); got != `{"a":1}` || !resp.Partial || resp.Header.Get("Ruralz-Partial") != "true" {
			t.Errorf("response = %s partial %v %v, want {\"a\":1} partial", got, resp.Partial, resp.Header)
		}
	})
	t.Run("every step left out", func(t *testing.T) {
		f := newFixture(t, "a", "b")
		for _, n := range []string{"a", "b"} {
			f.leg(n).fn = func(context.Context, upstream.Target) (*snapshot.UpstreamResponse, error) {
				return nil, &upstream.LegError{Upstream: n, Err: codedErr("RZ-UP-008")}
			}
		}
		comp := f.compile(composition(v1alpha1.CompositionModeAggregate, optional(stepTo("a", "a", "/a")), optional(stepTo("b", "b", "/b"))))
		resp, err := f.forward(comp)
		if err != nil {
			t.Fatal(err)
		}
		if got := read(t, resp); got != `{}` || resp.Status != 200 || !resp.Partial || resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("response = %d %s %v, want 200 {} partial no-store", resp.Status, got, resp.Header)
		}
	})
}

// TestStepPanicRecovered checks that a fault in a step, on an aggregate
// step's goroutine or the request goroutine, is recovered as the step's
// failure with 502 RZ-RT-015 and logged.
func TestStepPanicRecovered(t *testing.T) {
	for _, mode := range []v1alpha1.CompositionMode{v1alpha1.CompositionModeAggregate, v1alpha1.CompositionModeSequential} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, "a")
			var logged strings.Builder
			f.e.log = newTextLogger(&logged)
			f.leg("a").fn = func(context.Context, upstream.Target) (*snapshot.UpstreamResponse, error) {
				panic("step exploded")
			}
			comp := f.compile(composition(mode, stepTo("a", "a", "/a")))
			_, err := f.forward(comp)
			if codeOf(err) != CodeStepFailed || !errors.Is(err, errPanic) {
				t.Errorf("Forward = %v, want RZ-RT-015 for a recovered fault", err)
			}
			if out := logged.String(); !strings.Contains(out, "composition step panic recovered") || !strings.Contains(out, "panic_type=string") || strings.Contains(out, "step exploded") {
				t.Errorf("log = %q, want the fault with its type and no panic text", out)
			}
		})
	}
}

// TestLastStepThatRan checks the access log fields: upstream, endpoint and
// attempts describe the last step in list order whose leg ran, on a merged
// response and on a failure.
func TestLastStepThatRan(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.leg("a").fn = answer(func() *snapshot.UpstreamResponse {
		r := jsonResp(200, `{"a":1}`)
		r.Endpoint, r.Attempts = "10.0.0.1:80", 1
		return r
	})
	f.leg("b").fn = answer(func() *snapshot.UpstreamResponse {
		r := jsonResp(200, `{"b":1}`)
		r.Endpoint, r.Attempts = "10.0.0.2:80", 2
		return r
	})
	comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("a", "a", "/a"), stepTo("b", "b", "/b")))
	resp, err := f.forward(comp)
	if err != nil {
		t.Fatal(err)
	}
	read(t, resp)
	if resp.Upstream != "b" || resp.Endpoint != "10.0.0.2:80" || resp.Attempts != 2 {
		t.Errorf("response describes %s %s %d, want b 10.0.0.2:80 2", resp.Upstream, resp.Endpoint, resp.Attempts)
	}
	f.leg("b").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(404, `{}`) })
	_, err = f.forward(comp)
	var ce *Error
	if !errors.As(err, &ce) || ce.Upstream != "b" || ce.Step != "b" {
		t.Errorf("Forward = %+v, want the failure to describe step b", err)
	}
}

// TestResourcesReleased checks that no buffer budget is held once the
// response body is closed, and that every step's leg body is closed.
func TestResourcesReleased(t *testing.T) {
	f := newFixture(t, "a", "b")
	bodies := map[string]*trackedBody{}
	for _, n := range []string{"a", "b"} {
		r := jsonResp(200, `{"`+n+`":[1,2,3]}`)
		bodies[n] = r.Body.(*trackedBody)
		f.leg(n).fn = func(context.Context, upstream.Target) (*snapshot.UpstreamResponse, error) { return r, nil }
	}
	comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("a", "a", "/a"), stepTo("b", "b", "/b")))
	resp, err := f.forward(comp)
	if err != nil {
		t.Fatal(err)
	}
	if f.budget.Used() == 0 {
		t.Error("the merged body holds no reservation before it is closed")
	}
	read(t, resp)
	if got := f.budget.Used(); got != 0 {
		t.Errorf("budget used after Close = %d, want 0", got)
	}
	for n, b := range bodies {
		if !b.closed.Load() {
			t.Errorf("leg body of %s not closed", n)
		}
	}
}
