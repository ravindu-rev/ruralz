// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"cmp"
	"math"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// Stages of a run, in run order. A diagnostic's stage, item and position
// break the ties of the diag.List.Sort key, so the kept set is one total
// order whatever the worker count.
const (
	stageLoad = iota + 1
	stageF
	stageG
	stageH
	stageI
	stageJ
	stageK
	stageL
	stageOnline
)

// kept is one diagnostic with its sort key.
type kept struct {
	d     diag.Diagnostic
	path  string
	stage int
	item  int
	seq   int
}

// keeper keeps the first diagnostics of a run in the total order of
// compareKept: one more than the cap, which tells that the cap was passed.
// It sorts and drops its tail whenever it holds twice that, so a run holds
// a bounded number of diagnostics however many the stages report. A
// keeper belongs to the goroutine of its run: the stages hand their
// findings over once they return.
type keeper struct {
	max  int
	list []kept
	// seen counts every diagnostic offered and errs the errors among them.
	seen, errs int
}

// newKeeper returns a keeper for the cap maxDiagnostics. math.MaxInt, the
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

// add offers the diagnostics of item i of stage s.
func (k *keeper) add(s, i int, ds diag.List) {
	for j := range ds {
		k.seen++
		if ds[j].Severity != diag.SeverityWarning {
			k.errs++
		}
		k.list = append(k.list, kept{d: ds[j], path: ds[j].Path.String(), stage: s, item: i, seq: j})
		if len(k.list) > k.trimAt() {
			k.trim()
		}
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

// full reports whether more diagnostics than the cap were offered: the
// cap stops the run (docs/architecture/02-configuration-model.md
// "Validation and diff semantics").
func (k *keeper) full() bool { return k.seen >= k.max }

// hasErrors reports whether an error was offered or the cap was passed,
// which the result reports as an RZ-CFG-001 error. A run with an error
// has no Revision.
func (k *keeper) hasErrors() bool { return k.errs > 0 || k.full() }

// result returns the kept diagnostics in diag.List.Sort order: all of them
// below the cap, else the first max-1 and one RZ-CFG-001 "too many
// diagnostics", exactly as a diag.Collector of the cap returns them.
func (k *keeper) result() diag.List {
	k.trim()
	c := diag.NewCollector(k.max - 1)
	for _, x := range k.list {
		if !c.Add(x.d) {
			break
		}
	}
	return c.List()
}

// compareKept orders diagnostics as diag.List.Sort does, then by stage,
// item and position, which makes the order total.
func compareKept(a, b kept) int {
	return cmp.Or(
		cmp.Compare(a.d.Environment, b.d.Environment),
		cmp.Compare(a.d.File, b.d.File),
		cmp.Compare(a.d.Line, b.d.Line),
		cmp.Compare(a.d.Column, b.d.Column),
		cmp.Compare(a.d.Code, b.d.Code),
		strings.Compare(a.path, b.path),
		cmp.Compare(a.d.Message, b.d.Message),
		cmp.Compare(a.stage, b.stage),
		cmp.Compare(a.item, b.item),
		cmp.Compare(a.seq, b.seq),
	)
}
