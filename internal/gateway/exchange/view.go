// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// view is a Request as the filter.Exchange of its client Phases. It is the
// same memory as the Request (a conversion, no allocation).
type view Request

var _ filter.Exchange = (*view)(nil)

// RequestID implements filter.Exchange: the 32-hex trace ID.
func (v *view) RequestID() string { return (*Request)(v).requestID() }

// Now implements filter.Exchange: the request start.
func (v *view) Now() time.Time { return v.start }

// Listener implements filter.Exchange.
func (v *view) Listener() string { return v.listener }

// Route implements filter.Exchange: the matched Route's view, nil when
// unmatched.
func (v *view) Route() *expr.Route { return v.vars.Route }

// Method implements filter.Exchange: the method as received.
func (v *view) Method() string { return v.req.Method }

// Scheme implements filter.Exchange.
func (v *view) Scheme() string { return v.req.Scheme }

// Host implements filter.Exchange: the host lower-cased without port, as
// the Router matched it.
func (v *view) Host() string { return v.req.Host }

// Path implements filter.Exchange: the one normalized path.
func (v *view) Path() string { return v.path }

// Received implements filter.Exchange: the client request as it arrived.
func (v *view) Received() filter.Received { return (*Request)(v).received() }

// LegResponse implements filter.Exchange: the leg response kept for the
// Response Cache store.
func (v *view) LegResponse() (int, http.Header) { return v.legStatus, v.legHdr }

// Header implements filter.Exchange: the client request's live header map.
func (v *view) Header() http.Header { return v.hdr }

// Source implements filter.Exchange.
func (v *view) Source() filter.Source { return v.src }

// TLS implements filter.Exchange.
func (v *view) TLS() *filter.ConnTLS { return v.tls }

// Message implements filter.Exchange: the client request in request
// Phases, the client response in onResponse and later.
func (v *view) Message() filter.Message {
	switch v.ph {
	case phase.OnResponse, phase.OnLog, phase.OnChunk:
		return (*responseMessage)(v)
	default:
		return (*requestMessage)(v)
	}
}

// Leg implements filter.Exchange: nil, the client view has no leg.
func (*view) Leg() *filter.Leg { return nil }

// Vars implements filter.Exchange: the CEL activation of the current
// Phase.
func (v *view) Vars() *expr.Vars { return (*Request)(v).activation() }

// Identity implements filter.Exchange.
func (v *view) Identity() *filter.Identity { return v.ident }

// SetIdentity implements filter.Exchange; a second successful
// authentication is filter.ErrSecondBinding.
func (v *view) SetIdentity(id *filter.Identity) error { return (*Request)(v).setIdentity(id) }

// StateBudget implements filter.Exchange.
func (v *view) StateBudget() *statestore.RequestBudget { return &v.budget }

// Reserve implements filter.Exchange: n bytes of decoded values charged to
// limits.maxBufferedBytes, filter.ErrBudget when refused.
func (v *view) Reserve(n int64) error { return v.acct.Reserve(n) }

// Release implements filter.Exchange.
func (v *view) Release(n int64) { v.acct.Release(n) }

// TeeResponse implements filter.Exchange (onResponse).
func (v *view) TeeResponse(limit int64) filter.Tee { return (*Request)(v).teeResponse(limit) }

// ReplaceResponse implements filter.Exchange (onResponse, before commit).
func (v *view) ReplaceResponse(r *filter.Response) { (*Request)(v).replaceResponse(r) }

// AddRateLimitField implements filter.Exchange.
func (v *view) AddRateLimitField(f filter.RateLimitField) { (*Request)(v).addRateLimitField(f) }

// SetCacheResult implements filter.Exchange: the access record's cache
// field (FillAccess).
func (v *view) SetCacheResult(result string) { v.cache = result }

// Annotate implements filter.Exchange: an attribute of the current
// Filter's span; nothing when the request is not sampled.
func (v *view) Annotate(key string, val slog.Value) {
	if v.span != nil {
		v.span.SetAttr(key, val)
	}
}

// Final implements filter.Exchange: the outcome SetFinal recorded, with
// RejectedAfter for the calling Policy.
func (v *view) Final() filter.Final {
	f := v.final
	f.LegStatus = v.legStatus
	f.RejectedAfter = (*Request)(v).rejectedAfter(v.cur)
	return f
}

// PolicyState implements filter.Exchange: the calling Policy's slot.
func (v *view) PolicyState() *any { return v.slots.at(v.cur) }

// Stripe implements filter.Exchange.
func (v *view) Stripe() emit.Stripe { return v.stripe }

// RouteMetrics implements filter.Exchange; never nil.
func (v *view) RouteMetrics() *emit.RouteMetrics { return (*Request)(v).routeMetrics() }
