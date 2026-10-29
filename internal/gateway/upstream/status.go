// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/balance"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/discovery"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/health"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Status is one Upstream runtime's state for /debug/upstreams (Endpoint
// sets, health, ejections and breaker states; docs/architecture/03-data-plane.md
// "Admin endpoints").
type Status struct {
	// Name is the Upstream's metadata.name.
	Name string
	// Source is the active Endpoint source's refresh state.
	Source discovery.Status
	// Balancers describe the published balancer structures, one per
	// algorithm the live snapshots use.
	Balancers []balance.Info
	// Breaker is the circuit breaker's state and window counts.
	Breaker resilience.BreakerStats
	// InFlight and Pending are the bulkhead's held slots and waiters.
	InFlight, Pending int
	// RetryOriginals and RetryRetries are the retry budget's in-flight
	// legs and retries.
	RetryOriginals, RetryRetries int
	// Panic reports panic mode (no healthy Endpoint).
	Panic bool
	// Endpoints are the current Endpoints in identity order.
	Endpoints []EndpointStatus
}

// EndpointStatus is one Endpoint's state.
type EndpointStatus struct {
	health.EndpointStatus
	// Weight is the effective weight.
	Weight uint32
	// InFlight counts attempts holding a slot on the Endpoint.
	InFlight int64
	// Addresses are the cached IPs with the port, in answer order.
	Addresses []string
}

// status reads the runtime's state.
func (rt *runtime) status() Status {
	st := Status{Name: rt.name}
	rt.mu.Lock()
	tab := rt.table
	if rt.provider != nil {
		st.Source = rt.provider.Current().Status
	}
	for _, b := range rt.balancers {
		st.Balancers = append(st.Balancers, b.bal.Info())
	}
	rt.mu.Unlock()
	st.Breaker = rt.res.Breaker().Stats()
	st.InFlight, st.Pending = rt.res.Bulkhead().InFlight(), rt.res.Bulkhead().Pending()
	st.RetryOriginals, st.RetryRetries = rt.res.Budget().InFlight()
	rt.statusMu.Lock()
	st.Panic = rt.panicOn
	rt.statusMu.Unlock()
	hs := rt.tracker.Status(rt.m.clk.Now())
	byID := make(map[string]health.EndpointStatus, len(hs))
	for _, h := range hs {
		byID[h.Identity] = h
	}
	st.Endpoints = make([]EndpointStatus, 0, len(tab.eps))
	for _, e := range tab.eps {
		es := EndpointStatus{EndpointStatus: byID[e.st.identity], Weight: e.weight, InFlight: e.st.inflight.Load()}
		es.Identity = e.st.identity
		if _, ok := byID[e.st.identity]; !ok {
			es.Healthy = true
		}
		if n := e.st.net.Load(); n != nil {
			for _, a := range n.addrs {
				es.Addresses = append(es.Addresses, a.String())
			}
		}
		st.Endpoints = append(st.Endpoints, es)
	}
	return st
}

// withNoops returns a copy of m whose missing handles record nothing, so
// the request path never checks for nil; nil m gives handles that all
// record nothing.
func withNoops(m *emit.UpstreamMetrics) *emit.UpstreamMetrics {
	var out emit.UpstreamMetrics
	if m != nil {
		out = *m
	}
	c, g, h := nopCounter{}, nopGauge{}, nopHistogram{}
	if out.Attempts == nil {
		out.Attempts = nopAttempts{}
	}
	if out.AttemptDuration == nil {
		out.AttemptDuration = h
	}
	for _, p := range []*emit.Counter{&out.Retries, &out.RetryBudgetExhausted, &out.ProbesSkipped} {
		if *p == nil {
			*p = c
		}
	}
	if out.HealthyEndpoints == nil {
		out.HealthyEndpoints = g
	}
	for i := range out.BreakerState {
		if out.BreakerState[i] == nil {
			out.BreakerState[i] = g
		}
	}
	for i := range out.Ejections {
		if out.Ejections[i] == nil {
			out.Ejections[i] = c
		}
	}
	for i := range out.Degraded {
		if out.Degraded[i] == nil {
			out.Degraded[i] = g
		}
	}
	for i := range out.CELErrors {
		if out.CELErrors[i] == nil {
			out.CELErrors[i] = c
		}
	}
	for i := range out.PoolConnections {
		if out.PoolConnections[i] == nil {
			out.PoolConnections[i] = g
		}
	}
	return &out
}

type (
	nopCounter   struct{}
	nopGauge     struct{}
	nopHistogram struct{}
	nopAttempts  struct{}
)

func (nopCounter) Add(emit.Stripe, uint64)                             {}
func (nopGauge) Add(emit.Stripe, int64)                                {}
func (nopGauge) Set(int64)                                             {}
func (nopHistogram) Record(emit.Stripe, uint64)                        {}
func (nopHistogram) RecordExemplar(emit.Stripe, uint64, emit.Exemplar) {}
func (nopAttempts) Inc(emit.Stripe, int, int)                          {}
