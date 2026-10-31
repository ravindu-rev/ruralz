// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"net/http"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/cache/coalesce"
	"github.com/ravindu-rev/ruralz/internal/filter/cache/revalidate"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Forward reasons of the Cache-Status member's fwd parameter (RFC 9211
// section 2.2).
const (
	fwdNone uint8 = iota
	fwdURIMiss
	fwdVaryMiss
	fwdRequest
	fwdStale
	fwdBypass
)

// fwdName returns the fwd token of r.
func fwdName(r uint8) string {
	switch r {
	case fwdURIMiss:
		return "uri-miss"
	case fwdVaryMiss:
		return "vary-miss"
	case fwdRequest:
		return "request"
	case fwdStale:
		return "stale"
	case fwdBypass:
		return "bypass"
	default:
		return ""
	}
}

// published is a leader's storable response as its coalesced followers
// reuse it; it is shared and never modified.
type published struct {
	status     int
	header     http.Header
	body       []byte
	initialAge time.Duration
	received   time.Time
}

// state is one GET or HEAD request's PolicyState, pooled by the Factory:
// the lookup's keys and stored response, the outcome its Cache-Status
// member and cache result report, and the store under way. Finish returns
// it to the pool.
type state struct {
	// result is the emit.Cache* result, -1 before one is decided; the
	// Cache-Status member's parameters follow.
	result    int
	fwd       uint8
	fwdStatus int
	hit       bool
	ttl       int64
	hasTTL    bool
	stored    bool
	collapsed bool

	// head marks a HEAD request. served marks a response the cache serves
	// from a stored one; forward a request sent to the Upstream; bypass
	// one the cache stays out of; validating a forward carrying the stored
	// response's validators.
	head, served, forward, bypass, validating bool

	req          revalidate.Directives
	route        string
	key          statestore.CacheKey
	names        string
	variant      statestore.Digest
	token        statestore.Token
	hasToken     bool
	credentialed bool
	authorized   bool
	// reqHeader is the request header at lookup, before any later Policy
	// or the cache's validators changed it, for the stored variant; cond
	// holds the client's own preconditions and Range a validation removed.
	reqHeader, cond     http.Header
	requested, received time.Time

	// e is the stored response the lookup found, decoded into found;
	// fresh, age, lookedAt and storedAt describe it at the lookup.
	e        *entry
	found    entry
	fresh    revalidate.Freshness
	age      time.Duration
	lookedAt time.Time
	storedAt time.Time
	// reserved is the buffer budget a served response holds.
	reserved int64

	call  statestore.Call
	calls [1]*statestore.Call
	// buf is the lookup's entry buffer, kept across requests.
	buf []byte

	ticket coalesce.Ticket[*published]

	// The store under way: its tee, the entry without its body and the
	// encoded prefix, the stored names, variant and TTL, and whether its
	// generation step creates the key.
	tee          filter.Tee
	pending      entry
	prefix       []byte
	storeNames   string
	storeVariant statestore.Digest
	storeTTL     time.Duration
	create       bool

	// The refresh a request-path 304 queues: the entry, its TTL and names.
	refresh      []byte
	refreshTTL   time.Duration
	refreshNames string
}

// state returns a pooled state ready for one request.
func (f *Factory) state() *state {
	st := f.states.Get().(*state)
	st.result = -1
	return st
}

// release returns st to the pool, dropping every reference to request
// memory but keeping its buffers.
func (f *Factory) release(st *state) {
	buf, prefix := st.buf[:0], st.prefix[:0]
	*st = state{buf: buf, prefix: prefix}
	f.states.Put(st)
}

// stateOf returns x's cache state; nil when the Policy did not handle the
// request.
func stateOf(x filter.Exchange) *state {
	st, _ := (*x.PolicyState()).(*state)
	return st
}
