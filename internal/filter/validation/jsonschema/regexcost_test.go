// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"math/rand/v2"
	"regexp"
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
	}
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
	prog := program(t, emailPattern)
	none := 0
	if q := queueBound(prog, &none); q != len(prog.Inst) {
		t.Errorf("queueBound without budget = %d, want the program's %d", q, len(prog.Inst))
	}
	pc := newPatternCosts()
	pc.budget = 0
	if c := pc.of(emailPattern); c.perByte != byteUnits+queueUnits*len(prog.Inst)+len(prog.Inst)/clearInstsPerUnit || c.insts != len(prog.Inst) {
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
		for _, r := range classReps(prog) {
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

// TestClassRepsFold checks that a case-folded rune brings its whole fold
// orbit into the classes: k, K and the Kelvin sign (07 section 9 item 21,
// the proposed work bound).
func TestClassRepsFold(t *testing.T) {
	reps := classReps(program(t, `(?i)k`))
	for _, r := range []rune{'K', 'k', 'K', '\n'} {
		if !slices.Contains(reps, r) {
			t.Errorf("classReps((?i)k) = %q, missing %q", reps, r)
		}
	}
}

// BenchmarkPatternModel measures the pattern model against Go's regexp:
// for each pattern's adversarial input of 8 bytes to 16 KiB it reports the
// nanoseconds a match takes per work unit of fixed plus perByte per byte
// (ns/unit), which must stay below 1 on the reference machine. A
// benchmark, not a test, for the reason BenchmarkWorkModel gives;
// TestQueueBound and TestQueueBoundCoversInputs pin the model.
func BenchmarkPatternModel(b *testing.B) {
	for _, tc := range costPatterns() {
		c := newPatternCosts().of(tc.pattern)
		re := regexp.MustCompile(tc.pattern)
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
func altPattern(n int) string {
	var b strings.Builder
	r := rune(0x4e00)
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
	for _, expr := range []string{altPattern(4000), `^(?:` + altPattern(4000) + `)$`, altPattern(300), `[a-z]{1000}x`} {
		prog := program(t, expr)
		n := len(prog.Inst)
		budget := patternBudget
		q := queueBound(prog, &budget)
		spent := patternBudget - budget
		if limit := patternBudget + len(classReps(prog)) + 5*n; spent > limit {
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
