// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package l4lb

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// No stickiness: new connections from one client alternate over the
// healthy backends (Scalability "L4 load balancing").
func TestRoundRobinWithoutStickiness(t *testing.T) {
	a, b := newNode(t, "a", 200), newNode(t, "b", 200)
	lb := startLB(t, Config{Backends: []Backend{a.backend(), b.backend()}, Clock: fake()})
	probed(t, lb, 1, "a", "b")
	got := backends(t, lb, 6)
	if want := []string{"a", "b", "a", "b", "a", "b"}; !slices.Equal(got, want) {
		t.Errorf("backends %v, want %v", got, want)
	}
	if s, _ := lb.Backend("a"); s.Routed != 3 || s.Weight != 1 || s.State != Healthy {
		t.Errorf("backend a: %+v", s)
	}
	if st := lb.Stats(); st.Accepted != 6 || st.Routed != 6 {
		t.Errorf("stats %+v, want 6 accepted and routed", st)
	}
}

// Bytes pass unchanged both ways, a client's half-close reaches the
// backend as EOF while the other direction stays open, and the counters
// see every byte.
func TestSpliceForwardsBytesAndHalfClose(t *testing.T) {
	a := newNode(t, "a", 200)
	lb := startLB(t, Config{Backends: []Backend{a.backend()}, Clock: fake()})
	probed(t, lb, 1, "a")
	c := mustDial(t, lb)
	c.roundTrip(t, "hello")
	if _, err := io.WriteString(c.c, "bye"); err != nil {
		t.Fatal(err)
	}
	if err := c.c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(c.r)
	if err != nil || string(rest) != "bye" {
		t.Fatalf("after the half-close read %q, %v; want the echo then EOF", rest, err)
	}
	local := c.c.LocalAddr().String()
	waitFor(t, "the connection to end", func() bool { return lb.Stats().Active == 0 })
	ci, ok := lb.ConnOf(local)
	if !ok || ci.BytesUp != 8 || ci.BytesDown != int64(len("a\n")+8) || ci.Closed.IsZero() || ci.Backend != "a" {
		t.Errorf("ConnOf = %+v, %v", ci, ok)
	}
	if s, _ := lb.Backend("a"); s.BytesUp != 8 || s.BytesDown != 10 || s.Active != 0 {
		t.Errorf("backend counters %+v", s)
	}
}

// TLS passes through untouched: the client sees the backend's certificate
// and negotiates HTTP/2 with it by ALPN; probes reach an https /readyz.
func TestTLSPassesThrough(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "secure")
	}))
	srv.EnableHTTP2 = true
	srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	lb := startLB(t, Config{
		Backends: []Backend{{Name: "tls", Addr: srv.Listener.Addr().String(), ReadyURL: srv.URL + "/readyz"}},
		ProbeTLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		Clock:    fake(),
	})
	if err := lb.WaitHealthy(t.Context(), "tls"); err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", lb.Addr())
		},
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true,
	}
	t.Cleanup(tr.CloseIdleConnections)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "secure" || resp.Proto != "HTTP/2.0" || !resp.TLS.PeerCertificates[0].Equal(srv.Certificate()) {
		t.Errorf("through the balancer: body %q proto %s, peer certificate from the backend: %v", body, resp.Proto,
			resp.TLS.PeerCertificates[0].Equal(srv.Certificate()))
	}
}

// A backend's reset reaches the client as a reset, not a clean end.
func TestResetPropagates(t *testing.T) {
	a := newNode(t, "a", 200)
	lb := startLB(t, Config{Backends: []Backend{a.backend()}, Clock: fake()})
	probed(t, lb, 1, "a")
	c := mustDial(t, lb)
	if _, err := io.WriteString(c.c, "rst"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.r.ReadByte(); err == nil || errors.Is(err, io.EOF) {
		t.Errorf("read after a backend reset = %v, want a reset", err)
	}
}

// Probes run on the injected clock: a new backend's first success makes
// it healthy; a not-ready one takes no connections until then.
func TestNewBackendHealthyAfterFirstSuccess(t *testing.T) {
	fk := fake()
	a := newNode(t, "a", 503)
	lb := startLB(t, Config{Backends: []Backend{a.backend()}, Clock: fk})
	probed(t, lb, 1, "a")
	s, _ := lb.Backend("a")
	if s.State != Initial || s.LastProbe.Status != 503 || s.LastProbe.Reason != adminapi.ReasonNoRevision || s.LastProbe.OK || s.Weight != 0 {
		t.Fatalf("not-ready backend %+v", s)
	}
	if _, err := dial(t, lb); err == nil {
		t.Fatal("a connection reached a backend that was never ready")
	}
	if st := lb.Stats(); st.NoBackend != 1 {
		t.Errorf("stats %+v, want one NoBackend reset", st)
	}
	a.status.Store(200)
	tick(t, lb, fk, "a")
	if s, _ = lb.Backend("a"); s.State != Healthy || s.Weight != 1 || !s.LastProbe.OK {
		t.Fatalf("after one success: %+v, want healthy at full weight", s)
	}
	tr := lb.Transitions()
	if len(tr) != 1 || tr[0].From != Initial || tr[0].To != Healthy || !tr[0].At.Equal(epoch().Add(time.Second)) {
		t.Errorf("transitions %+v", tr)
	}
	if c := mustDial(t, lb); c.be != "a" {
		t.Errorf("connection reached %q", c.be)
	}
}

// A Drain's 503 takes a backend out within ProbeInterval ×
// UnhealthyThreshold, inside the 5 s accept window (Zero-downtime upgrades
// "Drain timeline defaults"); HealthyThreshold successes bring it back.
func TestDrainDetectedWithinAcceptWindow(t *testing.T) {
	fk := fake()
	a, b := newNode(t, "a", 200), newNode(t, "b", 200)
	lb := startLB(t, Config{Backends: []Backend{a.backend(), b.backend()}, Clock: fk, SlowStart: -1})
	probed(t, lb, 1, "a", "b")
	if d := lb.DetectionTime(); d != 3*time.Second || d > AcceptWindow {
		t.Fatalf("DetectionTime = %v, want 3s within the %v accept window", d, AcceptWindow)
	}
	a.status.Store(503)
	a.setReason(adminapi.ReasonDraining)
	for i := 1; i <= 2; i++ {
		tick(t, lb, fk, "a", "b")
		if s, _ := lb.Backend("a"); s.State != Healthy {
			t.Fatalf("unhealthy after %d failed probes, want %d", i, DefaultUnhealthyThreshold)
		}
	}
	tick(t, lb, fk, "a", "b")
	s, _ := lb.Backend("a")
	if s.State != Unhealthy || s.ProbeFailures != 3 || s.LastProbe.Reason != adminapi.ReasonDraining {
		t.Fatalf("after three failed probes: %+v", s)
	}
	tr := lb.Transitions()
	last := tr[len(tr)-1]
	if last.To != Unhealthy || last.At.Sub(epoch().Add(time.Second)) > AcceptWindow || last.Probe.Reason != adminapi.ReasonDraining {
		t.Errorf("transition %+v, want unhealthy within the accept window of the first failure", last)
	}
	if got := backends(t, lb, 3); !slices.Equal(got, []string{"b", "b", "b"}) {
		t.Errorf("new connections went to %v, want b only", got)
	}

	a.status.Store(200)
	tick(t, lb, fk, "a", "b")
	if s, _ := lb.Backend("a"); s.State != Unhealthy {
		t.Fatal("back after one success, want two")
	}
	tick(t, lb, fk, "a", "b")
	if s, _ := lb.Backend("a"); s.State != Healthy || s.Transitions != 3 {
		t.Fatalf("after two successes: %+v", s)
	}
}

// A probe that gets no answer within ProbeTimeout fails, on the injected
// clock.
func TestProbeTimeoutFails(t *testing.T) {
	fk := fake()
	a := newNode(t, "a", 200)
	a.hang.Store(true)
	lb := startLB(t, Config{Backends: []Backend{a.backend()}, Clock: fk, ProbeTimeout: 500 * time.Millisecond})
	waitFor(t, "the probe and its timeout in flight", func() bool { return fk.Pending() == 1 })
	fk.Advance(500*time.Millisecond - 1)
	if s, _ := lb.Backend("a"); s.Probes != 0 {
		t.Fatal("the probe ended before its timeout")
	}
	fk.Advance(1)
	probed(t, lb, 1, "a")
	s, _ := lb.Backend("a")
	if !errors.Is(s.LastProbe.Err, ErrProbeTimeout) || s.State != Initial || s.ProbeFailures != 1 {
		t.Errorf("timed-out probe: %+v", s)
	}
}

// Probes reuse one keep-alive connection per backend, the keep-alive
// health checker a handover moves; ProbeNewConnections dials each time.
func TestProbeConnections(t *testing.T) {
	for _, c := range []struct {
		name      string
		fresh     bool
		wantConns int64
	}{
		{"keep-alive", false, 1},
		{"new connection per probe", true, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			fk := fake()
			a := newNode(t, "a", 200)
			lb := startLB(t, Config{Backends: []Backend{a.backend()}, Clock: fk, ProbeNewConnections: c.fresh})
			probed(t, lb, 1, "a")
			for range 3 {
				tick(t, lb, fk, "a")
			}
			if got := a.probeConns.Load(); got != c.wantConns {
				t.Errorf("4 probes used %d connections, want %d", got, c.wantConns)
			}
		})
	}
}

// failDial fails dials to addr and passes the rest.
func failDial(addr string, err error) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, a string) (net.Conn, error) {
		if a == addr {
			return nil, err
		}
		return (&net.Dialer{}).DialContext(ctx, network, a)
	}
}

// With Redispatch a failed dial moves to the next healthy backend;
// without it the client is reset.
func TestDialFailures(t *testing.T) {
	refused := errors.New("connection refused")
	for _, c := range []struct {
		name       string
		redispatch bool
		want       []string
		wantErrs   int
	}{
		{"redispatch", true, []string{"b", "b", "b", "b"}, 0},
		{"reset", false, []string{"", "b", "", "b"}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, b := newNode(t, "a", 200), newNode(t, "b", 200)
			lb := startLB(t, Config{
				Backends: []Backend{a.backend(), b.backend()}, Clock: fake(),
				Redispatch: c.redispatch, Dial: failDial(a.backend().Addr, refused),
			})
			probed(t, lb, 1, "a", "b")
			var got []string
			errs := 0
			for range 4 {
				cl, err := dial(t, lb)
				if err != nil {
					errs++
				}
				got = append(got, cl.be)
				if name, ok := lb.BackendOf(cl.c.LocalAddr().String()); !ok || name != cl.be {
					t.Errorf("BackendOf = %q, %v; want %q", name, ok, cl.be)
				}
			}
			if !slices.Equal(got, c.want) || errs != c.wantErrs {
				t.Errorf("connections reached %v with %d resets, want %v with %d", got, errs, c.want, c.wantErrs)
			}
			sa, _ := lb.Backend("a")
			st := lb.Stats()
			if sa.DialFailures != 2 || sa.Routed != 0 || st.Redispatched != int64(2-c.wantErrs) || st.DialFailed != int64(c.wantErrs) {
				t.Errorf("backend a %+v, stats %+v", sa, st)
			}
		})
	}
}

// A dial that does not complete within DialTimeout fails, on the injected
// clock.
func TestDialTimeoutOnTheInjectedClock(t *testing.T) {
	fk := fake()
	a := newNode(t, "a", 200)
	hang := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}
	lb := startLB(t, Config{Backends: []Backend{a.backend()}, Clock: fk, Dial: hang, DialTimeout: 2 * time.Second, ProbeInterval: time.Hour, ProbeTimeout: time.Second})
	probed(t, lb, 1, "a")
	nc, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", lb.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nc.Close() }()
	waitFor(t, "the backend dial in flight", func() bool { s, _ := lb.Backend("a"); return s.Active == 1 })
	waitFor(t, "the dial timeout armed", func() bool { return fk.Pending() == 2 }) // the probe timer and the dial timeout
	fk.Advance(2 * time.Second)
	waitFor(t, "the dial to fail", func() bool { return lb.Stats().DialFailed == 1 })
	if _, err := nc.Read(make([]byte, 1)); err == nil {
		t.Error("the client connection survived a failed dial")
	}
}

// MaxConnsPerBackend caps each backend (Capacity planning's L4 connection
// cap); MaxConns caps the balancer.
func TestConnectionCaps(t *testing.T) {
	a, b := newNode(t, "a", 200), newNode(t, "b", 200)
	lb := startLB(t, Config{Backends: []Backend{a.backend(), b.backend()}, Clock: fake(), MaxConnsPerBackend: 1})
	probed(t, lb, 1, "a", "b")
	first, second := mustDial(t, lb), mustDial(t, lb)
	if first.be != "a" || second.be != "b" {
		t.Fatalf("first two connections reached %s and %s", first.be, second.be)
	}
	if _, err := dial(t, lb); err == nil || lb.Stats().AtCapacity != 1 {
		t.Fatalf("third connection at capacity: err %v, stats %+v", err, lb.Stats())
	}
	_ = first.c.Close()
	waitFor(t, "a's connection to end", func() bool { s, _ := lb.Backend("a"); return s.Active == 0 })
	if c := mustDial(t, lb); c.be != "a" {
		t.Errorf("after a freed a slot the connection reached %s", c.be)
	}

	c := newNode(t, "c", 200)
	small := startLB(t, Config{Backends: []Backend{c.backend()}, Clock: fake(), MaxConns: 1})
	probed(t, small, 1, "c")
	mustDial(t, small)
	if _, err := dial(t, small); err == nil || small.Stats().Overflow != 1 {
		t.Errorf("connection beyond MaxConns: err %v, stats %+v", err, small.Stats())
	}
}

// A connection idle for IdleTimeout closes, traffic restarting the count;
// the default stays above the Node's 120 s.
func TestIdleTimeoutOnTheInjectedClock(t *testing.T) {
	if DefaultIdleTimeout <= NodeIdleTimeout {
		t.Fatalf("DefaultIdleTimeout %v not above the Node's %v", DefaultIdleTimeout, NodeIdleTimeout)
	}
	fk := fake()
	a := newNode(t, "a", 200)
	lb := startLB(t, Config{Backends: []Backend{a.backend()}, Clock: fk, IdleTimeout: 10 * time.Second, ProbeInterval: time.Hour, ProbeTimeout: time.Second})
	probed(t, lb, 1, "a")
	c := mustDial(t, lb)
	fk.Advance(6 * time.Second)
	c.roundTrip(t, "ping")
	fk.Advance(6 * time.Second)
	if lb.Stats().IdleClosed != 0 {
		t.Fatal("closed 6s after traffic, want 10s")
	}
	c.roundTrip(t, "still open")
	fk.Advance(10*time.Second - 1)
	if lb.Stats().IdleClosed != 0 {
		t.Fatal("closed before 10s of idleness")
	}
	fk.Advance(1)
	if _, err := c.r.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("read after the idle timeout = %v, want EOF", err)
	}
	waitFor(t, "the connection to end", func() bool { return lb.Stats().Active == 0 })
	if ci, _ := lb.ConnOf(c.c.LocalAddr().String()); !ci.IdleClosed || lb.Stats().IdleClosed != 1 {
		t.Errorf("ConnOf %+v, stats %+v", ci, lb.Stats())
	}

	never := startLB(t, Config{Backends: []Backend{a.backend()}, Clock: fk, IdleTimeout: -1, ProbeInterval: time.Hour, ProbeTimeout: time.Second})
	probed(t, never, 1, "a")
	c = mustDial(t, never)
	fk.Advance(24 * time.Hour)
	c.roundTrip(t, "alive")
}

// A backend joining while another serves ramps from 10% to a full share
// over SlowStart (Deployment topologies: 30 to 60 s, target): one added
// by AddBackend, as on scale-out, and one returning from unhealthy, as a
// restarted Node.
func TestSlowStartRamp(t *testing.T) {
	fk := fake()
	a, b := newNode(t, "a", 200), newNode(t, "b", 200)
	lb := startLB(t, Config{Backends: []Backend{a.backend()}, Clock: fk})
	probed(t, lb, 1, "a")
	if err := lb.AddBackend(t.Context(), b.backend()); err != nil {
		t.Fatal(err)
	}
	probed(t, lb, 1, "b")
	if s, _ := lb.Backend("b"); s.State != Healthy || s.Weight != MinSlowStartWeight {
		t.Fatalf("b on its first success: %+v, want healthy at weight %v", s, MinSlowStartWeight)
	}
	ramp(t, lb, fk)

	// b fails out and returns: slow start again.
	b.status.Store(503)
	for range DefaultUnhealthyThreshold {
		tick(t, lb, fk, "a", "b")
	}
	b.status.Store(200)
	for range DefaultHealthyThreshold {
		tick(t, lb, fk, "a", "b")
	}
	if s, _ := lb.Backend("b"); s.State != Healthy || s.Weight != MinSlowStartWeight {
		t.Fatalf("b back from unhealthy: %+v, want healthy at weight %v", s, MinSlowStartWeight)
	}
}

// ramp checks b's slow start from its first instant: 10%, half way at
// 15 s, full at 30 s.
func ramp(t *testing.T, lb *Balancer, fk *clocktest.Fake) {
	t.Helper()
	if got := count(backends(t, lb, 11)); got["a"] != 10 || got["b"] != 1 {
		t.Errorf("at 10%% weight 11 connections split %v, want a 10, b 1", got)
	}
	for range 15 {
		tick(t, lb, fk, "a", "b")
	}
	if s, _ := lb.Backend("b"); s.Weight != 0.5 {
		t.Fatalf("halfway through slow start b weighs %v, want 0.5", s.Weight)
	}
	if got := backends(t, lb, 3); !slices.Equal(got, []string{"a", "b", "a"}) {
		t.Errorf("at half weight connections reached %v, want a, b, a", got)
	}
	for range 15 {
		tick(t, lb, fk, "a", "b")
	}
	if s, _ := lb.Backend("b"); s.Weight != 1 {
		t.Fatalf("after slow start b weighs %v, want 1", s.Weight)
	}
	if got := count(backends(t, lb, 4)); got["a"] != 2 || got["b"] != 2 {
		t.Errorf("after slow start 4 connections split %v, want 2 each", got)
	}
}

// Backends configured at Start join together at full weight, whichever
// answers its first probe first.
func TestStartupJoinsAtFullWeight(t *testing.T) {
	fk := fake()
	a, b := newNode(t, "a", 200), newNode(t, "b", 503)
	lb := startLB(t, Config{Backends: []Backend{a.backend(), b.backend()}, Clock: fk})
	probed(t, lb, 1, "a", "b")
	b.status.Store(200)
	tick(t, lb, fk, "a", "b")
	for _, name := range []string{"a", "b"} {
		if s, _ := lb.Backend(name); s.State != Healthy || s.Weight != 1 {
			t.Errorf("backend %s after startup: %+v, want healthy at full weight", name, s)
		}
	}
}

// A backend alone, or with slow start disabled, takes a full share at
// once.
func TestNoSlowStart(t *testing.T) {
	fk := fake()
	a := newNode(t, "a", 200)
	lb := startLB(t, Config{Backends: []Backend{a.backend()}, Clock: fk})
	probed(t, lb, 1, "a")
	if s, _ := lb.Backend("a"); s.Weight != 1 {
		t.Errorf("a backend healthy alone weighs %v, want 1", s.Weight)
	}
	b := newNode(t, "b", 200)
	off := startLB(t, Config{Backends: []Backend{a.backend()}, Clock: fk, SlowStart: -1})
	probed(t, off, 1, "a")
	if err := off.AddBackend(t.Context(), b.backend()); err != nil {
		t.Fatal(err)
	}
	probed(t, off, 1, "b")
	if s, _ := off.Backend("b"); s.Weight != 1 || s.State != Healthy {
		t.Errorf("with slow start off a joining backend: %+v, want full weight", s)
	}
	if DefaultSlowStart < 30*time.Second || DefaultSlowStart > 60*time.Second {
		t.Errorf("DefaultSlowStart %v outside 30 to 60 s", DefaultSlowStart)
	}
}

// BackendOf attributes a client address to the backend that carried its
// connection, open or among the last MaxConns closed.
func TestBackendOfAttribution(t *testing.T) {
	a, b := newNode(t, "a", 200), newNode(t, "b", 200)
	lb := startLB(t, Config{Backends: []Backend{a.backend(), b.backend()}, Clock: fake(), MaxConns: 2})
	probed(t, lb, 1, "a", "b")
	var locals []string
	for range 3 {
		c := mustDial(t, lb)
		local := c.c.LocalAddr().String()
		if name, ok := lb.BackendOf(local); !ok || name != c.be {
			t.Errorf("open connection: BackendOf = %q, %v; want %q", name, ok, c.be)
		}
		if conns := lb.Conns(); len(conns) != 1 || conns[0].Client != local {
			t.Errorf("Conns = %+v", conns)
		}
		_ = c.c.Close()
		waitFor(t, "the connection to end", func() bool { return lb.Stats().Active == 0 })
		locals = append(locals, local)
	}
	// The history keeps MaxConns = 2 closed connections.
	if _, ok := lb.BackendOf(locals[0]); ok {
		t.Error("the oldest closed connection is still attributed beyond MaxConns")
	}
	for i, want := range []string{"b", "a"} {
		if name, ok := lb.BackendOf(locals[i+1]); !ok || name != want {
			t.Errorf("closed connection %d: BackendOf = %q, %v; want %q", i+1, name, ok, want)
		}
	}
	if _, ok := lb.BackendOf("127.0.0.1:1"); ok {
		t.Error("an unknown client is attributed")
	}
}

// AddBackend joins a backend after its first success; RemoveBackend stops
// new connections while open ones continue.
func TestAddAndRemoveBackend(t *testing.T) {
	a, b := newNode(t, "a", 200), newNode(t, "b", 200)
	lb := startLB(t, Config{Backends: []Backend{a.backend()}, Clock: fake(), SlowStart: -1})
	probed(t, lb, 1, "a")
	open := mustDial(t, lb)
	if err := lb.AddBackend(t.Context(), b.backend()); err != nil {
		t.Fatal(err)
	}
	if err := lb.WaitHealthy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := lb.RemoveBackend("a"); err != nil {
		t.Fatal(err)
	}
	if got := backends(t, lb, 2); !slices.Equal(got, []string{"b", "b"}) {
		t.Errorf("after removing a: %v", got)
	}
	open.roundTrip(t, "still served by a")
	if _, ok := lb.Backend("a"); ok || len(lb.Backends()) != 1 {
		t.Error("a removed backend is still listed")
	}
	for _, c := range []struct {
		name string
		err  error
	}{
		{"duplicate", lb.AddBackend(t.Context(), b.backend())},
		{"no name", lb.AddBackend(t.Context(), Backend{Addr: "127.0.0.1:1", ReadyURL: "http://127.0.0.1:1/readyz"})},
		{"remove unknown", lb.RemoveBackend("zz")},
		{"wait unknown", lb.WaitState(t.Context(), "zz", Healthy)},
		{"probes unknown", lb.WaitProbes(t.Context(), "zz", 1)},
		{"healthy unknown", lb.WaitHealthy(t.Context(), "zz")},
	} {
		if c.err == nil {
			t.Errorf("%s: succeeded, want an error", c.name)
		}
	}
	if err := lb.WaitState(ctxDone(), "b", Unhealthy); !errors.Is(err, context.Canceled) {
		t.Errorf("WaitState on an ended context = %v", err)
	}
}

// Close ends every connection and goroutine; later calls fail with
// ErrClosed, and the end of Start's context closes the balancer too.
func TestCloseEndsEverything(t *testing.T) {
	a := newNode(t, "a", 200)
	lb := startLB(t, Config{Backends: []Backend{a.backend()}, Clock: fake()})
	probed(t, lb, 1, "a")
	c := mustDial(t, lb)
	if err := lb.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lb.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if _, err := c.r.ReadByte(); err == nil {
		t.Error("a connection survived Close")
	}
	if err := lb.AddBackend(t.Context(), newNode(t, "b", 200).backend()); !errors.Is(err, ErrClosed) {
		t.Errorf("AddBackend after Close = %v", err)
	}
	if err := lb.RemoveBackend("a"); !errors.Is(err, ErrClosed) {
		t.Errorf("RemoveBackend after Close = %v", err)
	}
	if err := lb.WaitState(t.Context(), "a", Unhealthy); !errors.Is(err, ErrClosed) {
		t.Errorf("WaitState after Close = %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	other, err := Start(ctx, Config{Backends: []Backend{a.backend()}, Clock: fake()})
	if err != nil {
		t.Fatal(err)
	}
	late := newNode(t, "c", 200).backend()
	cancel()
	waitFor(t, "the context to close the balancer", func() bool {
		return errors.Is(other.AddBackend(t.Context(), late), ErrClosed)
	})
}

func TestStartRejects(t *testing.T) {
	good := Backend{Name: "a", Addr: "127.0.0.1:1", ReadyURL: "http://127.0.0.1:2/readyz"}
	for _, c := range []struct {
		name string
		cfg  Config
	}{
		{"negative interval", Config{ProbeInterval: -1}},
		{"negative threshold", Config{UnhealthyThreshold: -1}},
		{"negative cap", Config{MaxConnsPerBackend: -1}},
		{"probe timeout above interval", Config{ProbeInterval: time.Second, ProbeTimeout: 2 * time.Second}},
		{"backend without name", Config{Backends: []Backend{{Addr: good.Addr, ReadyURL: good.ReadyURL}}}},
		{"backend address", Config{Backends: []Backend{{Name: "a", Addr: "nowhere", ReadyURL: good.ReadyURL}}}},
		{"ready URL", Config{Backends: []Backend{{Name: "a", Addr: good.Addr, ReadyURL: "ftp://x/readyz"}}}},
		{"duplicate backend", Config{Backends: []Backend{good, good}}},
		{"listen address", Config{Listen: "256.0.0.1:0"}},
	} {
		if b, err := Start(t.Context(), c.cfg); err == nil {
			_ = b.Close()
			t.Errorf("%s: Start succeeded, want an error", c.name)
		}
	}
	for s, want := range map[State]string{Initial: "initial", Healthy: "healthy", Unhealthy: "unhealthy", State(7): "State(7)"} {
		if s.String() != want {
			t.Errorf("State(%d).String() = %q, want %q", int(s), s.String(), want)
		}
	}
}

// The transition log keeps the last TransitionLogSize transitions, oldest
// first.
func TestTransitionLogIsBounded(t *testing.T) {
	a := newNode(t, "a", 200)
	a.hang.Store(true) // the prober's own probe never ends, so only the test records
	lb := startLB(t, Config{Backends: []Backend{a.backend()}, Clock: fake()})
	waitFor(t, "the probe in flight", func() bool { return a.probeConns.Load() == 1 })
	lb.mu.Lock()
	be := lb.byName["a"]
	lb.mu.Unlock()
	// Each cycle flips the backend twice: two successes, then
	// UnhealthyThreshold failures.
	for {
		if s, _ := lb.Backend("a"); s.Transitions > TransitionLogSize+10 {
			break
		}
		for range DefaultHealthyThreshold {
			lb.record(be, ProbeResult{OK: true})
		}
		for range DefaultUnhealthyThreshold {
			lb.record(be, ProbeResult{Status: 503})
		}
	}
	tr := lb.Transitions()
	s, _ := lb.Backend("a")
	if len(tr) != TransitionLogSize || s.Transitions <= TransitionLogSize {
		t.Fatalf("kept %d of %d transitions, want the last %d", len(tr), s.Transitions, TransitionLogSize)
	}
	for i := 1; i < len(tr); i++ {
		if tr[i].From != tr[i-1].To {
			t.Fatalf("transitions %d and %d do not chain: %+v then %+v", i-1, i, tr[i-1], tr[i])
		}
	}
	if last := tr[len(tr)-1]; last.To != Unhealthy {
		t.Errorf("newest transition %+v, want the last flip to unhealthy", last)
	}
}
