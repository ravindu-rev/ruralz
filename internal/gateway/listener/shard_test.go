// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"context"
	"net/http"
	"testing"
)

// dialServed opens n HTTP/1.1 connections to addr one after another, each
// answered once before the next is dialed, so connection k is the k-th the
// dispatching listener accepts and goes to shard k modulo Shards.
func dialServed(t *testing.T, addr string, n int) []*rawConn {
	t.Helper()
	conns := make([]*rawConn, n)
	for k := range conns {
		conns[k] = dialRaw(t, addr, nil)
		if r := conns[k].mustRoundTrip(t); r.status != http.StatusOK {
			t.Fatalf("connection %d: status %d", k, r.status)
		}
	}
	return conns
}

// servedKeptAlive reports whether c answers a request and stays open.
func servedKeptAlive(t *testing.T, c *rawConn) bool {
	t.Helper()
	r, err := c.roundTrip(t.Context(), "/", nil)
	return err == nil && r.status == http.StatusOK && !r.close
}

// TestEveryListenerServesThroughShards checks that a client listener is
// served by Shards = 8 Server shards (the K of the Drain timeline), each
// with its own channel-fed listener, its position and the Set's handler.
func TestEveryListenerServesThroughShards(t *testing.T) {
	if Shards != 8 {
		t.Fatalf("Shards = %d, want K = 8", Shards)
	}
	h := newHarness(t)
	h.activate(t, snap(httpListener("web", 0)), nil)
	g := h.endpoint(t, "web").group.Load()
	listeners := make(map[*shardListener]bool)
	h.set.mu.Lock()
	defer h.set.mu.Unlock()
	for i, sv := range g.shards {
		if sv == nil || sv.shard != i {
			t.Fatalf("shard %d = %+v, want a Server at position %d", i, sv, i)
		}
		if _, ok := h.set.servers[sv]; !ok {
			t.Errorf("shard %d is not tracked by the Set", i)
		}
		if sv.http.Handler != h.set.opts.Handler {
			t.Errorf("shard %d does not share the Set's handler", i)
		}
		listeners[sv.ln] = true
	}
	if len(listeners) != Shards {
		t.Errorf("%d distinct shard listeners, want %d", len(listeners), Shards)
	}
}

// TestShutdownShardClosesOnlyItsIdleConnections checks the round-robin
// dispatch and one shard's shutdown: of 16 idle HTTP/1.1 connections
// accepted in order, shutting down shard 3 closes connections 3 and 11
// only; every other connection keeps serving with keep-alive.
func TestShutdownShardClosesOnlyItsIdleConnections(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("web", 0)), nil)
	conns := dialServed(t, h.addr(t, "web"), 2*Shards)

	h.set.ShutdownShard(3)
	for _, k := range []int{3, 3 + Shards} {
		if !conns[k].closedByPeer(t) {
			t.Errorf("connection %d of shard 3 stayed open", k)
		}
	}
	for k, c := range conns {
		if k%Shards == 3 {
			continue
		}
		if !servedKeptAlive(t, c) {
			t.Errorf("connection %d of shard %d was not served with keep-alive after shard 3 shut down", k, k%Shards)
		}
	}
}

// TestShutdownShardGivesBusyConnectionsConnectionClose checks that a busy
// HTTP/1.1 connection of the shard that shuts down gets Connection: close
// on its response, while a busy connection of another shard keeps
// keep-alive.
func TestShutdownShardGivesBusyConnectionsConnectionClose(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("web", 0)), nil)
	addr := h.addr(t, "web")
	results := make([]chan reply, 2)
	for k := range results {
		c := dialRaw(t, addr, nil) // connection k, on shard k
		results[k] = make(chan reply, 1)
		go func() {
			r, err := c.roundTrip(context.WithoutCancel(t.Context()), "/block", nil)
			if err != nil {
				t.Errorf("busy request on shard %d: %v", k, err)
			}
			results[k] <- r
		}()
		recv(t, "the busy request", h.handler.entered)
	}

	h.set.ShutdownShard(0)
	h.handler.unblock()
	if r := recv(t, "shard 0's response", results[0]); !r.close {
		t.Errorf("busy response on the shut down shard = %+v, want Connection: close", r)
	}
	if r := recv(t, "shard 1's response", results[1]); r.close {
		t.Errorf("busy response on a serving shard = %+v, want keep-alive", r)
	}
}

// TestShutdownShardSendsGOAWAY checks that shutting down a shard sends
// GOAWAY on its HTTP/2 connections only: the client of shard 0 closes its
// connection and dials anew for its next request, while the client of
// shard 1 keeps its connection. The GOAWAY goes out after ShutdownShard
// returns, so the next requests wait until the client closed shard 0's
// connection, which it does only once it processed the GOAWAY.
func TestShutdownShardSendsGOAWAY(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("web", 0)), nil)
	url := "http://" + h.addr(t, "web") + "/"
	onShard0, dials0 := client(t, nil, false, false, true)
	onShard1, dials1 := client(t, nil, false, false, true)
	for _, c := range []*http.Client{onShard0, onShard1} {
		if _, body := get(t, c, url); field(body, 0) != "HTTP/2.0" {
			t.Fatalf("h2c: %q", body)
		}
	}

	h.set.ShutdownShard(0)
	recv(t, "GOAWAY to end shard 0's connection", dials0.closed(t, 0))
	for _, c := range []*http.Client{onShard0, onShard1} {
		if status, _ := get(t, c, url); status != http.StatusOK {
			t.Fatalf("request after shard 0 shut down: %d", status)
		}
	}
	if n := dials0.count(); n != 2 {
		t.Errorf("shard 0's client dialed %d times, want 2: GOAWAY ended its connection", n)
	}
	if n := dials1.count(); n != 1 {
		t.Errorf("shard 1's client dialed %d times, want 1: its connection got no GOAWAY", n)
	}
	select {
	case <-dials1.closed(t, 0):
		t.Error("shard 1's client closed its connection: it got a GOAWAY")
	default:
	}
}

// TestDispatcherSkipsShutDownShards checks a Set still accepting after a
// shard shut down: the shut down shard's turn passes to the next shard
// still serving, so every new connection is served; once every shard shut
// down, a new connection is closed unanswered.
func TestDispatcherSkipsShutDownShards(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("web", 0)), nil)
	addr := h.addr(t, "web")
	g := h.endpoint(t, "web").group.Load()
	h.set.ShutdownShard(0)
	recv(t, "shard 0 to stop", g.shards[0].done)

	dialServed(t, addr, Shards) // the first turn is shard 0's

	for i := 1; i < Shards; i++ {
		h.set.ShutdownShard(i)
	}
	for i := range Shards {
		recv(t, "every shard to stop", g.shards[i].done)
	}
	if r, err := dialRaw(t, addr, nil).roundTrip(t.Context(), "/", nil); err == nil {
		t.Errorf("a connection was answered %d with every shard shut down, want it closed", r.status)
	}
}

// TestShutdownShardOutOfRange checks that a shard index outside
// [0, Shards) shuts nothing down.
func TestShutdownShardOutOfRange(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("web", 0)), nil)
	conns := dialServed(t, h.addr(t, "web"), Shards)
	h.set.ShutdownShard(-1)
	h.set.ShutdownShard(Shards)
	h.set.mu.Lock()
	for sv := range h.set.servers {
		if sv.shutting {
			t.Errorf("shard %d is shutting down", sv.shard)
		}
	}
	h.set.mu.Unlock()
	for k, c := range conns {
		if !servedKeptAlive(t, c) {
			t.Errorf("connection %d was not served with keep-alive", k)
		}
	}
}

// TestShutdownEndsShardsShutDownEarlier checks the Drain deadline after
// the shard waves: a busy connection of a shard ShutdownShard shut down
// earlier is closed when Shutdown's context ends, and Shutdown returns
// that context's error.
func TestShutdownEndsShardsShutDownEarlier(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("web", 0)), nil)
	busy := dialRaw(t, h.addr(t, "web"), nil) // connection 0, on shard 0
	busyDone := make(chan error, 1)
	go func() {
		_, err := busy.roundTrip(context.WithoutCancel(t.Context()), "/block", nil)
		busyDone <- err
	}()
	recv(t, "the busy request", h.handler.entered)
	h.set.StopAccepting()
	h.set.ShutdownShard(0)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.set.Shutdown(ctx); err == nil {
		t.Error("Shutdown with an ended context = nil, want its error")
	}
	if err := recv(t, "the busy connection", busyDone); err == nil {
		t.Error("the busy connection of an earlier shard shutdown survived the Drain deadline")
	}
}

// TestBound checks Bound: false before any listener serves and while a
// Plan's socket is only bound, true while a socket accepts, false once
// StopAccepting closed every socket.
func TestBound(t *testing.T) {
	h := newHarness(t)
	if h.set.Bound() {
		t.Error("Bound before any listener")
	}
	p, err := h.set.Prepare(t.Context(), snap(httpListener("web", 0)), nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if h.set.Bound() {
		t.Error("Bound with only a Plan's socket")
	}
	if err := p.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !h.set.Bound() {
		t.Error("not Bound while a listener serves")
	}
	h.set.StopAccepting()
	if h.set.Bound() {
		t.Error("Bound after StopAccepting")
	}
}

// TestShardTurnsSpanListeners checks that the dispatch turn order spans
// every client listener: of three connections to each of two listeners,
// dialed alternately, the Node's connection n goes to shard n modulo
// Shards, so each shard's shutdown closes exactly that one connection
// while the later ones keep serving: no shard holds more than an eighth
// of the Node's six, rounded up, as a Drain wave must
// (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md "Drain
// timeline defaults").
func TestShardTurnsSpanListeners(t *testing.T) {
	h := newHarness(t)
	h.activate(t, snap(httpListener("web", 0), httpListener("api", 0)), nil)
	web, api := h.addr(t, "web"), h.addr(t, "api")
	var conns []*rawConn
	for k := range 6 {
		addr := web
		if k%2 == 1 {
			addr = api
		}
		conns = append(conns, dialServed(t, addr, 1)...)
	}
	for shard := range len(conns) {
		h.set.ShutdownShard(shard)
		if !conns[shard].closedByPeer(t) {
			t.Errorf("connection %d stayed open after shard %d shut down", shard, shard)
		}
		for k := shard + 1; k < len(conns); k++ {
			if !servedKeptAlive(t, conns[k]) {
				t.Fatalf("connection %d closed when shard %d shut down, want shard %d alone to hold it", k, shard, k)
			}
		}
	}
}
