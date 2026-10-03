// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package breaker

import (
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// Tests for spec 08 req 22 (full-jitter reconnect pacing, at most 4
// connects in any sliding second) and the Pacer row of the 08 section 6.1
// unit table.

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

func TestPacedRefusalIsNotConnectFailure(t *testing.T) {
	// 08 req 22: a dial refused by pacing fails at once without touching
	// the network and does not count toward the breaker's 5 connect
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
