// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"context"
	"net/http"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// The four filter.Message views (filter.Message documents the methods).
// Each is a conversion of the Request or leg handle it belongs to, so
// returning one allocates nothing.
var (
	_ filter.Message = (*requestMessage)(nil)
	_ filter.Message = (*responseMessage)(nil)
	_ filter.Message = (*legRequestMessage)(nil)
	_ filter.Message = (*legResponseMessage)(nil)
)

// requestMessage is the client request, the message of onRequestHeaders,
// onRequestBody and onRoute. Its body is available in onRequestBody and
// onRoute once PrepareBody gated it.
type requestMessage Request

// Header returns the client request's live header map.
func (m *requestMessage) Header() http.Header { return m.hdr }

// Body returns the gated request body; filter.ErrNotAvailable outside
// onRequestBody and onRoute or when the body streams.
func (m *requestMessage) Body(context.Context) ([]byte, error) {
	x := (*Request)(m)
	if !x.requestPhase() {
		return nil, filter.ErrNotAvailable
	}
	b, ok := x.gatedRequest()
	if !ok {
		return nil, filter.ErrNotAvailable
	}
	return b, nil
}

// SetBody replaces the gated request body (body.Request.Replace) for the
// later Policies and every leg; filter.ErrNotAvailable where Body is.
func (m *requestMessage) SetBody(_ context.Context, b []byte) error {
	x := (*Request)(m)
	if _, ok := x.gatedRequest(); !ok || !x.requestPhase() {
		return filter.ErrNotAvailable
	}
	if err := x.reqBody.Replace(b); err != nil {
		return err
	}
	setBodyFields(x.hdr, len(b))
	x.invalidateRequestView()
	return nil
}

// Decoded returns the shared decoded request body, the value CEL reads as
// request.body; filter.ErrNotAvailable where Body is.
func (m *requestMessage) Decoded(context.Context) (expr.Value, error) {
	x := (*Request)(m)
	if _, ok := x.gatedRequest(); !ok || !x.requestPhase() {
		return nil, filter.ErrNotAvailable
	}
	d, ok := x.requestView()
	if !ok {
		return nil, filter.ErrNotAvailable
	}
	return d.val, d.err
}

// RawQuery returns the client request's raw query.
func (m *requestMessage) RawQuery() string { return m.req.RawQuery() }

// SetRawQuery replaces the client request's raw query, which CEL
// request.query and the Forwarder read.
func (m *requestMessage) SetRawQuery(q string) { m.req.SetRawQuery(q) }

// Status returns 0: a request has none.
func (*requestMessage) Status() int { return 0 }

// Generated reports false.
func (*requestMessage) Generated() bool { return false }

// responseMessage is the client response, the message of onResponse (and
// read-only in onLog). Its body is available in onResponse: a generated
// response's literal body, or the Upstream body gated within
// limits.maxResponseBodyBytes when the Route gates it.
type responseMessage Request

// Header returns the client response's live header map; before a response
// is set, an empty map that later Phases keep.
func (m *responseMessage) Header() http.Header {
	x := (*Request)(m)
	if x.resp.header == nil {
		x.resp.header = x.nextHeader()
	}
	return x.resp.header
}

// Body returns the client response body in onResponse.
func (m *responseMessage) Body(ctx context.Context) ([]byte, error) {
	x := (*Request)(m)
	if x.ph != phase.OnResponse {
		return nil, filter.ErrNotAvailable
	}
	return x.responseBody(ctx)
}

// SetBody replaces the client response body in onResponse; every started
// tee stops, since it copied the old response.
func (m *responseMessage) SetBody(ctx context.Context, b []byte) error {
	x := (*Request)(m)
	if x.ph != phase.OnResponse {
		return filter.ErrNotAvailable
	}
	return x.setResponseBody(ctx, b)
}

// Decoded returns the decoded client response body in onResponse, the
// value CEL reads as response.body.
func (m *responseMessage) Decoded(ctx context.Context) (expr.Value, error) {
	x := (*Request)(m)
	if x.ph != phase.OnResponse {
		return nil, filter.ErrNotAvailable
	}
	d := x.responseView(ctx)
	if !x.resp.generated {
		x.respV.Body = d.cel()
	}
	return d.val, d.err
}

// RawQuery returns "": a response has none.
func (*responseMessage) RawQuery() string { return "" }

// SetRawQuery does nothing on a response.
func (*responseMessage) SetRawQuery(string) {}

// Status returns the client response status.
func (m *responseMessage) Status() int { return m.resp.status }

// Generated reports a Filter- or Node-generated response.
func (m *responseMessage) Generated() bool { return m.resp.generated }

// legRequestMessage is the attempt's outgoing request, the message of
// onUpstreamRequest. Its body is the client's gated input, the same each
// attempt, until a Filter sets this attempt's body. Like the leg's view,
// it is its handle: once the handle is released it is an empty request
// whose writes reach nothing.
type legRequestMessage legHandle

// state returns the leg state in onUpstreamRequest; nil in another Phase
// and once the handle is released.
func (m *legRequestMessage) state() *leg {
	if l := (*legHandle)(m).leg(); l != nil && l.ph == phase.OnUpstreamRequest {
		return l
	}
	return nil
}

// Header returns the outgoing request's header map.
func (m *legRequestMessage) Header() http.Header {
	if l := (*legHandle)(m).leg(); l != nil {
		return l.outHeader()
	}
	return http.Header{}
}

// Body returns this attempt's request body in onUpstreamRequest;
// filter.ErrNotAvailable when the client body streams and none was set.
func (m *legRequestMessage) Body(context.Context) ([]byte, error) {
	l := m.state()
	if l == nil {
		return nil, filter.ErrNotAvailable
	}
	b, ok := l.requestBody()
	if !ok {
		return nil, filter.ErrNotAvailable
	}
	return b, nil
}

// SetBody makes b this attempt's outgoing body in onUpstreamRequest; the
// client's input is unchanged, so the next attempt starts from it again.
func (m *legRequestMessage) SetBody(_ context.Context, b []byte) error {
	l := m.state()
	if l == nil {
		return filter.ErrNotAvailable
	}
	return l.setRequestBody(b)
}

// Decoded returns the decoded body of this attempt's request in
// onUpstreamRequest.
func (m *legRequestMessage) Decoded(context.Context) (expr.Value, error) {
	l := m.state()
	if l == nil {
		return nil, filter.ErrNotAvailable
	}
	d, ok := l.requestDecoded()
	if !ok {
		return nil, filter.ErrNotAvailable
	}
	return d.val, d.err
}

// RawQuery returns the outgoing request's raw query.
func (m *legRequestMessage) RawQuery() string {
	l := (*legHandle)(m).leg()
	if l == nil {
		return ""
	}
	if a := l.attempt; a != nil && a.Request != nil && a.Request.URL != nil {
		return a.Request.URL.RawQuery
	}
	return l.req.RawQuery()
}

// SetRawQuery replaces the outgoing request's raw query and CEL
// request.query on the leg.
func (m *legRequestMessage) SetRawQuery(q string) {
	l := (*legHandle)(m).leg()
	if l == nil {
		return
	}
	if a := l.attempt; a != nil && a.Request != nil && a.Request.URL != nil {
		a.Request.URL.RawQuery = q
	}
	l.req.SetRawQuery(q)
}

// Status returns 0: a request has none.
func (*legRequestMessage) Status() int { return 0 }

// Generated reports false.
func (*legRequestMessage) Generated() bool { return false }

// legResponseMessage is the attempt's response, the message of
// onUpstreamResponseHeaders and onUpstreamResponseBody. Its body is
// available in onUpstreamResponseBody, gated within the leg's
// MaxBodyBytes, else limits.maxResponseBodyBytes. An attempt without a
// response has an empty one: status 0, a header map that a new attempt
// clears, no body. Once the handle is released it is an empty response
// whose writes reach nothing.
type legResponseMessage legHandle

// state returns the leg state in onUpstreamResponseBody; nil in another
// Phase and once the handle is released.
func (m *legResponseMessage) state() *leg {
	if l := (*legHandle)(m).leg(); l != nil && l.ph == phase.OnUpstreamResponseBody {
		return l
	}
	return nil
}

// Header returns the response's live header map.
func (m *legResponseMessage) Header() http.Header {
	if l := (*legHandle)(m).leg(); l != nil {
		return l.inHeader()
	}
	return http.Header{}
}

// Body returns the gated response body in onUpstreamResponseBody.
func (m *legResponseMessage) Body(ctx context.Context) ([]byte, error) {
	l := m.state()
	if l == nil {
		return nil, filter.ErrNotAvailable
	}
	lb, err := l.gateResponse(ctx)
	if err != nil {
		return nil, err
	}
	return lb.bytes(), nil
}

// SetBody replaces the response body in onUpstreamResponseBody.
func (m *legResponseMessage) SetBody(ctx context.Context, b []byte) error {
	l := m.state()
	if l == nil {
		return filter.ErrNotAvailable
	}
	return l.setResponseBody(ctx, b)
}

// Decoded returns the decoded response body in onUpstreamResponseBody, the
// value CEL reads as response.body.
func (m *legResponseMessage) Decoded(ctx context.Context) (expr.Value, error) {
	l := m.state()
	if l == nil {
		return nil, filter.ErrNotAvailable
	}
	d := l.responseDecoded(ctx)
	l.respV.Body = d.cel()
	return d.val, d.err
}

// RawQuery returns "": a response has none.
func (*legResponseMessage) RawQuery() string { return "" }

// SetRawQuery does nothing on a response.
func (*legResponseMessage) SetRawQuery(string) {}

// Status returns the response status; 0 without a response.
func (m *legResponseMessage) Status() int {
	if l := (*legHandle)(m).leg(); l != nil {
		if a := l.attempt; a != nil && a.Response != nil {
			return a.Response.StatusCode
		}
	}
	return 0
}

// Generated reports false: a leg's response comes from its Upstream.
func (*legResponseMessage) Generated() bool { return false }
