// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package filter is the Filter SPI: what a built-in Filter implements
// (Filter, Factory), what it sees of one request (Exchange) and what it
// returns (Result). Filters under internal/filter/... never import
// internal/gateway/...; the data plane implements Exchange and runs the
// executor (internal/gateway/executor), so the offline validators of all
// three binaries can reuse Filter check code.
package filter

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"reflect"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Outcome is what a Filter decided.
type Outcome uint8

// Outcomes.
const (
	// Continue passes the request or response on.
	Continue Outcome = iota
	// Respond short-circuits with Result.Response (request Phases only).
	Respond
	// CannotDecide applies the Policy's failureMode.
	CannotDecide
	// Retry asks the Upstream layer to retry the current attempt
	// (onUpstreamResponseHeaders only; 05 req 33 "or a Filter request"):
	// every other retry condition still applies and retryOn is not
	// consulted; the remaining Filters of the Phase are skipped for this
	// attempt. In any other Phase the executor treats it as CannotDecide.
	Retry
)

// Response is a Filter- or Node-generated response. With Body nil and Code
// set, the request handler writes the RFC 9457 problem document for Code
// (internal/problem); the executor only passes the Response on.
type Response struct {
	// Status is the HTTP status.
	Status int
	// Header holds extra headers (WWW-Authenticate, Retry-After, CORS).
	Header http.Header
	// Body is a literal body (a cache hit, a 204 preflight); nil for a
	// problem document.
	Body []byte
	// Code is the RZ code of an error response; "" for a plain response.
	Code string
	// Detail is the optional problem detail (RZ-RT-009 keyword location).
	Detail string
}

// Result is a Filter's answer for one Phase.
type Result struct {
	Outcome Outcome
	// Response is set with Respond.
	Response *Response
	// Code is the RZ code to use under closed with CannotDecide; "" means
	// the class default (docs/architecture/03-data-plane.md "Failure
	// semantics").
	Code string
	// Err is the cause of CannotDecide: logged and put on the span as
	// error.type, never sent to the client.
	Err error
}

// Next returns Continue.
func Next() Result { return Result{} }

// Reply returns Respond with r.
func Reply(r *Response) Result { return Result{Outcome: Respond, Response: r} }

// Deny returns Respond with a problem document for code at status.
func Deny(status int, code string, h http.Header) Result {
	return Result{Outcome: Respond, Response: &Response{Status: status, Code: code, Header: h}}
}

// Undecided returns CannotDecide with an optional code and cause.
func Undecided(code string, err error) Result {
	return Result{Outcome: CannotDecide, Code: code, Err: err}
}

// RetryAttempt returns Retry (onUpstreamResponseHeaders only).
func RetryAttempt() Result { return Result{Outcome: Retry} }

// Sentinel errors of the SPI.
var (
	// ErrBudget: a limits.maxBufferedBytes reservation failed. 503
	// RZ-RT-004 under either failureMode, in any Phase before commit.
	ErrBudget = errors.New("filter: buffer budget spent")
	// ErrTooLarge: a body or decoded value is over its limit: 413
	// RZ-RT-003 (request), 502 RZ-UP-010 (plain upstreams response) or a
	// step failure RZ-RT-015.
	ErrTooLarge = errors.New("filter: body over limit")
	// ErrSecondBinding: SetIdentity after a successful authentication
	// (401 RZ-AUTH-002, Security and identity rule 2).
	ErrSecondBinding = errors.New("filter: second authentication")
	// ErrNotAvailable: the message or body is not available in this Phase.
	ErrNotAvailable = errors.New("filter: not available in this Phase")
)

// Identity methods (auth.method).
const (
	MethodJWT    = "jwt"
	MethodAPIKey = "api-key"
	MethodBasic  = "basic"
	MethodMTLS   = "mtls"
)

// Identity is set once per request by the first successful auth-class
// Policy; it feeds the CEL consumer and auth variables, quota keys, cache
// partitions and the access log.
type Identity struct {
	// Method is one of the Method constants.
	Method string
	// Policy is the authenticating Policy's name.
	Policy string
	// Claims is the verified JWT payload; an empty map otherwise.
	Claims expr.Value
	// Consumer is the bound Consumer; nil when unbound.
	Consumer *expr.Consumer
	// CertSubject is the RFC 4514 leaf subject (auth.mtls only).
	CertSubject string
	// Principal is the non-secret cache partition key: the Consumer name,
	// else "jwt:<iss>#<sub>", else "mtls:<subject>".
	Principal string
}

// Source is the client address after trusted-proxy and PROXY v2 handling.
type Source struct {
	// IP is source.ip, IPv4-mapped unmapped.
	IP netip.Addr
	// Port is the peer port; 0 when taken from a forwarding header.
	Port uint16
	// Peer is the TCP (or PROXY v2) peer.
	Peer netip.AddrPort
	// FromHeader is true when IP came from Forwarded or X-Forwarded-For.
	FromHeader bool
}

// ConnTLS is the TLS state of the client connection.
type ConnTLS struct {
	// State is the handshake result.
	State *tls.ConnectionState
	// ClientCertRequested is true when the handshake requested a client
	// certificate (a listener serving an auth.mtls Route).
	ClientCertRequested bool
	// Cache memoizes per-connection verification results (auth.mtls).
	Cache *ConnCache
}

// PeerCertificates returns the presented client chain.
func (c *ConnTLS) PeerCertificates() []*x509.Certificate {
	if c == nil || c.State == nil {
		return nil
	}
	return c.State.PeerCertificates
}

// ConnCache is a small per-connection memo (at most 8 entries), safe for
// concurrent HTTP/2 streams. Keys are non-nil comparable values, such as a
// string or a [32]byte digest, compared with ==. A nil key, or one that
// cannot be compared (a slice, a map, a func, or a struct, array or
// interface holding one), is never stored and never found, so the caller
// recomputes instead of panicking.
type ConnCache struct {
	mu   sync.Mutex
	keys [8]any
	vals [8]any
	next int
}

// Get returns the value stored under key; a nil or uncomparable key misses.
func (c *ConnCache) Get(key any) (any, bool) {
	if !cacheable(key) {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, k := range c.keys {
		if k == key {
			return c.vals[i], true
		}
	}
	return nil, false
}

// Put stores v under key, replacing the value of a stored key in place and
// otherwise evicting the oldest entry when full; a nil or uncomparable key
// is not stored.
func (c *ConnCache) Put(key, v any) {
	if !cacheable(key) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, k := range c.keys {
		if k == key {
			c.vals[i] = v
			return
		}
	}
	c.keys[c.next], c.vals[c.next] = key, v
	c.next = (c.next + 1) % len(c.keys)
}

// cacheable reports whether key is non-nil and == on it cannot panic: it is
// comparable down to the dynamic values it holds. Empty slots (nil) then
// never equal a lookup key.
func cacheable(key any) bool { return reflect.ValueOf(key).Comparable() }

// Message is the HTTP message a Phase acts on: the client request
// (onRequestHeaders, onRequestBody, onRoute), the leg's outgoing request
// (onUpstreamRequest), the leg's response (onUpstreamResponseHeaders,
// onUpstreamResponseBody) or the client response (onResponse).
type Message interface {
	// Header is the live header map; edits are seen by later Policies.
	Header() http.Header
	// Body returns the whole body when the Phase is a gate for it;
	// ErrNotAvailable otherwise; nil for an empty body.
	Body(ctx context.Context) ([]byte, error)
	// SetBody replaces the body: it reserves b from the buffer budget
	// (ErrBudget), sets Content-Length, removes Content-Encoding and body
	// digests, and invalidates the decoded body and CEL views.
	SetBody(ctx context.Context, b []byte) error
	// Decoded returns the shared read-only decoded JSON body (nil when not
	// JSON); ErrTooLarge past 4x the raw limit.
	Decoded(ctx context.Context) (expr.Value, error)
	// RawQuery returns the raw query of a request message; "" otherwise.
	RawQuery() string
	// SetRawQuery replaces the raw query of a request message.
	SetRawQuery(q string)
	// Status returns the status of a response message; 0 for requests.
	Status() int
	// Generated reports a Node- or Filter-generated response.
	Generated() bool
}

// Leg describes the current upstream attempt in upstream-leg Phases.
type Leg struct {
	// Upstream is the Upstream name.
	Upstream string
	// Step is the composition step name, "" for plain upstreams.
	Step string
	// Attempt counts from 1.
	Attempt int
	// Endpoint is the selected Endpoint address host:port.
	Endpoint string
	// Request is the outgoing request, mutable in onUpstreamRequest.
	Request *http.Request
	// Response is set after response headers arrived.
	Response *http.Response
	// ErrorKind is connect, timeout, reset, tls or "".
	ErrorKind string
}

// RateLimitField is one applied limit for the RateLimit-Policy and
// RateLimit response fields (OQ-traffic-management-and-resilience-2 (c):
// the data plane appends them after onResponse).
type RateLimitField struct {
	// Name is the Policy name, suffixed .1, .2 for multi-limit Policies.
	Name string
	// Q is the quota (requests or quota limit); W the window in seconds.
	Q, W int64
	// R is the remaining count and T the seconds to reset, when Known.
	R, T  int64
	Known bool
}

// Final is the outcome of a request, available in onLog.
type Final struct {
	// Status is the final HTTP status; 0 when the client went away first.
	Status int
	// Code is the final RZ code, if any.
	Code string
	// Committed is true once response headers were sent.
	Committed bool
	// RejectedAfter is true when a Policy later in request order than the
	// calling Policy responded or failed closed (quota refunds).
	RejectedAfter bool
}

// Tee is the store side of the Response Cache: a copy kept while the
// response streams, which stops (skipping the store) past its limit or
// the buffer budget.
type Tee interface {
	// Result returns the copied body and whether it is complete; valid in
	// Finish, after the response finished streaming.
	Result() (body []byte, complete bool)
}

// Exchange is one request as a Filter sees it, valid only during the call
// it is passed to (Handle, Prepare, Complete, Undo, Finish). The data plane
// implements it over pooled per-request state. Upstream-leg Phases get a
// leg view: the same client request read-only, with its own Leg, Message,
// Vars copy (upstream fields replaced), PolicyState slots and span; legs of
// an aggregate composition run concurrently, each on its own view, so a
// Filter never shares a view between goroutines. On a leg view
// SetIdentity returns ErrNotAvailable, TeeResponse returns nil, Final
// returns the zero value (a leg ends before the request does), and
// ReplaceResponse and AddRateLimitField do nothing.
type Exchange interface {
	// RequestID returns the 32-hex trace ID.
	RequestID() string
	// Now returns the request start time.
	Now() time.Time
	// Listener returns the listener name.
	Listener() string
	// Route returns the matched Route view.
	Route() *expr.Route
	// Method, Scheme, Host and Path describe the client request (Path is
	// the one normalized path).
	Method() string
	Scheme() string
	Host() string
	Path() string
	// Header returns the client request headers.
	Header() http.Header
	// Source returns the client address.
	Source() Source
	// TLS returns the connection TLS state; nil on cleartext.
	TLS() *ConnTLS
	// Message returns the message of the current Phase.
	Message() Message
	// Leg returns the current attempt in upstream-leg Phases; nil otherwise.
	Leg() *Leg
	// Vars returns the CEL activation of the current Phase.
	Vars() *expr.Vars
	// Identity returns the request identity; nil before authentication.
	Identity() *Identity
	// SetIdentity records a successful authentication; a second one
	// returns ErrSecondBinding.
	SetIdentity(id *Identity) error
	// StateBudget returns the per-request State Store deadline (pack 8.7
	// rule 2); stores and the post-commit queue come from BuildEnv.
	StateBudget() *statestore.RequestBudget
	// Reserve and Release charge decoded values to limits.maxBufferedBytes.
	Reserve(n int64) error
	Release(n int64)
	// TeeResponse starts the cache store copy (onResponse); nil when the
	// budget or limit refuses.
	TeeResponse(limit int64) Tee
	// ReplaceResponse replaces the uncommitted response (stale-if-error).
	ReplaceResponse(r *Response)
	// AddRateLimitField records an applied limit.
	AddRateLimitField(f RateLimitField)
	// Annotate adds an attribute to the current Filter span.
	Annotate(key string, v slog.Value)
	// Final returns the request outcome; valid in onLog and Finish.
	Final() Final
	// PolicyState returns the per-request slot of the Policy being run
	// (the executor selects it before every call), for state a Filter
	// carries between its Phases, Consumptive steps and Finish: a quota
	// reservation's key digest and window start, the rate-limit tokens
	// Prepare took, a cache lookup's keys, stale variant and coalescing
	// role. It holds nil at the Policy's first call. A Filter stores a
	// pointer to state it pools itself (no allocation) and releases it in
	// Finish. On a leg view the slot is private to that leg.
	PolicyState() *any
	// Stripe returns the request's metric stripe for emit handles.
	Stripe() emit.Stripe
	// RouteMetrics returns the matched Route's handles (the Response Cache
	// records ruralz_cache_requests_total there); never nil.
	RouteMetrics() *emit.RouteMetrics
}

// Filter is a compiled Policy at one attachment environment. Handle is
// called for each Phase the Policy subscribes to, on the request
// goroutine (or a composition step goroutine); it must not retain x.
type Filter interface {
	Handle(ctx context.Context, p phase.Phase, x Exchange) Result
}

// Finisher is implemented by Filters that act once the request is over:
// the Response Cache stores the teed body, queues an unsafe-method
// invalidation and releases a hit's buffer reservation; Filters release
// their PolicyState. The executor calls Finish for every Filter that
// implements it and ran in at least one Phase of the request (not skipped
// by spec.when), after onLog, in request order, on the request goroutine,
// whether or not the Policy subscribes to onLog, including when the client
// went away (x.Final() tells). Upstream-scope Filters get Finish on their
// leg view when the leg ends. Finish must not block: State Store writes go
// to the post-commit queue.
type Finisher interface {
	Finish(ctx context.Context, x Exchange)
}

// Consumptive is implemented by admission Filters whose decision needs one
// consumptive State Store call (ratelimit, quota). The executor batches a
// run of consecutive Consumptive Filters of onRequestHeaders into shared
// round trips (pack 8.7 rule 3) instead of calling Handle, selecting each
// member's PolicyState before Prepare, Complete and Undo; a member keeps in
// it what Undo and onLog need (the call is the executor's and is reused).
type Consumptive interface {
	Filter
	// Prepare runs every Node-local step. It either decides (done true,
	// with r) or fills call and returns done false.
	Prepare(ctx context.Context, x Exchange, call *statestore.Call) (r Result, done bool)
	// Complete applies the reply (call.Err set on failure) and decides.
	Complete(ctx context.Context, x Exchange, call *statestore.Call) Result
	// Undo returns local tokens when an earlier member denied.
	Undo(x Exchange)
}

// Closer is implemented by Filters holding pools, goroutines or secret
// watches; it is called when no live snapshot shares the Filter.
type Closer interface{ Close() error }

// Component is a Node-wide part of a Policy type that outlives snapshots:
// the JWKS manager, the upstream token registry, the auth.basic throttle,
// the rate-limit key table, the Response Cache revalidation workers.
// internal/filter/builtin constructs each once per process and hands it to
// the Factories that use it; the ruralzd supervisor runs every Component
// on one goroutine it owns before the first activation and cancels it at
// shutdown after the last snapshot retired (Registry.Components).
type Component interface {
	// Name identifies the component in logs and shutdown errors.
	Name() string
	// Run serves until ctx is done and returns after every goroutine it
	// started has exited.
	Run(ctx context.Context) error
}

// Replayer sends a saved request through one Upstream's leg (upstream-leg
// Policies, breaker, bulkhead, retries) outside any client request, for
// Response Cache revalidation (05 req 86). The implementation
// (internal/gateway/replay) pins the currently published snapshot for the
// whole call and resolves route and upstream by name in it; either being
// gone is ErrNotAvailable. The caller closes the response body.
type Replayer interface {
	Replay(ctx context.Context, route, upstream string, req *http.Request) (*http.Response, error)
}

// Limits are the effective Node limits a Filter may need.
type Limits struct {
	MaxRequestBodyBytes  int64
	MaxResponseBodyBytes int64
	MaxBufferedBytes     int64
}

// Shared memoizes per-snapshot artifacts (such as the Consumer credential
// index) across the Factories of one snapshot compile.
type Shared interface {
	Get(key any, build func() (any, error)) (any, error)
}

// BuildEnv is everything a Factory needs to compile one Policy at one
// attachment environment. Filters are built per (Policy, scope, Phase
// set), never per Route.
type BuildEnv struct {
	// Policy is the typed Policy and Config its typed config.
	Policy *v1alpha1.Policy
	Config any
	// Scope and Phases come from the effective chains (precedence).
	Scope  phase.Scope
	Phases phase.Set
	// Bundle is the validated Revision; Consumers the compiled Consumers.
	Bundle    *hub.Bundle
	Consumers map[string]*expr.Consumer
	// CEL compiles this Policy's expressions; Site refines a place for
	// this attachment.
	CEL  expr.Builder
	Site func(place expr.PlaceID) expr.Site
	// Secrets is the resolved secret table of the Revision.
	Secrets secret.Store
	// StateStore is the main store; CacheStore the Response Cache
	// connection (the main store when none is configured).
	StateStore statestore.Store
	CacheStore statestore.Store
	Enqueuer   statestore.Enqueuer
	// StateStoreTimeout is the effective spec.stateStoreTimeout.
	StateStoreTimeout time.Duration
	Limits            Limits
	Metrics           *emit.PolicyMetrics
	Status            emit.NodeStatus
	Clock             clock.Clock
	Logger            *slog.Logger
	Shared            Shared
	// Replayer revalidates Response Cache entries (cache type only).
	Replayer Replayer
	// Previous is the Filter built for the same Policy identity and
	// canonical config in the previous snapshot, for carry-over; nil if none.
	Previous Filter
}

// Factory builds Filters for one Policy type. An error carries an RZ-CFG
// code (errcode.Wrap) and rejects the Revision.
type Factory interface {
	Build(ctx context.Context, env BuildEnv) (Filter, error)
}

// Registry maps Policy types to Factories and lists the Node-wide
// Components they share; internal/filter/builtin constructs it as an
// explicit table.
type Registry struct {
	factories  map[v1alpha1.PolicyType]Factory
	components []Component
}

// NewRegistry returns a Registry over a copy of f with the components c.
func NewRegistry(f map[v1alpha1.PolicyType]Factory, c ...Component) *Registry {
	m := make(map[v1alpha1.PolicyType]Factory, len(f))
	for k, v := range f {
		m[k] = v
	}
	return &Registry{factories: m, components: append([]Component(nil), c...)}
}

// Factory returns the Factory of t.
func (r *Registry) Factory(t v1alpha1.PolicyType) (Factory, bool) {
	f, ok := r.factories[t]
	return f, ok
}

// Components returns the Node-wide components to run, in construction order.
func (r *Registry) Components() []Component { return append([]Component(nil), r.components...) }
