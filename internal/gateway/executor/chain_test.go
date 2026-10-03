// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for spec 04 reqs 39 to 42 and 44 (Phases, order, when, short-circuit,
// metrics and spans), Security rule 1 (spec 06 section 2.1 rule 1), onLog
// and Finish (R-40), and spec 07 req 17 (onResponse after a replacement).

var allClientPhases = []phase.Phase{phase.OnRequestHeaders, phase.OnRequestBody, phase.OnRoute, phase.OnResponse, phase.OnLog}

// fullRun runs every client Phase (stopping at a short-circuit), then
// onResponse with the matching kind, onLog and Finish.
func (h *harness) fullRun(ch *snapshot.Chain) (short, repl *filter.Response) {
	ctx := context.Background()
	r := h.e.Begin(h.st, ch, h.store)
	defer r.Release()
	kind := ResponseUpstream
	for _, ph := range []phase.Phase{phase.OnRequestHeaders, phase.OnRequestBody, phase.OnRoute} {
		if short = r.Request(ctx, ph); short != nil {
			kind = ResponseGenerated
			break
		}
	}
	repl = r.Response(ctx, kind)
	r.Log(ctx)
	return short, repl
}

func TestOrderFollowsTheCompiledChain(t *testing.T) {
	// Spec 04 req 40: class, then scope, then position in request Phases;
	// response Phases reversed; onLog in request order; Finish in request
	// order after onLog (R-40).
	h := newHarness(t)
	all := []phase.Phase{phase.OnRequestHeaders, phase.OnResponse, phase.OnLog}
	var ps []*snapshot.Policy
	for _, s := range []struct {
		name  string
		class phase.Class
		scope phase.Scope
	}{
		{"cors", phase.ClassCORS, phase.ScopeGateway},
		{"jwt", phase.ClassAuth, phase.ScopeGateway},
		{"authz-g", phase.ClassAuthz, phase.ScopeGateway},
		{"authz-r", phase.ClassAuthz, phase.ScopeRoute},
		{"hdr", phase.ClassTransform, phase.ScopeRoute},
	} {
		ps = append(ps, policy(spec{
			name: s.name, class: s.class, scope: s.scope, phases: all,
			f: &finishFilter{*h.filter(s.name, nil)},
		}))
	}
	ch := chain(ps, nil)
	if short, repl := h.fullRun(ch); short != nil || repl != nil {
		t.Fatalf("unexpected responses %s %s", got(short), got(repl))
	}
	names := []string{"cors", "jwt", "authz-g", "authz-r", "hdr"}
	var want []string
	for _, n := range names {
		want = append(want, n+".onRequestHeaders")
	}
	for _, n := range slices.Backward(names) {
		want = append(want, n+".onResponse")
	}
	for _, n := range names {
		want = append(want, n+".onLog")
	}
	for _, n := range names {
		want = append(want, n+".finish")
	}
	if evs := h.ev.list(); !slices.Equal(evs, want) {
		t.Fatalf("order:\n got %v\nwant %v", evs, want)
	}
}

func TestEmptyPhaseMakesNoCall(t *testing.T) {
	// Spec 04 req 40: a Phase with no subscriber costs one length check —
	// no Enter, no span, no metric.
	h := newHarness(t)
	ch := chain(nil, nil)
	h.fullRun(ch)
	r := h.e.Begin(h.st, ch, nil)
	if end := r.Chunk(context.Background()); end {
		t.Fatal("an empty onChunk ended the stream")
	}
	l := r.BeginLeg(context.Background(), "missing", "")
	if l.OnUpstreamRequest(context.Background(), &filter.Leg{}) != nil {
		t.Fatal("a leg without Policies responded")
	}
	if rt, resp := l.OnUpstreamResponseHeaders(context.Background(), &filter.Leg{}); rt || resp != nil {
		t.Fatal("a leg without Policies retried")
	}
	if l.OnUpstreamResponseBody(context.Background(), &filter.Leg{}) != nil {
		t.Fatal("a leg without Policies replaced")
	}
	l.End(context.Background())
	r.Release()
	if h.st.enters != 0 || h.st.legCalls != 0 || len(h.tr.spans) != 0 {
		t.Fatalf("enters %d, leg states %d, spans %d", h.st.enters, h.st.legCalls, len(h.tr.spans))
	}
	// A nil chain runs nothing either.
	r = h.e.Begin(h.st, nil, nil)
	defer r.Release()
	if r.Request(context.Background(), phase.OnRequestHeaders) != nil || r.Response(context.Background(), ResponseUpstream) != nil || r.Chunk(context.Background()) {
		t.Fatal("a nil chain ran")
	}
	r.Log(context.Background())
	// Only the three client request Phases run in Request.
	p := policy(spec{name: "p", phases: []phase.Phase{phase.OnRequestHeaders}, f: h.filter("p", nil)})
	r2 := h.e.Begin(h.st, chain([]*snapshot.Policy{p}, nil), nil)
	defer r2.Release()
	for _, ph := range []phase.Phase{phase.OnUpstreamRequest, phase.OnResponse, phase.OnLog, phase.Count} {
		if r2.Request(context.Background(), ph) != nil {
			t.Fatalf("Request(%s) ran", ph)
		}
	}
	if len(h.ev.list()) != 0 {
		t.Fatalf("calls: %v", h.ev.list())
	}
}

func TestWhenEvaluatedOnceBeforeFirstPhase(t *testing.T) {
	// Spec 04 req 41: when is evaluated once, before the Policy's first
	// Phase; false skips it for the request; a runtime error runs it under
	// closed and skips it under open (test plan item 7: false/true/error x
	// closed/open). Spec 03 req 43 (Policy.spec.when row) and section 9
	// item 18: a runtime error counts one ruralz_filter_failures_total
	// {phase=<first Phase>, mode=<applied>}, one RecordFailure (access log
	// failure_modes) and marks ruralz.filter.<name>: the call's span under
	// closed, a skipped span under open.
	celErr := errors.New("cel: no_such_key")
	tests := []struct {
		name    string
		prog    *constProgram
		mode    v1alpha1.FailureMode
		wantRun bool
		// wantErr is the error.type of a when runtime error, "" for none.
		wantErr string
	}{
		{"true/closed", &constProgram{ok: true}, v1alpha1.FailureModeClosed, true, ""},
		{"true/open", &constProgram{ok: true}, v1alpha1.FailureModeOpen, true, ""},
		{"false/closed", &constProgram{ok: false}, v1alpha1.FailureModeClosed, false, ""},
		{"false/open", &constProgram{ok: false}, v1alpha1.FailureModeOpen, false, ""},
		{"error/closed", &constProgram{err: celErr}, v1alpha1.FailureModeClosed, true, errTypeInternal},
		{"error/open", &constProgram{err: celErr}, v1alpha1.FailureModeOpen, false, errTypeInternal},
		{"eval error/closed", &constProgram{err: newEvalError()}, v1alpha1.FailureModeClosed, true, errTypeCEL},
		{"eval error/open", &constProgram{err: newEvalError()}, v1alpha1.FailureModeOpen, false, errTypeCEL},
		{"panic/closed", &constProgram{panic: true}, v1alpha1.FailureModeClosed, true, errTypeInternal},
		{"panic/open", &constProgram{panic: true}, v1alpha1.FailureModeOpen, false, errTypeInternal},
		{"absent", nil, v1alpha1.FailureModeClosed, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			s := spec{
				name: "p", class: phase.ClassTransform, mode: tt.mode, phases: allClientPhases,
				f: &finishFilter{*h.filter("p", nil)},
			}
			if tt.prog != nil {
				s.when = tt.prog
			}
			p := policy(s)
			m, pr := newProbe()
			p.Metrics = m
			h.fullRun(chain([]*snapshot.Policy{p}, nil))
			if tt.prog != nil && tt.prog.n.Load() != 1 {
				t.Fatalf("when evaluated %d times, want once per request", tt.prog.n.Load())
			}
			decided, skip := h.st.When(p)
			if !decided || skip == tt.wantRun {
				t.Fatalf("When = decided %v skip %v, want run %v", decided, skip, tt.wantRun)
			}
			n := len(h.ev.list())
			if tt.wantRun && n != len(allClientPhases)+1 || !tt.wantRun && n != 0 { // +1: Finish
				t.Fatalf("calls %v (run %v)", h.ev.list(), tt.wantRun)
			}
			spans := h.tr.named("p")
			if tt.wantErr == "" {
				if len(h.st.fails) != 0 || pr.failures(phase.OnRequestHeaders, emit.ModeOpen)+pr.failures(phase.OnRequestHeaders, emit.ModeClosed) != 0 {
					t.Fatalf("failures recorded without a when error: %+v", h.st.fails)
				}
				for _, sp := range spans {
					if sp.attr(catalog.AttrFailureMode) != "" || sp.errType != "" {
						t.Fatalf("span %s marked without a when error: %+v %q", sp.ph, sp.attrs, sp.errType)
					}
				}
				return
			}
			applied := emit.ModeClosed
			if tt.mode == v1alpha1.FailureModeOpen {
				applied = emit.ModeOpen
			}
			if len(h.st.fails) != 1 || h.st.fails[0] != (failRec{"p", phase.OnRequestHeaders, tt.mode}) {
				t.Fatalf("RecordFailure = %+v, want one in onRequestHeaders %s", h.st.fails, tt.mode)
			}
			for ph := range phase.Count {
				for mode := range emit.NumModes {
					want := int64(0)
					if ph == phase.OnRequestHeaders && mode == applied {
						want = 1
					}
					if got := pr.failures(ph, mode); got != want {
						t.Fatalf("failures{phase=%s, mode=%d} = %d, want %d", ph, mode, got, want)
					}
				}
			}
			if len(spans) == 0 {
				t.Fatal("no span marks the when error")
			}
			first := spans[0]
			wantOutcome := emit.OutcomeContinue
			if !tt.wantRun {
				wantOutcome = emit.OutcomeSkipped
				if len(spans) != 1 {
					t.Fatalf("spans after an open skip: %d", len(spans))
				}
			}
			if first.ph != phase.OnRequestHeaders || first.ended != 1 || first.errType != tt.wantErr ||
				first.attr(catalog.AttrFailureMode) != string(tt.mode) || first.attr(catalog.AttrOutcome) != wantOutcome {
				t.Fatalf("first span %s: outcome %q mode %q error.type %q ended %d", first.ph,
					first.attr(catalog.AttrOutcome), first.attr(catalog.AttrFailureMode), first.errType, first.ended)
			}
			// Only the first span carries the mark.
			for _, sp := range spans[1:] {
				if sp.attr(catalog.AttrFailureMode) != "" || sp.errType != "" {
					t.Fatalf("span %s marked too: %+v", sp.ph, sp.attrs)
				}
			}
		})
	}
}

func TestWhenErrorMarksEveryCallOutcome(t *testing.T) {
	// Spec 03 req 43: under closed the Policy runs after a when error and
	// its call's span carries the applied mode and the error.type whatever
	// the call answers (respond, retry); a call that cannot decide itself
	// keeps its own error.type. In onLog there is no span, only the count.
	tests := []struct {
		name    string
		ph      phase.Phase
		res     result
		outcome string
		errType string
	}{
		{"respond", phase.OnRequestBody, respondWith(403, "RZ-AUTH-010"), emit.OutcomeRespond, errTypeCEL},
		{"retry", phase.OnUpstreamResponseHeaders, outcome(filter.Retry), emit.OutcomeContinue, errTypeCEL},
		{"cannot decide", phase.OnRoute, failWith(filter.ErrBudget), emit.OutcomeCannotDecide, errTypeBudget},
		{"onLog", phase.OnLog, nil, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			m, pr := newProbe()
			ch, p := h.single(spec{
				name: "p", class: phase.ClassTransform, when: &constProgram{err: newEvalError()},
				f: &fakeFilter{name: "p", ev: h.ev, script: map[phase.Phase]result{tt.ph: tt.res}},
			}, tt.ph)
			p.Metrics = m
			out := h.runPhase(tt.ph, ch, "")
			st := h.st
			if out.leg != nil {
				st = out.leg
			}
			if len(st.fails) == 0 || st.fails[0] != (failRec{"p", tt.ph, v1alpha1.FailureModeClosed}) || pr.failures(tt.ph, emit.ModeClosed) < 1 {
				t.Fatalf("when failure not recorded: %+v", st.fails)
			}
			if len(h.ev.list()) != 1 {
				t.Fatalf("calls %v, want the Policy to run once", h.ev.list())
			}
			spans := h.tr.named("p")
			if tt.outcome == "" {
				if len(spans) != 0 {
					t.Fatalf("onLog spans: %d", len(spans))
				}
				return
			}
			if len(spans) != 1 || spans[0].attr(catalog.AttrOutcome) != tt.outcome ||
				spans[0].attr(catalog.AttrFailureMode) != "closed" || spans[0].errType != tt.errType {
				t.Fatalf("span outcome %q mode %q error.type %q", spans[0].attr(catalog.AttrOutcome),
					spans[0].attr(catalog.AttrFailureMode), spans[0].errType)
			}
		})
	}
}

func TestWhenErrorCountsInThePhaseFirstReached(t *testing.T) {
	// Spec 03 req 43: the when failure's phase label is the Phase the
	// Policy is first reached in. A security-headers Policy whose
	// onRequestHeaders call never happened (auth responded 401) is first
	// reached in onResponse; its when panic is counted and logged there,
	// not in p.FirstPhase.
	rh := &recordHandler{}
	h := newHarness(t)
	h.e = New(Deps{Tracer: h.tr, Logger: slog.New(rh)})
	auth := policy(spec{
		name: "jwt", class: phase.ClassAuth, phases: []phase.Phase{phase.OnRequestHeaders},
		f: h.filter("jwt", map[phase.Phase]result{phase.OnRequestHeaders: respondWith(401, "RZ-AUTH-001")}),
	})
	sec := policy(spec{
		name: "sec", class: phase.ClassTransform, mode: v1alpha1.FailureModeOpen, when: &constProgram{panic: true},
		phases: []phase.Phase{phase.OnRequestHeaders, phase.OnResponse}, f: h.filter("sec", nil),
	})
	m, pr := newProbe()
	sec.Metrics = m
	if sec.FirstPhase != phase.OnRequestHeaders {
		t.Fatalf("FirstPhase = %s", sec.FirstPhase)
	}
	h.fullRun(chain([]*snapshot.Policy{auth, sec}, nil))
	if len(h.st.fails) != 1 || h.st.fails[0] != (failRec{"sec", phase.OnResponse, v1alpha1.FailureModeOpen}) {
		t.Fatalf("RecordFailure = %+v", h.st.fails)
	}
	if pr.failures(phase.OnResponse, emit.ModeOpen) != 1 || pr.failures(phase.OnRequestHeaders, emit.ModeOpen) != 0 {
		t.Fatal("when failure not counted in onResponse")
	}
	out := rh.text()
	if !strings.Contains(out, "filter panic recovered policy=sec phase=onResponse") {
		t.Fatalf("panic log does not name onResponse:\n%s", out)
	}
	if s := h.tr.named("sec"); len(s) != 1 || s[0].ph != phase.OnResponse || s[0].attr(catalog.AttrOutcome) != emit.OutcomeSkipped {
		t.Fatalf("sec spans %+v", s)
	}
}

func TestWhenFirstEncounterOnGeneratedResponse(t *testing.T) {
	// Spec 04 req 42: onResponse runs the full response chain on generated
	// responses, so a security-headers Policy that never reached its
	// onRequestHeaders call (an earlier auth Policy responded 401) still
	// runs in onResponse; its when is evaluated there, once.
	h := newHarness(t)
	auth := policy(spec{
		name: "jwt", class: phase.ClassAuth, phases: []phase.Phase{phase.OnRequestHeaders},
		f: h.filter("jwt", map[phase.Phase]result{phase.OnRequestHeaders: respondWith(401, "RZ-AUTH-001")}),
	})
	prog := &constProgram{ok: true}
	sec := policy(spec{
		name: "sec", class: phase.ClassTransform, when: prog,
		phases: []phase.Phase{phase.OnRequestHeaders, phase.OnResponse}, f: h.filter("sec", nil),
	})
	short, _ := h.fullRun(chain([]*snapshot.Policy{auth, sec}, nil))
	if got(short) != "401 RZ-AUTH-001" {
		t.Fatalf("short-circuit = %s", got(short))
	}
	want := []string{"jwt.onRequestHeaders", "sec.onResponse"}
	if evs := h.ev.list(); !slices.Equal(evs, want) || prog.n.Load() != 1 {
		t.Fatalf("events %v (when evaluated %d)", evs, prog.n.Load())
	}
}

func TestShortCircuitFromEachRequestPhase(t *testing.T) {
	// Spec 04 req 42: Respond in onRequestHeaders, onRequestBody or onRoute
	// skips to onResponse (minus transform.response, cache and
	// ai.semantic-cache) and onLog; the responding Policy and Phase feed the
	// access record and ruralz_filter_short_circuits_total.
	for _, ph := range []phase.Phase{phase.OnRequestHeaders, phase.OnRequestBody, phase.OnRoute} {
		t.Run(ph.String(), func(t *testing.T) {
			h := newHarness(t)
			m, pr := newProbe()
			resPhases := []phase.Phase{phase.OnResponse, phase.OnLog}
			cors := policy(spec{name: "cors", class: phase.ClassCORS, phases: []phase.Phase{phase.OnRequestHeaders, phase.OnResponse, phase.OnLog}, f: h.filter("cors", nil)})
			deny := policy(spec{
				name: "deny", class: phase.ClassAuthz, phases: []phase.Phase{ph, phase.OnLog},
				f: h.filter("deny", map[phase.Phase]result{ph: respondWith(403, "RZ-AUTH-010")}),
			})
			deny.Metrics = m
			cache := policy(spec{name: "cache", class: phase.ClassCache, typ: v1alpha1.PolicyTypeCache, phases: resPhases, f: h.filter("cache", nil)})
			tr := policy(spec{name: "tr", class: phase.ClassTransform, typ: v1alpha1.PolicyTypeTransformResponse, phases: resPhases, f: h.filter("tr", nil)})
			sem := policy(spec{name: "sem", class: phase.ClassCache, typ: v1alpha1.PolicyTypeAISemanticCache, phases: resPhases, f: h.filter("sem", nil)})
			hdr := policy(spec{name: "hdr", class: phase.ClassTransform, typ: v1alpha1.PolicyTypeHeaders, phases: resPhases, f: h.filter("hdr", nil)})
			later := policy(spec{name: "later", class: phase.ClassCustom, phases: []phase.Phase{phase.OnRoute}, f: h.filter("later", nil)})
			ch := chain([]*snapshot.Policy{cors, deny, cache, sem, tr, hdr, later}, nil)
			short, repl := h.fullRun(ch)
			if got(short) != "403 RZ-AUTH-010" || repl != nil {
				t.Fatalf("short %s repl %s", got(short), got(repl))
			}
			for _, e := range h.ev.list() {
				if strings.HasPrefix(e, "later.") || e == "cache.onResponse" || e == "tr.onResponse" || e == "sem.onResponse" {
					t.Fatalf("%s ran after the short-circuit: %v", e, h.ev.list())
				}
			}
			if slices.Index(h.ev.list(), "hdr.onResponse") < 0 || slices.Index(h.ev.list(), "cors.onResponse") < 0 {
				t.Fatalf("headers or CORS missing on the generated response: %v", h.ev.list())
			}
			if len(h.st.shorts) != 1 || h.st.shorts[0] != (shortRec{"deny", ph, 403}) {
				t.Fatalf("RecordShortCircuit = %+v", h.st.shorts)
			}
			if pr.short[ph].by[403] != 1 {
				t.Fatalf("short-circuit metric = %v", pr.short[ph].by)
			}
			// onLog runs for every Policy that ran or subscribes first there.
			for _, n := range []string{"cors.onLog", "deny.onLog", "hdr.onLog"} {
				if !slices.Contains(h.ev.list(), n) {
					t.Fatalf("%s missing: %v", n, h.ev.list())
				}
			}
		})
	}
}

func TestOnUpstreamRequestShortCircuitEndsOnlyTheLeg(t *testing.T) {
	// Spec 04 req 42: in onUpstreamRequest a short-circuit ends only that
	// leg; the other leg of the request runs its chain.
	h := newHarness(t)
	a := policy(spec{
		name: "a", class: phase.ClassUpstreamAuth, scope: phase.ScopeUpstream, phases: []phase.Phase{phase.OnUpstreamRequest},
		f: &fakeFilter{name: "a", ev: h.ev, script: map[phase.Phase]result{phase.OnUpstreamRequest: respondWith(204, "")}},
	})
	a2 := policy(spec{
		name: "a2", class: phase.ClassTransform, scope: phase.ScopeUpstream, phases: []phase.Phase{phase.OnUpstreamRequest},
		f: &fakeFilter{name: "a2", ev: h.ev},
	})
	b := policy(spec{
		name: "b", class: phase.ClassTransform, scope: phase.ScopeUpstream, phases: []phase.Phase{phase.OnUpstreamRequest},
		f: &fakeFilter{name: "b", ev: h.ev},
	})
	ch := chain(nil, map[string][]*snapshot.Policy{"up-a": {a, a2}, "up-b": {b}})
	ctx := context.Background()
	r := h.e.Begin(h.st, ch, nil)
	defer r.Release()
	la, lb := r.BeginLeg(ctx, "up-a", "s1"), r.BeginLeg(ctx, "up-b", "s2")
	if resp := la.OnUpstreamRequest(ctx, &filter.Leg{Upstream: "up-a", Step: "s1", Attempt: 1}); got(resp) != "204 " {
		t.Fatalf("leg a = %s", got(resp))
	}
	if resp := lb.OnUpstreamRequest(ctx, &filter.Leg{Upstream: "up-b", Step: "s2", Attempt: 1}); resp != nil {
		t.Fatalf("leg b = %s", got(resp))
	}
	la.End(ctx)
	lb.End(ctx)
	if evs := fmt.Sprint(h.ev.list()); evs != "[a.onUpstreamRequest b.onUpstreamRequest]" {
		t.Fatalf("events %s", evs)
	}
	legA := h.st.legs[0]
	if legA.upstream != "up-a" || legA.step != "s1" || len(legA.shorts) != 1 || legA.shorts[0].status != 204 {
		t.Fatalf("leg a state %+v", legA.shorts)
	}
	if len(h.st.shorts) != 0 {
		t.Fatal("a leg short-circuit was recorded on the client state")
	}
}

func TestFilterRetryOnlyInResponseHeaders(t *testing.T) {
	// R-44 and spec 05 req 33 ("or a Filter request"): Retry in
	// onUpstreamResponseHeaders asks for a retry and skips the rest of the
	// Phase for this attempt; the next attempt runs the chain again.
	h := newHarness(t)
	attempt := 0
	first := policy(spec{
		name: "first", class: phase.ClassTransform, scope: phase.ScopeUpstream,
		phases: []phase.Phase{phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders},
		f:      &fakeFilter{name: "first", ev: h.ev},
	})
	retrier := policy(spec{
		name: "retrier", class: phase.ClassCustom, scope: phase.ScopeUpstream,
		phases: []phase.Phase{phase.OnUpstreamResponseHeaders},
		f: &fakeFilter{name: "retrier", ev: h.ev, script: map[phase.Phase]result{
			phase.OnUpstreamResponseHeaders: func(_ context.Context, x filter.Exchange) filter.Result {
				if x.Leg().Attempt == 1 {
					return filter.RetryAttempt()
				}
				return filter.Next()
			},
		}},
	})
	ch := chain(nil, map[string][]*snapshot.Policy{"up": {first, retrier}})
	ctx := context.Background()
	r := h.e.Begin(h.st, ch, nil)
	defer r.Release()
	l := r.BeginLeg(ctx, "up", "")
	for attempt = 1; attempt <= 2; attempt++ {
		leg := &filter.Leg{Upstream: "up", Attempt: attempt}
		if resp := l.OnUpstreamRequest(ctx, leg); resp != nil {
			t.Fatal(got(resp))
		}
		rt, repl := l.OnUpstreamResponseHeaders(ctx, leg)
		if rt != (attempt == 1) || repl != nil {
			t.Fatalf("attempt %d: retry %v replace %s", attempt, rt, got(repl))
		}
	}
	l.End(ctx)
	want := []string{
		"first.onUpstreamRequest", "retrier.onUpstreamResponseHeaders",
		"first.onUpstreamRequest", "retrier.onUpstreamResponseHeaders", "first.onUpstreamResponseHeaders",
	}
	if evs := h.ev.list(); !slices.Equal(evs, want) {
		t.Fatalf("events %v, want %v", evs, want)
	}
	if h.st.legs[0].setLegs != 4 {
		t.Fatalf("SetLeg calls = %d, want one per hook", h.st.legs[0].setLegs)
	}
}

func TestAuthRuleEveryAuthPolicySkipped(t *testing.T) {
	// Security rule 1 (spec 06 rule 1, spec 04 req 41): a chain with
	// auth-class Policies whose when skipped every one fails 401
	// RZ-AUTH-001 with every auth Policy's challenge, decided after the
	// last auth-class Policy of onRequestHeaders; one running auth Policy
	// (or none in the chain) means no 401.
	tests := []struct {
		name     string
		whens    []bool // per auth Policy
		want     string
		authzRun bool
	}{
		{"all skipped", []bool{false, false}, "401 RZ-AUTH-001", false},
		{"one runs", []bool{false, true}, "no response", true},
		{"all run", []bool{true, true}, "no response", true},
		{"no auth Policy", nil, "no response", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			var ps []*snapshot.Policy
			ps = append(ps, policy(spec{name: "cors", class: phase.ClassCORS, phases: []phase.Phase{phase.OnRequestHeaders}, f: h.filter("cors", nil)}))
			for i, w := range tt.whens {
				name := fmt.Sprintf("auth%d", i)
				ps = append(ps, policy(spec{
					name: name, class: phase.ClassAuth, phases: []phase.Phase{phase.OnRequestHeaders},
					when: &constProgram{ok: w},
					f:    &challengeFilter{fakeFilter: *h.filter(name, nil), challenge: fmt.Sprintf("Scheme%d realm=\"ruralz\"", i)},
				}))
			}
			ps = append(ps, policy(spec{name: "authz", class: phase.ClassAuthz, phases: []phase.Phase{phase.OnRequestHeaders}, f: h.filter("authz", nil)}))
			ctx := context.Background()
			r := h.e.Begin(h.st, chain(ps, nil), nil)
			defer r.Release()
			resp := r.Request(ctx, phase.OnRequestHeaders)
			if got(resp) != tt.want {
				t.Fatalf("response = %s, want %s", got(resp), tt.want)
			}
			if ran := slices.Contains(h.ev.list(), "authz.onRequestHeaders"); ran != tt.authzRun {
				t.Fatalf("authz ran = %v: %v", ran, h.ev.list())
			}
			if resp != nil {
				if ch := resp.Header.Values("WWW-Authenticate"); len(ch) != 2 || ch[0] != `Scheme0 realm="ruralz"` {
					t.Fatalf("challenges = %q", ch)
				}
			}
		})
	}
}

func TestAuthRuleAtEndOfPhaseAndWithoutChallenges(t *testing.T) {
	// Rule 1 also applies when the auth-class Policies are the last of the
	// Phase; auth Filters without a challenge add no WWW-Authenticate.
	h := newHarness(t)
	a := policy(spec{
		name: "mtls", class: phase.ClassAuth, phases: []phase.Phase{phase.OnRequestHeaders},
		when: &constProgram{ok: false}, f: h.filter("mtls", nil),
	})
	r := h.e.Begin(h.st, chain([]*snapshot.Policy{a}, nil), nil)
	defer r.Release()
	resp := r.Request(context.Background(), phase.OnRequestHeaders)
	if got(resp) != "401 RZ-AUTH-001" || resp.Header != nil {
		t.Fatalf("response = %s header %v", got(resp), resp.Header)
	}
	// The 401 is generated: onResponse skips transform.response.
	if !r.generated {
		t.Fatal("the rule 1 response is not marked generated")
	}
	// An auth Policy not yet decided (a later Phase) prevents the 401.
	h = newHarness(t)
	late := policy(spec{name: "late", class: phase.ClassAuth, phases: []phase.Phase{phase.OnRequestBody}, f: h.filter("late", nil)})
	skipped := policy(spec{
		name: "skipped", class: phase.ClassAuth, phases: []phase.Phase{phase.OnRequestHeaders},
		when: &constProgram{ok: false}, f: h.filter("skipped", nil),
	})
	r2 := h.e.Begin(h.st, chain([]*snapshot.Policy{skipped, late}, nil), nil)
	defer r2.Release()
	if resp := r2.Request(context.Background(), phase.OnRequestHeaders); resp != nil {
		t.Fatalf("response = %s", got(resp))
	}
	// Inconsistent counts (AuthPolicies without auth-class Policies) never 401.
	ch := chain([]*snapshot.Policy{policy(spec{name: "x", phases: []phase.Phase{phase.OnRequestHeaders}, f: h.filter("x", nil)})}, nil)
	ch.AuthPolicies = 1
	r3 := h.e.Begin(h.st, ch, nil)
	defer r3.Release()
	if resp := r3.Request(context.Background(), phase.OnRequestHeaders); resp != nil {
		t.Fatalf("response = %s", got(resp))
	}
}

func TestOnResponseReplacementContinues(t *testing.T) {
	// Spec 07 req 17: after a closed failure replaces the response in
	// onResponse, the remaining Policies run on the generated response,
	// except transform.response, cache and ai.semantic-cache; a failure on
	// the generated response is counted and never replaces it again. The
	// replacement goes to Exchange.ReplaceResponse.
	h := newHarness(t)
	m, pr := newProbe()
	sec := policy(spec{
		name: "sec", class: phase.ClassTransform, phases: []phase.Phase{phase.OnResponse},
		f: h.filter("sec", map[phase.Phase]result{phase.OnResponse: undecided("")}),
	})
	sec.Metrics = m
	cache := policy(spec{name: "cache", class: phase.ClassCache, typ: v1alpha1.PolicyTypeCache, phases: []phase.Phase{phase.OnResponse}, f: h.filter("cache", nil)})
	failing := policy(spec{
		name: "failing", class: phase.ClassTransform, phases: []phase.Phase{phase.OnResponse},
		f: h.filter("failing", map[phase.Phase]result{phase.OnResponse: undecided("")}),
	})
	cors := policy(spec{name: "cors", class: phase.ClassCORS, phases: []phase.Phase{phase.OnResponse}, f: h.filter("cors", nil)})
	// Request order cors, cache, sec, failing: onResponse runs failing, sec,
	// cache, cors.
	ch := chain([]*snapshot.Policy{cors, cache, sec, failing}, nil)
	r := h.e.Begin(h.st, ch, nil)
	defer r.Release()
	repl := r.Response(context.Background(), ResponseUpstream)
	if got(repl) != "502 RZ-RT-012" || h.st.x.Replaced != repl {
		t.Fatalf("replacement %s (exchange %v)", got(repl), h.st.x.Replaced)
	}
	want := []string{"failing.onResponse", "sec.onResponse", "cors.onResponse"}
	if evs := h.ev.list(); !slices.Equal(evs, want) {
		t.Fatalf("events %v, want %v", evs, want)
	}
	if pr.failures(phase.OnResponse, emit.ModeClosed) != 1 || len(h.st.fails) != 2 {
		t.Fatalf("second failure not counted: %+v", h.st.fails)
	}
}

func TestResponseKinds(t *testing.T) {
	// Spec 04 req 42 and spec 05 req 87: on an Upstream failure (RZ-UP)
	// cache still runs (stale-if-error) while transform.response and
	// ai.semantic-cache do not; on an Upstream response every Policy runs.
	tests := []struct {
		kind ResponseKind
		want []string
	}{
		{ResponseUpstream, []string{"tr.onResponse", "sem.onResponse", "cache.onResponse", "cors.onResponse"}},
		{ResponseUpstreamError, []string{"cache.onResponse", "cors.onResponse"}},
		{ResponseGenerated, []string{"cors.onResponse"}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.kind), func(t *testing.T) {
			h := newHarness(t)
			on := []phase.Phase{phase.OnResponse}
			ps := []*snapshot.Policy{
				policy(spec{name: "cors", class: phase.ClassCORS, phases: on, f: h.filter("cors", nil)}),
				policy(spec{name: "cache", class: phase.ClassCache, typ: v1alpha1.PolicyTypeCache, phases: on, f: h.filter("cache", nil)}),
				policy(spec{name: "sem", class: phase.ClassCache, typ: v1alpha1.PolicyTypeAISemanticCache, phases: on, f: h.filter("sem", nil)}),
				policy(spec{name: "tr", class: phase.ClassTransform, typ: v1alpha1.PolicyTypeTransformResponse, phases: on, f: h.filter("tr", nil)}),
			}
			r := h.e.Begin(h.st, chain(ps, nil), nil)
			defer r.Release()
			r.Response(context.Background(), tt.kind)
			if evs := h.ev.list(); !slices.Equal(evs, tt.want) {
				t.Fatalf("events %v, want %v", evs, tt.want)
			}
		})
	}
}

func TestOnLogFailureIsTelemetryOnly(t *testing.T) {
	// FP 4 and spec 04 req 43: an onLog failure is counted and changes
	// nothing; onLog runs read-only with no span (req 44), even for a
	// Policy that responds or asks for a retry there.
	h := newHarness(t)
	m, pr := newProbe()
	p := policy(spec{
		name: "q", class: phase.ClassAdmission, phases: []phase.Phase{phase.OnLog},
		f: h.filter("q", map[phase.Phase]result{phase.OnLog: undecided("RZ-STS-002")}),
	})
	p.Metrics = m
	p2 := policy(spec{
		name: "r", class: phase.ClassCustom, phases: []phase.Phase{phase.OnLog},
		f: h.filter("r", map[phase.Phase]result{phase.OnLog: respondWith(500, "")}),
	})
	r := h.e.Begin(h.st, chain([]*snapshot.Policy{p, p2}, nil), nil)
	defer r.Release()
	r.Log(context.Background())
	r.Log(context.Background()) // a second call does nothing
	if pr.failures(phase.OnLog, emit.ModeClosed) != 1 || pr.dur[phase.OnLog].n.Load() != 1 {
		t.Fatalf("onLog metrics: failures %d, durations %d", pr.failures(phase.OnLog, emit.ModeClosed), pr.dur[phase.OnLog].n.Load())
	}
	if len(h.tr.spans) != 0 {
		t.Fatalf("onLog created %d spans", len(h.tr.spans))
	}
	if len(h.st.shorts) != 0 || len(h.st.fails) != 2 || h.st.x.Replaced != nil {
		t.Fatalf("onLog changed the response: %+v %+v", h.st.shorts, h.st.fails)
	}
}

func TestFinishForEveryFilterThatRan(t *testing.T) {
	// R-40 and filter.Finisher: Finish runs after onLog, in request order,
	// for every Finisher that ran in at least one Phase (subscribed to onLog
	// or not), never for one skipped by when or never reached, and not for
	// Upstream-scope Policies (they finish at the leg's End).
	h := newHarness(t)
	fin := func(name string, script map[phase.Phase]result) *finishFilter {
		return &finishFilter{*h.filter(name, script)}
	}
	a := policy(spec{name: "a", class: phase.ClassCORS, phases: []phase.Phase{phase.OnRequestHeaders}, f: fin("a", nil)})
	b := policy(spec{name: "b", class: phase.ClassAuthz, phases: []phase.Phase{phase.OnRequestHeaders}, when: &constProgram{ok: false}, f: fin("b", nil)})
	c := policy(spec{
		name: "c", class: phase.ClassCache, phases: []phase.Phase{phase.OnRequestHeaders, phase.OnResponse},
		f: fin("c", map[phase.Phase]result{phase.OnRequestHeaders: respondWith(200, "")}),
	})
	d := policy(spec{name: "d", class: phase.ClassTransform, phases: []phase.Phase{phase.OnRoute}, f: fin("d", nil)})
	e := policy(spec{name: "e", class: phase.ClassCustom, phases: []phase.Phase{phase.OnRequestHeaders}, f: h.filter("e", nil)})
	up := policy(spec{name: "up", class: phase.ClassUpstreamAuth, scope: phase.ScopeUpstream, phases: []phase.Phase{phase.OnUpstreamRequest}, f: fin("up", nil)})
	ch := chain([]*snapshot.Policy{a, b, c, d, e, up}, nil)
	short, _ := h.fullRun(ch)
	if got(short) != "200 " {
		t.Fatalf("short = %s", got(short))
	}
	if fins := h.ev.with(""); !slices.Equal(filterSuffix(fins, ".finish"), []string{"a.finish", "c.finish"}) {
		t.Fatalf("finish calls %v", fins)
	}
	// Finish comes after onLog: last events.
	evs := h.ev.list()
	if evs[len(evs)-1] != "c.finish" {
		t.Fatalf("events %v", evs)
	}
}

func filterSuffix(evs []string, suffix string) []string {
	var out []string
	for _, e := range evs {
		if strings.HasSuffix(e, suffix) {
			out = append(out, e)
		}
	}
	return out
}

func TestMetricsAndSpans(t *testing.T) {
	// Spec 04 req 44: every call records ruralz_filter_duration_seconds on
	// the request's stripe; sampled requests get one ruralz.filter.<name>
	// span per Policy Phase call with type, Phase, outcome and the applied
	// failureMode; onLog creates none; spans carry only the RZ code (spec 06
	// rule 11).
	h := newHarness(t)
	m, pr := newProbe()
	p := policy(spec{
		name: "hdr", class: phase.ClassTransform, mode: v1alpha1.FailureModeOpen,
		phases: []phase.Phase{phase.OnRequestHeaders, phase.OnRoute, phase.OnResponse, phase.OnLog},
		f:      h.filter("hdr", map[phase.Phase]result{phase.OnRoute: undecided("")}),
	})
	p.Metrics = m
	h.fullRun(chain([]*snapshot.Policy{p}, nil))
	spans := h.tr.named("hdr")
	if len(spans) != 2 {
		t.Fatalf("hdr spans = %d, want 2 (onRequestHeaders, onRoute; open skips the rest, onLog none)", len(spans))
	}
	if s := spans[0]; s.ph != phase.OnRequestHeaders || s.policyTyp != "headers" || s.attr(catalog.AttrOutcome) != emit.OutcomeContinue || s.ended != 1 {
		t.Fatalf("first span %+v", s)
	}
	if s := spans[1]; s.attr(catalog.AttrOutcome) != emit.OutcomeCannotDecide || s.attr(catalog.AttrFailureMode) != "open" || s.errType != "internal" {
		t.Fatalf("second span %+v", s.attrs)
	}
	h = newHarness(t)
	d := policy(spec{
		name: "deny", class: phase.ClassAuthz, phases: []phase.Phase{phase.OnRequestBody},
		f: h.filter("deny", map[phase.Phase]result{phase.OnRequestBody: respondWith(403, "RZ-AUTH-010")}),
	})
	h.fullRun(chain([]*snapshot.Policy{d}, nil))
	ds := h.tr.named("deny")
	if len(ds) != 1 || ds[0].attr(catalog.AttrOutcome) != emit.OutcomeRespond || ds[0].status != 403 || ds[0].code != "RZ-AUTH-010" {
		t.Fatalf("deny span %+v", ds)
	}
	if pr.dur[phase.OnRequestHeaders].n.Load() != 1 || pr.dur[phase.OnRoute].n.Load() != 1 || pr.dur[phase.OnRequestHeaders].stripe.Load() != 3 {
		t.Fatal("durations not recorded on the request stripe")
	}
	if pr.failures(phase.OnRoute, emit.ModeOpen) != 1 {
		t.Fatal("open failure not counted")
	}
}

func TestNilMetricsAndNilTracer(t *testing.T) {
	// Policies compiled without handles and an executor without a tracer
	// still run (tests, replay).
	e := New(Deps{Logger: slog.New(slog.DiscardHandler)})
	st := newState()
	ev := &events{}
	p := policy(spec{
		name: "p", phases: []phase.Phase{phase.OnRequestHeaders, phase.OnLog},
		f: &fakeFilter{name: "p", ev: ev, script: map[phase.Phase]result{phase.OnRequestHeaders: respondWith(http.StatusTeapot, "")}},
	})
	r := e.Begin(st, chain([]*snapshot.Policy{p}, nil), nil)
	defer r.Release()
	if resp := r.Request(context.Background(), phase.OnRequestHeaders); resp == nil || resp.Status != http.StatusTeapot {
		t.Fatalf("response %s", got(resp))
	}
	r.Log(context.Background())
	if st.span != nil {
		t.Fatal("a span was entered without a tracer")
	}
}
