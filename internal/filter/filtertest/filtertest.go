// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package filtertest provides an in-memory filter.Exchange,
// filter.Message, statestore.Store and filter.Replayer for Filter and
// executor unit tests, so Policy packages and the executor are tested
// without the data plane or a State Store driver.
package filtertest

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Message is an in-memory filter.Message. Set Unavailable to model a
// Phase that is not a body gate.
type Message struct {
	Hdr http.Header
	// Raw is the body; Unavailable makes Body return ErrNotAvailable.
	Raw         []byte
	Unavailable bool
	// DecodedValue is returned by Decoded.
	DecodedValue expr.Value
	Query        string
	// PathV is the forwarded path of a request message (Path, SetPath).
	PathV string
	Code  int
	Gen   bool
	// Budget, when non-nil, is charged by SetBody.
	Budget *Budget
}

var _ filter.Message = (*Message)(nil)

// Header implements filter.Message.
func (m *Message) Header() http.Header {
	if m.Hdr == nil {
		m.Hdr = http.Header{}
	}
	return m.Hdr
}

// Body implements filter.Message.
func (m *Message) Body(context.Context) ([]byte, error) {
	if m.Unavailable {
		return nil, filter.ErrNotAvailable
	}
	return m.Raw, nil
}

// SetBody implements filter.Message.
func (m *Message) SetBody(_ context.Context, b []byte) error {
	if m.Budget != nil {
		if err := m.Budget.Reserve(int64(len(b))); err != nil {
			return err
		}
	}
	m.Raw = b
	m.DecodedValue = nil
	h := m.Header()
	h.Set("Content-Length", strconv.Itoa(len(b)))
	h.Del("Content-Encoding")
	h.Del("Content-Digest")
	h.Del("Repr-Digest")
	h.Del("Digest")
	h.Del("Content-Md5")
	return nil
}

// Decoded implements filter.Message.
func (m *Message) Decoded(context.Context) (expr.Value, error) { return m.DecodedValue, nil }

// RawQuery implements filter.Message.
func (m *Message) RawQuery() string { return m.Query }

// SetRawQuery implements filter.Message.
func (m *Message) SetRawQuery(q string) { m.Query = q }

// Path implements filter.Message: PathV.
func (m *Message) Path() string { return m.PathV }

// SetPath implements filter.Message: it sets PathV.
func (m *Message) SetPath(p string) { m.PathV = p }

// Status implements filter.Message.
func (m *Message) Status() int { return m.Code }

// Generated implements filter.Message.
func (m *Message) Generated() bool { return m.Gen }

// Budget models limits.maxBufferedBytes.
type Budget struct {
	mu        sync.Mutex
	Limit     int64
	Reserved  int64
	Exhausted bool
}

// Reserve charges n bytes; ErrBudget past Limit (Limit 0 is unlimited).
func (b *Budget) Reserve(n int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Exhausted || (b.Limit > 0 && b.Reserved+n > b.Limit) {
		return filter.ErrBudget
	}
	b.Reserved += n
	return nil
}

// Release returns n bytes.
func (b *Budget) Release(n int64) {
	b.mu.Lock()
	b.Reserved -= n
	b.mu.Unlock()
}

// Tee is a completed filter.Tee.
type Tee struct {
	Body     []byte
	Complete bool
	// Stop is what Stopped returns: nil never stops; a test closes its
	// own channel to stop the copy.
	Stop chan struct{}
}

// Result implements filter.Tee.
func (t *Tee) Result() ([]byte, bool) { return t.Body, t.Complete }

// Stopped implements filter.Tee.
func (t *Tee) Stopped() <-chan struct{} { return t.Stop }

// Exchange is an in-memory filter.Exchange. Zero values are usable;
// recorded effects (identity, rate-limit fields, cache result, annotations,
// replaced response) are readable after Handle returns.
type Exchange struct {
	ID            string
	Start         time.Time
	ListenerV     string
	RouteV        *expr.Route
	MethodV       string
	SchemeV       string
	HostV         string
	PathV         string
	ReceivedV     filter.Received
	Hdr           http.Header
	SourceV       filter.Source
	TLSV          *filter.ConnTLS
	Msg           filter.Message
	LegV          *filter.Leg
	VarsV         *expr.Vars
	Ident         *filter.Identity
	Budget        Budget
	StateDeadline *statestore.RequestBudget
	TeeV          *Tee
	// TeeLimit records the limit of the last TeeResponse call.
	TeeLimit int64
	// LegStatusV and LegHeaderV are what LegResponse returns.
	LegStatusV int
	LegHeaderV http.Header
	FinalV     filter.Final
	// State is the PolicyState slot; StripeV and RouteMetricsV are returned
	// as is (RouteMetrics falls back to an empty value).
	State         any
	StripeV       emit.Stripe
	RouteMetricsV *emit.RouteMetrics
	// PolicyName names the calling Policy in the Charges it makes.
	PolicyName string

	// Effects recorded by the Filter under test.
	Replaced    *filter.Response
	RateLimits  []filter.RateLimitField
	CacheResult string
	Annotations map[string]slog.Value
	// Charged is the request's charged-counter record (Charge), made on
	// first use; Exchanges standing for several Policies of one request
	// share one.
	Charged *Charges
}

// Charges is a request's charged-counter record (filter.Exchange.Charge).
type Charges struct {
	// List holds the request's Charges in charge order.
	List []*filter.Charge
}

var _ filter.Exchange = (*Exchange)(nil)

// RequestID implements filter.Exchange.
func (x *Exchange) RequestID() string { return x.ID }

// Now implements filter.Exchange.
func (x *Exchange) Now() time.Time { return x.Start }

// Listener implements filter.Exchange.
func (x *Exchange) Listener() string { return x.ListenerV }

// Route implements filter.Exchange.
func (x *Exchange) Route() *expr.Route { return x.RouteV }

// Method implements filter.Exchange.
func (x *Exchange) Method() string { return x.MethodV }

// Scheme implements filter.Exchange.
func (x *Exchange) Scheme() string { return x.SchemeV }

// Host implements filter.Exchange.
func (x *Exchange) Host() string { return x.HostV }

// Path implements filter.Exchange.
func (x *Exchange) Path() string { return x.PathV }

// Received implements filter.Exchange.
func (x *Exchange) Received() filter.Received { return x.ReceivedV }

// Header implements filter.Exchange.
func (x *Exchange) Header() http.Header {
	if x.Hdr == nil {
		x.Hdr = http.Header{}
	}
	return x.Hdr
}

// Source implements filter.Exchange.
func (x *Exchange) Source() filter.Source { return x.SourceV }

// TLS implements filter.Exchange.
func (x *Exchange) TLS() *filter.ConnTLS { return x.TLSV }

// Message implements filter.Exchange.
func (x *Exchange) Message() filter.Message { return x.Msg }

// Leg implements filter.Exchange.
func (x *Exchange) Leg() *filter.Leg { return x.LegV }

// Vars implements filter.Exchange.
func (x *Exchange) Vars() *expr.Vars {
	if x.VarsV == nil {
		x.VarsV = &expr.Vars{}
	}
	return x.VarsV
}

// Identity implements filter.Exchange.
func (x *Exchange) Identity() *filter.Identity { return x.Ident }

// SetIdentity implements filter.Exchange.
func (x *Exchange) SetIdentity(id *filter.Identity) error {
	if x.Ident != nil {
		return filter.ErrSecondBinding
	}
	x.Ident = id
	return nil
}

// StateBudget implements filter.Exchange.
func (x *Exchange) StateBudget() *statestore.RequestBudget { return x.StateDeadline }

// Reserve implements filter.Exchange.
func (x *Exchange) Reserve(n int64) error { return x.Budget.Reserve(n) }

// Release implements filter.Exchange.
func (x *Exchange) Release(n int64) { x.Budget.Release(n) }

// LegResponse implements filter.Exchange.
func (x *Exchange) LegResponse() (int, http.Header) { return x.LegStatusV, x.LegHeaderV }

// TeeResponse implements filter.Exchange; it records limit in TeeLimit and
// returns TeeV as is.
func (x *Exchange) TeeResponse(limit int64) filter.Tee {
	x.TeeLimit = limit
	if x.TeeV == nil {
		return nil
	}
	return x.TeeV
}

// ReplaceResponse implements filter.Exchange.
func (x *Exchange) ReplaceResponse(r *filter.Response) { x.Replaced = r }

// AddRateLimitField implements filter.Exchange.
func (x *Exchange) AddRateLimitField(f filter.RateLimitField) {
	x.RateLimits = append(x.RateLimits, f)
}

// SetCacheResult implements filter.Exchange.
func (x *Exchange) SetCacheResult(result string) { x.CacheResult = result }

// Charge implements filter.Exchange: the record of c in Charged, made for
// PolicyName when the calling Policy is the first to charge c.
func (x *Exchange) Charge(c filter.Counter) (*filter.Charge, bool) {
	if x.Charged == nil {
		x.Charged = &Charges{}
	}
	for _, ch := range x.Charged.List {
		if ch.Counter == c {
			return ch, false
		}
	}
	ch := &filter.Charge{Counter: c, Policy: x.PolicyName}
	x.Charged.List = append(x.Charged.List, ch)
	return ch, true
}

// Annotate implements filter.Exchange.
func (x *Exchange) Annotate(key string, v slog.Value) {
	if x.Annotations == nil {
		x.Annotations = map[string]slog.Value{}
	}
	x.Annotations[key] = v
}

// Final implements filter.Exchange.
func (x *Exchange) Final() filter.Final { return x.FinalV }

// PolicyState implements filter.Exchange; one slot for the Filter under test.
func (x *Exchange) PolicyState() *any { return &x.State }

// Stripe implements filter.Exchange.
func (x *Exchange) Stripe() emit.Stripe { return x.StripeV }

// RouteMetrics implements filter.Exchange.
func (x *Exchange) RouteMetrics() *emit.RouteMetrics {
	if x.RouteMetricsV == nil {
		x.RouteMetricsV = &emit.RouteMetrics{}
	}
	return x.RouteMetricsV
}

// Store is a scriptable statestore.Store that counts round trips. With a
// nil ConsumeFn every call is answered: Done, no error, Allowed. It is
// safe for concurrent use.
type Store struct {
	mu sync.Mutex
	// ConsumeFn answers one Consume round trip and returns how many calls
	// it took (at least 1).
	ConsumeFn func(calls []*statestore.Call) int
	// ReadFn answers one Read batch.
	ReadFn func(calls []*statestore.Call)
	// Trips counts Consume and Read round trips; Writes the Write batches.
	Trips, Writes int
	// Written records every post-commit write.
	Written []*statestore.Write
	// Caps are the supported capabilities (default CapScripts).
	Caps statestore.Capability
}

var _ statestore.Store = (*Store)(nil)

// Consume implements statestore.Store.
func (s *Store) Consume(_ context.Context, _ *statestore.RequestBudget, calls []*statestore.Call) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Trips++
	if s.ConsumeFn != nil {
		return max(s.ConsumeFn(calls), 1)
	}
	for _, c := range calls {
		c.Done = true
		c.GCRA.Allowed, c.Quota.Allowed = true, true
		c.GCRA.Denied = -1
	}
	return len(calls)
}

// Read implements statestore.Store.
func (s *Store) Read(_ context.Context, _ *statestore.RequestBudget, calls []*statestore.Call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Trips++
	if s.ReadFn != nil {
		s.ReadFn(calls)
		return
	}
	for _, c := range calls {
		c.Done = true
	}
}

// Write implements statestore.Store.
func (s *Store) Write(_ context.Context, batch []*statestore.Write) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Writes++
	for _, w := range batch {
		w.Applied = true
		s.Written = append(s.Written, w)
	}
}

// AdmitStoreBytes implements statestore.Store; it always admits.
func (*Store) AdmitStoreBytes(statestore.CacheKey, int) bool { return true }

// Supports implements statestore.Store.
func (s *Store) Supports(c statestore.Capability) bool {
	caps := s.Caps
	if caps == 0 {
		caps = statestore.CapScripts
	}
	return caps&c == c
}

// RoundTrips implements statestore.Store; the fake counts as remote.
func (*Store) RoundTrips() bool { return true }

// ReplayFunc adapts a function to filter.Replayer.
type ReplayFunc func(ctx context.Context, route, upstream string, req *http.Request) (*http.Response, error)

// Replay implements filter.Replayer.
func (f ReplayFunc) Replay(ctx context.Context, route, upstream string, req *http.Request) (*http.Response, error) {
	return f(ctx, route, upstream, req)
}
