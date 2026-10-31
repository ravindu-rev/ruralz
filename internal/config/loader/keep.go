// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"cmp"
	"math"
	"slices"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// Work steps, in run order. A diagnostic's step, item and position break
// ties of the diag.List.Sort key, so the kept set is one total order.
const (
	stepEnvironmentsParse = iota + 1
	stepEnvironments
	stepCycles
	stepSelect
	stepDiscover
	stepParse
	stepUnion
	stepOverlayParse
	stepOverlayUnion
	stepCheckBase
	stepApply
	stepMerged
	stepSubstitute
)

// kept is one diagnostic with its sort key.
type kept struct {
	d    diag.Diagnostic
	path string
	step int
	item int
	seq  int
}

// keeper keeps the first max diagnostics of a run in the total order of
// compareKept, however they are offered: workers add whole lists, and the
// keeper sorts and drops the tail whenever it holds twice max. The result
// is the same for every worker count and schedule. It is safe for
// concurrent use.
type keeper struct {
	mu   sync.Mutex
	max  int
	list []kept
	// seen counts every diagnostic offered and errs the errors among them.
	seen, errs int
}

// newKeeper returns a keeper for the cap maxDiagnostics: it keeps one more
// than the cap, which tells that the cap was passed. math.MaxInt, the
// usual way to say no cap, counts as math.MaxInt-1, so no count
// overflows.
func newKeeper(maxDiagnostics int) *keeper {
	return &keeper{max: min(maxDiagnostics, math.MaxInt-1) + 1}
}

// trimAt returns the size past which the keeper sorts and drops its tail:
// twice what it keeps, or never for a cap too large to double.
func (k *keeper) trimAt() int {
	if k.max > math.MaxInt/2 {
		return math.MaxInt
	}
	return 2 * k.max
}

// add offers the diagnostics of item i of step s.
func (k *keeper) add(s, i int, ds diag.List) {
	if len(ds) == 0 {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for j := range ds {
		k.seen++
		if ds[j].Severity != diag.SeverityWarning {
			k.errs++
		}
		k.list = append(k.list, kept{d: ds[j], path: ds[j].Path.String(), step: s, item: i, seq: j})
	}
	if len(k.list) > k.trimAt() {
		k.trim()
	}
}

// trim sorts the kept diagnostics and drops all but the first max.
func (k *keeper) trim() {
	slices.SortFunc(k.list, compareKept)
	if len(k.list) > k.max {
		clear(k.list[k.max:])
		k.list = k.list[:k.max]
	}
}

// full reports whether more diagnostics than the cap were offered.
func (k *keeper) full() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.seen >= k.max
}

// hasErrors reports whether an error was offered or the cap was passed,
// which the result reports as an RZ-CFG-001 error.
func (k *keeper) hasErrors() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.errs > 0 || k.seen >= k.max
}

// clone returns a keeper holding what k holds.
func (k *keeper) clone() *keeper {
	k.mu.Lock()
	defer k.mu.Unlock()
	return &keeper{max: k.max, list: slices.Clone(k.list), seen: k.seen, errs: k.errs}
}

// merge offers everything o kept, keeping o's sort keys and counts.
func (k *keeper) merge(o *keeper) {
	o.mu.Lock()
	list, seen, errs := slices.Clone(o.list), o.seen, o.errs
	o.mu.Unlock()
	k.mu.Lock()
	defer k.mu.Unlock()
	k.list = append(k.list, list...)
	k.seen += seen
	k.errs += errs
	k.trim()
}

// result returns the kept diagnostics in diag.List.Sort order: all of them
// below the cap, else the first max-1 and one RZ-CFG-001 "too many
// diagnostics", exactly as a diag.Collector of the cap returns them.
func (k *keeper) result() diag.List {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.trim()
	c := diag.NewCollector(k.max - 1)
	for _, x := range k.list {
		if !c.Add(x.d) {
			break
		}
	}
	return c.List()
}

// compareKept orders diagnostics as diag.List.Sort does, then by step,
// item and position, which makes the order total.
func compareKept(a, b kept) int {
	return cmp.Or(
		cmp.Compare(a.d.Environment, b.d.Environment),
		cmp.Compare(a.d.File, b.d.File),
		cmp.Compare(a.d.Line, b.d.Line),
		cmp.Compare(a.d.Column, b.d.Column),
		cmp.Compare(a.d.Code, b.d.Code),
		cmp.Compare(a.path, b.path),
		cmp.Compare(a.d.Message, b.d.Message),
		cmp.Compare(a.step, b.step),
		cmp.Compare(a.item, b.item),
		cmp.Compare(a.seq, b.seq),
	)
}
