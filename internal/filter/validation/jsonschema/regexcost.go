// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// The work of a pattern (proposed, package documentation "Work bound").
// The library matches pattern and patternProperties with Go's regexp,
// whose matchers (one-pass, backtracking and the NFA simulation) never
// visit more program instructions per input rune than the NFA simulation
// holds in its run queue. A pattern's cost per byte is therefore bounded
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
	// at 6 to 16 units for adversarial inputs.
	queueUnits = 16
	// clearInstsPerUnit is the program instructions per work unit and
	// byte that clearing the backtracker's visited set takes.
	clearInstsPerUnit = 16
	// regexpBudget bounds the queue entries the exploration of all the
	// patterns of one document may touch, and patternBudget those of one
	// pattern (about 7 ms; BenchmarkPatternCost).
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

// of returns the cost of the RE2 expression expr. An expression that does
// not compile (the walk refuses it first) costs the largest count.
func (pc *patternCosts) of(expr string) patternCost {
	if c, ok := pc.cache[expr]; ok {
		return c
	}
	c := patternCost{fixed: countCap, perByte: countCap, insts: countCap}
	if re, err := syntax.Parse(expr, syntax.Perl); err == nil {
		if prog, err := syntax.Compile(re.Simplify()); err == nil {
			n := len(prog.Inst)
			b := min(pc.budget, patternBudget)
			q := queueBound(prog, &b)
			pc.budget -= min(pc.budget, patternBudget) - b
			c = patternCost{
				fixed:   matchUnits + matchInstUnits*n,
				perByte: byteUnits + queueUnits*q + n/clearInstsPerUnit,
				insts:   n,
			}
		}
	}
	pc.cache[expr] = c
	return c
}

// queueBound returns the largest run queue the NFA simulation of prog can
// build: the instructions it adds for one input position, which is the
// closure of the threads that survive the previous rune (and, unanchored,
// of the start). Empty-width assertions are assumed to hold, which can
// only enlarge a queue. Past budget or regexpStates queues, the result is
// the whole program, which bounds every queue.
func queueBound(prog *syntax.Prog, budget *int) int {
	reps := classReps(prog)
	anchored := prog.StartCond()&syntax.EmptyBeginText != 0
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
	start := closure(prog, []int{prog.Start})
	best := len(start)
	visit(start)
	for len(work) > 0 {
		if len(seen) > regexpStates || *budget < 0 || best == len(prog.Inst) {
			return len(prog.Inst)
		}
		q := work[len(work)-1]
		work = work[:len(work)-1]
		for _, r := range reps {
			var next []int
			for _, pc := range q {
				i := &prog.Inst[pc]
				if isRuneInst(i.Op) && i.MatchRune(r) {
					next = append(next, int(i.Out))
				}
			}
			*budget -= len(q)
			if !anchored {
				next = append(next, prog.Start)
			}
			if len(next) == 0 {
				continue
			}
			c := closure(prog, next)
			*budget -= len(c)
			best = max(best, len(c))
			visit(c)
		}
	}
	return best
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

// closure returns the instructions the simulation adds to a queue from
// pcs: each instruction once, following alternations, captures, no-ops
// and (assumed true) empty-width assertions; sorted.
func closure(prog *syntax.Prog, pcs []int) []int {
	in := map[int]bool{}
	var out []int
	stack := slices.Clone(pcs)
	for len(stack) > 0 {
		pc := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if in[pc] {
			continue
		}
		in[pc] = true
		out = append(out, pc)
		i := &prog.Inst[pc]
		switch i.Op {
		case syntax.InstAlt, syntax.InstAltMatch:
			stack = append(stack, int(i.Arg), int(i.Out))
		case syntax.InstCapture, syntax.InstNop, syntax.InstEmptyWidth:
			stack = append(stack, int(i.Out))
		default:
			// A rune, match or fail instruction holds a thread or none.
		}
	}
	slices.Sort(out)
	return out
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
// orbit, and the newline that RuneAnyNotNL excludes.
func classReps(prog *syntax.Prog) []rune {
	bounds := []rune{0, '\n', '\n' + 1}
	single := func(r rune) {
		bounds = append(bounds, r, r+1)
	}
	for i := range prog.Inst {
		in := &prog.Inst[i]
		switch in.Op {
		case syntax.InstRune1:
			single(in.Rune[0])
		case syntax.InstRune:
			if len(in.Rune) == 1 {
				single(in.Rune[0])
				if in.Arg&uint32(syntax.FoldCase) != 0 {
					for f := unicode.SimpleFold(in.Rune[0]); f != in.Rune[0]; f = unicode.SimpleFold(f) {
						single(f)
					}
				}
				continue
			}
			for j := 0; j+1 < len(in.Rune); j += 2 {
				bounds = append(bounds, in.Rune[j], in.Rune[j+1]+1)
			}
		default:
			// Other instructions match no rune, or every rune but the
			// newline already among the bounds.
		}
	}
	slices.Sort(bounds)
	bounds = slices.Compact(bounds)
	for len(bounds) > 0 && bounds[len(bounds)-1] > unicode.MaxRune {
		bounds = bounds[:len(bounds)-1]
	}
	return bounds
}
