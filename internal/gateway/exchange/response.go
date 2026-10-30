// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// response is the client response onResponse acts on.
type response struct {
	set       bool
	status    int
	header    http.Header
	generated bool
	// cached is a response the Response Cache serves (filter.Response
	// Cached): no generated response, its literal body the stored one.
	cached bool
	// code and detail are a generated error response's RZ code and problem
	// detail; literal is a generated response's literal body.
	code, detail string
	literal      []byte
	// stream is the Upstream body; the handler closes it whatever happens.
	stream io.ReadCloser
	// gated is set when respBody holds the body: gated, or set by a
	// Filter; gateErr is a failed gate's error.
	gated   bool
	gateErr error
}

// Reply is the client response as onResponse left it, which the handler
// commits.
type Reply struct {
	// Status is the response status and Header its live header map.
	Status int
	Header http.Header
	// Generated is set for a Filter- or Node-generated response.
	Generated bool
	// Cached is set for a response the Response Cache served from a stored
	// response (filter.Response.Cached): neither generated nor passed
	// through from an Upstream.
	Cached bool
	// Code and Detail are a generated error response's RZ code and problem
	// detail: without a Body, the handler writes the problem document.
	Code, Detail string
	// Buffered is set when Body is the whole body: gated, set by a Filter,
	// or a generated response's literal body. Body is x's memory, valid
	// until Put.
	Buffered bool
	Body     []byte
	// Stream is the Upstream body, streamed when the body is not Buffered.
	// The handler closes it either way: closing releases the Upstream's
	// bulkhead slot.
	Stream io.ReadCloser
}

// SetResponse makes u the client response onResponse runs on: an Upstream
// response, or the response a leg Filter generated (u.Generated). The
// last leg's Upstream and Endpoint become CEL upstream. An Upstream
// response is the leg response Final.LegStatus reports; on a Route that
// tees its response (BodyNeeds.ResponseTee) its header is also copied
// before any onResponse Policy edits it, for the Response Cache store
// (filter.Exchange.LegResponse).
func (x *Request) SetResponse(u *snapshot.UpstreamResponse) {
	if u == nil {
		return
	}
	x.up = expr.Upstream{Name: u.Upstream, Endpoint: u.Endpoint}
	x.vars.Upstream = nil
	if u.Upstream != "" {
		x.vars.Upstream = &x.up
	}
	x.legStatus, x.legHdr, x.legKept, x.legLost, x.legLostBudget = 0, nil, false, false, false
	if u.Generated != nil {
		x.setGenerated(u.Generated)
		x.resp.stream = u.Body
		return
	}
	x.clearResponse()
	h := u.Header
	if h == nil {
		h = x.nextHeader()
	}
	x.legStatus = u.Status
	if x.route != nil && x.route.Body.ResponseTee {
		x.legHdr = h.Clone()
		if x.legHdr == nil {
			x.legHdr = http.Header{}
		}
	}
	x.resp = response{set: true, status: u.Status, header: h, stream: u.Body}
	x.respV = expr.Response{Status: u.Status, Header: h}
	x.vars.Response = &x.respV
}

// SetGenerated makes r, a Filter's short-circuit or a Node-generated
// response, the client response onResponse runs on; r.Header's fields are
// copied, so onResponse edits never reach r. A Cached r, which the
// Response Cache serves, is no generated response: its literal body is
// the body Policies read and rewrite, and CEL response.body decodes it.
func (x *Request) SetGenerated(r *filter.Response) {
	if r == nil {
		return
	}
	x.vars.Upstream = nil
	x.setGenerated(r)
}

// setGenerated replaces the client response with r, keeping the Upstream
// stream for the handler to close; any gate or Filter body is dropped.
func (x *Request) setGenerated(r *filter.Response) {
	h := x.nextHeader()
	for k, vs := range r.Header {
		// Clipped, so an append in onResponse never writes into r's arrays.
		h[k] = slices.Clip(vs)
	}
	stream := x.resp.stream
	x.clearResponse()
	x.resp = response{
		set: true, status: r.Status, header: h, generated: !r.Cached, cached: r.Cached,
		code: r.Code, detail: r.Detail, literal: r.Body, stream: stream,
	}
	if r.Cached {
		x.resp.code, x.resp.detail = "", ""
	}
	x.respV = expr.Response{Status: r.Status, Header: h}
	x.vars.Response = &x.respV
}

// nextHeader returns the generated header map not in use, cleared, and
// makes it the one in use.
func (x *Request) nextHeader() http.Header {
	x.genIdx ^= 1
	h := x.genHdr[x.genIdx]
	if h == nil {
		h = make(http.Header)
		x.genHdr[x.genIdx] = h
	}
	clear(h)
	return h
}

// clearResponse drops the response body state: the gate or Filter body and
// its reservation, the decoded view and every started tee.
func (x *Request) clearResponse() {
	x.respBody.Reset()
	x.resp.gated, x.resp.gateErr = false, nil
	x.invalidateResponseView()
	x.dropTees()
}

// GateResponse reads the Upstream response whole within
// limits.maxResponseBodyBytes, for a Route that gates it
// (BodyNeeds.ResponseGate), before onResponse. A body over the limit is
// filter.ErrTooLarge (502 RZ-UP-010), a refused reservation
// filter.ErrBudget (503 RZ-RT-004); body.Code maps them with
// body.SideResponse. A generated response, or one already gated, needs
// nothing.
func (x *Request) GateResponse(ctx context.Context) error {
	r := &x.resp
	switch {
	case !r.set || r.generated || r.cached || r.gated:
		return nil
	case r.gateErr != nil:
		return r.gateErr
	}
	err := x.respBody.Prepare(ctx, r.stream, declaredLength(r.header), true, x.limits.MaxResponseBodyBytes, x.pool.budget, x.stripe)
	if err != nil {
		r.gateErr = err
		return err
	}
	r.gated = true
	return nil
}

// responseBody returns the whole client response body for onResponse: the
// gated or replaced body, a generated response's literal body, or the
// Upstream body gated now when the Route gates it; filter.ErrNotAvailable
// otherwise.
func (x *Request) responseBody(ctx context.Context) ([]byte, error) {
	r := &x.resp
	switch {
	case !r.set:
		return nil, filter.ErrNotAvailable
	case r.gated:
		b, _ := x.respBody.Gated()
		return b, nil
	case r.generated, r.cached:
		return r.literal, nil
	case x.route == nil || !x.route.Body.ResponseGate:
		return nil, filter.ErrNotAvailable
	}
	if err := x.GateResponse(ctx); err != nil {
		return nil, err
	}
	b, _ := x.respBody.Gated()
	return b, nil
}

// setResponseBody replaces the client response body with b in onResponse
// (filter.Message.SetBody). A body that was not gated is replaced whole:
// the response body is first bound, empty, to the budget b is reserved
// from (an empty body reads nothing, so ctx is never used). The Response
// Cache stores the body the leg delivered, so on a Route that tees, the
// first replacement of a gated leg body keeps a copy of it, and a streamed
// one replaced whole is lost to the store; a tee still copying the old
// stream stops, while one already filled with the leg body stays.
func (x *Request) setResponseBody(ctx context.Context, b []byte) error {
	if !x.resp.set {
		return filter.ErrNotAvailable
	}
	x.keepLegBody()
	if !x.resp.gated {
		_ = x.respBody.Prepare(ctx, nil, 0, false, 0, x.pool.budget, x.stripe)
	}
	if err := x.respBody.Replace(b); err != nil {
		return err
	}
	x.resp.gated, x.resp.gateErr, x.resp.literal = true, nil, nil
	setBodyFields(x.resp.header, len(b))
	x.invalidateResponseView()
	x.dropCopyingTees()
	return nil
}

// keepLegBody copies the leg's body before an onResponse Policy first
// replaces it, on a Route that tees an Upstream response: a gated body is
// copied, charged to the request's account (legLostBudget when refused),
// and a streamed one is lost, as the handler will never write it.
func (x *Request) keepLegBody() {
	r := &x.resp
	if x.legKept || x.legLost || r.generated || r.cached || x.legStatus == 0 || x.route == nil || !x.route.Body.ResponseTee {
		return
	}
	if !r.gated {
		x.legLost = true
		return
	}
	b, _ := x.respBody.Gated()
	if err := x.acct.Reserve(int64(len(b))); err != nil {
		x.legLost, x.legLostBudget = true, true
		return
	}
	x.legBody = append(x.legBody[:0], b...)
	x.legKept = true
}

// dropCopyingTees stops the tees still copying the response stream, whose
// body was replaced; a tee already filled with the leg's body is kept.
func (x *Request) dropCopyingTees() {
	for _, t := range x.tees {
		if _, complete := t.Result(); !complete {
			t.Release()
		}
	}
}

// ensureResponseView builds the client response body's decoded view and
// puts it in response.body, for an Upstream response the Route gates (or
// one already gated or replaced); a generated response is never decoded
// for CEL, since the Policies reading response.body do not run on it.
func (x *Request) ensureResponseView(ctx context.Context) {
	r := &x.resp
	if x.respDec.done || !r.set || r.generated {
		return
	}
	if !r.gated && (x.route == nil || !x.route.Body.ResponseGate) {
		return
	}
	x.respV.Body = x.responseView(ctx).cel()
}

// responseView returns the decoded view of the client response body,
// building it once per body version; a body not available yet is not
// remembered, so a later gate still builds the view.
func (x *Request) responseView(ctx context.Context) *decoded {
	if x.respDec.done {
		return &x.respDec
	}
	b, err := x.responseBody(ctx)
	if err != nil {
		x.respDec = decoded{done: !errors.Is(err, filter.ErrNotAvailable), err: err}
		return &x.respDec
	}
	x.respDec = x.pool.decode(&x.acct, x.resp.header, b, x.limits.MaxResponseBodyBytes)
	return &x.respDec
}

// invalidateResponseView drops the response body's decoded view and its
// charge after the body changed.
func (x *Request) invalidateResponseView() {
	x.respDec.drop(&x.acct)
	x.respV.Body = nil
}

// replaceResponse replaces the uncommitted client response with r in
// onResponse (filter.Exchange.ReplaceResponse).
func (x *Request) replaceResponse(r *filter.Response) {
	if r == nil || x.ph != phase.OnResponse {
		return
	}
	x.setGenerated(r)
}

// Reply returns the client response to commit after onResponse.
func (x *Request) Reply() Reply {
	r := &x.resp
	out := Reply{
		Status: r.status, Header: r.header, Generated: r.generated, Cached: r.cached,
		Code: r.code, Detail: r.detail, Stream: r.stream,
	}
	switch {
	case r.gated:
		out.Body, _ = x.respBody.Gated()
		out.Buffered = true
	case r.literal != nil:
		out.Body, out.Buffered = r.literal, true
	}
	return out
}

// teeResponse starts the Response Cache store copy of the leg response's
// body for the calling Policy in onResponse (filter.Exchange.TeeResponse):
// the body as the leg delivered it, whatever an earlier onResponse Policy
// wrote to the client. A gated leg body, or the copy keepLegBody kept of
// one a Policy replaced, fills the tee at once; a streamed one is copied
// as the handler writes it (Tees). A body whose known length is over
// limit, or a negative limit, gets no tee and counts
// ruralz_cache_store_skipped_total{reason="size_limit"} on the Policy's
// handles, and one lost to a refused reservation counts buffer_budget;
// a generated or Cached response, a leg body lost to a replacement, and a
// Node without a budget get no tee.
func (x *Request) teeResponse(limit int64) filter.Tee {
	r := &x.resp
	if x.ph != phase.OnResponse || !r.set || r.generated || r.cached || x.pool.budget == nil {
		return nil
	}
	var skippedBy *[emit.NumSkipReasons]emit.Counter
	if p := x.cur; p != nil && p.Metrics != nil {
		skippedBy = &p.Metrics.StoreSkipped
	}
	skip := func(reason int) filter.Tee {
		if skippedBy != nil && skippedBy[reason] != nil {
			skippedBy[reason].Add(x.stripe, 1)
		}
		return nil
	}
	if x.legLost {
		if x.legLostBudget {
			return skip(emit.SkipBufferBudget)
		}
		return nil
	}
	body, whole := x.legBody, x.legKept
	if !whole && r.gated {
		body, whole = x.gatedLegBody(), true
	}
	n := int64(len(body))
	if !whole {
		n = declaredLength(r.header)
		if x.legHdr != nil {
			n = declaredLength(x.legHdr)
		}
	}
	if limit < 0 || n > limit {
		return skip(emit.SkipSizeLimit)
	}
	t := x.pool.budget.NewTee(limit, skippedBy, x.stripe)
	x.tees = append(x.tees, t)
	if whole {
		_, _ = t.Write(body)
		t.Complete()
	}
	return t
}

// gatedLegBody returns the gated client response body, which is the
// leg's own while no Policy replaced it.
func (x *Request) gatedLegBody() []byte {
	b, _ := x.respBody.Gated()
	return b
}

// dropTees stops every started tee: the response it copies was replaced,
// so its copy is never stored (Result reports none).
func (x *Request) dropTees() {
	for _, t := range x.tees {
		t.Release()
	}
}

// Tees returns the Response Cache tees Filters started in onResponse, for
// the handler to feed with the body it writes (body.Tee.Write or Reader,
// then Complete); a tee whose response was replaced copies nothing. The
// slice is x's memory, valid until Put, which releases the tees after
// Finish read them.
func (x *Request) Tees() []*body.Tee { return x.tees }

// addRateLimitField records f for the calling Policy, keeping the fields
// in request order of their Policies (filter.Exchange.AddRateLimitField):
// it goes after every field of a Policy that is not later than the
// calling one, so a batched member that completes after a later member
// decided in Prepare still lists first, and a Policy's own fields keep
// their order.
func (x *Request) addRateLimitField(f filter.RateLimitField) {
	i := len(x.rateLimits)
	for i > 0 && x.cur != nil && x.rateLimitBy[i-1] != nil && before(x.cur, x.rateLimitBy[i-1]) {
		i--
	}
	x.rateLimits = slices.Insert(x.rateLimits, i, f)
	x.rateLimitBy = slices.Insert(x.rateLimitBy, i, x.cur)
}

// RateLimitFields returns the applied limits Filters recorded, for the
// RateLimit-Policy and RateLimit fields the handler appends after
// onResponse. The slice is x's memory, valid until Put.
func (x *Request) RateLimitFields() []filter.RateLimitField { return x.rateLimits }
