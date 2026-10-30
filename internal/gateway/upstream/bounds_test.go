// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/health"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// hangingDial blocks every dial until its context ends, like a black-holed
// address; started receives a value at each call.
type hangingDial struct{ started chan struct{} }

func (d hangingDial) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	d.started <- struct{}{}
	<-ctx.Done()
	return nil, context.Cause(ctx)
}

// signaled waits for a value on ch, failing the test after a real-time
// bound; it only waits for goroutines the test started.
func signaled(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// notReturned checks that the leg on ch has not returned yet.
func notReturned(t *testing.T, ch <-chan legResult, what string) {
	t.Helper()
	select {
	case res := <-ch:
		t.Fatalf("Leg returned before %s: %v", what, res.err)
	case <-time.After(20 * time.Millisecond):
	}
}

// TestLegConnectTimeout checks Upstream.spec.connectTimeout
// (docs/architecture/09-traffic-management-and-resilience.md "Deadlines"):
// a dial still pending when it expires fails the attempt as error kind
// connect, 502 RZ-UP-001 with no retry left, and the bound defaults to
// 1 s.
func TestLegConnectTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout *v1alpha1.Duration
		want    time.Duration
	}{
		{"default 1s", nil, time.Second},
		{"configured 3s", dur(3 * time.Second), 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := hangingDial{started: make(chan struct{}, 8)}
			f := newFixture(t)
			f.m = NewManager(Options{Clock: f.clk, Rand: lowSource{}, Dial: d.dial, Status: f.status, Tracer: f.tracer})
			met := newMetrics()
			u := newUpstream("orders", "10.0.0.1:80")
			u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0)}
			u.Spec.ConnectTimeout = tc.timeout
			s := f.compile(nil, Config{Upstream: u, Metrics: met.m})
			ch := goLeg(t.Context(), f.upstream(s, "orders"), outbound(exchange(http.MethodGet, "/"), nil))
			signaled(t, "the dial", d.started)
			f.clk.Advance(tc.want - time.Nanosecond)
			notReturned(t, ch, "connectTimeout")
			f.clk.Advance(time.Nanosecond)
			res := await(t, ch)
			var le *LegError
			if !errors.As(res.err, &le) || codeOf(res.err) != "RZ-UP-001" || le.Kind != resilience.KindConnect || le.Attempts != 1 {
				t.Errorf("Leg = %v, want RZ-UP-001 of kind connect after 1 attempt", res.err)
			}
			if a := met.attempts.list(); !slices.Equal(a, []string{"connect"}) {
				t.Errorf("attempts = %v, want [connect]", a)
			}
		})
	}
}

// silentTLSServer accepts TCP connections on loopback and reads the start
// of each ClientHello but never answers it, so a client handshake hangs;
// hello receives a value per connection that sent one.
func silentTLSServer(t *testing.T) (addr string, hello <-chan struct{}) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan struct{}, 8)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns []net.Conn
	)
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			wg.Go(func() {
				if _, err := c.Read(make([]byte, 1)); err == nil {
					ch <- struct{}{}
				}
			})
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return ln.Addr().String(), ch
}

// TestLegHandshakeTimeout checks Upstream.spec.tls.handshakeTimeout: a TLS
// handshake still pending when it expires fails the attempt as error kind
// tls, 502 RZ-UP-002, and the bound defaults to 2 s.
func TestLegHandshakeTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout *v1alpha1.Duration
		want    time.Duration
	}{
		{"default 2s", nil, 2 * time.Second},
		{"configured 5s", dur(5 * time.Second), 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, hello := silentTLSServer(t)
			f := newFixture(t)
			met := newMetrics()
			u := newUpstream("orders", addr)
			u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0)}
			u.Spec.TLS = &v1alpha1.UpstreamTLS{HandshakeTimeout: tc.timeout}
			s := f.compile(nil, Config{Upstream: u, Metrics: met.m})
			ch := goLeg(t.Context(), f.upstream(s, "orders"), outbound(exchange(http.MethodGet, "/"), nil))
			signaled(t, "the ClientHello", hello)
			f.clk.Advance(tc.want - time.Nanosecond)
			notReturned(t, ch, "tls.handshakeTimeout")
			f.clk.Advance(time.Nanosecond)
			res := await(t, ch)
			var le *LegError
			if !errors.As(res.err, &le) || codeOf(res.err) != "RZ-UP-002" || le.Kind != resilience.KindTLS || le.Attempts != 1 {
				t.Errorf("Leg = %v, want RZ-UP-002 of kind tls after 1 attempt", res.err)
			}
			if !errors.Is(res.err, errHandshakeTimeout) {
				t.Errorf("Leg = %v, want the handshake timeout as its cause", res.err)
			}
			if a := met.attempts.list(); !slices.Equal(a, []string{"tls"}) {
				t.Errorf("attempts = %v, want [tls]", a)
			}
		})
	}
}

// TestLegStallTimeout checks that healthCheck.stallTimeout reaches the
// Upstream's health Tracker: an attempt without response headers makes its
// Endpoint suspect once stallTimeout passes, 1 s by default.
func TestLegStallTimeout(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stall *v1alpha1.Duration
		want  time.Duration
	}{
		{"default 1s", nil, time.Second},
		{"configured 3s", dur(3 * time.Second), 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			stop := sync.OnceFunc(func() { close(release) })
			defer stop()
			srv := newServer(t, blocking(release))
			f := newFixture(t)
			u := newUpstream("orders", srv.addr)
			u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0)}
			if tc.stall != nil {
				u.Spec.HealthCheck = &v1alpha1.HealthCheck{StallTimeout: tc.stall}
			}
			s := f.compile(nil, Config{Upstream: u})
			up := f.upstream(s, "orders")
			ch := goLeg(t.Context(), up, outbound(exchange(http.MethodGet, "/"), nil))
			waitFor(t, "the attempt", func() bool { return srv.hits.Load() == 1 })
			suspect := func() bool { return up.rt.tracker.Status(f.clk.Now())[0].Suspect }
			f.clk.Advance(tc.want - time.Nanosecond)
			if suspect() {
				t.Fatal("suspect before stallTimeout")
			}
			f.clk.Advance(time.Nanosecond)
			if !suspect() {
				t.Fatal("not suspect once stallTimeout passed without response headers")
			}
			stop()
			if res := await(t, ch); res.err == nil {
				readAll(t, res.r)
			}
		})
	}
}

// TestStallTimeoutHotReload checks that activation installs the new
// snapshot's healthCheck.stallTimeout in the carried Tracker.
func TestStallTimeoutHotReload(t *testing.T) {
	f := newFixture(t)
	cfg := func(stall time.Duration) Config {
		u := newUpstream("orders", "10.0.0.1:80")
		u.Spec.HealthCheck = &v1alpha1.HealthCheck{StallTimeout: dur(stall)}
		return Config{Upstream: u}
	}
	a := f.compile(nil, cfg(3*time.Second))
	tr := f.upstream(a, "orders").rt.tracker
	if got := tr.Passive().StallTimeout; got != 3*time.Second {
		t.Fatalf("stallTimeout = %v, want 3s", got)
	}
	b := f.compile(nil, cfg(2*time.Second))
	if f.upstream(b, "orders").rt.tracker != tr {
		t.Fatal("the reload built a new Tracker, want the carried one")
	}
	if got := tr.Passive().StallTimeout; got != 2*time.Second {
		t.Errorf("stallTimeout after the reload = %v, want 2s", got)
	}
}

// poolOf returns the pool of the snapshot's Upstream orders for Endpoint
// identity.
func poolOf(t *testing.T, f *fixture, s *Set, identity string) *pool {
	t.Helper()
	u := f.upstream(s, "orders")
	st, ok := u.rt.lookup(identity)
	if !ok {
		t.Fatalf("no Endpoint %s", identity)
	}
	return u.rt.poolFor(st, u.tls)
}

// TestPoolPingTimeout checks healthCheck.pingTimeout: a pool's HTTP/2
// connections close when a ping goes unanswered that long, 5 s by default,
// while the 1 s of silence before the ping stays fixed.
func TestPoolPingTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		ping *v1alpha1.Duration
		want time.Duration
	}{
		{"default 5s", nil, 5 * time.Second},
		{"configured 7s", dur(7 * time.Second), 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			u := newUpstream("orders", "10.0.0.1:80")
			u.Spec.TLS = &v1alpha1.UpstreamTLS{}
			if tc.ping != nil {
				u.Spec.HealthCheck = &v1alpha1.HealthCheck{PingTimeout: tc.ping}
			}
			p := poolOf(t, f, f.compile(nil, Config{Upstream: u}), "10.0.0.1:80")
			if h := p.tr.HTTP2; h == nil || h.PingTimeout != tc.want || h.SendPingTimeout != health.PingAfter || health.PingAfter != time.Second {
				t.Fatalf("HTTP2Config = %+v, want a ping after 1 s of silence and a %v ping timeout", h, tc.want)
			}
		})
	}
}

// TestPoolBoundsInPoolKey checks that the connection bounds belong to the
// pool key (docs/architecture/03-data-plane.md "Upstream layer"): a Hot
// Reload that changes connectTimeout, tls.handshakeTimeout or
// healthCheck.pingTimeout builds new pools with the new bounds, and one
// that keeps them, changing only stallTimeout, keeps the pools.
func TestPoolBoundsInPoolKey(t *testing.T) {
	base := func() *v1alpha1.Upstream {
		u := newUpstream("orders", "10.0.0.1:80")
		u.Spec.TLS = &v1alpha1.UpstreamTLS{}
		return u
	}
	for _, tc := range []struct {
		name string
		mod  func(u *v1alpha1.Upstream)
		new  bool
		want connBounds
	}{
		{"connectTimeout", func(u *v1alpha1.Upstream) { u.Spec.ConnectTimeout = dur(4 * time.Second) }, true,
			connBounds{connect: 4 * time.Second, handshake: 2 * time.Second, ping: 5 * time.Second}},
		{"tls.handshakeTimeout", func(u *v1alpha1.Upstream) { u.Spec.TLS.HandshakeTimeout = dur(6 * time.Second) }, true,
			connBounds{connect: time.Second, handshake: 6 * time.Second, ping: 5 * time.Second}},
		{"healthCheck.pingTimeout", func(u *v1alpha1.Upstream) {
			u.Spec.HealthCheck = &v1alpha1.HealthCheck{PingTimeout: dur(8 * time.Second)}
		}, true, connBounds{connect: time.Second, handshake: 2 * time.Second, ping: 8 * time.Second}},
		{"stallTimeout only", func(u *v1alpha1.Upstream) {
			u.Spec.HealthCheck = &v1alpha1.HealthCheck{StallTimeout: dur(3 * time.Second)}
		}, false, connBounds{connect: time.Second, handshake: 2 * time.Second, ping: 5 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			a := f.compile(nil, Config{Upstream: base()})
			pa := poolOf(t, f, a, "10.0.0.1:80")
			changed := base()
			tc.mod(changed)
			b := f.compile(nil, Config{Upstream: changed})
			pb := poolOf(t, f, b, "10.0.0.1:80")
			if (pb != pa) != tc.new {
				t.Fatalf("new pool = %v, want %v", pb != pa, tc.new)
			}
			if pb.entry.conn != tc.want || pb.tr.HTTP2.PingTimeout != tc.want.ping {
				t.Errorf("pool bounds = %+v, ping timeout %v; want %+v", pb.entry.conn, pb.tr.HTTP2.PingTimeout, tc.want)
			}
		})
	}
}
