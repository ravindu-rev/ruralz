// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"strconv"
	"testing"

	"github.com/redis/rueidis"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// TestFirstPassOpensEveryPipelinedConnection checks that the State client
// is created off the request path with every pipelined connection of each
// shard open, so a call never waits for a first dial inside its
// stateStoreTimeout: rueidis dials a pipelined connection only when the
// first command is routed to it, and on a Node taking its first requests
// that made calls wait for the dial past the per-request deadline, so a
// ratelimit's GCRA call timed out and spent the deadline of the quota call
// behind it, which then failed open unmetered. After the first maintenance
// pass the driver holds both pipelined connections of each shard, and with
// every new connection refused, calls to each shard still succeed: they go
// out on connections the pass opened. A cluster names one seed, so the
// other primaries' connections are the ones rueidis would dial at a call.
func TestFirstPassOpensEveryPipelinedConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    fixtureOptions
		// primaries returns the servers that serve calls.
		primaries func(f *fixture) []*fakeServer
	}{
		{"standalone", fixtureOptions{}, func(f *fixture) []*fakeServer { return []*fakeServer{f.srv} }},
		{"cluster", fixtureOptions{cluster: 3, seeds: 1}, func(f *fixture) []*fakeServer { return f.nodes }},
		{"sentinel", fixtureOptions{sentinel: 2}, func(f *fixture) []*fakeServer { return f.nodes[:1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.o)
			f.ready()
			f.awaitIdle()
			want := pipelinedConns(statestore.DefaultLimits().PipelinedPerShard)
			servers := tc.primaries(f)
			for i, s := range servers {
				if got := f.d.conns.countOf(f.shardFor(s)); got != want {
					t.Fatalf("after the first maintenance pass the driver holds %d connections to primary %d, want its %d pipelined connections", got, i, want)
				}
			}
			for _, s := range servers {
				s.setReject(true)
			}
			for i := range servers {
				for n := range 20 {
					c := gcraCall(f.digestFor(i, "first"+strconv.Itoa(n)), oneLimit())
					if f.consume(c); c.Err != nil || !c.Done {
						t.Fatalf("call %d to primary %d with new connections refused: Done %v Err %v, want an answer on an open connection", n, i, c.Done, c.Err)
					}
				}
			}
		})
	}
}

// TestWarmOpensOnePipelinedConnection checks the count warm opens when
// Limits.PipelinedPerShard is 1: rueidis reads a PipelineMultiplex of 0 on
// a standalone or sentinel client as log2(GOMAXPROCS) connections, up to
// 4, and a negative one as one connection for every client kind, so the
// client asks for a negative one, and every call goes out on the
// connection the first pass holds.
func TestWarmOpensOnePipelinedConnection(t *testing.T) {
	lim := fillLimits(statestore.Limits{PipelinedPerShard: 1})
	d := Driver{lim: lim, ep: endpoint{opt: rueidis.ClientOption{InitAddress: []string{"127.0.0.1:6379"}}}}
	if got := d.options().PipelineMultiplex; got >= 0 {
		t.Fatalf("options() PipelineMultiplex = %d for one pipelined connection, want a negative value", got)
	}
	f := newFixture(t, fixtureOptions{limits: statestore.Limits{PipelinedPerShard: 1}})
	f.ready()
	f.awaitIdle()
	if got := f.d.conns.countOf(f.d.single); got != 1 {
		t.Fatalf("after the first maintenance pass the driver holds %d connections, want 1", got)
	}
	f.srv.setReject(true)
	for n := range 20 {
		c := gcraCall(statestore.DigestOf("one"+strconv.Itoa(n)), oneLimit())
		if f.consume(c); c.Err != nil || !c.Done {
			t.Fatalf("call %d with new connections refused: Done %v Err %v, want an answer on the one connection", n, c.Done, c.Err)
		}
	}
}

// TestWarmRunsOncePerShard checks that the warm-up opens a shard's
// pipelined connections only when the shard first serves: once they
// closed, later maintenance passes dial nothing, and the next call redials
// on demand, as a closed connection redials.
func TestWarmRunsOncePerShard(t *testing.T) {
	f := newFixture(t, fixtureOptions{limits: quietLimits()})
	f.ready()
	f.awaitIdle()
	f.srv.dropConns()
	f.awaitDropped()
	accepted := f.srv.connections()
	for range 3 {
		f.clk.Advance(MaintenanceTick)
		f.awaitIdle()
	}
	if got := f.srv.connections(); got != accepted {
		t.Fatalf("maintenance passes after the connections closed dialed %d connections, want none", got-accepted)
	}
	c := gcraCall(statestore.DigestOf("again"), oneLimit())
	if f.consume(c); c.Err != nil || !c.Done {
		t.Fatalf("call after the connections closed: Done %v Err %v", c.Done, c.Err)
	}
	if f.srv.connections() == accepted {
		t.Fatal("the call after the connections closed did not redial")
	}
}

// shardFor returns the driver's shard of server s.
func (f *fixture) shardFor(s *fakeServer) *shard {
	if f.d.single != nil {
		return f.d.single
	}
	return f.d.shardAt(s.addr)
}

// digestFor returns a key digest served by primary i: any key under a
// standalone or sentinel topology, a key in primary i's slots in a
// cluster.
func (f *fixture) digestFor(i int, salt string) statestore.Digest {
	if f.cl == nil {
		return statestore.DigestOf(salt)
	}
	return f.digestOn(i, salt)
}
