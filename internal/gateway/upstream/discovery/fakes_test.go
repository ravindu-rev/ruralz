// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// epoch is the fake clock's start.
func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// nxdomain is the error the pure-Go resolver returns for NXDOMAIN.
func nxdomain(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

// servfail is a lookup failure that is not NXDOMAIN.
func servfail(name string) error {
	return &net.DNSError{Err: "server misbehaving", Name: name, IsTemporary: true}
}

// ipAnswer is a scripted LookupIPAddr answer.
type ipAnswer struct {
	ips []string
	err error
}

// srvAnswer is a scripted LookupSRV answer.
type srvAnswer struct {
	recs []*net.SRV
	err  error
}

// fakeResolver answers from scripted tables; a missing host is NXDOMAIN.
type fakeResolver struct {
	mu    sync.Mutex
	hosts map[string]ipAnswer
	srv   map[string]srvAnswer
	calls map[string]int
	// block, when set, makes every lookup wait for it or for ctx.
	block chan struct{}
	// inFlight and peak count concurrent lookups.
	inFlight, peak atomic.Int64
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{hosts: map[string]ipAnswer{}, srv: map[string]srvAnswer{}, calls: map[string]int{}}
}

func (r *fakeResolver) setHost(host string, ips ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts[host] = ipAnswer{ips: ips}
}

func (r *fakeResolver) failHost(host string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts[host] = ipAnswer{err: err}
}

func (r *fakeResolver) setSRV(name string, recs ...*net.SRV) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.srv[name] = srvAnswer{recs: recs}
}

func (r *fakeResolver) failSRV(name string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.srv[name] = srvAnswer{err: err}
}

// partialSRV scripts records returned together with err, as LookupSRV does
// after dropping malformed records.
func (r *fakeResolver) partialSRV(name string, err error, recs ...*net.SRV) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.srv[name] = srvAnswer{recs: recs, err: err}
}

func (r *fakeResolver) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[name]
}

func (r *fakeResolver) enter(ctx context.Context, name string) error {
	r.mu.Lock()
	r.calls[name]++
	block := r.block
	r.mu.Unlock()
	n := r.inFlight.Add(1)
	for {
		p := r.peak.Load()
		if n <= p || r.peak.CompareAndSwap(p, n) {
			break
		}
	}
	defer r.inFlight.Add(-1)
	if block == nil {
		return nil
	}
	select {
	case <-block:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (r *fakeResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if err := r.enter(ctx, host); err != nil {
		return nil, err
	}
	r.mu.Lock()
	a, ok := r.hosts[host]
	r.mu.Unlock()
	if !ok {
		return nil, nxdomain(host)
	}
	if a.err != nil {
		return nil, a.err
	}
	out := make([]net.IPAddr, 0, len(a.ips))
	for _, s := range a.ips {
		ip, zone, _ := cutZone(s)
		out = append(out, net.IPAddr{IP: net.ParseIP(ip), Zone: zone})
	}
	return out, nil
}

func cutZone(s string) (ip, zone string, ok bool) {
	for i := range len(s) {
		if s[i] == '%' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func (r *fakeResolver) LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error) {
	key := "_" + service + "._" + proto + "." + name
	if err := r.enter(ctx, key); err != nil {
		return "", nil, err
	}
	r.mu.Lock()
	a, ok := r.srv[key]
	r.mu.Unlock()
	if !ok {
		return "", nil, nxdomain(key)
	}
	return key, a.recs, a.err
}

// statusEvent is one SetDegraded call.
type statusEvent struct {
	reason catalog.Reason
	source string
	on     bool
}

// fakeStatus records SetDegraded calls.
type fakeStatus struct {
	mu     sync.Mutex
	events []statusEvent
}

func (s *fakeStatus) SetDegraded(r catalog.Reason, source string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, statusEvent{r, source, on})
}

func (*fakeStatus) SetCleartextHops(catalog.Hop, int) {}

func (s *fakeStatus) list() []statusEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]statusEvent(nil), s.events...)
}

// held reports whether source holds reason after the recorded events.
func (s *fakeStatus) held(r catalog.Reason, source string) bool {
	on := false
	for _, e := range s.list() {
		if e.reason == r && e.source == source {
			on = e.on
		}
	}
	return on
}

// node reports the Node gauge of reason: 1 while any source holds it
// (emit.NodeStatus).
func (s *fakeStatus) node(r catalog.Reason) bool {
	on := map[string]bool{}
	for _, e := range s.list() {
		if e.reason == r {
			on[e.source] = e.on
		}
	}
	for _, v := range on {
		if v {
			return true
		}
	}
	return false
}

// gauge is a test emit.Gauge.
type gauge struct{ v atomic.Int64 }

func (g *gauge) Add(_ emit.Stripe, d int64) { g.v.Add(d) }
func (g *gauge) Set(v int64)                { g.v.Store(v) }

// newMetrics returns Upstream handles whose degraded gauges are inspectable.
func newMetrics() (*emit.UpstreamMetrics, *[emit.NumUpDegraded]*gauge) {
	var gs [emit.NumUpDegraded]*gauge
	m := &emit.UpstreamMetrics{}
	for i := range gs {
		gs[i] = &gauge{}
		m.Degraded[i] = gs[i]
	}
	return m, &gs
}

// lockedSource is a concurrency-safe deterministic rand.Source.
type lockedSource struct {
	mu  sync.Mutex
	src *rand.PCG
}

func newRand(seed uint64) *lockedSource {
	return &lockedSource{src: rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)}
}

func (l *lockedSource) Uint64() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.src.Uint64()
}

// fixedSource always returns v.
type fixedSource uint64

func (f fixedSource) Uint64() uint64 { return uint64(f) }

// harness bundles a Source with its fakes.
type harness struct {
	src     *Source
	res     *fakeResolver
	clk     *clocktest.Fake
	status  *fakeStatus
	metrics *emit.UpstreamMetrics
	degrade *[emit.NumUpDegraded]*gauge
}

func newHarness(t *testing.T, spec Spec, mod func(*Options)) *harness {
	t.Helper()
	h := &harness{res: newFakeResolver(), clk: clocktest.New(epoch()), status: &fakeStatus{}}
	m, gs := newMetrics()
	h.degrade = gs
	o := Options{
		Upstream: "orders",
		Clock:    h.clk,
		Resolver: h.res,
		Rand:     newRand(1),
		Status:   h.status,
		Metrics:  m,
	}
	if mod != nil {
		mod(&o)
	}
	src, err := New(spec, o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.src = src
	h.metrics = m
	return h
}

// stale reports the discovery_stale state on both gauges, failing the test
// when they disagree.
func (h *harness) stale(t *testing.T) bool {
	t.Helper()
	node := h.status.node(catalog.ReasonDiscoveryStale)
	up := h.degrade[emit.UpDegradedDiscoveryStale].v.Load() == 1
	if node != up {
		t.Fatalf("node gauge %v, upstream gauge %v disagree", node, up)
	}
	return node
}

// waitFor polls cond with a real-time bound; it only waits for goroutines
// the test started, never for fake time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}
