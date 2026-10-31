// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/breaker"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// advanceWhile advances the fake clock by step every millisecond of real
// time while cond is false.
func (f *fixture) advanceWhile(what string, cond func() bool, step time.Duration) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		f.clk.Advance(step)
		time.Sleep(time.Millisecond)
	}
}

// TestBreakerOpensOnFailedCalls checks the per-shard breaker: 50% of at
// least 20 calls failing opens it, a call then is RZ-STS-003 without
// reaching the server, Status reports it, and the opening is logged by
// shard id.
func TestBreakerOpensOnFailedCalls(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	f.openBreaker()
	evals := f.srv.count("EVALSHA")
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	f.consume(c)
	if c.Err != statestore.ErrBreakerOpen || c.Elapsed != 0 {
		t.Fatalf("call with the breaker open: Err %v Elapsed %v, want RZ-STS-003", c.Err, c.Elapsed)
	}
	if f.srv.count("EVALSHA") != evals {
		t.Fatal("a call reached the server through an open breaker")
	}
	if f.m.call(emit.StateOpGCRA, statestore.ResultSkipped) != 1 {
		t.Fatal("the skipped call is not counted")
	}
	if !f.d.Status().BreakerNotClosed {
		t.Fatal("Status does not report the open breaker")
	}
	logs := f.logs.text()
	if !strings.Contains(logs, "state store breaker opened shard="+f.d.single.id) {
		t.Fatalf("no breaker log naming the shard:\n%s", logs)
	}
}

// TestOpenBreakerSkipsWritesAndReads checks that writes and lookups to a
// shard whose breaker is open are RZ-STS-003 without a call, so the
// post-commit queue drops them at once.
func TestOpenBreakerSkipsWritesAndReads(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	f.openBreaker()
	before := len(f.srv.commands())
	d := statestore.DigestOf("k")
	k := statestore.CacheKey{URI: d, Partition: d}
	ws := []*statestore.Write{
		{Kind: statestore.OpRefund, Timeout: callTO, Refund: statestore.Refund{Name: "q", Window: time.Hour, WindowStart: start(), Digest: d}},
		{Kind: statestore.OpCacheInvalidate, Timeout: callTO, Invalidate: d},
		{Kind: statestore.OpCacheSet, Timeout: callTO, Cache: statestore.CacheStore{Key: k, Variant: d, TTL: time.Hour, Entry: []byte("e"), Token: 1}},
		{Kind: statestore.OpCacheSet, Timeout: callTO, Lease: statestore.CacheLease{Key: k, Mode: statestore.LeaseTake, Token: 1}},
	}
	f.write(ws...)
	for i, w := range ws {
		if w.Err != statestore.ErrBreakerOpen || w.Applied {
			t.Fatalf("write %d: Err %v Applied %v, want RZ-STS-003", i, w.Err, w.Applied)
		}
	}
	l := lookupCall(k, d)
	f.read(l)
	if l.Err != statestore.ErrBreakerOpen {
		t.Fatalf("lookup: Err %v, want RZ-STS-003", l.Err)
	}
	for _, c := range f.srv.commands()[before:] {
		if c != "PING" && c != "INFO" {
			t.Fatalf("%s reached the server through an open breaker", c)
		}
	}
}

// TestBreakerClosesAfterPingProbes checks the half-open path without
// traffic: after the open delay and 1 s without a request probe the
// driver probes with PING, and 3 successes close the breaker, logged at
// INFO.
func TestBreakerClosesAfterPingProbes(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	f.openBreaker()
	f.advanceWhile("the breaker to close", func() bool { return f.d.single.brk.State() == breaker.Closed }, 100*time.Millisecond)
	if f.logs.count("state store breaker closed") != 1 {
		t.Fatal("the closing is not logged")
	}
	if f.d.Status().BreakerNotClosed {
		t.Fatal("Status still reports the breaker")
	}
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != nil {
		t.Fatalf("call after the breaker closed: Err %v", c.Err)
	}
}

// TestRequestIsTheHalfOpenProbe checks that once the open delay ends one
// request is admitted as the probe, and others are skipped while it is in
// flight.
func TestRequestIsTheHalfOpenProbe(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	f.openBreaker()
	f.clk.Advance(time.Second)
	evals := f.srv.count("EVALSHA")
	hold := f.srv.inject(&fault{cmd: "EVALSHA", hold: make(chan struct{}), times: 1})
	probe := gcraCall(statestore.DigestOf("probe"), oneLimit())
	done := background(func() { f.consume(probe) })
	waitFor(t, "the probe to be sent", func() bool { return f.srv.count("EVALSHA") == evals+1 })
	other := gcraCall(statestore.DigestOf("other"), oneLimit())
	f.consume(other)
	if other.Err != statestore.ErrBreakerOpen {
		t.Fatalf("call during the probe: Err %v, want RZ-STS-003", other.Err)
	}
	close(hold.hold)
	<-done
	if probe.Err != nil || !probe.Done {
		t.Fatalf("probe: Done %v Err %v", probe.Done, probe.Err)
	}
	if st := f.d.single.brk.State(); st != breaker.HalfOpen {
		t.Fatalf("state %v after one probe success, want half_open", st)
	}
}

// TestDialsArePaced checks the reconnect pacer in the dialer: at most 4
// dials of a shard start in a second, a refused dial fails at once without
// touching the network and is not a connect failure, so the breaker stays
// closed through them.
func TestDialsArePaced(t *testing.T) {
	f := newFixture(t, fixtureOptions{fake: fakeOptions{reject: true}})
	f.ready()
	for range 10 {
		c := gcraCall(statestore.DigestOf("k"), oneLimit())
		f.consume(c)
		if c.Err != statestore.ErrFailed {
			t.Fatalf("call with every connection refused: Err %v, want RZ-STS-002", c.Err)
		}
	}
	// Every real dial here fails: the pacer's failures count the dials.
	if n := f.d.single.pace.Failures(); n == 0 || n > 4 {
		t.Fatalf("%d dials in one second, want 1 to 4", n)
	}
	if st := f.d.single.brk.State(); st != breaker.Closed {
		t.Fatalf("breaker %v after at most 4 connect failures and 10 calls, want closed", st)
	}
}

// TestBreakerOpensOnConnectFailures checks that 5 consecutive dial
// failures open a shard's breaker before 20 calls could, after which no
// dial starts until it half-opens.
func TestBreakerOpensOnConnectFailures(t *testing.T) {
	// Memory readings, retried every second after a failure, would dial
	// beside the calls: park them so only calls dial.
	lim := statestore.DefaultLimits()
	lim.MemoryPollSlow, lim.MemoryPollFast = time.Hour, time.Hour
	f := newFixture(t, fixtureOptions{fake: fakeOptions{reject: true}, limits: lim})
	f.ready()
	// The first reading is tried at start; once it failed, the next is an
	// hour away.
	waitFor(t, "the first reading attempt", func() bool { return !f.d.single.mem.due(f.clk.Now()) })
	calls := 0
	for f.d.single.brk.State() != breaker.Open {
		if calls++; calls > 19 {
			t.Fatal("the breaker did not open on connect failures before 20 calls")
		}
		f.consume(gcraCall(statestore.DigestOf("k"), oneLimit()))
		f.clk.Advance(400 * time.Millisecond)
	}
	// Every real dial here fails, so the pacer's consecutive failures count
	// the dials; the server's accept count may lag the client's connect.
	dials := f.d.single.pace.Failures()
	for range 5 {
		c := gcraCall(statestore.DigestOf("k"), oneLimit())
		if f.consume(c); c.Err != statestore.ErrBreakerOpen {
			t.Fatalf("call with the breaker open: Err %v, want RZ-STS-003", c.Err)
		}
	}
	if f.d.single.pace.Failures() != dials {
		t.Fatal("a dial started while the breaker was open")
	}
	if got := f.logs.count("state store dial failed"); got < 1 || got > 2 {
		t.Fatalf("%d dial failure logs, want them rate-limited to one per 10 s", got)
	}
}

// TestTimeoutsOpenTheBreaker checks that a caller timeout is a breaker
// failure, recorded at the timeout.
func TestTimeoutsOpenTheBreaker(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	hold := f.srv.inject(&fault{cmd: "EVALSHA", hold: make(chan struct{})})
	defer close(hold.hold)
	for i := range 20 {
		c := gcraCall(statestore.DigestOf("k"), oneLimit())
		c.Timeout = 5 * time.Millisecond
		f.advanceUntil(background(func() { f.consume(c) }), time.Millisecond)
		if c.Err != statestore.ErrTimeout {
			t.Fatalf("call %d: Err %v, want RZ-STS-001", i, c.Err)
		}
	}
	if st := f.d.single.brk.State(); st != breaker.Open {
		t.Fatalf("breaker %v after 20 timeouts, want open", st)
	}
}
