// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package quota

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
const Type = v1alpha1.PolicyTypeQuota

// Codes, each registered in internal/errcode.
const (
	// CodeExhausted is a request whose cost exceeds the units left in its
	// window (429).
	CodeExhausted = "RZ-RL-003"
	// CodeNoQuota is a request without a Consumer, or of one lacking the
	// quota or holding it with unit tokens (403).
	CodeNoQuota = "RZ-RL-004"
	// CodeUndecided is a config.key or config.cost runtime error, or a
	// cost below 1, under failureMode closed (503).
	CodeUndecided = "RZ-RL-005"
	// CodeConfig is a config of another type.
	CodeConfig = "RZ-CFG-005"
	// CodeCEL is an expression with a syntax, type or unavailable-variable
	// error.
	CodeCEL = "RZ-CFG-014"
	// CodeCost is an expression over the static cost ceiling.
	CodeCost = "RZ-CFG-015"
	// CodeScope is a placement where the Policy never decides.
	CodeScope = "RZ-CFG-020"
)

// Materialized defaults of config.key and config.cost.
const (
	defaultKey  = "consumer.name"
	defaultCost = "1"
)

// headerRetryAfter is the Retry-After field of every 429.
const headerRetryAfter = "Retry-After"

// The spread of a denial's Retry-After past its window's end: up to 1% of
// the window, at most 60 s (target).
const (
	maxJitter   = 60 * time.Second
	jitterShare = 100
)

// Error types a Filter names for the span's error.type.
const (
	errTypeCEL          = "cel_error"
	errTypeResultType   = "result_type"
	errTypeInvalidValue = "invalid_value"
)

// Build errors without a code: programming errors of the caller.
var (
	// ErrNoCache reports a Factory without the Node-wide denial cache.
	ErrNoCache = errors.New("quota: no denial cache")
	// ErrNoCEL reports a Build with expressions but no CEL builder.
	ErrNoCEL = errors.New("quota: no CEL builder")
)

// Factory builds quota Filters (filter.Factory) over the Node-wide denial
// cache, which internal/filter/builtin constructs once per process.
type Factory struct {
	// Cache is the Node-wide denial cache every quota Filter shares.
	Cache *Cache
}

var _ filter.Factory = Factory{}

// Build compiles one quota Policy: config.key and config.cost at their
// places (RZ-CFG-014, RZ-CFG-015), and a placement where the Policy
// decides, onRequestHeaders at Gateway or Route scope (RZ-CFG-020). The
// default key, consumer.name, reads the Consumer's name without a
// program, and the default cost needs none. An ended ctx is its error,
// without a code.
func (f Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, ok := env.Config.(*v1alpha1.QuotaConfig)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(CodeConfig, "quota: config is %T, want *v1alpha1.QuotaConfig", env.Config)
	}
	switch {
	case env.Scope == phase.ScopeUpstream:
		return nil, errcode.Errorf(CodeScope, "quota: not allowed at Upstream scope (allowed: Gateway, Route)")
	case !env.Phases.Has(phase.OnRequestHeaders):
		return nil, errcode.Errorf(CodeScope, "quota: not subscribed to onRequestHeaders")
	}
	if f.Cache == nil {
		return nil, ErrNoCache
	}
	name := ""
	if env.Policy != nil {
		name = env.Policy.Metadata.Name
	}
	flt := &Filter{
		name: name, quota: cfg.ConsumerQuota, cache: f.Cache, store: env.StateStore, enqueue: env.Enqueuer,
		timeout: env.StateStoreTimeout, clock: env.Clock, open: true, emptyKey: statestore.DigestOf(""),
	}
	if flt.clock == nil {
		flt.clock = clock.Real()
	}
	if env.Policy != nil && env.Policy.Spec.FailureMode != nil {
		flt.open = *env.Policy.Spec.FailureMode == v1alpha1.FailureModeOpen
	}
	if env.Metrics != nil {
		flt.decisions = env.Metrics.Quota
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(cfg.ConsumerQuota))
	flt.nameHash = h.Sum64()
	if err := flt.compile(env, cfg); err != nil {
		return nil, err
	}
	flt.denyStatus = errcode.Status(CodeExhausted)
	flt.noQuotaStatus = errcode.Status(CodeNoQuota)
	flt.pool.New = func() any { return new(reqState) }
	return flt, nil
}

// compile compiles config.key and config.cost: the default key reads the
// Consumer's name, an empty key puts every request in one partition, and
// the default cost is 1; none of them needs a program.
func (flt *Filter) compile(env filter.BuildEnv, cfg *v1alpha1.QuotaConfig) error {
	key, cost := defaultKey, defaultCost
	if cfg.Key != nil {
		key = *cfg.Key
	}
	if cfg.Cost != nil {
		cost = *cfg.Cost
	}
	flt.keyIsName = key == defaultKey
	needKey := !flt.keyIsName && key != ""
	if !needKey && cost == defaultCost {
		return nil
	}
	if env.CEL == nil {
		return ErrNoCEL
	}
	var err error
	if needKey {
		if flt.key, err = compileAt(env, expr.PlaceQuotaKey, "config.key", key); err != nil {
			return err
		}
	}
	if cost != defaultCost {
		if flt.cost, err = compileAt(env, expr.PlaceQuotaCost, "config.cost", cost); err != nil {
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
		return nil, errcode.Errorf(CodeCEL, "quota: %s did not compile", field)
	}
	code := CodeCEL
	if iss[0].Code == CodeCost {
		code = CodeCost
	}
	return nil, errcode.Errorf(code, "quota: %s: %s", field, iss[0].Message)
}

// Filter is a compiled quota Policy: a Consumptive admission Filter whose
// reservation the executor batches, which refunds in onLog. It is
// immutable after Build and safe for concurrent use; per-request state
// lives in a pooled reqState in the Policy's PolicyState slot.
type Filter struct {
	name    string
	quota   string
	cache   *Cache
	store   statestore.Store
	enqueue statestore.Enqueuer
	timeout time.Duration
	clock   clock.Clock
	// open is failureMode open.
	open bool
	// keyIsName reads the Consumer's name for the default key; key is the
	// compiled config.key otherwise, nil for an empty key (emptyKey). cost
	// is nil for the default cost.
	keyIsName bool
	key       expr.Program
	cost      expr.Program
	emptyKey  statestore.Digest
	// nameHash identifies config.consumerQuota in the denial cache.
	nameHash uint64
	// The registered statuses of the 429 and 403 codes.
	denyStatus    int
	noQuotaStatus int
	decisions     [emit.NumQuotaResults]emit.Counter
	pool          sync.Pool
}

var (
	_ filter.Consumptive = (*Filter)(nil)
	_ filter.Finisher    = (*Filter)(nil)
)

// reqState is one request's state of the Policy, pooled by the Filter
// and kept in the Policy's PolicyState slot from Prepare to Finish.
type reqState struct {
	// The applied Consumer quota: its limit, its window or calendar month,
	// and its RateLimit field's w.
	limit  int64
	window time.Duration
	month  bool
	w      int64
	// The request's key digest and cost, and the denial cache tag.
	digest statestore.Digest
	cost   int64
	tag    uint64
	// charge is the request's record of the counter; charger is set when
	// this Policy charged it, follower when it took the charger's
	// decision, counted once a follower counted that decision.
	charge   *filter.Charge
	charger  bool
	follower bool
	counted  bool
	// cached is set when the denial cache held the key; reserved when the
	// call reserved the cost in the window starting at start, which a
	// refund returns.
	cached   bool
	reserved bool
	start    time.Time
	// call and calls back Handle's own round trip.
	call  statestore.Call
	calls [1]*statestore.Call
}

// reset clears the decision fields for a new decision, keeping Handle's
// round-trip scratch.
func (st *reqState) reset() {
	call, calls := st.call, st.calls
	*st = reqState{call: call, calls: calls}
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

// peek returns the request's reqState when the Policy decided in this
// request.
func peek(x filter.Exchange) (*reqState, bool) {
	st, ok := (*x.PolicyState()).(*reqState)
	return st, ok && st != nil
}

// Handle decides in onRequestHeaders and settles in onLog. The executor
// batches onRequestHeaders through Prepare and Complete instead; Handle
// runs the same steps with its own round trip, for a caller without
// batching.
func (flt *Filter) Handle(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	switch p {
	case phase.OnRequestHeaders:
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
	case phase.OnLog:
		flt.settle(x)
		return filter.Next()
	default:
		return filter.Next()
	}
}

// Prepare runs every Node-local step (Consumptive). A request without a
// Consumer, or of one lacking the quota or holding it with unit tokens,
// is denied 403 RZ-RL-004 before config.key and config.cost are
// evaluated. A runtime error of either, or a cost below 1, applies
// failureMode (RZ-RL-005 under closed). A counter an earlier Policy of
// the request charged takes that Policy's decision without a call, its
// field without a count; a cached denial whose units the cost exceeds is
// denied 429 RZ-RL-003; otherwise Prepare fills call with one Quota
// reservation.
func (flt *Filter) Prepare(ctx context.Context, x filter.Exchange, call *statestore.Call) (filter.Result, bool) {
	st := flt.state(x)
	st.reset()
	v := x.Vars()
	var c *expr.Consumer
	if v != nil {
		c = v.Consumer
	}
	q, ok := quotaOf(c, flt.quota)
	if !ok {
		flt.count(x, emit.QuotaNoQuota)
		return filter.Deny(flt.noQuotaStatus, CodeNoQuota, nil), true
	}
	now := flt.clock.Now()
	st.limit, st.month = q.Limit, q.Calendar == v1alpha1.QuotaCalendarMonth
	if !st.month && q.Window != nil {
		st.window = time.Duration(*q.Window)
	}
	st.w = windowSeconds(st.window, st.month, now)
	if err := flt.evaluate(ctx, v, c, st); err != nil {
		flt.record(x, st, false, 0, 0)
		flt.count(x, flt.failure())
		return filter.Undecided(CodeUndecided, err), true
	}
	rec, first := x.Charge(filter.Counter{Quota: flt.quota, Window: st.window, Month: st.month, Key: st.digest})
	st.charge = rec
	if !first {
		// The charging Policy's denial ends the request, so a follower
		// only ever admits, without a count of its own; it counts the
		// charger's decision once known.
		st.follower = true
		flt.record(x, st, false, 0, 0)
		if rec != nil && rec.Decided {
			st.counted = true
			flt.count(x, rec.Result)
		}
		return filter.Next(), true
	}
	st.charger = true
	st.tag = flt.cache.tag(flt.nameHash, st.window, st.month, st.limit)
	if units, end, held := flt.cache.lookup(st.tag, &st.digest, now.UnixNano()); held {
		st.cached = true
		if st.cost > units {
			flt.decide(st, emit.QuotaDeny)
			flt.count(x, emit.QuotaDeny)
			return flt.deny(x, st, units, time.Unix(0, end), now), true
		}
	}
	*call = statestore.Call{Kind: statestore.OpQuota, Timeout: flt.timeout, Batch: -1, Quota: statestore.Quota{
		Name: flt.quota, Window: st.window, Month: st.month, Limit: st.limit, Cost: st.cost, Digest: st.digest,
	}}
	return filter.Result{}, false
}

// Complete applies the reservation's reply (Consumptive): an admission
// records the units left and the seconds to the window's end and forgets
// any cached denial; a denial caches the units left until min(60 s, the
// window's end) and answers 429 RZ-RL-003; a failed, timed-out or
// unattempted call, or an open breaker, applies failureMode: open admits
// unmetered, closed answers 503 with the call's RZ-STS code.
func (flt *Filter) Complete(_ context.Context, x filter.Exchange, call *statestore.Call) filter.Result {
	st := flt.state(x)
	if call.Err != nil || !call.Done {
		err := call.Err
		if err == nil {
			err = statestore.ErrNotAttempted
		}
		flt.record(x, st, false, 0, 0)
		res := flt.failure()
		flt.decide(st, res)
		flt.count(x, res)
		return filter.Undecided(err.Code(), err)
	}
	q := &call.Quota
	if st.month && q.WindowEnd.After(q.WindowStart) {
		st.w = seconds(q.WindowEnd.Sub(q.WindowStart))
	}
	units := max(st.limit-q.Used, 0)
	if !q.Allowed {
		flt.cache.store(st.tag, &st.digest, units, q.WindowEnd.UnixNano(), flt.clock.Now().UnixNano())
		flt.decide(st, emit.QuotaDeny)
		flt.count(x, emit.QuotaDeny)
		return flt.deny(x, st, units, q.WindowEnd, q.ServerNow)
	}
	st.reserved, st.start = true, q.WindowStart
	if st.cached {
		flt.cache.forget(st.tag, &st.digest)
	}
	flt.decide(st, emit.QuotaAllow)
	flt.count(x, emit.QuotaAllow)
	flt.record(x, st, true, units, seconds(q.WindowEnd.Sub(q.ServerNow)))
	return filter.Next()
}

// Undo is the Consumptive step for a Policy that will not be completed:
// Prepare reserves nothing on the Node, and a call never sent charged
// nothing, so nothing returns.
func (*Filter) Undo(filter.Exchange) {}

// settle returns the reservation in onLog when the request ends refunded
// (filter.Final.Refunds): only the charging Policy refunds, and only a
// call that reserved; the refund of the cost goes to the post-commit
// queue's settle class with the Policy's stateStoreTimeout. A dropped
// refund only over-charges.
func (flt *Filter) settle(x filter.Exchange) {
	st, ok := peek(x)
	if !ok || !st.charger || !st.reserved || flt.enqueue == nil || !x.Final().Refunds() {
		return
	}
	st.reserved = false
	flt.enqueue.Enqueue(statestore.ClassSettle, &statestore.Write{
		Kind: statestore.OpRefund, Timeout: flt.timeout, Refund: statestore.Refund{
			Name: flt.quota, Window: st.window, Month: st.month, WindowStart: st.start, Units: st.cost, Digest: st.digest,
		},
	})
}

// Finish counts a follower's decision, the charging Policy's, when it
// was not known in Prepare, and returns the request's reqState to the pool
// (filter.Finisher).
func (flt *Filter) Finish(_ context.Context, x filter.Exchange) {
	p := x.PolicyState()
	st, ok := (*p).(*reqState)
	if !ok || st == nil {
		return
	}
	if st.follower && !st.counted && st.charge != nil && st.charge.Decided {
		flt.count(x, st.charge.Result)
	}
	*p = nil
	*st = reqState{}
	flt.pool.Put(st)
}

// quotaOf returns consumer c's quota named name when it counts requests.
func quotaOf(c *expr.Consumer, name string) (v1alpha1.Quota, bool) {
	if c == nil {
		return v1alpha1.Quota{}, false
	}
	q, ok := c.QuotaByName[name]
	return q, ok && q.Unit == v1alpha1.QuotaUnitRequests
}

// evaluate evaluates config.key and config.cost once into st: the key
// digest and the cost. It returns the first runtime error, an
// invalid-value error for a cost below 1, or for a quota without a
// window of at least 1 ms or a calendar month.
func (flt *Filter) evaluate(ctx context.Context, v *expr.Vars, c *expr.Consumer, st *reqState) error {
	var first error
	switch {
	case flt.keyIsName:
		st.digest = digestOf(c.Name)
	case flt.key != nil:
		s, err := flt.key.EvalString(ctx, v)
		if err != nil {
			first = classify(err)
		}
		st.digest = digestOf(s)
	default:
		st.digest = flt.emptyKey
	}
	st.cost = 1
	if flt.cost != nil {
		n, err := flt.cost.EvalInt(ctx, v)
		if err != nil && first == nil {
			first = classify(err)
		}
		st.cost = n
	}
	switch {
	case first != nil:
	case st.cost < 1:
		first = errBadCost
	case !st.month && st.window < time.Millisecond:
		first = errNoWindow
	}
	return first
}

// decide records the charging Policy's decision for its followers.
func (*Filter) decide(st *reqState, result int) {
	if st.charge != nil {
		st.charge.Decided, st.charge.Result = true, result
	}
}

// record adds the Policy's RateLimit field: the quota's limit and window,
// with the units left and the seconds to the window's end when known.
func (flt *Filter) record(x filter.Exchange, st *reqState, known bool, r, t int64) {
	f := filter.RateLimitField{Name: flt.name, Q: st.limit, W: st.w}
	if known {
		f.R, f.T, f.Known = r, t, true
	}
	x.AddRateLimitField(f)
}

// deny answers 429 RZ-RL-003 for a window with units left that ends at
// end: Retry-After is the seconds from now to end plus a spread of up to
// 1% of the window, at most 60 s (target), rounded up and at least 1, and
// the field lists r = units and t = Retry-After for the denied quota.
func (flt *Filter) deny(x filter.Exchange, st *reqState, units int64, end, now time.Time) filter.Result {
	spread := maxJitter
	if !st.month {
		spread = min(st.window/jitterShare, maxJitter)
	}
	t := max(seconds(end.Add(flt.cache.jitter(spread)).Sub(now)), 1)
	x.AddRateLimitField(filter.RateLimitField{Name: flt.name, Q: st.limit, W: st.w, R: units, T: t, Known: true, Denied: true})
	return filter.Deny(flt.denyStatus, CodeExhausted, http.Header{headerRetryAfter: {strconv.FormatInt(t, 10)}})
}

// failure returns the decision result of a failure under the Policy's
// failureMode.
func (flt *Filter) failure() int {
	if flt.open {
		return emit.QuotaFailOpen
	}
	return emit.QuotaFailClosed
}

// count adds one ruralz_quota_decisions_total observation.
func (flt *Filter) count(x filter.Exchange, result int) {
	if result < 0 || result >= len(flt.decisions) {
		return
	}
	if c := flt.decisions[result]; c != nil {
		c.Add(x.Stripe(), 1)
	}
}

// windowSeconds returns the RateLimit field's w: the window in seconds,
// rounded up, or the length of the UTC calendar month holding now.
func windowSeconds(window time.Duration, month bool, now time.Time) int64 {
	if month {
		w := statestore.QuotaWindowOf(now.UnixMilli(), 0, true)
		return (w.End - w.Start) / 1000
	}
	return seconds(window)
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

// invalidValue is an invalid-value error, error.type invalid_value.
type invalidValue string

// Error describes the error.
func (e invalidValue) Error() string { return string(e) }

// ErrorType returns the span's error.type.
func (invalidValue) ErrorType() string { return errTypeInvalidValue }

// Invalid values that apply failureMode.
var (
	// errBadCost is a config.cost below 1.
	errBadCost error = invalidValue("quota: config.cost is below 1")
	// errNoWindow is a Consumer quota with neither a window of at least
	// 1 ms nor a calendar month, which validation refuses.
	errNoWindow error = invalidValue("quota: the Consumer quota has no window")
)
