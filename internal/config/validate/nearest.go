// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import "strings"

// nearest returns what diag.Nearest returns for input and candidates (an
// exact case-insensitive match first, else the smallest optimal string
// alignment distance of at most max(1, min(3, len(input)/3)), ties broken
// by byte order; candidates whose length differs by more than 3 skipped;
// at most 64 compared), without allocating: a distance is only needed up
// to that limit, so it is computed in a band around the diagonal, which
// every alignment within the limit stays in. A Bundle with thousands of
// missing references thus pays for its hints in time linear in them.
// TestNearestMatchesDiag holds the two equal.
func nearest(input string, candidates []string) (string, bool) {
	for _, c := range candidates {
		if strings.EqualFold(c, input) {
			return c, true
		}
	}
	limit := max(1, min(3, len(input)/3))
	lowIn := strings.ToLower(input)
	best, bestD, compared := "", limit+1, 0
	for _, c := range candidates {
		if abs(len(c)-len(input)) > 3 {
			continue
		}
		if compared++; compared > maxCandidates {
			break
		}
		d := osaWithin(lowIn, strings.ToLower(c), limit)
		if d < bestD || (d == bestD && c < best) {
			best, bestD = c, d
		}
	}
	return best, bestD <= limit
}

// abs returns |x|.
func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// bandRows is the longest second string osaWithin keeps on the stack.
const bandRows = 2*maxHintBytes + 8

// osaWithin returns the optimal string alignment distance of a and b over
// bytes when it is at most limit, else limit+1. An alignment within limit
// never strays more than limit cells from the diagonal, so row i computes
// only the cells j within limit of i, plus one cell on each side holding
// limit+1 (or the first column's i), which is all the next two rows read.
// The three rows live on the stack for strings a hint compares.
func osaWithin(a, b string, limit int) int {
	over := limit + 1
	if abs(len(a)-len(b)) > limit {
		return over
	}
	var buf [3][bandRows]int
	var rows [3][]int
	for k := range rows {
		if len(b)+1 <= bandRows {
			rows[k] = buf[k][:len(b)+1]
		} else {
			rows[k] = make([]int, len(b)+1)
		}
	}
	prev2, prev, cur := rows[0], rows[1], rows[2]
	for j := range prev {
		prev[j] = min(j, over)
	}
	for i := 1; i <= len(a); i++ {
		lo, hi := max(1, i-limit), min(len(b), i+limit)
		// The cell left of the band: the first column, or out of reach.
		if lo-1 == 0 {
			cur[0] = min(i, over)
		} else {
			cur[lo-1] = over
		}
		for j := lo; j <= hi; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d := min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d = min(d, prev2[j-2]+1)
			}
			cur[j] = min(d, over)
		}
		// The cell right of the band, which the next row reads above it.
		if hi+1 <= len(b) {
			cur[hi+1] = over
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return min(prev[len(b)], over)
}
