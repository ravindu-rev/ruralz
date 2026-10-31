// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package listener

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/reuseport"
)

// foreignPort listens with SO_REUSEPORT on a free loopback port, as a
// ruralzd with another RURALZ_DATA_DIR would, and returns the port.
func foreignPort(t *testing.T) int {
	t.Helper()
	ln, err := reuseport.Listen(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// TestProbeBindRefusesAForeignPort checks the probe bind of
// docs/architecture/03-data-plane.md "Listeners and protocols": a lock
// holder's Prepare of a port another process listens on with
// SO_REUSEPORT, which the bind alone would join, is RZ-CFG-039, while a
// handover successor skips the probe and binds beside the holder's socket.
func TestProbeBindRefusesAForeignPort(t *testing.T) {
	port := foreignPort(t)
	holder := newHarness(t, func(o *Options) { o.Steer = true })
	_, err := holder.set.Prepare(t.Context(), snap(httpListener("web", port)), nil)
	if code, _ := errcode.CodeOf(err); code != codeBind {
		t.Fatalf("holder Prepare on a foreign port = %v, want %s", err, codeBind)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("holder Prepare error = %v, want it to wrap EADDRINUSE", err)
	}
	if holder.set.Bound() {
		t.Error("a refused Prepare left a socket bound")
	}

	successor := newHarness(t)
	p, err := successor.set.Prepare(t.Context(), snap(httpListener("web", port)), nil)
	if err != nil {
		t.Fatalf("successor Prepare beside the listening socket: %v", err)
	}
	p.Abort()
}

// TestProbeBindSkipsPortsTheSetServes checks that a holder replacing a
// listener on a port it serves binds the new socket without a probe, which
// its own listening socket would refuse.
func TestProbeBindSkipsPortsTheSetServes(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Steer = true })
	h.activate(t, snap(httpListener("web", 0)), nil)
	next := httpListener("web", portOf(t, h.addr(t, "web")))
	next.ProxyProtocol = true
	p, err := h.set.Prepare(t.Context(), snap(next), nil)
	if err != nil {
		t.Fatalf("Prepare of a replacement on a served port: %v", err)
	}
	p.Abort()
}

// TestPinTurnsTheProbeOn checks that a successor that took the lock
// probes the ports it does not serve yet: after Pin, a Prepare of a port
// another process listens on is RZ-CFG-039.
func TestPinTurnsTheProbeOn(t *testing.T) {
	port := foreignPort(t)
	h := newHarness(t)
	if err := h.set.Pin(); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	_, err := h.set.Prepare(t.Context(), snap(httpListener("web", port)), nil)
	if code, _ := errcode.CodeOf(err); code != codeBind {
		t.Fatalf("Prepare after Pin on a foreign port = %v, want %s", err, codeBind)
	}
}

// TestRePinAfterLostSuccessor checks the re-pin of ADR-0015 "Steering": a
// holder steered to a successor that is lost during the linger pins each
// group back to its own sockets, so every new connection reaches the
// holder even while the lost successor's socket still listens; once that
// socket is gone the holder pins its later binds again.
func TestRePinAfterLostSuccessor(t *testing.T) {
	for _, tt := range []struct {
		name   string
		closed bool // the lost successor's sockets are closed
	}{
		{name: "successor still listening"},
		{name: "successor closed", closed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			holder := newHarness(t, func(o *Options) { o.Steer = true; o.Handler = newTestHandler("holder") })
			holder.activate(t, snap(httpListener("web", 0)), nil)
			addr := holder.addr(t, "web")
			successor := newHarness(t, func(o *Options) { o.Handler = newTestHandler("successor") })
			successor.activate(t, snap(httpListener("web", portOf(t, addr))), nil)
			if err := holder.set.SteerToSuccessor(); err != nil {
				t.Fatalf("SteerToSuccessor: %v", err)
			}
			if got := tagsOf(t, addr, nil); got["successor"] != connections {
				t.Fatalf("steered: %v, want every connection on the successor", got)
			}
			if tt.closed {
				_ = successor.set.Close()
			}
			if err := holder.set.Pin(); err != nil {
				t.Fatalf("re-pin: %v", err)
			}
			if got := tagsOf(t, addr, nil); got["holder"] != connections {
				t.Fatalf("re-pinned: %v, want every connection on the holder", got)
			}
			if !tt.closed {
				return
			}
			next := httpListener("web", portOf(t, addr))
			next.ProxyProtocol = true
			holder.activate(t, snap(next), nil)
			hdr := proxyV2(netip.MustParseAddrPort("192.0.2.40:1000"), netip.MustParseAddrPort("198.51.100.1:80"))
			if got := tagsOf(t, addr, hdr); got["holder"] != connections {
				t.Fatalf("a replacement after the re-pin: %v, want every connection on the new socket", got)
			}
		})
	}
}

// TestWaitLingersUntilTheLingerEnds checks that WaitLingers returns at
// once with no lingering socket, waits while a replaced socket lingers,
// returning the context's error when it ends first, and returns once the
// Linger ends.
func TestWaitLingersUntilTheLingerEnds(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Steer = true })
	if err := h.set.WaitLingers(t.Context()); err != nil {
		t.Fatalf("WaitLingers with no socket = %v, want nil", err)
	}
	h.activate(t, snap(httpListener("web", 0)), nil)
	if err := h.set.WaitLingers(t.Context()); err != nil {
		t.Fatalf("WaitLingers with no lingering socket = %v, want nil", err)
	}
	next := httpListener("web", portOf(t, h.addr(t, "web")))
	next.ProxyProtocol = true
	h.activate(t, snap(next), nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.set.WaitLingers(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitLingers during the Linger = %v, want context.Canceled", err)
	}
	done := make(chan error, 1)
	go func() { done <- h.set.WaitLingers(t.Context()) }()
	h.clk.Advance(Linger)
	if err := recv(t, "WaitLingers to return", done); err != nil {
		t.Fatalf("WaitLingers after the Linger = %v, want nil", err)
	}
}
