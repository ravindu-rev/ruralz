// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package emit is the telemetry contract every area records through:
// metric handles resolved at snapshot compile time, the tracer, the access
// log, degraded states and cleartext hops. internal/telemetry implements
// it on Ruralz-owned aggregates and the OpenTelemetry SDK; no other
// package imports OpenTelemetry. Recording methods take no locks, allocate
// nothing and never block; loggers are *slog.Logger values from
// internal/telemetry (tests pass slog.New(slog.DiscardHandler)).
package emit

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Stripe selects a counter stripe (min(GOMAXPROCS, 8) stripes); it is
// assigned per connection at accept and offset per request.
type Stripe uint8

// Counter is a monotonic counter handle.
type Counter interface{ Add(s Stripe, n uint64) }

// Gauge is a gauge handle.
type Gauge interface {
	Add(s Stripe, d int64)
	Set(v int64)
}

// Histogram records integers: nanoseconds for duration sets, bytes for the
// bytes set, millionths for the ratio set.
type Histogram interface {
	Record(s Stripe, v uint64)
	// RecordExemplar is called only for sampled requests.
	RecordExemplar(s Stripe, v uint64, ex Exemplar)
}

// StatusCounter counts by status_class derived from an HTTP status.
type StatusCounter interface{ Inc(s Stripe, status int) }

// CodeCounter counts by a registered RZ code label, created on first use.
type CodeCounter interface{ Inc(s Stripe, code string) }

// Exemplar links a histogram observation to a sampled trace.
type Exemplar struct {
	TraceID [16]byte
	SpanID  [8]byte
	Time    time.Time
}

// Label value indexes. Each enumerated label of catalog.Families is an
// index into the handle arrays below; Num* is the array length.

// Protocols: http1, http2.
const (
	ProtoHTTP1 = iota
	ProtoHTTP2
	NumProtocols
)

// Origins: upstream, node, dependency.
const (
	OriginUpstream = iota
	OriginNode
	OriginDependency
	NumOrigins
)

// Connection results: accepted, tls_failure, refused.
const (
	ConnAccepted = iota
	ConnTLSFailure
	ConnRefused
	NumConnResults
)

// Cache results: hit, miss, bypass, stale, stale_error.
const (
	CacheHit = iota
	CacheMiss
	CacheBypass
	CacheStale
	CacheStaleError
	NumCacheResults
)

// Rate limit results: allow, deny_local, deny_global, fail_open.
const (
	RateLimitAllow = iota
	RateLimitDenyLocal
	RateLimitDenyGlobal
	RateLimitFailOpen
	NumRateLimitResults
)

// Quota results: allow, deny, no_quota, fail_open.
const (
	QuotaAllow = iota
	QuotaDeny
	QuotaNoQuota
	QuotaFailOpen
	NumQuotaResults
)

// Store skip reasons: size_limit, buffer_budget, memory.
const (
	SkipSizeLimit = iota
	SkipBufferBudget
	SkipMemory
	NumSkipReasons
)

// Failure modes: open, closed.
const (
	ModeOpen = iota
	ModeClosed
	NumModes
)

// Attempt errors: none, connect, timeout, reset, tls.
const (
	ErrNone = iota
	ErrConnect
	ErrTimeout
	ErrReset
	ErrTLS
	NumAttemptErrors
)

// Breaker states: closed, open, half_open.
const (
	BreakerClosed = iota
	BreakerOpen
	BreakerHalfOpen
	NumBreakerStates
)

// Ejection reasons: passive, active.
const (
	EjectPassive = iota
	EjectActive
	NumEjectReasons
)

// Upstream degraded reasons: panic, discovery_stale, balancer_budget.
const (
	UpDegradedPanic = iota
	UpDegradedDiscoveryStale
	UpDegradedBalancerBudget
	NumUpDegraded
)

// Upstream CEL fields: hashKey, retryOn, failureWhen.
const (
	CELHashKey = iota
	CELRetryOn
	CELFailureWhen
	NumCELFields
)

// Pool states: idle, active.
const (
	PoolIdle = iota
	PoolActive
	NumPoolStates
)

// State Store op labels, in catalog.Ops() order: gcra, quota, cache_get,
// script_multi, pipeline, cache_set, cache_invalidate, refund. They index
// StateMetrics; statestore.OpKind.Label and RoundTrip.Label map onto them.
const (
	StateOpGCRA = iota
	StateOpQuota
	StateOpCacheGet
	StateOpScriptMulti
	StateOpPipeline
	StateOpCacheSet
	StateOpCacheInvalidate
	StateOpRefund
	NumStateOps
)

// State Store call results: ok, error, timeout, skipped (statestore.Result
// values are these indexes).
const (
	StateResultOK = iota
	StateResultError
	StateResultTimeout
	StateResultSkipped
	NumStateResults
)

// Post-commit write kinds, in catalog.WriteKinds() order: refund,
// cache_set, cache_invalidate.
const (
	WriteRefund = iota
	WriteCacheSet
	WriteCacheInvalidate
	NumWriteKinds
)

// ListenerRequests counts ruralz_http_listener_requests_total.
type ListenerRequests interface {
	Inc(s Stripe, protocol int, status int, origin int)
}

// ListenerMetrics are one listener's handles.
type ListenerMetrics struct {
	Requests        ListenerRequests
	GatewayDuration Histogram
	RequestBody     Histogram
	ResponseBody    Histogram
	Active          Gauge
	OpenConns       [NumProtocols]Gauge
	Conns           [NumProtocols][NumConnResults]Counter
	TLSHandshake    Histogram
}

// RouteMetrics are one Route's handles (folded Routes share _overflow).
type RouteMetrics struct {
	Requests StatusCounter
	Duration Histogram
	Cache    [NumCacheResults]Counter
}

// AuthDecisions counts ruralz_auth_decisions_total: allow with no code,
// deny with the RZ code.
type AuthDecisions interface {
	Allow(s Stripe)
	Deny(s Stripe, code string)
}

// PolicyMetrics are one Policy's handles. Families a Policy type never
// records hold no-op handles.
type PolicyMetrics struct {
	Duration          [phase.Count]Histogram
	ShortCircuits     [phase.Count]StatusCounter
	Failures          [phase.Count][NumModes]Counter
	Auth              AuthDecisions
	JWKSAge           Gauge
	UpstreamTokenAge  Gauge
	UpstreamRefreshes Counter
	RateLimit         [NumRateLimitResults]Counter
	BucketEvictions   Counter
	Quota             [NumQuotaResults]Counter
	StoreSkipped      [NumSkipReasons]Counter
}

// UpstreamAttempts counts ruralz_upstream_attempts_total.
type UpstreamAttempts interface {
	Inc(s Stripe, status int, attemptErr int)
}

// UpstreamMetrics are one Upstream's handles.
type UpstreamMetrics struct {
	Attempts             UpstreamAttempts
	AttemptDuration      Histogram
	Retries              Counter
	RetryBudgetExhausted Counter
	BreakerState         [NumBreakerStates]Gauge
	Ejections            [NumEjectReasons]Counter
	HealthyEndpoints     Gauge
	ProbesSkipped        Counter
	Degraded             [NumUpDegraded]Gauge
	CELErrors            [NumCELFields]Counter
	PoolConnections      [NumPoolStates]Gauge
}

// StateMetrics are the State Store handles, indexed by the StateOp*,
// StateResult* and Write* constants (never by statestore.OpKind values).
type StateMetrics struct {
	CallDuration  [NumStateOps]Histogram
	Calls         [NumStateOps][NumStateResults]Counter
	Ops           [NumStateOps]Counter // by kind (single-operation ops)
	WritesDropped [NumWriteKinds]Counter
	QueueItems    Gauge
}

// ConfigMetrics are the configuration and snapshot handles.
type ConfigMetrics struct {
	// Activations counts {result, code}: Activated(code "") or Rejected(code).
	Activated          Counter
	Rejected           CodeCounter
	ActivationDuration func(stage string, sizeClass string) Histogram
	RetiredSnapshots   Gauge
	RetirementEnded    Counter
	// RevisionInfo sets the at most two ruralz_config_revision_info series.
	RevisionInfo       func(role string, display string)
	SecretRotationFail func(provider string) Counter
}

// NodeMetrics are the Node-wide handles, available before any Revision.
type NodeMetrics struct {
	NodeResponses          CodeCounter
	GatewayDurationSkipped Counter
	BufferedBytes          Gauge
	TapEventsDropped       Counter
	State                  StateMetrics
	Config                 ConfigMetrics
}

// Shape is what admission needs from a compiled Revision.
type Shape struct {
	Listeners []string
	Routes    []string
	Upstreams []string
	Policies  []PolicyShape
}

// PolicyShape describes one Policy for admission.
type PolicyShape struct {
	Name    string
	Type    string
	Scopes  phase.ScopeSet
	Phases  phase.Set
	Codes   []string
	Gateway bool // attached at Gateway scope: its hot label sets are striped
}

// Plan is the admission result of one Revision: handles for every
// resource, folded ones pointing at _overflow. Never nil handles.
type Plan interface {
	Listener(name string) *ListenerMetrics
	Route(name string) *RouteMetrics
	Upstream(name string) *UpstreamMetrics
	Policy(name string) *PolicyMetrics
}

// Binding tracks which label sets a snapshot references.
type Binding interface {
	// Retire marks the snapshot retired or closing.
	Retire()
	// Release ends label sets no live snapshot references.
	Release()
}

// Meter admits Revisions and exposes Node-wide handles.
type Meter interface {
	// Admit plans handles off the request path, deterministically.
	Admit(s Shape) (Plan, error)
	// Bind is called at the swap.
	Bind(p Plan) Binding
	// Node returns the Node-wide handles.
	Node() *NodeMetrics
	// NewStripe returns the stripe of a new connection (round robin).
	NewStripe() Stripe
}

// NodeStatus sets degraded reasons and cleartext hops.
type NodeStatus interface {
	// SetDegraded raises or clears reason for source; the gauge is 1 while
	// any source holds it.
	SetDegraded(r catalog.Reason, source string, on bool)
	// SetCleartextHops sets the count of cleartext hops of one kind.
	SetCleartextHops(h catalog.Hop, n int)
}

// Decision is the one sampling decision of a request.
type Decision struct {
	TraceID      [16]byte
	ServerSpanID [8]byte
	ParentSpanID [8]byte
	Remote       bool
	Sampled      bool
	// TraceState is the validated raw tracestate, re-injected unchanged.
	TraceState string
}

// Outcome values of ruralz.outcome.
const (
	OutcomeContinue     = "continue"
	OutcomeRespond      = "respond"
	OutcomeCannotDecide = "cannot_decide"
	OutcomeSkipped      = "skipped"
)

// Span is a started span; the no-op span of an unsampled request costs
// nothing. End must be called exactly once.
type Span interface {
	SetAttr(key string, v slog.Value)
	SpanID() [8]byte
	End(status int, code, errorType string)
}

// ServerAttrs are the SERVER span attributes known at start.
type ServerAttrs struct {
	Listener, Scheme, Host, Path, ClientAddress, UserAgent, Protocol string
	Port                                                             int
}

// FilterAttrs are the attributes of ruralz.filter.<name>.
type FilterAttrs struct {
	PolicyType string
	Phase      phase.Phase
}

// UpstreamAttrs are the attributes of ruralz.upstream.<name>.
type UpstreamAttrs struct {
	Attempt  int
	Endpoint string
	Step     string
}

// Tracer makes the per-request trace decision and starts spans. Span names
// are precomputed at compile time (catalog.FilterSpanName).
type Tracer interface {
	// Decide extracts traceparent and tracestate, applies the ratio and the
	// root and parent caps, and fills d; it never allocates.
	Decide(h http.Header, ratio float64, d *Decision)
	StartServer(ctx context.Context, d *Decision, method string, a ServerAttrs) (context.Context, Span)
	StartRouteMatch(ctx context.Context) (context.Context, Span)
	StartFilter(ctx context.Context, spanName string, a FilterAttrs) (context.Context, Span)
	StartUpstream(ctx context.Context, spanName string, a UpstreamAttrs) (context.Context, Span)
	// Inject replaces client traceparent and tracestate on an outgoing leg.
	Inject(ctx context.Context, d *Decision, h http.Header)
}

// Excluder marks sections excluded from gateway-added time (client I/O,
// upstream I/O, State Store round trips, declared remote calls). Calls
// nest; it is safe from parallel legs.
type Excluder interface {
	Enter(now time.Time)
	Leave(now time.Time)
}

// AccessRecord is one access log record, filled on the request goroutine
// in onLog. Strings must be immutable values (net/http request strings are),
// never views of pooled buffers; the writer truncates and encodes.
type AccessRecord struct {
	Start                                time.Time
	TraceID                              [16]byte
	SpanID                               [8]byte
	Sampled                              bool
	Revision                             string
	Listener, Protocol, Route            string
	Method, Host, Path                   string
	Status                               int
	Code                                 string
	Duration                             time.Duration
	RequestBytes, ResponseBytes          int64
	GatewayDuration                      time.Duration
	GatewayDurationSkipped               bool
	UpstreamDuration, StateStoreDuration time.Duration
	ClientAddress, UserAgent, TLSVersion string
	Consumer, Tier, AuthMethod           string
	Upstream, Endpoint                   string
	Attempts                             int
	ShortCircuitPolicy                   string
	ShortCircuitPhase                    phase.Phase
	FailureModes                         []FailureModeEntry
	Cache                                string
}

// FailureModeEntry is one undecided Policy (at most 8 per record).
type FailureModeEntry struct {
	Policy string
	Phase  phase.Phase
	Mode   string
}

// AccessLog is the bounded, non-blocking access log writer.
type AccessLog interface {
	// Acquire returns a pooled, reset record.
	Acquire() *AccessRecord
	// Submit enqueues r (dropping with a counter when full) and returns it
	// to the pool after encoding; the caller must not touch r afterwards.
	Submit(r *AccessRecord)
}
