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
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/filter"
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
		prog := program(t, noOnePass+tc.pattern)
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
		// As the library's engine compiles it (minor review finding).
		prog := program(t, noOnePass+tc.pattern)
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

// TestBacktrackBytes covers the job stack charge (review findings): a
// program of at most maxBacktrackProg instructions on an input shorter
// than maxBacktrackVector bits per instruction is charged its held jobs
// per position and the first, for the program holding the most that the
// backtracker may run on that many bytes; a longer input or a larger
// program runs on the NFA simulation and is charged none. Programs that
// hold no more than a smaller one are dropped.
//
// Requirements: 07 reqs 80 to 82 and architecture R-77.
func TestBacktrackBytes(t *testing.T) {
	costs := newPatternCosts()
	small, wide := costs.of(`a*b`), costs.of(`(?:(?:a??){240}a)*b`)
	large := costs.of(largestPattern('x', 0))
	if small.insts != 6 || small.held != 1 || wide.insts != 486 || wide.held != 241 || large.insts <= maxBacktrackProg || large.held != 0 {
		t.Fatalf("programs: a*b %+v, (a??){240} %+v, largest %+v", small, wide, large)
	}
	r := reachOf(small, wide, large)
	if want := []backtrackProg{{6, 1}, {486, 241}}; r.insts != large.insts || !slices.Equal(r.progs, want) {
		t.Fatalf("reachOf = %+v, want the largest program and %+v", r, want)
	}
	jobs := func(held, n int) int64 { return jobArrayBytes + jobBytes*int64(held*(n+1)+1) }
	for _, tc := range []struct {
		n    int
		want int64
	}{
		{0, jobs(241, 0)},
		{537, jobs(241, 537)},
		{maxBacktrackVector/486 - 1, jobs(241, maxBacktrackVector/486-1)},
		// Past the wide program's inputs, only a*b runs on the backtracker.
		{maxBacktrackVector / 486, jobs(1, maxBacktrackVector/486)},
		{maxBacktrackVector/6 - 1, jobs(1, maxBacktrackVector/6-1)},
		{maxBacktrackVector / 6, 0},
		{10 << 20, 0},
	} {
		if got := backtrackBytes(r.progs, tc.n); got != tc.want {
			t.Errorf("backtrackBytes(%d) = %d, want %d", tc.n, got, tc.want)
		}
		if got, want := r.bytes(tc.n), matcherBytes+matcherInstBytes*int64(large.insts)+tc.want; got != want {
			t.Errorf("bytes(%d) = %d, want %d", tc.n, got, want)
		}
	}
	if got := backtrackBytes(nil, 10); got != 0 {
		t.Errorf("backtrackBytes without programs = %d", got)
	}
	if got := (patternReach{}).bytes(10); got != 0 {
		t.Errorf("bytes without a pattern = %d", got)
	}
	// A program that holds no more than a smaller one never decides.
	got := backtrackProgs([]backtrackProg{{10, 2}, {5, 3}, {20, 3}, {20, 4}, {600, 9}, {5, 1}})
	if want := []backtrackProg{{5, 3}, {20, 4}}; !slices.Equal(got, want) {
		t.Errorf("backtrackProgs = %+v, want %+v", got, want)
	}
	// A merge is the reach of every pattern added; with none it is empty,
	// and a merger is empty again once read.
	var m reachMerger
	var lists reachLists
	m.add(reachOf(small))
	m.add(patternReach{})
	m.add(reachOf(wide))
	if u := m.reach(&lists); u.insts != 486 || !slices.Equal(u.progs, []backtrackProg{{6, 1}, {486, 241}}) {
		t.Errorf("merged reach = %+v", u)
	}
	if u := m.reach(&lists); u.insts != 0 || u.progs != nil {
		t.Errorf("merger after a read = %+v, want no pattern", u)
	}
	m.add(reachOf(large))
	if u := m.reach(&lists); u.insts != large.insts || len(u.progs) != 0 {
		t.Errorf("merged reach of a program past the backtracker = %+v", u)
	}
	// A larger program that holds no more is dropped, and nothing of an
	// earlier merge is left in the table.
	literal := costs.of(strings.Repeat("x", 490))
	if literal.insts <= wide.insts || literal.insts > maxBacktrackProg || literal.held > small.held {
		t.Fatalf("x{490}: %+v", literal)
	}
	m.add(reachOf(wide))
	m.reach(&lists)
	m.add(reachOf(literal))
	m.add(reachOf(small))
	if u := m.reach(&lists); u.insts != literal.insts || !slices.Equal(u.progs, []backtrackProg{{6, 1}}) {
		t.Errorf("merged reach after another = %+v, want %d instructions and a*b's program alone", u, literal.insts)
	}
	// The alternations are the program's own, the empty group included.
	for _, tc := range []struct {
		expr string
		alts int
	}{
		{`abc`, 0},
		{`ab|cd|ef`, 2},
		{`a?b*c+`, 3},
		{`(?:ab){2,4}`, 2},
	} {
		if got := alternations(program(t, noOnePass+tc.expr)); got != tc.alts {
			t.Errorf("alternations(%s) = %d, want %d", tc.expr, got, tc.alts)
		}
	}
}

// emailExpr returns the expression emailPattern writes in JSON.
func emailExpr() string { return strings.ReplaceAll(emailPattern, `\\`, `\`) }

// hostnamePattern is a common hostname pattern: labels of up to 63
// characters, then a top-level domain.
const hostnamePattern = `^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,63}$`

// TestHeldAlternations pins the jobs per input position the backtracker
// may hold (review finding: every alternation of the program was charged
// at every position, 126 for the WHATWG e-mail pattern, which holds at
// most 3): the alternations reachable through empty-width steps from the
// start or from the instruction after a rune, at most every alternation
// of the program, which past its budget it charges.
//
// Requirements: 07 reqs 80 to 82 and architecture R-77.
func TestHeldAlternations(t *testing.T) {
	for _, tc := range []struct {
		expr       string
		held, alts int
	}{
		{`abc`, 0, 0},
		{`a*b`, 1, 1},
		{`^[^<>]*$`, 1, 1},
		{`ab|cd|ef`, 2, 2},
		{`a?b*c+`, 2, 3},
		{emailExpr(), 3, 126},
		{hostnamePattern, 2, 124},
		{`(?:(?:a??){240}a)*b`, 241, 241},
		{`^(?:a?|b?)(?:c?|d?)e`, 6, 6},
		{`\b(?:foo|bar)\b`, 1, 1},
	} {
		prog := program(t, noOnePass+tc.expr)
		budget := heldBudget
		if got := heldAlternations(prog, &budget); got != tc.held {
			t.Errorf("heldAlternations(%.40s) = %d, want %d", tc.expr, got, tc.held)
		}
		if got := alternations(prog); got != tc.alts {
			t.Errorf("alternations(%.40s) = %d, want %d", tc.expr, got, tc.alts)
		}
		// Past the budget, every alternation.
		for _, b := range []int{0, 3} {
			if got := heldAlternations(prog, &b); got != tc.alts && tc.alts > 0 {
				t.Errorf("heldAlternations(%.40s) past its budget = %d, want %d", tc.expr, got, tc.alts)
			}
		}
	}
	// The budget is the document's: once spent, later programs are charged
	// every alternation, and a large program is never searched.
	pc := newPatternCosts()
	pc.held = 1
	if c := pc.of(emailPattern); c.held != 126 || pc.held >= 0 {
		t.Errorf("e-mail pattern past the budget: %+v, budget left %d", c, pc.held)
	}
	pc = newPatternCosts()
	if c := pc.of(largestPattern('x', 0)); c.held != 0 || pc.held != heldBudget {
		t.Errorf("large program: %+v, budget left %d", c, pc.held)
	}
}

// BenchmarkHeldAlternations times the search for held alternations of one
// document at its budget: programs whose runs of empty-width steps reach
// hundreds of alternations from every rune, until heldBudget is spent.
func BenchmarkHeldAlternations(b *testing.B) {
	var progs []*syntax.Prog
	for i := range 64 {
		progs = append(progs, program(b, noOnePass+fmt.Sprintf(`(?:(?:a??){240}%c)*b`, 'c'+i)))
	}
	for b.Loop() {
		budget := heldBudget
		for _, p := range progs {
			heldAlternations(p, &budget)
		}
		if budget >= 0 {
			b.Fatal("the programs did not spend the budget")
		}
	}
}

// backtrackJobs replicates Go's backtracker (regexp/backtrack.go) for a
// match without submatches, as the library asks, and returns the most jobs
// its stack holds on input.
func backtrackJobs(prog *syntax.Prog, input string) int {
	type job struct {
		pc  uint32
		pos int
		arg bool
	}
	end := len(input)
	visited := make([]bool, len(prog.Inst)*(end+1))
	visit := func(pc uint32, pos int) bool {
		i := int(pc)*(end+1) + pos
		if visited[i] {
			return false
		}
		visited[i] = true
		return true
	}
	var jobs []job
	most := 0
	push := func(pc uint32, pos int, arg bool) {
		if prog.Inst[pc].Op != syntax.InstFail && (arg || visit(pc, pos)) {
			jobs = append(jobs, job{pc, pos, arg})
			most = max(most, len(jobs))
		}
	}
	at := func(pos int) (rune, int) {
		if pos >= end {
			return -1, 0
		}
		return utf8.DecodeRuneInString(input[pos:])
	}
	before := func(pos int) rune {
		if pos == 0 {
			return -1
		}
		r, _ := utf8.DecodeLastRuneInString(input[:pos])
		return r
	}
	try := func(start int) bool {
		push(uint32(prog.Start), start, false) //nolint:gosec // G115: an instruction index.
		for len(jobs) > 0 {
			j := jobs[len(jobs)-1]
			jobs = jobs[:len(jobs)-1]
			pc, pos, arg := j.pc, j.pos, j.arg
			for first := true; first || visit(pc, pos); first = false {
				in := &prog.Inst[pc]
				switch in.Op {
				case syntax.InstAlt:
					if arg {
						arg, pc = false, in.Arg
					} else {
						push(pc, pos, true)
						pc = in.Out
					}
					continue
				case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
					if r, w := at(pos); w > 0 && in.MatchRune(r) {
						pos, pc = pos+w, in.Out
						continue
					}
				case syntax.InstCapture, syntax.InstNop:
					pc = in.Out
					continue
				case syntax.InstEmptyWidth:
					r, _ := at(pos)
					//nolint:gosec // G115: an empty-width instruction's Arg holds its syntax.EmptyOp, as regexp/syntax writes it.
					if syntax.EmptyOp(in.Arg)&^syntax.EmptyOpContext(before(pos), r) == 0 {
						pc = in.Out
						continue
					}
				case syntax.InstMatch:
					return true
				default:
					panic(in.Op)
				}
				break
			}
		}
		return false
	}
	if prog.StartCond()&syntax.EmptyBeginText != 0 {
		try(0)
		return most
	}
	for pos := 0; pos <= end; {
		if try(pos) {
			break
		}
		_, w := at(pos)
		if w == 0 {
			break
		}
		pos += w
	}
	return most
}

// TestHeldJobsCoverBacktracker checks the job stack bound against a replica
// of Go's backtracker: on crafted and random inputs the stack never holds
// more than held jobs per position and the first, the bound is reached by
// a pattern whose alternations all follow one another, and the e-mail and
// hostname patterns hold about one job per byte, not one per alternation
// (review finding).
//
// Requirements: 07 reqs 80 to 82 and architecture R-77.
func TestHeldJobsCoverBacktracker(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // G404: fixed-seed inputs make the test deterministic; nothing here is secret.
	as := strings.Repeat
	host := hostnamePattern
	for _, tc := range []struct {
		expr, alphabet string
		inputs         []string
	}{
		{emailExpr(), "a-.@!x", []string{as("a", 900), "a@" + as("a", 898), "a@" + as("a-", 449), "a@" + as("a.", 449), as("a", 450) + "@" + as("-", 449)}},
		{host, "a-.1", []string{as("a", 900), as("a.", 450), as("a-", 450), as("a", 60) + ".example.com", as("a", 897) + ".1"}},
		{`^[^<>]*$`, "x<", []string{as("x", 900), as("x", 899) + "<"}},
		{`(?:(?:a??){240}a)*b`, "ab", []string{as("a", 537)}},
		{`a*b`, "ab", []string{as("a", 900)}},
		{`^(?:a?|b?)(?:c?|d?)*e`, "abcde", []string{as("cd", 400)}},
		{`(?i)^[a-z0-9_-]{3,16}$`, "aZ_-0!", []string{as("a", 16), as("a", 17)}},
		{`\b(?:foo|bar)+\b`, "fobar ", []string{as("foo", 200), as("bar", 199) + "x"}},
		{`(x|y|z|w|v)*(xx|yy|zz)*q`, "xyzq", []string{as("xx", 300)}},
	} {
		prog := program(t, noOnePass+tc.expr)
		budget := heldBudget
		held := heldAlternations(prog, &budget)
		inputs := tc.inputs
		for range 200 {
			b := make([]byte, rng.IntN(300))
			for i := range b {
				b[i] = tc.alphabet[rng.IntN(len(tc.alphabet))]
			}
			inputs = append(inputs, string(b))
		}
		most := 0.0
		for _, in := range inputs {
			jobs := backtrackJobs(prog, in)
			if bound := held*(len(in)+1) + 1; jobs > bound {
				t.Errorf("%.40s on %.40q: %d jobs, bound %d", tc.expr, in, jobs, bound)
			}
			most = max(most, float64(jobs-1)/float64(len(in)+1))
		}
		t.Logf("%.40s: held %d of %d alternations, at most %.2f jobs per position", tc.expr, held, alternations(prog), most)
	}
	// The bound is tight: 537 bytes under 241 alternations that follow one
	// another hold one job short of it.
	prog := program(t, noOnePass+`(?:(?:a??){240}a)*b`)
	if got := backtrackJobs(prog, strings.Repeat("a", 537)); got != 241*538 {
		t.Errorf("(a??){240} on 537 bytes held %d jobs, want %d", got, 241*538)
	}
}

// TestHandleBacktrackCharge is the regression test of the review finding on
// the backtracker's job stack: Go's regexp grows it with the input, one
// 16-byte job per alternation and position on the search path, while the
// matcher charge covered only its visited set and the NFA's queues, so a*b
// on a string of 43,688 bytes held 2.2 MB and (?:(?:a??){240}a)*b on 537
// bytes 5.8 MB, charged 126 and 97 KB. Through Factory.Build and Handle at
// the 10 MiB default, failing and valid bodies, a string value, a member
// name under patternProperties and one under propertyNames now hold at their
// peak no more than Handle reserved, but for heapNoise: the live heap at
// each pass's match with the matcher's own peak on top, its state, its
// visited set and the largest pair of job arrays live while the stack grows,
// computed from the match's allocations (heapMeter, matcherPeak; audit class
// GC-08: the sampled heap in use this replaces failed under load).
//
// Requirements: 07 reqs 80 to 82 and architecture R-77.
func TestHandleBacktrackCharge(t *testing.T) {
	requireMeter(t)
	const wide = `(?:(?:a??){240}a)*b`
	as := strings.Repeat
	for _, tc := range []struct {
		name, schema, body, detail string
		// expr and input are the match whose job stack peaks.
		expr, input string
	}{
		{"string", `{"type":"string","pattern":"a*b"}`, `"` + as("a", 43688) + `"`, "/pattern", "a*b", as("a", 43688)},
		{"valid string", `{"type":"string","pattern":"a*b"}`, `"` + as("a", 43687) + `b"`, "", "a*b", as("a", 43687) + "b"},
		{"alternations", `{"type":"string","pattern":"` + wide + `"}`, `"` + as("a", 537) + `"`, "/pattern", wide, as("a", 537)},
		{"member name", `{"patternProperties":{"` + wide + `":false}}`, `{"` + as("a", 530) + `b":1}`, "/patternProperties/" + wide, wide, as("a", 530) + "b"},
		{"property name", `{"propertyNames":{"pattern":"` + wide + `"}}`, `{"` + as("a", 530) + `":1}`, "/propertyNames/pattern", wide, as("a", 530)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFilter(t, tc.schema, 0)
			m := &heapMeter{}
			r, reserved := handlePeak(t, f, tc.schema, tc.body, m)
			if tc.detail == "" {
				if r.Outcome != filter.Continue {
					t.Fatalf("Handle = %+v, want Continue", r)
				}
			} else if got := detailOf(t, r); got != DetailMismatch+" at "+tc.detail {
				t.Fatalf("detail = %q, want the location %s", got, tc.detail)
			}
			t.Logf("reserved %d, live peak %d, of which the matcher %d", reserved, m.peak(), m.matcher)
			// The measurement sees the job stack: the most jobs a replica
			// of the backtracker holds, 16 bytes each.
			if jobs := int64(backtrackJobs(program(t, noOnePass+tc.expr), tc.input)); m.matcher < 16*jobs {
				t.Errorf("the matcher's peak measured %d bytes, less than its %d jobs", m.matcher, jobs)
			}
			if exceeds(m.peak(), reserved) {
				t.Errorf("Handle held %d bytes at its peak, reserved %d", m.peak(), reserved)
			}
		})
	}
}
