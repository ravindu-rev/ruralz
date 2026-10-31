// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"math"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Budget is a node budget that the files of one load share, so the trees
// of all of them together hold at most its limit, in whatever order and on
// however many goroutines they are parsed. Every node either front end
// builds takes one unit: a scalar, key, collection or empty node of a YAML
// document, a value of a JSON file. The node past the limit stops its file
// with RZ-CFG-001 at that node, worded by the Budget's message, and every
// other file parsed with the Budget stops the same way at its next node.
// With a Budget, the per-file bound of five quarters of MaxDocumentTokens
// gives way to it; MaxDocumentTokens still bounds each YAML document.
//
// Which file a shared Budget stops first depends on the schedule. A caller
// that needs a deterministic finding parses again in a fixed order, each
// file with a Budget of what the files before it left (Taken).
type Budget struct {
	limit int64
	msg   string
	taken atomic.Int64
}

// NewBudget returns a Budget of limit nodes whose RZ-CFG-001 reads msg. A
// limit of 0 or below admits no node, and math.MaxInt is no practical
// limit.
func NewBudget(limit int, msg string) *Budget {
	return &Budget{limit: int64(max(0, limit)), msg: msg}
}

// take takes one node and reports whether it fits the limit. Once a node
// does not, no later node fits.
func (b *Budget) take() bool {
	if b.taken.Load() > b.limit {
		return false
	}
	return b.taken.Add(1) <= b.limit
}

// Taken returns the nodes the Budget admitted: every node built when it
// was not exceeded.
func (b *Budget) Taken() int {
	return int(min(b.taken.Load(), b.limit, math.MaxInt))
}

// Exceeded reports whether the Budget refused a node, which stopped the
// file that built it.
func (b *Budget) Exceeded() bool { return b.taken.Load() > b.limit }

// budgetError stops a JSON file at the value the Budget refused.
type budgetError struct{ at tree.Pos }

func (*budgetError) Error() string { return "the node budget is exhausted" }
