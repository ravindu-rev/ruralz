// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"math/rand/v2"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
	"time"
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
	q := closure(prog, []int{prog.Start})
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
		q = closure(prog, next)
		best = max(best, len(q))
	}
	return best
}

// TestQueueBoundCoversInputs checks the character classes of the bound:
// on inputs drawn from runes at and around every class boundary, case
// folds included, the simulation never builds a queue the bound does not
// allow.
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
// orbit into the classes: k, K and the Kelvin sign.
func TestClassRepsFold(t *testing.T) {
	reps := classReps(program(t, `(?i)k`))
	for _, r := range []rune{'K', 'k', 'K', '\n'} {
		if !slices.Contains(reps, r) {
			t.Errorf("classReps((?i)k) = %q, missing %q", reps, r)
		}
	}
}

// TestPatternCostCoversMatch checks the pattern model against Go's
// regexp: matching each pattern's adversarial input of 8 bytes to 16 KiB
// takes no more nanoseconds than fixed plus perByte work units per byte.
// A timing test: skipped under -race, best of several runs, retried.
func TestPatternCostCoversMatch(t *testing.T) {
	if raceEnabled {
		t.Skip("-race slows the matcher, the model is calibrated without it")
	}
	for _, tc := range costPatterns() {
		c := newPatternCosts().of(tc.pattern)
		re := regexp.MustCompile(tc.pattern)
		rs := []rune(tc.input)
		last := string(rs[len(rs)-1])
		for _, size := range []int{8, 64, 1024, 16384} {
			s := tc.input + strings.Repeat(last, max(0, size-len(tc.input))/len(last))
			runs := max(1, 20000/size)
			limit := float64(c.fixed + c.perByte*len(s))
			var best float64
			for range 3 {
				best = 1e18
				for range 3 {
					start := time.Now()
					for range runs {
						re.MatchString(s)
					}
					best = min(best, float64(time.Since(start).Nanoseconds())/float64(runs))
				}
				if best <= limit {
					break
				}
			}
			if best > limit {
				t.Errorf("pattern %.30q on %d bytes: %.0f ns, the model charges %.0f units", tc.pattern, len(s), best, limit)
			}
		}
	}
}

// BenchmarkPatternCost times the exploration of the costliest pattern
// within one pattern's budget.
func BenchmarkPatternCost(b *testing.B) {
	for b.Loop() {
		newPatternCosts().of(`[a-z]{1000}x`)
	}
}
