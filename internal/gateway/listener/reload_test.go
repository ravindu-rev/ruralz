// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/reuseport"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// TestPrepareBindsBeforeTheSwap checks that Prepare binds an added
// listener without serving it and Commit serves it: on Linux the bound
// socket refuses connections until Commit; elsewhere it already listens,
// and a connection made before Commit is served after it.
func TestPrepareBindsBeforeTheSwap(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("a", 0)), nil)
	p, err := h.set.Prepare(t.Context(), snap(httpListener("a", 0), httpListener("b", 0)), nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(p.added) != 1 || p.added[0].key.Name != "b" {
		t.Fatalf("added = %v, want b", p.added)
	}
	addr := p.added[0].local.String()
	if h.set.Addr("b") != nil {
		t.Fatal("b serves before Commit")
	}
	var early chan string
	if reuseport.Steering() {
		if !refused(t, addr) {
			t.Fatal("a bound listener accepted a connection before Commit")
		}
	} else {
		c := dialRaw(t, addr, nil)
		early = make(chan string, 1)
		go func() {
			r, err := c.roundTrip(context.WithoutCancel(t.Context()), "/", nil)
			if err != nil {
				r.body = err.Error()
			}
			early <- r.body
		}()
	}
	if err := p.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if early != nil {
		if body := recv(t, "the early connection's response", early); field(body, 1) != "b" {
			t.Errorf("connection made before Commit: %q, want served by b", body)
		}
	}
	if r := dialRaw(t, h.addr(t, "b"), nil).mustRoundTrip(t); field(r.body, 1) != "b" {
		t.Errorf("after Commit: %q", r.body)
	}
	if r := dialRaw(t, h.addr(t, "a"), nil).mustRoundTrip(t); field(r.body, 1) != "a" {
		t.Errorf("kept listener: %q", r.body)
	}
}

// TestBindFailureRejectsWithRZCFG039 checks that a port Prepare cannot
// bind rejects the Revision with RZ-CFG-039, releases what the Plan had
// built and leaves the running listeners serving.
func TestBindFailureRejectsWithRZCFG039(t *testing.T) {
	p := newTestPKI(t)
	h := newHarness(t)
	h.activate(t, snap(httpListener("a", 0)), nil)
	var lc net.ListenConfig
	taken, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()

	next := snap(httpListener("a", 0), httpsListener("c", false, p.cert(t, "c", "127.0.0.1")), httpListener("b", portOf(t, taken.Addr().String())))
	_, err = h.set.Prepare(t.Context(), next, p.secrets)
	if code, _ := errcode.CodeOf(err); code != "RZ-CFG-039" {
		t.Fatalf("Prepare error = %v, want RZ-CFG-039", err)
	}
	if n := p.secrets.watches.Load(); n != 0 {
		t.Errorf("%d secret watches left: the failed Plan kept a TLS state", n)
	}
	if r := dialRaw(t, h.addr(t, "a"), nil).mustRoundTrip(t); field(r.body, 1) != "a" {
		t.Errorf("running listener after the failure: %q", r.body)
	}
	plan, err := h.set.Prepare(t.Context(), snap(httpListener("a", 0)), nil)
	if err != nil {
		t.Fatalf("Prepare after a failed Prepare: %v", err)
	}
	plan.Abort()
}

// TestPortConflictsRejected checks that two listeners on one port, or a
// listener on the admin port, are RZ-CFG-039 before any bind.
func TestPortConflictsRejected(t *testing.T) {
	for _, tt := range []struct {
		name string
		next func() *snapshot.Snapshot
	}{
		{name: "shared port", next: func() *snapshot.Snapshot { return snap(httpListener("a", 18080), httpListener("b", 18080)) }},
		{name: "admin port", next: func() *snapshot.Snapshot { return snap(httpListener("a", 9901)) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			_, err := h.set.Prepare(t.Context(), tt.next(), nil)
			if code, _ := errcode.CodeOf(err); code != "RZ-CFG-039" {
				t.Fatalf("Prepare error = %v, want RZ-CFG-039", err)
			}
		})
	}
}

// TestAbortReleasesWhatPrepareBuilt checks that Abort closes the Plan's
// sockets and TLS states and lets the next Prepare run, and that an
// aborted Plan cannot commit.
func TestAbortReleasesWhatPrepareBuilt(t *testing.T) {
	p := newTestPKI(t)
	h := newHarness(t)
	h.activate(t, snap(httpListener("a", 0)), nil)
	plan, err := h.set.Prepare(t.Context(), snap(httpListener("a", 0), httpsListener("b", false, p.cert(t, "b", "127.0.0.1"))), p.secrets)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if p.secrets.watches.Load() == 0 {
		t.Fatal("the https listener's TLS state watches nothing")
	}
	plan.Abort()
	if n := p.secrets.watches.Load(); n != 0 {
		t.Errorf("%d secret watches left after Abort", n)
	}
	ep := plan.added[0]
	if ep.sock != nil {
		if _, err := ep.sock.Listen(); !errors.Is(err, net.ErrClosed) {
			t.Errorf("Listen on the aborted socket = %v, want net.ErrClosed", err)
		}
	} else if _, err := ep.tcp.Accept(); err == nil {
		t.Error("the aborted socket still accepts")
	}
	if err := plan.Commit(); !errors.Is(err, ErrPlanDone) {
		t.Errorf("Commit after Abort = %v, want ErrPlanDone", err)
	}
	if h.set.Addr("b") != nil {
		t.Error("the aborted listener serves")
	}
	next, err := h.set.Prepare(t.Context(), snap(httpListener("a", 0)), nil)
	if err != nil {
		t.Fatalf("Prepare after Abort: %v", err)
	}
	next.Abort()
}

// TestKeptListenerKeepsSocketAndConnections checks that a listener whose
// identity did not change keeps its socket, Server shards and connections
// across a Hot Reload, and takes the new snapshot's metric handles.
func TestKeptListenerKeepsSocketAndConnections(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("a", 0)), nil)
	ep := h.endpoint(t, "a")
	sv := ep.group.Load()
	c := dialRaw(t, h.addr(t, "a"), nil)
	c.mustRoundTrip(t)

	l := httpListener("a", 0)
	l.Hostnames = []string{"api.shop.example"}
	m := newFakeMetrics()
	l.Metrics = m.m
	h.activate(t, snap(l), nil)
	if h.endpoint(t, "a") != ep || ep.group.Load() != sv {
		t.Fatal("an unchanged listener identity got a new endpoint or Server shards")
	}
	if r := c.mustRoundTrip(t); r.status != http.StatusOK {
		t.Errorf("open connection after the reload: %d", r.status)
	}
	dialRaw(t, h.addr(t, "a"), nil).mustRoundTrip(t)
	if got := m.count(emit.ProtoHTTP1, emit.ConnAccepted); got != 1 {
		t.Errorf("new snapshot's accepted connections = %d, want 1: a new connection records to the new handles", got)
	}
}

// TestRemovedListenerStopsAcceptingAfterTheSwap checks that a removed
// listener accepts until Commit, then refuses new connections while its
// busy connection finishes with Connection: close.
func TestRemovedListenerStopsAcceptingAfterTheSwap(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("a", 0), httpListener("b", 0)), nil)
	addr := h.addr(t, "b")
	busy := dialRaw(t, addr, nil)
	busyDone := make(chan reply, 1)
	go func() {
		r, err := busy.roundTrip(context.WithoutCancel(t.Context()), "/block", nil)
		if err != nil {
			t.Errorf("busy request: %v", err)
		}
		busyDone <- r
	}()
	recv(t, "the busy request", h.handler.entered)

	p, err := h.set.Prepare(t.Context(), snap(httpListener("a", 0)), nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if r := dialRaw(t, addr, nil).mustRoundTrip(t); field(r.body, 1) != "b" {
		t.Fatalf("before Commit: %q, want b still serving", r.body)
	}
	if err := p.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !refused(t, addr) {
		t.Error("a removed listener accepted a connection after Commit")
	}
	h.handler.unblock()
	if r := recv(t, "the busy response", busyDone); !r.close {
		t.Errorf("busy response = %+v, want Connection: close", r)
	}
	if r := dialRaw(t, h.addr(t, "a"), nil).mustRoundTrip(t); field(r.body, 1) != "a" {
		t.Errorf("kept listener: %q", r.body)
	}
}

// TestReplacedListenerLingersThenStops checks a listener replaced on the
// same port (here proxyProtocol turned on): the new socket serves after
// Commit while the old one keeps accepting for the Linger; then the old
// socket closes and its Server shuts down, closing its idle connection.
func TestReplacedListenerLingersThenStops(t *testing.T) {
	if !reuseport.SharedPorts() {
		t.Skip("this platform cannot bind a second socket on a port")
	}
	h := newHarness(t, func(o *Options) { o.Steer = reuseport.Steering() })
	h.activate(t, snap(httpListener("web", 0)), nil)
	old := h.endpoint(t, "web")
	addr := h.addr(t, "web")
	conn0 := dialRaw(t, addr, nil)
	conn0.mustRoundTrip(t)

	next := httpListener("web", portOf(t, addr))
	next.ProxyProtocol = true
	h.activate(t, snap(next), nil)
	if h.endpoint(t, "web") == old {
		t.Fatal("a changed listener identity kept its endpoint")
	}
	select {
	case <-old.stopped:
		t.Fatal("the replaced socket closed before the Linger")
	default:
	}
	if r := conn0.mustRoundTrip(t); field(r.body, 1) != "web" {
		t.Errorf("old connection during the Linger: %q", r.body)
	}

	h.clk.Advance(Linger)
	recv(t, "the replaced socket to close", old.stopped)
	if !conn0.closedByPeer(t) {
		t.Error("the replaced Server kept its idle connection open")
	}
	if _, err := dialRaw(t, addr, nil).roundTrip(t.Context(), "/", nil); err == nil {
		t.Error("a connection without a PROXY header was served after the Linger")
	}
}

// TestPlanLifecycleErrors checks the Plan protocol: one pending Plan at a
// time, a Plan commits once, and nothing prepares or commits after
// StopAccepting.
func TestPlanLifecycleErrors(t *testing.T) {
	h := newHarness(t)
	p1, err := h.set.Prepare(t.Context(), snap(httpListener("a", 0)), nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := h.set.Prepare(t.Context(), snap(httpListener("a", 0)), nil); !errors.Is(err, ErrPending) {
		t.Errorf("second Prepare = %v, want ErrPending", err)
	}
	if err := p1.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := p1.Commit(); !errors.Is(err, ErrPlanDone) {
		t.Errorf("second Commit = %v, want ErrPlanDone", err)
	}
	p1.Abort()
	if h.set.Addr("a") == nil {
		t.Fatal("Abort after Commit undid the Plan")
	}
	p2, err := h.set.Prepare(t.Context(), snap(httpListener("a", 0), httpListener("b", 0)), nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.set.StopAccepting()
	if err := p2.Commit(); !errors.Is(err, ErrStopped) {
		t.Errorf("Commit after StopAccepting = %v, want ErrStopped", err)
	}
	if _, err := h.set.Prepare(t.Context(), snap(httpListener("a", 0)), nil); !errors.Is(err, ErrStopped) {
		t.Errorf("Prepare after StopAccepting = %v, want ErrStopped", err)
	}
}

// TestCleartextHopsReported checks that the http listeners are reported as
// cleartext client hops after every Commit, and none after Close.
func TestCleartextHopsReported(t *testing.T) {
	p := newTestPKI(t)
	h := newHarness(t)
	cert := p.cert(t, "c", "127.0.0.1")
	h.activate(t, snap(httpListener("a", 0), httpListener("b", 0), httpsListener("c", false, cert)), p.secrets)
	if n := h.status.hops.Load(); n != 2 {
		t.Errorf("cleartext client hops = %d, want 2", n)
	}
	h.activate(t, snap(httpListener("a", 0), httpsListener("c", false, cert)), p.secrets)
	if n := h.status.hops.Load(); n != 1 {
		t.Errorf("after removing b: %d, want 1", n)
	}
	_ = h.set.Close()
	if n := h.status.hops.Load(); n != 0 {
		t.Errorf("after Close: %d, want 0", n)
	}
}
