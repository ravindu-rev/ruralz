// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"net/http"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/cache/coalesce"
	"github.com/ravindu-rev/ruralz/internal/filter/cache/revalidate"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// storable reports whether a leg response with status and header h to a
// request that carried Authorization (authorized) may be stored ("Response
// caching", Storable row): a storable final status, the header rules and
// never a partial composition response.
func storable(status int, h http.Header, authorized bool) bool {
	return revalidate.StorableStatus(status) && !revalidate.Partial(h) && revalidate.Storable(h, authorized)
}

// startStore starts storing a forwarded GET's leg response, as the leg
// delivered it, before any client onResponse Policy changed it
// (filter.Exchange.LegResponse): when it is storable with TTL left, it
// encodes the entry without its body and starts the tee of at most the
// room MaxEntry leaves the body. Encoded header blocks and saved request
// alone past MaxEntry count size_limit, and the data plane counts a tee it
// refuses or stops. A leader tells its flight the copy started; a response
// that is not stored resolves the flight at once.
func (f *Filter) startStore(x filter.Exchange, st *state) {
	status, lh := x.LegResponse()
	if status == 0 || lh == nil || !storable(status, lh, st.authorized) {
		st.ticket.Abandon()
		return
	}
	fr := revalidate.FreshnessOf(lh, st.received)
	age := revalidate.InitialAge(lh, st.requested, st.received)
	ttl := fr.TTL(age)
	if ttl <= 0 {
		st.ticket.Abandon()
		return
	}
	vars := x.Vars()
	e := &st.pending
	*e = entry{
		status: status, initialAge: age, route: st.route, method: x.Method(),
		reqHeader: f.savedRequest(x.Header()), header: storedHeader(lh, st.received),
	}
	rawQuery := x.Received().RawQuery
	if vars != nil {
		if vars.Request != nil {
			rawQuery = vars.Request.RawQuery()
		}
		if vars.Upstream != nil {
			e.upstream = vars.Upstream.Name
		}
	}
	e.target = target(x, rawQuery)
	if st.credentialed {
		e.flags |= flagCredentialed
	}
	if st.authorized {
		e.flags |= flagAuthorized
	}
	if e.upstream == "" {
		e.flags |= flagComposition
	}
	if st.hasToken {
		e.token = st.token
	} else {
		e.token, st.create = f.f.newToken(), true
	}
	st.prefix = appendPrefix(st.prefix[:0], e)
	if len(st.prefix) > MaxEntry {
		f.skip(x, emit.SkipSizeLimit)
		st.ticket.Abandon()
		return
	}
	tee := x.TeeResponse(int64(MaxEntry - len(st.prefix)))
	if tee == nil {
		st.ticket.Abandon()
		return
	}
	st.tee, st.stored, st.storeTTL = tee, true, ttl
	st.storeNames = Names(lh)
	st.storeVariant = Variant(st.reqHeader, st.storeNames)
	st.ticket.Streaming(tee.Stopped())
}

// finishStore completes the store startStore began: once the tee holds the
// whole body it publishes the response to the flight's followers, learns
// the URI's Vary names and, when the memory rules admit the entry's bytes
// (plus the generation key a store creates), queues the store, under the
// token its lookup read, or creating the generation key with a fresh one.
// A refusal of the memory rules counts memory; a store the queue drops is
// counted there.
func (f *Filter) finishStore(ctx context.Context, x filter.Exchange, st *state) {
	if st.tee == nil {
		return
	}
	body, complete := st.tee.Result()
	n := len(st.prefix) + len(body)
	if !complete || n > MaxEntry {
		st.ticket.Abandon()
		return
	}
	e := &st.pending
	if st.ticket.Role() == coalesce.Leader {
		st.ticket.Publish(st.storeNames, st.storeVariant, &published{
			status: e.status, header: e.header, body: append([]byte(nil), body...),
			initialAge: e.initialAge, received: st.received,
		})
	}
	f.f.names.learn(st.key.URI, st.storeNames)
	charged := n
	if st.create {
		charged += generationKeyBytes
	}
	if !f.store.AdmitStoreBytes(st.key, charged) {
		f.skip(x, emit.SkipMemory)
		return
	}
	b := make([]byte, 0, n)
	b = append(append(b, st.prefix...), body...)
	f.enqueueStore(ctx, &statestore.CacheStore{
		Key: st.key, Names: st.storeNames, Variant: st.storeVariant, Generation: e.token, Create: st.create,
		TTL: st.storeTTL, Entry: b, Received: st.received,
	})
}

// finishRefresh queues the refresh a request-path 304 prepared: the
// freshened entry under the token its lookup read, when the memory rules
// admit it.
func (f *Filter) finishRefresh(ctx context.Context, x filter.Exchange, st *state) {
	if st.refresh == nil {
		return
	}
	if !f.store.AdmitStoreBytes(st.key, len(st.refresh)) {
		f.skip(x, emit.SkipMemory)
		return
	}
	f.enqueueStore(ctx, &statestore.CacheStore{
		Key: st.key, Names: st.refreshNames, Variant: st.variant, Generation: st.token,
		TTL: st.refreshTTL, Entry: st.refresh, Received: st.received,
	})
}

// enqueueStore queues c, with a fresh fill lease token, in the store class
// of the post-commit queue, which owns the write from then on.
func (f *Filter) enqueueStore(ctx context.Context, c *statestore.CacheStore) {
	c.Token = f.f.uint64()
	w := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: f.timeout, Cache: *c}
	if !f.queue.Enqueue(statestore.ClassStore, w) {
		f.log.DebugContext(ctx, "cache store dropped by the post-commit queue", catalog.KeyPolicy, f.policy)
	}
}

// savedRequest returns the saved request's header: the request header as
// the request Phases left it, without hop-by-hop fields, credential
// fields, preconditions or Range, which a replay's validation replaces.
func (f *Filter) savedRequest(h http.Header) http.Header {
	out := h.Clone()
	if out == nil {
		out = http.Header{}
	}
	httpfield.StripHopByHop(out, httpfield.TowardUpstream)
	for _, name := range f.credentials {
		strip(out, name)
	}
	for _, name := range conditionFields() {
		strip(out, name)
	}
	return out
}

// storedHeader returns the header a stored response keeps of the leg's
// header h, received at received: every end-to-end field, without
// Content-Length, which serving sets from the body, and with Date set to
// received when it is missing or not a valid HTTP-date (RFC 9110 section
// 6.6.1), so freshness and age never depend on the receipt time again.
func storedHeader(h http.Header, received time.Time) http.Header {
	out := h.Clone()
	if out == nil {
		out = http.Header{}
	}
	httpfield.StripHopByHop(out, httpfield.TowardClient)
	delete(out, "Content-Length")
	if _, err := http.ParseTime(firstLine(out, "Date")); err != nil {
		out["Date"] = []string{received.UTC().Format(http.TimeFormat)}
	}
	return out
}

// target returns the saved request's target in absolute form: the scheme,
// the authority as the client sent it, the normalized path and rawQuery.
func target(x filter.Exchange, rawQuery string) string {
	authority := x.Received().Authority
	if authority == "" {
		authority = x.Host()
	}
	t := x.Scheme() + "://" + authority + x.Path()
	if rawQuery != "" {
		t += "?" + rawQuery
	}
	return t
}
