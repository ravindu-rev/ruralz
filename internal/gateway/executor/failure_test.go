// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the failureMode table (spec 04 req 43 with the FP 8.10 rows,
// spec 07 reqs 14 to 16, spec 08 reqs 36 and 38, OQ-data-plane-8 (a)),
// the SPI sentinels (spec 07 req 15) and the metrics every cannot decide
// records (spec 04 req 44, spec 07 req 19).

var errUndecidedTest = errors.New("test: cannot decide")

// harness runs one chain on a fake client state.
type harness struct {
	t     *testing.T
	e     *Executor
	st    *fakeState
	ev    *events
	tr    *fakeTracer
	store *filtertest.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	tr := &fakeTracer{}
	return &harness{
		t: t, e: New(Deps{Tracer: tr}), st: newState(), ev: &events{}, tr: tr,
		store: &filtertest.Store{},
	}
}

// filter returns a fakeFilter checking Enter on the client state.
func (h *harness) filter(name string, script map[phase.Phase]result) *fakeFilter {
	return &fakeFilter{name: name, ev: h.ev, script: script, st: h.st, t: h.t}
}

// outcome of running one Phase.
type ran struct {
	resp  *filter.Response
	retry bool
	end   bool
	// leg is the leg state of an upstream-leg Phase.
	leg *fakeState
}

// runPhase runs ph over ch: client Phases on the client state, leg Phases
// on one leg to "up" (step names a composition step).
func (h *harness) runPhase(ph phase.Phase, ch *snapshot.Chain, step string) ran {
	ctx := context.Background()
	r := h.e.Begin(h.st, ch, h.store)
	defer r.Release()
	var out ran
	switch ph {
	case phase.OnRequestHeaders, phase.OnRequestBody, phase.OnRoute:
		out.resp = r.Request(ctx, ph)
	case phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders, phase.OnUpstreamResponseBody:
		l := r.BeginLeg(ctx, "up", step)
		leg := &filter.Leg{Upstream: "up", Step: step, Attempt: 1}
		switch ph {
		case phase.OnUpstreamRequest:
			out.resp = l.OnUpstreamRequest(ctx, leg)
		case phase.OnUpstreamResponseHeaders:
			out.retry, out.resp = l.OnUpstreamResponseHeaders(ctx, leg)
		default:
			out.resp = l.OnUpstreamResponseBody(ctx, leg)
		}
		l.End(ctx)
		if n := len(h.st.legs); n > 0 {
			out.leg = h.st.legs[n-1]
		}
	case phase.OnResponse:
		out.resp = r.Response(ctx, ResponseUpstream)
	case phase.OnLog:
		r.Log(ctx)
	case phase.OnChunk:
		out.end = r.Chunk(ctx)
	default:
		h.t.Fatalf("phase %v", ph)
	}
	return out
}

// single compiles one Policy subscribed to ph: Upstream scope for the leg
// Phases, Route scope otherwise.
func (h *harness) single(s spec, ph phase.Phase) (*snapshot.Chain, *snapshot.Policy) {
	s.phases = []phase.Phase{ph}
	if ph >= phase.OnUpstreamRequest && ph <= phase.OnUpstreamResponseBody {
		s.scope = phase.ScopeUpstream
		p := policy(s)
		return chain(nil, map[string][]*snapshot.Policy{"up": {p}}), p
	}
	p := policy(s)
	return chain([]*snapshot.Policy{p}, nil), p
}

type want struct {
	status int
	code   string
}

func (w want) String() string { return fmt.Sprintf("%d %s", w.status, w.code) }

func got(r *filter.Response) string {
	if r == nil {
		return "no response"
	}
	return want{r.Status, r.Code}.String()
}

// requestRow is the "Request Phase, closed" column: the status by class and
// the class default code when the Filter supplies none.
func requestRow(c phase.Class) want {
	switch c {
	case phase.ClassAuth:
		return want{401, CodeAuthUndecided}
	case phase.ClassAuthz:
		return want{403, "RZ-AUTH-015"}
	case phase.ClassUpstreamAuth:
		return want{401, "RZ-AUTH-020"}
	case phase.ClassCustom:
		return want{503, "RZ-PLG-001"}
	default: // cors, admission, validation, cache, transform
		return want{503, "RZ-RT-011"}
	}
}

// responseRow is the "Response Phase before commit, closed" column; ok is
// false when the response passes (cache: store skipped).
func responseRow(c phase.Class) (want, bool) {
	switch c {
	case phase.ClassCache:
		return want{}, false
	case phase.ClassCustom:
		return want{502, "RZ-PLG-001"}, true
	default:
		return want{502, "RZ-RT-012"}, true
	}
}

// closedOnlyClass reports the classes the data plane makes closed only
// (RZ-CFG-029; spec 06 rule 1): auth, authz and upstream-auth.
func closedOnlyClass(c phase.Class) bool {
	return c == phase.ClassAuth || c == phase.ClassAuthz || c == phase.ClassUpstreamAuth
}

func TestFailureModeTableEveryClassPhaseMode(t *testing.T) {
	// Spec 04 req 43: every Filter class x Phase x failureMode cell with
	// the Filter supplying no code (the class default applies, spec 07 req
	// 14). Also req 44: each cannot decide counts
	// ruralz_filter_failures_total{mode} as applied and feeds the access
	// record (RecordFailure). The security classes apply closed even when
	// configured open (spec 06 rule 1, defense in depth behind RZ-CFG-029).
	modes := []v1alpha1.FailureMode{v1alpha1.FailureModeOpen, v1alpha1.FailureModeClosed}
	for class := range phase.NumClasses {
		for ph := range phase.Count {
			for _, configured := range modes {
				mode := configured
				if closedOnlyClass(class) {
					mode = v1alpha1.FailureModeClosed
				}
				t.Run(fmt.Sprintf("%s/%s/%s", class, ph, configured), func(t *testing.T) {
					h := newHarness(t)
					m, pr := newProbe()
					ch, p := h.single(spec{
						name: "p", class: class, mode: configured,
						f: h.filter("p", map[phase.Phase]result{ph: undecided("")}),
					}, ph)
					p.Metrics = m
					if ph.UpstreamLeg() && ph != phase.OnChunk {
						p.Filter.(*fakeFilter).st = nil // entered on the leg state
					}
					out := h.runPhase(ph, ch, "")

					var wantResp *want
					wantEnd := false
					if mode == v1alpha1.FailureModeClosed {
						switch {
						case ph == phase.OnLog:
						case ph == phase.OnChunk:
							wantEnd = true
						case ph.CanShortCircuit():
							w := requestRow(class)
							wantResp = &w
						default:
							if w, ok := responseRow(class); ok {
								wantResp = &w
							}
						}
					}
					if wantResp == nil && out.resp != nil || wantResp != nil && got(out.resp) != wantResp.String() {
						t.Fatalf("response = %s, want %v", got(out.resp), wantResp)
					}
					if out.end != wantEnd || out.retry {
						t.Fatalf("end = %v retry = %v, want end %v", out.end, out.retry, wantEnd)
					}
					applied := emit.ModeClosed
					if mode == v1alpha1.FailureModeOpen {
						applied = emit.ModeOpen
					}
					if pr.failures(ph, applied) != 1 || pr.failures(ph, 1-applied) != 0 {
						t.Fatalf("failures{mode} = open %d closed %d, want 1 for %s",
							pr.failures(ph, emit.ModeOpen), pr.failures(ph, emit.ModeClosed), mode)
					}
					if pr.dur[ph].n.Load() != 1 {
						t.Fatalf("duration observations = %d, want 1", pr.dur[ph].n.Load())
					}
					st := h.st
					if out.leg != nil {
						st = out.leg
					}
					if len(st.fails) != 1 || st.fails[0] != (failRec{"p", ph, mode}) {
						t.Fatalf("RecordFailure = %+v", st.fails)
					}
					if len(st.shorts) != 0 {
						t.Fatalf("a failure recorded a short-circuit: %+v", st.shorts)
					}
				})
			}
		}
	}
}

func TestSecurityClassesAreClosedOnly(t *testing.T) {
	// Data plane "Security types are closed only (RZ-CFG-029)" and spec 06
	// rule 1, defense in depth: a security-class Policy that reaches the
	// executor configured open still applies closed. One that cannot decide
	// (JWKS unreachable) gets the class status and is never skipped for its
	// later Phases; one whose when errs runs, so it never counts as skipped
	// for Security rule 1 (spec 03 req 43).
	tests := []struct {
		name  string
		class phase.Class
		ph    phase.Phase
		when  bool
		res   result
		want  string
	}{
		{"auth cannot decide", phase.ClassAuth, phase.OnRequestHeaders, false, undecided(""), "401 " + CodeAuthUndecided},
		{"auth when error", phase.ClassAuth, phase.OnRequestHeaders, true, respondWith(401, "RZ-AUTH-006"), "401 RZ-AUTH-006"},
		// Not skipped, so Security rule 1 (401 RZ-AUTH-001) does not fire.
		{"auth when error continues", phase.ClassAuth, phase.OnRequestHeaders, true, nil, "no response"},
		{"authz cannot decide", phase.ClassAuthz, phase.OnRequestBody, false, undecided(""), "403 RZ-AUTH-015"},
		{"authz when error", phase.ClassAuthz, phase.OnRequestHeaders, true, nil, "no response"},
		{"upstream-auth cannot decide", phase.ClassUpstreamAuth, phase.OnUpstreamRequest, false, undecided(""), "401 RZ-AUTH-020"},
		{"upstream-auth when error", phase.ClassUpstreamAuth, phase.OnUpstreamRequest, true, nil, "no response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			m, pr := newProbe()
			s := spec{
				name: "p", class: tt.class, mode: v1alpha1.FailureModeOpen,
				f: &fakeFilter{name: "p", ev: h.ev, script: map[phase.Phase]result{tt.ph: tt.res}},
			}
			if tt.when {
				s.when = &constProgram{err: newEvalError()}
			}
			ch, p := h.single(s, tt.ph)
			p.Metrics = m
			out := h.runPhase(tt.ph, ch, "")
			if got(out.resp) != tt.want {
				t.Fatalf("response = %s, want %s", got(out.resp), tt.want)
			}
			if calls := h.ev.list(); len(calls) != 1 {
				t.Fatalf("calls %v: the Policy was skipped", calls)
			}
			st := h.st
			if out.leg != nil {
				st = out.leg
			}
			if decided, skip := st.When(p); !decided || skip {
				t.Fatalf("When = decided %v skip %v: counted as skipped", decided, skip)
			}
			if len(st.fails) != 1 || st.fails[0].mode != v1alpha1.FailureModeClosed {
				t.Fatalf("RecordFailure = %+v, want one closed", st.fails)
			}
			if pr.failures(tt.ph, emit.ModeClosed) != 1 || pr.failures(tt.ph, emit.ModeOpen) != 0 {
				t.Fatal("failures{mode} not counted as closed")
			}
		})
	}

	// The auth Filter that could not decide is not put on the failed-open
	// list: it still runs in its later Phase.
	h := newHarness(t)
	p := policy(spec{
		name: "jwt", class: phase.ClassAuth, mode: v1alpha1.FailureModeOpen,
		phases: []phase.Phase{phase.OnRequestHeaders, phase.OnResponse},
		f:      h.filter("jwt", map[phase.Phase]result{phase.OnRequestHeaders: undecided("")}),
	})
	short, _ := h.fullRun(chain([]*snapshot.Policy{p}, nil))
	if got(short) != "401 "+CodeAuthUndecided {
		t.Fatalf("short-circuit = %s", got(short))
	}
	if evs := fmt.Sprint(h.ev.list()); evs != "[jwt.onRequestHeaders jwt.onResponse]" {
		t.Fatalf("events = %s", evs)
	}
}

func TestFailureModeFilterCodePassesThrough(t *testing.T) {
	// Spec 04 req 43: "code from the Filter when it supplies one, else the
	// class default"; the status stays the class's (test plan item 7:
	// RZ-STS-001, RZ-RL-005 and RZ-PLG passthrough, RZ-AUTH-015 default).
	tests := []struct {
		class phase.Class
		ph    phase.Phase
		code  string
		want  want
	}{
		{phase.ClassAdmission, phase.OnRequestHeaders, "RZ-STS-001", want{503, "RZ-STS-001"}},
		{phase.ClassAdmission, phase.OnRequestHeaders, "RZ-RL-005", want{503, "RZ-RL-005"}},
		{phase.ClassCache, phase.OnRequestHeaders, "RZ-STS-003", want{503, "RZ-STS-003"}},
		{phase.ClassCustom, phase.OnRequestHeaders, "RZ-PLG-002", want{503, "RZ-PLG-002"}},
		{phase.ClassCustom, phase.OnResponse, "RZ-PLG-003", want{502, "RZ-PLG-003"}},
		{phase.ClassAuth, phase.OnRequestHeaders, "RZ-AUTH-006", want{401, "RZ-AUTH-006"}},
		{phase.ClassAuthz, phase.OnRequestBody, "RZ-AUTH-013", want{403, "RZ-AUTH-013"}},
		{phase.ClassAuthz, phase.OnRequestHeaders, "", want{403, "RZ-AUTH-015"}},
		{phase.ClassUpstreamAuth, phase.OnUpstreamRequest, "", want{401, "RZ-AUTH-020"}},
		{phase.ClassValidation, phase.OnResponse, "RZ-AI-010", want{502, "RZ-AI-010"}},
		{phase.ClassTransform, phase.OnUpstreamResponseBody, "", want{502, "RZ-RT-012"}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%s/%s", tt.class, tt.ph, tt.code), func(t *testing.T) {
			h := newHarness(t)
			ch, _ := h.single(spec{
				name: "p", class: tt.class,
				f: &fakeFilter{name: "p", ev: h.ev, script: map[phase.Phase]result{tt.ph: undecided(tt.code)}},
			}, tt.ph)
			if out := h.runPhase(tt.ph, ch, ""); got(out.resp) != tt.want.String() {
				t.Fatalf("response = %s, want %s", got(out.resp), tt.want)
			}
		})
	}
}

func TestStatusesComeFromTheRegistry(t *testing.T) {
	// Spec 07 req 14 and OQ-data-plane-8: statuses of generated codes come
	// from errcode, so a registry row change moves them.
	e := New(Deps{})
	for _, tc := range []struct {
		code string
		got  int
	}{
		{CodeAuthSkipped, e.authSkippedStatus},
		{CodeBudget, e.budgetStatus},
		{CodeRequestTooLarge, e.requestLargeStatus},
		{CodeResponseTooLarge, e.responseLargeStatus},
		{CodeStepFailed, e.stepFailedStatus},
		{CodeUpstreamAuth, e.rules[phase.ClassUpstreamAuth].reqStatus},
		{CodeUndecided, e.rules[phase.ClassTransform].reqStatus},
		{CodeResponseFailed, e.rules[phase.ClassTransform].respStatus},
		{CodeAuthzUndecided, e.rules[phase.ClassAuthz].reqStatus},
		{CodeAuthUndecided, e.rules[phase.ClassAuth].reqStatus},
	} {
		if want := errcode.Status(tc.code); want == 0 || tc.got != want {
			t.Errorf("status of %s = %d, registry says %d", tc.code, tc.got, want)
		}
	}
	for _, c := range []string{
		CodeAuthSkipped, CodeAuthUndecided, CodeAuthzUndecided, CodeUpstreamAuth,
		CodeRequestTooLarge, CodeBudget, CodeUndecided, CodeResponseFailed, CodeStepFailed,
		CodeResponseTooLarge, CodePluginFault,
	} {
		if _, ok := errcode.Lookup(c); !ok {
			t.Errorf("%s is not registered", c)
		}
	}
	// An unknown class takes the custom row.
	if e.rule(phase.NumClasses+3) != e.rules[phase.ClassCustom] {
		t.Error("an unknown class does not take the custom row")
	}
}

func TestSentinelsIgnoreFailureMode(t *testing.T) {
	// Spec 07 req 15: filter.ErrBudget is 503 RZ-RT-004 under either
	// failureMode in any Phase before commit; filter.ErrTooLarge is 413
	// RZ-RT-003 on the request side, 502 RZ-UP-010 on a plain upstreams
	// response and 502 RZ-RT-015 on a composition step (T8).
	tests := []struct {
		ph   phase.Phase
		err  error
		step string
		want want
	}{
		{phase.OnRequestHeaders, filter.ErrBudget, "", want{503, "RZ-RT-004"}},
		{phase.OnRequestBody, filter.ErrBudget, "", want{503, "RZ-RT-004"}},
		{phase.OnUpstreamRequest, filter.ErrBudget, "", want{503, "RZ-RT-004"}},
		{phase.OnUpstreamResponseBody, filter.ErrBudget, "", want{503, "RZ-RT-004"}},
		{phase.OnResponse, filter.ErrBudget, "", want{503, "RZ-RT-004"}},
		{phase.OnRequestBody, filter.ErrTooLarge, "", want{413, "RZ-RT-003"}},
		{phase.OnUpstreamRequest, filter.ErrTooLarge, "", want{413, "RZ-RT-003"}},
		{phase.OnUpstreamRequest, filter.ErrTooLarge, "step-a", want{502, "RZ-RT-015"}},
		{phase.OnUpstreamResponseBody, filter.ErrTooLarge, "", want{502, "RZ-UP-010"}},
		{phase.OnUpstreamResponseBody, filter.ErrTooLarge, "step-a", want{502, "RZ-RT-015"}},
		{phase.OnResponse, filter.ErrTooLarge, "", want{502, "RZ-UP-010"}},
	}
	for _, tt := range tests {
		for _, mode := range []v1alpha1.FailureMode{v1alpha1.FailureModeOpen, v1alpha1.FailureModeClosed} {
			t.Run(fmt.Sprintf("%s/%v/%s/%s", tt.ph, tt.err, tt.step, mode), func(t *testing.T) {
				h := newHarness(t)
				wrapped := fmt.Errorf("transform: %w", tt.err)
				m, pr := newProbe()
				ch, p := h.single(spec{
					name: "p", class: phase.ClassCache, mode: mode,
					f: &fakeFilter{name: "p", ev: h.ev, script: map[phase.Phase]result{tt.ph: failWith(wrapped)}},
				}, tt.ph)
				p.Metrics = m
				out := h.runPhase(tt.ph, ch, tt.step)
				if got(out.resp) != tt.want.String() {
					t.Fatalf("response = %s, want %s", got(out.resp), tt.want)
				}
				if pr.failures(tt.ph, emit.ModeClosed) != 1 {
					t.Fatal("a sentinel does not count as a closed failure")
				}
			})
		}
	}
}

func TestSentinelOnLogAndGeneratedResponse(t *testing.T) {
	// FP 4: an onLog failure reaches telemetry only, sentinel included;
	// spec 07 req 17: a failure on an already generated response is
	// counted and does not replace it again.
	h := newHarness(t)
	ch, _ := h.single(spec{name: "p", f: &fakeFilter{
		name: "p", ev: h.ev,
		script: map[phase.Phase]result{phase.OnLog: failWith(filter.ErrBudget)},
	}}, phase.OnLog)
	if out := h.runPhase(phase.OnLog, ch, ""); out.resp != nil {
		t.Fatal("onLog produced a response")
	}

	h = newHarness(t)
	ch, _ = h.single(spec{name: "p", f: &fakeFilter{
		name: "p", ev: h.ev,
		script: map[phase.Phase]result{phase.OnResponse: failWith(filter.ErrBudget)},
	}}, phase.OnResponse)
	r := h.e.Begin(h.st, ch, nil)
	defer r.Release()
	if resp := r.Response(context.Background(), ResponseGenerated); resp != nil || h.st.x.Replaced != nil {
		t.Fatal("a generated response was replaced")
	}
	if len(h.st.fails) != 1 {
		t.Fatalf("failure not recorded: %+v", h.st.fails)
	}
}

func TestOpenSkipsTheRemainingPhases(t *testing.T) {
	// Spec 04 req 43: "open skips the Policy for all its remaining Phases
	// of this request" (test plan item 7); the Policy still ran, so it gets
	// Finish (R-40).
	h := newHarness(t)
	f := &finishFilter{fakeFilter{
		name: "hdr", ev: h.ev,
		script: map[phase.Phase]result{phase.OnRequestHeaders: undecided("")},
	}}
	p := policy(spec{
		name: "hdr", class: phase.ClassTransform, mode: v1alpha1.FailureModeOpen, f: f,
		phases: []phase.Phase{phase.OnRequestHeaders, phase.OnRoute, phase.OnResponse, phase.OnLog},
	})
	ch := chain([]*snapshot.Policy{p}, nil)
	ctx := context.Background()
	r := h.e.Begin(h.st, ch, nil)
	defer r.Release()
	for _, ph := range []phase.Phase{phase.OnRequestHeaders, phase.OnRequestBody, phase.OnRoute} {
		if resp := r.Request(ctx, ph); resp != nil {
			t.Fatalf("%s responded %s under open", ph, got(resp))
		}
	}
	r.Response(ctx, ResponseUpstream)
	r.Log(ctx)
	want := []string{"hdr.onRequestHeaders", "hdr.finish"}
	if evs := h.ev.list(); fmt.Sprint(evs) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", evs, want)
	}
}

func TestInvalidOutcomesAreCannotDecide(t *testing.T) {
	// R-44: Retry outside onUpstreamResponseHeaders is cannot decide;
	// Respond outside the four request Phases (spec 04 req 42) or without a
	// Response, and an unknown outcome, are too.
	tests := []struct {
		name string
		ph   phase.Phase
		res  result
		want want
	}{
		{"retry in onRequestHeaders", phase.OnRequestHeaders, outcome(filter.Retry), want{503, "RZ-RT-011"}},
		{"retry in onUpstreamRequest", phase.OnUpstreamRequest, outcome(filter.Retry), want{503, "RZ-RT-011"}},
		{"retry in onUpstreamResponseBody", phase.OnUpstreamResponseBody, outcome(filter.Retry), want{502, "RZ-RT-012"}},
		{"respond in onResponse", phase.OnResponse, respondWith(403, "RZ-RT-008"), want{502, "RZ-RT-012"}},
		{"respond in onUpstreamResponseHeaders", phase.OnUpstreamResponseHeaders, respondWith(403, "RZ-RT-008"), want{502, "RZ-RT-012"}},
		{"respond without response", phase.OnRoute, outcome(filter.Respond), want{503, "RZ-RT-011"}},
		{"respond with status 0", phase.OnRequestHeaders, respondWith(0, "RZ-RT-008"), want{503, "RZ-RT-011"}},
		{"respond with status 100", phase.OnUpstreamRequest, respondWith(100, ""), want{503, "RZ-RT-011"}},
		{"unknown outcome", phase.OnRequestBody, outcome(filter.Outcome(9)), want{503, "RZ-RT-011"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			ch, _ := h.single(spec{
				name: "p", class: phase.ClassTransform,
				f: &fakeFilter{name: "p", ev: h.ev, script: map[phase.Phase]result{tt.ph: tt.res}},
			}, tt.ph)
			out := h.runPhase(tt.ph, ch, "")
			if got(out.resp) != tt.want.String() || out.retry {
				t.Fatalf("response = %s retry = %v, want %s", got(out.resp), out.retry, tt.want)
			}
			spans := h.tr.named("p")
			if len(spans) != 1 || spans[0].errType != errTypeBadResult {
				t.Fatalf("span error.type = %+v", spans)
			}
		})
	}
}

func TestPanicsAreCannotDecide(t *testing.T) {
	// Spec 04 req 43: a recovered panic is cannot decide (test plan item 7:
	// "a panicking Filter becomes cannot-decide").
	for _, mode := range []v1alpha1.FailureMode{v1alpha1.FailureModeOpen, v1alpha1.FailureModeClosed} {
		t.Run(string(mode), func(t *testing.T) {
			h := newHarness(t)
			ch, _ := h.single(spec{
				name: "p", class: phase.ClassValidation, mode: mode,
				f: &fakeFilter{name: "p", ev: h.ev, script: map[phase.Phase]result{phase.OnRequestBody: panics}},
			}, phase.OnRequestBody)
			out := h.runPhase(phase.OnRequestBody, ch, "")
			if mode == v1alpha1.FailureModeOpen && out.resp != nil ||
				mode == v1alpha1.FailureModeClosed && got(out.resp) != (want{503, "RZ-RT-011"}).String() {
				t.Fatalf("response = %s", got(out.resp))
			}
			if s := h.tr.named("p"); len(s) != 1 || s[0].errType != errTypeInternal || s[0].ended != 1 {
				t.Fatalf("span = %+v", s)
			}
		})
	}
}

func TestErrorTypes(t *testing.T) {
	// Spec 04 req 44 and spec 07 req 86: error.type is a fixed word, never
	// request data; a Filter may name it through an ErrorType method, from
	// the fixed set only.
	tests := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{ErrPanic, "internal"},
		{fmt.Errorf("x: %w", ErrInvalidResult), "invalid_result"},
		{filter.ErrBudget, "buffer_budget"},
		{filter.ErrTooLarge, "too_large"},
		{fmt.Errorf("key: %w", newEvalError()), "cel_error"},
		{statestore.ErrTimeout, "state_store"},
		{fmt.Errorf("wrapped: %w", typedErr("path_error")), "path_error"},
		{typedErr("output_cap"), "output_cap"},
		{typedErr("result_type"), "result_type"},
		{typedErr("state_store"), "state_store"},
		// Spec 06 rule 11, spec 07 req 86: a word outside the fixed set,
		// which may hold request data, becomes "internal".
		{typedErr("user alice@example.com not found"), "internal"},
		{typedErr(""), "internal"},
		{errors.New("secret value abc"), "internal"},
	}
	for _, tt := range tests {
		if got := errorType(tt.err); got != tt.want {
			t.Errorf("errorType(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

// typedErr is a Filter error naming its own error.type.
type typedErr string

func (typedErr) Error() string       { return "path through a non-object" }
func (t typedErr) ErrorType() string { return string(t) }

func TestLegClosedFailureEndsOnlyTheLeg(t *testing.T) {
	// Spec 07 req 16: a closed failure in onUpstreamRequest ends only that
	// leg with 503 RZ-RT-011 and no retry; in onUpstreamResponseHeaders it
	// replaces the leg's response with 502 RZ-RT-012 and stops the Phase.
	h := newHarness(t)
	a := policy(spec{
		name: "a", class: phase.ClassTransform, scope: phase.ScopeUpstream,
		phases: []phase.Phase{phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders},
		f: &fakeFilter{name: "a", ev: h.ev, script: map[phase.Phase]result{
			phase.OnUpstreamResponseHeaders: undecided(""),
		}},
	})
	b := policy(spec{
		name: "b", class: phase.ClassTransform, scope: phase.ScopeUpstream,
		phases: []phase.Phase{phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders},
		f: &fakeFilter{name: "b", ev: h.ev, script: map[phase.Phase]result{
			phase.OnUpstreamRequest: undecided(""),
		}},
	})
	ch := chain(nil, map[string][]*snapshot.Policy{"up": {a, b}})
	ctx := context.Background()
	r := h.e.Begin(h.st, ch, nil)
	defer r.Release()
	l := r.BeginLeg(ctx, "up", "")
	leg := &filter.Leg{Upstream: "up", Attempt: 1}
	if resp := l.OnUpstreamRequest(ctx, leg); got(resp) != "503 RZ-RT-011" {
		t.Fatalf("onUpstreamRequest = %s", got(resp))
	}
	// Response Phases run reversed: b first, then a replaces.
	retryAttempt, repl := l.OnUpstreamResponseHeaders(ctx, leg)
	if retryAttempt || got(repl) != "502 RZ-RT-012" {
		t.Fatalf("onUpstreamResponseHeaders = %v %s", retryAttempt, got(repl))
	}
	l.End(ctx)
	want := "[a.onUpstreamRequest b.onUpstreamRequest b.onUpstreamResponseHeaders a.onUpstreamResponseHeaders]"
	if evs := fmt.Sprint(h.ev.list()); evs != want {
		t.Fatalf("events = %s, want %s", evs, want)
	}
	if !h.st.legs[0].released {
		t.Fatal("End did not release the leg state")
	}
}

func TestClassDefaultsForGeneratedResponseStatus(t *testing.T) {
	// The generated response carries no body and no header: the handler
	// writes the problem document for Code (filter.Response contract).
	h := newHarness(t)
	ch, _ := h.single(spec{
		name: "p", class: phase.ClassCORS,
		f: &fakeFilter{name: "p", ev: h.ev, script: map[phase.Phase]result{phase.OnRequestHeaders: undecided("")}},
	},
		phase.OnRequestHeaders)
	out := h.runPhase(phase.OnRequestHeaders, ch, "")
	if out.resp == nil || out.resp.Body != nil || out.resp.Header != nil || out.resp.Status != http.StatusServiceUnavailable {
		t.Fatalf("response = %+v", out.resp)
	}
}

func newEvalError() error {
	return expr.NewEvalError(expr.PlaceRateLimitKey, expr.KindNull, "request.headers.x is null")
}
