// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"math/bits"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// The work of a pattern (architecture R-77, package documentation "Work
// bound").
// The library matches pattern and patternProperties with Go's regexp,
// whose matchers (the backtracker and the NFA simulation: compilePattern
// never lets it build the one-pass matcher) never visit more program
// instructions per input rune than the NFA simulation holds in its run
// queue. A pattern's cost per byte is therefore bounded
// by the largest run queue any input can build, which queueBound computes
// by exploring the queues the NFA reaches over the program's character
// classes; a pattern whose exploration does not finish within its budget
// is charged its whole program per byte instead.
const (
	// matchUnits is the work of one match call that does not grow with
	// the input (taking a matcher from the pool, the setup), measured at
	// 50 to 140 work units for short patterns.
	matchUnits = 128
	// matchInstUnits is the setup work per program instruction.
	matchInstUnits = 2
	// byteUnits is the work per input byte that does not depend on the
	// queue (decoding the rune, the step loop).
	byteUnits = 16
	// queueUnits is the work per run-queue entry and input byte, measured
	// at 6 to 16 units for adversarial inputs; searchUnits is the work per
	// entry, input byte and step of the search a rune instruction makes in
	// the ranges of its class, at most classSteps of the program's largest
	// class (a repetition of \pL takes about 21 ns per entry and byte, of a
	// one-range class 9; measured 9 to 27 ns from 1 to 20,000 ranges,
	// BenchmarkPatternModel).
	queueUnits, searchUnits = 16, 2
	// clearInstsPerUnit is the program instructions per work unit and
	// byte that clearing the backtracker's visited set takes.
	clearInstsPerUnit = 16
	// regexpBudget bounds the steps the exploration of all the patterns of
	// one document may take, and patternBudget those of one pattern, each
	// a queue entry scanned or added (about 25 ms and 3 ms;
	// BenchmarkPatternCost, TestQueueBoundBudget).
	regexpBudget, patternBudget = 1 << 20, 1 << 17
	// regexpStates bounds the queues the exploration of one pattern
	// visits.
	regexpStates = 4096
)

// patternCost is the work one match of a pattern may take on an input of
// n bytes: fixed + n*perByte.
type patternCost struct {
	fixed, perByte int
	// insts is the program's instruction count, for the matcher's memory.
	insts int
}

// patternCosts computes pattern costs under one document's budget,
// caching them by expression.
type patternCosts struct {
	budget int
	cache  map[string]patternCost
}

func newPatternCosts() *patternCosts {
	return &patternCosts{budget: regexpBudget, cache: map[string]patternCost{}}
}

// of returns the cost of the RE2 expression expr, as the library's
// engine compiles it (compilePattern). An expression that does not compile
// (the walk refuses it first) costs the largest count.
func (pc *patternCosts) of(expr string) patternCost {
	if c, ok := pc.cache[expr]; ok {
		return c
	}
	c := patternCost{fixed: countCap, perByte: countCap, insts: countCap}
	if re, err := syntax.Parse(noOnePass+expr, syntax.Perl); err == nil {
		if prog, err := syntax.Compile(re.Simplify()); err == nil {
			n := len(prog.Inst)
			b := min(pc.budget, patternBudget)
			q := queueBound(prog, &b)
			pc.budget -= min(pc.budget, patternBudget) - b
			c = patternCost{
				fixed:   matchUnits + matchInstUnits*n,
				perByte: byteUnits + (queueUnits+searchUnits*classSteps(prog))*q + n/clearInstsPerUnit,
				insts:   n,
			}
		}
	}
	pc.cache[expr] = c
	return c
}

// classSteps returns the most steps a rune instruction of prog takes to
// find a rune in its ranges (syntax.Inst.MatchRunePos): none for one
// range or one rune, one per range for up to four, which it scans, and a
// binary search past them; a case-folded rune walks its fold orbit of at
// most four.
func classSteps(prog *syntax.Prog) int {
	steps := 0
	for i := range prog.Inst {
		in := &prog.Inst[i]
		if in.Op != syntax.InstRune {
			continue
		}
		switch r := len(in.Rune) / 2; {
		case len(in.Rune) == 1:
			steps = max(steps, 4)
		case r <= 4:
			steps = max(steps, r)
		default:
			steps = max(steps, bits.Len(uint(r)))
		}
	}
	return steps
}

// queueBound returns the largest run queue the NFA simulation of prog can
// build: the instructions it adds for one input position, which is the
// closure of the threads that survive the previous rune (and, unanchored,
// of the start). Empty-width assertions are assumed to hold, which can
// only enlarge a queue. Past budget or regexpStates queues, the result is
// the whole program, which bounds every queue.
//
// The scan of a queue for each class representative is charged to budget
// before it is taken, and its closure as soon as it is computed, and the
// budget is checked before each representative, so the exploration
// overruns its budget by at most one representative's closure, about twice
// the program (review finding: checked only between queues, one queue of
// an alternation of thousands of literals took seconds).
func queueBound(prog *syntax.Prog, budget *int) int {
	n := len(prog.Inst)
	if *budget < 0 {
		return n
	}
	reps, ok := classReps(prog, budget)
	if !ok {
		return n
	}
	anchored := prog.StartCond()&syntax.EmptyBeginText != 0
	cl := closer{prog: prog, mark: make([]uint32, n)}
	start := cl.closure([]int{prog.Start}, nil)
	*budget -= 2 * len(start)
	seen := map[string]bool{}
	var work [][]int
	visit := func(q []int) {
		if len(q) == 0 {
			return
		}
		k := queueKey(q)
		if !seen[k] {
			seen[k] = true
			work = append(work, q)
		}
	}
	best := len(start)
	visit(start)
	// Unanchored, the start's closure joins every queue: it is computed
	// once and merged.
	var always []int
	if !anchored {
		always = start
	}
	var next []int
	for len(work) > 0 {
		if len(seen) > regexpStates || best == n {
			return n
		}
		q := work[len(work)-1]
		work = work[:len(work)-1]
		for _, r := range reps {
			// The scan of the queue, then the closure, at most twice the
			// program each.
			if *budget -= len(q); *budget < 0 {
				return n
			}
			next = next[:0]
			for _, pc := range q {
				i := &prog.Inst[pc]
				if isRuneInst(i.Op) && i.MatchRune(r) {
					next = append(next, int(i.Out))
				}
			}
			if len(next) == 0 && len(always) == 0 {
				continue
			}
			c := cl.closure(next, always)
			if *budget -= len(next) + 2*len(c); *budget < 0 {
				return n
			}
			best = max(best, len(c))
			visit(c)
		}
	}
	return best
}

// closer computes closures of one program, with a scratch mark per
// instruction.
type closer struct {
	prog  *syntax.Prog
	mark  []uint32
	gen   uint32
	stack []int
}

// closure returns the instructions the simulation adds to a queue from
// pcs, together with base, a closure already computed: each instruction
// once, following alternations, captures, no-ops and (assumed true)
// empty-width assertions; sorted.
func (c *closer) closure(pcs, base []int) []int {
	c.gen++
	out := make([]int, 0, len(base)+len(pcs))
	for _, pc := range base {
		c.mark[pc] = c.gen
		out = append(out, pc)
	}
	c.stack = append(c.stack[:0], pcs...)
	for len(c.stack) > 0 {
		pc := c.stack[len(c.stack)-1]
		c.stack = c.stack[:len(c.stack)-1]
		if c.mark[pc] == c.gen {
			continue
		}
		c.mark[pc] = c.gen
		out = append(out, pc)
		i := &c.prog.Inst[pc]
		switch i.Op {
		case syntax.InstAlt, syntax.InstAltMatch:
			c.stack = append(c.stack, int(i.Arg), int(i.Out))
		case syntax.InstCapture, syntax.InstNop, syntax.InstEmptyWidth:
			c.stack = append(c.stack, int(i.Out))
		default:
			// A rune, match or fail instruction holds a thread or none.
		}
	}
	slices.Sort(out)
	return out
}

// isRuneInst reports whether op consumes a rune.
func isRuneInst(op syntax.InstOp) bool {
	switch op {
	case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
		return true
	default:
		return false
	}
}

// queueKey returns the identity of a sorted queue.
func queueKey(q []int) string {
	var b strings.Builder
	for _, pc := range q {
		b.WriteString(strconv.Itoa(pc))
		b.WriteByte(',')
	}
	return b.String()
}

// classReps returns one rune of every class of runes that no rune
// instruction of prog tells apart: the bounds of every range an
// instruction matches, case-folded single runes with their whole fold
// orbit, and the newline that RuneAnyNotNL excludes. The copies a
// repetition makes of a class share its ranges, which count once. Every
// bound is charged to budget twice, for adding and for sorting it, and
// the budget is checked before an instruction's bounds are added, so the
// classes of a large program end the exploration without overrunning it
// (review finding: 990 copies of a class of 19,000 ranges took 1.4 s and
// 788 MB here before the budget was charged). It reports false when the
// budget runs out.
func classReps(prog *syntax.Prog, budget *int) ([]rune, bool) {
	// A fold orbit holds at most four runes.
	const maxOrbit = 4
	type ranges struct {
		first *rune
		n     int
	}
	seen := map[ranges]bool{}
	bounds := []rune{0, '\n', '\n' + 1}
	for i := range prog.Inst {
		in := &prog.Inst[i]
		var add []rune
		switch {
		case in.Op == syntax.InstRune1, in.Op == syntax.InstRune && len(in.Rune) == 1:
			add = in.Rune[:1]
		case in.Op == syntax.InstRune && len(in.Rune) > 1:
			k := ranges{&in.Rune[0], len(in.Rune)}
			if seen[k] {
				continue
			}
			seen[k] = true
			add = in.Rune
		default:
			// Other instructions match no rune, or every rune but the
			// newline already among the bounds.
			continue
		}
		if 2*(len(bounds)+max(len(add), 2*maxOrbit)) > *budget {
			*budget -= 2 * len(bounds)
			return nil, false
		}
		if len(add) == 1 {
			r := add[0]
			bounds = append(bounds, r, r+1)
			if in.Op == syntax.InstRune && in.Arg&uint32(syntax.FoldCase) != 0 {
				for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
					bounds = append(bounds, f, f+1)
				}
			}
			continue
		}
		for j := 0; j+1 < len(add); j += 2 {
			bounds = append(bounds, add[j], add[j+1]+1)
		}
	}
	*budget -= 2 * len(bounds)
	slices.Sort(bounds)
	bounds = slices.Compact(bounds)
	for len(bounds) > 0 && bounds[len(bounds)-1] > unicode.MaxRune {
		bounds = bounds[:len(bounds)-1]
	}
	return bounds, true
}
