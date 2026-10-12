// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"math/bits"
	"math/rand/v2"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
)

// costPatterns are patterns with an input that keeps their run queue
// large: small and anchored, unanchored, with alternations and
// repetitions, case-folded, Unicode classes, and large programs.
func costPatterns() []struct{ pattern, input string } {
	return []struct{ pattern, input string }{
		{`^a`, "a"},
		{`b`, "a"},
		{`a$`, "a"},
		{`^[a-z0-9]+$`, "a"},
		{`^(a|bb|ccc)+$`, "a"},
		{`.*b`, "a"},
		{`(a+a+)+b`, "a"},
		{`a{100}b`, "a"},
		{`(a|aa){50}b`, "a"},
		{`[a-z]{300}x`, "a"},
		{emailPattern, "a@a"},
		{`\pL+x`, "a"},
		{`[\p{Greek}\p{Han}]+x`, "α"},
		{`(?i)abcdefghij+x`, "a"},
		{`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`, "0"},
		{`(x|y|z|w|v)*(xx|yy|zz)*q`, "x"},
		{`(?m)^a$`, "a"},
		{`(?s).*`, "a"},
		{`(?i)k+x`, "K"},
		// Classes of many ranges, which every queue entry searches.
		{`\pL{200}x`, "a"},
		{wideClass(4096) + "{200}x", "a"},
	}
}

// wideClass returns a class of n ranges that matches a: a and n-1
// distinct runes from U+0102 on.
func wideClass(n int) string {
	var b strings.Builder
	b.WriteString("[a")
	for i := 1; i < n; i++ {
		b.WriteRune(rune(0x100 + 2*i))
	}
	b.WriteByte(']')
	return b.String()
}

// program compiles expr as Go's regexp does.
func program(t testing.TB, expr string) *syntax.Prog {
	t.Helper()
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		t.Fatal(err)
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		t.Fatal(err)
	}
	return prog
}

// TestQueueBound covers the run-queue bound of a pattern (proposed,
// package documentation "Work bound"): deterministic patterns, even large
// ones such as the e-mail pattern, keep a small queue, a repetition of a
// class keeps one entry per position, and past its budget the bound is
// the whole program.
//
// Requirements: 07 section 9 item 21.
func TestQueueBound(t *testing.T) {
	for _, tc := range []struct {
		pattern  string
		min, max int
	}{
		{`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`, 1, 4},
		{emailPattern, 1, 16},
		{`^a`, 1, 4},
		{`[a-z]{300}x`, 300, 1000},
	} {
		budget := patternBudget
		prog := program(t, tc.pattern)
		if q := queueBound(prog, &budget); q < tc.min || q > tc.max {
			t.Errorf("queueBound(%.30q) = %d of %d instructions, want %d to %d", tc.pattern, q, len(prog.Inst), tc.min, tc.max)
		}
	}
	prog := program(t, noOnePass+emailPattern)
	none := 0
	if q := queueBound(prog, &none); q != len(prog.Inst) {
		t.Errorf("queueBound without budget = %d, want the program's %d", q, len(prog.Inst))
	}
	pc := newPatternCosts()
	pc.budget = 0
	n := len(prog.Inst)
	if c := pc.of(emailPattern); c.perByte != byteUnits+(queueUnits+searchUnits*classSteps(prog))*n+n/clearInstsPerUnit || c.insts != n {
		t.Errorf("cost past the document budget = %+v, want the whole program per byte", c)
	}
	if c := newPatternCosts().of(`(`); c.perByte != countCap {
		t.Errorf("cost of an invalid expression = %+v, want the largest", c)
	}
}

// simulate returns the largest run queue the NFA simulation of prog
// builds on input, with the closure the bound uses.
func simulate(prog *syntax.Prog, input string) int {
	anchored := prog.StartCond()&syntax.EmptyBeginText != 0
	cl := closer{prog: prog, mark: make([]uint32, len(prog.Inst))}
	q := cl.closure([]int{prog.Start}, nil)
	best := len(q)
	for _, r := range input {
		var next []int
		for _, pc := range q {
			if i := &prog.Inst[pc]; isRuneInst(i.Op) && i.MatchRune(r) {
				next = append(next, int(i.Out))
			}
		}
		if !anchored {
			next = append(next, prog.Start)
		}
		if len(next) == 0 {
			break
		}
		q = cl.closure(next, nil)
		best = max(best, len(q))
	}
	return best
}

// TestQueueBoundCoversInputs checks the character classes of the bound:
// on inputs drawn from runes at and around every class boundary, case
// folds included, the simulation never builds a queue the bound does not
// allow (07 section 9 item 21, the proposed work bound).
func TestQueueBoundCoversInputs(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: fixed-seed inputs make the test deterministic; nothing here is secret.
	for _, tc := range costPatterns() {
		prog := program(t, tc.pattern)
		budget := patternBudget
		bound := queueBound(prog, &budget)
		var alphabet []rune
		for _, r := range reps(t, prog) {
			alphabet = append(alphabet, r, max(r-1, 0))
		}
		alphabet = append(alphabet, 'a', 'A', 'k', 'K', 'K', '@', '.', '-', '0', 'α', '中')
		for range 200 {
			var b strings.Builder
			for range rng.IntN(40) {
				b.WriteRune(alphabet[rng.IntN(len(alphabet))])
			}
			if got := simulate(prog, b.String()); got > bound {
				t.Fatalf("pattern %.30q, input %q: queue %d, bound %d", tc.pattern, b.String(), got, bound)
			}
		}
	}
}

// TestClassSteps pins the search a rune instruction makes in its class,
// which every queue entry and byte is charged (package documentation "Work
// bound"): none for a literal, one step per range up to four, a binary
// search past them, and the fold orbit of a case-folded rune; the largest
// class of the program decides.
//
// Requirements: 07 section 9 item 21.
func TestClassSteps(t *testing.T) {
	for _, tc := range []struct {
		expr  string
		steps int
	}{
		{`abc`, 0},
		{`[a-z]x`, 1},
		{`[a-bd-eg-hj-k]`, 4},
		{`[a-bd-eg-hj-km]`, 3},
		{`(?i)k`, 4},
		{`\pL{3}`, bits.Len(uint(classRanges(t, `\pL`)))},
		{wideClass(4096) + "a", 13},
	} {
		if got := classSteps(program(t, tc.expr)); got != tc.steps {
			t.Errorf("classSteps(%.20q) = %d, want %d", tc.expr, got, tc.steps)
		}
	}
	steps := bits.Len(uint(classRanges(t, `\pL`)))
	if c := newPatternCosts().of(`\pL{100}`); c.perByte < (queueUnits+searchUnits*steps)*100 {
		t.Errorf("cost of a repetition of \\pL = %+v, want at least %d per byte for 100 queue entries", c, (queueUnits+searchUnits*steps)*100)
	}
}

// TestClassRepsFold checks that a case-folded rune brings its whole fold
// orbit into the classes: k, K and the Kelvin sign (07 section 9 item 21,
// the proposed work bound).
func TestClassRepsFold(t *testing.T) {
	got := reps(t, program(t, `(?i)k`))
	for _, r := range []rune{'K', 'k', 'K', '\n'} {
		if !slices.Contains(got, r) {
			t.Errorf("classReps((?i)k) = %q, missing %q", got, r)
		}
	}
}

// reps returns the class representatives of prog under a budget that
// does not run out.
func reps(t testing.TB, prog *syntax.Prog) []rune {
	t.Helper()
	budget := 1 << 30
	r, ok := classReps(prog, &budget)
	if !ok {
		t.Fatal("classReps ran out of an unbounded budget")
	}
	return r
}

// cjkClass returns a character class of n distinct runes, every other
// code point from U+4E00 on, which the parser keeps as n ranges.
func cjkClass(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := range n {
		b.WriteRune(rune(0x4e00 + 2*i))
	}
	b.WriteByte(']')
	return b.String()
}

// TestClassRepsBudget is the regression test of the review finding that
// the classes of a program were collected and sorted before any budget
// was charged: 990 copies of a class of 19,000 ranges took 1.4 s and 788
// MB in classReps alone (07 F6, a compile returns within 1 s; architecture
// R-77). The copies a repetition makes share their ranges, which count
// once; the bounds are charged before they are added, so the classes of a
// large program stop the exploration within its budget, which then
// charges the whole program.
//
// Requirements: 07 req 75 and F6.
func TestClassRepsBudget(t *testing.T) {
	// 990 copies of one class of 400 ranges: the shared ranges count once.
	prog := program(t, `^`+cjkClass(400)+`{990}`)
	budget := patternBudget
	got, ok := classReps(prog, &budget)
	if spent := patternBudget - budget; !ok || len(got) > 2*400+8 || spent > 4*(400+8) {
		t.Errorf("classReps of 990 copies of 400 ranges: %d representatives, ok %v, spent %d", len(got), ok, spent)
	}
	// Distinct classes past the budget: refused before the bounds that
	// would exceed it are added.
	expr := ""
	for i := range 4 {
		expr += strings.Replace(cjkClass(20000), "[", "[a-"+string(rune('b'+i)), 1)
	}
	prog = program(t, expr)
	budget = patternBudget
	if _, ok := classReps(prog, &budget); ok || budget < 0 {
		t.Errorf("classReps of 80,000 ranges: ok %v, budget left %d, want refused within the budget", ok, budget)
	}
	budget = patternBudget
	if q := queueBound(prog, &budget); q != len(prog.Inst) {
		t.Errorf("queueBound past the classes' budget = %d, want the program's %d", q, len(prog.Inst))
	}
}

// BenchmarkPatternModel measures the pattern model against Go's regexp as
// the library's engine compiles it (compilePattern): for each pattern's
// adversarial input of 8 bytes to 16 KiB it reports the nanoseconds a
// match takes per work unit of fixed plus perByte per byte (ns/unit),
// which must stay below 1 on the reference machine. A benchmark, not a
// test, for the reason BenchmarkWorkModel gives; TestQueueBound,
// TestQueueBoundCoversInputs and TestClassSteps pin the model.
func BenchmarkPatternModel(b *testing.B) {
	for _, tc := range costPatterns() {
		c := newPatternCosts().of(tc.pattern)
		re, err := compilePattern(tc.pattern)
		if err != nil {
			b.Fatal(err)
		}
		rs := []rune(tc.input)
		last := string(rs[len(rs)-1])
		for _, size := range []int{8, 64, 1024, 16384} {
			s := tc.input + strings.Repeat(last, max(0, size-len(tc.input))/len(last))
			b.Run(fmt.Sprintf("%.20s/%d", tc.pattern, size), func(b *testing.B) {
				for b.Loop() {
					re.MatchString(s)
				}
				units := float64(c.fixed + c.perByte*len(s))
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/units, "ns/unit")
			})
		}
	}
}

// altPattern returns an alternation of n two-rune literals of distinct
// CJK runes, which the parser cannot merge into one class: every queue of
// the unanchored program holds the start's thousands of alternatives, and
// every literal's rune is a class of its own.
func altPattern(n int) string { return altPatternFrom(0x4e00, n) }

// altPatternFrom returns an alternation of n two-rune literals of distinct
// runes from first on.
func altPatternFrom(first rune, n int) string {
	var b strings.Builder
	r := first
	for i := range n {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteRune(r)
		b.WriteRune(r + 1)
		r += 2
	}
	return b.String()
}

// TestQueueBoundBudget is the regression test of the review finding that
// the exploration checked its budget only between queues: one queue of an
// alternation of 4,000 two-rune literals scanned every class
// representative with a closure of the whole start each, 61 times the
// budget at 1,000 literals, 244 times at 2,000, and compile times grew
// with the square of the pattern (07 F6, a compile returns within 1 s).
// The steps the exploration charges now stay within its budget plus one
// representative's closure and the setup, the classes and the start's
// closure, and the bound is then the whole program. Steps, not time, so
// the test is deterministic.
func TestQueueBoundBudget(t *testing.T) {
	// Review finding (a mutant that survived): an anchored pattern whose
	// classes mostly advance no thread spends its budget scanning the start
	// queue, never building a closure, so only the check before each class
	// representative stops it.
	sparse := `^(?:` + altPattern(8000) + `)(?:` + altPatternFrom(0x100, 8000) + `)$`
	for _, expr := range []string{altPattern(4000), `^(?:` + altPattern(4000) + `)$`, altPattern(300), `[a-z]{1000}x`, sparse} {
		prog := program(t, expr)
		n := len(prog.Inst)
		budget := patternBudget
		q := queueBound(prog, &budget)
		spent := patternBudget - budget
		if limit := patternBudget + 5*n; spent > limit {
			t.Errorf("queueBound(%.20q, %d instructions) spent %d steps, want at most %d", expr, n, spent, limit)
		}
		if budget < 0 && q != n {
			t.Errorf("queueBound(%.20q) = %d past its budget, want the program's %d", expr, q, n)
		}
	}
	// A document's patterns share regexpBudget: past it every pattern costs
	// its whole program without being explored.
	pc := newPatternCosts()
	for i := range 20 {
		if c := pc.of(altPattern(1000 + i)); c.perByte < queueUnits*c.insts {
			t.Fatalf("pattern %d: per byte %d, want the whole program's %d", i, c.perByte, queueUnits*c.insts)
		}
	}
	if pc.budget > 0 {
		t.Errorf("document budget left %d after 20 exploded patterns, want it spent", pc.budget)
	}
}

// BenchmarkPatternCost times the exploration of the costliest patterns
// within one pattern's budget: a long repetition of a class, and an
// alternation of 4,000 literals, unanchored and anchored (review finding).
func BenchmarkPatternCost(b *testing.B) {
	for _, tc := range []struct{ name, expr string }{
		{"repetition", `[a-z]{1000}x`},
		{"alternation", altPattern(4000)},
		{"anchored alternation", `^(?:` + altPattern(4000) + `)$`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				newPatternCosts().of(tc.expr)
			}
		})
	}
}
