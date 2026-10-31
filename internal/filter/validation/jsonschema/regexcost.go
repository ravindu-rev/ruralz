// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"cmp"
	"encoding/binary"
	"math/bits"
	"regexp/syntax"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// The work of a pattern (package documentation "Work bound"). The library
// matches pattern and patternProperties with Go's regexp, whose matchers
// (the backtracker and the NFA simulation: compilePattern never lets it
// build the one-pass matcher) never visit more program instructions per
// input rune than the NFA simulation holds in its run queue. A pattern's
// cost per byte is therefore bounded by the largest run queue any input
// can build, which queueBound computes by exploring the queues the NFA
// reaches over the program's character classes; a pattern whose
// exploration does not finish within its budget is charged its whole
// program per byte instead. That bounds the work of the matchers, not
// their memory: the backtracker's job stack grows with the input
// (backtrackBytes).
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
	// insts is the program's instruction count, for the matcher's memory,
	// and held the most jobs the backtracker may hold per input position
	// (heldAlternations), for its job stack.
	insts, held int
}

// The backtracker's job stack. Go's regexp matches with its backtracker
// whenever the program has at most maxBacktrackProg instructions and the
// input is shorter than maxBacktrackVector bits per instruction, that is
// shorter than maxBacktrackVector divided by the program's instruction
// count, in bytes (regexp/backtrack.go); otherwise with the NFA simulation,
// whose queues and threads grow with the program alone. The backtracker
// marks every pair of instruction and input position it visits, so it
// visits each at most once, and searches depth first, keeping a stack of
// jobs. For a match without submatches, as the library asks, the only jobs
// on the stack besides the one it starts from are reminders, one for every
// alternation on the current search path whose first branch it is still
// exploring, at the position it entered it; the reminder is popped when the
// search turns to the second branch. The path moves through the input
// positions in order, consuming a rune between two of them, so at one
// position it is a run of empty-width steps (alternations, no-ops, captures
// and assertions) from the instruction the position's search starts at,
// the program's start or the instruction after a rune, and the visited
// marks keep the run's alternations distinct. The stack therefore holds at
// most held jobs per input position, held being the most alternations
// reachable through empty-width steps from any one such instruction, plus
// the first: (n+1)*held+1 on n bytes, which never exceeds the visited set's
// maxBacktrackVector pairs, about 12.6 MB at jobBytes. (?:(?:a??){240}a)*b,
// whose 241 alternations all follow one another, held 129,658 jobs on 537
// bytes, a charge of 129,659; the WHATWG e-mail pattern, 126 alternations
// in all, holds at most 3 per position (charged every alternation of the
// program, small bodies under it would be refused at a 64 KiB limit).
const (
	// maxBacktrackProg and maxBacktrackVector are Go's limits: the largest
	// program the backtracker runs, and the bits of its visited set, which
	// bound the input length at maxBacktrackVector/len(prog.Inst).
	maxBacktrackProg, maxBacktrackVector = 500, 256 << 10
	// jobBytes is what one job may hold: 16 bytes in a slice grown by
	// appending, up to three times that while append copies the array
	// into one twice as large, both live (past 256 jobs Go 1.26 and 1.27
	// grow it by less: the matcher's peak of a*b on 43,688 bytes,
	// computed exactly from the match's allocations, holds its 43,689 jobs
	// in 1.93 times their 16 bytes, TestHandleBacktrackCharge);
	// jobArrayBytes the 256 jobs the stack starts with.
	jobBytes, jobArrayBytes = 48, 256 * 16
	// heldBudget bounds the instructions the search for held alternations
	// may visit in all the patterns of one document (about 10 ms,
	// BenchmarkHeldAlternations); past it a program is charged every
	// alternation it has.
	heldBudget = 1 << 21
)

// backtrackProg is one program the backtracker may run: its instruction
// count and its held jobs per input position.
type backtrackProg struct{ insts, held int }

// backtrackProgs returns the programs of progs that the backtracker may
// run, by size, keeping only those that hold more jobs per position than
// every smaller one: the backtracker runs every program smaller than one
// it runs on the same input, so the others never hold the most.
func backtrackProgs(progs []backtrackProg) []backtrackProg {
	out := make([]backtrackProg, 0, len(progs))
	for _, p := range progs {
		if p.insts <= maxBacktrackProg {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b backtrackProg) int { return cmp.Or(a.insts-b.insts, b.held-a.held) })
	k := 0
	for _, p := range out {
		if k == 0 || p.held > out[k-1].held {
			out[k] = p
			k++
		}
	}
	return slices.Clip(out[:k])
}

// backtrackBytes returns the job stack a match of n bytes may hold under
// progs (backtrackProgs): none when the backtracker runs no program on n
// bytes, else the starting array and jobBytes for the first job and held
// jobs per input position of the program holding the most that it runs on
// n bytes, those smaller than maxBacktrackVector/n instructions.
func backtrackBytes(progs []backtrackProg, n int) int64 {
	runs := sort.Search(len(progs), func(i int) bool { return n >= maxBacktrackVector/progs[i].insts })
	if runs == 0 {
		return 0
	}
	jobs := int64(progs[runs-1].held)*int64(n+1) + 1
	return jobArrayBytes + jobBytes*jobs
}

// patternReach is what may match one string while the library validates
// it: the largest program of the patterns that may, for the matcher, and
// those of them the backtracker may run (backtrackProgs), for its job
// stack. The zero value is no pattern. Its slice is never changed once
// made, so reaches share it.
type patternReach struct {
	insts int
	progs []backtrackProg
}

// reachOf returns the reach of the patterns of costs.
func reachOf(costs ...patternCost) patternReach {
	var r patternReach
	progs := make([]backtrackProg, 0, len(costs))
	for _, c := range costs {
		r.insts = max(r.insts, c.insts)
		progs = append(progs, backtrackProg{insts: c.insts, held: c.held})
	}
	r.progs = backtrackProgs(progs)
	return r
}

// reachMerger merges the reaches of the schemas of one state
// (stateBuilder.weigh) in time linear in their programs, with no
// allocation per schema: the backtracker runs programs of at most
// maxBacktrackProg instructions, so the most jobs of each size fit a
// table, which the merged list reads in order of size (a union per schema,
// concatenating and sorting the growing list again, would compile 1,000
// patterns at each of 349 states for 2.9 s and allocate 2.4 GB).
// The zero value is empty.
type reachMerger struct {
	// insts is the largest program added; n counts the programs of the
	// backtracker added, between lo and hi instructions, and held[i] is one
	// more than the most jobs one of i instructions holds, 0 for none.
	insts, n, lo, hi int
	held             [maxBacktrackProg + 1]int
}

// add merges the reach r.
func (m *reachMerger) add(r patternReach) {
	m.insts = max(m.insts, r.insts)
	for _, p := range r.progs {
		if m.n == 0 {
			m.lo, m.hi = p.insts, p.insts
		}
		m.n++
		m.lo, m.hi = min(m.lo, p.insts), max(m.hi, p.insts)
		m.held[p.insts] = max(m.held[p.insts], p.held+1)
	}
}

// reach returns the merged reach, its list of programs interned in lists
// (backtrackProgs of every program added), and empties the merger.
func (m *reachMerger) reach(lists *reachLists) patternReach {
	r := patternReach{insts: m.insts}
	if m.n > 0 {
		out := lists.scratch[:0]
		for i := m.lo; i <= m.hi; i++ {
			if h := m.held[i] - 1; h >= 0 {
				if len(out) == 0 || h > out[len(out)-1].held {
					out = append(out, backtrackProg{insts: i, held: h})
				}
				m.held[i] = 0
			}
		}
		lists.scratch = out
		r.progs = lists.intern(out)
	}
	m.insts, m.n = 0, 0
	return r
}

// reachLists interns the program lists of merged reaches, so the states
// with the same patterns share one list and the compiled Schema keeps each
// distinct list once (1,200 states each keeping a list of 241 programs
// would hold 4.5 MB).
type reachLists struct {
	lists   map[string][]backtrackProg
	key     []byte
	scratch []backtrackProg
}

// intern returns the list equal to progs that lists holds, adding a copy
// when it holds none.
func (l *reachLists) intern(progs []backtrackProg) []backtrackProg {
	l.key = l.key[:0]
	for _, p := range progs {
		l.key = binary.AppendVarint(l.key, int64(p.insts))
		l.key = binary.AppendVarint(l.key, int64(p.held))
	}
	if s, ok := l.lists[string(l.key)]; ok {
		return s
	}
	if l.lists == nil {
		l.lists = map[string][]backtrackProg{}
	}
	s := slices.Clone(progs)
	l.lists[string(l.key)] = s
	return s
}

// bytes returns what one match of n bytes may hold under r: nothing
// without a pattern, else a matcher of the largest program and the
// backtracker's job stack (a pattern of 241 alternations holds 4.9 MB for
// 537 bytes).
func (r patternReach) bytes(n int) int64 {
	if r.insts == 0 {
		return 0
	}
	return matcherBytes + matcherInstBytes*int64(r.insts) + backtrackBytes(r.progs, n)
}

// patternCosts computes pattern costs under one document's budget,
// caching them by expression.
type patternCosts struct {
	// budget is what is left of regexpBudget, held of heldBudget.
	budget, held int
	cache        map[string]patternCost
}

func newPatternCosts() *patternCosts {
	return &patternCosts{budget: regexpBudget, held: heldBudget, cache: map[string]patternCost{}}
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
			if n <= maxBacktrackProg {
				c.held = heldAlternations(prog, &pc.held)
			}
		}
	}
	pc.cache[expr] = c
	return c
}

// heldAlternations returns the most jobs the backtracker may hold per
// input position under prog (see "The backtracker's job stack"): the most
// alternations reachable through empty-width steps from the program's
// start or from the instruction after a rune, where the search at one
// position starts. Empty-width assertions are assumed to hold. Every
// instruction it visits is charged to budget; past it, the result is
// every alternation of the program, which bounds each such set.
func heldAlternations(prog *syntax.Prog, budget *int) int {
	n := len(prog.Inst)
	start := make([]bool, n)
	start[prog.Start] = true
	for i := range prog.Inst {
		if isRuneInst(prog.Inst[i].Op) {
			start[prog.Inst[i].Out] = true
		}
	}
	mark := make([]int, n)
	var stack []int
	held := 0
	for s, ok := range start {
		if !ok {
			continue
		}
		count := 0
		stack = append(stack[:0], s)
		for len(stack) > 0 {
			pc := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if mark[pc] == s+1 {
				continue
			}
			mark[pc] = s + 1
			if *budget--; *budget < 0 {
				return alternations(prog)
			}
			switch in := &prog.Inst[pc]; in.Op {
			case syntax.InstAlt, syntax.InstAltMatch:
				count++
				stack = append(stack, int(in.Out), int(in.Arg))
			case syntax.InstCapture, syntax.InstNop, syntax.InstEmptyWidth:
				stack = append(stack, int(in.Out))
			default:
				// A rune, match or fail instruction ends the run.
			}
		}
		held = max(held, count)
	}
	return held
}

// alternations returns the alternation instructions of prog, which bound
// the alternations reachable from any one instruction.
func alternations(prog *syntax.Prog) int {
	n := 0
	for i := range prog.Inst {
		if op := prog.Inst[i].Op; op == syntax.InstAlt || op == syntax.InstAltMatch {
			n++
		}
	}
	return n
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
// the program (checked only between queues, one queue of an alternation of
// thousands of literals would take seconds).
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
// (uncharged, 990 copies of a class of 19,000 ranges would take 1.4 s and
// 788 MB here). It reports false when the budget runs out.
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
