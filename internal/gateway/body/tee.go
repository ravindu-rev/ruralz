// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package body

import (
	"errors"
	"io"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Tee states.
const (
	teeCopying uint8 = iota
	teeComplete
	teeSkipped
	teeReleased
)

// Tee is the store side of the Response Cache (spec 04 req 46): a copy of
// the response kept while it streams on to the client. Its backing array is
// reserved from the budget (Gate kind) in whole increments before it is
// allocated, so the reservation always covers the memory the copy holds.
// Past its limit or when the budget refuses an increment, it stops copying,
// returns its reservation at once, counts ruralz_cache_store_skipped_total
// with reason size_limit or buffer_budget, and never fails the response. It
// implements filter.Tee. A Tee is written by one goroutine at a time;
// Result is read in Finish, after the response finished streaming.
type Tee struct {
	bud     *Budget
	b       []byte
	held    int64 // increments reserved; cap(b) == held*Increment
	limit   int64
	state   uint8
	reason  int
	skipped *[emit.NumSkipReasons]emit.Counter
	stripe  emit.Stripe
	rd      teeReader
}

var _ filter.Tee = (*Tee)(nil)

// NewTee starts a copy of at most limit bytes charged to b. skipped is the
// calling Policy's ruralz_cache_store_skipped_total handles (nil counts
// nothing); the skip counter and the buffered-bytes gauge record on the
// request's stripe s. A limit below zero copies nothing and skips at the
// first byte. Only the copy's backing array is pooled; the Tee itself is
// never reused, so a stale handle cannot reach another response.
func (b *Budget) NewTee(limit int64, skipped *[emit.NumSkipReasons]emit.Counter, s emit.Stripe) *Tee {
	return &Tee{bud: b, limit: limit, skipped: skipped, stripe: s}
}

// Write copies p while the Tee is copying; it always reports len(p) and no
// error, so an io.MultiWriter toward the client never fails because of it.
func (t *Tee) Write(p []byte) (int, error) {
	t.copy(p)
	return len(p), nil
}

func (t *Tee) copy(p []byte) {
	if t.state != teeCopying || len(p) == 0 {
		return
	}
	n := int64(len(t.b)) + int64(len(p))
	if n > t.limit {
		t.skip(emit.SkipSizeLimit)
		return
	}
	if n > int64(cap(t.b)) && !t.grow(n) {
		t.skip(emit.SkipBufferBudget)
		return
	}
	t.b = append(t.b, p...)
}

// grow raises the capacity to hold n bytes: doubling, at least one
// increment, never past what the limit needs (n is within it), so a copy
// of up to 64 KiB stays poolable. It reserves the increments covering the
// new capacity before allocating it; when the budget refuses them it asks
// for fewer (halving, down to those covering n) and allocates only what it
// reserved. It reports false, changing nothing, when even the increments
// covering n are refused.
func (t *Tee) grow(n int64) bool {
	if t.bud == nil {
		return false
	}
	want := min(max(2*int64(cap(t.b)), Increment, n), t.limit)
	got := t.bud.reserveUpTo(Gate, increments(n)-t.held, increments(want)-t.held, t.stripe)
	if got == 0 {
		return false
	}
	t.held += got
	nb := append(t.bud.array(t.held*Increment), t.b...)
	t.bud.recycle(t.b)
	t.b = nb
	return true
}

// skip stops copying and returns the reservation and the array at once.
func (t *Tee) skip(reason int) {
	t.state, t.reason = teeSkipped, reason
	t.drop()
	if t.skipped != nil && t.skipped[reason] != nil {
		t.skipped[reason].Add(t.stripe, 1)
	}
}

// drop returns the reservation and recycles the array.
func (t *Tee) drop() {
	if t.bud != nil {
		t.bud.release(Gate, t.held, t.stripe)
		t.bud.recycle(t.b)
	}
	t.b, t.held = nil, 0
}

// Complete records that the response body reached its end; only a complete
// copy is stored.
func (t *Tee) Complete() {
	if t.state == teeCopying {
		t.state = teeComplete
	}
}

// Reader returns r copying every byte read into the Tee; io.EOF from r
// completes it. After Release the reader still reads r but copies nothing.
func (t *Tee) Reader(r io.Reader) io.Reader {
	t.rd = teeReader{t: t, r: r}
	return &t.rd
}

// Result returns the copied body and whether it is complete (the source
// ended and nothing was skipped); implements filter.Tee.
func (t *Tee) Result() (body []byte, complete bool) {
	if t.state != teeComplete {
		return nil, false
	}
	return t.b, true
}

// Skipped reports whether the copy stopped and why (emit.SkipSizeLimit or
// emit.SkipBufferBudget).
func (t *Tee) Skipped() (skipped bool, reason int) {
	return t.state == teeSkipped, t.reason
}

// Reserved returns the bytes reserved from the budget (whole increments);
// it is the capacity of the copy's backing array.
func (t *Tee) Reserved() int64 { return t.held * Increment }

// Release returns the reservation and recycles the copy's backing array
// (one of one or two increments, at most PoolMax, goes back to the
// budget's pool). Call it after Result was consumed: no slice obtained from
// Result may be used afterwards. The Tee is inert after Release: Result
// reports no copy, writes and reads through Reader copy nothing, and a
// second Release does nothing.
func (t *Tee) Release() {
	if t.state == teeReleased {
		return
	}
	t.drop()
	t.state, t.bud, t.skipped = teeReleased, nil, nil
}

// teeReader copies what it reads into its Tee.
type teeReader struct {
	t *Tee
	r io.Reader
}

// Read reads from the source and copies the bytes read.
func (tr *teeReader) Read(p []byte) (int, error) {
	n, err := tr.r.Read(p)
	tr.t.copy(p[:n])
	if errors.Is(err, io.EOF) {
		tr.t.Complete()
	}
	return n, err
}
