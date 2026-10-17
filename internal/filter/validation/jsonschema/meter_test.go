// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"math"
	"regexp/syntax"
	"runtime"
	"runtime/debug"
	"testing"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// The memory measurements of the tests (audit class GC-08, rows VAL-44
// and CX-13). A sample of the heap in use, taken on a timer while the
// library runs, also counts the garbage the collector has not reached yet,
// so under load it exceeded any fixed slack and the tests asserting it
// failed at random (round 9: TestWorkingSetCoversPeak and
// TestHandleUnevaluatedBranches on the Go 1.26 floor and on go1.27.1 beside
// busy loops), while on a quiet machine it hid model errors in either
// direction. The tests measure the live heap instead, at points the code
// path fixes: a heapMeter mark collects the heap fully and reads the bytes
// still reachable, which depend on the code path, not on the collector's
// pacing, so a figure holds no garbage. What the runtime itself allocates
// meanwhile and keeps is not garbage either, and it does depend on the
// machine's load: heapNoise bounds it, and every comparison of a measured
// figure with a charge allows it (exceeds), or allows treeSlack, which is
// larger.

// heapMeter measures the live heap deterministically. A mark runs
// runtime.GC, which returns once the collection and its sweep have
// finished, and reads HeapAlloc, which then counts only the objects
// reachable from the running code (liveHeap), and the meter keeps the
// largest. The marks are taken where the measured code calls the test's
// code: at the probes of a probed compile (heapProbe), at a reservation
// (meterExchange) and at a regexp match (meteredPattern). A pass marks at
// its first probe calls and matches, then at those spread over the rest
// of it however long it is (due), at every probe call at the document's
// root, which ends a location pass and a decision pass that succeeds, and
// at the end of a decision pass that fails (decideProbe). A matcher's own
// peak, inside Go's regexp where no mark can run, is computed exactly
// from the match's allocations instead (matcherPeak) and kept apart.
type heapMeter struct {
	// base is the live heap when the measurement started, live the
	// largest live heap at a mark since.
	base, live uint64
	// matcher is the largest own peak of a metered match's matcher, and
	// matched the largest live heap growth at a metered match with its
	// matcher's own peak on top.
	matcher, matched int64
	// only, when set, is the one position whose probe marks, at every
	// call, for the tests of a shape repeated along branches; the ends of
	// the passes and the matches still mark.
	only string
	// calls and matches count the probe calls and the matches of the
	// current pass, and nextCall and nextMatch are the counts at which each
	// next marks past the head (due); passes counts the passes that ended
	// failing, at the probe of decideProbe, and marks the marks of the
	// measurement.
	calls, matches, nextCall, nextMatch, passes, marks int
	// byPass counts the probe marks and the metered matches of each pass.
	byPass []passMarks
	// at records the live heap growth at each probe mark by the probe's
	// position (endMark for the end of a failing decision pass).
	at map[string][]int64
	// fail is set when the replica of Go's backtracker does not account
	// for a metered match's allocations (matcherPeak).
	fail string
}

// passMarks counts the probe marks and the metered matches of a pass.
type passMarks struct{ probes, matches int }

// probeHead is how many probe calls and matches of a pass mark the meter
// first, and probeGrowth sets the marks past them: one each time the
// pass's count has grown by a quarter since the last (due). The first
// probe calls of a pass are the deepest evaluations of its first path,
// while the unevaluated copies and the errors of every evaluation above
// them are live; the marks past them follow the pass to its end, so that
// the error tree a failing body's pass grows over its values is measured
// where it is largest (a schema's extension keywords run after its
// applicators failed, in either pass, validator.go validate), and its
// last mark falls in its last fifth, before the end mark. The goroutine's
// stack is not heap: TestStackWithinCharge measures it.
const probeHead, probeGrowth = 8, 4

// endMark is the key of at for the end of a failing decision pass.
const endMark = "end"

// due reports whether the n-th probe call or match of a pass marks the
// meter: one of the first probeHead, or the one at which the count has
// grown by a quarter since the last mark past them, *next holding that
// count (0 at the start of a pass).
func due(n int, next *int) bool {
	if n <= probeHead {
		return true
	}
	if n < *next {
		return false
	}
	*next = n + n/probeGrowth
	return true
}

// heapNoise bounds what the runtime and the test framework allocate and
// keep while a measurement runs, besides the measured code: under load the
// scheduler starts a thread, whose m, goroutine records and profiling
// stacks take 5,312 bytes and live on, and the collector's workers cache
// the semaphore records they wait on, 112 bytes each, freed 64 at a time
// when a cache overflows. Beside four busy loops, 9,000 measurements of a
// loop that keeps nothing new, on Go 1.26 and 1.27, saw at most one thread
// started in a measurement (+5.5 KB with its records), at most seven
// records cached (+784 bytes) and -7.2 KB when a cache was freed;
// heapNoise allows three threads and four records at once.
const heapNoise = 16 << 10

// exceeds reports whether a live heap figure the tests measured exceeds
// what is charged for it by more than heapNoise.
func exceeds(measured, charged int64) bool { return measured > charged+heapNoise }

// liveHeap collects the heap fully twice and returns its live bytes: the
// first collection moves what the pools hold to their victim caches, and
// the second drops them, so the live heap holds nothing pooled, which
// under -race the pools also drop at random (sync.Pool.Put): neither the
// decoders the Filter pools, which the reservation does not cover, nor the
// matchers Go's regexp pools, whose peak matcherPeak computes.
func liveHeap() uint64 {
	var m runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// start starts a measurement at the live heap now.
func (m *heapMeter) start() {
	m.calls, m.matches, m.nextCall, m.nextMatch, m.passes, m.marks = 0, 0, 0, 0, 0, 0
	m.matcher, m.matched, m.at, m.fail = 0, 0, nil, ""
	m.byPass = []passMarks{{}}
	m.base = liveHeap()
	m.live = m.base
}

// endPass marks the end of a failing decision pass and starts the next
// pass's schedule.
func (m *heapMeter) endPass() {
	m.mark(endMark)
	m.passes++
	m.calls, m.matches, m.nextCall, m.nextMatch = 0, 0, 0, 0
	m.byPass = append(m.byPass, passMarks{})
}

// mark measures the live heap at the probe at position ptr.
func (m *heapMeter) mark(ptr string) {
	live := liveHeap()
	m.live = max(m.live, live)
	m.marks++
	if m.at == nil {
		m.at = map[string][]int64{}
	}
	m.at[ptr] = append(m.at[ptr], growth(live, m.base))
}

// peak returns the largest live heap growth the measurement marked, a
// metered match's with its matcher's own peak on top.
func (m *heapMeter) peak() int64 { return max(growth(m.live, m.base), m.matched) }

// check fails the test when a metered match could not be measured.
func (m *heapMeter) check(t testing.TB) {
	t.Helper()
	if m.fail != "" {
		t.Error(m.fail)
	}
}

// probeVocabulary is the URL of the test vocabulary of the x-probe and
// x-probe-end keywords.
const probeVocabulary = "https://schema.ruralz.invalid/vocab/probe"

// heapProbe is the x-probe keyword of a probed compile, or with end set
// the x-probe-end keyword of decideProbe. Each evaluation that reaches an
// x-probe counts a probe call of the pass, which marks the meter when it
// is due or at the document's root, or with the meter's only set when it
// is at that position; x-probe-end marks the end of the pass.
//
// The library calls a schema's extension keywords after its applicators
// (validator.go:171-182): in the location pass at every evaluation, and
// in the decision pass, which asks for a boolean result, at every
// evaluation where no keyword checked before them failed (type, const,
// enum, format, $ref and the keywords of the value's type), so also after
// its $dynamicRef, $recursiveRef, not, allOf, anyOf, oneOf or if failed,
// and in every member name's evaluation under propertyNames, which
// validates the name in a pass of its own (validator.go:285-294).
type heapProbe struct {
	m    *heapMeter
	ptr  string
	root bool
	end  bool
}

// Validate marks the meter.
func (p *heapProbe) Validate(*sjs.ValidatorContext, any) {
	m := p.m
	if p.end {
		m.endPass()
		return
	}
	m.calls++
	switch {
	case m.only != "":
		if p.ptr != m.only {
			return
		}
	case !p.root && !due(m.calls, &m.nextCall):
		return
	}
	m.byPass[m.passes].probes++
	m.mark(p.ptr)
}

// decideProbe is the decision pass of a probed compile: as the decision
// pass, the negation of the document, whose anyOf keeps the document's
// errors while it evaluates its second branch, where x-probe-end marks the
// end of the pass with the whole error tree live before not true fails
// the branch, so that the wrapper decides as the decision pass does. The
// library evaluates the second branch only when the document failed, the
// wrapper having no unevaluated keyword (validator.go:580-598).
func decideProbe() map[string]any {
	return map[string]any{"not": map[string]any{"anyOf": []any{
		map[string]any{"$ref": baseURL},
		map[string]any{"not": true, "x-probe-end": true},
	}}}
}

// probedCompiler returns a compiler that holds schema at baseURL, as
// compileLibrary adds it, with the regexp engine of a compile and the
// x-probe keywords' vocabulary, every probe marking m. With probes set,
// every object schema position the rule walk lists gets an x-probe naming
// its position; with matchers set, every pattern matches through a
// meteredPattern. The compile's Schema is not used: its model is the one
// the measured figures are compared with.
func probedCompiler(t testing.TB, schema string, m *heapMeter, probes, matchers bool) *sjs.Compiler {
	t.Helper()
	p, issues := prepare([]byte(schema))
	if len(issues) > 0 {
		t.Fatalf("prepare(%.80q) = %v", schema, issues)
	}
	if probes {
		w := newWalker(p.doc)
		w.walk()
		if len(w.issues) > 0 {
			t.Fatalf("walk(%.80q) = %v", schema, w.issues)
		}
		for _, ptr := range w.positions {
			if obj, ok := w.values[ptr].(map[string]any); ok {
				obj["x-probe"] = ptr
			}
		}
	}
	eng := newRegexpEngine()
	t.Cleanup(eng.freeze)
	c := sjs.NewCompiler()
	c.DefaultDraft(sjs.Draft2020)
	c.UseLoader(refusingLoader{})
	c.UseRegexpEngine(func(expr string) (sjs.Regexp, error) {
		re, err := eng.compile(expr)
		cp, ok := re.(*compiledPattern)
		if err != nil || !ok || !matchers {
			return re, err
		}
		return &meteredPattern{compiledPattern: cp, prog: program(t, noOnePass+expr), m: m}, nil
	})
	c.AssertVocabs()
	c.RegisterVocabulary(&sjs.Vocabulary{
		URL: probeVocabulary,
		Compile: func(_ *sjs.CompilerContext, obj map[string]any) (sjs.SchemaExt, error) {
			if _, ok := obj["x-probe-end"]; ok {
				return &heapProbe{m: m, end: true}, nil
			}
			if ptr, ok := obj["x-probe"].(string); ok {
				return &heapProbe{m: m, ptr: ptr, root: ptr == ""}, nil
			}
			return nil, nil
		},
	})
	if err := c.AddResource(baseURL, p.doc); err != nil {
		t.Fatal(err)
	}
	return c
}

// probedSchemas returns the document's schema and its decision pass
// (decideProbe), compiled by probedCompiler with probes at every position
// and metered patterns.
func probedSchemas(t testing.TB, schema string, m *heapMeter) (root, decide *sjs.Schema) {
	t.Helper()
	c := probedCompiler(t, schema, m, true, true)
	if err := c.AddResource(decideURL, decideProbe()); err != nil {
		t.Fatal(err)
	}
	root, err := c.Compile(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if decide, err = c.Compile(decideURL); err != nil {
		t.Fatal(err)
	}
	return root, decide
}

// probedFilter returns a copy of f whose passes validate through the
// probed schemas of schema, f's document: the copy reserves what f's model
// charges and validates as f does, its probes marking m.
func probedFilter(t testing.TB, f *Filter, schema string, m *heapMeter) *Filter {
	t.Helper()
	s := *f.schema
	s.root, s.decide = probedSchemas(t, schema, m)
	return newFilter(&s, f.status, f.limits)
}

// meterExchange records the largest reservation, as peakExchange does,
// and marks the meter at every reservation, where the decoded values are
// live and the pass about to run has not started.
type meterExchange struct {
	*filtertest.Exchange
	m    *heapMeter
	peak int64
}

// Reserve reserves n and marks the meter.
func (x *meterExchange) Reserve(n int64) error {
	if err := x.Exchange.Reserve(n); err != nil {
		return err
	}
	x.peak = max(x.peak, x.Budget.Reserved)
	x.m.mark("reserve")
	return nil
}

// handlePeak runs Handle in onRequestBody on a JSON body through f's
// probed copy (probedFilter), measuring with m: it returns the result and
// the largest reservation, and m's peak is the live heap growth at
// Handle's peak, marked at each reservation, which holds the decoded
// values, and in each pass at its probes and matches as heapMeter
// schedules them, the end of the decision pass starting the location
// pass's schedule.
func handlePeak(t testing.TB, f *Filter, schema, body string, m *heapMeter) (filter.Result, int64) {
	t.Helper()
	pf := probedFilter(t, f, schema, m)
	x, _ := exchange("application/json", []byte(body))
	mx := &meterExchange{Exchange: x, m: m}
	m.start()
	r := pf.Handle(t.Context(), phase.OnRequestBody, mx)
	m.check(t)
	if x.Budget.Reserved != 0 {
		t.Errorf("reservation left: %d", x.Budget.Reserved)
	}
	return r, mx.peak
}

// meteredPattern is a pattern of a probed compile whose matches mark the
// meter as heapMeter schedules them: the live heap at the match, whose
// collections empty the pools of Go's regexp, so that the match starts
// from nothing as after any collection, and the matcher's own peak on top
// of it (matcherPeak).
type meteredPattern struct {
	*compiledPattern
	prog *syntax.Prog
	m    *heapMeter
}

// matchAttempts is how many times a metered match is measured at most.
const matchAttempts = 3

// MatchString matches s, measuring the match when it is due. The
// collector is off while it matches, so that no collection during the
// match empties the pools again and makes the match allocate a pool's
// per-processor array anew: what the match allocates is then a function
// of the match alone, plus what the runtime allocates meanwhile (a thread
// it starts, heapNoise), never less. So a match whose allocations the
// replica of Go's backtracker does not account for is measured again, up
// to matchAttempts times, and its allocations are the fewest measured.
func (p *meteredPattern) MatchString(s string) bool {
	m := p.m
	if m.matches++; !due(m.matches, &m.nextMatch) {
		return p.compiledPattern.MatchString(s)
	}
	live, allocated := uint64(math.MaxUint64), uint64(math.MaxUint64)
	var ok, measured bool
	var peak int64
	for range matchAttempts {
		live = min(live, liveHeap())
		var ms runtime.MemStats
		gc := debug.SetGCPercent(-1)
		runtime.ReadMemStats(&ms)
		before := ms.TotalAlloc
		ok = p.compiledPattern.MatchString(s)
		runtime.ReadMemStats(&ms)
		debug.SetGCPercent(gc)
		allocated = min(allocated, ms.TotalAlloc-before)
		if peak, measured = matcherPeak(p.prog, s, allocated); measured {
			break
		}
	}
	if !measured {
		m.fail = fmt.Sprintf("the replica of Go's backtracker does not account for the job stack of %.40q on %d bytes", p.String(), len(s))
	}
	m.live = max(m.live, live)
	m.matched = max(m.matched, growth(live, m.base)+peak)
	m.matcher = max(m.matcher, peak)
	m.marks++
	m.byPass[m.passes].matches++
	return ok
}

// The backtracker's own allocations (regexp/backtrack.go): its state, the
// per-processor array of its pool, which a match after a collection
// allocates anew, and the visited set, allocated at its largest whatever
// the input, then the job stack, 16 bytes a job, starting at 256 jobs.
const (
	replayJobBytes = 16
	replayJobs     = 256
	visitedBytes   = maxBacktrackVector / 8
	// bitStateBytes bounds the backtracker's state struct and the pool's
	// bookkeeping besides its per-processor array, poolLocalBytes a
	// processor.
	bitStateBytes, poolLocalBytes = 1 << 10, 128
)

// matcherPeak returns the bytes Go's regexp held at its peak while it
// matched s under prog, starting from empty pools, given what the match
// allocated in all. The NFA simulation allocates its machine, queues and
// threads and drops only the arrays its list of free threads outgrows, so
// what it allocated bounds its peak. The backtracker allocates its state,
// its visited set and a job stack that grows by appending, dropping each
// array it outgrows: its peak is its state and visited set, what it
// allocated besides the job stack, with the largest pair of job arrays
// live at once, an array and the one it grows into while append copies
// it, for the most jobs a replica of Go's backtracker holds on s
// (backtrackJobs). The replica's arrays must account for what the match
// allocated beyond its state and visited set, so it is checked against
// Go's backtracker on every metered match.
func matcherPeak(prog *syntax.Prog, s string, allocated uint64) (peak int64, ok bool) {
	n := len(prog.Inst)
	total := int64(min(allocated, math.MaxInt64))
	if total == 0 || n > maxBacktrackProg || len(s) >= maxBacktrackVector/n {
		return total, true
	}
	all, pair := jobArrays(backtrackJobs(prog, s))
	rest := total - all
	return rest + pair, rest >= visitedBytes && rest <= visitedBytes+bitStateBytes+poolLocalBytes*int64(runtime.GOMAXPROCS(0))
}

// jobArrays replays the growth of the backtracker's job stack to jobs
// jobs by appending, as Go's backtracker grows it: it returns the bytes of
// every array the stack takes and of the largest pair live at once.
func jobArrays(jobs int) (all, pair int64) {
	// A job of regexp/backtrack.go: an instruction, a flag and an input
	// position, 16 bytes without pointers.
	type job [2]uint64
	stack := make([]job, 0, replayJobs)
	all, pair = replayJobs*replayJobBytes, replayJobs*replayJobBytes
	for len(stack) < jobs {
		old := cap(stack)
		stack = append(stack, job{})
		if c := cap(stack); c != old {
			all += int64(c) * replayJobBytes
			pair = max(pair, int64(old+c)*replayJobBytes)
		}
	}
	return all, pair
}

// decisionTree returns the bytes of the error tree the decision pass holds
// for v under schema, the live heap growth at the end of the pass with the
// whole tree live: the decision pass of decideProbe, without probes in the
// document, marks the meter only there. It is 0 for a valid v.
func decisionTree(t testing.TB, schema string, v any) int64 {
	t.Helper()
	m := &heapMeter{}
	c := probedCompiler(t, schema, m, false, false)
	if err := c.AddResource(decideURL, decideProbe()); err != nil {
		t.Fatal(err)
	}
	measure, err := c.Compile(decideURL)
	if err != nil {
		t.Fatal(err)
	}
	m.start()
	_ = measure.Validate(v)
	if end := m.at[endMark]; len(end) > 0 {
		return end[0]
	}
	return 0
}

// growth returns after - before, two heap sizes, as an int64.
func growth(after, before uint64) int64 {
	if after < before {
		return -growth(before, after)
	}
	return int64(min(after-before, math.MaxInt64))
}

// locationTree returns the bytes of the error tree the location pass
// returns for v, which holds every error it collected: the live heap
// growth at one mark, with s live to it, so that a Schema the caller drops
// is not counted out of the tree.
func locationTree(s *Schema, v any) int64 {
	m := &heapMeter{}
	m.start()
	err := s.root.Validate(v)
	m.mark("")
	runtime.KeepAlive(err)
	runtime.KeepAlive(s)
	return m.at[""][0]
}

// treeSlack covers what a measured tree holds besides the errors: the
// frames on the path to the measuring keyword, the collector's own
// bookkeeping and what heapNoise bounds.
const treeSlack = 64 << 10

// TestHeapMeter covers the meter's schedule (audit class GC-08, review
// finding: the meter marked the first probe calls and matches of a whole
// measurement and the document's root alone, so a failing body's decision
// pass, whose tree grows to its end where the root's probe does not run,
// was measured at 0.00 to 0.34 of its charge while it held up to 0.86, and
// Handle's location pass found the head used up by the decision pass).
//
//   - due marks the first probeHead counts of a pass, then each count that
//     has grown by a quarter since the last mark, so the last falls in the
//     pass's last fifth.
//   - Through Handle, a failing body's passes are scheduled apart: each
//     pass of 20 strings failing their pattern meters 13 of its matches,
//     the location pass marks 13 of its probe calls and its root, and the
//     decision pass, where no probe runs, ends at decideProbe's probe.
//   - A pass whose live heap peaks before its end, contains' list of
//     matching items, which the decision pass drops when minContains fails
//     there, is measured with at least four fifths of the list live.
//   - locationTree counts no Schema out of a tree: a Schema of the largest
//     patterns that the caller drops stays live to the mark.
func TestHeapMeter(t *testing.T) {
	var marked []int
	next := 0
	for n := 1; n <= 100; n++ {
		if due(n, &next) {
			marked = append(marked, n)
		}
	}
	want := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 11, 13, 16, 20, 25, 31, 38, 47, 58, 72, 90}
	if fmt.Sprint(marked) != fmt.Sprint(want) {
		t.Errorf("due marks %v of 100 counts, want %v", marked, want)
	}
	for _, total := range []int{9, 10, 100, 20000, 400001} {
		last, next := 0, 0
		for n := 1; n <= total; n++ {
			if due(n, &next) {
				last = n
			}
		}
		if 5*last < 4*total {
			t.Errorf("of %d counts the last marked is %d, before the last fifth", total, last)
		}
	}

	schema := `{"type":"array","items":{"type":"string","pattern":"^a"}}`
	f := buildFilter(t, schema, 0)
	m := &heapMeter{}
	if r, _ := handlePeak(t, f, schema, denseArray(20, `"x"`, 0), m); detailOf(t, r) != DetailMismatch+" at /items/pattern" {
		t.Fatalf("Handle = %+v, want the pattern's location", r)
	}
	if got := fmt.Sprint(m.byPass); m.passes != 1 || got != "[{0 13} {14 13}]" || len(m.at[endMark]) != 1 {
		t.Errorf("the passes marked %s, ended %d times at decideProbe, %d end marks; want [{0 13} {14 13}], once and once", got, m.passes, len(m.at[endMark]))
	}

	const items = 20000
	schema = `{"contains":{"type":"number"},"minContains":100000}`
	s := mustCompile(t, schema)
	v, _ := scanBody(t, s, denseArray(items, `1`, 0))
	m = &heapMeter{}
	_, decide := probedSchemas(t, schema, m)
	m.start()
	err := decide.Validate(v)
	peak := m.peak()
	runtime.KeepAlive(err)
	end := m.at[endMark]
	t.Logf("contains: live peak %d, %d marks, at the end %v", peak, m.marks, end)
	if len(end) != 1 || end[0] > treeSlack {
		t.Fatalf("the decision pass ended with %v live, want once with the list dropped", end)
	}
	if list := int64(items * 8 * 4 / 5); peak < list-heapNoise {
		t.Errorf("the decision pass measured %d bytes at its peak, want at least four fifths of the list, %d", peak, list)
	}

	if tree := locationTree(mustCompile(t, patternMemoryCases()[0].schema), map[string]any{}); tree < -heapNoise {
		t.Errorf("locationTree of a valid body = %d, want no Schema counted out of it", tree)
	}
}
