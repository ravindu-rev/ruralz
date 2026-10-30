// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"errors"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// TestReadOnlyReconnects: a READONLY reply, as from a primary a failover
// demoted behind an endpoint that now names the new primary, fails its
// call (RZ-STS-002), closes the shard's connections and logs once; a
// second READONLY within minRefreshGap closes nothing, one after it closes
// again, and the redial reaches the new primary. Each call after the
// connections closed waits until the client dropped them, so it dials
// anew instead of going out on a closed connection, whichever of the two
// pipelined connections rueidis picks.
func TestReadOnlyReconnects(t *testing.T) {
	f := newFixture(t, fixtureOptions{limits: quietLimits()})
	f.ready()
	waitFor(t, "the first memory reading", func() bool { return !f.d.single.mem.due(f.clk.Now()) })
	if c := gcraCall(statestore.DigestOf("k"), oneLimit()); f.consume(c) == 0 || c.Err != nil {
		t.Fatalf("first call: Err %v", c.Err)
	}
	f.srv.setReplica(true)
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != statestore.ErrFailed {
		t.Fatalf("call answered READONLY: Err %v, want RZ-STS-002", c.Err)
	}
	if n := f.d.conns.len(); n != 0 {
		t.Fatalf("%d connections open after READONLY, want 0", n)
	}
	waitFor(t, "the server to see its connections close", func() bool { return f.srv.open() == 0 })
	f.awaitDropped()

	// Within minRefreshGap the next READONLY keeps its redialed connection.
	if c := gcraCall(statestore.DigestOf("k"), oneLimit()); f.consume(c) == 0 || c.Err != statestore.ErrFailed {
		t.Fatalf("second READONLY call: Err %v", c.Err)
	}
	if f.d.conns.len() == 0 {
		t.Fatal("a READONLY within minRefreshGap closed the connections again")
	}

	f.clk.Advance(minRefreshGap)
	if c := gcraCall(statestore.DigestOf("k"), oneLimit()); f.consume(c) == 0 || c.Err != statestore.ErrFailed {
		t.Fatalf("third READONLY call: Err %v", c.Err)
	}
	if n := f.d.conns.len(); n != 0 {
		t.Fatalf("%d connections open after a READONLY past minRefreshGap, want 0", n)
	}
	if got := f.logs.count("state store server answered READONLY; reconnecting"); got != 1 {
		t.Errorf("%d READONLY logs, want 1 per 10 s", got)
	}

	// The endpoint now reaches the new primary.
	f.srv.setReplica(false)
	f.clk.Advance(time.Second)
	f.awaitDropped()
	accepted := f.srv.connections()
	c = gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != nil {
		t.Fatalf("call after the reconnect: Err %v", c.Err)
	}
	if f.srv.connections() == accepted {
		t.Error("the call after the reconnect did not redial")
	}
}

// TestReadOnlyFollowsTheSentinels: after a failover the client missed
// (no event, no periodic refresh), a READONLY reply from the demoted
// primary closes its connections and the sentinel connections, so the
// client asks the sentinels again and later calls reach the new primary.
func TestReadOnlyFollowsTheSentinels(t *testing.T) {
	f := newFixture(t, fixtureOptions{sentinel: 2, limits: quietLimits()})
	f.ready()
	if c := gcraCall(statestore.DigestOf("k"), oneLimit()); f.consume(c) == 0 || c.Err != nil {
		t.Fatalf("first call: Err %v", c.Err)
	}
	f.sent.setMaster("main", f.nodes[1].addr)
	f.nodes[0].setReplica(true)
	f.clk.Advance(time.Second) // a fresh dial budget for the redials
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != statestore.ErrFailed {
		t.Fatalf("call answered READONLY: Err %v, want RZ-STS-002", c.Err)
	}
	waitFor(t, "the client to follow the sentinels to the new primary", func() bool { return f.primaryAt(f.nodes[1].addr) })
	c = gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != nil {
		t.Fatalf("call after the failover: Err %v", c.Err)
	}
	if f.nodes[1].count("EVALSHA") != 1 {
		t.Fatalf("EVALSHA on the new primary %d, want 1", f.nodes[1].count("EVALSHA"))
	}
}

// TestIsReadOnly: only a READONLY error reply is one.
func TestIsReadOnly(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	f.srv.setReplica(true)
	c := f.d.loadClient()
	res := c.Do(t.Context(), c.B().Arbitrary("SET").Keys("x").Args("1").Build())
	if err := res.Error(); !isReadOnly(err) {
		t.Fatalf("isReadOnly(%v) = false", err)
	}
	for _, err := range []error{nil, errors.New("READONLY but not a reply"), errClosed} {
		if isReadOnly(err) {
			t.Errorf("isReadOnly(%v) = true", err)
		}
	}
	f.srv.setReplica(false)
	if err := c.Do(t.Context(), c.B().Arbitrary("EVALSHA").Args("0", "0").Build()).Error(); isReadOnly(err) {
		t.Errorf("isReadOnly(%v) = true", err)
	}
}
