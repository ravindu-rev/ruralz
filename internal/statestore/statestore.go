// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package statestore is the driver-neutral State Store contract
// (docs/architecture/11-scalability-and-distributed-state.md, foundation
// pack section 8.7): typed operations, the Store every stateful Filter
// calls, the per-request deadline, the five RZ-STS failures and the
// post-commit queue interface. Drivers live in internal/statestore/memory
// and internal/statestore/redis (the only rueidis importer); the Manager
// that owns them across Hot Reloads lives in internal/statestore/manager.
package statestore

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// DriverKind selects the implementation (pack section 7: memory and redis).
type DriverKind uint8

// Drivers.
const (
	DriverMemory DriverKind = iota + 1
	DriverRedis
)

// Topology is the redis deployment shape.
type Topology uint8

// Topologies.
const (
	TopologyStandalone Topology = iota + 1
	TopologyCluster
)

// Role says which connection of a Gateway a driver serves.
type Role uint8

// Roles: the main State Store and the optional Response Cache connection
// (OQ-scalability-and-distributed-state-3 (a)).
const (
	RoleMain Role = iota + 1
	RoleCache
)

// Source records where the configuration came from.
type Source uint8

// Sources.
const (
	SourceGateway  Source = iota + 1 // Gateway spec.stateStore
	SourceEnv                        // RURALZ_STATE_STORE_URL
	SourceFallback                   // neither: memory, state_store_memory_fallback
)

// DefaultTimeout applies when Gateway spec.stateStore.timeout is unset.
const DefaultTimeout = 50 * time.Millisecond

// Config is the resolved configuration of one driver. URL is a resolved
// secret: never logged, rendered or put in an error.
type Config struct {
	Role           Role
	Driver         DriverKind
	Topology       Topology
	URL            string
	DefaultTimeout time.Duration
	Source         Source
}

// Digest is the SHA-256 of a key value (config.key result).
type Digest [sha256.Size]byte

// DigestOf hashes s.
func DigestOf(s string) Digest { return sha256.Sum256([]byte(s)) }

// OpKind is one operation; String returns the ruralz_state_* op label.
type OpKind uint8

// Operations of M1. M2/M3 add budget, settle, semantic_get,
// semantic_set and the Plugin operations.
const (
	OpGCRA OpKind = iota + 1
	OpQuota
	OpCacheGet
	OpCacheSet
	OpCacheInvalidate
	OpRefund
)

// String returns the op label.
func (o OpKind) String() string {
	switch o {
	case OpGCRA:
		return "gcra"
	case OpQuota:
		return "quota"
	case OpCacheGet:
		return "cache_get"
	case OpCacheSet:
		return "cache_set"
	case OpCacheInvalidate:
		return "cache_invalidate"
	case OpRefund:
		return "refund"
	default:
		return ""
	}
}

// Label returns the op's index in emit.StateMetrics (the emit.StateOp*
// constants, catalog.Ops order), or -1 for an unknown op. OpKind values
// are not label indexes: catalog.Ops places script_multi and pipeline
// before cache_set.
func (o OpKind) Label() int {
	switch o {
	case OpGCRA:
		return emit.StateOpGCRA
	case OpQuota:
		return emit.StateOpQuota
	case OpCacheGet:
		return emit.StateOpCacheGet
	case OpCacheSet:
		return emit.StateOpCacheSet
	case OpCacheInvalidate:
		return emit.StateOpCacheInvalidate
	case OpRefund:
		return emit.StateOpRefund
	default:
		return -1
	}
}

// WriteLabel returns the index of a post-commit write in
// emit.StateMetrics.WritesDropped (catalog.WriteKinds order), or -1 for an
// op that is not a post-commit write.
func (o OpKind) WriteLabel() int {
	switch o {
	case OpRefund:
		return emit.WriteRefund
	case OpCacheSet:
		return emit.WriteCacheSet
	case OpCacheInvalidate:
		return emit.WriteCacheInvalidate
	default:
		return -1
	}
}

// RoundTrip labels one round trip: a single op, script_multi or pipeline.
type RoundTrip uint8

// Round trip labels beyond single ops.
const (
	TripSingle RoundTrip = iota
	TripScriptMulti
	TripPipeline
)

// Label returns the op label index of a round trip: script_multi, pipeline,
// or single's own label for TripSingle.
func (t RoundTrip) Label(single OpKind) int {
	switch t {
	case TripScriptMulti:
		return emit.StateOpScriptMulti
	case TripPipeline:
		return emit.StateOpPipeline
	default:
		return single.Label()
	}
}

// Result is the ruralz_state_calls_total result label; its values are the
// emit.StateResult* indexes.
type Result uint8

// Results.
const (
	ResultOK      Result = emit.StateResultOK
	ResultError   Result = emit.StateResultError
	ResultTimeout Result = emit.StateResultTimeout
	ResultSkipped Result = emit.StateResultSkipped
)

// GCRALimit is one ratelimit limits[] entry; tau = Burst*Window/Requests.
type GCRALimit struct {
	Requests int64
	Window   time.Duration
	Burst    int64
}

// GCRAOutcome is one limit's state after the decision.
type GCRAOutcome struct {
	Remaining  int64
	ResetAfter time.Duration
}

// GCRA is one ratelimit Policy's call: every limit in one script, all or
// nothing, with the server's clock.
type GCRA struct {
	Policy string
	Digest Digest
	Limits []GCRALimit
	// Out is caller-provided with len(Limits).
	Out []GCRAOutcome

	Allowed    bool
	Denied     int // index of the limit setting RetryAfter; -1 when allowed
	RetryAfter time.Duration
	ServerNow  time.Time
}

// Quota reserves one unit of a Consumer quota window.
type Quota struct {
	Name   string
	Window time.Duration
	Limit  int64
	Digest Digest

	Allowed     bool
	WindowStart time.Time
	Used        int64
	RetryAfter  time.Duration
	ServerNow   time.Time
}

// CacheKey locates a Response Cache partition: SHA-256 of the URI and of
// the partition value.
type CacheKey struct{ URI, Partition Digest }

// CacheLookup is the pipelined lookup: GET generation, HMGET partition.
type CacheLookup struct {
	Key     CacheKey
	Variant Digest

	Generation int64
	Names      string
	Found      bool
	EntryGen   int64
	// Entry is opaque and MAC-verified; valid until the Call is reused.
	Entry []byte
}

// Call is one Policy's blocking operation and outcome; callers own it.
// Exactly one operation field is used, selected by Kind.
type Call struct {
	Kind    OpKind
	Timeout time.Duration
	GCRA    GCRA
	Quota   Quota
	Lookup  CacheLookup

	// Done is true when the store decided (false after an earlier deny).
	Done bool
	// Err is nil when the store answered, else one of the sentinels.
	Err *Error
	// Elapsed is the round-trip time; 0 when not attempted or memory.
	Elapsed time.Duration
	// Batch is the index of the call whose span records a shared round
	// trip, or -1.
	Batch int
	// Trip labels the round trip.
	Trip RoundTrip
}

// Refund returns one unit to the window a reservation charged.
type Refund struct {
	Name        string
	Window      time.Duration
	WindowStart time.Time
	Digest      Digest
}

// CacheStore stores one variant under the partition's fill lease.
type CacheStore struct {
	Key        CacheKey
	Names      string
	Variant    Digest
	Generation int64
	TTL        time.Duration
	Entry      []byte
	Token      uint64
}

// LeaseMode selects a revalidation lease operation.
type LeaseMode uint8

// Lease modes.
const (
	LeaseTake LeaseMode = iota + 1
	LeaseRefresh
	LeaseEndStale
)

// CacheLease takes, uses or ends the partition lease.
type CacheLease struct {
	Key     CacheKey
	Variant Digest
	Mode    LeaseMode
	Token   uint64
	Entry   []byte
	TTL     time.Duration
}

// Write is one post-commit write.
type Write struct {
	Kind       OpKind // OpRefund, OpCacheSet, OpCacheInvalidate
	Timeout    time.Duration
	Refund     Refund
	Cache      CacheStore
	Lease      CacheLease
	Invalidate Digest // URI digest for OpCacheInvalidate

	Err     *Error
	Applied bool
}

// WriteClass is a post-commit queue class.
type WriteClass uint8

// Queue classes in writer turn order.
const (
	ClassSettle WriteClass = iota + 1 // Quota refunds (Token Budget settlement M3)
	ClassInvalidate
	ClassStore
	ClassPlugin // M2; empty in M1
)

// Enqueuer is the bounded post-commit queue. Enqueue never blocks and
// never allocates on the drop path; false means dropped and counted.
type Enqueuer interface {
	Enqueue(class WriteClass, w *Write) bool
}

// Capability is a command set a Policy may need (RZ-STS-005).
type Capability uint8

// Capabilities.
const (
	CapScripts Capability = 1 << iota
	CapVectorSets
	CapValkeySearch
)

// Store is what Filters use. Implementations are safe for concurrent use
// and never block beyond the computed timeouts.
type Store interface {
	// Consume runs, as one round trip, the longest prefix of calls whose
	// keys share a hash slot (one script; script_multi when longer than
	// one), in order, stopping at the first deny. It returns how many calls
	// it took (at least 1); calls after a deny stay !Done.
	Consume(ctx context.Context, rb *RequestBudget, calls []*Call) int
	// Read runs independent read-only calls as one pipelined batch.
	Read(ctx context.Context, rb *RequestBudget, calls []*Call)
	// Write sends one post-commit batch (queue writers only).
	Write(ctx context.Context, batch []*Write)
	// AdmitStoreBytes applies the Response Cache memory rules to key's shard.
	AdmitStoreBytes(key CacheKey, n int) bool
	// Supports reports whether the deployment offers c.
	Supports(c Capability) bool
	// RoundTrips is false for memory: its time stays gateway-added.
	RoundTrips() bool
}

// Status feeds degraded reasons and the cleartext gauge.
type Status struct {
	BreakerNotClosed bool
	EvictionPolicy   Tristate
	Cleartext        bool
	Unauthenticated  bool
}

// Tristate is yes, no or unknown.
type Tristate uint8

// Tristate values.
const (
	Unknown Tristate = iota
	Yes
	No
)

// Driver is a live Store with a lifecycle, owned by the Manager.
type Driver interface {
	Store
	Status() Status
	Close(ctx context.Context) error
}

// Failure classifies an unanswered call.
type Failure uint8

// Failures.
const (
	FailTimeout      Failure = iota + 1 // RZ-STS-001, result timeout
	FailError                           // RZ-STS-002, result error
	FailBreakerOpen                     // RZ-STS-003, result skipped
	FailNotAttempted                    // RZ-STS-004, result skipped
	FailUnsupported                     // RZ-STS-005, result skipped
)

// Error is a State Store failure; the five sentinels are the only values,
// so failures never allocate. Causes are logged, never shown to clients.
type Error struct{ failure Failure }

// Sentinel failures.
var (
	ErrTimeout      = &Error{FailTimeout}
	ErrFailed       = &Error{FailError}
	ErrBreakerOpen  = &Error{FailBreakerOpen}
	ErrNotAttempted = &Error{FailNotAttempted}
	ErrUnsupported  = &Error{FailUnsupported}
)

// Error returns the code and meaning.
func (e *Error) Error() string { return e.Code() + ": state store call failed" }

// Failure returns the classification.
func (e *Error) Failure() Failure { return e.failure }

// Code returns RZ-STS-001 to RZ-STS-005.
func (e *Error) Code() string {
	switch e.failure {
	case FailTimeout:
		return "RZ-STS-001"
	case FailError:
		return "RZ-STS-002"
	case FailBreakerOpen:
		return "RZ-STS-003"
	case FailNotAttempted:
		return "RZ-STS-004"
	default:
		return "RZ-STS-005"
	}
}

// Result returns the metric result label.
func (e *Error) Result() Result {
	switch e.failure {
	case FailTimeout:
		return ResultTimeout
	case FailError:
		return ResultError
	default:
		return ResultSkipped
	}
}

// RequestBudget is pack 8.7 rule 2's per-request deadline: it starts at
// the request's first blocking State Store call and lasts the Route's
// largest stateStoreTimeout. Policies of one request run sequentially, so
// it needs no lock. It also carries the request's metric stripe, which
// drivers use for emit.StateMetrics. The zero value means no State Store
// Policy.
type RequestBudget struct {
	max      time.Duration
	deadline time.Time
	stripe   emit.Stripe
}

// NewRequestBudget returns a budget of routeMax (compile-time RouteMax)
// for a request recorded on stripe s.
func NewRequestBudget(routeMax time.Duration, s emit.Stripe) RequestBudget {
	return RequestBudget{max: routeMax, stripe: s}
}

// Stripe returns the request's metric stripe.
func (b *RequestBudget) Stripe() emit.Stripe { return b.stripe }

// CallTimeout starts the budget on first use and returns
// min(policy, budget left, ctx deadline left); ok is false when spent
// (RZ-STS-004).
func (b *RequestBudget) CallTimeout(ctx context.Context, now time.Time, policy time.Duration) (time.Duration, bool) {
	if b.max <= 0 || policy <= 0 {
		return 0, false
	}
	if b.deadline.IsZero() {
		b.deadline = now.Add(b.max)
	}
	t := min(policy, b.deadline.Sub(now))
	if d, ok := ctx.Deadline(); ok {
		t = min(t, d.Sub(now))
	}
	return t, t > 0
}

// RouteMax returns the largest effective stateStoreTimeout of the Route's
// State Store Policies (ratelimit, quota, cache in M1).
func RouteMax(timeouts []time.Duration) time.Duration {
	var m time.Duration
	for _, t := range timeouts {
		m = max(m, t)
	}
	return m
}

// Limits are the fixed bounds (OQ-scalability-and-distributed-state-4 (a));
// only tests override them.
type Limits struct {
	InFlight                       int
	PipelinedPerShard              int
	DedicatedPerShard              int
	BreakerWindow                  time.Duration
	BreakerMinCalls                int
	BreakerFailureRatio            float64
	BreakerConnectFailures         int
	BreakerOpenMin, BreakerOpenMax time.Duration
	BreakerCloseAfter              int
	ReconnectBase, ReconnectCap    time.Duration
	ConnectsPerSecond              int
	ShardsRefresh                  time.Duration
	MemoryPollSlow, MemoryPollFast time.Duration
}

// DefaultLimits returns the target values of the scalability document.
func DefaultLimits() Limits {
	return Limits{
		InFlight: 8192, PipelinedPerShard: 2, DedicatedPerShard: 8,
		BreakerWindow: 5 * time.Second, BreakerMinCalls: 20, BreakerFailureRatio: 0.5, BreakerConnectFailures: 5,
		BreakerOpenMin: time.Second, BreakerOpenMax: 3 * time.Second, BreakerCloseAfter: 3,
		ReconnectBase: 100 * time.Millisecond, ReconnectCap: 5 * time.Second, ConnectsPerSecond: 4,
		ShardsRefresh: 10 * time.Second, MemoryPollSlow: 10 * time.Second, MemoryPollFast: time.Second,
	}
}

// Deps are shared by drivers.
type Deps struct {
	Clock   clock.Clock
	Metrics *emit.StateMetrics
	Status  emit.NodeStatus
	NodeID  string
	Limits  Limits
	// MACKey is the optional entry MAC key read from
	// RURALZ_STATE_STORE_MAC_KEY_FILE (at least 32 bytes); nil stores and
	// accepts entries without a tag. A wrong or missing tag is a miss.
	MACKey []byte
	// Logger is from internal/telemetry; shards appear as ss-<8 hex>, never
	// as the URL.
	Logger *slog.Logger
}

// Opener constructs a driver without dialing synchronously; it fails only
// for an invalid configuration (RZ-CFG-026 reasons).
type Opener func(ctx context.Context, cfg Config, deps Deps) (Driver, error)
