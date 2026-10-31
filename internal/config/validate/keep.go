// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"cmp"
	"container/heap"
	"slices"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// finding is one diagnostic with its path text, computed once, and its
// report order: the work unit (a resource's index, or a cross-resource
// pass after them) in the high 32 bits and the unit's own count below.
type finding struct {
	d    diag.Diagnostic
	path string
	seq  uint64
}

// compareFindings is the diag.List.Sort order, ties broken by report
// order, so the kept findings and their order never depend on the worker
// count.
func compareFindings(a, b *finding) int {
	return cmp.Or(
		cmp.Compare(a.d.Environment, b.d.Environment),
		cmp.Compare(a.d.File, b.d.File),
		cmp.Compare(a.d.Line, b.d.Line),
		cmp.Compare(a.d.Column, b.d.Column),
		cmp.Compare(a.d.Code, b.d.Code),
		cmp.Compare(a.path, b.path),
		cmp.Compare(a.d.Message, b.d.Message),
		cmp.Compare(a.seq, b.seq),
	)
}

// keeper holds the first limit findings of a run in compareFindings order:
// a max-heap, so a finding that sorts after every kept one is dropped once
// the heap is full. It is safe for concurrent use.
type keeper struct {
	mu    sync.Mutex
	limit int
	h     findingHeap
}

// newKeeper returns a keeper of at most limit findings (at least 1).
func newKeeper(limit int) *keeper {
	return &keeper{limit: max(1, limit)}
}

// add offers f: it is kept while the heap has room, or when it sorts
// before the last kept finding, which it then replaces.
func (k *keeper) add(f *finding) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.h) < k.limit {
		heap.Push(&k.h, f)
		return
	}
	if compareFindings(f, k.h[0]) < 0 {
		k.h[0] = f
		heap.Fix(&k.h, 0)
	}
}

// list returns the kept diagnostics in compareFindings order.
func (k *keeper) list() diag.List {
	k.mu.Lock()
	fs := slices.Clone(k.h)
	k.mu.Unlock()
	slices.SortFunc(fs, compareFindings)
	out := make(diag.List, len(fs))
	for i, f := range fs {
		out[i] = f.d
	}
	return out
}

// findingHeap is a max-heap of findings by compareFindings.
type findingHeap []*finding

func (h findingHeap) Len() int           { return len(h) }
func (h findingHeap) Less(i, j int) bool { return compareFindings(h[i], h[j]) > 0 }
func (h findingHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

// Push appends x, a *finding (heap.Interface).
func (h *findingHeap) Push(x any) {
	f, _ := x.(*finding)
	*h = append(*h, f)
}

// Pop removes the last finding (heap.Interface).
func (h *findingHeap) Pop() any {
	old := *h
	f := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return f
}

// sink numbers the findings of one work unit and hands them to the
// keeper. It belongs to one goroutine.
type sink struct {
	k    *keeper
	unit uint64
	n    uint64
}

// add offers d with the unit's next report number.
func (s *sink) add(d diag.Diagnostic) {
	s.n++
	s.k.add(&finding{d: d, path: d.Path.String(), seq: s.unit<<32 | s.n})
}
