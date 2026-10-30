// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminapi

// Endpoint source types of /debug/upstreams.
const (
	SourceStatic = "static"
	SourceDNS    = "dns"
)

// Circuit breaker states of /debug/upstreams.
const (
	BreakerClosed   = "closed"
	BreakerOpen     = "open"
	BreakerHalfOpen = "half_open"
)

// Upstreams is the /debug/upstreams body: one entry per Upstream runtime a
// live snapshot holds, in name order, so [] before the first activation.
// Times are RFC 3339 UTC, or null when unset; lists are [] when empty,
// never null.
type Upstreams struct {
	Upstreams []UpstreamInfo `json:"upstreams"`
}

// UpstreamInfo is one Upstream runtime on the Node: its Endpoint source,
// circuit breaker, bulkhead, retry budget, panic mode, balancer structures
// and Endpoints.
type UpstreamInfo struct {
	// Name is the Upstream's metadata.name.
	Name string `json:"name"`
	// Source is the active Endpoint source's refresh state.
	Source SourceInfo `json:"source"`
	// Breaker is the circuit breaker's state and rolling-window counts.
	Breaker BreakerInfo `json:"breaker"`
	// InFlight counts the attempts holding a bulkhead slot, response bodies
	// included.
	InFlight int `json:"inFlight"`
	// Pending counts the attempts waiting for a bulkhead slot.
	Pending int `json:"pending"`
	// RetryBudget counts the legs and retries the retry budget holds.
	RetryBudget RetryBudgetInfo `json:"retryBudget"`
	// Panic reports panic mode: no healthy Endpoint, so selection uses every
	// Endpoint.
	Panic bool `json:"panic"`
	// Balancers are the published balancer structures, one per algorithm the
	// live snapshots use.
	Balancers []BalancerInfo `json:"balancers"`
	// Endpoints are the current Endpoints in identity order.
	Endpoints []EndpointInfo `json:"endpoints"`
}

// SourceInfo is the refresh state of an Upstream's Endpoint source.
type SourceInfo struct {
	// Type is SourceStatic or SourceDNS.
	Type string `json:"type"`
	// LastRefresh is when the last refresh with a good answer ended; null
	// before one.
	LastRefresh *string `json:"lastRefresh"`
	// Stale is true while the last refresh failed and the set is the last
	// good one (degraded reason discovery_stale).
	Stale bool `json:"stale"`
	// Failures counts consecutive failed refreshes.
	Failures int `json:"failures"`
	// Error is the last refresh failure; null after a good answer.
	Error *string `json:"error"`
}

// BreakerInfo is the state of an Upstream's circuit breaker.
type BreakerInfo struct {
	// State is BreakerClosed, BreakerOpen or BreakerHalfOpen.
	State string `json:"state"`
	// OpenedAt is when the breaker last opened; it stays set while
	// half-open and is null while closed.
	OpenedAt *string `json:"openedAt"`
	// OpenUntil is when an open breaker lets its probe through; null when
	// not open.
	OpenUntil *string `json:"openUntil"`
	// WindowLegs counts the legs of the rolling window ending now.
	WindowLegs int `json:"windowLegs"`
	// WindowFailures counts the failed legs among WindowLegs.
	WindowFailures int `json:"windowFailures"`
	// ConsecutiveFailures is the current run of failed legs; every
	// transition resets it.
	ConsecutiveFailures int `json:"consecutiveFailures"`
}

// RetryBudgetInfo is the use of an Upstream's retry budget.
type RetryBudgetInfo struct {
	// Originals counts the legs in flight.
	Originals int `json:"originals"`
	// Retries counts the retries in flight, backoff included.
	Retries int `json:"retries"`
}

// BalancerInfo describes one published balancer structure.
type BalancerInfo struct {
	// Algorithm is the configured loadBalancing.algorithm.
	Algorithm string `json:"algorithm"`
	// Effective is the algorithm serving picks: random for a ring-hash
	// Upstream that fell back for lack of balancer budget.
	Effective string `json:"effective"`
	// VirtualNodes is the ring's virtual nodes per Endpoint; 0 without a
	// ring.
	VirtualNodes int `json:"virtualNodes"`
	// Fallback reports a ring-hash Upstream running weighted random for lack
	// of budget (degraded reason balancer_budget).
	Fallback bool `json:"fallback"`
	// Bytes is the structure's budgeted size.
	Bytes int64 `json:"bytes"`
	// Endpoints is the size of the current Endpoint set.
	Endpoints int `json:"endpoints"`
	// Built is when the published structure was built.
	Built *string `json:"built"`
	// Stale reports that the Endpoint set or plan changed since the
	// structure was built; a rebuild follows.
	Stale bool `json:"stale"`
}

// EndpointInfo is the state of one Endpoint.
type EndpointInfo struct {
	// Identity is the Endpoint's stable name: the configured address, the
	// ip:port of an A or AAAA answer or the target:port of an SRV answer.
	Identity string `json:"identity"`
	// Weight is the effective weight.
	Weight uint32 `json:"weight"`
	// Healthy is the active-check verdict, true without active checks.
	Healthy bool `json:"healthy"`
	// Ejected reports a passive ejection in force.
	Ejected bool `json:"ejected"`
	// EjectedUntil is when the ejection ends; null when not ejected.
	EjectedUntil *string `json:"ejectedUntil"`
	// Ejections is the current ejection count, which multiplies the
	// ejection time.
	Ejections int `json:"ejections"`
	// ConsecutiveErrors is the current run of failed attempts.
	ConsecutiveErrors int `json:"consecutiveErrors"`
	// Suspect reports an Endpoint holding a stalled attempt without
	// response headers, which selection skips while another remains.
	Suspect bool `json:"suspect"`
	// Stalled counts the Endpoint's stalled attempts.
	Stalled int `json:"stalled"`
	// Failures1s counts the attempts matching failureWhen in the last
	// second.
	Failures1s int64 `json:"failures1s"`
	// InFlight counts the attempts holding a slot on the Endpoint.
	InFlight int64 `json:"inFlight"`
	// Addresses are the Endpoint's cached ip:port values in answer order.
	Addresses []string `json:"addresses"`
}
