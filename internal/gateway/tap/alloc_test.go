// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tap

import "testing"

// Allocation gate and benchmarks of docs/architecture/10-observability.md
// "Overhead per signal": /tap without a subscriber, 0 allocations (one
// atomic load); with one subscriber, 1 allocation per event at most.

// TestNoSubscriberAllocatesNothing checks the no-subscriber row: Active
// and Publish allocate nothing.
func TestNoSubscriberAllocatesNothing(t *testing.T) {
	h := New(Config{})
	ev, _ := fullEvent()
	if n := testing.AllocsPerRun(100, func() {
		if h.Active() {
			t.Fatal("Active without subscribers")
		}
		h.Publish(0, &ev)
	}); n != 0 {
		t.Fatalf("%v allocations per exchange, want 0", n)
	}
}

// TestOneSubscriberAllocations checks the one-subscriber row: at most 1
// allocation per published event, headers and redaction included.
func TestOneSubscriberAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are meaningless under -race")
	}
	h := New(Config{})
	s := h.subscribe(1)
	ev, _ := fullEvent()
	if n := testing.AllocsPerRun(1000, func() {
		h.Publish(0, &ev)
		s.consume(len(s.pending()))
	}); n > 1 {
		t.Fatalf("%v allocations per event, want at most 1", n)
	}
}

func BenchmarkActiveNoSubscriber(b *testing.B) {
	h := New(Config{})
	for b.Loop() {
		if h.Active() {
			b.Fatal("Active without subscribers")
		}
	}
}

func BenchmarkPublishOneSubscriber(b *testing.B) {
	h := New(Config{})
	s := h.subscribe(1)
	ev, _ := fullEvent()
	b.ReportAllocs()
	for b.Loop() {
		h.Publish(0, &ev)
		s.consume(len(s.pending()))
	}
}
