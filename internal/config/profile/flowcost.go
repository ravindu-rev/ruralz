// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import "github.com/ravindu-rev/ruralz/internal/config/tree"

// Bounds of goccy's parse of one flow collection (01 req 9; 11 req 17,
// 26; 01 test plan FuzzProfileYAML). Every block collection is parsed
// entry by entry (split.go), so goccy parses a flow collection, with what
// it holds, as one value on its own. These bounds, and the depth the token
// pass counts, hold only while the token pass predicts goccy's nesting and
// paths exactly: one level per flow collection, one path per entry and at
// most one inserted null per entry, which the entry shape check guarantees
// (flowentry.go). The converter's depth bound (converter.enter) bounds the
// tree goccy's parse returns, never the cost of that parse.
const (
	// maxFlowPath bounds the path bytes the token pass charges for one
	// flow collection (flowCost). goccy's parser builds every node's path,
	// "$.key[3].key", by concatenating its parent's, so long keys above
	// many entries cost memory quadratic in the input: a 1,000-byte key at
	// each of 62 levels above 4,096 entries took 263 MiB. The charge
	// over-counts goccy's paths, which therefore stay below this bound;
	// ordinary flow collections charge tens of bytes per token.
	maxFlowPath = 32 << 20
	// maxFlowNullShift bounds the token moves goccy's parser makes for the
	// entries of one flow collection's mappings that have no value and for
	// its scalar tags directly before ',' or ':'. goccy inserts the
	// implicit null of such an entry, or the tag's default value, into its
	// token slice, moving every token after it, so 80,000 such entries
	// ("{a, a, ...}") took 4 s, 160,000 took 35 s, and 99,000 tagged empty
	// nodes ("[!!str , ...]") took 10 s. 2^26 moves take a few tens of
	// milliseconds, about 11,000 such entries in one collection.
	maxFlowNullShift = 1 << 26
)

// Messages of the flow collection bounds.
const (
	msgFlowPath  = "flow collection nests too many long keys to parse within its memory bound; write its outer levels in block style"
	msgFlowNulls = "flow collection holds too many mapping entries without a value or empty tagged nodes to parse within its time bound; give them values or write them in block style"
)

// flowCost is the state of one open flow collection that the token pass
// keeps to bound goccy's parse of it: the length of its path and of its
// current entry's path segment, and how its current entry ends.
type flowCost struct {
	seq   bool // a sequence ('['); otherwise a mapping
	base  int  // path bytes of the collection node
	seg   int  // path bytes the current entry adds: "[i]", ".key", or both for a pair
	index int  // the current entry's index (sequence)
	key   int  // value bytes of the last scalar directly in the collection
	entry bool // the current entry holds a node
	colon bool // the current entry has its ':'
	empty bool // the last token directly in the collection was that ':'
}

// openFlow notes a '[' (seq) or '{' at at, before its frame is pushed, and
// returns its cost record: the outermost collection of a value starts a
// new path at "$", as goccy's parse of that value does, and a nested one
// extends the path of its parent's current entry.
func (t *tokenPass) openFlow(seq bool, at tree.Pos) flowCost {
	base := 1
	if t.flow > 0 {
		top := &t.stack[len(t.stack)-1].cost
		base = top.base + top.seg
		top.entry, top.empty = true, false
	} else {
		t.pathCost, t.nulls, t.nullSum, t.flowAt = 0, 0, 0, at
	}
	c := flowCost{seq: seq, base: base}
	if seq {
		c.seg = digitsOf(0) + 2
	}
	return c
}

// chargePath charges a token at at inside a flow collection with the path
// goccy gives the node it belongs to: at least one path per entry, and
// each entry has a token. Past maxFlowPath the file stops with RZ-CFG-001.
func (t *tokenPass) chargePath(at tree.Pos) {
	top := t.stack[len(t.stack)-1].cost
	t.pathCost += top.base + top.seg
	if t.pathCost > maxFlowPath && t.errs == 0 {
		t.fail(at, msgFlowPath)
	}
}

// flowNode notes a node token directly in the innermost flow collection: a
// scalar of n value bytes (n < 0 for a property, which starts a node).
func (t *tokenPass) flowNode(n int) {
	top := &t.stack[len(t.stack)-1].cost
	top.entry, top.empty = true, false
	if n >= 0 {
		top.key = n
	}
}

// flowColon notes a ':' directly in the innermost flow collection: the
// scalar before it is a key, whose ".key" extends the entry's path (in a
// sequence, the key of a single-pair mapping).
func (t *tokenPass) flowColon() {
	t.stack[len(t.stack)-1].key.open = false
	top := &t.stack[len(t.stack)-1].cost
	if !top.colon {
		if top.seq {
			top.seg += top.key + 3
		} else {
			top.seg = top.key + 3
		}
	}
	top.colon, top.empty = true, true
}

// flowEntryEnd notes the ',' or closing bracket at token idx that ends the
// current entry of the innermost flow collection. A mapping entry with no
// ':' or nothing after it gets goccy's inserted null, and so does a single
// pair of a sequence with nothing after its ':' ("[k:\n, ...]") or an
// explicit key with no ':' ("[? k, ...]"): 40,000 of those took 0.5 s. A
// ',' starts the next entry.
func (t *tokenPass) flowEntryEnd(idx int, next bool) {
	f := &t.stack[len(t.stack)-1]
	f.key.open = false
	top := &f.cost
	if (!top.seq && top.entry && !top.colon) || (top.colon && top.empty) || (top.seq && f.shape.explicit && !top.colon) {
		t.nulls++
		t.nullSum += idx
	}
	if !next {
		return
	}
	top.entry, top.colon, top.empty = false, false, false
	if top.seq {
		top.index++
		top.seg = digitsOf(top.index) + 2
	} else {
		top.seg = 0
	}
}

// closeFlow checks, at the closing bracket idx of the outermost flow
// collection, the token moves goccy makes for its inserted nulls: each
// moves the tokens after it in the collection.
func (t *tokenPass) closeFlow(idx int) {
	if t.nulls > 0 && t.nulls*idx-t.nullSum > maxFlowNullShift && t.errs == 0 {
		t.fail(t.flowAt, msgFlowNulls)
	}
}

// digitsOf returns the number of decimal digits of n >= 0.
func digitsOf(n int) int {
	d := 1
	for ; n >= 10; n /= 10 {
		d++
	}
	return d
}
