// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package breaker

import (
	"testing"
	"time"
)

// TestCancelFreesProbe checks that a canceled probe frees the half-open
// breaker for the next probe without recording an outcome.
func TestCancelFreesProbe(t *testing.T) {
	b, clk, _ := newBreaker(t, time.Second)
	run(t, b, 20, 20)
	if b.State() != Open {
		t.Fatalf("state %v after 20 failures, want open", b.State())
	}
	clk.Advance(time.Second)
	var probe Ticket
	if !b.Allow(&probe) || !probe.Probe() {
		t.Fatal("first call after the delay not admitted as the probe")
	}
	var other Ticket
	if b.Allow(&other) {
		t.Fatal("a second call admitted while the probe is in flight")
	}
	if !b.Cancel(&probe) {
		t.Fatal("Cancel did not withdraw the probe")
	}
	if b.State() != HalfOpen {
		t.Fatalf("state %v after a canceled probe, want half_open", b.State())
	}
	var next Ticket
	if !b.Allow(&next) || !next.Probe() {
		t.Fatal("the next call not admitted as a probe after Cancel")
	}
	if b.Done(&probe, true) {
		t.Fatal("Done of a canceled ticket recorded an outcome")
	}
	b.Done(&next, true)
	if b.State() != HalfOpen {
		t.Fatalf("state %v after one probe success, want half_open", b.State())
	}
}

// TestCancelClosedRecordsNothing checks that a canceled ticket of a closed
// breaker adds nothing to the failure window.
func TestCancelClosedRecordsNothing(t *testing.T) {
	b, _, _ := newBreaker(t, time.Second)
	run(t, b, 19, 9)
	for range 30 {
		var tk Ticket
		if !b.Allow(&tk) {
			t.Fatal("closed breaker refused a call")
		}
		b.Cancel(&tk)
	}
	// 9 failures of 19 calls: one more failure reaches 10 of 20 and opens
	// the breaker, so the canceled tickets counted as nothing.
	run(t, b, 1, 1)
	if b.State() != Open {
		t.Fatalf("state %v, want open after the 20th call", b.State())
	}
	var late Ticket
	if b.Cancel(&late) {
		t.Fatal("Cancel withdrew a ticket Allow never admitted")
	}
}
