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
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/secret"
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

// Topologies: one endpoint that follows the primary, a Redis Cluster seed
// list, or a Redis Sentinel seed list naming a master set.
const (
	TopologyStandalone Topology = iota + 1
	TopologyCluster
	TopologySentinel
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
	Role     Role
	Driver   DriverKind
	Topology Topology
	URL      string
	// TLS is the connection's TLS block: Gateway spec.stateStore.tls for
	// the main connection, spec.stateStore.cache.tls for the cache; nil
	// without it.
	TLS            *TLS
	DefaultTimeout time.Duration
	Source         Source
}

// TLS is the client TLS setting of one redis connection, Gateway
// spec.stateStore.tls or spec.stateStore.cache.tls: references converted
// by secret.RefOf, each nil when unset, and the Revision's resolved
// values. A driver builds its TLS
// configuration from them at Open and follows their rotations, which reach
// new connections only.
type TLS struct {
	CACertificate     *secret.Ref
	ClientCertificate *secret.Ref
	ClientKey         *secret.Ref
	// Secrets holds the resolved values; nil fails Open.
	Secrets secret.Store
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
	// Burst is used as given: tau = Burst × Window / Requests, so 0 means
	// no burst tolerance (after the first admission, one per emission
	// interval T). Drivers never default it. The ratelimit Filter sets
	// Burst = Requests when config.limits[].burst is unset; v1alpha1 Burst
	// is *int64 with no schema default.
	Burst int64
}

// Cost returns the units a GCRA or Quota call charges and a Refund
// returns: c, or 1 for 0; ok is false for a negative c, which drivers
// answer with RZ-STS-002.
func Cost(c int64) (cost int64, ok bool) {
	switch {
	case c < 0:
		return 0, false
	case c == 0:
		return 1, true
	default:
		return c, true
	}
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
	// Cost is the request's config.cost, the emission intervals the call
	// charges every limit: a limit conforms when TAT - now is at most
	// τ - (Cost - 1) × T, so all Cost units would conform, and an allow
	// advances each TAT by Cost × T. 0 counts as 1; a negative Cost is an
	// error reply (RZ-STS-002).
	Cost int64
	// Out is caller-provided with len(Limits).
	Out []GCRAOutcome

	Allowed    bool
	Denied     int // index of the limit setting RetryAfter; -1 when allowed
	RetryAfter time.Duration
	ServerNow  time.Time
}

// Quota reserves Cost units of one Consumer quota window: the fixed,
// epoch-aligned Window, or the UTC calendar month when Month is set
// (docs/architecture/09-traffic-management-and-resilience.md "Quotas").
// The server's clock picks the window from the Node's two candidates
// (QuotaCandidates).
type Quota struct {
	Name   string
	Window time.Duration
	// Month counts the UTC calendar month instead of Window, which is
	// then ignored.
	Month bool
	Limit int64
	// Cost is the units the call reserves: a window whose count plus Cost
	// is at most Limit admits it and reserves all of them, otherwise it
	// denies it and reserves nothing. 0 counts as 1; a negative Cost is
	// an error reply (RZ-STS-002).
	Cost   int64
	Digest Digest

	Allowed     bool
	WindowStart time.Time
	// WindowEnd is when the charged window closes.
	WindowEnd time.Time
	// Used is the window's count after the decision: Cost included on an
	// admission, as found on a denial.
	Used int64
	// RetryAfter is the time to WindowEnd on a denial, 0 otherwise.
	RetryAfter time.Duration
	ServerNow  time.Time
}

// QuotaWindow is one Quota window, [Start, End) in Unix milliseconds.
type QuotaWindow struct{ Start, End int64 }

// QuotaWindowOf returns the window holding ms: the epoch-aligned window of
// length w, or with month the UTC calendar month, which starts at 00:00 UTC
// on its first day. w is at least 1 ms unless month is set.
func QuotaWindowOf(ms int64, w time.Duration, month bool) QuotaWindow {
	if month {
		t := time.UnixMilli(ms).UTC()
		start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		return QuotaWindow{Start: start.UnixMilli(), End: start.AddDate(0, 1, 0).UnixMilli()}
	}
	wm := w.Milliseconds()
	start := ms / wm * wm
	if ms%wm < 0 {
		start -= wm
	}
	return QuotaWindow{Start: start, End: start + wm}
}

// QuotaCandidates returns the windows a Node offers a Quota call for its
// clock reading nodeMs: its own window A and the nearer neighbor B, the
// window before A when nodeMs lies in A's first half (2 × (nodeMs -
// A.Start) < A.End - A.Start), else the one after. The server charges the
// candidate holding its own clock, so Node clock skew up to half a window,
// half the shorter month for a calendar month, does not matter.
func QuotaCandidates(nodeMs int64, w time.Duration, month bool) (a, b QuotaWindow) {
	a = QuotaWindowOf(nodeMs, w, month)
	if 2*(nodeMs-a.Start) < a.End-a.Start {
		return a, QuotaWindowOf(a.Start-1, w, month)
	}
	return a, QuotaWindowOf(a.End, w, month)
}

// CacheKey locates a Response Cache partition: SHA-256 of the URI and of
// the partition value.
type CacheKey struct{ URI, Partition Digest }

// Token is a Response Cache generation token
// (docs/architecture/11-scalability-and-distributed-state.md "Response
// Cache layout"): 128 random bits the Node draws for each creation and
// each bump of a URI's generation key, which holds them as 32 lowercase
// hex characters. Every entry records the token it was written under, and
// a lookup hits only while the generation key holds it, so an entry never
// outlives an invalidation, an eviction or an expiry of its key. A counter
// or a time could repeat once the key is gone; 128 random bits do not. The
// Node never draws the zero Token, which therefore means "none".
type Token [16]byte

// IsZero reports the zero Token.
func (t Token) IsZero() bool { return t == Token{} }

// CacheLookup is the pipelined lookup of one variant: a GET of the URI's
// generation key, an HMGET of the partition's names n, the variant's store
// time t:<V> and its entry e:<V>, and TIME on the partition's shard; read
// only, so it never runs a script.
type CacheLookup struct {
	Key     CacheKey
	Variant Digest
	// SkipToken leaves the generation key unread: the Node's hot-entry
	// layer holds its token, read within the last second, in Token and
	// HasToken, which the driver keeps.
	SkipToken bool

	// HasToken reports that the generation key exists and Token is the
	// token it holds.
	Token    Token
	HasToken bool
	// Partition reports that the partition exists (it holds n), and Names
	// is n: the Vary names the partition was stored under, "" without
	// Vary.
	Partition bool
	Names     string
	// Found reports the variant's entry. Entry is opaque and MAC-verified;
	// valid until the Call is reused.
	Found bool
	Entry []byte
	// Resident is how long the variant has been stored, on the State
	// Store's clock: the partition shard's TIME minus t:<V>, never
	// negative, so Node clock skew never changes an age.
	Resident time.Duration
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

// Refund returns a reservation's units to the window it charged, never
// taking the count below 0 and never creating a key.
type Refund struct {
	Name   string
	Window time.Duration
	// Month names a UTC calendar month window, as Quota.Month does.
	Month       bool
	WindowStart time.Time
	// Units is the reserved Cost to return; 0 counts as 1, and a negative
	// Units is an error reply (RZ-STS-002).
	Units  int64
	Digest Digest
}

// CacheStore stores one variant under the partition's fill lease, in two
// steps, each one script: the generation step on the generation key's
// shard, then, only when it applied, the partition step on the partition's
// shard. The generation step checks that the URI's generation key still
// holds Generation and refreshes its 26 h TTL, or, with Create, creates
// the key holding Generation (SET NX) and fails when a key appeared
// meanwhile; the partition step takes the 1 s fill lease (SET NX PX) and
// writes only under it. The two keys hash to different Redis Cluster slots,
// so one script cannot hold both; since an entry hits only while the
// generation key holds the token it records, an invalidation between the
// steps leaves the new entry unservable, never stale. Applied reports that
// both steps applied.
type CacheStore struct {
	Key     CacheKey
	Names   string
	Variant Digest
	// Generation is the token the entry is written under: the one the
	// store's lookup read, or, with Create, a fresh one.
	Generation Token
	// Create reports that the store's lookup found no generation key: the
	// generation step creates it holding Generation.
	Create bool
	TTL    time.Duration
	Entry  []byte
	// Received is when the response arrived, on the Node clock: the
	// partition step records t:<V> as its script's TIME minus the time the
	// write waited since then, which the driver passes.
	Received time.Time
	// Token is the fill lease holder's value.
	Token uint64
}

// LeaseMode selects a revalidation lease operation.
type LeaseMode uint8

// Lease modes.
const (
	LeaseTake LeaseMode = iota + 1
	LeaseRefresh
	LeaseEndStale
)

// CacheLease takes, uses or ends the partition's revalidation lease.
// LeaseRefresh and LeaseEndStale act only while the lease holds Token and
// the URI's generation key holds Generation, the token the entry was
// written under, and neither releases the lease, which expires after its
// 5 s. Like a store, each runs a generation step (a check, which a refresh
// follows with the key's 26 h TTL) on the generation key's shard, then,
// only when it applied, the lease step on the partition's shard.
type CacheLease struct {
	// Key is the partition the lease guards.
	Key CacheKey
	// Variant is the variant LeaseRefresh rewrites and LeaseEndStale
	// deletes; LeaseTake ignores it.
	Variant Digest
	// Mode selects the operation.
	Mode LeaseMode
	// Token identifies the holder: LeaseTake stores it, and LeaseRefresh
	// and LeaseEndStale act only while the lease still holds it.
	Token uint64
	// Generation is the token the variant's entry was written under;
	// LeaseTake ignores it.
	Generation Token
	// Entry is the variant's new entry, LeaseRefresh only: the stored
	// response freshened by a 304, or a full response that replaces it.
	Entry []byte
	// TTL applies only to LeaseRefresh, where it extends the partition as
	// CacheStore.TTL does (the larger of the remaining TTL and the entry
	// TTL, capped at 25 h). LeaseTake ignores it, because the revalidation
	// lease is fixed at 5 s (SET NX PX 5000); LeaseEndStale ignores it
	// too.
	TTL time.Duration
	// Received is when the 304 or the full response arrived, on the Node
	// clock, LeaseRefresh only: t:<V> becomes the script's TIME minus the
	// time the write waited since then.
	Received time.Time
}

// Write is one post-commit write.
type Write struct {
	Kind OpKind // OpRefund, OpCacheSet, OpCacheInvalidate
	// Timeout is the effective stateStoreTimeout of the Policy that
	// enqueued the write (the per-request RequestBudget does not apply).
	// The enqueuing Filter sets it from BuildEnv.StateStoreTimeout: quota
	// refunds, cache stores and invalidations, revalidation lease writes.
	// Zero means the write is not attempted, and drivers answer it with
	// ErrNotAttempted, RZ-STS-004. The post-commit queue passes it
	// unchanged.
	Timeout    time.Duration
	Refund     Refund
	Cache      CacheStore
	Lease      CacheLease
	Invalidate Digest // URI digest for OpCacheInvalidate
	// Bump is the fresh token OpCacheInvalidate sets the URI's generation
	// key to (SET with the 26 h TTL), so no entry written under an earlier
	// token hits again.
	Bump Token

	Err     *Error
	Applied bool
}

// ReceiptDelay returns how long a write has waited since its response
// arrived at received, on the Node clock now: the delay a store, refresh
// or replacement passes so its script records t:<V> as its TIME minus it.
// A zero received, or a clock that went back, is no delay.
func ReceiptDelay(received, now time.Time) time.Duration {
	if received.IsZero() {
		return 0
	}
	return max(now.Sub(received), 0)
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
	// Write sends one batch of writes off the request path: a post-commit
	// queue writer's batch, or a Response Cache revalidation worker's lease
	// take, refresh or end-stale write, which needs its answer within the
	// lease and so never queues.
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
	// EvictionPolicy reports maxmemory-policy noeviction. Yes: every
	// shard's latest INFO memory reading reports noeviction. No: any shard
	// reports another policy; state_store_eviction_policy is raised while
	// a ratelimit or quota Policy uses this store. Unknown: no shard
	// reports another policy, but some reading lacks the field or has not
	// arrived; no reason is raised and the driver logs one WARN. The
	// precedence is No, then Unknown, then Yes. The memory driver reports
	// Yes.
	EvictionPolicy Tristate
	// CacheEvictionPolicy reports a maxmemory-policy a Response Cache
	// connection tolerates, noeviction or volatile-ttl
	// (docs/architecture/09-traffic-management-and-resilience.md "Response
	// caching"): Yes when every shard's latest reading reports one of them,
	// No when any shard reports another policy, and Unknown otherwise, with
	// EvictionPolicy's precedence; state_store_eviction_policy is raised
	// while a cache Policy uses a store reporting No. The memory driver
	// reports Yes.
	CacheEvictionPolicy Tristate
	Cleartext           bool
	Unauthenticated     bool
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
	// InFlight is the Node's in-flight ceiling, shared by every driver the
	// Manager opens; nil makes a driver keep its own of Limits.InFlight.
	InFlight *InFlight
}

// InFlight is the Node-wide ceiling on State Store calls in flight
// (Limits.InFlight, 8,192): a call holds one slot from its send until its
// reply or a connection reset, not until its caller's timeout. It is safe
// for concurrent use.
type InFlight struct {
	n     atomic.Int64
	limit int64
}

// NewInFlight returns a ceiling of limit slots; limit 0 or below takes
// DefaultLimits().InFlight.
func NewInFlight(limit int) *InFlight {
	if limit <= 0 {
		limit = DefaultLimits().InFlight
	}
	return &InFlight{limit: int64(limit)}
}

// TryAcquire takes a slot; false means the ceiling is full and the call is
// skipped (RZ-STS-004).
func (g *InFlight) TryAcquire() bool {
	for {
		n := g.n.Load()
		if n >= g.limit {
			return false
		}
		if g.n.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// Release returns a slot TryAcquire took.
func (g *InFlight) Release() { g.n.Add(-1) }

// Len returns the slots in use.
func (g *InFlight) Len() int { return int(g.n.Load()) }

// Opener constructs a driver without dialing synchronously; it fails only
// for an invalid configuration: a URL that does not fit its topology or a
// TLS value that does not resolve (RZ-CFG-026), a URL the transport rules
// refuse (RZ-CFG-042), or a client certificate without its key
// (RZ-CFG-005).
type Opener func(ctx context.Context, cfg Config, deps Deps) (Driver, error)
