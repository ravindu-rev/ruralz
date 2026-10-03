// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for logging (spec 06 rule 11 and spec 07 req 87: no request data;
// filter.Result.Err is logged), contained panics in Finish, and the
// remaining leg and onChunk paths.

// recordHandler keeps every record it handles and the context it was
// logged with.
type recordHandler struct {
	mu   sync.Mutex
	recs []slog.Record
	ctxs []context.Context
}

func (*recordHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	h.recs = append(h.recs, r)
	h.ctxs = append(h.ctxs, ctx)
	h.mu.Unlock()
	return nil
}

// spanOf returns the name of the fake span in the context of the first
// record with message msg, "" when there is none.
func (h *recordHandler) spanOf(msg string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, r := range h.recs {
		if r.Message != msg {
			continue
		}
		if s, _ := h.ctxs[i].Value(spanKey{}).(*fakeSpan); s != nil {
			return s.name
		}
		return ""
	}
	return ""
}

func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordHandler) text() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var b strings.Builder
	for _, r := range h.recs {
		b.WriteString(r.Level.String() + " " + r.Message)
		r.Attrs(func(a slog.Attr) bool {
			b.WriteString(" " + a.Key + "=" + a.Value.String())
			return true
		})
		b.WriteString("\n")
	}
	return b.String()
}

type secretPanic struct{ token string }

func TestPanicLogNamesTypeNotValue(t *testing.T) {
	// A recovered panic is logged with the Policy, the Phase and the panic
	// value's type, never the value (it may hold a credential).
	rh := &recordHandler{}
	e := New(Deps{Logger: slog.New(rh)})
	st := newState()
	f := &fakeFilter{name: "jwt", script: map[phase.Phase]result{
		phase.OnRequestHeaders: func(context.Context, filter.Exchange) filter.Result {
			panic(secretPanic{token: "Bearer s3cr3t"})
		},
	}}
	p := policy(spec{name: "jwt", class: phase.ClassAuth, phases: []phase.Phase{phase.OnRequestHeaders}, f: f})
	r := e.Begin(st, chain([]*snapshot.Policy{p}, nil), nil)
	defer r.Release()
	if resp := r.Request(context.Background(), phase.OnRequestHeaders); got(resp) != "401 RZ-AUTH-002" {
		t.Fatalf("response %s", got(resp))
	}
	out := rh.text()
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("panic value logged: %s", out)
	}
	for _, want := range []string{"ERROR filter panic recovered", "policy=jwt", "phase=onRequestHeaders", "panic_type=executor.secretPanic", "stack=", "DEBUG filter could not decide", "error_type=internal"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %q:\n%s", want, out)
		}
	}
}

// panicFinish panics in Finish.
type panicFinish struct{ fakeFilter }

func (*panicFinish) Finish(context.Context, filter.Exchange) { panic("finish") }

func TestFinishPanicIsContained(t *testing.T) {
	// Finish must not take the request goroutine down (R-40): a panic is
	// recovered and the remaining Finishers still run.
	rh := &recordHandler{}
	ev := &events{}
	e := New(Deps{Logger: slog.New(rh)})
	st := newState()
	a := policy(spec{name: "a", class: phase.ClassCORS, phases: []phase.Phase{phase.OnRequestHeaders}, f: &panicFinish{fakeFilter{name: "a", ev: ev}}})
	b := policy(spec{name: "b", class: phase.ClassCache, phases: []phase.Phase{phase.OnRequestHeaders}, f: &finishFilter{fakeFilter{name: "b", ev: ev}}})
	r := e.Begin(st, chain([]*snapshot.Policy{a, b}, nil), nil)
	defer r.Release()
	r.Request(context.Background(), phase.OnRequestHeaders)
	r.Log(context.Background())
	if evs := ev.list(); evs[len(evs)-1] != "b.finish" {
		t.Fatalf("events %v", evs)
	}
	if !strings.Contains(rh.text(), "phase=onLog") {
		t.Fatalf("Finish panic not logged: %s", rh.text())
	}
}

func TestLegChunkOnlyAndSkippedLegPolicies(t *testing.T) {
	// A leg chain whose only subscriber is onChunk still gets a leg state
	// (M3 sources); leg Policies skipped by when are not called in any leg
	// Phase and get no Finish.
	h := newHarness(t)
	chunk := policy(spec{name: "chunk", class: phase.ClassCustom, scope: phase.ScopeUpstream, phases: []phase.Phase{phase.OnChunk}, f: &fakeFilter{name: "chunk", ev: h.ev}})
	skipped := policy(spec{
		name: "skipped", class: phase.ClassTransform, scope: phase.ScopeUpstream, when: &constProgram{ok: false},
		phases: []phase.Phase{phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders, phase.OnUpstreamResponseBody},
		f:      &finishFilter{fakeFilter{name: "skipped", ev: h.ev}},
	})
	ch := chain(nil, map[string][]*snapshot.Policy{"chunky": {chunk}, "up": {skipped}})
	ctx := context.Background()
	r := h.e.Begin(h.st, ch, nil)
	defer r.Release()
	lc := r.BeginLeg(ctx, "chunky", "")
	if _, ok := lc.(*legRun); !ok {
		t.Fatal("an onChunk-only leg chain got no leg state")
	}
	lc.End(ctx)
	l := r.BeginLeg(ctx, "up", "")
	leg := &filter.Leg{Upstream: "up", Attempt: 1}
	l.OnUpstreamRequest(ctx, leg)
	l.OnUpstreamResponseHeaders(ctx, leg)
	l.OnUpstreamResponseBody(ctx, leg)
	l.End(ctx)
	if len(h.ev.list()) != 0 {
		t.Fatalf("calls %v", h.ev.list())
	}
}

func TestChunkSkipsAndEnds(t *testing.T) {
	// onChunk (M3 sources): a when-skipped Policy is not called; open
	// passes the chunk, closed ends the stream (FP 4).
	h := newHarness(t)
	skip := policy(spec{name: "skip", class: phase.ClassCustom, when: &constProgram{ok: false}, phases: []phase.Phase{phase.OnChunk}, f: h.filter("skip", nil)})
	open := policy(spec{
		name: "open", class: phase.ClassCustom, mode: v1alpha1.FailureModeOpen, phases: []phase.Phase{phase.OnChunk},
		f: h.filter("open", map[phase.Phase]result{phase.OnChunk: undecided("")}),
	})
	r := h.e.Begin(h.st, chain([]*snapshot.Policy{skip, open}, nil), nil)
	defer r.Release()
	for range 2 {
		if r.Chunk(context.Background()) {
			t.Fatal("open ended the stream")
		}
	}
	// The second chunk skips the failed-open Policy.
	if evs := h.ev.list(); len(evs) != 1 {
		t.Fatalf("events %v", evs)
	}
	// Chunk never batches Consumptive Filters: Handle is called.
	h2 := newHarness(t)
	c := h2.consumer("budget")
	p := policy(spec{name: "budget", class: phase.ClassAdmission, phases: []phase.Phase{phase.OnChunk}, f: c})
	r2 := h2.e.Begin(h2.st, chain([]*snapshot.Policy{p}, nil), h2.store)
	defer r2.Release()
	r2.Chunk(context.Background())
	if c.callCount() != 1 || h2.store.Trips != 0 {
		t.Fatalf("Handle %d trips %d", c.callCount(), h2.store.Trips)
	}
}

func TestOpNames(t *testing.T) {
	// ruralz.state.op values come from the catalog, indexed by the
	// emit.StateOp* labels (R-56).
	e := New(Deps{})
	for _, tc := range []struct {
		call statestore.Call
		want string
	}{
		{statestore.Call{Kind: statestore.OpGCRA}, "gcra"},
		{statestore.Call{Kind: statestore.OpQuota}, "quota"},
		{statestore.Call{Kind: statestore.OpQuota, Trip: statestore.TripScriptMulti}, "script_multi"},
		{statestore.Call{Kind: statestore.OpCacheGet, Trip: statestore.TripPipeline}, "pipeline"},
		{statestore.Call{}, ""},
	} {
		if got := e.opName(&tc.call); got != tc.want {
			t.Errorf("opName(%+v) = %q, want %q", tc.call.Kind, got, tc.want)
		}
	}
	if nanos(-5) != 0 || nanos(7) != 7 {
		t.Error("nanos")
	}
	if statusOf("RZ-PLG-001", 599) != 599 {
		t.Error("statusOf fallback")
	}
}
