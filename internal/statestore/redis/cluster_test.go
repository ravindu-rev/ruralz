// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/breaker"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
)

// digestOn returns a digest whose tag hashes into the slots of node i.
func (f *fixture) digestOn(i int, salt string) statestore.Digest {
	f.t.Helper()
	r := f.cl.ranges[i]
	for n := 0; n < 1_000_000; n++ {
		d := statestore.DigestOf(salt + strconv.Itoa(n))
		if s := int(keys.DigestSlot(d)); s >= r.first && s <= r.last {
			return d
		}
	}
	f.t.Fatal("no digest for the node")
	return statestore.Digest{}
}

// TestClusterRoutesBySlot checks the cluster topology: calls go to the
// primary owning their slot, each primary loads the library on connect,
// and the driver's slot map names one shard per primary.
func TestClusterRoutesBySlot(t *testing.T) {
	f := newFixture(t, fixtureOptions{cluster: 2})
	f.ready()
	for i, n := range f.nodes {
		before := n.count("EVALSHA")
		c := gcraCall(f.digestOn(i, "route"), oneLimit())
		if f.consume(c); c.Err != nil || !c.Done {
			t.Fatalf("call to node %d: Done %v Err %v", i, c.Done, c.Err)
		}
		if n.count("EVALSHA") != before+1 {
			t.Fatalf("node %d did not run its slot's script", i)
		}
		if n.count("SCRIPT LOAD") == 0 {
			t.Fatalf("node %d never loaded the library", i)
		}
	}
	m := f.d.slots.Load()
	if m == nil || !m.full || len(m.shards) != 3 {
		t.Fatalf("slot map %+v, want both primaries and every slot mapped", m)
	}
	var buf [8]*shard
	if got := f.d.active(buf[:0]); len(got) != 2 {
		t.Fatalf("%d active shards, want the 2 primaries", len(got))
	}
}

// TestClusterBreakerPerShard checks that breakers are per shard: one
// primary failing opens its breaker only, and the other keeps serving.
func TestClusterBreakerPerShard(t *testing.T) {
	f := newFixture(t, fixtureOptions{cluster: 2})
	f.ready()
	f.nodes[0].inject(&fault{cmd: "EVALSHA", err: "ERR injected"})
	for i := range 20 {
		c := gcraCall(f.digestOn(0, "bad"+strconv.Itoa(i)), oneLimit())
		f.consume(c)
	}
	bad := f.d.shardOfSlot(keys.DigestSlot(f.digestOn(0, "x")))
	good := f.d.shardOfSlot(keys.DigestSlot(f.digestOn(1, "x")))
	if bad.brk.State() != breaker.Open || good.brk.State() != breaker.Closed {
		t.Fatalf("breakers %v and %v, want open and closed", bad.brk.State(), good.brk.State())
	}
	c := gcraCall(f.digestOn(1, "ok"), oneLimit())
	if f.consume(c); c.Err != nil {
		t.Fatalf("call to the healthy primary: Err %v", c.Err)
	}
	c = gcraCall(f.digestOn(0, "skip"), oneLimit())
	if f.consume(c); c.Err != statestore.ErrBreakerOpen {
		t.Fatalf("call to the failing primary: Err %v, want RZ-STS-003", c.Err)
	}
	if !f.d.Status().BreakerNotClosed {
		t.Fatal("Status does not report the open shard breaker")
	}
}

// TestClusterFollowsMovedAndRefreshes checks resharding: after every slot
// moves to the second primary, a call to a moved slot follows MOVED and
// succeeds, and the driver's slot map follows within the 10 s refresh.
func TestClusterFollowsMovedAndRefreshes(t *testing.T) {
	f := newFixture(t, fixtureOptions{cluster: 2})
	f.ready()
	d := f.digestOn(0, "moved")
	f.cl.mu.Lock()
	f.cl.ranges = []fakeRange{{first: 0, last: keys.Slots - 1, srv: f.nodes[1]}}
	f.cl.mu.Unlock()
	before := f.nodes[1].count("EVALSHA")
	c := gcraCall(d, oneLimit())
	if f.consume(c); c.Err != nil || !c.Done {
		t.Fatalf("call to a moved slot: Done %v Err %v", c.Done, c.Err)
	}
	if f.nodes[1].count("EVALSHA") <= before {
		t.Fatal("the call did not reach the slot's new owner")
	}
	want := f.d.shardAt(f.nodes[1].addr)
	f.advanceWhile("the slot map refresh", func() bool { return f.d.shardOfSlot(keys.DigestSlot(d)) == want }, time.Second)
}

// TestClusterReadAcrossShards checks a lookup whose generation key and
// partition live on different primaries: one Read, each command at its
// owner.
func TestClusterReadAcrossShards(t *testing.T) {
	f := newFixture(t, fixtureOptions{cluster: 2})
	f.ready()
	var k statestore.CacheKey
	found := false
	for i := 0; i < 100_000 && !found; i++ {
		k = statestore.CacheKey{URI: statestore.DigestOf("u" + strconv.Itoa(i)), Partition: statestore.DigestOf("p")}
		found = f.cl.owner(int(keys.DigestSlot(k.URI))) == f.nodes[0] && f.cl.owner(int(keys.PartitionSlot(k))) == f.nodes[1]
	}
	if !found {
		t.Fatal("no key split across the primaries")
	}
	l := lookupCall(k, statestore.DigestOf("v"))
	f.read(l)
	if l.Err != nil || !l.Done {
		t.Fatalf("lookup: Done %v Err %v", l.Done, l.Err)
	}
	if f.nodes[0].count("GET") != 1 || f.nodes[1].count("HMGET") != 1 {
		t.Fatalf("GET at node 0: %d, HMGET at node 1: %d; want 1 and 1", f.nodes[0].count("GET"), f.nodes[1].count("HMGET"))
	}
}

// TestClusterSeedNotCluster checks that a cluster topology never falls
// back to a single client: a seed answering as a standalone server
// publishes no client, so calls wait for one and time out.
func TestClusterSeedNotCluster(t *testing.T) {
	f := newFixture(t, fixtureOptions{topology: statestore.TopologyCluster})
	waitFor(t, "the refused seed", func() bool {
		return f.logs.count("state store cluster seed answered as a standalone server; retrying") == 1
	})
	select {
	case <-f.d.ready:
		t.Fatal("a standalone server became a cluster client")
	default:
	}
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	f.advanceUntil(background(func() { f.consume(c) }), 10*time.Millisecond)
	if c.Err != statestore.ErrTimeout {
		t.Fatalf("call without a cluster client: Err %v, want RZ-STS-001", c.Err)
	}
}

// TestClusterUnmappedBreaker checks the unmapped shard: with every seed
// unreachable, calls wait for the client and its connect failures open
// the unmapped breaker, which Status reports and calls then skip.
func TestClusterUnmappedBreaker(t *testing.T) {
	f := newFixture(t, fixtureOptions{cluster: 2, fake: fakeOptions{reject: true}})
	f.advanceWhile("the unmapped breaker to open", func() bool { return f.d.unmapped.brk.State() == breaker.Open }, 300*time.Millisecond)
	if !f.d.Status().BreakerNotClosed {
		t.Fatal("Status does not report the unmapped breaker")
	}
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != statestore.ErrBreakerOpen {
		t.Fatalf("call with the unmapped breaker open: Err %v, want RZ-STS-003", c.Err)
	}
	for _, n := range f.nodes {
		n.setReject(false)
	}
	f.advanceWhile("the client", func() bool {
		select {
		case <-f.d.ready:
			return f.d.slots.Load() != nil && f.d.slots.Load().full
		default:
			return false
		}
	}, 300*time.Millisecond)
	// The client's creation attempts spent this second's 4 dials per
	// shard; the second pipelined connection dials in the next.
	f.clk.Advance(time.Second)
	c = gcraCall(f.digestOn(1, "after"), oneLimit())
	if f.consume(c); c.Err != nil {
		t.Fatalf("call once the cluster answers: Err %v", c.Err)
	}
}
