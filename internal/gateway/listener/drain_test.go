// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// TestShutdownDrainsConnections checks the Drain's stop-accepting step:
// the socket closes, an idle HTTP/1.1 connection closes, a busy one
// finishes with Connection: close, an HTTP/2 stream in flight finishes,
// and Shutdown returns once every connection ended.
func TestShutdownDrainsConnections(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("web", 0)), nil)
	ep := h.endpoint(t, "web")
	addr := h.addr(t, "web")
	url := "http://" + addr

	idle := dialRaw(t, addr, nil)
	idle.mustRoundTrip(t)
	busy := dialRaw(t, addr, nil)
	busyDone := make(chan reply, 1)
	go func() {
		r, err := busy.roundTrip(context.WithoutCancel(t.Context()), "/block", nil)
		if err != nil {
			t.Errorf("busy HTTP/1.1 request: %v", err)
		}
		busyDone <- r
	}()
	recv(t, "the busy HTTP/1.1 request", h.handler.entered)
	h2c, _ := client(t, nil, false, false, true)
	streamDone := make(chan string, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), http.MethodGet, url+"/block", nil)
		if err != nil {
			streamDone <- err.Error()
			return
		}
		resp, err := h2c.Do(req)
		if err != nil {
			streamDone <- err.Error()
			return
		}
		_ = resp.Body.Close()
		streamDone <- resp.Proto
	}()
	recv(t, "the HTTP/2 stream", h.handler.entered)

	shut := make(chan error, 1)
	go func() { shut <- h.set.Shutdown(context.WithoutCancel(t.Context())) }()
	recv(t, "the socket to close", ep.stopped)
	if !refused(t, addr) {
		t.Error("the socket accepted a connection after Shutdown")
	}
	if !idle.closedByPeer(t) {
		t.Error("idle connection still open")
	}
	h.handler.unblock()
	if r := recv(t, "the busy response", busyDone); !r.close {
		t.Errorf("busy HTTP/1.1 response = %+v, want Connection: close", r)
	}
	if proto := recv(t, "the HTTP/2 stream", streamDone); proto != "HTTP/2.0" {
		t.Errorf("HTTP/2 stream in flight: %q, want it finished", proto)
	}
	h2c.CloseIdleConnections()
	if err := recv(t, "Shutdown", shut); err != nil {
		t.Errorf("Shutdown = %v, want nil", err)
	}
}

// TestShutdownDeadlineClosesConnections checks that a Drain whose context
// ends closes the remaining connections and returns the context's error.
func TestShutdownDeadlineClosesConnections(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("web", 0)), nil)
	busy := dialRaw(t, h.addr(t, "web"), nil)
	busyDone := make(chan error, 1)
	go func() {
		_, err := busy.roundTrip(context.WithoutCancel(t.Context()), "/block", nil)
		busyDone <- err
	}()
	recv(t, "the busy request", h.handler.entered)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.set.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Shutdown with an ended context = %v, want context.Canceled", err)
	}
	if err := recv(t, "the busy connection", busyDone); err == nil {
		t.Error("the busy connection survived the Drain deadline")
	}
}

// TestStopAcceptingKeepsServing checks the handover's linger end: every
// socket closes while the Servers keep serving their connections, and no
// Hot Reload prepares afterwards.
func TestStopAcceptingKeepsServing(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("web", 0)), nil)
	addr := h.addr(t, "web")
	c := dialRaw(t, addr, nil)
	c.mustRoundTrip(t)

	h.set.StopAccepting()
	if !refused(t, addr) {
		t.Error("the socket accepted a connection after StopAccepting")
	}
	if r := c.mustRoundTrip(t); r.status != http.StatusOK || r.close {
		t.Errorf("open connection after StopAccepting: %d, close %t; want served and kept", r.status, r.close)
	}
	if _, err := h.set.Prepare(t.Context(), snap(httpListener("web", 0)), nil); !errors.Is(err, ErrStopped) {
		t.Errorf("Prepare after StopAccepting = %v, want ErrStopped", err)
	}
}

// TestCloseEndsEverything checks Close: connections close, Close is
// idempotent, and nothing prepares or shuts down afterwards.
func TestCloseEndsEverything(t *testing.T) {
	p := newTestPKI(t)
	h := newHarness(t)
	h.activate(t, snap(httpListener("web", 0), httpsListener("api", false, p.cert(t, "api", "127.0.0.1"))), p.secrets)
	c := dialRaw(t, h.addr(t, "web"), nil)
	c.mustRoundTrip(t)
	if err := h.set.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if !c.closedByPeer(t) {
		t.Error("a connection survived Close")
	}
	if n := p.secrets.watches.Load(); n != 0 {
		t.Errorf("%d secret watches left after Close", n)
	}
	if err := h.set.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if _, err := h.set.Prepare(t.Context(), snap(httpListener("web", 0)), nil); !errors.Is(err, ErrStopped) {
		t.Errorf("Prepare after Close = %v, want ErrStopped", err)
	}
	if err := h.set.Shutdown(t.Context()); !errors.Is(err, ErrStopped) {
		t.Errorf("Shutdown after Close = %v, want ErrStopped", err)
	}
}

// TestNewRequiresHandlerAndCeiling checks the required Options.
func TestNewRequiresHandlerAndCeiling(t *testing.T) {
	if _, err := New(Options{Conns: newHarness(t).conns}); !errors.Is(err, ErrOptions) {
		t.Errorf("New without Handler = %v, want ErrOptions", err)
	}
	if _, err := New(Options{Handler: http.NotFoundHandler()}); !errors.Is(err, ErrOptions) {
		t.Errorf("New without Conns = %v, want ErrOptions", err)
	}
}
