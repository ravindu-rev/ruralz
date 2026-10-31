// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package breaker

import (
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// Tests for the Pacer: full-jitter reconnect pacing, at most 4 connects in
// any sliding second.

func TestPacerBackoffBounds(t *testing.T) {
	// Delays lie in [0, min(5 s, 100 ms × 2^n)] after n consecutive
	// connect failures.
	clk := clocktest.New(start())
	var gotHi []time.Duration
	rnd := func(lo, hi time.Duration) time.Duration {
		if lo != 0 {
			t.Fatalf("jitter lower bound %v, want 0", lo)
		}
		gotHi = append(gotHi, hi)
		return hi
	}
	p := NewPacer(clk, DefaultPacerConfig(), rnd)
	want := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 3200 * time.Millisecond, 5 * time.Second, 5 * time.Second}
	for n := range want {
		if !p.TryDial() {
			t.Fatalf("dial %d refused after waiting out the backoff", n)
		}
		p.Result(false)
		if p.Failures() != n+1 {
			t.Fatalf("failures %d, want %d", p.Failures(), n+1)
		}
		if gotHi[n] != want[n] {
			t.Fatalf("after %d failures the jitter ceiling is %v, want %v", n+1, gotHi[n], want[n])
		}
		// Refused until the delay ends, then allowed (the 1 s window also
		// passes: each delay here is at least 200 ms, and we wait 1 s more).
		if p.TryDial() {
			t.Fatalf("dial allowed inside the %v backoff", want[n])
		}
		if w := p.Wait(); w != want[n] {
			t.Fatalf("Wait = %v, want %v", w, want[n])
		}
		clk.Advance(want[n] + time.Second)
	}
	// A huge failure count does not overflow the ceiling.
	if c := p.ceiling(200); c != 5*time.Second {
		t.Fatalf("ceiling(200) = %v", c)
	}
	// The default Rand stays inside the bounds.
	d := NewPacer(clk, PacerConfig{}, nil)
	for range 50 {
		clk.Advance(10 * time.Second)
		if !d.TryDial() {
			t.Fatal("dial refused after 10 s")
		}
		d.Result(false)
		hi := d.ceiling(d.Failures())
		if w := d.Wait(); w < 0 || w > hi {
			t.Fatalf("delay %v outside [0, %v]", w, hi)
		}
	}
}

func TestPacerPerSecond(t *testing.T) {
	// At most 4 connects in any sliding 1 s window: the 5th within 1 s is
	// refused without counting as a connect failure.
	clk := clocktest.New(start())
	p := NewPacer(clk, DefaultPacerConfig(), fixed(0))
	for i := range 4 {
		if !p.TryDial() {
			t.Fatalf("dial %d refused", i)
		}
		p.Result(true)
		clk.Advance(100 * time.Millisecond)
	}
	if p.TryDial() {
		t.Fatal("5th dial within 1 s allowed")
	}
	if p.Failures() != 0 {
		t.Fatal("a paced refusal counted as a connect failure")
	}
	if w := p.Wait(); w != 600*time.Millisecond {
		t.Fatalf("Wait = %v, want 600ms until the first start leaves the window", w)
	}
	clk.Advance(600 * time.Millisecond)
	if !p.TryDial() {
		t.Fatal("dial refused 1 s after the oldest start")
	}
	if p.TryDial() {
		t.Fatal("window did not slide one start at a time")
	}
}

func TestPacerResetOnSuccess(t *testing.T) {
	clk := clocktest.New(start())
	p := NewPacer(clk, DefaultPacerConfig(), fixed(time.Hour))
	p.TryDial()
	p.Result(false)
	p.Result(false)
	if p.TryDial() {
		t.Fatal("dial allowed during backoff")
	}
	p.Result(true)
	if p.Failures() != 0 || !p.TryDial() {
		t.Fatal("success did not reset the backoff")
	}
}

// TestPacerBackoffPerAddress checks that the backoff after connect
// failures belongs to the address that failed: the primary shard of a
// sentinel deployment dials the replica the sentinels promote at once,
// while the dead primary's dials keep backing off, and the per-second
// limit stays the shard's (CE-5's reconnection bound, under sentinel).
func TestPacerBackoffPerAddress(t *testing.T) {
	clk := clocktest.New(start())
	p := NewPacer(clk, DefaultPacerConfig(), fixed(5*time.Second))
	for range 3 {
		if !p.TryDialAddr("primary:6379") {
			t.Fatal("a dial to the primary refused before its backoff")
		}
		p.ResultAddr("primary:6379", false)
		clk.Advance(5 * time.Second)
	}
	p.ResultAddr("primary:6379", false) // a failure that starts a fresh 5 s backoff
	if p.TryDialAddr("primary:6379") {
		t.Fatal("a dial to the failing primary allowed inside its backoff")
	}
	if !p.TryDialAddr("replica:6380") {
		t.Fatal("the promoted replica waits out the dead primary's backoff")
	}
	p.ResultAddr("replica:6380", true)
	if p.TryDialAddr("primary:6379") || p.Failures() != 4 {
		t.Fatalf("a success on the replica cleared the primary's backoff (failures %d)", p.Failures())
	}
	// The per-second limit counts every address's dials.
	for i := range 3 {
		if !p.TryDialAddr("replica:6380") {
			t.Fatalf("replica dial %d refused within the per-second limit", i)
		}
	}
	if p.TryDialAddr("other:6381") {
		t.Fatal("a fifth dial of the shard within 1 s allowed for another address")
	}
	// The kept backoffs stay bounded.
	for i := range MaxPacedAddrs + 4 {
		clk.Advance(time.Second)
		addr := "server-" + string(rune('a'+i)) + ":6379"
		if p.TryDialAddr(addr) {
			p.ResultAddr(addr, false)
		}
	}
	if n := len(p.backoffs); n > MaxPacedAddrs {
		t.Errorf("the pacer keeps %d addresses' backoffs, want at most %d", n, MaxPacedAddrs)
	}
}

func TestPacedRefusalIsNotConnectFailure(t *testing.T) {
	// A dial refused by pacing fails at once without touching the
	// network and does not count toward the breaker's 5 connect
	// failures.
	clk := clocktest.New(start())
	b := New(clk, DefaultConfig(), fixed(time.Second))
	p := NewPacer(clk, DefaultPacerConfig(), fixed(0))
	dial := func(fail bool) {
		if !p.TryDial() {
			return // refused: no network, no breaker event
		}
		if fail {
			b.ConnectFailed()
		} else {
			b.ConnectSucceeded()
		}
		p.Result(!fail)
	}
	for range 20 {
		dial(true) // only 4 reach the network within this second
	}
	if b.State() != Closed {
		t.Fatal("paced refusals opened the breaker")
	}
	clk.Advance(time.Second)
	dial(true)
	if b.State() != Open {
		t.Fatal("5th real connect failure did not open the breaker")
	}
}
