// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// lineWords is the number of 64-bit words in one 64-byte cache line.
const lineWords = 8

// cell holds the values of one group. A striped cell has one block of
// stride words per stripe, stride a multiple of eight words, so blocks of
// different stripes never share a 64-byte cache line (spec 09 req 49):
// the slice size is a multiple of 64 bytes, Go's size classes for such
// sizes are multiples of 64 too and spans start on page boundaries, so
// every block starts on a line boundary. An unstriped cell has stride 0
// and one unpadded word per value, so every stripe maps to it.
type cell struct {
	v      []atomic.Uint64
	tab    *[256]uint8
	stride int
}

// newCell returns a cell of words values, striped over stripes blocks when
// striped is true and there is more than one stripe.
func newCell(tab *[256]uint8, words, stripes int, striped bool) *cell {
	if !striped || stripes <= 1 {
		return &cell{v: make([]atomic.Uint64, words), tab: tab}
	}
	stride := (words + lineWords - 1) / lineWords * lineWords
	return &cell{v: make([]atomic.Uint64, stride*stripes), tab: tab, stride: stride}
}

// sum returns the sum over stripes of word i, modulo 2^64.
func (c *cell) sum(i int) uint64 {
	if c.stride == 0 {
		return c.v[i].Load()
	}
	var s uint64
	for b := i; b < len(c.v); b += c.stride {
		s += c.v[b].Load()
	}
	return s
}

// slot addresses one counter or gauge value (or the first value of a
// status, attempts or listener-requests block) inside a cell. Slots are
// immutable; handles swap slot pointers when a label set folds.
type slot struct {
	c   *cell
	off int
}

// word returns the value's word on stripe s.
func (s *slot) word(st emit.Stripe) *atomic.Uint64 {
	return &s.c.v[int(s.c.tab[st])*s.c.stride+s.off]
}

// wordAt returns word off+i of the slot's block on stripe s.
func (s *slot) wordAt(st emit.Stripe, i int) *atomic.Uint64 {
	return &s.c.v[int(s.c.tab[st])*s.c.stride+s.off+i]
}

// sum returns the value's sum over stripes.
func (s *slot) sum() uint64 { return s.c.sum(s.off) }

// set makes the gauge's sum over stripes v: an unstriped gauge stores v,
// a striped one adds the difference to the current sum on stripe 0, so
// concurrent Adds on other stripes are kept.
func (s *slot) set(v int64) {
	c := s.c
	if c.stride == 0 {
		c.v[s.off].Store(u64(v))
		return
	}
	c.v[s.off].Add(u64(v) - c.sum(s.off))
}

// statusClass maps an HTTP status to its status_class index (1xx is 0).
// Statuses outside 100 to 599, including 0 for an attempt without a
// response, count as 5xx (spec 09 section 9 item 9).
func statusClass(status int) int {
	c := status/100 - 1
	if uint(c) >= numClasses {
		return numClasses - 1
	}
	return c
}

// errBounds reports a histogram bound set this package cannot record into.
var errBounds = errors.New("aggregate: unsupported histogram bounds")

// histBounds are one bound set in the integer unit recorded values use:
// nanoseconds for fast, request and control, bytes, or millionths for
// ratio (spec 09 req 42).
type histBounds struct {
	ints   [numBounds]uint64
	floats []float64
	// micros is true when _sum accumulates microseconds, rounded to the
	// nearest (request and control).
	micros bool
	// sumDiv converts the integer _sum unit to base units by division,
	// which rounds 3500 microseconds to exactly 0.0035 s.
	sumDiv float64
	// valDiv converts a recorded value to base units (exemplars).
	valDiv float64
}

// newHistBounds converts a catalog bound set.
func newHistBounds(b catalog.Bounds) (*histBounds, error) {
	f := b.Seconds()
	if len(f) != numBounds {
		return nil, fmt.Errorf("%w: set %d has %d bounds, want %d", errBounds, b, len(f), numBounds)
	}
	hb := &histBounds{floats: f}
	var scale float64
	switch b {
	case catalog.BoundsFast:
		scale, hb.sumDiv, hb.valDiv = 1e9, 1e9, 1e9
	case catalog.BoundsRequest, catalog.BoundsControl:
		scale, hb.sumDiv, hb.valDiv, hb.micros = 1e9, 1e6, 1e9, true
	case catalog.BoundsBytes:
		scale, hb.sumDiv, hb.valDiv = 1, 1, 1
	case catalog.BoundsRatio:
		scale, hb.sumDiv, hb.valDiv = 1e6, 1e6, 1e6
	default:
		return nil, fmt.Errorf("%w: set %d", errBounds, b)
	}
	for i, x := range f {
		hb.ints[i] = uint64(math.Round(x * scale))
		if i > 0 && hb.ints[i] <= hb.ints[i-1] {
			return nil, fmt.Errorf("%w: set %d is not increasing at %d", errBounds, b, i)
		}
	}
	return hb, nil
}

// bucket returns the index of the first bound v does not exceed, 13 for
// +Inf. Bucket selection compares integers, so a threshold is exact:
// 150,000 ns falls in le="0.00015" and 150,001 ns does not.
func (b *histBounds) bucket(v uint64) int {
	lo, hi := 0, numBounds
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if v <= b.ints[m] {
			hi = m
		} else {
			lo = m + 1
		}
	}
	return lo
}

// sumUnits converts a recorded value to the _sum unit.
func (b *histBounds) sumUnits(v uint64) uint64 {
	if b.micros {
		return v/1000 + (v%1000+500)/1000
	}
	return v
}

// histSlot addresses one histogram label set: a 16-word block per stripe
// and its exemplar slot. Immutable, like slot.
type histSlot struct {
	c   *cell
	off int
	ex  *exemplarSlot
	b   *histBounds
}

// record adds one observation on stripe s.
func (h *histSlot) record(s emit.Stripe, v uint64) {
	base := int(h.c.tab[s])*h.c.stride + h.off
	h.c.v[base+h.b.bucket(v)].Add(1)
	h.c.v[base+sumWord].Add(h.b.sumUnits(v))
}

// exemplarSlot is the one exemplar of a histogram label set, written only
// by sampled requests under a sequence lock (spec 09 req 52). Every field
// is an atomic word so readers never race writers; a writer that finds
// another writer active drops its exemplar.
type exemplarSlot struct {
	seq   atomic.Uint64
	val   atomic.Uint64
	at    atomic.Int64
	trace [2]atomic.Uint64
	span  atomic.Uint64
}

// exemplarValue is a consistent copy of an exemplar slot.
type exemplarValue struct {
	val   uint64
	at    int64
	trace [16]byte
	span  [8]byte
}

// store writes an exemplar unless another writer holds the slot.
func (e *exemplarSlot) store(v uint64, ex *emit.Exemplar) {
	s := e.seq.Load()
	if s&1 != 0 || !e.seq.CompareAndSwap(s, s+1) {
		return
	}
	e.val.Store(v)
	var at int64
	if !ex.Time.IsZero() {
		at = ex.Time.UnixNano()
	}
	e.at.Store(at)
	e.trace[0].Store(binary.BigEndian.Uint64(ex.TraceID[:8]))
	e.trace[1].Store(binary.BigEndian.Uint64(ex.TraceID[8:]))
	e.span.Store(binary.BigEndian.Uint64(ex.SpanID[:]))
	e.seq.Store(s + 2)
}

// load returns the exemplar, false when none was written or a writer kept
// the slot busy through every attempt.
func (e *exemplarSlot) load() (exemplarValue, bool) {
	const attempts = 4
	for range attempts {
		s1 := e.seq.Load()
		if s1 == 0 {
			return exemplarValue{}, false
		}
		if s1&1 != 0 {
			continue
		}
		var x exemplarValue
		x.val = e.val.Load()
		x.at = e.at.Load()
		binary.BigEndian.PutUint64(x.trace[:8], e.trace[0].Load())
		binary.BigEndian.PutUint64(x.trace[8:], e.trace[1].Load())
		binary.BigEndian.PutUint64(x.span[:], e.span.Load())
		if e.seq.Load() == s1 {
			return x, true
		}
	}
	return exemplarValue{}, false
}

// time returns the exemplar time, or fallback when the recorder gave none.
func (x *exemplarValue) time(fallback time.Time) time.Time {
	if x.at == 0 {
		return fallback
	}
	return time.Unix(0, x.at)
}

// u64 reinterprets a two's complement int64 (gauge arithmetic).
func u64(v int64) uint64 { return uint64(v) } //nolint:gosec // G115: two's complement reinterpretation is intended.

// i64 reinterprets a uint64 sum of gauge words as a two's complement int64.
func i64(v uint64) int64 { return int64(v) } //nolint:gosec // G115: two's complement reinterpretation is intended.
