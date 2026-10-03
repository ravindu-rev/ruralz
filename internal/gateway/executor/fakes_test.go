// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Test fakes: an in-memory snapshot.RequestState over filtertest.Exchange
// with per-Policy PolicyState slots and when decisions, scripted Filters,
// counting metric handles and a recording tracer.

// events is a goroutine-safe ordered log of what Filters saw.
type events struct {
	mu  sync.Mutex
	evs []string
}

func (e *events) add(s string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.evs = append(e.evs, s)
	e.mu.Unlock()
}

func (e *events) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.evs)
}

// with returns the events that start with prefix.
func (e *events) with(prefix string) []string {
	var out []string
	for _, s := range e.list() {
		if strings.HasPrefix(s, prefix) {
			out = append(out, s)
		}
	}
	return out
}

type shortRec struct {
	policy string
	ph     phase.Phase
	status int
}

type failRec struct {
	policy string
	ph     phase.Phase
	mode   v1alpha1.FailureMode
}

// maxPolicies bounds Policy.Index in the fake state.
const maxPolicies = 64

// fakeState is a snapshot.RequestState. Slots and when decisions are
// arrays indexed by Policy.Index, so steady-state use allocates nothing.
type fakeState struct {
	x     *filtertest.Exchange
	cur   *snapshot.Policy
	ph    phase.Phase
	span  emit.Span
	whens [maxPolicies]uint8 // 0 undecided, 1 runs, 2 skipped
	slots [maxPolicies]any

	budget   statestore.RequestBudget
	noBudget bool
	shorts   []shortRec
	fails    []failRec
	enters   int
	setLegs  int
	released bool

	upstream, step string
	// legs are the leg states handed out; spare is reused by Leg when set
	// (allocation tests).
	legs     []*fakeState
	spare    *fakeState
	legCalls int // unsynchronized on purpose: the race detector checks legMu
}

func newState() *fakeState {
	return &fakeState{x: &filtertest.Exchange{Msg: &filtertest.Message{}, StripeV: 3}}
}

var _ snapshot.RequestState = (*fakeState)(nil)

func (s *fakeState) Exchange() filter.Exchange { return s.x }

func (s *fakeState) Enter(p *snapshot.Policy, ph phase.Phase, span emit.Span) {
	if s.cur != nil {
		s.slots[s.cur.Index] = s.x.State
	}
	s.x.State = s.slots[p.Index]
	s.cur, s.ph, s.span = p, ph, span
	s.enters++
}

func (s *fakeState) SetLeg(l *filter.Leg) {
	s.x.LegV = l
	s.setLegs++
}

func (s *fakeState) When(p *snapshot.Policy) (decided, skip bool) {
	w := s.whens[p.Index]
	return w != 0, w == 2
}

func (s *fakeState) SetWhen(p *snapshot.Policy, skip bool) {
	if skip {
		s.whens[p.Index] = 2
	} else {
		s.whens[p.Index] = 1
	}
}

func (s *fakeState) RecordShortCircuit(p *snapshot.Policy, ph phase.Phase, status int) {
	s.shorts = append(s.shorts, shortRec{p.Name, ph, status})
}

func (s *fakeState) RecordFailure(p *snapshot.Policy, ph phase.Phase, mode v1alpha1.FailureMode) {
	s.fails = append(s.fails, failRec{p.Name, ph, mode})
}

func (s *fakeState) Budget() *statestore.RequestBudget {
	if s.noBudget {
		return nil
	}
	return &s.budget
}

func (s *fakeState) Leg(upstream, step string) snapshot.RequestState {
	s.legCalls++
	if l := s.spare; l != nil {
		l.upstream, l.step, l.released = upstream, step, false
		l.cur = nil
		l.whens = [maxPolicies]uint8{}
		l.slots = [maxPolicies]any{}
		return l
	}
	l := newState()
	l.upstream, l.step = upstream, step
	s.legs = append(s.legs, l)
	return l
}

func (s *fakeState) Release() { s.released = true }

// result is a scripted Filter answer for one Phase.
type result func(ctx context.Context, x filter.Exchange) filter.Result

// fakeFilter logs "<name>.<phase>" and answers from its script (Continue
// for an unscripted Phase). It asserts that the executor entered it.
type fakeFilter struct {
	name   string
	ev     *events
	script map[phase.Phase]result
	calls  int64 // atomic
	// st, when set, is checked for Enter before every call.
	st *fakeState
	t  testing.TB
}

// callCount returns how many times Handle ran.
func (f *fakeFilter) callCount() int64 { return atomic.LoadInt64(&f.calls) }

func (f *fakeFilter) Handle(ctx context.Context, ph phase.Phase, x filter.Exchange) filter.Result {
	atomic.AddInt64(&f.calls, 1)
	f.ev.add(f.name + "." + ph.String())
	if f.st != nil && (f.st.cur == nil || f.st.cur.Name != f.name || f.st.ph != ph) {
		f.t.Errorf("%s: Handle(%s) without Enter", f.name, ph)
	}
	if fn := f.script[ph]; fn != nil {
		return fn(ctx, x)
	}
	return filter.Next()
}

// finishFilter is a fakeFilter that implements filter.Finisher.
type finishFilter struct{ fakeFilter }

func (f *finishFilter) Finish(context.Context, filter.Exchange) { f.ev.add(f.name + ".finish") }

// challengeFilter is an auth Filter with a WWW-Authenticate challenge.
type challengeFilter struct {
	fakeFilter
	challenge string
}

func (f *challengeFilter) Challenge() string { return f.challenge }

// consumer is a scripted filter.Consumptive. Prepare stores its name in
// PolicyState, which Complete and Undo check (per-member PolicyState).
type consumer struct {
	fakeFilter
	// prepare decides locally when set (done true).
	prepare func(x filter.Exchange, call *statestore.Call) (filter.Result, bool)
	// complete answers Complete; nil maps the call: Err to CannotDecide with
	// its code, a deny to 429 RZ-RL-002, else Continue.
	complete func(x filter.Exchange, call *statestore.Call) filter.Result
	timeout  time.Duration
	t        testing.TB
	// spanCtx, when set, checks that Prepare and Complete get the context
	// of the member's own ruralz.filter.<name> span.
	spanCtx bool
}

// checkSpan reports a Prepare or Complete call without the member's span
// context.
func (c *consumer) checkSpan(ctx context.Context, call string) {
	if !c.spanCtx {
		return
	}
	if s, _ := ctx.Value(spanKey{}).(*fakeSpan); s == nil || s.name != catalog.FilterSpanName(c.name) {
		c.t.Errorf("%s: %s without its span context (got %v)", c.name, call, s)
	}
}

func (c *consumer) Prepare(ctx context.Context, x filter.Exchange, call *statestore.Call) (filter.Result, bool) {
	c.ev.add(c.name + ".prepare")
	c.checkSpan(ctx, "Prepare")
	if got := *x.PolicyState(); got != nil {
		c.t.Errorf("%s: PolicyState holds %v at Prepare", c.name, got)
	}
	*x.PolicyState() = c.name
	if call.Batch != -1 {
		c.t.Errorf("%s: call.Batch = %d at Prepare, want -1", c.name, call.Batch)
	}
	if c.prepare != nil {
		return c.prepare(x, call)
	}
	call.Kind = statestore.OpGCRA
	call.Timeout = c.timeout
	call.GCRA.Policy = c.name
	return filter.Result{}, false
}

func (c *consumer) Complete(ctx context.Context, x filter.Exchange, call *statestore.Call) filter.Result {
	c.ev.add(c.name + ".complete")
	c.checkSpan(ctx, "Complete")
	if got := *x.PolicyState(); got != c.name {
		c.t.Errorf("%s: PolicyState holds %v at Complete", c.name, got)
	}
	if c.complete != nil {
		return c.complete(x, call)
	}
	switch {
	case call.Err != nil:
		return filter.Undecided(call.Err.Code(), call.Err)
	case !call.GCRA.Allowed:
		return filter.Deny(http.StatusTooManyRequests, "RZ-RL-002", nil)
	default:
		return filter.Next()
	}
}

func (c *consumer) Undo(x filter.Exchange) {
	c.ev.add(c.name + ".undo")
	if got := *x.PolicyState(); got != c.name {
		c.t.Errorf("%s: PolicyState holds %v at Undo", c.name, got)
	}
}

// constProgram is an expr.Program with a fixed answer that counts calls.
type constProgram struct {
	ok    bool
	err   error
	panic bool
	n     atomic.Int64
}

func (*constProgram) Place() expr.PlaceID { return expr.PlacePolicyWhen }
func (*constProgram) Source() string      { return "when" }
func (*constProgram) Refs() expr.Refs     { return expr.Refs{} }
func (*constProgram) Cost() expr.CostRange {
	return expr.CostRange{}
}

func (p *constProgram) EvalBool(context.Context, *expr.Vars) (bool, error) {
	p.n.Add(1)
	if p.panic {
		panic("when exploded")
	}
	return p.ok, p.err
}

func (*constProgram) EvalString(context.Context, *expr.Vars) (string, error) { return "", nil }

func (*constProgram) EvalValue(context.Context, *expr.Vars) (expr.Value, error) {
	return nil, nil //nolint:nilnil // unused by the executor
}

// Metric fakes.

type counter struct{ n atomic.Int64 }

func (c *counter) Add(_ emit.Stripe, n uint64) { c.n.Add(int64(n)) } //nolint:gosec // test counts are small

type statusCounter struct {
	mu sync.Mutex
	by map[int]int
}

func (c *statusCounter) Inc(_ emit.Stripe, status int) {
	c.mu.Lock()
	if c.by == nil {
		c.by = map[int]int{}
	}
	c.by[status]++
	c.mu.Unlock()
}

type histogram struct {
	n      atomic.Int64
	sum    atomic.Uint64
	stripe atomic.Int64
}

func (h *histogram) Record(s emit.Stripe, v uint64) {
	h.n.Add(1)
	h.sum.Add(v)
	h.stripe.Store(int64(s))
}

func (h *histogram) RecordExemplar(s emit.Stripe, v uint64, _ emit.Exemplar) { h.Record(s, v) }

// probe holds the fake handles of one Policy.
type probe struct {
	dur   [phase.Count]*histogram
	short [phase.Count]*statusCounter
	fail  [phase.Count][emit.NumModes]*counter
}

func newProbe() (*emit.PolicyMetrics, *probe) {
	m, pr := &emit.PolicyMetrics{}, &probe{}
	for ph := range phase.Count {
		pr.dur[ph], pr.short[ph] = &histogram{}, &statusCounter{}
		m.Duration[ph], m.ShortCircuits[ph] = pr.dur[ph], pr.short[ph]
		for mode := range emit.NumModes {
			pr.fail[ph][mode] = &counter{}
			m.Failures[ph][mode] = pr.fail[ph][mode]
		}
	}
	return m, pr
}

func (pr *probe) failures(ph phase.Phase, mode int) int64 { return pr.fail[ph][mode].n.Load() }

// Tracer fakes.

type fakeSpan struct {
	name      string
	ph        phase.Phase
	policyTyp string
	mu        sync.Mutex
	attrs     map[string]string
	ended     int
	status    int
	code      string
	errType   string
}

func (s *fakeSpan) SetAttr(key string, v slog.Value) {
	s.mu.Lock()
	if s.attrs == nil {
		s.attrs = map[string]string{}
	}
	s.attrs[key] = v.String()
	s.mu.Unlock()
}

func (*fakeSpan) SpanID() [8]byte { return [8]byte{1} }

func (s *fakeSpan) End(status int, code, errorType string) {
	s.mu.Lock()
	s.ended++
	s.status, s.code, s.errType = status, code, errorType
	s.mu.Unlock()
}

func (s *fakeSpan) attr(k string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attrs[k]
}

type fakeTracer struct {
	mu    sync.Mutex
	spans []*fakeSpan
}

type spanKey struct{}

func (*fakeTracer) Decide(http.Header, float64, *emit.Decision) {}

func (*fakeTracer) StartServer(ctx context.Context, _ *emit.Decision, _ string, _ emit.ServerAttrs) (context.Context, emit.Span) {
	return ctx, &fakeSpan{}
}

func (*fakeTracer) StartRouteMatch(ctx context.Context) (context.Context, emit.Span) {
	return ctx, &fakeSpan{}
}

func (t *fakeTracer) StartFilter(ctx context.Context, name string, a emit.FilterAttrs) (context.Context, emit.Span) {
	s := &fakeSpan{name: name, ph: a.Phase, policyTyp: a.PolicyType}
	t.mu.Lock()
	t.spans = append(t.spans, s)
	t.mu.Unlock()
	return context.WithValue(ctx, spanKey{}, s), s
}

func (*fakeTracer) StartUpstream(ctx context.Context, _ string, _ emit.UpstreamAttrs) (context.Context, emit.Span) {
	return ctx, &fakeSpan{}
}

func (*fakeTracer) Inject(context.Context, *emit.Decision, http.Header) {}

func (t *fakeTracer) named(name string) []*fakeSpan {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*fakeSpan
	for _, s := range t.spans {
		if s.name == catalog.FilterSpanName(name) {
			out = append(out, s)
		}
	}
	return out
}

// noopTracer returns one shared no-op span (allocation tests).
type noopTracer struct{ fakeTracer }

type noopSpan struct{}

func (noopSpan) SetAttr(string, slog.Value) {}
func (noopSpan) SpanID() [8]byte            { return [8]byte{} }
func (noopSpan) End(int, string, string)    {}

func (*noopTracer) StartFilter(ctx context.Context, _ string, _ emit.FilterAttrs) (context.Context, emit.Span) {
	return ctx, noopSpan{}
}

// Chain construction, as internal/gateway/compile will do it: Policies in
// request order, request Phases in that order, response Phases reversed.

type spec struct {
	name   string
	class  phase.Class
	scope  phase.Scope
	mode   v1alpha1.FailureMode
	typ    v1alpha1.PolicyType
	phases []phase.Phase
	f      filter.Filter
	when   expr.Program
}

// policy builds a compiled Policy; Index and Position are set by chain.
func policy(s spec) *snapshot.Policy {
	if s.scope == phase.ScopeNone {
		s.scope = phase.ScopeRoute
	}
	if s.mode == "" {
		s.mode = v1alpha1.FailureModeClosed
	}
	if s.typ == "" {
		s.typ = v1alpha1.PolicyTypeHeaders
	}
	set := phase.Of(s.phases...)
	first, _ := set.First()
	p := &snapshot.Policy{
		Name: s.name, Type: s.typ, Class: s.class, Scope: s.scope, FailureMode: s.mode,
		When: s.when, FirstPhase: first, Phases: set, Filter: s.f,
		SpanName: catalog.FilterSpanName(s.name), StateStoreTimeout: 20 * time.Millisecond,
	}
	if c, ok := s.f.(filter.Consumptive); ok {
		p.Consumptive = c
	}
	return p
}

// lists places ps (in request order) into per-Phase lists.
func lists(ps []*snapshot.Policy) [phase.Count][]*snapshot.Policy {
	var out [phase.Count][]*snapshot.Policy
	for ph := range phase.Count {
		for _, p := range ps {
			if p.Phases.Has(ph) {
				out[ph] = append(out[ph], p)
			}
		}
		if ph.IsResponse() {
			slices.Reverse(out[ph])
		}
	}
	return out
}

// chain compiles client Policies (request order) and leg chains.
func chain(ps []*snapshot.Policy, legs map[string][]*snapshot.Policy) *snapshot.Chain {
	c := &snapshot.Chain{Client: lists(ps), Policies: ps, Legs: map[string]*snapshot.LegChain{}}
	idx := 0
	for i, p := range ps {
		p.Index, p.Position = idx, i
		idx++
		if p.Class == phase.ClassAuth {
			c.AuthPolicies++
		}
	}
	for up, lps := range legs {
		for i, p := range lps {
			p.Index, p.Position = idx, i
			idx++
		}
		c.Legs[up] = &snapshot.LegChain{Upstream: up, Phases: lists(lps)}
	}
	return c
}

func undecided(code string) result {
	return func(context.Context, filter.Exchange) filter.Result {
		return filter.Undecided(code, errUndecidedTest)
	}
}

func respondWith(status int, code string) result {
	return func(context.Context, filter.Exchange) filter.Result {
		return filter.Deny(status, code, nil)
	}
}

func outcome(o filter.Outcome) result {
	return func(context.Context, filter.Exchange) filter.Result { return filter.Result{Outcome: o} }
}

func failWith(err error) result {
	return func(context.Context, filter.Exchange) filter.Result { return filter.Undecided("", err) }
}

func panics(context.Context, filter.Exchange) filter.Result { panic("filter exploded") }
