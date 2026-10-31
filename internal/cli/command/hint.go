// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

// nearest returns the candidate closest to s by edit distance when it is
// close enough to be a likely typo (at most 2 edits and fewer than
// len(s)), preferring the first candidate on ties; "" otherwise.
func nearest(s string, candidates []string) string {
	best, bestDist := "", 3
	for _, c := range candidates {
		d := editDistance(s, c)
		if d < bestDist && d < len(s) {
			best, bestDist = c, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between a and b over bytes.
func editDistance(a, b string) int {
	if len(a) > 64 || len(b) > 64 {
		// Long words are never typos of a command or flag name.
		return 64
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
