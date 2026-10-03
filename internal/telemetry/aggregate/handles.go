// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Handles are what request-path code holds: one atomic pointer load
// selects the label set (its own, or _overflow once folded; spec 09 req
// 48), then one atomic add records. No handle method locks, allocates or
// blocks.

// counterH is an emit.Counter.
type counterH struct {
	p atomic.Pointer[slot]
	i int
}

// Add implements emit.Counter.
func (h *counterH) Add(s emit.Stripe, n uint64) { h.p.Load().word(s).Add(n) }

// gaugeH is an emit.Gauge.
type gaugeH struct {
	p atomic.Pointer[slot]
	i int
}

// Add implements emit.Gauge.
func (h *gaugeH) Add(s emit.Stripe, d int64) { h.p.Load().word(s).Add(u64(d)) }

// Set implements emit.Gauge.
func (h *gaugeH) Set(v int64) { h.p.Load().set(v) }

// histH is an emit.Histogram.
type histH struct {
	p atomic.Pointer[histSlot]
	i int
}

// Record implements emit.Histogram. It never touches the exemplar slot.
func (h *histH) Record(s emit.Stripe, v uint64) { h.p.Load().record(s, v) }

// RecordExemplar implements emit.Histogram for sampled requests.
func (h *histH) RecordExemplar(s emit.Stripe, v uint64, ex emit.Exemplar) {
	hs := h.p.Load()
	hs.record(s, v)
	hs.ex.store(v, &ex)
}

// statusH is an emit.StatusCounter over five status-class words.
type statusH struct{ p atomic.Pointer[slot] }

// Inc implements emit.StatusCounter.
func (h *statusH) Inc(s emit.Stripe, status int) {
	h.p.Load().wordAt(s, statusClass(status)).Add(1)
}

// attemptsTarget is the attempts layout of one Upstream: the five status
// classes of error="none" (striped when hot) and the four errors connect,
// timeout, reset and tls with status_class="5xx" (unsharded).
type attemptsTarget struct {
	none slot
	errs slot
}

// attemptsH is an emit.UpstreamAttempts.
type attemptsH struct {
	p atomic.Pointer[attemptsTarget]
}

// Inc implements emit.UpstreamAttempts; an attemptErr outside the
// enumeration records nothing.
func (h *attemptsH) Inc(s emit.Stripe, status int, attemptErr int) {
	t := h.p.Load()
	if attemptErr == emit.ErrNone {
		t.none.wordAt(s, statusClass(status)).Add(1)
		return
	}
	if uint(attemptErr) >= emit.NumAttemptErrors {
		return
	}
	t.errs.c.v[t.errs.off+attemptErr-1].Add(1)
}

// listenerReqH is an emit.ListenerRequests over protocol x status_class x
// origin.
type listenerReqH struct{ p atomic.Pointer[slot] }

// Inc implements emit.ListenerRequests; a protocol or origin outside its
// enumeration records nothing.
func (h *listenerReqH) Inc(s emit.Stripe, protocol int, status int, origin int) {
	if uint(protocol) >= emit.NumProtocols || uint(origin) >= emit.NumOrigins {
		return
	}
	i := (protocol*numClasses+statusClass(status))*emit.NumOrigins + origin
	h.p.Load().wordAt(s, i).Add(1)
}

// authTarget is the allow counter and deny code table of one auth group.
type authTarget struct {
	allow slot
	codes *codeTable
}

// authH is an emit.AuthDecisions.
type authH struct{ p atomic.Pointer[authTarget] }

// Allow implements emit.AuthDecisions.
func (h *authH) Allow(s emit.Stripe) { h.p.Load().allow.word(s).Add(1) }

// Deny implements emit.AuthDecisions; the code label set is created on
// its first record.
func (h *authH) Deny(_ emit.Stripe, code string) { h.p.Load().codes.inc(code) }

// codeH is an emit.CodeCounter over a Node-wide code table.
type codeH struct{ t *codeTable }

// Inc implements emit.CodeCounter.
func (h *codeH) Inc(_ emit.Stripe, code string) { h.t.inc(code) }

// retargeter is a handle that can be pointed at another group of the
// same layout (a fold, or a rebind at Bind).
type retargeter interface{ retarget(g *group) }

func (h *counterH) retarget(g *group)     { h.p.Store(&g.slots[h.i]) }
func (h *gaugeH) retarget(g *group)       { h.p.Store(&g.slots[h.i]) }
func (h *histH) retarget(g *group)        { h.p.Store(&g.hslots[h.i]) }
func (h *statusH) retarget(g *group)      { h.p.Store(&g.slots[0]) }
func (h *attemptsH) retarget(g *group)    { h.p.Store(g.attempts) }
func (h *listenerReqH) retarget(g *group) { h.p.Store(&g.slots[0]) }
func (h *authH) retarget(g *group)        { h.p.Store(g.code.auth) }

// handleSet holds the handles of one plan's reference to one group: one
// per label set for value layouts; the one block handle of the status,
// attempts and listener-requests layouts; the allow counter then the auth
// handle of the result-code layout.
type handleSet []retargeter

func (hs handleSet) counter(i int) *counterH    { return hs[i].(*counterH) }
func (hs handleSet) gauge(i int) *gaugeH        { return hs[i].(*gaugeH) }
func (hs handleSet) hist(i int) *histH          { return hs[i].(*histH) }
func (hs handleSet) status() *statusH           { return hs[0].(*statusH) }
func (hs handleSet) attempts() *attemptsH       { return hs[0].(*attemptsH) }
func (hs handleSet) listenerReq() *listenerReqH { return hs[0].(*listenerReqH) }
func (hs handleSet) auth() *authH               { return hs[1].(*authH) }

// newHandleSet returns handles targeting g.
func newHandleSet(g *group) handleSet {
	f := g.fam
	var hs handleSet
	switch f.layout {
	case layoutStatus:
		hs = handleSet{&statusH{}}
	case layoutAttempts:
		hs = handleSet{&attemptsH{}}
	case layoutListenerRequests:
		hs = handleSet{&listenerReqH{}}
	case layoutResultCode:
		hs = handleSet{&counterH{}, &authH{}}
	case layoutValues:
		hs = make(handleSet, g.n)
		for i := range hs {
			switch {
			case f.hist:
				hs[i] = &histH{i: i}
			case f.gauge:
				hs[i] = &gaugeH{i: i}
			default:
				hs[i] = &counterH{i: i}
			}
		}
	case layoutCode, layoutRevisionInfo, layoutComputed:
	}
	hs.retarget(g)
	return hs
}

// retarget points every handle at group g, which has the same layout.
func (hs handleSet) retarget(g *group) {
	for _, h := range hs {
		h.retarget(g)
	}
}

// No-op handles for families a resource never records and for names a
// Plan does not know; zero-size values, so an interface holding one does
// not allocate.
type (
	noopCounter  struct{}
	noopGauge    struct{}
	noopHist     struct{}
	noopStatus   struct{}
	noopAuth     struct{}
	noopAttempts struct{}
	noopListener struct{}
)

// Add implements emit.Counter.
func (noopCounter) Add(emit.Stripe, uint64) {}

// Add implements emit.Gauge.
func (noopGauge) Add(emit.Stripe, int64) {}

// Set implements emit.Gauge.
func (noopGauge) Set(int64) {}

// Record implements emit.Histogram.
func (noopHist) Record(emit.Stripe, uint64) {}

// RecordExemplar implements emit.Histogram.
func (noopHist) RecordExemplar(emit.Stripe, uint64, emit.Exemplar) {}

// Inc implements emit.StatusCounter.
func (noopStatus) Inc(emit.Stripe, int) {}

// Allow implements emit.AuthDecisions.
func (noopAuth) Allow(emit.Stripe) {}

// Deny implements emit.AuthDecisions.
func (noopAuth) Deny(emit.Stripe, string) {}

// Inc implements emit.UpstreamAttempts.
func (noopAttempts) Inc(emit.Stripe, int, int) {}

// Inc implements emit.ListenerRequests.
func (noopListener) Inc(emit.Stripe, int, int, int) {}

// noopListenerMetrics returns listener handles that record nothing.
func noopListenerMetrics() *emit.ListenerMetrics {
	m := &emit.ListenerMetrics{
		Requests:        noopListener{},
		GatewayDuration: noopHist{},
		RequestBody:     noopHist{},
		ResponseBody:    noopHist{},
		Active:          noopGauge{},
		TLSHandshake:    noopHist{},
	}
	for p := range m.OpenConns {
		m.OpenConns[p] = noopGauge{}
		for r := range m.Conns[p] {
			m.Conns[p][r] = noopCounter{}
		}
	}
	return m
}

// noopRouteMetrics returns Route handles that record nothing.
func noopRouteMetrics() *emit.RouteMetrics {
	m := &emit.RouteMetrics{Requests: noopStatus{}, Duration: noopHist{}}
	for i := range m.Cache {
		m.Cache[i] = noopCounter{}
	}
	return m
}

// noopPolicyMetrics returns Policy handles that record nothing.
func noopPolicyMetrics() *emit.PolicyMetrics {
	m := &emit.PolicyMetrics{
		Auth:              noopAuth{},
		JWKSAge:           noopGauge{},
		UpstreamTokenAge:  noopGauge{},
		UpstreamRefreshes: noopCounter{},
		BucketEvictions:   noopCounter{},
	}
	for p := range m.Duration {
		m.Duration[p] = noopHist{}
		m.ShortCircuits[p] = noopStatus{}
		for k := range m.Failures[p] {
			m.Failures[p][k] = noopCounter{}
		}
	}
	for i := range m.RateLimit {
		m.RateLimit[i] = noopCounter{}
	}
	for i := range m.Quota {
		m.Quota[i] = noopCounter{}
	}
	for i := range m.StoreSkipped {
		m.StoreSkipped[i] = noopCounter{}
	}
	return m
}

// noopUpstreamMetrics returns Upstream handles that record nothing.
func noopUpstreamMetrics() *emit.UpstreamMetrics {
	m := &emit.UpstreamMetrics{
		Attempts:             noopAttempts{},
		AttemptDuration:      noopHist{},
		Retries:              noopCounter{},
		RetryBudgetExhausted: noopCounter{},
		HealthyEndpoints:     noopGauge{},
		ProbesSkipped:        noopCounter{},
	}
	for i := range m.BreakerState {
		m.BreakerState[i] = noopGauge{}
	}
	for i := range m.Ejections {
		m.Ejections[i] = noopCounter{}
	}
	for i := range m.Degraded {
		m.Degraded[i] = noopGauge{}
	}
	for i := range m.CELErrors {
		m.CELErrors[i] = noopCounter{}
	}
	for i := range m.PoolConnections {
		m.PoolConnections[i] = noopGauge{}
	}
	return m
}
