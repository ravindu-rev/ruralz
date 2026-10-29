// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package listener

import (
	"net/netip"
	"testing"
)

// connections is the number of fresh connections each routing check makes;
// a hashed reuseport group would spread them over its sockets.
const connections = 16

// tagsOf sends one request on each of connections fresh connections to
// addr, after a PROXY v2 header when hdr is set, and returns how many
// responses carried each handler tag ("" for a connection that got no
// response).
func tagsOf(t *testing.T, addr string, hdr []byte) map[string]int {
	t.Helper()
	tags := make(map[string]int)
	for range connections {
		c := dialRaw(t, addr, hdr)
		r, err := c.roundTrip(t.Context(), "/", nil)
		if err != nil {
			tags[""]++
		} else {
			tags[field(r.body, 6)]++
		}
		_ = c.Close()
	}
	return tags
}

// TestCommitSteersPortToNewSocket checks the steering of a Hot Reload that
// replaces a listener on its port: right after Commit, before the old
// socket's Linger ends, every new connection reaches the new socket (here
// one requiring a PROXY v2 header); after the Linger the group is pinned
// to the new socket alone.
func TestCommitSteersPortToNewSocket(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Steer = true })
	h.activate(t, snap(httpListener("web", 0)), nil)
	old := h.endpoint(t, "web")
	addr := h.addr(t, "web")

	next := httpListener("web", portOf(t, addr))
	next.ProxyProtocol = true
	h.activate(t, snap(next), nil)
	hdr := proxyV2(netip.MustParseAddrPort("192.0.2.30:1000"), netip.MustParseAddrPort("198.51.100.1:80"))
	if got := tagsOf(t, addr, hdr); got["set"] != connections {
		t.Fatalf("during the Linger: %v, want all %d connections on the new socket", got, connections)
	}
	h.clk.Advance(Linger)
	recv(t, "the replaced socket to close", old.stopped)
	if got := tagsOf(t, addr, hdr); got["set"] != connections {
		t.Fatalf("after the Linger: %v, want all %d connections on the new socket", got, connections)
	}
	h.set.mu.Lock()
	g := h.set.groups[portOf(t, addr)]
	h.set.mu.Unlock()
	if g == nil || g.Len() != 1 {
		t.Fatalf("group after the Linger = %v, want the new socket alone", g)
	}
}

// TestHandoverSteersToSuccessor checks the in-place handover of ADR-0015
// with two Sets on one port: the successor's bound socket and then its
// listening socket receive nothing while the holder's pin holds;
// SteerToSuccessor moves every new connection to the successor, which
// keeps them after the holder stops accepting and pins its own group.
func TestHandoverSteersToSuccessor(t *testing.T) {
	holder := newHarness(t, func(o *Options) { o.Steer = true; o.Handler = newTestHandler("holder") })
	holder.activate(t, snap(httpListener("web", 0)), nil)
	addr := holder.addr(t, "web")
	successor := newHarness(t, func(o *Options) { o.Handler = newTestHandler("successor") })

	p, err := successor.set.Prepare(t.Context(), snap(httpListener("web", portOf(t, addr))), nil)
	if err != nil {
		t.Fatalf("successor Prepare: %v", err)
	}
	if got := tagsOf(t, addr, nil); got["holder"] != connections {
		t.Fatalf("successor bound: %v, want every connection on the holder", got)
	}
	if err := p.Commit(); err != nil {
		t.Fatalf("successor Commit: %v", err)
	}
	if got := tagsOf(t, addr, nil); got["holder"] != connections {
		t.Fatalf("successor listening, holder pinned: %v, want every connection on the holder", got)
	}
	if err := holder.set.SteerToSuccessor(); err != nil {
		t.Fatalf("SteerToSuccessor: %v", err)
	}
	if got := tagsOf(t, addr, nil); got["successor"] != connections {
		t.Fatalf("steered: %v, want every connection on the successor", got)
	}
	holder.set.StopAccepting()
	if got := tagsOf(t, addr, nil); got["successor"] != connections {
		t.Fatalf("holder stopped accepting: %v, want every connection on the successor", got)
	}
	if err := successor.set.Pin(); err != nil {
		t.Fatalf("successor Pin: %v", err)
	}
	if got := tagsOf(t, addr, nil); got["successor"] != connections {
		t.Fatalf("successor pinned: %v, want every connection on the successor", got)
	}
}
