// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package protocol

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"golang.org/x/net/http2"

	"github.com/ravindu-rev/ruralz/internal/gateway/listener"
)

// drainDoc is the document of the Drain's listener shutdown.
const drainDoc = "docs/architecture/03-data-plane.md#sources-last-known-good-and-drain"

// drainRouteTimeout is the Route timeout of the Drain's held requests,
// beyond the end of the jitter window, 15 s after the stop signal.
const drainRouteTimeout = "60s"

// TestDrain checks one GOAWAY at Drain, the HTTP/2 frames case of
// docs/engineering/03-testing-and-quality-strategy.md "Conformance
// suites", with the rest of the listener shutdown that
// docs/architecture/03-data-plane.md "Sources, Last-Known-Good and Drain"
// states: within the 10 s jitter window after the Node stops accepting,
// each server shard's Shutdown sends one GOAWAY NO_ERROR on each of its
// HTTP/2 connections, whose last stream ID covers the streams in flight,
// which then complete before the connection closes; it closes its idle
// HTTP/1.1 connections, and a busy HTTP/1.1 connection gets Connection:
// close on its next response. One idle HTTP/1.1 connection per shard
// proves every shard shut down before the held requests answer: the
// listener hands connections to its shards round-robin. The Node then
// exits 0. The jitter window runs on the Node clock, which the test moves.
func TestDrain(t *testing.T) {
	t.Parallel()
	needsTestClock(t)
	t.Logf("rule: %s", drainDoc)
	// The held requests take the /deadline/ Routes with a timeout beyond
	// the jitter window: the Route timeout runs on the Node clock, which
	// the test moves through the window.
	g := startGateway(t, bundleOptions{routeTimeout: drainRouteTimeout})
	ctx := guard(t, deadlineWait)

	// A busy HTTP/1.1 connection: a request the origin holds.
	busy := g.dialH1(t, h1)
	defer busy.close()
	busyCase := g.origin.newCase()
	busyReq := request{Target: "/deadline/" + bHold, Header: withCase(busyCase)}
	busy.write(busyReq.h1Bytes(g.addr(h1)))
	// One idle keep-alive connection per shard of the listener web.
	idle := make([]*h1Conn, listener.Shards)
	defer func() {
		for _, c := range idle {
			if c != nil {
				c.close()
			}
		}
	}()
	for i := range idle {
		idle[i] = g.dialH1(t, h1)
		req := request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase())}
		idle[i].write(req.h1Bytes(g.addr(h1)))
		wantOK(t, idle[i].read(guard(t, exchangeGuard), req.method()))
	}
	// An HTTP/2 connection per protocol, each with a held stream.
	type held struct {
		p    proto
		c    *h2Conn
		id   uint32
		kase string
	}
	var streams []held
	defer func() {
		for _, h := range streams {
			h.c.close()
		}
	}()
	for _, p := range h2Protos() {
		h := held{p: p, c: g.dialH2(t, p), kase: g.origin.newCase()}
		h.id = h.c.newStream()
		streams = append(streams, h)
		if err := h.c.writeHeaders(h.id, request{Target: "/deadline/" + bHold, Header: withCase(h.kase)}.h2Fields(p, g.addr(p)), true); err != nil {
			t.Fatal(err)
		}
	}
	kases := []string{busyCase}
	for _, h := range streams {
		kases = append(kases, h.kase)
	}
	for _, id := range kases {
		if err := g.origin.waitHeld(ctx, id, 1); err != nil {
			t.Fatal(err)
		}
	}

	if err := g.node.Signal(ctx, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	goaways := make([]h2Event, len(streams))
	if err := advancing(ctx, g.top, func() error {
		for i, h := range streams {
			ev, ok := h.c.waitEvent(ctx, func(e h2Event) bool { return e.Type == http2.FrameGoAway })
			if !ok {
				t.Errorf("%v: no GOAWAY at Drain: %v", h.p, ev.Err)
			}
			goaways[i] = ev
		}
		for i, c := range idle {
			if !c.waitClosed(exchangeGuard) {
				t.Errorf("idle HTTP/1.1 connection %d stayed open through the Drain's jitter window", i)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i, h := range streams {
		if ev := goaways[i]; ev.Code != http2.ErrCodeNo || ev.LastStreamID < h.id {
			t.Errorf("%v: GOAWAY %v last stream %d, want NO_ERROR covering stream %d", h.p, ev.Code, ev.LastStreamID, h.id)
		}
	}

	// Every shard has shut down: the held requests answer.
	g.origin.releaseCase(busyCase)
	r := busy.read(guard(t, exchangeGuard), busyReq.method())
	wantOK(t, r)
	if !r.Close {
		t.Errorf("busy HTTP/1.1 response %v without Connection: close", r)
	}
	if !busy.waitClosed(exchangeGuard) {
		t.Error("busy HTTP/1.1 connection stayed open after its response")
	}
	for _, h := range streams {
		g.origin.releaseCase(h.kase)
		r := h.c.collect(guard(t, exchangeGuard), h.id)
		wantOK(t, r)
		if r.SawGoAway {
			t.Errorf("%v: a second GOAWAY %v at Drain", h.p, r.GoAway)
		}
		extra := 0
		ev, _ := h.c.waitEvent(guard(t, exchangeGuard), func(e h2Event) bool {
			if e.Type == http2.FrameGoAway {
				extra++
			}
			return false
		})
		if ev.Err == nil || errors.Is(ev.Err, context.DeadlineExceeded) {
			t.Errorf("%v: the connection stayed open after its last stream: %+v", h.p, ev)
		}
		if extra > 0 {
			t.Errorf("%v: %d more GOAWAY frames at Drain, want one in all", h.p, extra)
		}
	}
	code, err := g.node.Stop(guard(t, deadlineWait))
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Errorf("the Node exited %d after the Drain, want 0", code)
	}
}
