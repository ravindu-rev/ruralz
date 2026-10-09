// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// codeEntry is one code label set of a code table. Its counter is
// unstriped: code series are rarer than their family's hot label sets.
type codeEntry struct {
	code string
	n    atomic.Uint64

	// Collection state, owned by the collector.
	seen  bool
	start time.Time
	last  uint64
	total float64
	attrs attribute.Set
}

// codeTable maps RZ codes to counters without locks: a fixed
// open-addressing table whose slots are installed once by compare and
// swap. Entries created up front (the codes a Policy type can emit, or
// every registered code) are exported only after their first record, so
// label sets with a code dimension appear on first record (spec 09 req
// 45). A code the registry does not accept, or one past the table's
// capacity, records nothing and is logged once per table.
type codeTable struct {
	name   string // the metric family, named in the drop record
	slots  []atomic.Pointer[codeEntry]
	mask   uint32
	max    int32
	count  atomic.Int32
	valid  func(string) bool
	logger *slog.Logger
	logged atomic.Bool
}

// Reasons a code label is not recorded (catalog.KeyReason values of the
// DEBUG record).
const (
	dropUnregistered = "unregistered"
	dropTableFull    = "table_full"
)

// newCodeTable returns a table of family name holding at most capacity
// codes (at least the pre-created ones), pre-populated with codes.
func newCodeTable(name string, codes []string, capacity int, valid func(string) bool, logger *slog.Logger) *codeTable {
	capacity = max(capacity, len(codes))
	size := 16
	for size < 2*capacity {
		size *= 2
	}
	t := &codeTable{
		name:   name,
		slots:  make([]atomic.Pointer[codeEntry], size),
		mask:   uint32(size - 1), //nolint:gosec // G115: size is a small power of two.
		max:    int32(capacity),  //nolint:gosec // G115: at most the registered code count plus spare.
		valid:  valid,
		logger: logger,
	}
	for _, c := range codes {
		t.insert(&codeEntry{code: c})
	}
	return t
}

// hashCode is 32-bit FNV-1a.
func hashCode(s string) uint32 {
	h := uint32(2166136261)
	for i := range len(s) {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// inc counts one record of code.
func (t *codeTable) inc(code string) {
	h := hashCode(code) & t.mask
	for range t.slots {
		e := t.slots[h].Load()
		if e == nil {
			t.install(h, code)
			return
		}
		if e.code == code {
			e.n.Add(1)
			return
		}
		h = (h + 1) & t.mask
	}
	t.drop(code, dropTableFull)
}

// install is the slow path of the first record of a code that has no
// entry yet, starting at empty slot h. It reserves a place in count before
// installing, so concurrent first records of different codes never hold
// more than max entries, which the retiring ceiling (group ceil) relies
// on. A reservation that loses to an entry for the same code is returned;
// one past max re-probes for an entry of the same code that a concurrent
// first record installed in the last place, and otherwise drops the
// record. Near capacity, a reservation held briefly by such a losing
// record can therefore drop the first record of a code that would have
// fit once it is returned.
//
//go:noinline
func (t *codeTable) install(h uint32, code string) {
	if !t.valid(code) {
		t.drop(code, dropUnregistered)
		return
	}
	if t.count.Add(1) > t.max {
		t.count.Add(-1)
		for range t.slots {
			o := t.slots[h].Load()
			if o == nil {
				break
			}
			if o.code == code {
				o.n.Add(1)
				return
			}
			h = (h + 1) & t.mask
		}
		t.drop(code, dropTableFull)
		return
	}
	e := &codeEntry{code: strings.Clone(code)}
	e.n.Store(1)
	for range t.slots {
		if t.slots[h].CompareAndSwap(nil, e) {
			return
		}
		if o := t.slots[h].Load(); o.code == code {
			t.count.Add(-1)
			o.n.Add(1)
			return
		}
		h = (h + 1) & t.mask
	}
	t.count.Add(-1)
	t.drop(code, dropTableFull)
}

// insert adds a pre-created entry; used before the table is shared.
func (t *codeTable) insert(e *codeEntry) {
	h := hashCode(e.code) & t.mask
	for range t.slots {
		o := t.slots[h].Load()
		if o == nil {
			t.slots[h].Store(e)
			t.count.Add(1)
			return
		}
		if o.code == e.code {
			return
		}
		h = (h + 1) & t.mask
	}
}

// drop logs the first dropped code of the table at DEBUG (spec 09 test
// 12). It runs on the recording goroutine, but the record is enqueued
// without blocking (req 61). emit.CodeCounter and emit.AuthDecisions
// carry no context, so the record has no trace context.
func (t *codeTable) drop(code, reason string) {
	if t.logger == nil || !t.logged.CompareAndSwap(false, true) {
		return
	}
	t.logger.LogAttrs(context.Background(), slog.LevelDebug, "metric code label not recorded",
		slog.String(catalog.KeyMetric, t.name), slog.String(catalog.KeyCode, code), slog.String(catalog.KeyReason, reason))
}

// entries appends the table's entries to dst.
func (t *codeTable) entries(dst []*codeEntry) []*codeEntry {
	for i := range t.slots {
		if e := t.slots[i].Load(); e != nil {
			dst = append(dst, e)
		}
	}
	return dst
}

// syntacticCode reports whether s has the shape RZ-<AREA>-<NNN> with an
// area of two to four upper-case letters; the registry uses it when it
// was given no registered code list.
func syntacticCode(s string) bool {
	rest, ok := strings.CutPrefix(s, "RZ-")
	if !ok {
		return false
	}
	area, num, ok := strings.Cut(rest, "-")
	if !ok || len(area) < 2 || len(area) > 4 || len(num) != 3 {
		return false
	}
	for i := range len(area) {
		if area[i] < 'A' || area[i] > 'Z' {
			return false
		}
	}
	for i := range len(num) {
		if num[i] < '0' || num[i] > '9' {
			return false
		}
	}
	return true
}
