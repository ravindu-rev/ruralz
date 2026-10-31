// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/cache/coalesce"
	"github.com/ravindu-rev/ruralz/internal/filter/cache/revalidate"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// generationKeyBytes is what a store that creates a URI's generation key
// counts for it under the memory rules ("Response caching": about 110
// bytes with its token; hypothesis).
const generationKeyBytes = 110

// Filter is a compiled cache Policy. It is immutable after Build and safe
// for concurrent use; the Node-wide state stays its Factory's.
type Filter struct {
	f        *Factory
	policy   string
	key      expr.Program
	store    statestore.Store
	queue    statestore.Enqueuer
	timeout  time.Duration
	replayer filter.Replayer
	skipped  *[emit.NumSkipReasons]emit.Counter
	// credentials are the canonical names of the credential fields.
	credentials []string
	clk         clock.Clock
	log         *slog.Logger
}

var (
	_ filter.Filter      = (*Filter)(nil)
	_ filter.Finisher    = (*Filter)(nil)
	_ filter.Invalidator = (*Filter)(nil)
	_ filter.Substituter = (*Filter)(nil)
)

// Handle looks the request up in onRequestHeaders and records the outcome
// on the response in onResponse (package documentation).
func (f *Filter) Handle(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	switch p {
	case phase.OnRequestHeaders:
		return f.request(ctx, x)
	case phase.OnResponse:
		f.response(x)
	default:
	}
	return filter.Next()
}

// request decides a GET or HEAD in onRequestHeaders: bypass, serve a
// stored response, revalidate it on the request path, or forward a miss,
// alone or within a coalesced flight. Every other method continues: the
// Policy only invalidates for it.
func (f *Filter) request(ctx context.Context, x filter.Exchange) filter.Result {
	method := x.Method()
	if method != http.MethodGet && method != http.MethodHead {
		return filter.Next()
	}
	st := f.f.state()
	*x.PolicyState() = st
	st.head = method == http.MethodHead
	h := x.Header()
	st.req = revalidate.Parse(h)
	if r := x.Route(); r != nil {
		st.route = r.Name
	}
	if x.Received().ContentLength != 0 || st.req.NoStore {
		return f.bypass(x, st)
	}
	value, ok := f.partition(ctx, x)
	if !ok {
		return f.bypass(x, st)
	}
	st.credentialed, st.authorized = f.credentialsOf(x, h)
	now := f.clk.Now()
	st.key = statestore.CacheKey{URI: f.uri(x), Partition: PartitionDigest(st.route, value)}
	st.names = f.f.names.get(st.key.URI)
	st.variant = Variant(h, st.names)
	lk, serr := f.lookup(ctx, x, st, now)
	if serr != nil {
		// Under open the executor skips the Policy for the rest of the
		// request, under closed it answers 503 with the RZ-STS code;
		// either way the request is a bypass, counted in Finish.
		st.result, st.fwd, st.bypass = emit.CacheBypass, fwdBypass, true
		f.setResult(x, st)
		return filter.Undecided(serr.Code(), serr)
	}
	e := f.classify(st, lk)
	if e == nil {
		return f.miss(ctx, x, st)
	}
	st.e = e
	st.age = e.initialAge + lk.Resident
	st.fresh = revalidate.FreshnessOf(e.header, now.Add(-lk.Resident))
	st.lookedAt, st.storedAt = now, now.Add(-lk.Resident)
	switch st.fresh.Use(st.age, &st.req) {
	case revalidate.UseFresh:
		return f.serveStored(ctx, x, st, emit.CacheHit, len(lk.Entry))
	case revalidate.UseStale:
		if !e.credentialed() && f.replayer != nil {
			r := f.serveStored(ctx, x, st, emit.CacheStale, len(lk.Entry))
			if st.served {
				f.revalidateLater(x, st, lk.Entry)
			}
			return r
		}
		st.fwd = fwdStale
	default:
		st.fwd = fwdStale
		if st.fresh.Use(st.age, nil) != revalidate.UseValidate {
			st.fwd = fwdRequest
		}
	}
	return f.validate(x, st)
}

// partition returns the partition value: config.key's result; else the
// principal of an authenticated request; else "". A config.key that fails
// to evaluate, or an authenticated request without a principal
// (OQ-security-and-identity-32), bypasses the cache: ok is false.
func (f *Filter) partition(ctx context.Context, x filter.Exchange) (string, bool) {
	if f.key != nil {
		v, err := f.key.EvalString(ctx, x.Vars())
		if err != nil {
			f.log.DebugContext(ctx, "cache config.key failed to evaluate; the request bypasses the cache",
				catalog.KeyPolicy, f.policy, catalog.KeyError, err.Error())
			return "", false
		}
		return v, true
	}
	if id := x.Identity(); id != nil {
		return id.Principal, id.Principal != ""
	}
	return "", true
}

// credentialsOf reports whether the request carries credentials, a
// credential field or an identity auth.api-key or auth.basic proved and
// removed, and whether it carried Authorization.
func (f *Filter) credentialsOf(x filter.Exchange, h http.Header) (credentialed, authorized bool) {
	for _, name := range f.credentials {
		if lines(h, name) > 0 {
			credentialed = true
			authorized = authorized || name == fieldAuthorization
		}
	}
	if id := x.Identity(); id != nil {
		switch id.Method {
		case filter.MethodBasic:
			credentialed, authorized = true, true
		case filter.MethodAPIKey:
			credentialed = true
		default:
		}
	}
	return credentialed, authorized
}

// uri returns U of x's request as it arrived.
func (f *Filter) uri(x filter.Exchange) statestore.Digest {
	rcv := x.Received()
	return URIDigest(x.Scheme(), x.Host(), rcv.Authority, x.Path(), rcv.RawQuery)
}

// lookup reads st's variant at now: from the hot-entry layer when it holds
// both the URI's generation token and the variant, else in one pipelined
// State Store batch, which leaves the generation key unread when the layer
// holds its token. Failures are the batch's RZ-STS error.
func (f *Filter) lookup(ctx context.Context, x filter.Exchange, st *state, now time.Time) (*statestore.CacheLookup, *statestore.Error) {
	l := &st.call.Lookup
	tok, tokHeld := f.f.hot.token(st.key.URI, now)
	if tokHeld {
		if he, ok := f.f.hot.entry(st.key, st.variant, now); ok {
			*l = statestore.CacheLookup{
				Key: st.key, Variant: st.variant, SkipToken: true, Token: tok, HasToken: true,
				Partition: true, Names: he.names, Found: true, Entry: he.entry, Resident: he.resident,
			}
			return l, nil
		}
	}
	st.call = statestore.Call{Kind: statestore.OpCacheGet, Timeout: f.timeout, Lookup: statestore.CacheLookup{
		Key: st.key, Variant: st.variant, SkipToken: tokHeld, Token: tok, HasToken: tokHeld, Entry: st.buf[:0],
	}}
	st.calls[0] = &st.call
	f.store.Read(ctx, x.StateBudget(), st.calls[:])
	if cap(l.Entry) > cap(st.buf) {
		st.buf = l.Entry[:0]
	}
	if st.call.Err != nil {
		return nil, st.call.Err
	}
	if !tokHeld {
		f.f.hot.readToken(st.key.URI, l.Token, l.HasToken, now)
	}
	f.f.hot.readEntry(st.key, st.variant, l, now)
	return l, nil
}

// classify returns the stored response the lookup found when it can
// serve: the partition holds the variant under the names V was computed
// from, and the URI's generation key holds the token the entry records.
// Otherwise it sets the miss reason, learning the partition's names when
// they differ, and returns nil: uri-miss without the partition or a
// usable entry, vary-miss when the partition holds no matching variant.
func (f *Filter) classify(st *state, lk *statestore.CacheLookup) *entry {
	st.token, st.hasToken = lk.Token, lk.HasToken
	switch {
	case !lk.Partition:
		st.fwd = fwdURIMiss
		return nil
	case lk.Names != st.names:
		f.f.names.learn(st.key.URI, lk.Names)
		st.fwd = fwdVaryMiss
		return nil
	case !lk.Found:
		st.fwd = fwdVaryMiss
		return nil
	}
	if err := decodeEntry(&st.found, lk.Entry); err != nil || !lk.HasToken || st.found.token != lk.Token {
		st.fwd = fwdURIMiss
		return nil
	}
	return &st.found
}

// serveStored serves the stored response as result (hit or stale) after
// reserving size bytes of the buffer budget; a refused reservation
// bypasses the cache.
func (f *Filter) serveStored(_ context.Context, x filter.Exchange, st *state, result, size int) filter.Result {
	if err := x.Reserve(int64(size)); err != nil {
		return f.bypass(x, st)
	}
	st.reserved = int64(size)
	st.result, st.hit, st.served = result, true, true
	st.ttl, st.hasTTL = ttlOf(st.fresh, st.age), true
	f.setResult(x, st)
	e := st.e
	return filter.Reply(f.reply(st, x.Header(), e.status, e.header, e.body, st.age, st.storedAt, true))
}

// revalidateLater queues the background revalidation of the stale variant
// st serves, over its own copy of the entry bytes b, unless one for the
// partition is already pending on this Node.
func (f *Filter) revalidateLater(x filter.Exchange, st *state, b []byte) {
	if f.f.rv.Pending(st.key) {
		return
	}
	e := &entry{}
	if decodeEntry(e, append([]byte(nil), b...)) != nil {
		return
	}
	f.f.rv.Enqueue(revalidate.Job{
		Key: st.key, Variant: st.variant, Store: f.store, Timeout: f.timeout, Replayer: f.replayer,
		Route: st.route, Upstream: e.upstream, Entry: &jobEntry{e: e}, Metrics: x.RouteMetrics(),
		StoreSkipped: f.skipped,
	})
}

// validate forwards the request with the stored response's validators in
// place of the client's own preconditions and Range, which st keeps for
// the response the cache may serve in its place (Substitute); with
// only-if-cached it answers 504 instead.
func (f *Filter) validate(x filter.Exchange, st *state) filter.Result {
	st.result = emit.CacheMiss
	if st.req.OnlyIfCached {
		return f.onlyIfCached(x, st)
	}
	h := x.Header()
	st.reqHeader = h.Clone()
	st.cond = conditions(h)
	revalidate.Conditional(h, st.e.header)
	st.validating, st.forward = true, true
	st.requested = f.clk.Now()
	f.setResult(x, st)
	return filter.Next()
}

// miss forwards a request no stored response serves; with only-if-cached
// it answers 504 instead. A GET without Range leads or follows the key's
// coalesced flight, a HEAD or Range request only follows one, and a
// request that asks for revalidation takes no part. A follower reuses the
// leader's response like a hit when its own Vary-selected values match,
// and otherwise fetches alone.
func (f *Filter) miss(ctx context.Context, x filter.Exchange, st *state) filter.Result {
	st.result = emit.CacheMiss
	if st.req.OnlyIfCached {
		return f.onlyIfCached(x, st)
	}
	h := x.Header()
	k := coalesce.Key{Cache: st.key}
	if st.hasToken {
		k.Token = st.token
	}
	var tk coalesce.Ticket[*published]
	switch {
	case st.req.Revalidates():
	case st.head || lines(h, "Range") > 0:
		tk = f.f.flights.Follow(k)
	default:
		tk = f.f.flights.Lead(k)
	}
	if tk.Role() == coalesce.Follower {
		if f.f.onWait != nil {
			f.f.onWait()
		}
		if p, out := tk.Wait(ctx, selector{h: h}); out == coalesce.Reuse {
			if r, ok := f.serveFlight(x, st, p); ok {
				return r
			}
		}
	} else {
		st.ticket = tk
	}
	st.forward = true
	if !st.head {
		st.reqHeader = h.Clone()
	}
	st.requested = f.clk.Now()
	f.setResult(x, st)
	return filter.Next()
}

// selector computes a follower's variant under its leader's names.
type selector struct{ h http.Header }

// Variant implements coalesce.Selector.
func (s selector) Variant(names string) statestore.Digest { return Variant(s.h, names) }

// serveFlight serves the leader's published response p to a follower like
// a hit, recorded coalesced; false when the buffer budget refuses it, so
// the follower fetches alone.
func (f *Filter) serveFlight(x filter.Exchange, st *state, p *published) (filter.Result, bool) {
	if err := x.Reserve(int64(len(p.body))); err != nil {
		return filter.Result{}, false
	}
	st.reserved = int64(len(p.body))
	st.result, st.collapsed, st.served = emit.CacheCoalesced, true, true
	age := p.initialAge + max(f.clk.Now().Sub(p.received), 0)
	f.setResult(x, st)
	return filter.Reply(f.reply(st, x.Header(), p.status, p.header, p.body, age, p.received, true)), true
}

// bypass keeps the cache out of the request: no lookup result is used and
// nothing is stored; with only-if-cached it answers 504 instead.
func (f *Filter) bypass(x filter.Exchange, st *state) filter.Result {
	st.result, st.fwd, st.bypass, st.e = emit.CacheBypass, fwdBypass, true, nil
	if st.req.OnlyIfCached {
		return f.onlyIfCached(x, st)
	}
	st.forward = true
	f.setResult(x, st)
	return filter.Next()
}

// onlyIfCached answers a request with only-if-cached that no stored
// response serves: 504 RZ-RT-018, never forwarded. The cache's onResponse
// does not run on a generated response, so it carries its Cache-Status
// member, which names neither a hit nor a forward.
func (f *Filter) onlyIfCached(x filter.Exchange, st *state) filter.Result {
	st.forward = false
	f.setResult(x, st)
	return filter.Deny(http.StatusGatewayTimeout, CodeOnlyIfCached, http.Header{"Cache-Status": {memberName}})
}

// setResult records st's result in the access log's cache field.
func (f *Filter) setResult(x filter.Exchange, st *state) {
	if st.result >= 0 && st.result < emit.NumCacheResults {
		x.SetCacheResult(f.f.results[st.result])
	}
}

// response runs in onResponse: it starts the store of a forwarded GET's
// leg response, resolves a flight it cannot lead to a store, and appends
// the Policy's Cache-Status member.
func (f *Filter) response(x filter.Exchange) {
	st := stateOf(x)
	if st == nil || st.result < 0 {
		return
	}
	if st.received.IsZero() {
		st.received = f.clk.Now()
	}
	if st.forward && !st.served && !st.bypass && !st.head {
		f.startStore(x, st)
	} else {
		st.ticket.Abandon()
	}
	if h := x.Message().Header(); h != nil {
		h.Add("Cache-Status", member(st))
	}
}

// Substitute serves a stored response in place of the leg's response to a
// request-path validation (filter.Substituter): the stored response a 304
// selecting it freshened, whose refresh Finish queues when it is still
// storable under the same Vary names with TTL left, or, under
// stale-if-error, the stored response in place of a 500, 502, 503 or 504,
// from the Upstream or Node-generated for a failed leg. A 304 that selects
// no stored response passes through. The served response applies the
// client's own preconditions and Range; an unsatisfiable range gets the
// whole response.
func (f *Filter) Substitute(_ context.Context, x filter.Exchange) *filter.Response {
	st := stateOf(x)
	if st == nil || st.e == nil || !st.validating || st.served {
		return nil
	}
	now := f.clk.Now()
	if st.received.IsZero() {
		st.received = now
	}
	status := x.Message().Status()
	if legStatus, legHdr := x.LegResponse(); legStatus == http.StatusNotModified && status == http.StatusNotModified {
		if legHdr == nil {
			legHdr = x.Message().Header()
		}
		return f.refreshed(x, st, legHdr)
	}
	if revalidate.IsError(status) {
		return f.staleIfError(x, st, status, now)
	}
	return nil
}

// refreshed serves the stored response freshened by a 304 with header nm,
// and prepares its refresh.
func (f *Filter) refreshed(x filter.Exchange, st *state, nm http.Header) *filter.Response {
	e := st.e
	h, ok := revalidate.Freshen(e.header, nm, st.received)
	if !ok {
		return nil
	}
	if err := x.Reserve(int64(len(e.body))); err != nil {
		return nil
	}
	st.reserved += int64(len(e.body))
	fr := revalidate.FreshnessOf(h, st.received)
	age := revalidate.InitialAge(h, st.requested, st.received)
	if ttl := fr.TTL(age); ttl > 0 && Names(h) == Names(e.header) && revalidate.Storable(h, e.authorized()) {
		re := *e
		re.header, re.initialAge = h, age
		if b := encodeEntry(&re); len(b) <= MaxEntry {
			st.refresh, st.refreshTTL, st.refreshNames = b, ttl, Names(h)
		}
	}
	st.served, st.fwdStatus = true, http.StatusNotModified
	if t := ttlOf(fr, age); t < 0 {
		st.ttl, st.hasTTL = t, true
	}
	f.setResult(x, st)
	return f.reply(st, st.cond, e.status, h, e.body, age, st.received, false)
}

// staleIfError serves the stored response in place of an error with
// status when stale-if-error, the response's or the request's, covers its
// current age.
func (f *Filter) staleIfError(x filter.Exchange, st *state, status int, now time.Time) *filter.Response {
	e := st.e
	age := st.age + max(now.Sub(st.lookedAt), 0)
	if !st.fresh.ServesOnError(age, &st.req) {
		return nil
	}
	if err := x.Reserve(int64(len(e.body))); err != nil {
		return nil
	}
	st.reserved += int64(len(e.body))
	st.result, st.served, st.fwdStatus = emit.CacheStaleError, true, status
	if t := ttlOf(st.fresh, age); t < 0 {
		st.ttl, st.hasTTL = t, true
	}
	f.setResult(x, st)
	return f.reply(st, st.cond, e.status, e.header, e.body, age, st.storedAt, false)
}

// Invalidate bumps the URI's generation key to a fresh token after a 2xx
// or 3xx leg response to an unsafe method, any method other than GET,
// HEAD, OPTIONS and TRACE (filter.Invalidator; RFC 9111 section 4.4), so
// no entry written under an earlier token hits again. It runs whatever
// spec.when decided, also when the client left before commit; the bump
// goes to the invalidation class of the post-commit queue, and this
// Node's hot-entry layer forgets the URI's token at once.
func (f *Filter) Invalidate(ctx context.Context, x filter.Exchange) {
	switch x.Method() {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return
	}
	if s := x.Final().LegStatus; s < http.StatusOK || s >= http.StatusBadRequest {
		return
	}
	u := f.uri(x)
	w := &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: f.timeout, Invalidate: u, Bump: f.f.newToken()}
	if !f.queue.Enqueue(statestore.ClassInvalidate, w) {
		f.log.DebugContext(ctx, "cache invalidation dropped by the post-commit queue", catalog.KeyPolicy, f.policy)
	}
	f.f.hot.dropToken(u)
}

// Finish counts the request's cache result, queues the store its tee
// completed and the refresh a 304 prepared, resolves its flight, releases
// the buffer budget a served response held and returns its state.
func (f *Filter) Finish(ctx context.Context, x filter.Exchange) {
	st := stateOf(x)
	if st == nil {
		return
	}
	*x.PolicyState() = nil
	defer f.f.release(st)
	if st.result >= 0 && st.result < emit.NumCacheResults {
		if c := x.RouteMetrics().Cache[st.result]; c != nil {
			c.Add(x.Stripe(), 1)
		}
	}
	f.finishStore(ctx, x, st)
	f.finishRefresh(ctx, x, st)
	st.ticket.Abandon()
	if st.reserved > 0 {
		x.Release(st.reserved)
	}
}

// skip counts a skipped store for reason (emit.Skip*).
func (f *Filter) skip(x filter.Exchange, reason int) {
	if f.skipped != nil && f.skipped[reason] != nil {
		f.skipped[reason].Add(x.Stripe(), 1)
	}
}
