// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"sync"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Decoder decodes one JSON body for CEL and Filters; expr.Compiler
// implements it. budget is the decoded-size cap: past it the decoder fails
// with expr.ErrTooLarge. built is the size of the values it built.
type Decoder interface {
	DecodeJSON(data []byte, budget int64) (v expr.Value, built int64, err error)
}

// Deps are the Node-wide dependencies of every Request.
type Deps struct {
	// Budget is the Node buffer budget, limits.maxBufferedBytes, carried
	// across Hot Reloads. A nil Budget refuses every reservation, so gates,
	// rewrites and decodes of a non-empty body fail with filter.ErrBudget.
	Budget *body.Budget
	// Decoder decodes JSON bodies; nil treats every body as not JSON.
	Decoder Decoder
}

// Pool hands out Requests and their leg states. It is safe for concurrent
// use; a Request belongs to one request goroutine between Get and Put.
type Pool struct {
	budget *body.Budget
	dec    Decoder
	// noop is the RouteMetrics of a Request without a Route's handles: no-op
	// handles, shared because they record nothing.
	noop emit.RouteMetrics

	reqs sync.Pool // *Request
	legs sync.Pool // *leg
	// chunks hold the leg handles not issued yet (*handleChunks); a chunk
	// whose handles are all issued is dropped, never reused.
	chunks sync.Pool
}

// NewPool returns a Pool over d.
func NewPool(d Deps) *Pool {
	p := &Pool{budget: d.Budget, dec: d.Decoder}
	p.noop = emit.RouteMetrics{Requests: noopStatus{}, Duration: noopHistogram{}}
	for i := range p.noop.Cache {
		p.noop.Cache[i] = noopCounter{}
	}
	p.reqs.New = func() any { return &Request{pool: p} }
	p.legs.New = func() any { return &leg{pool: p, attemptN: noAttempt} }
	return p
}

// Get returns a reset Request; Begin starts it.
func (p *Pool) Get() *Request {
	x, _ := p.reqs.Get().(*Request)
	if x == nil {
		x = &Request{pool: p}
	}
	x.live.Store(true)
	return x
}

// Put returns x to the pool after its last use: the response committed and
// the access record submitted (FillAccess shares x's memory with the
// record). It ends every leg still live, releases x's buffers and
// reservations and drops every reference x holds. A second Put does
// nothing; a leg handle used after Put, Release included, does nothing.
func (p *Pool) Put(x *Request) {
	if x == nil || x.pool != p || !x.live.CompareAndSwap(true, false) {
		return
	}
	x.endLegs()
	x.reset()
	p.reqs.Put(x)
}

// noopCounter, noopStatus and noopHistogram are handles that record
// nothing.
type (
	noopCounter   struct{}
	noopStatus    struct{}
	noopHistogram struct{}
)

// Add records nothing.
func (noopCounter) Add(emit.Stripe, uint64) {}

// Inc records nothing.
func (noopStatus) Inc(emit.Stripe, int) {}

// Record records nothing.
func (noopHistogram) Record(emit.Stripe, uint64) {}

// RecordExemplar records nothing.
func (noopHistogram) RecordExemplar(emit.Stripe, uint64, emit.Exemplar) {}
