// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"slices"
	"strings"
	"sync"
	"unicode"
)

// Limits of the nearest-name search, as diag.Nearest applies them.
const (
	// hintLengthSlack is the most bytes a compared name's length may differ
	// from the undefined name's.
	hintLengthSlack = 3
	// hintCompared is the most length-compatible names compared.
	hintCompared = 64
	// hintMaxDistance is the largest distance a hint may have.
	hintMaxDistance = 3
	// hintRowsPerCharge is how many rows of the distance table the search
	// fills between two reports to the meter: at most textStep cells.
	hintRowsPerCharge = textStep / (2*hintMaxDistance + 1)
)

// hints computes the RZ-CFG-010 hint of 01 req 27: the HintNames entry
// nearest an undefined variable name. Its answer is exactly
// diag.Nearest(name, names) over the sorted, deduplicated names
// (TestHintsMatchNearest, FuzzHints), at a bounded cost (review finding:
// diag.Nearest allocates a table of (len(a)+1)*(len(b)+1) ints per
// compared name and reads every candidate in its case-insensitive pass,
// so a long undefined name cost time and memory quadratic in its length,
// once per occurrence):
//
//   - a name longer than the longest entry plus hintLengthSlack bytes has
//     no hint and costs nothing: a case-insensitive match is never shorter
//     in bytes than a name of ASCII letters, digits and underscores, which
//     the grammar of 01 req 26 admits only, and no entry is close enough
//     in length to be compared;
//   - the case-insensitive match is one lookup of the name's simple-fold
//     key, the relation strings.EqualFold tests;
//   - the optimal string alignment distance is computed with the common
//     prefix and suffix removed, in a band of the distance limit around
//     the diagonal and with three rows of at most 7 cells, and stops once
//     two consecutive rows exceed the best distance found so far:
//     O(len(name)) time per compared entry and O(1) memory;
//   - each distinct name is computed once per Substituter, safely for
//     concurrent workers; the memo keeps at most memoMax names.
//
// The index over the names, which sorts them and removes repeated ones,
// is built on the first undefined variable, so a render without one pays
// nothing, and its work is counted to that variable's walk as it is done
// (01 req 53). Every pass over a name reads it in windows reported to the
// meter. A contract change request asks diag.Nearest (WP-01) to adopt the
// same bounds.
type hints struct {
	// names are Options.HintNames as given: in any order, with repeats.
	names   []string
	memoMax int

	once sync.Once
	idx  hintIndex

	mu   sync.Mutex
	memo map[string]hintResult
}

// hintIndex is the index over hints.names.
type hintIndex struct {
	// names are hints.names sorted, without repeats.
	names []string
	// exact maps a fold key to the index of the first name holding it.
	exact map[string]int
	// lower holds strings.ToLower of each name, which diag.Nearest
	// compares.
	lower []string
	// byLen lists the indices of the names of each byte length, ascending.
	byLen map[int][]int
	// maxLen is the length of the longest name.
	maxLen int
}

// hintResult is one memoized answer.
type hintResult struct {
	name string
	ok   bool
}

// newHints returns the hint search over names, in any order; it reads
// them only once an undefined variable needs a hint, so the caller must
// not change them meanwhile.
func newHints(names []string, memoMax int) *hints {
	return &hints{names: names, memoMax: memoMax}
}

// index returns the index over the names, building it on the first call,
// whose meter receives the bytes the build reads, copies and compares.
func (h *hints) index(meter func(n int)) *hintIndex {
	h.once.Do(func() {
		names := make([]string, 0, len(h.names))
		for _, n := range h.names {
			meter(byteUnit)
			names = append(names, n)
		}
		names = sortStrings(names, meter)
		x := hintIndex{
			names: names,
			exact: map[string]int{},
			lower: make([]string, len(names)),
			byLen: map[int][]int{},
		}
		for i, n := range names {
			key := foldKey(n, meter)
			if _, dup := x.exact[key]; !dup {
				x.exact[key] = i
			}
			x.lower[i] = lower(n, meter)
			x.byLen[len(n)] = append(x.byLen[len(n)], i)
			x.maxLen = max(x.maxLen, len(n))
		}
		h.idx = x
	})
	return &h.idx
}

// nearest returns the hint for name, a variable name of the 01 req 26
// grammar (ASCII letters, digits and underscores), and whether there is
// one, reporting the bytes and table cells it reads to meter.
func (h *hints) nearest(name string, meter func(n int)) (string, bool) {
	if len(h.names) == 0 {
		return "", false
	}
	x := h.index(meter)
	if len(name) > x.maxLen+hintLengthSlack {
		return "", false
	}
	h.mu.Lock()
	r, hit := h.memo[name]
	h.mu.Unlock()
	if hit {
		return r.name, r.ok
	}
	r = h.compute(x, name, meter)
	h.mu.Lock()
	if h.memo == nil {
		h.memo = map[string]hintResult{}
	}
	if len(h.memo) < h.memoMax {
		h.memo[name] = r
	}
	h.mu.Unlock()
	return r.name, r.ok
}

// compute is diag.Nearest(name, x.names) over the index x.
func (h *hints) compute(x *hintIndex, name string, meter func(n int)) hintResult {
	if i, ok := x.exact[foldKey(name, meter)]; ok {
		return hintResult{name: x.names[i], ok: true}
	}
	limit := max(1, min(hintMaxDistance, len(name)/3))
	low := lower(name, meter)
	best, bestD := -1, limit+1
	for _, i := range x.window(len(name)) {
		if bestD == 0 {
			break
		}
		// Later names sort after the best, so only a smaller distance
		// replaces it, as diag.Nearest breaks ties by byte order.
		if d := osaWithin(low, x.lower[i], bestD-1, meter); d < bestD {
			best, bestD = i, d
		}
	}
	if best < 0 {
		return hintResult{}
	}
	return hintResult{name: x.names[best], ok: true}
}

// window returns the indices, ascending, of the first hintCompared names
// whose length differs from n by at most hintLengthSlack bytes: the names
// diag.Nearest compares.
func (x *hintIndex) window(n int) []int {
	var out []int
	// bounded: 2*hintLengthSlack+1 lengths.
	for l := max(0, n-hintLengthSlack); l <= n+hintLengthSlack; l++ {
		ids := x.byLen[l]
		out = append(out, ids[:min(len(ids), hintCompared)]...)
	}
	slices.Sort(out)
	return out[:min(len(out), hintCompared)]
}

// foldKey returns s with each rune replaced by the smallest rune of its
// simple case folding orbit, so foldKey(a) == foldKey(b) exactly when
// strings.EqualFold(a, b): EqualFold compares rune by rune, an invalid
// byte reading as utf8.RuneError in both, and two runes match when one
// is in the other's unicode.SimpleFold orbit. It reports the bytes it
// reads to meter every textStep bytes.
func foldKey(s string, meter func(n int)) string {
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	// counted: the runes are reported every textStep bytes.
	for i, r := range s {
		if i-last >= textStep {
			meter(i - last)
			last = i
		}
		m := r
		// bounded: a simple folding orbit holds at most a few runes.
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			m = min(m, f)
		}
		b.WriteRune(m)
	}
	meter(len(s) - last)
	return b.String()
}

// osaWithin returns the optimal string alignment distance of a and b over
// bytes, as diag.Nearest computes it, when it is at most k (0 to
// hintMaxDistance), and k+1 otherwise. It removes the common prefix and
// suffix, which leaves the distance unchanged, fills only the cells within
// k of the diagonal (a cell farther off holds more than k) and stops once
// two consecutive rows exceed k, after which every later cell does too.
func osaWithin(a, b string, k int, meter func(n int)) int {
	p := commonPrefix(a, b, meter)
	a, b = a[p:], b[p:]
	q := commonSuffix(a, b, meter)
	a, b = a[:len(a)-q], b[:len(b)-q]
	if abs(len(a)-len(b)) > k {
		return k + 1
	}
	if len(a) == 0 || len(b) == 0 {
		return max(len(a), len(b))
	}
	const width = 2*hintMaxDistance + 1
	inf := k + 1
	// Row i holds d[i][j] at index j-i+k, for j within k of i.
	var prev2, prev, cur [width]int
	// bounded: the cells of one row of the band.
	for x := range prev {
		prev[x], prev2[x] = inf, inf
	}
	// bounded: at most k+1 cells.
	for j := 0; j <= min(len(b), k); j++ {
		prev[j+k] = j
	}
	prevMin := 0
	// counted: the rows are reported every hintRowsPerCharge rows.
	for i := 1; i <= len(a); i++ {
		// bounded: the cells of one row of the band.
		for x := range cur {
			cur[x] = inf
		}
		rowMin := inf
		// left is d[i][j-1], the cell before j in this row: inf before the
		// band.
		left := inf
		// bounded: the 2k+1 cells of the band.
		for j := max(0, i-k); j <= min(len(b), i+k); j++ {
			x := j - i + k
			v := i
			if j > 0 {
				cost := 1
				if a[i-1] == b[j-1] {
					cost = 0
				}
				v = min(prev[x]+cost, left+1)
				if x+1 < width {
					v = min(v, prev[x+1]+1)
				}
				if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
					v = min(v, prev2[x]+1)
				}
			}
			cur[x] = min(v, inf)
			left = cur[x]
			rowMin = min(rowMin, cur[x])
		}
		if i%hintRowsPerCharge == 0 {
			meter(hintRowsPerCharge * width)
		}
		if rowMin > k && prevMin > k {
			meter((i % hintRowsPerCharge) * width)
			return inf
		}
		prev2, prev, prevMin = prev, cur, rowMin
	}
	meter((len(a) % hintRowsPerCharge) * width)
	return prev[len(b)-len(a)+k]
}

// commonPrefix returns the length of the longest common prefix of a and
// b, comparing 64 bytes at a time and reporting the bytes to meter every
// textStep bytes.
func commonPrefix(a, b string, meter func(n int)) int {
	n := min(len(a), len(b))
	i, reported := 0, 0
	// counted: the blocks are reported every textStep bytes.
	for i+64 <= n && a[i:i+64] == b[i:i+64] {
		if i += 64; i-reported >= textStep {
			meter(i - reported)
			reported = i
		}
	}
	// bounded: fewer than 64 bytes past the last compared block.
	for i < n && a[i] == b[i] {
		i++
	}
	meter(i - reported)
	return i
}

// commonSuffix returns the length of the longest common suffix of a and
// b, comparing 64 bytes at a time and reporting the bytes to meter every
// textStep bytes.
func commonSuffix(a, b string, meter func(n int)) int {
	n := min(len(a), len(b))
	i, reported := 0, 0
	// counted: the blocks are reported every textStep bytes.
	for i+64 <= n && a[len(a)-i-64:len(a)-i] == b[len(b)-i-64:len(b)-i] {
		if i += 64; i-reported >= textStep {
			meter(i - reported)
			reported = i
		}
	}
	// bounded: fewer than 64 bytes past the last compared block.
	for i < n && a[len(a)-i-1] == b[len(b)-i-1] {
		i++
	}
	meter(i - reported)
	return i
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
