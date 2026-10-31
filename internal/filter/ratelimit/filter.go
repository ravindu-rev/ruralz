// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"context"
	"crypto/sha256"
	"errors"
	"hash/fnv"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package implements.
const Type = v1alpha1.PolicyTypeRateLimit

// MaxLimits is the most limits[] entries one Policy holds (target).
const MaxLimits = 8

// Codes, each registered in internal/errcode.
const (
	// CodeLocalDeny is a request a local token bucket refuses (429).
	CodeLocalDeny = "RZ-RL-001"
	// CodeGlobalDeny is a request GCRA or the over-limit cache refuses
	// (429).
	CodeGlobalDeny = "RZ-RL-002"
	// CodeUndecided is a config.key, config.cost or limits[].when runtime
	// error, or a cost below 1, under failureMode closed (503).
	CodeUndecided = "RZ-RL-005"
	// CodeConfig is a config of another type or a broken config rule.
	CodeConfig = "RZ-CFG-005"
	// CodeCEL is an expression with a syntax, type or unavailable-variable
	// error.
	CodeCEL = "RZ-CFG-014"
	// CodeCost is an expression over the static cost ceiling.
	CodeCost = "RZ-CFG-015"
	// CodeScope is a placement where the Policy never decides.
	CodeScope = "RZ-CFG-020"
)

// defaultCost is the materialized default of config.cost.
const defaultCost = "1"

// headerRetryAfter is the Retry-After field of every 429.
const headerRetryAfter = "Retry-After"

// Error types a Filter names for the span's error.type.
const (
	errTypeCEL          = "cel_error"
	errTypeResultType   = "result_type"
	errTypeInvalidValue = "invalid_value"
)

// Build errors without a code: programming errors of the caller.
var (
	// ErrNoTable reports a Factory without the Node-wide key table.
	ErrNoTable = errors.New("ratelimit: no key table")
	// ErrNoCEL reports a Build with expressions but no CEL builder.
	ErrNoCEL = errors.New("ratelimit: no CEL builder")
)

// Factory builds ratelimit Filters (filter.Factory) over the Node-wide key
// table, which internal/filter/builtin constructs once per process.
type Factory struct {
	// Table is the Node-wide key table every ratelimit Filter shares.
	Table *Table
}

var _ filter.Factory = Factory{}

// limitDef is one compiled limits[] entry.
type limitDef struct {
	key      limitKey
	id       uint32
	requests int64
	window   time.Duration
	burst    int64
	perNode  int64
	// when is the compiled limits[].when; nil applies the limit always.
	when expr.Program
	// name is the RateLimit-Policy item, the Policy name suffixed .1, .2
	// by position when the Policy holds several limits; w is the window
	// in seconds, rounded up.
	name string
	w    int64
}

// Build compiles one ratelimit Policy: the config rules of Check
// (RZ-CFG-005), config.key, config.cost and each limits[].when at their
// places (RZ-CFG-014, RZ-CFG-015), and a placement where the Policy
// decides, onRequestHeaders at Gateway or Route scope (RZ-CFG-020). Each
// limit is registered in the key table, so buckets of an unchanged limit
// survive a Hot Reload. An ended ctx is its error, without a code.
func (f Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, ok := env.Config.(*v1alpha1.RateLimitConfig)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(CodeConfig, "ratelimit: config is %T, want *v1alpha1.RateLimitConfig", env.Config)
	}
	if ps := problems(cfg); len(ps) > 0 {
		return nil, errcode.Errorf(CodeConfig, "ratelimit: %s: %s", ps[0].path, ps[0].message)
	}
	switch {
	case env.Scope == phase.ScopeUpstream:
		return nil, errcode.Errorf(CodeScope, "ratelimit: not allowed at Upstream scope (allowed: Gateway, Route)")
	case !env.Phases.Has(phase.OnRequestHeaders):
		return nil, errcode.Errorf(CodeScope, "ratelimit: not subscribed to onRequestHeaders")
	}
	if f.Table == nil {
		return nil, ErrNoTable
	}
	name := ""
	if env.Policy != nil {
		name = env.Policy.Metadata.Name
	}
	flt := &Filter{
		name: name, table: f.Table, store: env.StateStore, timeout: env.StateStoreTimeout,
		clock: env.Clock, open: true, constCost: 1, headers: true, emptyKey: statestore.DigestOf(""),
	}
	if flt.clock == nil {
		flt.clock = clock.Real()
	}
	if env.Policy != nil && env.Policy.Spec.FailureMode != nil {
		flt.open = *env.Policy.Spec.FailureMode == v1alpha1.FailureModeOpen
	}
	flt.localOnly = cfg.LocalOnly != nil && *cfg.LocalOnly
	flt.headers = cfg.ResponseHeaders == nil || *cfg.ResponseHeaders != v1alpha1.RateLimitResponseHeadersNone
	if env.Metrics != nil {
		flt.decisions = env.Metrics.RateLimit
		flt.evictions = env.Metrics.BucketEvictions
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	flt.seed = h.Sum64()
	if err := flt.compile(env, cfg); err != nil {
		return nil, err
	}
	flt.limits = make([]limitDef, len(cfg.Limits))
	for i, l := range cfg.Limits {
		d := &flt.limits[i]
		d.requests, d.window = l.Requests, time.Duration(l.Window)
		d.burst = l.Requests
		if l.Burst != nil {
			d.burst = *l.Burst
		}
		if l.PerNodeCeiling != nil {
			d.perNode = *l.PerNodeCeiling
		}
		d.name = name
		if len(cfg.Limits) > 1 {
			d.name = name + "." + strconv.Itoa(i+1)
		}
		d.w = int64((d.window + time.Second - 1) / time.Second)
		d.when = flt.whens[i]
	}
	flt.whens = nil
	// Register last: a refused Build holds no table reference.
	for i := range flt.limits {
		d := &flt.limits[i]
		d.key = limitKey{policy: name, requests: d.requests, window: d.window}
		d.id = f.Table.register(d.key, &flt.evictions)
	}
	flt.status = errcode.Status(CodeLocalDeny)
	flt.pool.New = func() any { return new(reqState) }
	return flt, nil
}

// compile compiles config.key, config.cost and each limits[].when; an
// absent key or the default cost needs no program.
func (flt *Filter) compile(env filter.BuildEnv, cfg *v1alpha1.RateLimitConfig) error {
	flt.whens = make([]expr.Program, len(cfg.Limits))
	cost := defaultCost
	if cfg.Cost != nil {
		cost = *cfg.Cost
	}
	needCEL := cfg.Key != "" || cost != defaultCost
	for _, l := range cfg.Limits {
		needCEL = needCEL || l.When != ""
	}
	if !needCEL {
		return nil
	}
	if env.CEL == nil {
		return ErrNoCEL
	}
	var err error
	if cfg.Key != "" {
		if flt.key, err = compileAt(env, expr.PlaceRateLimitKey, "config.key", cfg.Key); err != nil {
			return err
		}
	}
	if cost != defaultCost {
		if flt.cost, err = compileAt(env, expr.PlaceRateLimitCost, "config.cost", cost); err != nil {
			return err
		}
	}
	for i, l := range cfg.Limits {
		if l.When == "" {
			continue
		}
		field := "config.limits[" + strconv.Itoa(i) + "].when"
		if flt.whens[i], err = compileAt(env, expr.PlaceRateLimitWhen, field, l.When); err != nil {
			return err
		}
	}
	return nil
}

// compileAt compiles src at place for this attachment: the first Issue's
// code, RZ-CFG-015 for the cost ceiling and RZ-CFG-014 otherwise, refuses
// it.
func compileAt(env filter.BuildEnv, place expr.PlaceID, field, src string) (expr.Program, error) {
	site := expr.Site{Place: place, Scope: env.Scope, PolicyType: Type}
	if env.Site != nil {
		site = env.Site(place)
	}
	prg, iss := env.CEL.Compile(site, src)
	if prg != nil {
		return prg, nil
	}
	if len(iss) == 0 {
		return nil, errcode.Errorf(CodeCEL, "ratelimit: %s did not compile", field)
	}
	code := CodeCEL
	if iss[0].Code == CodeCost {
		code = CodeCost
	}
	return nil, errcode.Errorf(code, "ratelimit: %s: %s", field, iss[0].Message)
}

// Filter is a compiled ratelimit Policy: a Consumptive admission Filter
// whose Node-local steps run on the key table and whose GCRA call the
// executor batches. It is immutable after Build and safe for concurrent
// use; per-request state lives in a pooled reqState in the Policy's
// PolicyState slot.
type Filter struct {
	name    string
	table   *Table
	store   statestore.Store
	timeout time.Duration
	clock   clock.Clock
	// open is failureMode open.
	open bool
	// key, cost: nil for an absent key (one partition) and the default
	// cost (constCost).
	key       expr.Program
	cost      expr.Program
	constCost int64
	whens     []expr.Program
	limits    []limitDef
	localOnly bool
	// headers records the RateLimit fields (responseHeaders: ietf).
	headers bool
	// seed spreads the Policy's keys over the table's shards.
	seed     uint64
	emptyKey statestore.Digest
	// status is the registered status of a 429 code.
	status    int
	decisions [emit.NumRateLimitResults]emit.Counter
	evictions [emit.NumBucketSegments]emit.Counter
	pool      sync.Pool
}

var (
	_ filter.Consumptive = (*Filter)(nil)
	_ filter.Finisher    = (*Filter)(nil)
	_ filter.Closer      = (*Filter)(nil)
)

// reqState is one request's state of the Policy, pooled by the Filter
// and kept in the Policy's PolicyState slot from Prepare to Finish.
type reqState struct {
	digest statestore.Digest
	cost   int64
	// n applied limits: idx their positions in limits, specs their table
	// view, gcra and out the GCRA call's limits and outcomes.
	n     int
	idx   [MaxLimits]uint8
	specs [MaxLimits]limitSpec
	gcra  [MaxLimits]statestore.GCRALimit
	out   [MaxLimits]statestore.GCRAOutcome
	// local is the outcome of the Node-local steps.
	local local
	// took reports tokens taken from the buckets, which Undo returns.
	took bool
	// call and calls back Handle's own round trip.
	call  statestore.Call
	calls [1]*statestore.Call
}

// state returns the request's reqState from the Policy's slot, taking one
// from the pool on the Policy's first call.
func (flt *Filter) state(x filter.Exchange) *reqState {
	p := x.PolicyState()
	if st, ok := (*p).(*reqState); ok && st != nil {
		return st
	}
	st, _ := flt.pool.Get().(*reqState)
	if st == nil {
		st = new(reqState)
	}
	*p = st
	return st
}

// Handle decides in onRequestHeaders and continues in any other Phase.
// The executor batches the Policy through Prepare and Complete instead;
// Handle runs the same steps with its own round trip, for a caller
// without batching.
func (flt *Filter) Handle(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	if p != phase.OnRequestHeaders {
		return filter.Next()
	}
	st := flt.state(x)
	call := &st.call
	*call = statestore.Call{Batch: -1}
	r, done := flt.Prepare(ctx, x, call)
	if done {
		return r
	}
	budget := x.StateBudget()
	if flt.store == nil || budget == nil {
		call.Err = statestore.ErrNotAttempted
	} else {
		st.calls[0] = call
		flt.store.Consume(ctx, budget, st.calls[:])
		st.calls[0] = nil
	}
	return flt.Complete(ctx, x, call)
}

// Prepare runs every Node-local step (Consumptive): it evaluates
// config.key, config.cost and each limits[].when once, then the over-limit
// cache, the first-seen budget and the local buckets. It decides a CEL
// error or a cost below 1 (failureMode, RZ-RL-005 under closed), a Policy
// left with no applied limit (admit), a cached GCRA denial (429
// RZ-RL-002), a bucket lacking the cost (429 RZ-RL-001), and an admission
// on local buckets alone (a local-only entry or a localOnly Policy);
// otherwise it fills call with one GCRA call for every applied limit.
func (flt *Filter) Prepare(ctx context.Context, x filter.Exchange, call *statestore.Call) (filter.Result, bool) {
	st := flt.state(x)
	st.n, st.took = 0, false
	if err := flt.evaluate(ctx, x, st); err != nil {
		flt.record(x, st, nil)
		flt.count(x, flt.failure())
		return filter.Undecided(CodeUndecided, err), true
	}
	if st.n == 0 {
		flt.count(x, emit.RateLimitAllow)
		return filter.Next(), true
	}
	count, control := flt.table.count.Count()
	for i := range st.n {
		d := &flt.limits[st.idx[i]]
		c, a := ceilingOf(d.requests, d.perNode, count, control)
		st.specs[i] = limitSpec{id: d.id, ceiling: c, allowance: a, window: int64(d.window)}
	}
	now := flt.clock.Now().UnixNano()
	flt.table.take(flt.seed, &st.digest, st.specs[:st.n], st.cost, flt.localOnly, now, x.Stripe(), &st.local)
	switch st.local.verdict {
	case verdictOverLimit:
		flt.count(x, emit.RateLimitDenyGlobal)
		return flt.deny(x, st, CodeGlobalDeny, st.local.denied, st.local.wait), true
	case verdictLocalDeny:
		flt.count(x, emit.RateLimitDenyLocal)
		return flt.deny(x, st, CodeLocalDeny, st.local.denied, st.local.wait), true
	case verdictLocal:
		st.took = true
		flt.count(x, emit.RateLimitAllowLocal)
		flt.record(x, st, func(i int) (int64, time.Duration, bool) {
			return st.local.remaining[i], st.local.reset[i], !st.local.failOpen
		})
		return filter.Next(), true
	case verdictGCRA:
	}
	st.took = true
	for i := range st.n {
		d := &flt.limits[st.idx[i]]
		st.gcra[i] = statestore.GCRALimit{Requests: d.requests, Window: d.window, Burst: d.burst}
	}
	*call = statestore.Call{Kind: statestore.OpGCRA, Timeout: flt.timeout, Batch: -1, GCRA: statestore.GCRA{
		Policy: flt.name, Digest: st.digest, Limits: st.gcra[:st.n], Cost: st.cost, Out: st.out[:st.n],
	}}
	return filter.Result{}, false
}

// Complete applies the GCRA reply (Consumptive): an allow admits with the
// counts the script returned and returns any bucket on the fail-open
// refill to the continuous one; a deny caches the denial until its
// retry-after and answers 429 RZ-RL-002; a failed, timed-out or
// unattempted call, or an open breaker, applies failureMode: open admits
// within the local buckets, which switch to the fail-open refill, closed
// answers 503 with the call's RZ-STS code.
func (flt *Filter) Complete(_ context.Context, x filter.Exchange, call *statestore.Call) filter.Result {
	st := flt.state(x)
	now := flt.clock.Now().UnixNano()
	if call.Err != nil || !call.Done {
		err := call.Err
		if err == nil {
			err = statestore.ErrNotAttempted
		}
		flt.record(x, st, nil)
		if !flt.open {
			flt.count(x, emit.RateLimitFailClosed)
			return filter.Undecided(err.Code(), err)
		}
		flt.table.failedOpen(flt.seed, &st.digest, st.specs[:st.n], now)
		flt.count(x, emit.RateLimitFailOpen)
		return filter.Undecided(err.Code(), err)
	}
	g := &call.GCRA
	if !g.Allowed {
		denied := g.Denied
		if denied < 0 || denied >= st.n {
			denied = 0
		}
		flt.table.answered(flt.seed, &st.digest, st.specs[:st.n], now, denied, g.RetryAfter)
		flt.count(x, emit.RateLimitDenyGlobal)
		return flt.deny(x, st, CodeGlobalDeny, denied, g.RetryAfter)
	}
	flt.table.answered(flt.seed, &st.digest, st.specs[:st.n], now, -1, 0)
	flt.count(x, emit.RateLimitAllow)
	flt.record(x, st, func(i int) (int64, time.Duration, bool) {
		return st.out[i].Remaining, st.out[i].ResetAfter, true
	})
	return filter.Next()
}

// Undo returns the tokens Prepare took when the Policy will not be
// completed (Consumptive).
func (flt *Filter) Undo(x filter.Exchange) {
	st := flt.state(x)
	if !st.took {
		return
	}
	st.took = false
	flt.table.returnTokens(flt.seed, &st.digest, st.specs[:st.n], st.cost, flt.clock.Now().UnixNano())
}

// Finish returns the request's reqState to the pool (filter.Finisher).
func (flt *Filter) Finish(_ context.Context, x filter.Exchange) {
	p := x.PolicyState()
	st, ok := (*p).(*reqState)
	if !ok || st == nil {
		return
	}
	*p = nil
	st.call, st.calls = statestore.Call{}, [1]*statestore.Call{}
	flt.pool.Put(st)
}

// Close releases the Filter's limits in the key table (filter.Closer);
// a limit no live Filter holds is forgotten and its entries age out.
func (flt *Filter) Close() error {
	for i := range flt.limits {
		flt.table.release(flt.limits[i].key)
	}
	return nil
}

// evaluate evaluates config.key, config.cost and each limits[].when once
// into st: the key digest, the cost and the applied limits. It returns the
// first runtime error, or an invalid-value error for a cost below 1; the
// limits whose when evaluated true, or that have none, are applied even
// then, for the fields.
func (flt *Filter) evaluate(ctx context.Context, x filter.Exchange, st *reqState) error {
	v := x.Vars()
	var first error
	st.digest, st.cost = flt.emptyKey, flt.constCost
	if flt.key != nil {
		s, err := flt.key.EvalString(ctx, v)
		switch {
		case err != nil:
			first = classify(err)
		case s != "":
			st.digest = digestOf(s)
		}
	}
	if flt.cost != nil {
		c, err := flt.cost.EvalInt(ctx, v)
		if err != nil && first == nil {
			first = classify(err)
		}
		st.cost = c
	}
	for i := range flt.limits {
		w := flt.limits[i].when
		if w != nil {
			ok, err := w.EvalBool(ctx, v)
			if err != nil {
				if first == nil {
					first = classify(err)
				}
				continue
			}
			if !ok {
				continue
			}
		}
		st.idx[st.n] = uint8(i)
		st.n++
	}
	if first == nil && st.cost < 1 {
		first = errBadCost
	}
	return first
}

// record adds the RateLimit fields of the applied limits when the Policy
// records them (responseHeaders: ietf): each limit's RateLimit-Policy
// item, with the count counts reports for it when known.
func (flt *Filter) record(x filter.Exchange, st *reqState, counts func(i int) (remaining int64, reset time.Duration, known bool)) {
	if !flt.headers {
		return
	}
	for i := range st.n {
		d := &flt.limits[st.idx[i]]
		f := filter.RateLimitField{Name: d.name, Q: d.requests, W: d.w}
		if counts != nil {
			r, t, ok := counts(i)
			if ok {
				f.R, f.T, f.Known = max(r, 0), seconds(t), true
			}
		}
		x.AddRateLimitField(f)
	}
}

// deny answers 429 with code for the applied limit at index denied, which
// admits after wait: Retry-After is its wait in whole seconds, rounded up
// and at least 1, and when the Policy records fields the denied limit is
// listed with r=0 and t equal to Retry-After, the others without a count.
func (flt *Filter) deny(x filter.Exchange, st *reqState, code string, denied int, wait time.Duration) filter.Result {
	t := max(seconds(wait), 1)
	if flt.headers {
		for i := range st.n {
			d := &flt.limits[st.idx[i]]
			f := filter.RateLimitField{Name: d.name, Q: d.requests, W: d.w}
			if i == denied {
				f.T, f.Known, f.Denied = t, true, true
			}
			x.AddRateLimitField(f)
		}
	}
	return filter.Deny(flt.status, code, http.Header{headerRetryAfter: {strconv.FormatInt(t, 10)}})
}

// failure returns the decision result of a failure under the Policy's
// failureMode.
func (flt *Filter) failure() int {
	if flt.open {
		return emit.RateLimitFailOpen
	}
	return emit.RateLimitFailClosed
}

// count adds one ruralz_ratelimit_decisions_total observation.
func (flt *Filter) count(x filter.Exchange, result int) {
	if c := flt.decisions[result]; c != nil {
		c.Add(x.Stripe(), 1)
	}
}

// digestBuffer is how long a key value hashes without a heap copy.
const digestBuffer = 256

// digestOf is statestore.DigestOf, hashing a key value of up to
// digestBuffer bytes from a stack copy, so the decision allocates nothing.
func digestOf(s string) statestore.Digest {
	if len(s) > digestBuffer {
		return statestore.DigestOf(s)
	}
	var buf [digestBuffer]byte
	return sha256.Sum256(append(buf[:0], s...))
}

// seconds returns d in whole seconds, rounded up.
func seconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64((d + time.Second - 1) / time.Second)
}

// celError is a runtime error of one of the Policy's expressions. It
// names the span's error.type, result_type for a dyn result of the wrong
// type and cel_error otherwise; its text is the *expr.EvalError's, which
// never holds request data.
type celError struct {
	kind string
	err  error
}

// Error returns the cause's text.
func (e *celError) Error() string { return e.err.Error() }

// Unwrap returns the cause.
func (e *celError) Unwrap() error { return e.err }

// ErrorType returns the span's error.type.
func (e *celError) ErrorType() string { return e.kind }

// classify wraps a runtime error with its error.type.
func classify(err error) error {
	kind := errTypeCEL
	var ee *expr.EvalError
	if errors.As(err, &ee) && ee.Kind == expr.KindResultType {
		kind = errTypeResultType
	}
	return &celError{kind: kind, err: err}
}

// costError is a config.cost below 1, error.type invalid_value.
type costError struct{}

// Error describes the error.
func (costError) Error() string { return "ratelimit: config.cost is below 1" }

// ErrorType returns the span's error.type.
func (costError) ErrorType() string { return errTypeInvalidValue }

// errBadCost is the cause of a cost below 1.
var errBadCost error = costError{}
