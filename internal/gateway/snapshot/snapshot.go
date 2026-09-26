// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package snapshot is the compiled, immutable form of one Revision that
// the request path reads lock-free: Routers per listener, compiled Routes
// with per-Phase Filter arrays and forwarders, listener and Gateway
// settings, compiled Consumers, CEL programs and metric handles. It is
// built by internal/gateway/compile, published and retired by
// internal/gateway/retire, and read by the handler, executor, Router and
// Upstream layer. It also fixes the request-path contracts between those
// packages: Forwarder and Outbound (handler to Upstream layer), LegHooks
// and LegRun (Upstream layer to executor), RequestBody (body buffering to
// Upstream layer) and RequestState (exchange to executor). It holds data
// and interfaces only, plus the pin stripes.
package snapshot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Snapshot is one compiled Revision. Every field is immutable after
// publication; resources are closed only at zero pins when no newer
// snapshot shares them. Resolved secrets and Endpoint sets live beside it
// and change copy-on-write.
type Snapshot struct {
	// Revision is the digest and exact canonical bytes (/config/dump, LKG).
	Revision revision.Revision
	// Validated is the pipeline output the snapshot was compiled from.
	Validated *hub.Validated
	// Gateway holds the Node-wide settings of the Revision.
	Gateway Gateway
	// Listeners are the client listeners in name order.
	Listeners []*Listener
	// Routers holds one Router per listener name.
	Routers map[string]Router
	// Routes are indexed by Route.Index.
	Routes []*Route
	// Consumers are the compiled Consumers by name.
	Consumers map[string]*expr.Consumer
	// Programs is the CEL program set, passed as prev to the next Builder.
	Programs expr.ProgramSet
	// Metrics is the admission plan of the Revision.
	Metrics emit.Plan
	// Binding is the snapshot's metric label-set binding (emit.Meter.Bind).
	// Activation sets it just before publication; the retirer calls Retire
	// when the snapshot is retired and Release at zero pins.
	Binding emit.Binding
	// LegChains are the upstream-leg chains by Upstream name; Route chains
	// share these values (a leg chain depends only on its Upstream).
	LegChains map[string]*LegChain
	// Legs runs one leg to a named Upstream outside a Route's forwarding
	// plan (Response Cache revalidation through internal/gateway/replay).
	Legs LegRunner
	// StateStore and CacheStore are the retained State Store handles.
	StateStore, CacheStore StoreHandle
	// Resources are closed at zero pins unless a newer snapshot shares them.
	Resources []io.Closer
	// Pins counts requests running on the snapshot.
	Pins *Pins
}

// StoreHandle is a snapshot's reference to a State Store driver.
type StoreHandle interface {
	Store() statestore.Store
	Enqueuer() statestore.Enqueuer
	Release()
}

// Gateway holds the compiled Gateway settings.
type Gateway struct {
	// AdminPort is spec.admin.port (default 9901).
	AdminPort int
	// TrustedProxies is spec.trustedProxies.
	TrustedProxies []netip.Prefix
	// Limits are the effective limits.
	Limits Limits
	// TraceSampling is the root ratio (default 0.01).
	TraceSampling float64
	// OTLPEndpoint is spec.telemetry.otlp.endpoint, "" for none.
	OTLPEndpoint string
	// AccessLogWhen is the compiled accessLog.when; nil selects every request.
	AccessLogWhen expr.Program
	// StateStoreTimeout is the default Policy stateStoreTimeout.
	StateStoreTimeout time.Duration
}

// Limits are effective Node limits of the Revision (bytes after decoding).
type Limits struct {
	MaxRequestBodyBytes  int64
	MaxResponseBodyBytes int64
	MaxBufferedBytes     int64
	MaxCompositionSteps  int
	// MaxRequestHeaderBytes is min(configured, 256 KiB).
	MaxRequestHeaderBytes int64
	// HeaderLimitCapped raises header_limit_capped while active.
	HeaderLimitCapped bool
}

// Listener is one compiled client listener.
type Listener struct {
	Name          string
	Protocol      v1alpha1.ListenerProtocol
	Port          int
	ProxyProtocol bool
	// Hostnames limits the listener; empty accepts any host.
	Hostnames []string
	// TLS is set for https.
	TLS     *ListenerTLS
	Metrics *emit.ListenerMetrics
}

// ListenerKey is the identity that decides whether a Hot Reload needs a
// new socket: TLS and hostnames changes never do.
type ListenerKey struct {
	Name          string
	Port          int
	Protocol      v1alpha1.ListenerProtocol
	ProxyProtocol bool
}

// Key returns the listener identity.
func (l *Listener) Key() ListenerKey {
	return ListenerKey{Name: l.Name, Port: l.Port, Protocol: l.Protocol, ProxyProtocol: l.ProxyProtocol}
}

// ListenerTLS is read by GetConfigForClient for new handshakes.
type ListenerTLS struct {
	// MinVersion is tls.VersionTLS13 (default) or VersionTLS12.
	MinVersion uint16
	// Certificates in name order; values come from the secret Store.
	Certificates []Certificate
	// RequestClientCert is true when a Route bound to the listener has
	// auth.mtls in its effective chain.
	RequestClientCert bool
}

// Certificate references one served certificate and key.
type Certificate struct {
	Name        string
	Certificate secret.Ref
	PrivateKey  secret.Ref
}

// Router is one listener's compiled Router.
type Router interface {
	// Match selects a Route; it allocates nothing. params is caller storage.
	Match(ctx context.Context, q *MatchRequest, params []expr.Param) MatchResult
}

// MatchRequest is the pre-body request data routing uses.
type MatchRequest struct {
	Host   string
	Path   string
	Method string
	Header http.Header
	// Vars is the pre-route activation (request, source, now) for match.when.
	Vars *expr.Vars
}

// MatchResult is the Router's answer.
type MatchResult struct {
	// Route is the index into Snapshot.Routes; -1 means RZ-RT-001.
	Route int32
	// Params are template captures.
	Params []expr.Param
	// Err is a match.when runtime error: RZ-RT-006, no fallthrough.
	Err error
}

// Route is one compiled Route.
type Route struct {
	Index int32
	Name  string
	// View is the CEL route variable.
	View *expr.Route
	// Listeners are the bound listener names.
	Listeners []string
	// Protocol is the protocol of the Route's Upstreams; the handler's
	// dispatch table is keyed by it. M1 serves only http (validation
	// rejects every other protocol with RZ-CFG-040); M3 and M4 add handlers.
	Protocol v1alpha1.UpstreamProtocol
	// Timeout is the effective Route timeout (15 s default in M1).
	Timeout time.Duration
	// StateStoreMax is the largest stateStoreTimeout of its State Store
	// Policies (statestore.RouteMax); 0 when none.
	StateStoreMax time.Duration
	// Chain holds the compiled Policies.
	Chain Chain
	// Forward runs plain upstreams or composition.
	Forward Forwarder
	// Body says which bodies are gated or teed.
	Body    BodyNeeds
	Metrics *emit.RouteMetrics
}

// BodyNeeds are the compile-time body decisions of a Route.
type BodyNeeds struct {
	// RequestGate buffers the request body before onRequestBody.
	RequestGate bool
	// ResponseGate buffers the client response before onResponse.
	ResponseGate bool
	// ResponseTee copies the response for the Response Cache store.
	ResponseTee bool
}

// Chain is a Route's compiled Filter Chain, built from hub.Chain.
type Chain struct {
	// Client holds client-leg Phases in execution order.
	Client [phase.Count][]*Policy
	// Legs holds upstream-leg chains by Upstream name.
	Legs map[string]*LegChain
	// Policies lists every Policy once, in request order; Policy.Index
	// points here (per-request skip bitsets, onLog order).
	Policies []*Policy
	// AuthPolicies counts auth-class Policies (Security rule 1).
	AuthPolicies int
}

// LegChain is one Upstream's upstream-leg chain.
type LegChain struct {
	Upstream string
	Phases   [phase.Count][]*Policy
}

// Policy is one compiled Policy attachment.
type Policy struct {
	// Index is the position in Chain.Policies.
	Index       int
	Name        string
	Type        v1alpha1.PolicyType
	Class       phase.Class
	Scope       phase.Scope
	Position    int
	FailureMode v1alpha1.FailureMode
	// When is the compiled spec.when; nil when absent.
	When       expr.Program
	FirstPhase phase.Phase
	Phases     phase.Set
	// StateStoreTimeout is the effective timeout for State Store Policies.
	StateStoreTimeout time.Duration
	Filter            filter.Filter
	// Consumptive is Filter as filter.Consumptive, or nil.
	Consumptive filter.Consumptive
	// SpanName is "ruralz.filter.<name>".
	SpanName string
	Metrics  *emit.PolicyMetrics
}

// Forwarder sends a request to its Upstream legs: the Upstream layer
// (plain upstreams) or the composition engine. Exactly one of the results
// is non-nil; a failure error carries its RZ code (errcode.CodeOf).
type Forwarder interface {
	Forward(ctx context.Context, o *Outbound) (*UpstreamResponse, error)
}

// LegRunner runs one leg to a named Upstream of the snapshot, outside any
// Route's forwarding plan; the Upstream layer implements it per snapshot.
type LegRunner interface {
	RunLeg(ctx context.Context, upstream string, o *Outbound) (*UpstreamResponse, error)
}

// Outbound is what a Forwarder needs from the request besides the context.
// The handler fills it after the request Phases; it is read-only for the
// Forwarder and its legs.
type Outbound struct {
	// X is the client exchange: method, normalized path, query, headers
	// after the request Phases, Source for forwarding headers, Vars (the
	// Base variables hashKey and step expressions read).
	X filter.Exchange
	// Body is the client request body.
	Body RequestBody
	// Trace is the request's sampling decision; every leg injects it
	// (emit.Tracer.Inject).
	Trace *emit.Decision
	// Stripe is the request's metric stripe.
	Stripe emit.Stripe
	// Deadline is the Route deadline (request start plus Route timeout).
	Deadline time.Time
	// Hooks runs the upstream-leg Phases of every leg.
	Hooks LegHooks
}

// ErrNotReplayable is returned by RequestBody.Open when the body cannot be
// sent again.
var ErrNotReplayable = errors.New("snapshot: request body is not replayable")

// RequestBody is the client request body as the Upstream layer sees it
// (05 req 31, 54); internal/gateway/body implements it over the body gate
// or the client stream. Opens happen on one goroutine at a time except for
// a gated body, whose readers are independent.
type RequestBody interface {
	// ContentLength is the length when known (declared or gated), -1 when
	// unknown, 0 for no body.
	ContentLength() int64
	// Replayable reports whether another Open can succeed: the body is
	// empty, gated (buffered within maxRequestBodyBytes), or streamed with
	// no byte read yet.
	Replayable() bool
	// Open returns a reader from the first byte for one attempt or step;
	// ErrNotReplayable once a streamed body was read. Closing the reader
	// never closes the client stream.
	Open() (io.ReadCloser, error)
	// Buffered returns the gated bytes, or nil for a streamed body; the
	// Upstream layer sets http.Request.GetBody only when it is non-nil.
	Buffered() []byte
}

// LegHooks run upstream-leg Phases; the executor implements them over the
// request's RequestState.
type LegHooks interface {
	// BeginLeg starts one leg: an Upstream leg of plain upstreams or one
	// composition step. Legs of an aggregate composition run concurrently,
	// each with its own LegRun.
	BeginLeg(ctx context.Context, upstream, step string) LegRun
}

// LegRun is one leg's hooks; its methods are called from the leg's
// goroutine only. End must be called exactly once, after the leg's last
// attempt; it runs Finish for the leg's Filters and releases the leg view.
type LegRun interface {
	// OnUpstreamRequest runs per attempt; a non-nil response ends the leg
	// without retry.
	OnUpstreamRequest(ctx context.Context, leg *filter.Leg) *filter.Response
	// OnUpstreamResponseHeaders runs per attempt in reverse order; retry is
	// true when a Filter returned filter.Retry; replace is a leg response
	// set by a failure (502 RZ-RT-012).
	OnUpstreamResponseHeaders(ctx context.Context, leg *filter.Leg) (retry bool, replace *filter.Response)
	// OnUpstreamResponseBody runs once per leg when subscribed.
	OnUpstreamResponseBody(ctx context.Context, leg *filter.Leg) *filter.Response
	// End finishes the leg.
	End(ctx context.Context)
}

// RequestState is the per-request state the executor drives, implemented
// by internal/gateway/exchange over pooled memory. A RequestState belongs
// to one goroutine at a time: the request goroutine for the client state,
// the leg's goroutine for a leg state.
type RequestState interface {
	// Exchange returns the Filter view; its Message, Leg, Vars,
	// PolicyState and Annotate follow the last Enter and SetLeg.
	Exchange() filter.Exchange
	// Enter selects the Policy and Phase of the next Filter call and the
	// span Annotate writes to (nil when the request is not sampled).
	Enter(p *Policy, ph phase.Phase, span emit.Span)
	// SetLeg sets the attempt of a leg state (Leg and Message follow it).
	SetLeg(l *filter.Leg)
	// When returns p's recorded spec.when decision: decided is false until
	// SetWhen, skip is true when the Policy is skipped. A client state
	// records one decision per request, a leg state one per leg.
	When(p *Policy) (decided, skip bool)
	// SetWhen records p's decision; a Policy without when is recorded
	// with skip false before its first Phase, so decided and !skip means
	// the Policy ran.
	SetWhen(p *Policy, skip bool)
	// RecordShortCircuit and RecordFailure feed the access record, the
	// Filter metrics labels and /tap.
	RecordShortCircuit(p *Policy, ph phase.Phase, status int)
	RecordFailure(p *Policy, ph phase.Phase, mode v1alpha1.FailureMode)
	// Budget is the per-request State Store deadline and stripe.
	Budget() *statestore.RequestBudget
	// Leg returns a leg state: the client request read-only, and its own
	// Leg, Message, Vars copy (upstream fields replaced), PolicyState slots
	// and when decisions for leg Policies.
	Leg(upstream, step string) RequestState
	// Release returns a leg state to its pool; a no-op on the client state,
	// which the handler releases.
	Release()
}

// UpstreamResponse is what the handler commits.
type UpstreamResponse struct {
	Status  int
	Header  http.Header
	Trailer http.Header
	// Body streams the response; Close releases the bulkhead slot.
	Body io.ReadCloser
	// Generated is set when a leg Filter responded.
	Generated *filter.Response
	// Partial sets ruralz-partial: true (optional steps failed).
	Partial bool
	// Upstream, Endpoint and Attempts describe the last leg for logs.
	Upstream  string
	Endpoint  string
	Attempts  int
	ErrorKind string
}

// EndReason says why the data plane ended a pinned request.
type EndReason uint8

// End reasons.
const (
	EndNone  EndReason = iota
	EndGrace           // RZ-RT-014
	EndDrain           // RZ-RT-016
)

// PinnedRequest is the pooled per-request record the ending protocol acts
// on. Mu guards Committed and Ended; the handler commits under Mu.
type PinnedRequest struct {
	Mu        sync.Mutex
	Committed bool
	Ended     EndReason
	// Cancel cancels the request's root context.
	Cancel context.CancelFunc
	// RC sets deadlines on the client connection.
	RC *http.ResponseController

	prev, next *PinnedRequest
}

// Pins counts and lists the requests pinned to one snapshot, in
// cache-line-padded stripes (S = min(GOMAXPROCS at start, 8)). Zero pins
// proves no request runs on the snapshot.
type Pins struct{ stripes []pinStripe }

type pinStripe struct {
	n    atomic.Int64
	mu   sync.Mutex
	head *PinnedRequest
	_    [40]byte
}

// NewPins returns n stripes.
func NewPins(n int) *Pins { return &Pins{stripes: make([]pinStripe, max(n, 1))} }

// Add pins r on stripe s.
func (p *Pins) Add(s int, r *PinnedRequest) {
	st := &p.stripes[s%len(p.stripes)]
	st.n.Add(1)
	st.mu.Lock()
	r.prev, r.next = nil, st.head
	if st.head != nil {
		st.head.prev = r
	}
	st.head = r
	st.mu.Unlock()
}

// Remove unpins r from stripe s.
func (p *Pins) Remove(s int, r *PinnedRequest) {
	st := &p.stripes[s%len(p.stripes)]
	st.mu.Lock()
	if r.prev != nil {
		r.prev.next = r.next
	} else if st.head == r {
		st.head = r.next
	}
	if r.next != nil {
		r.next.prev = r.prev
	}
	r.prev, r.next = nil, nil
	st.mu.Unlock()
	st.n.Add(-1)
}

// Count returns the number of pinned requests.
func (p *Pins) Count() int64 {
	var n int64
	for i := range p.stripes {
		n += p.stripes[i].n.Load()
	}
	return n
}

// Each calls fn for every pinned request, one stripe at a time; fn must
// not call Add or Remove.
func (p *Pins) Each(fn func(*PinnedRequest)) {
	for i := range p.stripes {
		st := &p.stripes[i]
		st.mu.Lock()
		for r := st.head; r != nil; r = r.next {
			fn(r)
		}
		st.mu.Unlock()
	}
}
