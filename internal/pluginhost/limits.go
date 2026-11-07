// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"time"
)

// Node defaults of docs/architecture/05-wasm-plugin-system.md "Default
// limits" (targets): limits.memoryBytes and limits.timeout are the only
// per-Plugin limit fields; the rest are Node defaults.
const (
	// DefaultMemoryBytes is limits.memoryBytes when unset.
	DefaultMemoryBytes = 16 << 20
	// DefaultTimeout is limits.timeout when unset.
	DefaultTimeout = 5 * time.Millisecond
	// DefaultNodeCap is Gateway limits.maxPluginMemoryBytes when unset.
	DefaultNodeCap = 2 << 30
	// pageBytes is one WebAssembly page.
	pageBytes = 64 << 10

	// callBudget is the Host Function calls one Phase call may make; the
	// next one returns ERR_BUDGET.
	callBudget = 1000
	// poolWait is the longest a Phase call waits for an idle instance.
	poolWait = time.Millisecond
	// poolShare and statePoolShare are a pool's share of the Node cap, in
	// percent: 25%, and 50% for a pool whose Plugin holds state.read or
	// state.write.
	poolShare      = 25
	statePoolShare = 50
	// swapHeadroom is the percentage of the Node cap that replacement
	// pools may reserve beyond it while a swap warms them.
	swapHeadroom = 10
	// idlePercent: the pool manager keeps idle at least max(1, 25% of
	// busy).
	idlePercent = 25
	// warmMinimum is the instances a pool warms before its snapshot
	// publishes.
	warmMinimum = 1
	// parkedBoundLow is the guaranteed parked bound below which a state.*
	// pool raises plugin_parked_bound_low.
	parkedBoundLow = 16

	// instantiateTimeout bounds instantiating an instance and its
	// rz_configure call.
	instantiateTimeout = 100 * time.Millisecond
	// refillPerSecond caps the refills of faulted instances per pool.
	refillPerSecond = 10
	// recycleAfter is the calls after which an instance is replaced.
	recycleAfter = 100_000

	// breakerWindow, breakerMinCalls, breakerFaultPercent and breakerOpen
	// are the fault breaker: faults above 50% of at least 200 calls in a
	// rolling 10 s trip it for 5 s, then half-open lets 5% of calls reach
	// the guest.
	breakerWindow       = 10 * time.Second
	breakerMinCalls     = 200
	breakerFaultPercent = 50
	breakerOpen         = 5 * time.Second
	breakerProbePercent = 5

	// compiledCodeLimit caps the accounted compiled code per Node.
	compiledCodeLimit = 512 << 20
	// codeFactor and codeAllowance are the accounting rule of compiled
	// code per distinct digest, from the wazero study: codeFactor ×
	// binary bytes plus codeAllowance for the two page-rounded mappings
	// of each compile (code and entry preambles) at up to 64 KiB pages.
	codeFactor    = 6
	codeAllowance = 128 << 10

	// coldCompileTimeout bounds one cold compile; a compile past it is
	// abandoned, at most maxAbandoned per Node at once, and retried
	// compileRetries times before the failure is reported.
	coldCompileTimeout = 30 * time.Second
	maxAbandoned       = 1
	compileRetries     = 2

	// logMessageBytes, logCallBytes and logRateBytes cap guest logs: a
	// message, a call, and a Policy per second.
	logMessageBytes = 4 << 10
	logCallBytes    = 16 << 10
	logRateBytes    = 1 << 20

	// contextBytes caps a Plugin Policy's per-request context.
	contextBytes = 4 << 10

	// stateKeyBytes caps a guest state key; ttlMin and ttlMax bound
	// state_incr's ttlMillis; stateKeysPerMinute caps the distinct keys a
	// Policy touches per Node per minute.
	stateKeyBytes      = 256
	ttlMin             = time.Millisecond
	ttlMax             = 24 * time.Hour
	stateKeysPerMinute = 10_000

	// metricNames caps the event names a Plugin Policy may add.
	metricNames = 16
	// metricNameMax is the longest event name.
	metricNameMax = 32
)

// accountedCode returns the compiled code accounted for a binary of n
// bytes, alike on every Node and in the online stage.
func accountedCode(n int) int64 { return codeFactor*int64(n) + codeAllowance }

// poolBounds are the sizing of one pool: its instance limit N, its compute
// bound and, for a state.* pool, its compute reserve R and parked bound
// N − R.
type poolBounds struct {
	n, compute, reserve, parked int
}

// shareOf returns a pool's share of the Node cap.
func shareOf(nodeCap int64, state bool) int64 {
	p := int64(poolShare)
	if state {
		p = statePoolShare
	}
	return nodeCap * p / 100
}

// instanceLimit returns N = floor(share / memoryBytes), 0 when
// memoryBytes exceeds the share (an oversized limits.memoryBytes).
func instanceLimit(nodeCap, memoryBytes int64, state bool) int {
	if memoryBytes <= 0 {
		return 0
	}
	return int(shareOf(nodeCap, state) / memoryBytes)
}

// boundsOf returns the bounds of a pool of N instances on a Node with
// procs GOMAXPROCS and, for a state.* pool, a 1-minute peak computing
// count of peak: compute min(2 × procs, N); R = min(ceil(N/4), max(2,
// ceil(2 × peak))), which never depends on the core count; parked N − R.
func boundsOf(n, procs, peak int, state bool) poolBounds {
	b := poolBounds{n: n, compute: min(2*procs, n)}
	if state {
		b.reserve = min(ceilDiv(n, 4), max(2, 2*peak))
		b.parked = max(n-b.reserve, 0)
	}
	return b
}

// guaranteedParked returns the parked bound a state.* pool of N keeps at
// any peak: N − ceil(N/4).
func guaranteedParked(n int) int { return max(n-ceilDiv(n, 4), 0) }

// ceilDiv returns ceil(a/b) for a ≥ 0 and b > 0.
func ceilDiv(a, b int) int { return (a + b - 1) / b }
