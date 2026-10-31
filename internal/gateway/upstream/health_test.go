// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// orderedServers starts n loopback servers and returns them in identity
// order, the order lowSource picks in; each answers through its
// handler slot, which the test sets.
func orderedServers(t *testing.T, n int) ([]*server, []*atomic.Pointer[http.HandlerFunc]) {
	t.Helper()
	slots := make([]*atomic.Pointer[http.HandlerFunc], n)
	srvs := make([]*server, n)
	for i := range n {
		slot := &atomic.Pointer[http.HandlerFunc]{}
		ok := status(http.StatusOK, "ok")
		slot.Store(&ok)
		slots[i] = slot
		srvs[i] = newServer(t, func(w http.ResponseWriter, r *http.Request) { (*slot.Load())(w, r) })
	}
	for i := 1; i < n; i++ {
		for j := i; j > 0 && srvs[j].addr < srvs[j-1].addr; j-- {
			srvs[j], srvs[j-1] = srvs[j-1], srvs[j]
			slots[j], slots[j-1] = slots[j-1], slots[j]
		}
	}
	return srvs, slots
}

// TestLegPassiveEjection checks passive ejection
// (docs/architecture/09-traffic-management-and-resilience.md "Health
// checking and outlier detection"): attempts matching failureWhen eject the
// Endpoint after consecutiveErrors, counted in
// ruralz_upstream_ejections_total{reason="passive"}, and later legs pick
// the other Endpoint.
func TestLegPassiveEjection(t *testing.T) {
	srvs, slots := orderedServers(t, 2)
	bad := status(http.StatusServiceUnavailable, "")
	slots[0].Store(&bad)
	f := newFixture(t)
	met := newMetrics()
	u := newUpstream("orders", srvs[0].addr, srvs[1].addr)
	u.Spec.HealthCheck = &v1alpha1.HealthCheck{Passive: &v1alpha1.PassiveHealthCheck{ConsecutiveErrors: ptr[int32](1)}}
	s := f.compile(nil, Config{Upstream: u, Metrics: met.m})
	up := f.upstream(s, "orders")

	r, err := up.Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if r.Status != 200 || r.Attempts != 2 || r.Endpoint != srvs[1].addr {
		t.Errorf("first Leg = %d after %d attempts from %s, want 200 after 2 from %s", r.Status, r.Attempts, r.Endpoint, srvs[1].addr)
	}
	st := up.rt.tracker.Status(f.clk.Now())
	if !st[0].Ejected || st[1].Ejected || met.eject[emit.EjectPassive].get() != 1 {
		t.Errorf("ejected = %v %v, passive ejections = %d; want true false 1", st[0].Ejected, st[1].Ejected, met.eject[emit.EjectPassive].get())
	}
	r, err = up.Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if r.Attempts != 1 || r.Endpoint != srvs[1].addr || srvs[0].hits.Load() != 1 {
		t.Errorf("second Leg after %d attempts from %s, %d requests to the ejected Endpoint; want 1 from %s and 1",
			r.Attempts, r.Endpoint, srvs[0].hits.Load(), srvs[1].addr)
	}
	if met.healthy.get() != 1 {
		t.Errorf("healthy Endpoints gauge = %d, want 1", met.healthy.get())
	}
}

// TestLegPanicMode checks panic mode: with every Endpoint ejected (a
// connect error bypasses the ejection cap) the Node raises upstream_panic
// and the Upstream panic, and legs still use the Endpoints.
func TestLegPanicMode(t *testing.T) {
	const refused = "127.0.0.1:1"
	f := newFixture(t, refused)
	met := newMetrics()
	u := newUpstream("orders", refused)
	u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0)}
	u.Spec.HealthCheck = &v1alpha1.HealthCheck{Passive: &v1alpha1.PassiveHealthCheck{ConsecutiveErrors: ptr[int32](1)}}
	s := f.compile(nil, Config{Upstream: u, Metrics: met.m})
	up := f.upstream(s, "orders")
	for range 2 {
		if _, err := up.Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{}); codeOf(err) != "RZ-UP-001" {
			t.Fatalf("Leg = %v, want RZ-UP-001", err)
		}
	}
	if !f.status.held(catalog.ReasonUpstreamPanic, "orders") || met.degraded[emit.UpDegradedPanic].get() != 1 {
		t.Errorf("upstream_panic held = %v, panic gauge = %d; want true and 1", f.status.held(catalog.ReasonUpstreamPanic, "orders"), met.degraded[emit.UpDegradedPanic].get())
	}
	if n := f.dial.count(refused); n != 2 {
		t.Errorf("dials in panic mode = %d, want 2", n)
	}
}

// TestLegEndpointCap checks the per-Endpoint in-flight cap: an Endpoint
// holding max(8, min(2 × its weighted share, 50%) of maxConnections)
// attempts is avoided while another Endpoint remains.
func TestLegEndpointCap(t *testing.T) {
	srvs, slots := orderedServers(t, 2)
	release := make(chan struct{})
	defer close(release)
	hold := blocking(release)
	slots[0].Store(&hold)
	f := newFixture(t)
	u := newUpstream("orders", srvs[0].addr, srvs[1].addr)
	u.Spec.LoadBalancing = &v1alpha1.LoadBalancing{Algorithm: ptr(v1alpha1.LoadBalancingRandom)}
	u.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{MaxConnections: ptr[int32](16)}
	s := f.compile(nil, Config{Upstream: u})
	up := f.upstream(s, "orders")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	held := make([]<-chan legResult, 0, 8)
	for i := range 8 {
		held = append(held, goLeg(ctx, up, outbound(exchange(http.MethodGet, "/"), nil)))
		waitFor(t, "a held attempt", func() bool { return srvs[0].hits.Load() == int64(i+1) })
	}
	r, err := up.Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if r.Endpoint != srvs[1].addr {
		t.Errorf("Leg past the cap went to %s, want %s", r.Endpoint, srvs[1].addr)
	}
	if limit := resilience.EndpointCap(16, 1, 2); limit != 8 {
		t.Fatalf("EndpointCap(16, 1, 2) = %d, want 8", limit)
	}
	cancel()
	for _, ch := range held {
		await(t, ch)
	}
}
