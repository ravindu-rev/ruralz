// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for consumptive batching (pack 8.7 rules 1 to 3; spec 05 reqs 64
// and 91; spec 08 reqs 27 to 29, 34 and 38; R-14, R-39): Prepare, shared
// Store.Consume round trips counted with filtertest.Store, Complete,
// Undo, per-member PolicyState and the request's RequestBudget.

// newConsumer returns a Consumptive admission Filter.
func (h *harness) consumer(name string) *consumer {
	return &consumer{fakeFilter: fakeFilter{name: name, ev: h.ev}, t: h.t}
}

// consumptive compiles members as consecutive admission Policies of
// onRequestHeaders (ratelimit and quota), with a cors Policy before and a
// headers Policy after.
func (h *harness) consumptive(modes []v1alpha1.FailureMode, cs ...*consumer) (*snapshot.Chain, []*snapshot.Policy) {
	var ps []*snapshot.Policy
	ps = append(ps, policy(spec{name: "cors", class: phase.ClassCORS, phases: []phase.Phase{phase.OnRequestHeaders}, f: h.filter("cors", nil)}))
	var members []*snapshot.Policy
	for i, c := range cs {
		mode := v1alpha1.FailureModeOpen
		if i < len(modes) {
			mode = modes[i]
		}
		p := policy(spec{
			name: c.name, class: phase.ClassAdmission, typ: v1alpha1.PolicyTypeRateLimit, mode: mode,
			phases: []phase.Phase{phase.OnRequestHeaders, phase.OnLog}, f: c,
		})
		members = append(members, p)
		ps = append(ps, p)
	}
	ps = append(ps, policy(spec{name: "hdr", class: phase.ClassTransform, phases: []phase.Phase{phase.OnRequestHeaders}, f: h.filter("hdr", nil)}))
	return chain(ps, nil), members
}

func (h *harness) requestHeaders(ch *snapshot.Chain) *filter.Response {
	r := h.e.Begin(h.st, ch, h.store)
	defer r.Release()
	return r.Request(context.Background(), phase.OnRequestHeaders)
}

func TestBatchOneRoundTrip(t *testing.T) {
	// Pack 8.7 rule 3: consecutive Consumptive members share one round trip
	// when their keys share a slot (filtertest.Store takes every call); each
	// member's Prepare, Complete and PolicyState are its own, and Handle is
	// never called in onRequestHeaders.
	h := newHarness(t)
	a, b, c := h.consumer("rl-a"), h.consumer("rl-b"), h.consumer("quota")
	ch, _ := h.consumptive(nil, a, b, c)
	if resp := h.requestHeaders(ch); resp != nil {
		t.Fatalf("response %s", got(resp))
	}
	if h.store.Trips != 1 {
		t.Fatalf("round trips = %d, want 1", h.store.Trips)
	}
	want := []string{
		"cors.onRequestHeaders", "rl-a.prepare", "rl-b.prepare", "quota.prepare",
		"rl-a.complete", "rl-b.complete", "quota.complete", "hdr.onRequestHeaders",
	}
	if evs := h.ev.list(); !slices.Equal(evs, want) {
		t.Fatalf("events %v, want %v", evs, want)
	}
	for _, m := range []*consumer{a, b, c} {
		if m.callCount() != 0 {
			t.Fatalf("%s: Handle called in onRequestHeaders", m.name)
		}
	}
}

func TestBatchSequentialRoundTrips(t *testing.T) {
	// Pack 8.7 rule 3 "otherwise sequential round trips": a Store taking one
	// call per round trip makes three trips, each member exactly one
	// (spec 08 req 27, at most one blocking round trip per Policy).
	h := newHarness(t)
	var sizes []int
	h.store.ConsumeFn = func(calls []*statestore.Call) int {
		sizes = append(sizes, len(calls))
		calls[0].Done, calls[0].GCRA.Allowed = true, true
		return 1
	}
	ch, _ := h.consumptive(nil, h.consumer("a"), h.consumer("b"), h.consumer("c"))
	if resp := h.requestHeaders(ch); resp != nil {
		t.Fatalf("response %s", got(resp))
	}
	if h.store.Trips != 3 || !slices.Equal(sizes, []int{3, 2, 1}) {
		t.Fatalf("trips %d sizes %v", h.store.Trips, sizes)
	}
	// Complete follows each round trip in chain order.
	want := []string{"a.complete", "b.complete", "c.complete"}
	if got := filterSuffix(h.ev.list(), ".complete"); !slices.Equal(got, want) {
		t.Fatalf("completes %v", got)
	}
}

func TestBatchDenyUndoesLaterMembers(t *testing.T) {
	// Spec 05 req 64: calls stop at the first deny and local tokens taken by
	// Policies after it are returned (Undo), including one that admitted
	// locally in Prepare; earlier members keep their decision.
	h := newHarness(t)
	h.store.ConsumeFn = func(calls []*statestore.Call) int {
		calls[0].Done, calls[0].GCRA.Allowed = true, true
		calls[1].Done, calls[1].GCRA.Allowed = true, false
		return len(calls) // calls[2] stays !Done after the deny
	}
	a, b, c := h.consumer("a"), h.consumer("b"), h.consumer("c")
	local := h.consumer("local")
	local.prepare = func(filter.Exchange, *statestore.Call) (filter.Result, bool) { return filter.Next(), true }
	ch, members := h.consumptive(nil, a, b, local, c)
	m, pr := newProbe()
	members[1].Metrics = m
	resp := h.requestHeaders(ch)
	if got(resp) != "429 RZ-RL-002" {
		t.Fatalf("response %s", got(resp))
	}
	want := []string{
		"cors.onRequestHeaders", "a.prepare", "b.prepare", "local.prepare", "c.prepare",
		"a.complete", "b.complete", "local.undo", "c.undo",
	}
	if evs := h.ev.list(); !slices.Equal(evs, want) {
		t.Fatalf("events %v, want %v", evs, want)
	}
	if h.store.Trips != 1 || pr.short[phase.OnRequestHeaders].by[429] != 1 {
		t.Fatalf("trips %d short-circuits %v", h.store.Trips, pr.short[phase.OnRequestHeaders].by)
	}
	if len(h.st.shorts) != 1 || h.st.shorts[0].policy != "b" {
		t.Fatalf("RecordShortCircuit %+v", h.st.shorts)
	}
	// c's span ends as skipped; it ran (Prepare), so it gets Finish.
	if s := h.tr.named("c"); len(s) != 1 || s[0].attr(catalog.AttrOutcome) != emit.OutcomeSkipped || s[0].ended != 1 {
		t.Fatalf("c span %+v", s)
	}
}

func TestBatchLocalDenySendsNothing(t *testing.T) {
	// Spec 05 req 91: local denials send zero State Store commands; members
	// prepared before the deny return their tokens, later ones never run.
	h := newHarness(t)
	a, b, c := h.consumer("a"), h.consumer("b"), h.consumer("c")
	b.prepare = func(filter.Exchange, *statestore.Call) (filter.Result, bool) {
		return filter.Deny(http.StatusTooManyRequests, "RZ-RL-001", http.Header{"Retry-After": {"1"}}), true
	}
	ch, _ := h.consumptive(nil, a, b, c)
	resp := h.requestHeaders(ch)
	if got(resp) != "429 RZ-RL-001" || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("response %s", got(resp))
	}
	if h.store.Trips != 0 {
		t.Fatalf("round trips = %d, want 0", h.store.Trips)
	}
	want := []string{"cors.onRequestHeaders", "a.prepare", "b.prepare", "a.undo"}
	if evs := h.ev.list(); !slices.Equal(evs, want) {
		t.Fatalf("events %v, want %v", evs, want)
	}
}

func TestBatchStoreFailureEachMemberAppliesItsMode(t *testing.T) {
	// Spec 08 req 38: in a merged round trip every call gets the same
	// failure; the executor applies each Policy's failureMode in chain order
	// and stops at the first closed rejection (503 with the Filter's RZ-STS
	// code, spec 08 req 36); the members after it return their tokens.
	h := newHarness(t)
	h.store.ConsumeFn = func(calls []*statestore.Call) int {
		for _, c := range calls {
			c.Err = statestore.ErrTimeout
		}
		return len(calls)
	}
	a, b, c := h.consumer("a"), h.consumer("b"), h.consumer("c")
	ch, members := h.consumptive([]v1alpha1.FailureMode{v1alpha1.FailureModeOpen, v1alpha1.FailureModeClosed, v1alpha1.FailureModeOpen}, a, b, c)
	ma, pa := newProbe()
	members[0].Metrics = ma
	resp := h.requestHeaders(ch)
	if got(resp) != "503 RZ-STS-001" {
		t.Fatalf("response %s", got(resp))
	}
	want := []string{"cors.onRequestHeaders", "a.prepare", "b.prepare", "c.prepare", "a.complete", "b.complete", "c.undo"}
	if evs := h.ev.list(); !slices.Equal(evs, want) {
		t.Fatalf("events %v, want %v", evs, want)
	}
	if pa.failures(phase.OnRequestHeaders, emit.ModeOpen) != 1 {
		t.Fatal("a's open failure not counted")
	}
	wantFails := []failRec{{"a", phase.OnRequestHeaders, v1alpha1.FailureModeOpen}, {"b", phase.OnRequestHeaders, v1alpha1.FailureModeClosed}}
	if !slices.Equal(h.st.fails, wantFails) {
		t.Fatalf("failures %+v", h.st.fails)
	}
	// a failed open: skipped for onLog, but Finish still sees it ran.
	r := h.e.Begin(h.st, ch, h.store)
	defer r.Release()
	if !r.isFailed(members[0]) && len(r.failed) != 0 {
		t.Fatal("unexpected failed state on a new Run")
	}
}

func TestBatchWithoutStoreIsNotAttempted(t *testing.T) {
	// Spec 08 req 28/33: with no State Store the prepared calls are not
	// attempted (RZ-STS-004) and each member applies its failureMode.
	h := newHarness(t)
	a := h.consumer("a")
	ch, _ := h.consumptive([]v1alpha1.FailureMode{v1alpha1.FailureModeClosed}, a)
	r := h.e.Begin(h.st, ch, nil)
	defer r.Release()
	if resp := r.Request(context.Background(), phase.OnRequestHeaders); got(resp) != "503 RZ-STS-004" {
		t.Fatalf("response %s", got(resp))
	}
}

func TestBatchUndecidedCallAfterAllow(t *testing.T) {
	// A call the Store left undecided without an error (a Store or Filter
	// bug) is completed as not attempted rather than dropped.
	h := newHarness(t)
	h.store.ConsumeFn = func(calls []*statestore.Call) int {
		calls[0].Done, calls[0].GCRA.Allowed = true, true
		return len(calls)
	}
	ch, _ := h.consumptive([]v1alpha1.FailureMode{v1alpha1.FailureModeOpen, v1alpha1.FailureModeClosed}, h.consumer("a"), h.consumer("b"))
	if resp := h.requestHeaders(ch); got(resp) != "503 RZ-STS-004" {
		t.Fatalf("response %s", got(resp))
	}
	// A Store returning 0 or more than it was given is clamped.
	h = newHarness(t)
	h.store.ConsumeFn = func(calls []*statestore.Call) int {
		for _, c := range calls {
			c.Done, c.GCRA.Allowed = true, true
		}
		return 99
	}
	ch, _ = h.consumptive(nil, h.consumer("a"), h.consumer("b"))
	if resp := h.requestHeaders(ch); resp != nil || h.store.Trips != 1 {
		t.Fatalf("response %s trips %d", got(resp), h.store.Trips)
	}
}

// budgetStore records the RequestBudget and timeouts Consume receives.
type budgetStore struct {
	filtertest.Store
	budgets  []*statestore.RequestBudget
	timeouts []time.Duration
}

func (s *budgetStore) Consume(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) int {
	s.budgets = append(s.budgets, rb)
	for _, c := range calls {
		s.timeouts = append(s.timeouts, c.Timeout)
	}
	return s.Store.Consume(ctx, rb, calls)
}

func TestBatchBudgetAndTimeouts(t *testing.T) {
	// Spec 04 req 45 and spec 08 req 28: the round trip gets the request's
	// RequestBudget (with its stripe); a call without a timeout takes the
	// Policy's effective stateStoreTimeout.
	h := newHarness(t)
	bs := &budgetStore{}
	a, b := h.consumer("a"), h.consumer("b")
	b.timeout = 7 * time.Millisecond
	ch, _ := h.consumptive(nil, a, b)
	r := h.e.Begin(h.st, ch, bs)
	defer r.Release()
	if resp := r.Request(context.Background(), phase.OnRequestHeaders); resp != nil {
		t.Fatalf("response %s", got(resp))
	}
	if len(bs.budgets) != 1 || bs.budgets[0] != &h.st.budget {
		t.Fatal("Consume did not get the request's RequestBudget")
	}
	if !slices.Equal(bs.timeouts, []time.Duration{20 * time.Millisecond, 7 * time.Millisecond}) {
		t.Fatalf("timeouts %v", bs.timeouts)
	}
	// Without a budget in the state, a zero budget stands in.
	h2 := newHarness(t)
	h2.st.noBudget = true
	bs2 := &budgetStore{}
	ch2, _ := h2.consumptive(nil, h2.consumer("a"))
	r2 := h2.e.Begin(h2.st, ch2, bs2)
	defer r2.Release()
	r2.Request(context.Background(), phase.OnRequestHeaders)
	if len(bs2.budgets) != 1 || bs2.budgets[0] == nil {
		t.Fatal("no stand-in budget")
	}
}

func TestBatchSpanStateAttributes(t *testing.T) {
	// Spec 08 req 65: a shared round trip is recorded on the first member's
	// span (op and duration) and named by the others in ruralz.state.batch.
	h := newHarness(t)
	h.store.ConsumeFn = func(calls []*statestore.Call) int {
		for _, c := range calls {
			c.Done, c.GCRA.Allowed = true, true
			c.Batch, c.Trip, c.Elapsed = 0, statestore.TripScriptMulti, 900*time.Microsecond
		}
		return len(calls)
	}
	ch, _ := h.consumptive(nil, h.consumer("first"), h.consumer("second"))
	if resp := h.requestHeaders(ch); resp != nil {
		t.Fatal(got(resp))
	}
	first, second := h.tr.named("first"), h.tr.named("second")
	if len(first) != 1 || first[0].attr(catalog.AttrStateOp) != "script_multi" || first[0].attr(catalog.AttrStateDuration) != "900µs" {
		t.Fatalf("first span %+v", first[0].attrs)
	}
	if len(second) != 1 || second[0].attr(catalog.AttrStateBatch) != "first" || second[0].attr(catalog.AttrStateOp) != "" {
		t.Fatalf("second span %+v", second[0].attrs)
	}
	// A single call records its own op.
	h = newHarness(t)
	ch, _ = h.consumptive(nil, h.consumer("only"))
	h.requestHeaders(ch)
	if s := h.tr.named("only"); s[0].attr(catalog.AttrStateOp) != "gcra" {
		t.Fatalf("single call span %+v", s[0].attrs)
	}
}

func TestBatchSkipsWhenFalseAndBreaksOnNonConsumptive(t *testing.T) {
	// A member skipped by when takes no part; a non-Consumptive Policy
	// between Consumptive ones splits the run into two batches.
	h := newHarness(t)
	a, b, c := h.consumer("a"), h.consumer("b"), h.consumer("c")
	pa := policy(spec{name: "a", class: phase.ClassAdmission, phases: []phase.Phase{phase.OnRequestHeaders}, f: a})
	pb := policy(spec{name: "b", class: phase.ClassAdmission, phases: []phase.Phase{phase.OnRequestHeaders}, f: b, when: &constProgram{ok: false}})
	mid := policy(spec{name: "mid", class: phase.ClassAdmission, phases: []phase.Phase{phase.OnRequestHeaders}, f: h.filter("mid", nil)})
	pc := policy(spec{name: "c", class: phase.ClassAdmission, phases: []phase.Phase{phase.OnRequestHeaders}, f: c})
	ch := chain([]*snapshot.Policy{pa, pb, mid, pc}, nil)
	if resp := h.requestHeaders(ch); resp != nil {
		t.Fatal(got(resp))
	}
	want := []string{"a.prepare", "a.complete", "mid.onRequestHeaders", "c.prepare", "c.complete"}
	if evs := h.ev.list(); !slices.Equal(evs, want) || h.store.Trips != 2 {
		t.Fatalf("events %v trips %d", evs, h.store.Trips)
	}
}

func TestConsumptiveOutsideRequestHeadersUsesHandle(t *testing.T) {
	// Consumptive batching is for onRequestHeaders only; a Consumptive
	// Filter's other Phases (quota's onLog settlement) call Handle.
	h := newHarness(t)
	q := h.consumer("quota")
	ch, _ := h.consumptive(nil, q)
	r := h.e.Begin(h.st, ch, h.store)
	defer r.Release()
	r.Request(context.Background(), phase.OnRequestHeaders)
	r.Log(context.Background())
	if q.callCount() != 1 || !slices.Contains(h.ev.list(), "quota.onLog") {
		t.Fatalf("Handle calls %d, events %v", q.callCount(), h.ev.list())
	}
}

func TestBatchPanicsAreCannotDecide(t *testing.T) {
	// Spec 04 req 43: panics in Prepare and Complete become cannot decide
	// (closed: 503 with the admission default); a panic in Undo is contained.
	tests := []struct {
		name  string
		setup func(a, b *consumer)
		want  string
	}{
		{"prepare", func(a, _ *consumer) {
			a.prepare = func(filter.Exchange, *statestore.Call) (filter.Result, bool) { panic("prepare") }
		}, "503 RZ-RT-011"},
		{"complete", func(a, _ *consumer) {
			a.complete = func(filter.Exchange, *statestore.Call) filter.Result { panic("complete") }
		}, "503 RZ-RT-011"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			a, b := h.consumer("a"), h.consumer("b")
			tt.setup(a, b)
			ch, _ := h.consumptive([]v1alpha1.FailureMode{v1alpha1.FailureModeClosed}, a, b)
			if resp := h.requestHeaders(ch); got(resp) != tt.want {
				t.Fatalf("response %s", got(resp))
			}
		})
	}
	// Undo panic: b denies locally after a prepared; a's Undo panics.
	h := newHarness(t)
	a := &panicUndo{consumer: *h.consumer("a")}
	b := h.consumer("b")
	b.prepare = func(filter.Exchange, *statestore.Call) (filter.Result, bool) {
		return filter.Deny(429, "RZ-RL-001", nil), true
	}
	pa := policy(spec{name: "a", class: phase.ClassAdmission, phases: []phase.Phase{phase.OnRequestHeaders}, f: a})
	pb := policy(spec{name: "b", class: phase.ClassAdmission, phases: []phase.Phase{phase.OnRequestHeaders}, f: b})
	if resp := h.requestHeaders(chain([]*snapshot.Policy{pa, pb}, nil)); got(resp) != "429 RZ-RL-001" {
		t.Fatalf("response %s", got(resp))
	}
}

func TestBatchMembersGetTheirSpanContext(t *testing.T) {
	// Prepare, Complete and Undo get the member's own ruralz.filter.<name>
	// span context, as Handle does, so what they log or start correlates
	// with that span; a run longer than the stack-held contexts too.
	for _, n := range []int{3, batchContexts + 2} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			h := newHarness(t)
			var cs []*consumer
			for i := range n {
				c := h.consumer(fmt.Sprintf("m%d", i))
				c.spanCtx = true
				cs = append(cs, c)
			}
			ch, _ := h.consumptive(nil, cs...)
			if resp := h.requestHeaders(ch); resp != nil {
				t.Fatalf("response %s", got(resp))
			}
			if completes := filterSuffix(h.ev.list(), ".complete"); len(completes) != n {
				t.Fatalf("completes %v", completes)
			}
		})
	}

	tests := []struct {
		name  string
		setup func(h *harness) *snapshot.Chain
	}{
		{"pending member before a local deny", func(h *harness) *snapshot.Chain {
			a := &panicUndo{consumer: *h.consumer("a")}
			b := h.consumer("b")
			b.prepare = func(filter.Exchange, *statestore.Call) (filter.Result, bool) {
				return filter.Deny(429, "RZ-RL-001", nil), true
			}
			return chain([]*snapshot.Policy{
				policy(spec{name: "a", class: phase.ClassAdmission, phases: []phase.Phase{phase.OnRequestHeaders}, f: a}),
				policy(spec{name: "b", class: phase.ClassAdmission, phases: []phase.Phase{phase.OnRequestHeaders}, f: b}),
			}, nil)
		}},
		{"locally admitted member after a round-trip deny", func(h *harness) *snapshot.Chain {
			h.store.ConsumeFn = func(calls []*statestore.Call) int {
				calls[0].Done, calls[0].GCRA.Allowed = true, false
				return len(calls)
			}
			b := h.consumer("b")
			a := &panicUndo{consumer: *h.consumer("a")}
			a.prepare = func(filter.Exchange, *statestore.Call) (filter.Result, bool) { return filter.Next(), true }
			return chain([]*snapshot.Policy{
				policy(spec{name: "b", class: phase.ClassAdmission, phases: []phase.Phase{phase.OnRequestHeaders}, f: b}),
				policy(spec{name: "a", class: phase.ClassAdmission, phases: []phase.Phase{phase.OnRequestHeaders}, f: a}),
			}, nil)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rh := &recordHandler{}
			h := newHarness(t)
			h.e = New(Deps{Tracer: h.tr, Logger: slog.New(rh)})
			if resp := h.requestHeaders(tt.setup(h)); resp == nil || resp.Status != http.StatusTooManyRequests {
				t.Fatalf("response %s", got(resp))
			}
			if name := rh.spanOf("filter panic recovered"); name != catalog.FilterSpanName("a") {
				t.Fatalf("Undo ran in span context %q, want a's span", name)
			}
		})
	}
}

func TestBatchWhenErrors(t *testing.T) {
	// Spec 03 req 43 in a batch: a member whose when errs runs under closed
	// (its span carries the applied mode and error.type) and is skipped
	// under open (no Prepare, a skipped span); each counts one failure in
	// onRequestHeaders and feeds RecordFailure.
	h := newHarness(t)
	a, b, c := h.consumer("a"), h.consumer("b"), h.consumer("c")
	ch, members := h.consumptive([]v1alpha1.FailureMode{v1alpha1.FailureModeClosed, v1alpha1.FailureModeOpen}, a, b, c)
	members[0].When = &constProgram{err: newEvalError()}
	members[1].When = &constProgram{err: newEvalError()}
	ma, pa := newProbe()
	mb, pb := newProbe()
	members[0].Metrics, members[1].Metrics = ma, mb
	if resp := h.requestHeaders(ch); resp != nil {
		t.Fatalf("response %s", got(resp))
	}
	want := []string{"cors.onRequestHeaders", "a.prepare", "c.prepare", "a.complete", "c.complete", "hdr.onRequestHeaders"}
	if evs := h.ev.list(); !slices.Equal(evs, want) {
		t.Fatalf("events %v, want %v", evs, want)
	}
	wantFails := []failRec{{"a", phase.OnRequestHeaders, v1alpha1.FailureModeClosed}, {"b", phase.OnRequestHeaders, v1alpha1.FailureModeOpen}}
	if !slices.Equal(h.st.fails, wantFails) {
		t.Fatalf("failures %+v", h.st.fails)
	}
	if pa.failures(phase.OnRequestHeaders, emit.ModeClosed) != 1 || pb.failures(phase.OnRequestHeaders, emit.ModeOpen) != 1 {
		t.Fatal("when failures not counted")
	}
	sa, sb := h.tr.named("a"), h.tr.named("b")
	if len(sa) != 1 || sa[0].attr(catalog.AttrOutcome) != emit.OutcomeContinue || sa[0].attr(catalog.AttrFailureMode) != "closed" || sa[0].errType != errTypeCEL {
		t.Fatalf("a span %+v %q", sa[0].attrs, sa[0].errType)
	}
	if len(sb) != 1 || sb[0].attr(catalog.AttrOutcome) != emit.OutcomeSkipped || sb[0].attr(catalog.AttrFailureMode) != "open" || sb[0].errType != errTypeCEL {
		t.Fatalf("b span %+v %q", sb[0].attrs, sb[0].errType)
	}

	// A marked member still waiting when a later member denies locally is
	// undone; its skipped span keeps the mark.
	h = newHarness(t)
	a, b = h.consumer("a"), h.consumer("b")
	b.prepare = func(filter.Exchange, *statestore.Call) (filter.Result, bool) {
		return filter.Deny(429, "RZ-RL-001", nil), true
	}
	ch, members = h.consumptive([]v1alpha1.FailureMode{v1alpha1.FailureModeClosed}, a, b)
	members[0].When = &constProgram{panic: true}
	ma, pa = newProbe()
	members[0].Metrics = ma
	if resp := h.requestHeaders(ch); got(resp) != "429 RZ-RL-001" {
		t.Fatalf("response %s", got(resp))
	}
	if pa.dur[phase.OnRequestHeaders].n.Load() != 1 || pa.failures(phase.OnRequestHeaders, emit.ModeClosed) != 1 {
		t.Fatal("the undone member's duration or when failure not recorded")
	}
	if sa := h.tr.named("a"); len(sa) != 1 || sa[0].attr(catalog.AttrOutcome) != emit.OutcomeSkipped ||
		sa[0].attr(catalog.AttrFailureMode) != "closed" || sa[0].errType != errTypeInternal {
		t.Fatalf("a span %+v %q", sa[0].attrs, sa[0].errType)
	}
}

func TestBatchLocalOpenFailureIsSettled(t *testing.T) {
	// Spec 04 req 43: a member whose Prepare cannot decide locally under
	// open is settled at once (skipped for the request, never undone) and
	// sends nothing; the batch goes on with the next member.
	h := newHarness(t)
	a, b := h.consumer("a"), h.consumer("b")
	a.prepare = func(filter.Exchange, *statestore.Call) (filter.Result, bool) {
		return filter.Undecided("", errUndecidedTest), true
	}
	h.store.ConsumeFn = func(calls []*statestore.Call) int {
		calls[0].Done, calls[0].GCRA.Allowed = true, false
		return len(calls)
	}
	ch, _ := h.consumptive(nil, a, b)
	if resp := h.requestHeaders(ch); got(resp) != "429 RZ-RL-002" {
		t.Fatalf("response %s", got(resp))
	}
	want := []string{"cors.onRequestHeaders", "a.prepare", "b.prepare", "b.complete"}
	if evs := h.ev.list(); !slices.Equal(evs, want) {
		t.Fatalf("events %v, want %v", evs, want)
	}
	if len(h.st.fails) != 1 || h.st.fails[0] != (failRec{"a", phase.OnRequestHeaders, v1alpha1.FailureModeOpen}) {
		t.Fatalf("failures %+v", h.st.fails)
	}
}

type panicUndo struct{ consumer }

func (*panicUndo) Undo(filter.Exchange) { panic("undo") }

func TestBatchReusesScratchWithoutLeaks(t *testing.T) {
	// The batch scratch is cleared after each run so a pooled Run retains
	// no Policy, span or call (R-39: the executor's call is reused).
	h := newHarness(t)
	ch, _ := h.consumptive(nil, h.consumer("a"), h.consumer("b"))
	r := h.e.Begin(h.st, ch, h.store)
	r.Request(context.Background(), phase.OnRequestHeaders)
	for i, m := range r.members[:cap(r.members)] {
		if m.p != nil || m.span != nil {
			t.Fatalf("member %d retained", i)
		}
	}
	for i, c := range r.calls[:cap(r.calls)] {
		if c.GCRA.Policy != "" || c.Kind != 0 {
			t.Fatalf("call %d retained %+v", i, c.GCRA.Policy)
		}
	}
	r.Release()
	if fmt.Sprint(r.chain, r.store, r.rs) != "<nil> <nil> <nil>" {
		t.Fatal("Release kept references")
	}
}
