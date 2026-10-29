// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/balance"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/health"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestHotReloadKeepsState checks that health, ejection and breaker state
// survive a Hot Reload (docs/architecture/09-traffic-management-and-resilience.md
// "Load balancing"): the next snapshot's Upstream shares the runtime, and
// its metric handles receive the current breaker state at activation.
func TestHotReloadKeepsState(t *testing.T) {
	srvs, slots := orderedServers(t, 2)
	bad := status(http.StatusServiceUnavailable, "")
	slots[0].Store(&bad)
	f := newFixture(t)
	cfg := func() *v1alpha1.Upstream {
		u := newUpstream("orders", srvs[0].addr, srvs[1].addr)
		u.Spec.HealthCheck = &v1alpha1.HealthCheck{Passive: &v1alpha1.PassiveHealthCheck{ConsecutiveErrors: ptr[int32](1)}}
		return u
	}
	a := f.compile(nil, Config{Upstream: cfg()})
	r, err := f.upstream(a, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	// One failed leg under thresholds of 1 opens the breaker.
	ua := f.upstream(a, "orders")
	bc := ua.res.Breaker
	bc.ConsecutiveFailures, bc.MinimumLegs = 1, 1
	ua.rt.res.Breaker().Record(&bc, mustAllow(t, ua), resilience.LegFailure)
	if st := ua.rt.res.Breaker().State(); st != resilience.StateOpen {
		t.Fatalf("breaker = %v, want open", st)
	}

	met := newMetrics()
	b := f.compile(nil, Config{Upstream: cfg(), Metrics: met.m})
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	ub := f.upstream(b, "orders")
	if ub.rt != f.upstream(a, "orders").rt {
		t.Fatal("the new snapshot's Upstream has a new runtime, want the shared one")
	}
	if st := ub.rt.tracker.Status(f.clk.Now()); !st[0].Ejected {
		t.Error("the ejection did not survive the Hot Reload")
	}
	if met.breaker[resilience.StateOpen].get() != 1 || met.healthy.get() != 1 {
		t.Errorf("new handles: breaker open %d, healthy %d; want 1 and 1", met.breaker[resilience.StateOpen].get(), met.healthy.get())
	}
	if _, err := ub.Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{}); codeOf(err) != "RZ-UP-005" {
		t.Errorf("Leg after the reload = %v, want RZ-UP-005 from the carried breaker", err)
	}
}

// mustAllow admits one leg on u's breaker.
func mustAllow(t *testing.T, u *Upstream) resilience.Permit {
	t.Helper()
	p, ok := u.rt.res.Breaker().Allow(&u.res.Breaker)
	if !ok {
		t.Fatal("breaker refused")
	}
	return p
}

// TestRuntimeClosesWithLastSnapshot checks that an Upstream a new Revision
// drops keeps its runtime while an older snapshot holds it, and that the
// last Close ends it: Status no longer lists it and its degraded reasons
// clear.
func TestRuntimeClosesWithLastSnapshot(t *testing.T) {
	const refused = "127.0.0.1:1"
	f := newFixture(t, refused)
	u := newUpstream("orders", refused)
	u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0)}
	u.Spec.HealthCheck = &v1alpha1.HealthCheck{Passive: &v1alpha1.PassiveHealthCheck{ConsecutiveErrors: ptr[int32](1)}}
	a := f.compile(nil, Config{Upstream: u})
	if _, err := f.upstream(a, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{}); err == nil {
		t.Fatal("Leg to a refused Endpoint succeeded")
	}
	if !f.status.held(catalog.ReasonUpstreamPanic, "orders") {
		t.Fatal("upstream_panic not raised")
	}
	b := f.compile(nil, Config{Upstream: newUpstream("other")})
	if names := statusNames(f.m); len(names) != 2 {
		t.Errorf("runtimes while the old snapshot lives = %v, want orders and other", names)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if names := statusNames(f.m); len(names) != 1 || names[0] != "other" {
		t.Errorf("runtimes after the old snapshot closed = %v, want [other]", names)
	}
	if f.status.held(catalog.ReasonUpstreamPanic, "orders") {
		t.Error("upstream_panic still held after the runtime closed")
	}
	_ = b
}

// statusNames lists the runtimes Status reports.
func statusNames(m *Manager) []string {
	var out []string
	for _, s := range m.Status() {
		out = append(out, s.Name)
	}
	return out
}

// TestCompileRejects checks the Upstreams Compile refuses: features this
// release does not serve are RZ-CFG-040, shapes validation rejects first
// are RZ-CFG-005, an unresolved TLS secret is RZ-CFG-026; a failed Compile
// keeps no runtime.
func TestCompileRejects(t *testing.T) {
	grpc := newUpstream("orders", "10.0.0.1:80")
	grpc.Spec.Protocol = v1alpha1.UpstreamProtocolGRPC
	kube := newUpstream("orders")
	kube.Spec.Discovery = &v1alpha1.Discovery{Type: v1alpha1.DiscoveryTypeKubernetes, Service: "orders", Port: &v1alpha1.IntOrString{Int: 80}}
	ring := newUpstream("orders", "10.0.0.1:80")
	ring.Spec.LoadBalancing = &v1alpha1.LoadBalancing{Algorithm: ptr(v1alpha1.LoadBalancingRingHash)}
	addr := newUpstream("orders", "not-an-address")
	noPort := newUpstream("orders")
	noPort.Spec.Discovery = &v1alpha1.Discovery{Type: v1alpha1.DiscoveryTypeDNS, Service: "orders.svc"}
	tlsMissing := newUpstream("orders", "10.0.0.1:443")
	tlsMissing.Spec.TLS = &v1alpha1.UpstreamTLS{CACertificate: &v1alpha1.SecretValue{SecretRef: v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderFile, Name: "/missing.pem"}}}
	for _, tc := range []struct {
		name string
		u    *v1alpha1.Upstream
		code string
	}{
		{"grpc protocol", grpc, "RZ-CFG-040"},
		{"kubernetes discovery", kube, "RZ-CFG-040"},
		{"ring-hash without hashKey", ring, "RZ-CFG-005"},
		{"invalid address", addr, "RZ-CFG-005"},
		{"dns without port", noPort, "RZ-CFG-005"},
		{"unresolved CA", tlsMissing, "RZ-CFG-026"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			ok := newUpstream("fine", "10.0.0.1:80")
			_, err := f.m.Compile(t.Context(), Input{Upstreams: []Config{{Upstream: ok}, {Upstream: tc.u}}, Secrets: secretStore{}})
			if codeOf(err) != tc.code {
				t.Errorf("Compile = %v, want %s", err, tc.code)
			}
			if names := statusNames(f.m); len(names) != 0 {
				t.Errorf("runtimes after a failed Compile = %v, want none", names)
			}
		})
	}
	f := newFixture(t)
	if _, err := f.m.Compile(t.Context(), Input{Upstreams: []Config{{Upstream: newUpstream("dup")}, {Upstream: newUpstream("dup")}}}); err == nil {
		t.Error("Compile with a duplicate name succeeded")
	}
}

// TestParseSettingsDefaults checks the defaults of absent fields (the
// schema defaults of docs/architecture/02-configuration-model.md
// "Upstream") and the conversion of present ones.
func TestParseSettingsDefaults(t *testing.T) {
	st, err := parseSettings(&Config{Upstream: newUpstream("orders", "10.0.0.1:80")})
	if err != nil {
		t.Fatal(err)
	}
	def := resilience.DefaultConfig()
	if st.res != def || st.passive != (health.PassivePolicy{}).WithDefaults() || st.active != nil || st.alg != balance.LeastRequest || st.scheme != "http" {
		t.Errorf("defaults = %+v, want resilience.DefaultConfig, passive defaults, no active check, least-request, http", st)
	}
	if len(st.source.Static) != 1 || st.source.Static[0].Weight != 1 {
		t.Errorf("static source = %+v, want one Endpoint of weight 1", st.source.Static)
	}

	u := newUpstream("orders", "10.0.0.1:80")
	u.Spec.Endpoints[0].Weight = ptr[int32](0)
	u.Spec.Timeout = dur(3 * time.Second)
	u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](2), PerTryTimeout: dur(time.Second)}
	u.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{
		MaxConnections: ptr[int32](10), MaxPendingRequests: ptr[int32](0), ConsecutiveFailures: ptr[int32](7),
		MinimumLegs: ptr[int32](30), FailureRatio: ptr(0.25), OpenDuration: dur(time.Minute), HalfOpenSuccesses: ptr[int32](4),
	}
	u.Spec.HealthCheck = &v1alpha1.HealthCheck{
		Passive: &v1alpha1.PassiveHealthCheck{ConsecutiveErrors: ptr[int32](3), EjectionTime: dur(time.Minute)},
		Active:  &v1alpha1.ActiveHealthCheck{Path: ptr("/healthz"), UnhealthyThreshold: ptr[int32](4)},
	}
	u.Spec.LoadBalancing = &v1alpha1.LoadBalancing{Algorithm: ptr(v1alpha1.LoadBalancingRoundRobin)}
	u.Spec.TLS = &v1alpha1.UpstreamTLS{SNI: "orders.internal"}
	st, err = parseSettings(&Config{Upstream: u})
	if err != nil {
		t.Fatal(err)
	}
	want := resilience.Config{
		Timeout:  3 * time.Second,
		Retry:    resilience.RetryConfig{Attempts: 2, PerTryTimeout: time.Second},
		Breaker:  resilience.BreakerConfig{ConsecutiveFailures: 7, FailureRatio: 0.25, MinimumLegs: 30, HalfOpenSuccesses: 4, OpenDuration: time.Minute},
		Bulkhead: resilience.BulkheadConfig{MaxConnections: 10, MaxPendingRequests: 0},
	}
	if st.res != want {
		t.Errorf("resilience = %+v, want %+v", st.res, want)
	}
	if st.passive != (health.PassivePolicy{ConsecutiveErrors: 3, EjectionTime: time.Minute}) {
		t.Errorf("passive = %+v", st.passive)
	}
	wantActive := (health.ActivePolicy{Path: "/healthz", UnhealthyThreshold: 4}).WithDefaults()
	if st.active == nil || *st.active != wantActive {
		t.Errorf("active = %+v, want %+v", st.active, wantActive)
	}
	if st.alg != balance.RoundRobin || st.scheme != "https" || st.source.Static[0].Weight != 0 || st.poolKey != "http|tls|orders.internal|||" {
		t.Errorf("algorithm %v, scheme %s, weight %d, pool key %q", st.alg, st.scheme, st.source.Static[0].Weight, st.poolKey)
	}
}

// TestActivateReportsCleartextHops checks that activation sets the upstream
// cleartext hop count: Upstreams without tls.
func TestActivateReportsCleartextHops(t *testing.T) {
	srv := newTLSServer(t, status(http.StatusOK, ""))
	f := newFixture(t)
	secure := newUpstream("secure", srv.addr)
	secure.Spec.TLS = &v1alpha1.UpstreamTLS{CACertificate: &v1alpha1.SecretValue{SecretRef: v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderFile, Name: caRef().Name}}}
	f.compile(caStore(srv.Server), Config{Upstream: newUpstream("plain", "10.0.0.1:80")}, Config{Upstream: secure}, Config{Upstream: newUpstream("plain2", "10.0.0.2:80")})
	if n := f.status.hopCount(catalog.HopUpstream); n != 2 {
		t.Errorf("upstream cleartext hops = %d, want 2", n)
	}
}

// TestRunLegUnknownUpstream checks that a leg to an Upstream the snapshot
// does not hold is filter.ErrNotAvailable (snapshot.LegRunner for
// replay).
func TestRunLegUnknownUpstream(t *testing.T) {
	f := newFixture(t)
	s := f.compile(nil)
	_, err := s.RunLeg(t.Context(), "gone", outbound(exchange(http.MethodGet, "/"), nil))
	if !errors.Is(err, filter.ErrNotAvailable) {
		t.Errorf("RunLeg = %v, want filter.ErrNotAvailable", err)
	}
}

// fakeResolver answers A and AAAA lookups from a table.
type fakeResolver struct {
	mu    sync.Mutex
	hosts map[string][]string
}

func (r *fakeResolver) set(host string, ips ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts[host] = ips
}

func (r *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ips, ok := r.hosts[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	out := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return out, nil
}

func (*fakeResolver) LookupSRV(context.Context, string, string, string) (string, []*net.SRV, error) {
	return "", nil, errors.New("no SRV")
}

// redirectDial maps addresses to loopback servers.
type redirectDial struct {
	mu sync.Mutex
	to map[string]string
}

func (d *redirectDial) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	target, ok := d.to[address]
	d.mu.Unlock()
	if !ok {
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("no route")}
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, target)
}

// runManager runs m until the test ends.
func runManager(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
}

// TestDNSDiscoveryFollowsChanges checks discovery.type dns end to end:
// the compile-time lookup serves the first legs, Run refreshes the source,
// and a changed answer reaches the Endpoint table, the health Tracker and
// the balancer; Run then rebuilds the structure.
func TestDNSDiscoveryFollowsChanges(t *testing.T) {
	one := newServer(t, status(http.StatusOK, "one"))
	two := newServer(t, status(http.StatusOK, "two"))
	res := &fakeResolver{hosts: map[string][]string{}}
	res.set("orders.svc", "10.0.0.1")
	d := &redirectDial{to: map[string]string{"10.0.0.1:8080": one.addr, "10.0.0.2:8080": two.addr}}
	f := newFixture(t)
	f.m = NewManager(Options{Clock: f.clk, Rand: lowSource{}, Dial: d.dial, Resolver: res, Status: f.status, Tracer: f.tracer})
	met := newMetrics()
	u := newUpstream("orders")
	u.Spec.Discovery = &v1alpha1.Discovery{Type: v1alpha1.DiscoveryTypeDNS, Service: "orders.svc", Port: &v1alpha1.IntOrString{Int: 8080}}
	u.Spec.LoadBalancing = &v1alpha1.LoadBalancing{Algorithm: ptr(v1alpha1.LoadBalancingRoundRobin)}
	s := f.compile(nil, Config{Upstream: u, Metrics: met.m})
	up := f.upstream(s, "orders")
	r, err := up.Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if body := readAll(t, r); body != "one" || r.Endpoint != "10.0.0.1:8080" {
		t.Errorf("Leg = %q from %s, want one from 10.0.0.1:8080", body, r.Endpoint)
	}
	if r.Header == nil || one.last().Host != "orders.svc:8080" {
		t.Errorf("Host = %q, want orders.svc:8080 (discovery.service and port)", one.last().Host)
	}

	runManager(t, f.m)
	res.set("orders.svc", "10.0.0.2")
	waitFor(t, "the refresh delay", func() bool { return f.clk.Pending() > 0 })
	f.clk.Advance(discovery30s)
	waitFor(t, "the new Endpoint set", func() bool {
		st := f.m.Status()
		return len(st) == 1 && len(st[0].Endpoints) == 1 && st[0].Endpoints[0].Identity == "10.0.0.2:8080"
	})
	// The Endpoint that left retired its pool: its idle connection closed.
	if n := met.pool[emit.PoolIdle].get(); n != 0 {
		t.Errorf("idle connections after the Endpoint left = %d, want 0", n)
	}
	o := outbound(exchange(http.MethodGet, "/"), nil)
	o.Deadline = f.clk.Now().Add(15 * time.Second)
	r, err = up.Leg(t.Context(), o, Target{})
	if err != nil {
		t.Fatalf("Leg after the change: %v", err)
	}
	if body := readAll(t, r); body != "two" {
		t.Errorf("Leg after the change = %q, want two", body)
	}
	waitFor(t, "the rebuild", func() bool {
		st := f.m.Status()
		return len(st[0].Balancers) == 1 && !st[0].Balancers[0].Stale
	})
}

// discovery30s passes the discovery refresh delay of lowSource (27 s).
const discovery30s = 30 * time.Second

// TestBalancerBudgetFallback checks the balancer budget plan: a ring-hash
// Upstream whose ring does not fit at 64 virtual nodes falls back to
// weighted random with degraded reason balancer_budget (Node and
// Upstream) and a configuration warning, and still serves.
func TestBalancerBudgetFallback(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, ""))
	f := newFixture(t)
	f.m = NewManager(Options{Clock: f.clk, Rand: lowSource{}, Dial: f.dial.dial, Status: f.status, Tracer: f.tracer, BalancerBudget: balance.CopyOnWriteReserve + 1000})
	met := newMetrics()
	u := newUpstream("ring", srv.addr, "127.0.0.1:2")
	u.Spec.LoadBalancing = &v1alpha1.LoadBalancing{Algorithm: ptr(v1alpha1.LoadBalancingRingHash), HashKey: `"k"`}
	s := f.compile(nil, Config{Upstream: u, HashKey: celtest.Program(t, celtest.At(expr.PlaceHashKey), u.Spec.LoadBalancing.HashKey), Metrics: met.m})
	if !f.status.held(catalog.ReasonBalancerBudget, "ring") || met.degraded[emit.UpDegradedBalancerBudget].get() != 1 {
		t.Errorf("balancer_budget held %v, gauge %d; want true and 1", f.status.held(catalog.ReasonBalancerBudget, "ring"), met.degraded[emit.UpDegradedBalancerBudget].get())
	}
	if w := s.Warnings(); len(w) != 1 || w[0].Upstream != "ring" {
		t.Errorf("warnings = %+v, want one for ring", w)
	}
	info := f.m.Status()[0].Balancers[0]
	if !info.Fallback || info.Effective != balance.Random {
		t.Errorf("balancer = %+v, want a weighted random fallback", info)
	}
}

// TestActiveHealthCheck checks active checks: the Prober probes each
// Endpoint on its pool with GET path, and unhealthyThreshold failures mark
// it unhealthy, counted in ruralz_upstream_ejections_total{reason="active"}.
func TestActiveHealthCheck(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	f := newFixture(t)
	met := newMetrics()
	u := newUpstream("orders", srv.addr)
	u.Spec.HealthCheck = &v1alpha1.HealthCheck{Active: &v1alpha1.ActiveHealthCheck{Path: ptr("/healthz"), UnhealthyThreshold: ptr[int32](1)}}
	s, err := f.m.Compile(t.Context(), Input{Upstreams: []Config{{Upstream: u, Metrics: met.m}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// Checks start at activation, when the Upstream they probe through is
	// active.
	if f.upstream(s, "orders").rt.tracker.Active() != nil {
		t.Error("active checks enabled before activation")
	}
	s.Activate()
	if f.upstream(s, "orders").rt.tracker.Active() == nil {
		t.Fatal("active checks not enabled at activation")
	}
	runManager(t, f.m)
	waitFor(t, "the failed probe", func() bool { return met.eject[emit.EjectActive].get() == 1 })
	st := f.upstream(s, "orders").rt.tracker.Status(f.clk.Now())
	if st[0].Healthy {
		t.Error("Endpoint still healthy after a failed probe")
	}
	if got := srv.last(); got.Method != http.MethodGet || got.URL.Path != "/healthz" || got.Host != srv.addr {
		t.Errorf("probe = %s %s Host %s, want GET /healthz Host %s", got.Method, got.URL.Path, got.Host, srv.addr)
	}
}

// TestPoolsCountAndCarry checks the connection pools: connections are
// counted in ruralz_upstream_pool_connections (active while an attempt
// uses one, idle after), pooled connections are reused, and a Hot Reload
// that keeps the protocol, TLS setting and Endpoint keeps the pool.
func TestPoolsCountAndCarry(t *testing.T) {
	var mu sync.Mutex
	conns := 0
	srv := newServerWith(t, status(http.StatusOK, "pooled"), func(hs *http.Server) {
		hs.ConnState = func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				mu.Lock()
				conns++
				mu.Unlock()
			}
		}
	})
	f := newFixture(t)
	met := newMetrics()
	a := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr), Metrics: met.m})
	r, err := f.upstream(a, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if met.pool[emit.PoolActive].get() != 1 || met.pool[emit.PoolIdle].get() != 0 {
		t.Errorf("pool while the body is open: active %d idle %d, want 1 and 0", met.pool[emit.PoolActive].get(), met.pool[emit.PoolIdle].get())
	}
	readAll(t, r)
	waitFor(t, "the idle connection", func() bool {
		return met.pool[emit.PoolActive].get() == 0 && met.pool[emit.PoolIdle].get() == 1
	})

	u := newUpstream("orders", srv.addr)
	u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](3)}
	b := f.compile(nil, Config{Upstream: u, Metrics: met.m})
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = f.upstream(b, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg after the reload: %v", err)
	}
	readAll(t, r)
	mu.Lock()
	defer mu.Unlock()
	if conns != 1 {
		t.Errorf("connections opened = %d across the reload, want 1 (pool carried)", conns)
	}
}

// TestEgressGuard checks that Endpoint connections pass the egress guard:
// a loopback Endpoint is refused unless RURALZ_FETCH_ALLOW lists it, as a
// connect error (RZ-UP-001) that never reaches the server.
func TestEgressGuard(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, ""))
	for _, tc := range []struct {
		allow string
		code  string
	}{
		{"", "RZ-UP-001"},
		{"127.0.0.0/8", ""},
	} {
		t.Run("allow="+tc.allow, func(t *testing.T) {
			g, err := egress.ParseAllow(tc.allow)
			if err != nil {
				t.Fatal(err)
			}
			f := newFixture(t)
			f.m = NewManager(Options{Clock: f.clk, Rand: lowSource{}, Guard: g})
			u := newUpstream("orders", srv.addr)
			u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0)}
			s := f.compile(nil, Config{Upstream: u})
			before := srv.hits.Load()
			r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
			if err == nil {
				readAll(t, r)
			}
			if codeOf(err) != tc.code {
				t.Errorf("Leg = %v, want code %q", err, tc.code)
			}
			if tc.code != "" && srv.hits.Load() != before {
				t.Error("a refused connection reached the server")
			}
		})
	}
}

// TestNoTransparentCompression checks that the transport neither asks for
// gzip nor decodes the response: the Upstream's encoded body and
// Content-Encoding reach the client unchanged.
func TestNoTransparentCompression(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = io.WriteString(zw, "compressed")
	_ = zw.Close()
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(gz.Len()))
		_, _ = w.Write(gz.Bytes())
	})
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if body := readAll(t, r); body != gz.String() || r.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("response = %q with Content-Encoding %q, want the gzip bytes unchanged", body, r.Header.Get("Content-Encoding"))
	}
	if _, ok := srv.last().Header["Accept-Encoding"]; ok {
		t.Errorf("Accept-Encoding = %q, want none added", srv.last().Header.Get("Accept-Encoding"))
	}
}

// TestRunOnce checks that a second concurrent Run returns ErrRunning.
func TestRunOnce(t *testing.T) {
	f := newFixture(t)
	runManager(t, f.m)
	waitFor(t, "Run", func() bool { return f.m.running.Load() })
	if err := f.m.Run(t.Context()); !errors.Is(err, ErrRunning) {
		t.Errorf("second Run = %v, want ErrRunning", err)
	}
}

// blockingResolver is a fakeResolver whose lookups wait for release.
type blockingResolver struct {
	*fakeResolver
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.fakeResolver.LookupIPAddr(ctx, host)
}

// TestCompileResolvesWithoutLock checks that Compile resolves a new
// source without holding the Manager's lock, so a slow DNS answer stalls
// neither Status nor the background work, and that the resolved set
// serves once Compile returns.
func TestCompileResolvesWithoutLock(t *testing.T) {
	res := &blockingResolver{fakeResolver: &fakeResolver{hosts: map[string][]string{}}, entered: make(chan struct{}), release: make(chan struct{})}
	res.set("orders.svc", "10.0.0.1")
	f := newFixture(t)
	f.m = NewManager(Options{Clock: f.clk, Rand: lowSource{}, Resolver: res})
	u := newUpstream("orders")
	u.Spec.Discovery = &v1alpha1.Discovery{Type: v1alpha1.DiscoveryTypeDNS, Service: "orders.svc", Port: &v1alpha1.IntOrString{Int: 8080}}
	done := make(chan *Set, 1)
	go func() {
		s, err := f.m.Compile(context.Background(), Input{Upstreams: []Config{{Upstream: u}}})
		if err != nil {
			t.Errorf("Compile: %v", err)
		}
		done <- s
	}()
	<-res.entered
	status := make(chan []Status, 1)
	go func() { status <- f.m.Status() }()
	select {
	case st := <-status:
		if len(st) != 1 || len(st[0].Endpoints) != 0 {
			t.Errorf("Status during the lookup = %+v, want orders without Endpoints yet", st)
		}
	case <-time.After(5 * time.Second):
		t.Error("Status blocked behind a DNS lookup")
	}
	close(res.release)
	s := <-done
	if s == nil {
		return
	}
	defer func() { _ = s.Close() }()
	if st := f.m.Status(); len(st[0].Endpoints) != 1 || st[0].Endpoints[0].Identity != "10.0.0.1:8080" {
		t.Errorf("Endpoints after Compile = %+v, want 10.0.0.1:8080", st[0].Endpoints)
	}
}

// TestProbeTargetFollowsActivation checks that active checks probe through
// the active snapshot's Upstream only: when the active snapshot that held
// the Upstream closes while a snapshot not yet activated holds it, checks
// stop and probes find no target, until that snapshot activates.
func TestProbeTargetFollowsActivation(t *testing.T) {
	f := newFixture(t)
	withCheck := func() *v1alpha1.Upstream {
		u := newUpstream("orders", "10.0.0.1:80")
		u.Spec.HealthCheck = &v1alpha1.HealthCheck{Active: &v1alpha1.ActiveHealthCheck{}}
		return u
	}
	compile := func(cfgs ...Config) *Set {
		s, err := f.m.Compile(t.Context(), Input{Upstreams: cfgs})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	a := compile(Config{Upstream: withCheck()})
	a.Activate()
	rt := f.upstream(a, "orders").rt
	b := compile(Config{Upstream: newUpstream("other")})
	b.Activate()
	c := compile(Config{Upstream: withCheck()})
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if rt.target.Load() != nil || rt.tracker.Active() != nil {
		t.Error("checks still run through a released Upstream")
	}
	if _, err := rt.probe(t.Context(), healthProbe("orders", "10.0.0.1:80")); !errors.Is(err, errNoProbeTarget) {
		t.Errorf("probe without an active Upstream = %v, want errNoProbeTarget", err)
	}
	c.Activate()
	if tg := rt.target.Load(); tg == nil || tg.up != f.upstream(c, "orders") || rt.tracker.Active() == nil {
		t.Error("checks did not resume with the activated snapshot")
	}
}

// healthProbe names one active check.
func healthProbe(upstream, endpoint string) health.Probe {
	return health.Probe{Upstream: upstream, Endpoint: endpoint, Path: "/"}
}
