// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package balance selects Upstream Endpoints: the four
// loadBalancing.algorithm structures (round-robin schedule, least-request,
// ring-hash ring, random), the ejection-aware selection order of one
// attempt, the weighted split of a Route's plain upstreams entries, the
// throttled copy-on-write rebuild of balancer structures and the Node's
// balancer memory budget plan (spec 05 group C, requirements 10 to 17;
// docs/architecture/09-traffic-management-and-resilience.md "Load
// balancing").
//
// The package imports only the standard library. The Upstream runtime
// (internal/gateway/upstream) owns the per-Endpoint state and supplies it
// through View: health and ejection (internal/gateway/upstream/health), the
// Endpoints tried in the current leg, stall suspicion and the per-Endpoint
// in-flight cap. Per attempt it loads one Set (Balancer.Load) and builds
// the View from that Set's payload, its per-Endpoint state published with
// the list, so the View and the chosen index always refer to the same list.
// It evaluates the hashKey CEL expression and passes HashKey of the result
// to Set.Select; on a hashKey runtime error it calls Set.SelectRandom (05
// req 14). It maps ErrNoEndpoints to 503 RZ-UP-008, raises the panic and
// balancer_budget degraded reasons, and turns Plan warnings into
// configuration warnings (OQ-traffic-management-and-resilience-23 (b)).
//
// Selection per attempt (05 req 11 steps a to d) grades every candidate
// Endpoint: Eligible, Avoided (tried in this leg, suspect or capped; step
// c), Down (passively ejected or actively unhealthy; step b) or
// DownAvoided. Each algorithm walks its structure in its own order and
// returns the first Endpoint of the lowest grade present, which is exactly
// "drop Down Endpoints unless none remains (panic mode), then exclude
// Avoided ones while another remains".
//
// Pickers and Sets are immutable once built and shared by every request
// goroutine; a Balancer publishes Sets copy-on-write through an atomic
// pointer, so selection takes no lock and allocates nothing. The package
// starts no goroutine: the Upstream manager's scheduler calls Rebuild with
// a BuildGate that bounds concurrent builds per Node.
//
// Endpoint weights follow one rule everywhere: an Endpoint of weight 0 is
// never picked while another Endpoint weighs more than 0, and when every
// weight is 0 they all weigh 1 (the SRV weight rule of 05 req 6 applied to
// static weights too).
package balance
