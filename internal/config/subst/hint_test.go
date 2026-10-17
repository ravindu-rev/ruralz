// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// referenceOSA is the full-table optimal string alignment distance over
// bytes, as diag.Nearest computes it.
func referenceOSA(a, b string) int {
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

func noMeter(int) {}

// TestOSAWithin covers the banded distance of the hint search (review
// finding on the quadratic hint): osaWithin(a, b, k) is min(OSA(a, b),
// k+1) for every pair of strings up to length 5 over a three-letter
// alphabet and every k, so the common prefix and suffix removal, the
// band and the early stop never change the answer.
func TestOSAWithin(t *testing.T) {
	var words []string
	var gen func(prefix string)
	gen = func(prefix string) {
		words = append(words, prefix)
		if len(prefix) == 5 {
			return
		}
		for _, c := range "abc" {
			gen(prefix + string(c))
		}
	}
	gen("")
	for _, a := range words {
		for _, b := range words {
			want := referenceOSA(a, b)
			for k := 0; k <= hintMaxDistance; k++ {
				if got := osaWithin(a, b, k, noMeter); got != min(want, k+1) {
					t.Fatalf("osaWithin(%q, %q, %d) = %d, want %d", a, b, k, got, min(want, k+1))
				}
			}
		}
	}
}

// TestOSAEarlyStop: the distance table stops once two rows exceed the
// limit, so names that differ in their first bytes cost a few rows, not
// one per byte, while TestOSAWithin shows the stop never changes the
// answer.
func TestOSAEarlyStop(t *testing.T) {
	const l = 100_000
	a := "WXYZ" + strings.Repeat("A", l) + "Q"
	b := "BCDE" + strings.Repeat("A", l) + "R"
	for k := 0; k <= hintMaxDistance; k++ {
		read := 0
		if d := osaWithin(a, b, k, func(n int) { read += n }); d != k+1 {
			t.Fatalf("osaWithin(k=%d) = %d, want %d", k, d, k+1)
		}
		if read > 16*(2*hintMaxDistance+1) {
			t.Errorf("k=%d: %d cells read for names that differ in their first bytes, want a few rows", k, read)
		}
	}
}

// TestOSAAffix: the common prefix and suffix are compared at memory
// speed and leave the table only the bytes between, so near-identical
// long names cost about one read of the name, not a row per byte, while
// TestOSAWithin shows the removal never changes the answer.
func TestOSAAffix(t *testing.T) {
	const l = 100_000
	for name, pair := range map[string][2]string{
		"differ at the end":    {strings.Repeat("A", l) + "X", strings.Repeat("A", l) + "Y"},
		"differ at the start":  {"X" + strings.Repeat("A", l), "Y" + strings.Repeat("A", l)},
		"differ in the middle": {strings.Repeat("A", l) + "XY" + strings.Repeat("B", l), strings.Repeat("A", l) + "YX" + strings.Repeat("B", l)},
	} {
		read := 0
		if d := osaWithin(pair[0], pair[1], hintMaxDistance, func(n int) { read += n }); d != 1 {
			t.Errorf("%s: distance %d, want 1", name, d)
		}
		if limit := len(pair[0]) + 64; read > limit {
			t.Errorf("%s: %d bytes and cells read, want at most %d", name, read, limit)
		}
	}
}

// TestCommonAffix compares the chunked prefix and suffix scans with a
// byte loop for every length up to 200 and every position of the first
// mismatch, around the 64-byte chunks.
func TestCommonAffix(t *testing.T) {
	naivePrefix := func(a, b string) int {
		i := 0
		for i < len(a) && i < len(b) && a[i] == b[i] {
			i++
		}
		return i
	}
	naiveSuffix := func(a, b string) int {
		i := 0
		for i < len(a) && i < len(b) && a[len(a)-1-i] == b[len(b)-1-i] {
			i++
		}
		return i
	}
	for n := 0; n <= 200; n++ {
		base := strings.Repeat("A", n)
		for at := 0; at <= n; at++ {
			b := base
			if at < n {
				b = base[:at] + "B" + base[at+1:]
			}
			for _, extra := range []string{"", "AA", "C"} {
				x, y := base+extra, b
				if got, want := commonPrefix(x, y, noMeter), naivePrefix(x, y); got != want {
					t.Fatalf("commonPrefix(len %d, mismatch at %d, %q) = %d, want %d", n, at, extra, got, want)
				}
				x, y = extra+base, b
				if got, want := commonSuffix(x, y, noMeter), naiveSuffix(x, y); got != want {
					t.Fatalf("commonSuffix(len %d, mismatch at %d, %q) = %d, want %d", n, at, extra, got, want)
				}
			}
		}
	}
	a, b := "X"+strings.Repeat("A", 200)+"P", "Y"+strings.Repeat("A", 200)+"Q"
	if d := osaWithin(a, b, hintMaxDistance, noMeter); d != 2 {
		t.Errorf("long names differing at both ends: distance %d, want 2", d)
	}
}

// hintCase is one HintNames list and the names to look up in it.
type hintCase struct {
	name  string
	hints []string
	look  []string
}

func hintCases() []hintCase {
	var many []string
	for i := range 70 {
		many = append(many, "V"+strconv.Itoa(100+i))
	}
	var lengths []string
	for l := 1; l <= 12; l++ {
		lengths = append(lengths, strings.Repeat("B", l))
	}
	return []hintCase{
		{"no names", nil, []string{"A", "ABC"}},
		{"case-insensitive match first", []string{"db_host", "DB_HOSTS", "DB_HOST"}, []string{"DB_HOST", "Db_Host", "db_hosT"}},
		{"Kelvin sign and long s fold to ASCII", []string{"\u212Aey", "\u017Fecret", "kez"}, []string{"KEY", "key", "SECRET", "Secret", "SECRE"}},
		{"a fold match beats a nearer name", []string{"SECREX", "\u017Fecret", "KEX", "\u212Aey"}, []string{"SECRET", "secret", "KEY"}},
		{"invalid bytes and the replacement rune", []string{"A\xffB", "A\uFFFDC", "A\xfeD"}, []string{"AB", "ACX"}},
		{"non-ASCII names lower to other lengths", []string{"\u0130STANBUL", "ISTANBUL_", "\u00C9T\u00C9"}, []string{"ISTANBUL", "ETE", "ISTANBUX"}},
		{"distance limit by length", []string{"AB", "ABCDEF", "ABCDEFGHI", "ABCDEFGHIJKL"}, []string{"AX", "XY", "ABCXYZ", "ABCDEFXYZ", "ABCDXXXXIJKL", "ABCDEFXXXJKL"}},
		{"ties broken by byte order", []string{"ABD", "ABE", "ABC_"}, []string{"ABX", "ABCX"}},
		{"transpositions", []string{"PORT_HTTP", "HOST_NAME"}, []string{"PORT_HTPT", "HSOT_NAME", "OPRT_HTTP"}},
		{"more than 64 length-compatible names", append(slices.Clone(many), "ABCX"), []string{"ABCD", "V1000", "V10"}},
		{"length window of 3 bytes", lengths, []string{"BBBBB", "BBBBBBBBC", "C", "BBBBBBBBBBBBBBBB"}},
		{"common prefix and suffix", []string{"PREFIX_A_SUFFIX", "PREFIX_BC_SUFFIX", "PREFIX__SUFFIX"}, []string{"PREFIX_X_SUFFIX", "PREFIX_CB_SUFFIX", "PREFIXSUFFIX"}},
		{"names that differ at both ends", []string{"XAAAAAAAAY", "ZAAAAAAAAW"}, []string{"YAAAAAAAAX", "XAAAAAAAAW", "QAAAAAAAAQ"}},
		{"no candidate within the limit", []string{"ALPHA", "BRAVO"}, []string{"ZULU_", "OMEGA"}},
		{"empty undefined name", []string{"A", ""}, []string{""}},
	}
}

// TestHintsMatchNearest covers 01 req 27 (review finding on the hint
// cost): the bounded hint search gives exactly diag.Nearest's answer
// over the sorted, deduplicated HintNames, in every case diag.Nearest
// distinguishes (case-insensitive match first, simple folding beyond
// ASCII, invalid bytes, the length-dependent limit, ties, transpositions,
// the 64-name and 3-byte windows), and the memo answers a name the same
// way twice.
func TestHintsMatchNearest(t *testing.T) {
	for _, tc := range hintCases() {
		t.Run(tc.name, func(t *testing.T) {
			names := slices.Compact(slices.Sorted(slices.Values(tc.hints)))
			// The hint search sorts and deduplicates the names itself.
			h := newHints(append(reversed(tc.hints), tc.hints...), 100)
			for _, name := range tc.look {
				for round := range 2 {
					got, ok := h.nearest(name, noMeter)
					want, wok := diag.Nearest(name, names)
					if !wok {
						want = ""
					}
					if got != want || ok != wok {
						t.Errorf("round %d: nearest(%q) = %q, %v; diag.Nearest gives %q, %v", round, name, got, ok, want, wok)
					}
				}
			}
		})
	}
}

// FuzzHints is the differential property of the hint search: for any
// variable name of the 01 req 26 grammar and any HintNames, hints gives
// diag.Nearest's answer.
func FuzzHints(f *testing.F) {
	for _, tc := range hintCases() {
		for _, name := range tc.look {
			f.Add(name, strings.Join(tc.hints, ","))
		}
	}
	f.Fuzz(func(t *testing.T, name, list string) {
		if len(name)+len(list) > 4096 || nameLen(name) != len(name) {
			return
		}
		raw := strings.Split(list, ",")
		names := slices.Compact(slices.Sorted(slices.Values(raw)))
		got, ok := newHints(raw, 4).nearest(name, noMeter)
		want, wok := diag.Nearest(name, names)
		if !wok {
			want = ""
		}
		if got != want || ok != wok {
			t.Fatalf("nearest(%q) over %q = %q, %v; diag.Nearest gives %q, %v", name, names, got, ok, want, wok)
		}
	})
}

// endNames returns the 64 names diag.Nearest compares, of length l, that
// each differ from "C"+"A"*(l-2)+"C" in both end bytes, so the distance
// table runs over the whole name: nothing common to remove, no early stop
// before the end.
func endNames(l int) []string {
	out := make([]string, hintCompared)
	for i := range out {
		c := string(rune('D' + i%20))
		out[i] = c + strings.Repeat("A", l-2-i/20) + c + strings.Repeat("Q", i/20)
	}
	return out
}

// prefixNames returns k distinct names of length l that share their
// first l-6 bytes with "A"*l: a case-insensitive comparison of each with
// an undefined "A"*l reads l-6 bytes before it differs.
func prefixNames(k, l int) []string {
	out := make([]string, k)
	for i := range out {
		out[i] = strings.Repeat("A", l-6) + "B" + fmt4(i) + "B"
	}
	return out
}

// TestHintCost covers the review finding on the hint cost (01 req 27, 54
// and risk 1): the hint of a long undefined name, compared against long
// near names, allocates a bounded number of objects (diag.Nearest
// allocates one table row per byte of the name, at every occurrence), and
// its work is linear in the name: 64 names times the 7-cell band per
// byte at most, plus one read of the name for the case-insensitive match
// however many names share a long prefix with it (diag.Nearest's
// strings.EqualFold pass reads every name). The work units also bound
// repeated uses: each later use of a name costs a lookup, not a new
// search.
func TestHintCost(t *testing.T) {
	ends := "C" + strings.Repeat("A", 1998) + "C"
	for _, tc := range []struct {
		label, name string
		hints       []string
		uses        int
	}{
		{"near the last byte", ends, nearNames(2000), 1},
		{"differing at both ends", ends, endNames(2000), 1},
		{"differing at both ends, used 50 times", ends, endNames(2000), 50},
		{"longer than every name", ends, []string{"A", "B"}, 1},
		{"20000 names sharing a long prefix", strings.Repeat("A", 200), prefixNames(20_000, 200), 1},
		{"20000 shorter names sharing a long prefix", strings.Repeat("A", 2000), prefixNames(20_000, 200), 1},
	} {
		t.Run(tc.label, func(t *testing.T) {
			l := len(tc.name)
			src := labelsRoute(tc.uses, keyN, func(int) string { return "${" + tc.name + "}" })
			s := New(Options{Schemas: schemas(t), HintNames: tc.hints})
			rs := []*tree.Resource{docs(t, &tree.FileTable{}, "r.yaml", src)[0], docs(t, &tree.FileTable{}, "r.yaml", src)[0]}
			i := 0
			allocs := testing.AllocsPerRun(1, func() {
				ds := s.Apply(rs[i])
				i++
				if len(ds) != tc.uses {
					t.Fatalf("%d diagnostics, want %d", len(ds), tc.uses)
				}
			})
			if limit := float64(64 + 16*tc.uses); allocs > limit {
				t.Errorf("%.0f allocations for %d uses of a %d-byte name, want at most %.0f", allocs, tc.uses, l, limit)
			}
			calls := 0
			w := &walker{s: New(Options{Schemas: schemas(t), HintNames: tc.hints}), yield: func() { calls++ }}
			// The index over the names is built once per Substituter and
			// counted to the first use (TestHintIndexSpread): build it first.
			w.s.hints.index(noMeter)
			for range tc.uses {
				w.undefined(&tree.Node{}, tc.name)
			}
			units := calls*yieldEvery + w.units
			if limit := (64*7*l+16*l*tc.uses)/byteUnit + 4; units > limit {
				t.Errorf("%d work units for %d uses, want at most %d", units, tc.uses, limit)
			}
		})
	}
}

// TestHintMemo: a name is searched once per Substituter, whichever
// resource and worker uses it, and the memo keeps at most memoMax names
// while answers beyond it stay exact.
func TestHintMemo(t *testing.T) {
	const l = 400
	name := "C" + strings.Repeat("A", l-2) + "C"
	h := newHints(endNames(l), 2)
	cost := func(name string) int {
		n := 0
		_, _ = h.nearest(name, func(k int) { n += k })
		return n
	}
	first, second := cost(name), cost(name)
	if first < 64*l || second > 2*l {
		t.Errorf("searching %d bytes, then %d bytes for the same name: want a search (at least %d) and then a lookup (at most %d)", first, second, 64*l, 2*l)
	}
	other := "D" + strings.Repeat("A", l-2) + "E"
	third := "E" + strings.Repeat("A", l-2) + "F"
	_ = cost(other)
	full := cost(third)
	if len(h.memo) != 2 {
		t.Errorf("memo holds %d names, want memoMax = 2", len(h.memo))
	}
	if again := cost(third); again != full || again <= 2*l {
		t.Errorf("a name past memoMax cost %d, then %d: want the same search again, not a lookup", full, again)
	}
	want, wok := diag.Nearest(third, h.index(noMeter).names)
	if got, ok := h.nearest(third, noMeter); got != want || ok != wok {
		t.Errorf("a name past memoMax: nearest = %q, %v; diag.Nearest gives %q, %v", got, ok, want, wok)
	}
}
