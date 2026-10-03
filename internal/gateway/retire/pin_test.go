// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package retire

import (
	"runtime"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Tests for spec 04 req 49 (one active snapshot behind one atomic.Pointer)
// and req 50 (one pin per request; load, increment the stripe, re-load,
// retry; S = min(GOMAXPROCS, 8) stripes assigned round robin per
// connection and offset by the HTTP/2 stream ID; pinning allocates
// nothing).

func TestReq49PinBeforeFirstPublish(t *testing.T) {
	x := newHarness(t, Config{}, false)
	if s := x.h.Pin(0, &snapshot.PinnedRequest{}); s != nil {
		t.Fatalf("Pin before Publish = %v, want nil", s)
	}
	if x.h.Active() != nil {
		t.Fatal("Active before Publish is not nil")
	}
}

func TestReq49Req50PinReturnsActiveAndCounts(t *testing.T) {
	x := newHarness(t, Config{}, false)
	a := newSnap("a", x.log)
	x.publish(a)
	if x.h.Active() != a {
		t.Fatal("Active is not the published snapshot")
	}
	var ps []pinned
	for i := range 10 {
		p := x.pin(emit.Stripe(i))
		if p.s != a {
			t.Fatalf("Pin returned %v, want the active snapshot", p.s)
		}
		ps = append(ps, p)
	}
	if n := a.Pins.Count(); n != 10 {
		t.Fatalf("pins = %d, want 10", n)
	}
	var listed int
	a.Pins.Each(func(*snapshot.PinnedRequest) { listed++ })
	if listed != 10 {
		t.Fatalf("registered records = %d, want 10 (req 53 registration at pin time)", listed)
	}
	for _, p := range ps {
		x.unpin(p)
	}
	if n := a.Pins.Count(); n != 0 {
		t.Fatalf("pins after unpin = %d, want 0", n)
	}
}

func TestReq50PublishCreatesPinsWhenNil(t *testing.T) {
	x := newHarness(t, Config{Stripes: 3}, false)
	s := newSnap("a", nil)
	s.Pins = nil
	x.publish(s)
	if s.Pins == nil {
		t.Fatal("Publish left Pins nil")
	}
	p := x.pin(7)
	x.unpin(p)
}

// TestReq50PinAllocatesNothing is the "pin path 0 allocations" gate for
// the active snapshot and for a retired one (whose Unpin wakes the
// retirer).
func TestReq50PinAllocatesNothing(t *testing.T) {
	x := newHarness(t, Config{}, false)
	a := newSnap("a", nil)
	x.publish(a)
	rec := &snapshot.PinnedRequest{}
	allocs := testing.AllocsPerRun(1000, func() {
		s := x.h.Pin(1, rec)
		x.h.Unpin(s, 1, rec)
	})
	if allocs != 0 {
		t.Fatalf("Pin+Unpin allocates %v, want 0", allocs)
	}
	held := &snapshot.PinnedRequest{}
	s := x.h.Pin(2, held)
	x.publish(newSnap("b", nil))
	retiredRec := &snapshot.PinnedRequest{}
	allocs = testing.AllocsPerRun(1000, func() {
		s.Pins.Add(1, retiredRec)
		x.h.Unpin(s, 1, retiredRec)
	})
	if allocs != 0 {
		t.Fatalf("Unpin of a retired snapshot allocates %v, want 0", allocs)
	}
	x.h.Unpin(s, 2, held)
}

func TestReq50StripeCount(t *testing.T) {
	n := StripeCount()
	want := min(max(runtime.GOMAXPROCS(0), 1), MaxStripes)
	if n != want {
		t.Fatalf("StripeCount = %d, want %d", n, want)
	}
	if h := New(Config{}); h.Stripes() != n {
		t.Fatalf("default Stripes = %d, want %d", h.Stripes(), n)
	}
	if h := New(Config{Stripes: 1000}); h.Stripes() != 255 {
		t.Fatalf("clamped Stripes = %d, want 255", h.Stripes())
	}
}

func TestReq50ConnStripeRoundRobin(t *testing.T) {
	h := New(Config{Stripes: 4})
	var got []emit.Stripe
	for range 9 {
		got = append(got, h.ConnStripe())
	}
	want := []emit.Stripe{0, 1, 2, 3, 0, 1, 2, 3, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ConnStripe sequence = %v, want %v", got, want)
		}
	}
}

// TestReq50ConnStripeUsesTheMeterSource: with Config.NewStripe set (the
// wiring passes emit.Meter.NewStripe), ConnStripe returns the meter's
// stripes, so pins and metrics share one round robin.
func TestReq50ConnStripeUsesTheMeterSource(t *testing.T) {
	var next emit.Stripe
	h := New(Config{Stripes: 3, NewStripe: func() emit.Stripe {
		s := next
		next = (next + 1) % 3
		return s
	}})
	var got []emit.Stripe
	for range 5 {
		got = append(got, h.ConnStripe())
	}
	if want := []emit.Stripe{0, 1, 2, 0, 1}; !slices.Equal(got, want) {
		t.Fatalf("ConnStripe sequence = %v, want the source's %v", got, want)
	}
	if h.rr.Load() != 0 {
		t.Fatal("the Holder's own round robin advanced while a source is set")
	}
}

func TestReq50RequestStripeOffsetByStreamID(t *testing.T) {
	h := New(Config{Stripes: 4})
	tests := []struct {
		name     string
		conn     emit.Stripe
		streamID uint32
		want     emit.Stripe
	}{
		{"http1", 2, 0, 2},
		{"h2 stream 1", 2, 1, 2},
		{"h2 stream 3", 2, 3, 3},
		{"h2 stream 5 wraps", 2, 5, 0},
		{"h2 stream 7", 2, 7, 1},
		{"h2 stream 9", 2, 9, 2},
		{"large stream id", 3, 1<<31 - 1, emit.Stripe((3 + (1<<31-1)/2) % 4)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.RequestStripe(tt.conn, tt.streamID); got != tt.want {
				t.Fatalf("RequestStripe(%d, %d) = %d, want %d", tt.conn, tt.streamID, got, tt.want)
			}
		})
	}
}

// TestReq50ConcurrentConnStripesStayInRange assigns stripes from many
// goroutines; every value is a valid stripe and each stripe is used
// equally.
func TestReq50ConcurrentConnStripesStayInRange(t *testing.T) {
	h := New(Config{Stripes: 8})
	var mu sync.Mutex
	counts := make([]int, 8)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 100 {
				s := h.ConnStripe()
				mu.Lock()
				counts[s]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	for i, c := range counts {
		if c != 200 {
			t.Fatalf("stripe %d assigned %d times, want 200 (%v)", i, c, counts)
		}
	}
}

// TestReq50PinRetriesOntoTheNewSnapshot swaps the pointer continuously
// while requests pin: every pin lands on a snapshot that was published
// when its re-load ran, and every retry undoes its increment, so the
// counts of all snapshots return to zero. Workers yield after every
// pin, so the publisher is not starved at small GOMAXPROCS.
func TestReq50PinRetriesOntoTheNewSnapshot(t *testing.T) {
	x := newHarness(t, Config{K: 1000}, false)
	snaps := []*snapshot.Snapshot{newSnap("s0", nil)}
	x.publish(snaps[0])
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range emit.Stripe(8) {
		wg.Go(func() {
			rec := &snapshot.PinnedRequest{}
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := x.h.Pin(w, rec)
				if s == nil {
					t.Error("Pin returned nil")
					return
				}
				x.h.Unpin(s, w, rec)
				runtime.Gosched()
			}
		})
	}
	for i := 1; i < 300; i++ {
		s := newSnap("s"+strconv.Itoa(i), nil)
		snaps = append(snaps, s)
		x.publish(s)
		runtime.Gosched()
	}
	close(stop)
	wg.Wait()
	for _, s := range snaps {
		if n := s.Pins.Count(); n != 0 {
			t.Fatalf("snapshot %s keeps %d pins after every unpin", s.Revision.Digest.Short(), n)
		}
	}
}

// BenchmarkPinUnpin measures the request-path pin (spec 04 req 33 budgets
// pin plus match at 3 µs p50 and 0 allocations).
func BenchmarkPinUnpin(b *testing.B) {
	h := New(Config{})
	if _, err := h.Publish(newSnap("a", nil)); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		stripe := h.ConnStripe()
		rec := &snapshot.PinnedRequest{}
		for pb.Next() {
			s := h.Pin(stripe, rec)
			h.Unpin(s, stripe, rec)
		}
	})
}
