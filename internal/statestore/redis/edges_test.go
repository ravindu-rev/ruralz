// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/redis/rueidis"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/breaker"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
)

// TestClusterUnassignedSlotsUseUnmappedShard checks slots without an
// owner: their calls fail and open the unmapped breaker, which Status
// reports, and its probe, a slot map read, closes it once every slot has
// an owner again.
func TestClusterUnassignedSlotsUseUnmappedShard(t *testing.T) {
	f := newFixture(t, fixtureOptions{cluster: 2})
	f.ready()
	f.cl.mu.Lock()
	f.cl.ranges[1].last = 16000
	f.cl.mu.Unlock()
	f.d.requestRefresh()
	f.advanceWhile("the partial slot map", func() bool { m := f.d.slots.Load(); return m != nil && !m.full }, time.Second)
	var d statestore.Digest
	for n := 0; ; n++ {
		if d = statestore.DigestOf("gap" + strconv.Itoa(n)); keys.DigestSlot(d) > 16000 {
			break
		}
	}
	if f.d.shardOfSlot(keys.DigestSlot(d)) != f.d.unmapped {
		t.Fatal("an unassigned slot is not on the unmapped shard")
	}
	for range 20 {
		c := gcraCall(d, oneLimit())
		if f.consume(c); c.Err != statestore.ErrFailed {
			t.Fatalf("call to an unassigned slot: Err %v, want RZ-STS-002", c.Err)
		}
	}
	if f.d.unmapped.brk.State() != breaker.Open || !f.d.Status().BreakerNotClosed {
		t.Fatal("the unmapped breaker did not open")
	}
	f.cl.mu.Lock()
	f.cl.ranges[1].last = keys.Slots - 1
	f.cl.mu.Unlock()
	f.advanceWhile("the unmapped breaker to close", func() bool { return f.d.unmapped.brk.State() == breaker.Closed }, 250*time.Millisecond)
	if m := f.d.slots.Load(); !m.full || f.d.Status().BreakerNotClosed {
		t.Fatal("the full slot map did not clear the unmapped shard")
	}
}

// TestFailedProbeReopensTheBreaker checks the half-open path when the
// server still fails: a failed PING probe reopens the breaker for another
// delay, and probes resume once the server answers.
func TestFailedProbeReopensTheBreaker(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	f.openBreaker()
	f.srv.inject(&fault{cmd: "PING", err: "ERR injected"})
	opens := f.logs.count("state store breaker opened")
	f.advanceWhile("the failed probe", func() bool { return f.logs.count("state store breaker opened") > opens }, 250*time.Millisecond)
	f.srv.clearFaults()
	f.advanceWhile("the breaker to close", func() bool { return f.d.single.brk.State() == breaker.Closed }, 250*time.Millisecond)
}

// TestMalformedRepliesAreSTS002 checks that every unparsable reply is
// RZ-STS-002 for its call: a lookup's generation or partition, a write's
// script or lease reply, and a Quota outcome.
func TestMalformedRepliesAreSTS002(t *testing.T) {
	d := statestore.DigestOf("k")
	k := statestore.CacheKey{URI: d, Partition: d}
	for _, tc := range []struct {
		name string
		f    *fault
		run  func(f *fixture) *statestore.Error
	}{
		{"generation not an integer", &fault{cmd: "GET", raw: bulk("x")}, func(f *fixture) *statestore.Error {
			l := lookupCall(k, d)
			f.read(l)
			return l.Err
		}},
		{"partition not an array", &fault{cmd: "HMGET", raw: simple("OK")}, func(f *fixture) *statestore.Error {
			l := lookupCall(k, d)
			f.read(l)
			return l.Err
		}},
		{"partition fields missing", &fault{cmd: "HMGET", raw: array(null)}, func(f *fixture) *statestore.Error {
			l := lookupCall(k, d)
			f.read(l)
			return l.Err
		}},
		{"variant store time not an integer", &fault{cmd: "HMGET", raw: array(bulk("n"), bulk("x"), bulk("e"))}, func(f *fixture) *statestore.Error {
			l := lookupCall(k, d)
			f.read(l)
			return l.Err
		}},
		{"TIME reply not two integers", &fault{cmd: "TIME", raw: array(bulk("1790000000"))}, func(f *fixture) *statestore.Error {
			l := lookupCall(k, d)
			f.read(l)
			return l.Err
		}},
		{"write reply out of range", &fault{cmd: "EVALSHA", raw: integer(2)}, func(f *fixture) *statestore.Error {
			w := &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: callTO, Invalidate: d}
			f.write(w)
			return w.Err
		}},
		{"lease reply not OK", &fault{cmd: "SET", raw: simple("QUEUED")}, func(f *fixture) *statestore.Error {
			w := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: callTO, Lease: statestore.CacheLease{Key: k, Mode: statestore.LeaseTake, Token: 1}}
			f.write(w)
			return w.Err
		}},
		{"quota outcome off its window", &fault{cmd: "EVALSHA", raw: array(bulk("1790000000"), bulk("0"), array(integer(1), integer(1), integer(7)))}, func(f *fixture) *statestore.Error {
			c := quotaCall(time.Hour, 5, d)
			f.consume(c)
			return c.Err
		}},
		{"gcra outcome before now", &fault{cmd: "EVALSHA", raw: array(bulk("1790000000"), bulk("0"), array(integer(1), bulk("1")))}, func(f *fixture) *statestore.Error {
			c := gcraCall(d, oneLimit())
			f.consume(c)
			return c.Err
		}},
		{"deny before the last call", &fault{cmd: "EVALSHA", raw: array(bulk("1790000000"), bulk("0"), array(integer(0), integer(5), integer(0)), array(integer(1), integer(1), integer(0)))}, func(f *fixture) *statestore.Error {
			a, b := quotaCall(time.Millisecond, 5, d), quotaCall(time.Millisecond, 5, d)
			f.consume(a, b)
			return a.Err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{})
			f.ready()
			tc.f.times = 1
			f.srv.inject(tc.f)
			if err := tc.run(f); err != statestore.ErrFailed {
				t.Fatalf("Err %v, want RZ-STS-002", err)
			}
		})
	}
}

// TestPreloadProtocolErrorFailsTheDial checks that an unreadable reply to
// the dialer's SCRIPT LOAD fails the dial, a connect failure, and the call
// meets RZ-STS-002.
func TestPreloadProtocolErrorFailsTheDial(t *testing.T) {
	f := newFixture(t, fixtureOptions{setup: func(s *fakeServer) {
		s.inject(&fault{cmd: "SCRIPT LOAD", raw: integer(1)})
	}})
	f.ready()
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != statestore.ErrFailed {
		t.Fatalf("call over a connection whose load failed: Err %v, want RZ-STS-002", c.Err)
	}
	if f.d.single.pace.Failures() == 0 {
		t.Fatal("the protocol error was not a connect failure")
	}
	if f.srv.count("HELLO") != 0 {
		t.Fatal("rueidis took a connection whose load failed")
	}
}

// TestErrType checks the log classification of errors, which never
// carries an error's text.
func TestErrType(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{errPaced, "paced"},
		{errClosed, "closed"},
		{rueidis.ErrClosing, "closed"},
		{errNotCluster, "not_cluster"},
		{errProtocol, "protocol"},
		{context.DeadlineExceeded, "timeout"},
		{io.EOF, "reset"},
		{net.ErrClosed, "reset"},
		{&net.OpError{Op: "dial", Err: errors.New("x")}, "network"},
	} {
		if got := errType(tc.err); got != tc.want {
			t.Errorf("errType(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestParseSlotsEndpoints checks the CLUSTER SLOTS endpoint forms: an
// empty host is the queried node's, "?" is skipped, and a malformed range
// is an error. A standalone driver sends no CLUSTER SLOTS of its own, so
// the fake server's replies reach the test alone.
func TestParseSlotsEndpoints(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	c := f.d.loadClient()
	slots := func(reply string) (rueidis.RedisMessage, error) {
		f.srv.inject(&fault{cmd: "CLUSTER SLOTS", times: 1, raw: reply})
		return c.Do(t.Context(), c.B().ClusterSlots().Build()).ToMessage()
	}
	msg, err := slots(array(
		array(integer(0), integer(100), array(bulk(""), integer(7000))),
		array(integer(101), integer(200), array(bulk("?"), integer(7001))),
		array(integer(201), integer(300), array(bulk("10.0.0.9"), integer(7002))),
	))
	if err != nil {
		t.Fatal(err)
	}
	ranges, err := parseSlots(msg, "127.0.0.1:6379")
	if err != nil {
		t.Fatal(err)
	}
	want := []slotRange{{0, 100, "127.0.0.1:7000"}, {201, 300, "10.0.0.9:7002"}}
	if len(ranges) != len(want) || ranges[0] != want[0] || ranges[1] != want[1] {
		t.Fatalf("parseSlots = %+v, want %+v", ranges, want)
	}
	for _, bad := range []string{
		array(array(integer(9), integer(1), array(bulk("h"), integer(1)))),
		array(array(integer(0), integer(keys.Slots), array(bulk("h"), integer(1)))),
		array(array(integer(0), integer(1))),
		simple("OK"),
	} {
		msg, err := slots(bad)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseSlots(msg, "127.0.0.1:6379"); err == nil {
			t.Fatalf("parseSlots accepted %q", bad)
		}
	}
}

// TestCallWithoutBudget checks a call without a per-request budget: its
// Policy timeout and the context's deadline alone apply.
func TestCallWithoutBudget(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	f.d.Consume(context.Background(), nil, []*statestore.Call{c})
	if c.Err != nil || !c.Done {
		t.Fatalf("call without a budget: Done %v Err %v", c.Done, c.Err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	c = gcraCall(statestore.DigestOf("k"), oneLimit())
	f.d.Consume(ctx, nil, []*statestore.Call{c})
	if c.Err != statestore.ErrNotAttempted {
		t.Fatalf("call past its context deadline: Err %v, want RZ-STS-004", c.Err)
	}
}

// TestShardRegistryBound checks that the driver tracks at most MaxShards
// addresses; later ones share one overflow shard.
func TestShardRegistryBound(t *testing.T) {
	f := newFixture(t, fixtureOptions{noServer: true, url: "redis://127.0.0.1:1"})
	first := f.d.shardAt("10.1.0.0:6379")
	for i := range MaxShards + 10 {
		f.d.shardAt("10.0." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256) + ":6379")
	}
	a, b := f.d.shardAt("10.9.0.1:6379"), f.d.shardAt("10.9.0.2:6379")
	if a != b || a == first || len(f.d.reg.byAddr) != MaxShards {
		t.Fatalf("registry holds %d shards; overflow shared %v", len(f.d.reg.byAddr), a == b)
	}
	if f.d.shardAt("10.1.0.0:6379") != first {
		t.Fatal("a tracked address lost its shard")
	}
}

// TestMultiplexOf checks the pipelined connections per shard: rueidis
// opens 2^PipelineMultiplex.
func TestMultiplexOf(t *testing.T) {
	for n, want := range map[int]int{0: 0, 1: 0, 2: 1, 3: 1, 4: 2, 8: 3} {
		if got := multiplexOf(n); got != want {
			t.Errorf("multiplexOf(%d) = %d, want %d", n, got, want)
		}
	}
}
