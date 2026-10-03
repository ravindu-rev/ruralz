// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package body

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Tests for spec 04 req 47 (Node buffer budget: one per Node, carried
// across Hot Reloads, 32 KiB increments, 25% stream share gates cannot
// take, ErrBudget 503 RZ-RT-004, gauge ruralz_node_buffered_bytes), spec
// 04 req 67 (handover external usage) and spec 07 reqs 69-71 (decoded
// values in increments, 4x cap); test plan item 8.

// sumGauge is an emit.Gauge summing its adds across stripes.
type sumGauge struct{ v atomic.Int64 }

func (g *sumGauge) Add(_ emit.Stripe, d int64) { g.v.Add(d) }
func (g *sumGauge) Set(v int64)                { g.v.Store(v) }

const kib = 1 << 10

// invariantErr reports how the budget breaks spec 04 req 47, if it does:
// the reserved total must stay within the effective total, and gates never
// inside the stream share.
func invariantErr(b *Budget) error {
	g, s := unpack(b.used.Load())
	eff, share := unpack(b.lim.Load())
	switch {
	case g < 0 || s < 0:
		return fmt.Errorf("negative budget: gate %d, stream %d increments", g, s)
	case g+s > eff:
		return fmt.Errorf("used %d increments over the effective total %d", g+s, eff)
	case g+max(s, share) > eff:
		return fmt.Errorf("gates (%d) took the stream share (%d, streams %d, total %d)", g, share, s, eff)
	}
	return nil
}

func checkInvariants(t testing.TB, b *Budget) {
	t.Helper()
	if err := invariantErr(b); err != nil {
		t.Fatal(err)
	}
}

func TestReq47BudgetLimits(t *testing.T) {
	tests := []struct {
		name      string
		total     int64
		external  int64
		effective int64
		share     int64
	}{
		{"default 512 MiB, 128 MiB share", DefaultMaxBufferedBytes, 0, 512 << 20, 128 << 20},
		{"128 KiB", 128 * kib, 0, 128 * kib, 32 * kib},
		{"64 KiB: share rounds down to zero increments", 64 * kib, 0, 64 * kib, 0},
		{"not a multiple of 32 KiB rounds down", 100 * kib, 0, 96 * kib, 0},
		{"zero", 0, 0, 0, 0},
		{"negative is zero", -1, 0, 0, 0},
		{"handover external lowers the total, share kept", 128 * kib, 64 * kib, 64 * kib, 32 * kib},
		{"external above the total", 128 * kib, 1 << 30, 0, 32 * kib},
		{"negative external is zero", 128 * kib, -5, 128 * kib, 32 * kib},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBudget(tt.total, nil)
			b.SetExternal(tt.external)
			if b.Effective() != tt.effective || b.StreamShare() != tt.share {
				t.Fatalf("effective %d, share %d; want %d, %d", b.Effective(), b.StreamShare(), tt.effective, tt.share)
			}
			if b.Total() != max(0, tt.total) {
				t.Fatalf("Total = %d", b.Total())
			}
		})
	}
	// A total beyond 2^32 increments is clamped, never wrapped.
	b := NewBudget(1<<62, nil)
	if b.Effective() != counterMask*Increment {
		t.Fatalf("huge total effective %d", b.Effective())
	}
}

// TestReq47ReservationIncrements: an Account reserves whole 32 KiB
// increments as its charge grows and returns those no longer needed; a
// charge past the int64 range or the budget is ErrBudget and charges
// nothing.
func TestReq47ReservationIncrements(t *testing.T) {
	g := &sumGauge{}
	b := NewBudget(1<<20, g)
	var a Account
	a.Bind(b, Gate, 0)
	steps := []struct {
		reserve, release int64
		err              error
		charged, held    int64
	}{
		{reserve: 1, charged: 1, held: 32 * kib},
		{reserve: 32*kib - 1, charged: 32 * kib, held: 32 * kib},
		{reserve: 1, charged: 32*kib + 1, held: 64 * kib},
		{reserve: 100 * kib, charged: 132*kib + 1, held: 160 * kib},
		{release: 100 * kib, charged: 32*kib + 1, held: 64 * kib},
		{release: 1, charged: 32 * kib, held: 32 * kib},
		{reserve: 0, charged: 32 * kib, held: 32 * kib},
		{reserve: -7, charged: 32 * kib, held: 32 * kib},
		{release: -3, charged: 32 * kib, held: 32 * kib},
		{release: 1 << 20, charged: 0, held: 0}, // more than charged clamps
		{reserve: 1, charged: 1, held: 32 * kib},
		// 1 + maxInt64 overflows: refused, never a negative charge.
		{reserve: maxInt64, err: ErrBudget, charged: 1, held: 32 * kib},
		{reserve: maxInt64 - 1, err: ErrBudget, charged: 1, held: 32 * kib},
		// Within range but past the 768 KiB of gate budget.
		{reserve: 1 << 20, err: ErrBudget, charged: 1, held: 32 * kib},
		{release: 1, charged: 0, held: 0},
		{reserve: maxInt64, err: ErrBudget, charged: 0, held: 0},
	}
	for i, st := range steps {
		if st.reserve != 0 {
			if err := a.Reserve(st.reserve); !errors.Is(err, st.err) || (st.err == nil && err != nil) {
				t.Fatalf("step %d: Reserve(%d) = %v, want %v", i, st.reserve, err, st.err)
			}
		}
		a.Release(st.release)
		if a.Charged() != st.charged || a.Reserved() != st.held || b.GateUsed() != st.held || g.v.Load() != st.held {
			t.Fatalf("step %d: charged %d, reserved %d, budget %d, gauge %d; want %d, %d", i, a.Charged(), a.Reserved(), b.GateUsed(), g.v.Load(), st.charged, st.held)
		}
		checkInvariants(t, b)
	}
}

// TestReq47GatesNeverTakeStreamShare runs gate and stream reservations
// against a 128 KiB budget (32 KiB share): gates stop at 96 KiB, streams
// use the share and then free general budget.
func TestReq47GatesNeverTakeStreamShare(t *testing.T) {
	type op struct {
		kind    Kind
		release bool
		n       int64
		ok      bool
	}
	tests := []struct {
		name string
		ops  []op
		gate int64
		strm int64
	}{
		{
			name: "gates stop before the share",
			ops:  []op{{Gate, false, 96 * kib, true}, {Gate, false, 1, false}},
			gate: 96 * kib,
		},
		{
			name: "one gate cannot reach into the share",
			ops:  []op{{Gate, false, 96*kib + 1, false}},
		},
		{
			name: "a stream takes the share while gates hold the rest",
			ops:  []op{{Gate, false, 96 * kib, true}, {Stream, false, 32 * kib, true}, {Stream, false, 1, false}},
			gate: 96 * kib, strm: 32 * kib,
		},
		{
			name: "streams use the share then free general budget",
			ops:  []op{{Stream, false, 32 * kib, true}, {Stream, false, 64 * kib, true}, {Gate, false, 32 * kib, true}, {Gate, false, 1, false}},
			gate: 32 * kib, strm: 96 * kib,
		},
		{
			name: "streams in the general budget squeeze gates",
			ops:  []op{{Stream, false, 128 * kib, true}, {Gate, false, 1, false}, {Stream, false, 1, false}},
			strm: 128 * kib,
		},
		{
			name: "released general budget returns to gates",
			ops:  []op{{Stream, false, 128 * kib, true}, {Stream, true, 64 * kib, true}, {Gate, false, 64 * kib, true}},
			gate: 64 * kib, strm: 64 * kib,
		},
		{
			name: "a stream larger than the budget",
			ops:  []op{{Stream, false, 128*kib + 1, false}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBudget(128*kib, nil)
			var gate Account
			gate.Bind(b, Gate, 0)
			for i, o := range tt.ops {
				var err error
				switch {
				case o.kind == Stream && o.release:
					b.ReleaseStream(o.n, 0)
				case o.kind == Stream:
					err = b.ReserveStream(o.n, 0)
				default:
					err = gate.Reserve(o.n)
				}
				if (err == nil) != o.ok {
					t.Fatalf("op %d: err %v, want ok %v", i, err, o.ok)
				}
				if err != nil && !errors.Is(err, ErrBudget) {
					t.Fatalf("op %d: %v is not ErrBudget", i, err)
				}
				checkInvariants(t, b)
			}
			if b.GateUsed() != tt.gate || b.StreamUsed() != tt.strm {
				t.Fatalf("gate %d, stream %d; want %d, %d", b.GateUsed(), b.StreamUsed(), tt.gate, tt.strm)
			}
			gate.ReleaseAll()
			b.ReleaseStream(b.StreamUsed(), 0)
			if b.Used() != 0 {
				t.Fatalf("Used = %d after releasing everything", b.Used())
			}
		})
	}
}

// TestReq47SetTotalCarriesReservations: an activation lowering
// maxBufferedBytes keeps outstanding reservations; new ones fail until
// enough is released (a new value applies to new reservations).
func TestReq47SetTotalCarriesReservations(t *testing.T) {
	b := NewBudget(256*kib, nil)
	var a, c Account
	a.Bind(b, Gate, 0)
	c.Bind(b, Gate, 0)
	if err := a.Reserve(192 * kib); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	b.SetTotal(128 * kib)
	if a.Reserved() != 192*kib || b.Used() != 192*kib {
		t.Fatalf("SetTotal changed an outstanding reservation: %d", b.Used())
	}
	if err := c.Reserve(1); !errors.Is(err, ErrBudget) {
		t.Fatalf("Reserve over the lowered total = %v, want ErrBudget", err)
	}
	if err := b.ReserveStream(1, 0); !errors.Is(err, ErrBudget) {
		t.Fatalf("stream over the lowered total = %v, want ErrBudget", err)
	}
	a.Release(128 * kib)
	if err := c.Reserve(1); err != nil {
		t.Fatalf("Reserve after release: %v", err)
	}
	b.SetTotal(1 << 20)
	if err := c.Reserve(512 * kib); err != nil {
		t.Fatalf("Reserve after raising: %v", err)
	}
	a.ReleaseAll()
	c.ReleaseAll()
	if b.Used() != 0 {
		t.Fatalf("Used = %d", b.Used())
	}
}

// stripeGauge is an emit.Gauge keeping one sum per stripe.
type stripeGauge struct{ v [8]atomic.Int64 }

func (g *stripeGauge) Add(s emit.Stripe, d int64) { g.v[s].Add(d) }
func (g *stripeGauge) Set(int64)                  {}

// TestReq47GaugeStripes: every holder records ruralz_node_buffered_bytes on
// its own request stripe, so concurrent requests never share one gauge
// cache line Node-wide, and each stripe returns to zero.
func TestReq47GaugeStripes(t *testing.T) {
	g := &stripeGauge{}
	b := NewBudget(4<<20, g)
	var a Account
	a.Bind(b, Gate, 1)
	if err := a.Reserve(kib); err != nil {
		t.Fatal(err)
	}
	gate, err := ReadGate(context.Background(), bytes.NewReader(pattern(40*kib)), -1, 1<<20, b, 2)
	if err != nil {
		t.Fatal(err)
	}
	tee := b.NewTee(1<<20, nil, 3)
	_, _ = tee.Write(pattern(kib))
	if err := b.ReserveStream(kib, 4); err != nil {
		t.Fatal(err)
	}
	var q Request
	if err := q.Prepare(context.Background(), bytes.NewReader(pattern(kib)), -1, true, 1<<20, b, 5); err != nil {
		t.Fatal(err)
	}
	if err := q.Replace(pattern(40 * kib)); err != nil {
		t.Fatal(err)
	}
	want := [8]int64{0, Increment, 2 * Increment, Increment, Increment, 2 * Increment, 0, 0}
	for s := range g.v {
		if got := g.v[s].Load(); got != want[s] {
			t.Fatalf("stripe %d: gauge %d, want %d", s, got, want[s])
		}
	}
	a.ReleaseAll()
	gate.Release()
	tee.Release()
	b.ReleaseStream(kib, 4)
	q.Reset()
	for s := range g.v {
		if got := g.v[s].Load(); got != 0 {
			t.Fatalf("stripe %d: gauge %d after every release", s, got)
		}
	}
}

// TestReq47StreamZero: an empty stream reservation takes nothing.
func TestReq47StreamZero(t *testing.T) {
	b := NewBudget(0, nil)
	if err := b.ReserveStream(0, 0); err != nil {
		t.Fatalf("ReserveStream(0, 0) = %v", err)
	}
	if err := b.ReserveStream(1, 0); !errors.Is(err, ErrBudget) {
		t.Fatalf("ReserveStream on an empty budget = %v", err)
	}
	b.ReleaseStream(0, 0)
	if b.Used() != 0 {
		t.Fatalf("Used = %d", b.Used())
	}
}

// TestReq67BudgetSetExternal reduces the budget by the other process's
// buffered bytes during a handover.
func TestReq67BudgetSetExternal(t *testing.T) {
	b := NewBudget(512*kib, nil)
	b.SetExternal(256 * kib)
	var a Account
	a.Bind(b, Gate, 0)
	// 256 KiB effective, 128 KiB share: gates get 128 KiB.
	if err := a.Reserve(128 * kib); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Reserve(1); !errors.Is(err, ErrBudget) {
		t.Fatalf("Reserve = %v, want ErrBudget", err)
	}
	b.SetExternal(0)
	if err := a.Reserve(1); err != nil {
		t.Fatalf("Reserve after the holder exited: %v", err)
	}
	a.ReleaseAll()
}

// TestReq47AccountUnbound: the zero Account refuses and releases nothing.
func TestReq47AccountUnbound(t *testing.T) {
	var a Account
	if err := a.Reserve(1); !errors.Is(err, ErrBudget) {
		t.Fatalf("unbound Reserve = %v", err)
	}
	a.Release(1)
	a.ReleaseAll()
	if err := a.Reserve(0); err != nil {
		t.Fatalf("Reserve(0) = %v", err)
	}
	// Bind returns what an Account holds before rebinding.
	b1, b2 := NewBudget(1<<20, nil), NewBudget(1<<20, nil)
	a.Bind(b1, Gate, 0)
	if err := a.Reserve(40 * kib); err != nil {
		t.Fatal(err)
	}
	a.Bind(b2, Stream, 0)
	if b1.Used() != 0 || a.Charged() != 0 {
		t.Fatalf("rebinding kept %d bytes on the old budget", b1.Used())
	}
	if err := a.Reserve(1); err != nil || b2.StreamUsed() != 32*kib {
		t.Fatalf("stream account: %v, %d", err, b2.StreamUsed())
	}
	a.ReleaseAll()
	if b2.Used() != 0 {
		t.Fatalf("Used = %d", b2.Used())
	}
}

// TestReq47DecodedValues charges decoded values at their built size up to
// 4x the raw limit (spec 04 req 47, spec 07 reqs 15 and 69).
func TestReq47DecodedValues(t *testing.T) {
	const raw = 10 * kib
	tests := []struct {
		name  string
		built int64
		err   error
	}{
		{"small", 100, nil},
		{"exactly 4x", 4 * raw, nil},
		{"4x plus one is oversized", 4*raw + 1, ErrTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBudget(1<<20, nil)
			var a Account
			a.Bind(b, Gate, 0)
			err := a.ReserveDecoded(tt.built, raw)
			if !errors.Is(err, tt.err) || (tt.err == nil && err != nil) {
				t.Fatalf("ReserveDecoded(%d) = %v, want %v", tt.built, err, tt.err)
			}
			if tt.err != nil && b.Used() != 0 {
				t.Fatal("an oversized value was charged")
			}
			if tt.err == nil && a.Reserved() != increments(tt.built)*Increment {
				t.Fatalf("reserved %d for %d built bytes", a.Reserved(), tt.built)
			}
			a.ReleaseAll()
		})
	}
	// The budget refusing a decoded value is ErrBudget, not oversize.
	b := NewBudget(64*kib, nil)
	var a Account
	a.Bind(b, Gate, 0)
	if err := a.ReserveDecoded(100*kib, raw*4); !errors.Is(err, ErrBudget) {
		t.Fatalf("ReserveDecoded over the budget = %v, want ErrBudget", err)
	}
}

// lcg is a small deterministic generator for the property test.
type lcg struct{ s int64 }

func (x *lcg) int64n(n int64) int64 {
	x.s = (x.s*1103515245 + 12345) & 0x7fffffff
	return x.s % n
}

func (x *lcg) intn(n int) int { return int(x.int64n(int64(n))) }

// TestReq47BudgetConcurrentProperty reserves and releases random sizes of
// both kinds from many goroutines while an observer checks the invariants;
// the budget never goes negative or over its total and returns to zero.
func TestReq47BudgetConcurrentProperty(t *testing.T) {
	g := &sumGauge{}
	b := NewBudget(2<<20, g)
	stop := make(chan struct{})
	var observer sync.WaitGroup
	observer.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				if err := invariantErr(b); err != nil {
					t.Error(err)
					return
				}
			}
		}
	})
	var ok, refused atomic.Int64
	var wg sync.WaitGroup
	for w := range 32 {
		wg.Go(func() {
			r := &lcg{s: int64(w)*7919 + 47}
			var gate Account
			gate.Bind(b, Gate, 0)
			var streams []int64
			for range 2_000 {
				switch r.intn(4) {
				case 0:
					if gate.Reserve(r.int64n(96*kib)+1) == nil {
						ok.Add(1)
					} else {
						refused.Add(1)
					}
				case 1:
					gate.Release(r.int64n(96 * kib))
				case 2:
					n := r.int64n(64*kib) + 1
					if b.ReserveStream(n, 0) == nil {
						streams = append(streams, n)
						ok.Add(1)
					} else {
						refused.Add(1)
					}
				default:
					if len(streams) > 0 {
						b.ReleaseStream(streams[len(streams)-1], 0)
						streams = streams[:len(streams)-1]
					}
				}
			}
			gate.ReleaseAll()
			for _, n := range streams {
				b.ReleaseStream(n, 0)
			}
		})
	}
	wg.Wait()
	close(stop)
	observer.Wait()
	checkInvariants(t, b)
	if b.Used() != 0 || g.v.Load() != 0 {
		t.Fatalf("Used %d, gauge %d after every release; want 0", b.Used(), g.v.Load())
	}
	if ok.Load() == 0 || refused.Load() == 0 {
		t.Fatalf("the property run did not reach the ceiling: %d ok, %d refused", ok.Load(), refused.Load())
	}
}

// FuzzBudget replays a byte string as reserve and release operations on
// three accounts and a stream holder, checking the invariants after each
// and a return to zero at the end.
func FuzzBudget(f *testing.F) {
	f.Add([]byte{0, 10, 1, 5, 2, 200, 3, 7}, uint16(8))
	f.Add([]byte{2, 255, 2, 255, 0, 1, 4, 4}, uint16(4))
	f.Add([]byte{}, uint16(0))
	f.Fuzz(func(t *testing.T, ops []byte, totalInc uint16) {
		b := NewBudget(int64(totalInc)*Increment+int64(totalInc%7), nil)
		var accts [3]Account
		for i := range accts {
			accts[i].Bind(b, Kind(i%2), 0)
		}
		var streams []int64
		for i := 0; i+1 < len(ops); i += 2 {
			op, n := ops[i]%6, int64(ops[i+1])*kib
			switch op {
			case 0, 1, 2:
				_ = accts[op].Reserve(n)
			case 3:
				accts[int(n/kib)%3].Release(n)
			case 4:
				if b.ReserveStream(n, 0) == nil {
					streams = append(streams, n)
				}
			default:
				if len(streams) > 0 {
					b.ReleaseStream(streams[0], 0)
					streams = streams[1:]
				}
			}
			checkInvariants(t, b)
		}
		for i := range accts {
			accts[i].ReleaseAll()
		}
		for _, n := range streams {
			b.ReleaseStream(n, 0)
		}
		if b.Used() != 0 {
			t.Fatalf("Used = %d at the end", b.Used())
		}
	})
}

// BenchmarkBudgetAccount is one decoded-value reservation and release.
func BenchmarkBudgetAccount(b *testing.B) {
	bud := NewBudget(DefaultMaxBufferedBytes, &sumGauge{})
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var a Account
		a.Bind(bud, Gate, 0)
		for pb.Next() {
			if a.Reserve(4*kib) == nil {
				a.Release(4 * kib)
			}
		}
	})
}
