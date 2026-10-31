// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// osaFull is the full-matrix optimal string alignment distance, the
// oracle of osaWithin.
func osaFull(a, b string) int {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}

// randomName returns a short string over a small alphabet, so generated
// pairs often lie within a few edits of each other.
func randomName(rng *rand.Rand, maxLen int) string {
	const alphabet = "abcAB-"
	n := rng.IntN(maxLen + 1)
	var b strings.Builder
	for range n {
		b.WriteByte(alphabet[rng.IntN(len(alphabet))])
	}
	return b.String()
}

// TestOSAWithinMatchesFull: the banded distance equals the full one when
// that is within the limit, and is limit+1 otherwise, for every limit a
// hint uses.
func TestOSAWithinMatchesFull(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6)) //nolint:gosec // G404: a fixed seed makes the generated pairs the same on every run.
	for range 20_000 {
		a, b := randomName(rng, 9), randomName(rng, 9)
		full := osaFull(a, b)
		for limit := 1; limit <= 3; limit++ {
			want := min(full, limit+1)
			if got := osaWithin(a, b, limit); got != want {
				t.Fatalf("osaWithin(%q, %q, %d) = %d, want %d (full %d)", a, b, limit, got, want, full)
			}
		}
	}
	long := strings.Repeat("ab", bandRows)
	if got := osaWithin(long, long[1:]+"a", 3); got != min(osaFull(long, long[1:]+"a"), 4) {
		t.Errorf("osaWithin on strings past the stack rows = %d", got)
	}
}

// TestNearestMatchesDiag: nearest picks what diag.Nearest picks for every
// generated input and candidate list, ties and the 64-candidate cap
// included, so stage H hints read like every other stage's.
func TestNearestMatchesDiag(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8)) //nolint:gosec // G404: a fixed seed makes the generated lists the same on every run.
	for range 5_000 {
		input := randomName(rng, 10)
		cands := make([]string, rng.IntN(90))
		for i := range cands {
			cands[i] = randomName(rng, 12)
		}
		gotName, gotOK := nearest(input, cands)
		wantName, wantOK := diag.Nearest(input, cands)
		if gotName != wantName || gotOK != wantOK {
			t.Fatalf("nearest(%q, %q) = %q, %v; diag.Nearest = %q, %v", input, cands, gotName, gotOK, wantName, wantOK)
		}
	}
}

// TestHintAllocates: a hint over a large name set allocates only its
// candidate list and its message.
func TestHintAllocates(t *testing.T) {
	names := make([]string, 10_000)
	for i := range names {
		names[i] = "upstream-" + strings.Repeat("x", 30) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676%26))
	}
	s := newNameSet(names)
	input := "upstream-" + strings.Repeat("x", 30) + "zzq"
	if n := testing.AllocsPerRun(20, func() { _ = s.hint(input) }); n > 4 {
		t.Errorf("a hint allocates %.0f times", n)
	}
}
