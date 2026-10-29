// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// advanceUntil advances the fake clock by step every millisecond of real
// time until done closes.
func (f *fixture) advanceUntil(done <-chan struct{}, step time.Duration) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case <-done:
			return
		case <-time.After(time.Millisecond):
			if time.Now().After(deadline) {
				f.t.Fatal("the call never returned")
			}
			f.clk.Advance(step)
		}
	}
}

// background runs fn in a goroutine and returns a channel closed when it
// returns.
func background(fn func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	return done
}

// TestOpenDoesNotDialSynchronously checks the Opener contract: New
// returns while the server still holds the first connection, and the
// client is published only once the connection is ready.
func TestOpenDoesNotDialSynchronously(t *testing.T) {
	release := make(chan struct{})
	f := newFixture(t, fixtureOptions{setup: func(s *fakeServer) {
		s.inject(&fault{cmd: "SCRIPT LOAD", hold: release})
	}})
	waitFor(t, "the first dial", func() bool { return f.srv.count("SCRIPT LOAD") == 1 })
	select {
	case <-f.d.ready:
		t.Fatal("the client was published before its first connection was ready")
	default:
	}
	close(release)
	f.ready()
}

// TestCallWaitsForTheClient checks that a call made before the State
// client exists waits for it within its timeout and then succeeds.
func TestCallWaitsForTheClient(t *testing.T) {
	release := make(chan struct{})
	f := newFixture(t, fixtureOptions{setup: func(s *fakeServer) {
		s.inject(&fault{cmd: "SCRIPT LOAD", hold: release})
	}})
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	done := background(func() { f.consume(c) })
	waitFor(t, "the first dial", func() bool { return f.srv.count("SCRIPT LOAD") == 1 })
	close(release)
	<-done
	if c.Err != nil || !c.Done {
		t.Fatalf("call before the client: Done %v Err %v", c.Done, c.Err)
	}
}

// TestCallTimesOutWaitingForTheClient checks that a call waiting for a
// client that does not come fails at its timeout (RZ-STS-001).
func TestCallTimesOutWaitingForTheClient(t *testing.T) {
	release := make(chan struct{})
	f := newFixture(t, fixtureOptions{setup: func(s *fakeServer) {
		s.inject(&fault{cmd: "SCRIPT LOAD", hold: release})
	}})
	defer close(release)
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	f.advanceUntil(background(func() { f.consume(c) }), 10*time.Millisecond)
	if c.Err != statestore.ErrTimeout || c.Elapsed != callTO {
		t.Fatalf("Err %v Elapsed %v, want RZ-STS-001 after %v", c.Err, c.Elapsed, callTO)
	}
}

// TestTimeoutHoldsInFlightUntilReply checks RZ-STS-001 at the caller's
// timeout, and that the call keeps its in-flight slot until the reply,
// not its timeout.
func TestTimeoutHoldsInFlightUntilReply(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	hold := f.srv.inject(&fault{cmd: "EVALSHA", hold: make(chan struct{}), times: 1})
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	f.advanceUntil(background(func() { f.consume(c) }), 10*time.Millisecond)
	if c.Err != statestore.ErrTimeout || c.Done || c.Elapsed != callTO {
		t.Fatalf("held reply: Done %v Err %v Elapsed %v, want RZ-STS-001 after %v", c.Done, c.Err, c.Elapsed, callTO)
	}
	if f.m.call(emit.StateOpGCRA, statestore.ResultTimeout) != 1 || f.m.dur[emit.StateOpGCRA].n.Load() != 1 {
		t.Fatal("the timeout is not counted with its duration")
	}
	if f.gate.Len() != 1 {
		t.Fatalf("in flight %d after the caller's timeout, want 1 until the reply", f.gate.Len())
	}
	close(hold.hold)
	waitFor(t, "the late reply to free the slot", func() bool { return f.gate.Len() == 0 })
}

// TestTimeoutHandsOffTheTrip checks the hand-off of a trip at its
// timeout: the caller records the timeout and reads what it needs before
// leaving the trip, which the goroutine settles and recycles when the
// late reply comes, so the two never touch the trip together (run under
// -race), and every slot comes back.
func TestTimeoutHandsOffTheTrip(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	for i := range 50 {
		hold := f.srv.inject(&fault{cmd: "EVALSHA", hold: make(chan struct{}), times: 1})
		evals := f.srv.count("EVALSHA")
		c := gcraCall(statestore.DigestOf("k"), oneLimit())
		c.Timeout = time.Millisecond
		done := background(func() { f.consume(c) })
		waitFor(t, "the call to be sent", func() bool { return f.srv.count("EVALSHA") > evals })
		f.advanceUntil(done, time.Millisecond)
		if c.Err != statestore.ErrTimeout || c.Elapsed != time.Millisecond {
			t.Fatalf("iteration %d: Err %v Elapsed %v, want RZ-STS-001 after 1ms", i, c.Err, c.Elapsed)
		}
		close(hold.hold)
		waitFor(t, "the late reply", func() bool { return f.gate.Len() == 0 })
		// Let the timeouts leave the breaker's 5 s window.
		f.clk.Advance(6 * time.Second)
	}
}

// TestTimeoutRacingTheReply releases each reply as the timeout fires, so
// the reply and the timeout race for the trip: either way the call ends
// answered or with RZ-STS-001, and every slot comes back.
func TestTimeoutRacingTheReply(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	for i := range 50 {
		hold := f.srv.inject(&fault{cmd: "EVALSHA", hold: make(chan struct{}), times: 1})
		evals := f.srv.count("EVALSHA")
		c := gcraCall(statestore.DigestOf("k"), oneLimit())
		c.Timeout = time.Millisecond
		done := background(func() { f.consume(c) })
		waitFor(t, "the call to be sent", func() bool { return f.srv.count("EVALSHA") > evals })
		go close(hold.hold)
		f.advanceUntil(done, time.Millisecond)
		if c.Err != nil && c.Err != statestore.ErrTimeout {
			t.Fatalf("iteration %d: Err %v, want an answer or RZ-STS-001", i, c.Err)
		}
		waitFor(t, "the reply", func() bool { return f.gate.Len() == 0 })
		f.clk.Advance(6 * time.Second)
	}
}

// TestInFlightCeilingSkipsWithoutCall checks RZ-STS-004 when the in-flight
// ceiling is full: the call never reaches the server.
func TestInFlightCeilingSkipsWithoutCall(t *testing.T) {
	lim := statestore.DefaultLimits()
	lim.InFlight = 1
	f := newFixture(t, fixtureOptions{limits: lim})
	f.ready()
	hold := f.srv.inject(&fault{cmd: "EVALSHA", hold: make(chan struct{}), times: 1})
	first := gcraCall(statestore.DigestOf("a"), oneLimit())
	done := background(func() { f.consume(first) })
	waitFor(t, "the first call to fill the ceiling", func() bool { return f.srv.count("EVALSHA") == 1 })
	second := gcraCall(statestore.DigestOf("b"), oneLimit())
	f.consume(second)
	if second.Err != statestore.ErrNotAttempted {
		t.Fatalf("call beyond the ceiling: Err %v, want RZ-STS-004", second.Err)
	}
	if f.srv.count("EVALSHA") != 1 {
		t.Fatal("the skipped call reached the server")
	}
	if f.m.call(emit.StateOpGCRA, statestore.ResultSkipped) != 1 {
		t.Fatal("the skipped call is not counted")
	}
	close(hold.hold)
	<-done
	if first.Err != nil {
		t.Fatalf("first call: Err %v", first.Err)
	}
}

// TestPerRequestDeadlineClamps checks pack 8.7 rule 2: the round trip's
// timeout is the remaining per-request deadline when it is shorter, and a
// spent deadline is RZ-STS-004 without a call.
func TestPerRequestDeadlineClamps(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	hold := f.srv.inject(&fault{cmd: "EVALSHA", hold: make(chan struct{}), times: 1})
	defer close(hold.hold)
	rb := statestore.NewRequestBudget(20*time.Millisecond, 0)
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	f.advanceUntil(background(func() { f.d.Consume(context.Background(), &rb, []*statestore.Call{c}) }), 5*time.Millisecond)
	if c.Err != statestore.ErrTimeout || c.Elapsed != 20*time.Millisecond {
		t.Fatalf("Err %v Elapsed %v, want RZ-STS-001 after the 20ms deadline", c.Err, c.Elapsed)
	}
	evals := f.srv.count("EVALSHA")
	late := gcraCall(statestore.DigestOf("k2"), oneLimit())
	f.d.Consume(context.Background(), &rb, []*statestore.Call{late})
	if late.Err != statestore.ErrNotAttempted || f.srv.count("EVALSHA") != evals {
		t.Fatalf("spent deadline: Err %v, want RZ-STS-004 without a call", late.Err)
	}
}

// TestContextEndsCall checks a canceled request context: before the call
// it is RZ-STS-004 without a call, during the call RZ-STS-001.
func TestContextEndsCall(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	f.d.Consume(ctx, budget(), []*statestore.Call{c})
	if c.Err != statestore.ErrNotAttempted || f.srv.count("EVALSHA") != 0 {
		t.Fatalf("canceled before: Err %v, want RZ-STS-004 without a call", c.Err)
	}
	hold := f.srv.inject(&fault{cmd: "EVALSHA", hold: make(chan struct{}), times: 1})
	defer close(hold.hold)
	ctx, cancel = context.WithCancel(context.Background())
	c = gcraCall(statestore.DigestOf("k"), oneLimit())
	done := background(func() { f.d.Consume(ctx, budget(), []*statestore.Call{c}) })
	waitFor(t, "the call to be sent", func() bool { return f.srv.count("EVALSHA") == 1 })
	cancel()
	<-done
	if c.Err != statestore.ErrTimeout || c.Done {
		t.Fatalf("canceled during: Done %v Err %v, want RZ-STS-001", c.Done, c.Err)
	}
}

// TestFailedRepliesAreSTS002 checks RZ-STS-002 for an error reply, an
// unparsable reply and a dropped connection.
func TestFailedRepliesAreSTS002(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *fault
	}{
		{"error reply", &fault{cmd: "EVALSHA", err: "OOM command not allowed when used memory > 'maxmemory'.", times: 1}},
		{"unparsable reply", &fault{cmd: "EVALSHA", raw: "+OK\r\n", times: 1}},
		{"wrong shape", &fault{cmd: "EVALSHA", raw: "*1\r\n$1\r\n1\r\n", times: 1}},
		{"dropped connection", &fault{cmd: "EVALSHA", drop: true, times: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{})
			f.ready()
			f.srv.inject(tc.f)
			c := gcraCall(statestore.DigestOf("k"), oneLimit())
			f.consume(c)
			if c.Err != statestore.ErrFailed || c.Done {
				t.Fatalf("Done %v Err %v, want RZ-STS-002", c.Done, c.Err)
			}
			if f.m.call(emit.StateOpGCRA, statestore.ResultError) != 1 {
				t.Fatal("the failure is not counted as an error")
			}
		})
	}
}

// TestInvalidCallNotSent checks that a call the library would refuse
// fails with RZ-STS-002 without a round trip.
func TestInvalidCallNotSent(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	for _, c := range []*statestore.Call{
		gcraCall(statestore.DigestOf("k"), statestore.GCRALimit{Requests: 0, Window: time.Second}),
		gcraCall(statestore.DigestOf("k"), statestore.GCRALimit{Requests: 1, Window: time.Second, Burst: -1}),
		quotaCall(time.Microsecond, 1, statestore.DigestOf("k")),
		{Kind: statestore.OpGCRA, Timeout: callTO, GCRA: statestore.GCRA{Limits: []statestore.GCRALimit{oneLimit()}}},
	} {
		f.consume(c)
		if c.Err != statestore.ErrFailed || c.Done {
			t.Fatalf("invalid call: Done %v Err %v, want RZ-STS-002", c.Done, c.Err)
		}
	}
	if f.srv.count("EVALSHA") != 0 {
		t.Fatal("an invalid call reached the server")
	}
}

// TestMergedGroupOneEvalsha checks pack 8.7 rule 3: consumptive calls
// sharing a hash slot run as one EVALSHA labeled script_multi.
func TestMergedGroupOneEvalsha(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	d := statestore.DigestOf("k")
	g, q := gcraCall(d, oneLimit()), quotaCall(time.Hour, 5, d)
	if n := f.consume(g, q); n != 2 {
		t.Fatalf("Consume took %d calls, want 2", n)
	}
	if f.srv.count("EVALSHA") != 1 {
		t.Fatalf("%d EVALSHA for a merged group, want 1", f.srv.count("EVALSHA"))
	}
	if f.m.call(emit.StateOpScriptMulti, statestore.ResultOK) != 1 || f.m.ops[emit.StateOpGCRA].n.Load() != 1 || f.m.ops[emit.StateOpQuota].n.Load() != 1 {
		t.Fatal("the merged round trip is not counted as script_multi with one op per call")
	}
	if !g.Done || !q.Done || g.Trip != statestore.TripScriptMulti || q.Batch != 0 {
		t.Fatalf("merged calls: %+v %+v", g, q)
	}
}

// TestGCRAMatchesModel checks the driver's GCRA arithmetic over a burst
// and its denial against the memory driver's numbers: limit 10/1s, burst
// 10, so 11 admitted at one instant and the 12th denied for T.
func TestGCRAMatchesModel(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	d := statestore.DigestOf("k")
	for i := range 11 {
		c := gcraCall(d, oneLimit())
		if f.consume(c); !c.GCRA.Allowed {
			t.Fatalf("call %d denied", i+1)
		}
	}
	c := gcraCall(d, oneLimit())
	f.consume(c)
	g := c.GCRA
	if g.Allowed || g.Denied != 0 || g.RetryAfter != 100*time.Millisecond || g.Out[0].Remaining != 0 {
		t.Fatalf("12th call: %+v, want denied by limit 0 for 100ms", g)
	}
	if want := f.clk.Now().Truncate(time.Microsecond); !g.ServerNow.Equal(want) {
		t.Fatalf("ServerNow %v, want the server's TIME %v", g.ServerNow, want)
	}
}
