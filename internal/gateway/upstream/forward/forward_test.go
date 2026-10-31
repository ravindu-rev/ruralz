// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
)

// shares runs n requests through f and returns the share of responses
// each entry served, by entry index.
func shares(t *testing.T, f *Forwarder, entries, n int) []float64 {
	t.Helper()
	got := make([]float64, entries)
	byName := map[string]int{}
	for i := range entries {
		byName[fmt.Sprint("e", i)] = i
	}
	o := outbound(nil)
	for range n {
		r, err := f.Forward(t.Context(), o)
		if err != nil {
			t.Fatalf("Forward: %v", err)
		}
		got[byName[r.Upstream]]++
	}
	for i := range got {
		got[i] /= float64(n)
	}
	return got
}

// within fails when a share is off its want by more than 0.01.
func within(t *testing.T, got, want []float64) {
	t.Helper()
	for i := range want {
		if math.Abs(got[i]-want[i]) > 0.01 {
			t.Fatalf("shares = %.4f, want %.4f", got, want)
		}
	}
}

// legsFor returns serving legs e0, e1, ... for weights.
func legsFor(weights []uint32) []*fakeLeg {
	legs := make([]*fakeLeg, len(weights))
	for i := range weights {
		legs[i] = serving(fmt.Sprint("e", i))
	}
	return legs
}

// TestForwardPicksByWeight checks the per-request pick of an upstreams
// entry by weight (docs/architecture/09-traffic-management-and-resilience.md
// "Load balancing"): shares follow the weights, and an entry of weight 0
// is never picked while another weighs more.
func TestForwardPicksByWeight(t *testing.T) {
	tests := []struct {
		name    string
		weights []uint32
		want    []float64
	}{
		{"95/5 split", []uint32{95, 5}, []float64{0.95, 0.05}},
		{"blue-green", []uint32{0, 1}, []float64{0, 1}},
		{"canary three", []uint32{80, 0, 15, 5}, []float64{0.80, 0, 0.15, 0.05}},
		{"many entries", []uint32{1, 2, 3, 4, 0, 5, 6, 7, 8, 9, 10}, []float64{1. / 55, 2. / 55, 3. / 55, 4. / 55, 0, 5. / 55, 6. / 55, 7. / 55, 8. / 55, 9. / 55, 10. / 55}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legs := legsFor(tt.weights)
			f := forwarder(legs, tt.weights, pcg(11))
			within(t, shares(t, f, len(tt.weights), 100000), tt.want)
			for i, w := range tt.weights {
				if w == 0 && legs[i].calls.Load() != 0 {
					t.Errorf("weight-0 entry %d ran %d legs", i, legs[i].calls.Load())
				}
			}
		})
	}
}

// TestForwardAllZeroWeightsWeighOne checks the weight rule's second half:
// when every entry weighs 0 they all weigh 1, so each serves an equal
// share and no request is refused.
func TestForwardAllZeroWeightsWeighOne(t *testing.T) {
	weights := []uint32{0, 0, 0}
	f := forwarder(legsFor(weights), weights, pcg(5))
	within(t, shares(t, f, 3, 90000), []float64{1. / 3, 1. / 3, 1. / 3})
}

// TestForwardRepicksAfterRefusal checks the pick again: a leg refused
// before any attempt is sent, 503 RZ-UP-005, RZ-UP-006 or RZ-UP-008, is
// followed by a leg to another entry, whose response answers.
func TestForwardRepicksAfterRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate resilience.Gate
		code string
	}{
		{"breaker open", resilience.GateBreaker, "RZ-UP-005"},
		{"bulkhead full", resilience.GateBulkhead, "RZ-UP-006"},
		{"no Endpoint", resilience.GateNoEndpoint, "RZ-UP-008"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := refusing("a", tc.gate), serving("b")
			if code := codeOf(a.err); code != tc.code {
				t.Fatalf("refusal code = %s, want %s", code, tc.code)
			}
			// a weighs more, so it is always picked first.
			f := forwarder([]*fakeLeg{a, b}, []uint32{1, 0}, noDraw{t})
			r, err := f.Forward(t.Context(), outbound(nil))
			if err != nil {
				t.Fatalf("Forward: %v", err)
			}
			if r.Upstream != "b" || a.calls.Load() != 1 || b.calls.Load() != 1 {
				t.Errorf("Forward = %s after a:%d b:%d legs, want b after one leg each", r.Upstream, a.calls.Load(), b.calls.Load())
			}
		})
	}
}

// TestForwardLastRefusal checks that every entry is tried at most once and
// that the request answers with the last refusal when none is left; the
// weighted entries go first, the weight-0 one last.
func TestForwardLastRefusal(t *testing.T) {
	log := &callLog{}
	legs := []*fakeLeg{
		refusing("w1", resilience.GateBreaker),
		refusing("w2", resilience.GateBulkhead),
		refusing("zero", resilience.GateNoEndpoint),
		refusing("w3", resilience.GateBreaker),
	}
	for _, l := range legs {
		l.log = log
	}
	f := forwarder(legs, []uint32{1, 2, 0, 3}, pcg(9))
	for range 200 {
		log.reset()
		_, err := f.Forward(t.Context(), outbound(nil))
		order := log.list()
		if len(order) != 4 || order[3] != "zero" {
			t.Fatalf("legs ran %v, want each entry once, zero last", order)
		}
		sorted := slices.Sorted(slices.Values(order))
		if !slices.Equal(sorted, []string{"w1", "w2", "w3", "zero"}) {
			t.Fatalf("legs ran %v, want every entry once", order)
		}
		var le *upstream.LegError
		if !errors.As(err, &le) || le.Upstream != "zero" || codeOf(err) != "RZ-UP-008" {
			t.Fatalf("Forward = %v, want the last refusal, zero's RZ-UP-008", err)
		}
	}
	for _, l := range legs {
		if n := l.calls.Load(); n != 200 {
			t.Errorf("entry %s ran %d legs over 200 requests, want 200", l.name, n)
		}
	}
}

// TestForwardRepickByWeightAmongUntried checks that the pick again weighs
// the entries not tried: with the heaviest entry always refused, the
// others serve by their own weights' shares, 1:3, plus their first picks.
func TestForwardRepickByWeightAmongUntried(t *testing.T) {
	legs := []*fakeLeg{serving("e0"), serving("e1"), refusing("e2", resilience.GateBreaker)}
	f := forwarder(legs, []uint32{1, 3, 6}, pcg(17))
	// e0: first pick 0.1, or e2 first (0.6) then e0 among {e0, e1} (0.25).
	within(t, shares(t, f, 3, 100000), []float64{0.1 + 0.6*0.25, 0.3 + 0.6*0.75, 0})
}

// TestForwardWeightZeroIsLastResort checks a weight-0 entry: never picked
// while a weighted entry is left, and tried once every weighted entry was
// refused.
func TestForwardWeightZeroIsLastResort(t *testing.T) {
	standby := serving("standby")
	live := refusing("live", resilience.GateBreaker)
	f := forwarder([]*fakeLeg{standby, live}, []uint32{0, 1}, noDraw{t})
	r, err := f.Forward(t.Context(), outbound(nil))
	if err != nil || r.Upstream != "standby" {
		t.Fatalf("Forward = %+v, %v; want the standby entry", r, err)
	}
	live.err = nil
	live.resp = &snapshot.UpstreamResponse{Status: http.StatusOK, Upstream: "live"}
	for range 100 {
		if r, err := f.Forward(t.Context(), outbound(nil)); err != nil || r.Upstream != "live" {
			t.Fatalf("Forward with the live entry serving = %+v, %v; want live", r, err)
		}
	}
	if n := standby.calls.Load(); n != 1 {
		t.Errorf("standby ran %d legs, want 1", n)
	}
}

// TestForwardFinalOutcomes checks that only a refusal before any attempt
// picks again: a leg error after an attempt was sent (a retry attempt the
// breaker, bulkhead or Endpoint set refused included), the Route deadline,
// a canceled leg, an error that is no leg error, an Upstream response and
// a leg Filter's generated response are returned as they are, and no
// other entry runs.
func TestForwardFinalOutcomes(t *testing.T) {
	connect := &upstream.LegError{
		Upstream: "a", Endpoint: "10.0.0.1:80", Attempts: 1, Kind: resilience.KindConnect,
		Err: resilience.Outcome{Kind: resilience.KindConnect, Attempts: 1}.Err(errors.New("connection refused")),
	}
	tests := []struct {
		name string
		resp *snapshot.UpstreamResponse
		err  error
		code string
	}{
		{name: "connect error", err: connect, code: "RZ-UP-001"},
		{name: "retries failed", err: &upstream.LegError{
			Upstream: "a", Attempts: 2, Kind: resilience.KindReset,
			Err: resilience.Outcome{Kind: resilience.KindReset, Attempts: 2}.Err(errors.New("reset")),
		}, code: "RZ-UP-007"},
		{name: "attempt timeout", err: &upstream.LegError{
			Upstream: "a", Attempts: 1, Kind: resilience.KindTimeout,
			Err: resilience.Outcome{Kind: resilience.KindTimeout, Attempts: 1}.Err(context.DeadlineExceeded),
		}, code: "RZ-UP-003"},
		{name: "gate cause after an attempt", err: &upstream.LegError{Upstream: "a", Attempts: 1, Err: resilience.GateBulkhead.Err()}, code: "RZ-UP-006"},
		{name: "Route deadline passed", err: &upstream.LegError{
			Upstream: "a", Kind: resilience.KindTimeout,
			Err: errcode.Wrap(resilience.CodeRouteTimeout, errors.New("upstream: Route timeout expired before any attempt")),
		}, code: "RZ-RT-007"},
		{name: "canceled", err: &upstream.LegError{
			Upstream: "a", Attempts: 1, Kind: resilience.KindTimeout,
			Err: resilience.Outcome{Kind: resilience.KindTimeout, Attempts: 1}.Err(context.Canceled),
		}, code: "RZ-UP-003"},
		{name: "not a leg error", err: errors.New("upstream: leg without an Outbound exchange")},
		{name: "upstream 503", resp: &snapshot.UpstreamResponse{Status: http.StatusServiceUnavailable, Upstream: "a", Attempts: 1}},
		{name: "generated", resp: &snapshot.UpstreamResponse{
			Status: http.StatusUnauthorized, Upstream: "a",
			Generated: &filter.Response{Status: http.StatusUnauthorized, Code: "RZ-AUTH-020"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &fakeLeg{name: "a", resp: tt.resp, err: tt.err}
			b := serving("b")
			f := forwarder([]*fakeLeg{a, b}, []uint32{1, 0}, noDraw{t})
			r, err := f.Forward(t.Context(), outbound(nil))
			if r != tt.resp || !errors.Is(err, tt.err) || (tt.err == nil) != (err == nil) {
				t.Fatalf("Forward = %+v, %v; want %+v, %v", r, err, tt.resp, tt.err)
			}
			if err != nil && codeOf(err) != tt.code {
				t.Errorf("code = %q, want %q", codeOf(err), tt.code)
			}
			if b.calls.Load() != 0 || a.calls.Load() != 1 {
				t.Errorf("legs ran a:%d b:%d, want a:1 b:0", a.calls.Load(), b.calls.Load())
			}
		})
	}
}

// TestForwardWrappedRefusal checks that a refusal wrapped by the leg's
// caller chain still picks again.
func TestForwardWrappedRefusal(t *testing.T) {
	a := &fakeLeg{name: "a", err: fmt.Errorf("leg: %w", refusal("a", resilience.GateNoEndpoint))}
	b := serving("b")
	f := forwarder([]*fakeLeg{a, b}, []uint32{1, 0}, noDraw{t})
	if r, err := f.Forward(t.Context(), outbound(nil)); err != nil || r.Upstream != "b" {
		t.Fatalf("Forward = %+v, %v; want b", r, err)
	}
}

// TestForwardSingleEntry checks a Route with one entry: its pick draws
// nothing, and its refusal answers at once.
func TestForwardSingleEntry(t *testing.T) {
	only := refusing("only", resilience.GateBulkhead)
	f := forwarder([]*fakeLeg{only}, []uint32{0}, noDraw{t})
	_, err := f.Forward(t.Context(), outbound(nil))
	if codeOf(err) != "RZ-UP-006" || only.calls.Load() != 1 {
		t.Fatalf("Forward = %v after %d legs, want RZ-UP-006 after 1", err, only.calls.Load())
	}
}

// TestForwardConcurrent runs one Forwarder from many goroutines on the
// runtime generator: the Forwarder is safe for concurrent use, and every
// request picks again past the refused entry.
func TestForwardConcurrent(t *testing.T) {
	a, b := refusing("a", resilience.GateBreaker), serving("b")
	f := forwarder([]*fakeLeg{a, b}, []uint32{1, 1}, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			o := outbound(nil)
			for range 500 {
				r, err := f.Forward(context.Background(), o)
				if err != nil {
					errs <- fmt.Errorf("Forward: %w; want b", err)
					return
				}
				if r.Upstream != "b" {
					errs <- fmt.Errorf("Forward = %+v; want b", r)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if n := b.calls.Load(); n != 4000 {
		t.Errorf("b ran %d legs, want 4000", n)
	}
}

// TestForwardAllocations is the onRoute allocation gate
// (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Per-stage latency budget": 0 for the leg selection): the pick and the
// pick again after refusals allocate nothing beyond what the legs return.
func TestForwardAllocations(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("coverage instrumentation changes allocation counts")
	}
	o := outbound(nil)
	ctx := t.Context()
	tests := []struct {
		name    string
		legs    []*fakeLeg
		weights []uint32
	}{
		{"one entry", []*fakeLeg{serving("a")}, []uint32{1}},
		{"weighted split", []*fakeLeg{serving("a"), serving("b"), serving("c")}, []uint32{5, 3, 2}},
		{"refusals", []*fakeLeg{refusing("a", resilience.GateBreaker), refusing("b", resilience.GateNoEndpoint), serving("c")}, []uint32{5, 3, 0}},
	}
	for _, tt := range tests {
		f := forwarder(tt.legs, tt.weights, pcg(1))
		if a := testing.AllocsPerRun(1000, func() {
			if _, err := f.Forward(ctx, o); err != nil {
				t.Fatal(err)
			}
		}); a != 0 {
			t.Errorf("%s: Forward allocates %.1f", tt.name, a)
		}
	}
}
