// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package body

import (
	"bytes"
	"io"
	"sync/atomic"
	"testing"
	"testing/iotest"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Tests for spec 04 req 46 (tee for the store side of cache: out of budget
// or over its limit it stops copying and skips the store, counted by
// ruralz_cache_store_skipped_total, never failing the request) and spec 04
// req 47 (the tee's bytes come from the buffer budget); test plan item 8.

type countCounter struct {
	n      atomic.Uint64
	stripe atomic.Uint32
}

func (c *countCounter) Add(s emit.Stripe, n uint64) {
	c.stripe.Store(uint32(s))
	c.n.Add(n)
}

func skipCounters() (*[emit.NumSkipReasons]emit.Counter, [emit.NumSkipReasons]*countCounter) {
	var arr [emit.NumSkipReasons]emit.Counter
	var cs [emit.NumSkipReasons]*countCounter
	for i := range arr {
		cs[i] = &countCounter{}
		arr[i] = cs[i]
	}
	return &arr, cs
}

func TestReq46Tee(t *testing.T) {
	tests := []struct {
		name     string
		total    int64 // budget
		limit    int64
		size     int
		complete bool
		reason   int // -1: not skipped
	}{
		{"copies a small body", 1 << 20, 64 * kib, 10 * kib, true, -1},
		{"exactly the limit", 1 << 20, 64 * kib, 64 * kib, true, -1},
		{"over the limit skips", 1 << 20, 64 * kib, 64*kib + 1, false, emit.SkipSizeLimit},
		{"negative limit skips at the first byte", 1 << 20, -1, 1, false, emit.SkipSizeLimit},
		{"empty body completes", 1 << 20, 64 * kib, 0, true, -1},
		// Doubling would reach 16 MiB; the limit caps the copy's array.
		{"8 MiB + 1 under a 10 MiB limit", 64 << 20, 10 << 20, 8<<20 + 1, true, -1},
		// 128 KiB total, 32 KiB share: the tee gets 96 KiB of gate budget.
		{"out of budget skips", 128 * kib, 1 << 20, 100 * kib, false, emit.SkipBufferBudget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bud := NewBudget(tt.total, nil)
			skipped, counts := skipCounters()
			tee := bud.NewTee(tt.limit, skipped, 3)
			data := pattern(tt.size)
			src := tee.Reader(iotest.HalfReader(bytes.NewReader(data)))
			var client bytes.Buffer
			n, err := io.Copy(&client, src)
			// The client always gets the whole body.
			if err != nil || n != int64(tt.size) || !bytes.Equal(client.Bytes(), data) {
				t.Fatalf("client got %d bytes, %v", n, err)
			}
			got, complete := tee.Result()
			if complete != tt.complete {
				t.Fatalf("complete = %v, want %v", complete, tt.complete)
			}
			if complete && !bytes.Equal(got, data) {
				t.Fatalf("tee copied %d bytes, want %d", len(got), len(data))
			}
			isSkipped, reason := tee.Skipped()
			if isSkipped != (tt.reason >= 0) || (isSkipped && reason != tt.reason) {
				t.Fatalf("Skipped = %v, %d; want reason %d", isSkipped, reason, tt.reason)
			}
			for r, c := range counts {
				want := uint64(0)
				if r == tt.reason {
					want = 1
				}
				if c.n.Load() != want {
					t.Fatalf("skip counter %d = %d, want %d", r, c.n.Load(), want)
				}
				if want == 1 && c.stripe.Load() != 3 {
					t.Fatalf("skip counted on stripe %d", c.stripe.Load())
				}
			}
			if isSkipped && (bud.Used() != 0 || tee.Reserved() != 0) {
				t.Fatalf("a skipped tee kept %d bytes reserved", bud.Used())
			}
			// The reservation covers the copy's array, which grows with the
			// bytes copied: at least their increments, at most twice.
			if r := tee.Reserved(); !isSkipped && (bud.GateUsed() != r || int64(cap(tee.b)) > r ||
				r < increments(int64(tt.size))*Increment || r > 2*increments(int64(tt.size))*Increment) {
				t.Fatalf("reserved %d (budget %d, capacity %d) for %d bytes", r, bud.GateUsed(), cap(tee.b), tt.size)
			}
			tee.Release()
			if got, complete := tee.Result(); got != nil || complete || tee.Reserved() != 0 {
				t.Fatal("a released Tee is not inert")
			}
			tee.Release()
			if bud.Used() != 0 {
				t.Fatalf("Used = %d after Release", bud.Used())
			}
		})
	}
}

// TestReq46TeeWrite: Write never fails, so a tee next to the client writer
// cannot fail the response; Complete is explicit for writers.
func TestReq46TeeWrite(t *testing.T) {
	bud := NewBudget(1<<20, nil)
	tee := bud.NewTee(8, nil, 0)
	var _ filter.Tee = tee
	var client bytes.Buffer
	w := io.MultiWriter(&client, tee)
	for _, chunk := range []string{"abc", "", "defg"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if _, complete := tee.Result(); complete {
		t.Fatal("complete before Complete")
	}
	tee.Complete()
	if got, complete := tee.Result(); !complete || string(got) != "abcdefg" {
		t.Fatalf("Result = %q, %v", got, complete)
	}
	// Past the limit with nil counters: skipped, still no error.
	tee2 := bud.NewTee(4, nil, 0)
	if n, err := tee2.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	tee2.Complete()
	if _, complete := tee2.Result(); complete {
		t.Fatal("a skipped tee reported complete")
	}
	if _, err := tee2.Write([]byte("x")); err != nil {
		t.Fatalf("Write after skip = %v", err)
	}
	tee.Release()
	tee2.Release()
	if bud.Used() != 0 {
		t.Fatalf("Used = %d", bud.Used())
	}
}

// TestReq46TeeReaderError: a source error leaves the copy incomplete.
func TestReq46TeeReaderError(t *testing.T) {
	bud := NewBudget(1<<20, nil)
	tee := bud.NewTee(1<<20, nil, 0)
	src := tee.Reader(io.MultiReader(bytes.NewReader(pattern(100)), iotest.ErrReader(io.ErrUnexpectedEOF)))
	if _, err := io.Copy(io.Discard, src); err == nil {
		t.Fatal("no error")
	}
	if _, complete := tee.Result(); complete {
		t.Fatal("an interrupted body is complete")
	}
	tee.Release()
}

// TestReq46TeeStaleRelease: a Tee is never reused, so a stale handle's
// second Release or late Write cannot touch a later copy (spec 07 req 73).
func TestReq46TeeStaleRelease(t *testing.T) {
	bud := NewBudget(1<<20, nil)
	t1 := bud.NewTee(1<<20, nil, 0)
	_, _ = t1.Write(bytes.Repeat([]byte("A"), kib))
	t1.Complete()
	t1.Release()
	t2 := bud.NewTee(1<<20, nil, 0)
	if t1 == t2 {
		t.Fatal("NewTee handed out a released Tee again")
	}
	want := bytes.Repeat([]byte("B"), kib)
	_, _ = t2.Write(want)
	t1.Release()
	_, _ = t1.Write([]byte("late"))
	if _, err := io.Copy(io.Discard, t1.Reader(bytes.NewReader([]byte("late")))); err != nil {
		t.Fatalf("a released Tee's reader = %v", err)
	}
	t2.Complete()
	if got, complete := t2.Result(); !complete || !bytes.Equal(got, want) || bud.GateUsed() != Increment {
		t.Fatalf("a stale handle changed another copy: %d bytes, budget %d", len(got), bud.GateUsed())
	}
	if _, complete := t1.Result(); complete {
		t.Fatal("a released Tee reported a copy")
	}
	t2.Release()
	if bud.Used() != 0 {
		t.Fatalf("Used = %d", bud.Used())
	}
}

// TestReq46TeeGrowthNearCeiling: near the ceiling the copy grows in
// smaller steps, down to the increments its bytes need, before it skips.
func TestReq46TeeGrowthNearCeiling(t *testing.T) {
	// 128 KiB total, 32 KiB share: 3 increments for gates.
	bud := NewBudget(128*kib, nil)
	tee := bud.NewTee(1<<20, nil, 0)
	_, _ = tee.Write(pattern(64 * kib)) // 2 increments
	// 65 KiB needs 3; doubling asks for 4 and falls back to 3.
	_, _ = tee.Write(pattern(kib))
	if skipped, _ := tee.Skipped(); skipped || tee.Reserved() != 96*kib || int64(cap(tee.b)) > tee.Reserved() {
		t.Fatalf("skipped %v, reserved %d, capacity %d", skipped, tee.Reserved(), cap(tee.b))
	}
	_, _ = tee.Write(pattern(31 * kib)) // exactly 96 KiB
	_, _ = tee.Write(pattern(1))
	if skipped, reason := tee.Skipped(); !skipped || reason != emit.SkipBufferBudget || bud.Used() != 0 {
		t.Fatalf("skipped %v (%d), used %d", skipped, reason, bud.Used())
	}
	tee.Release()
	// A zero Tee has no budget: it skips at the first byte.
	var zero Tee
	zero.limit = 10
	_, _ = zero.Write([]byte("x"))
	if skipped, reason := zero.Skipped(); !skipped || reason != emit.SkipBufferBudget {
		t.Fatalf("zero Tee skipped %v (%d)", skipped, reason)
	}
	zero.Release()
}

// TestReq73TeePool: a released tee drops its array, returning one of one
// or two increments to the pool and leaving larger ones to the garbage
// collector; a new tee starts over.
func TestReq73TeePool(t *testing.T) {
	bud := NewBudget(4<<20, nil)
	big := bud.NewTee(1<<20, nil, 0)
	_, _ = big.Write(pattern(200 * kib))
	big.Complete()
	if cap(big.b) <= PoolMax {
		t.Fatalf("a 200 KiB copy has capacity %d", cap(big.b))
	}
	big.Release()
	if big.b != nil || big.bud != nil {
		t.Fatal("a released tee kept its array")
	}
	small := bud.NewTee(1<<20, nil, 0)
	_, _ = small.Write(pattern(kib))
	if cap(small.b) != Increment {
		t.Fatalf("a 1 KiB copy has capacity %d, want one increment", cap(small.b))
	}
	small.Release()
	if small.b != nil || small.bud != nil {
		t.Fatal("a released tee was not reset")
	}
	again := bud.NewTee(10, nil, 0)
	if _, complete := again.Result(); complete {
		t.Fatal("a new tee is complete")
	}
	if skipped, _ := again.Skipped(); skipped {
		t.Fatal("a new tee is skipped")
	}
	again.Release()
	// A skipped copy drops its array at once.
	sk := bud.NewTee(100*kib, nil, 0)
	_, _ = sk.Write(pattern(90 * kib))
	_, _ = sk.Write(pattern(20 * kib))
	if sk.b != nil {
		t.Fatal("a skipped large copy kept its slice")
	}
	sk.Release()
	if bud.Used() != 0 {
		t.Fatalf("Used = %d", bud.Used())
	}
}

// BenchmarkTee64KiB copies a 64 KiB response for the Response Cache.
func BenchmarkTee64KiB(b *testing.B) {
	bud := NewBudget(DefaultMaxBufferedBytes, &sumGauge{})
	data := pattern(64 * kib)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		tee := bud.NewTee(1<<20, nil, 0)
		for off := 0; off < len(data); off += 16 * kib {
			_, _ = tee.Write(data[off : off+16*kib])
		}
		tee.Complete()
		tee.Release()
	}
}
