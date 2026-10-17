// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"math"
	"runtime"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// yieldCheck is the number of work units (see work) between two reads of
// the yield timer (01 req 53: checked every 256 nodes inside a unit; a
// byte of text counts as a node here, as in stage F).
const yieldCheck = 256

// DefaultYieldAfter is the work a worker does before it yields the
// processor when Options.YieldAfter is zero (01 req 53; the loader's
// Options.Yield, 100 µs).
const DefaultYieldAfter = 100 * time.Microsecond

// work is the state one goroutine carries through every step of stage G
// (Materialize, conversion, Normalize, Decode) and from one resource to
// the next: the yield timer of 01 req 53, one counter for all steps, and
// the diagnostic budget of the resource being processed (01 req 50). A
// work value belongs to one goroutine.
//
// A work unit is one of: a node visited; a byte of a string, member name
// or number literal that a step reads (scalar normalization, the UTF-8
// and number checks, host lower-casing, the JSON text of a decode piece,
// the locator's number checks); a sort comparison and a byte of the
// common prefix it reads; or a member, node or byte that a lookup scans
// (lookup.go). total measures the stage's work independently of the
// machine (TestScaleLinear, TestScaleBytes).
//
// The timer is read every yieldCheck units and around each encoding/json
// call, so the time between two reads is bounded by yieldCheck units of
// stage G's own loops plus the longest single call it makes into another
// package, which cannot yield inside: the encoding/json call of one
// decode piece (at most decodeChunk nodes and about decodeBytes bytes of
// JSON text, or one scalar or member name longer than that), the one
// call of a value with its own UnmarshalJSON (chunker), one lookup
// (schemaidx.Node.Select, schemaidx.ItemElem, schemaidx.AppendItemJSON of
// one set element or key), and the parse of one scalar (time.ParseDuration,
// v1alpha1.ParseByteSize, jsonval.CheckNumber). Each of these is linear in
// the one value it reads, which the source limits bound (01 req 53;
// TestYieldGapBytes).
type work struct {
	// units counts the work units since the timer was last read.
	units int
	// total counts every work unit of the worker, for tests.
	total int
	// now is the clock of the yield timer (Options.Now); nil never yields.
	now func() time.Time
	// after is the work between two yields (Options.YieldAfter).
	after time.Duration
	// last is when the worker last yielded, or started.
	last time.Time
	// yields counts the yields, for tests.
	yields int
	// keep is the number of diagnostics one resource records: the cap of
	// the run plus one (Options.MaxDiagnostics).
	keep int
	// left is the number of diagnostics the current resource may still
	// record; begin resets it.
	left int
	// chunk, when positive, replaces decodeChunk, so tests can split
	// every value.
	chunk int
	// decodes counts the encoding/json calls, for tests.
	decodes int
	// unmarshals counts the direct UnmarshalJSON calls, for tests.
	unmarshals int
	// chunkBytes, when positive, replaces decodeBytes, so tests can split
	// by text size.
	chunkBytes int
	// widest is the most nodes one encoding/json call decoded, counted
	// up to the chunk limit plus one, and widestBytes the longest JSON
	// text one call decoded, for tests.
	widest, widestBytes int
	// decoded, when set, receives the JSON text of each encoding/json
	// call, for tests.
	decoded func(text []byte)
}

// newWork returns the state of a worker without a yield timer that keeps
// DefaultMaxDiagnostics+1 diagnostics per resource, as the exported step
// functions (Materialize, Normalize, Decode) use.
func newWork() *work {
	return &work{keep: keepOf(DefaultMaxDiagnostics), left: keepOf(DefaultMaxDiagnostics)}
}

// newWork returns the state of one worker of s: its yield timer starts
// now, and each resource keeps the cap of s plus one diagnostics.
func (s *Stage) newWork() *work {
	keep := keepOf(s.maxDiagnostics)
	w := &work{now: s.now, after: s.yieldAfter, keep: keep, left: keep}
	if w.now != nil {
		w.last = w.now()
	}
	return w
}

// keepOf returns the number of diagnostics kept for the cap
// maxDiagnostics (≥ 0): one more, so a collector of the same cap sees
// that it was reached, with math.MaxInt counting as math.MaxInt-1 so the
// sum never overflows (no run reaches either).
func keepOf(maxDiagnostics int) int {
	return min(maxDiagnostics, math.MaxInt-1) + 1
}

// begin starts a resource: its diagnostic budget is reset.
func (w *work) begin() { w.left = w.keep }

// tick counts one work unit and, every yieldCheck units, yields when due.
// A nil w does nothing, for the helpers tests call without a worker.
func (w *work) tick() { w.charge(1) }

// charge counts k work units, as k ticks would, reading the yield timer
// once when they reach yieldCheck. A nil w does nothing.
func (w *work) charge(k int) {
	if w == nil || k <= 0 {
		return
	}
	w.total += k
	if w.units += k; w.units >= yieldCheck {
		w.units = 0
		w.check()
	}
}

// piece records that one encoding/json call decodes nodes nodes from
// text bytes of JSON text. A nil w does nothing.
func (w *work) piece(nodes, text int) {
	if w != nil {
		w.widest = max(w.widest, nodes)
		w.widestBytes = max(w.widestBytes, text)
	}
}

// check yields the processor once w.after has passed on w's clock since
// the last yield (01 req 53). Run calls it between resources, Decode
// before and after each encoding/json call, charge every yieldCheck
// units. A nil w, or one without a clock, does nothing.
func (w *work) check() {
	if w == nil || w.now == nil {
		return
	}
	if w.now().Sub(w.last) >= w.after {
		runtime.Gosched()
		w.yields++
		w.last = w.now()
	}
}

// chunkLimit returns the most nodes one encoding/json call decodes.
func (w *work) chunkLimit() int {
	if w.chunk > 0 {
		return w.chunk
	}
	return decodeChunk
}

// bytesLimit returns the text bytes after which a decode piece is closed.
func (w *work) bytesLimit() int {
	if w.chunkBytes > 0 {
		return w.chunkBytes
	}
	return decodeBytes
}

// take reserves room for one diagnostic of the current resource. It
// returns false when the budget is spent: the caller then drops the
// diagnostic, and the resource still fails when it was an error.
func (w *work) take() bool {
	if w.left <= 0 {
		return false
	}
	w.left--
	return true
}

// admit returns the prefix of ds the budget has room for.
func (w *work) admit(ds diag.List) diag.List {
	for i := range ds {
		if !w.take() {
			return ds[:i]
		}
	}
	return ds
}
