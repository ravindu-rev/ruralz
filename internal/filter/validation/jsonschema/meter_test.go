// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"regexp/syntax"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
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
// direction. The tests measure the live heap instead, and replay what the
// library holds between two measurements:
//
//   - a heapMeter mark collects the heap fully and reads the bytes still
//     reachable, which depend on the code path, not on the collector's
//     pacing, so a figure holds no garbage;
//   - the marks are taken where the library calls the test's code, as the
//     meter schedules them: at the start of an evaluation that passes its
//     type, const and enum (the entry probe, an asserted format), at the
//     end of one that reaches its extension keywords (the exit probe,
//     x-probe), at the end of each pass (x-probe-end), at a reservation and
//     at a regexp match, whose matcher's own peak is computed from what the
//     match allocated (matcherPeak);
//   - what the library allocates and drops within one evaluation's own
//     keywords, where it calls no code of the test, is replayed from the
//     evaluation's schema and value (copies): a string's copy, the
//     uniqueItems duplicates map with the names it sorts, the names
//     additionalProperties false lists and the errors a contains loop over
//     false drops, and a number check's rationals are bounded
//     (numberCheckBytes; review finding: the duplicates map of a valid
//     uniqueItems array, about 58 bytes per item, and every copy and list
//     in an evaluation of true or in one that fails before its exit probe
//     were never measured, while the sampled heap had counted them). The
//     largest of them or of the matchers is added once to the largest live
//     heap a mark measured, as the model adds its transient copy once to a
//     pass's tree and path (peak);
//   - a list or a map the library grows holds its old array or table
//     besides the new one while it grows, which the marks never see
//     either: TestGrowthCharges replays that growth against the model's
//     charge per item;
//   - what the runtime itself allocates meanwhile and keeps is not garbage
//     either, and it does depend on the machine's load: heapNoise bounds
//     it, and every comparison of a measured figure with a charge allows it
//     (exceeds), or allows treeSlack, which is larger. The meter's own
//     records, its marks and its replays' cache, a few kilobytes, count in
//     its figures too, which only adds to them.
//
// The race detector changes what the library allocates (it gives every
// small allocation a block of its own) and slows every collection, and
// these tests run one goroutine, so the tests that measure memory skip
// under -race (requireMeter), as TestStackWithinCharge does, and run in
// the suites without it (review finding: the meter's collections took the
// package's race run past go test's default 10-minute timeout on a loaded
// 4-CPU machine).
//
// They run on one processor (requireMeter): runtime.GC finishes its sweep
// by yielding to the scheduler after every span, and with several
// processors on a loaded machine every yield contends for the scheduler's
// locks with the processors looking for work, so the collections of the
// marks took most of the suites' time (the suites without -race then timed
// out beside the race run on a loaded 8-CPU machine, and took 9.5 minutes
// alone; four rows of TestWorkingSetCoversPeak took 39 s there on eight
// processors, 22 s on two and 0.8 s on one). The library validates on the
// test's goroutine and keeps nothing per processor, so the figures do not
// depend on the processors, but for the per-processor arrays of Go's
// regexp pools, which matcherPeak allows for at the processors it runs on
// (backtrackState).

// heapMeter measures the live heap deterministically. A mark runs
// runtime.GC, which returns once the collection and its sweep have
// finished, and reads HeapAlloc, which then counts only the objects
// reachable from the running code (liveHeap), and the meter keeps the
// largest. Each pass marks at its first entry probes, exit probes and
// matches, then at those spread over the rest of it however long it is
// (due), at every exit probe at the document's root and at the end of the
// pass. The copies the library makes between two marks are replayed
// (copies), the largest added to the largest mark (peak).
type heapMeter struct {
	// base is the live heap when the measurement started, live the
	// largest live heap at a mark since.
	base, live uint64
	// transient is the largest copy the measurement replayed or measured,
	// and copies the largest of each kind (copyKind).
	transient int64
	copies    [copyKinds]int64
	// matcher is the largest own peak of a metered match's matcher, and
	// matched the largest live heap growth at a metered match with its
	// matcher's own peak on top.
	matcher, matched int64
	// only, when set, is the one position whose exit probe marks, at every
	// call, for the tests of a shape repeated along branches; the entry
	// probes then mark nothing, while the ends of the passes and the
	// matches still mark.
	only string
	// pass is the index of the current pass. entries, exits and matches
	// count the entry probes, the exit probes and the matches of the pass,
	// and probes the probes of both kinds; nextProbe and nextMatch are the
	// counts of probes and of matches at which each next marks past the
	// heads (due, dueProbe). ends counts the passes ended at an end probe
	// and marks the marks of the measurement.
	pass                    int
	entries, exits, matches int
	probes, nextProbe       int
	nextMatch, ends, marks  int
	byPass                  []passMarks
	// at records the live heap growth at each mark by the probe's position
	// (an exit probe), entryKey of it (an entry probe) or endMark.
	at map[string][]int64
	// fail is set when the replica of Go's backtracker does not account
	// for a metered match's allocations (matcherPeak).
	fail string
	// parses is set when the document holds a keyword with which the
	// library parses a number of the value (probedCompiler).
	parses bool
	// jobs holds the most jobs of the backtracker at a metered match of
	// each pattern in the measurement, and duplicates caches the
	// duplicates copies replayed, by array.
	jobs       map[*meteredPattern]int
	duplicates map[uintptr]int64
}

// passMarks counts the marks of a pass at entry probes, at exit probes and
// at metered matches.
type passMarks struct{ entries, exits, matches int }

// copyKind names a kind of copy the meter replays or measures.
type copyKind int

// The kinds of copy.
const (
	// copyString is a string's copy ([]byte(str) in strValidate).
	copyString copyKind = iota
	// copyDuplicates is the uniqueItems check (duplicates, writeHash).
	copyDuplicates
	// copyListed is the names additionalProperties false lists.
	copyListed
	// copyDropped is the errors a contains loop over false drops.
	copyDropped
	// copyNumber is a number check's rationals.
	copyNumber
	// copyMatcher is a matcher's own peak.
	copyMatcher
	// copyWalk is the pointers the location walk builds (walkCopy).
	copyWalk
	// copyKinds is the number of kinds.
	copyKinds
)

// probeHead is how many entry probes, exit probes and matches of a pass
// each mark the meter first, and probeGrowth sets the marks past them: one
// each time the pass's count of probes of both kinds, or of matches, has
// grown by a quarter since the last (due, dueProbe). The first entry
// probes of a pass descend its first path and its first exit probes are
// the deepest evaluations there, while the unevaluated copies and the
// errors of every evaluation above them are live; the marks past them
// follow the pass to its end, so that the error tree a failing body's pass
// grows over its values is measured where it is largest (a schema's
// extension keywords run after its applicators failed, in either pass,
// validator.go validate), and the last falls in its last fifth, before the
// end mark. Each mark collects the heap twice, so the marks of a pass
// grow only with the logarithm of its probes. The goroutine's stack is
// not heap: TestStackWithinCharge measures it.
const probeHead, probeGrowth = 8, 4

// endMark is the key of at for the end of a pass, and entryPrefix starts
// the key of an entry probe (entryKey).
const endMark, entryPrefix = "end", "entry "

// entryKey returns the key of at for the entry probe at position ptr.
func entryKey(ptr string) string { return entryPrefix + ptr }

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

// dueProbe counts a probe call of the pass, the n-th of its kind, and
// reports whether it marks the meter: one of the first probeHead of its
// kind, or due among the probes of both kinds.
func (m *heapMeter) dueProbe(n int) bool {
	m.probes++
	return due(m.probes, &m.nextProbe) || n <= probeHead
}

// dueMatch counts a match of the pass and reports whether it marks the
// meter as due.
func (m *heapMeter) dueMatch() bool {
	m.matches++
	return due(m.matches, &m.nextMatch)
}

// heapNoise bounds what the runtime and the test framework allocate and
// keep while a measurement runs, besides the measured code: under load the
// scheduler starts a thread, whose m, goroutine records and profiling
// stacks take 5,312 bytes and live on, and the collector's workers cache
// the semaphore records they wait on, 112 bytes each, freed 64 at a time
// when a cache overflows. Beside four busy loops, 9,000 measurements of a
// loop that keeps nothing new, on Go 1.26 and 1.27 with every processor,
// saw at most one thread started in a measurement (+5.5 KB with its
// records), at most seven records cached (+784 bytes) and -7.2 KB when a
// cache was freed; on the one processor the tests run on (requireMeter),
// 54,000 such measurements, three processes on each toolchain, saw none.
// heapNoise allows three threads and four records at once.
const heapNoise = 16 << 10

// exceeds reports whether a live heap figure the tests measured exceeds
// what is charged for it by more than heapNoise.
func exceeds(measured, charged int64) bool { return measured > charged+heapNoise }

// requireMeter prepares a test that measures memory: it skips the test
// under -race and runs the rest of it on one processor (see the class
// comment above).
func requireMeter(t testing.TB) {
	t.Helper()
	if raceEnabled {
		t.Skip("the live heap is not measured under -race: the detector changes what the library allocates")
	}
	procs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(procs) })
}

// liveHeap collects the heap fully twice and returns its live bytes: the
// first collection moves what the pools hold to their victim caches, and
// the second drops them, so the live heap holds nothing pooled: neither
// the decoders the Filter pools, which the reservation does not cover, nor
// the matchers Go's regexp pools, whose peak matcherPeak computes.
func liveHeap() uint64 {
	var m runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// start starts a measurement at the live heap now.
func (m *heapMeter) start() {
	m.pass, m.entries, m.exits, m.matches, m.probes = 0, 0, 0, 0, 0
	m.nextProbe, m.nextMatch, m.ends, m.marks = 0, 0, 0, 0
	m.transient, m.copies, m.matcher, m.matched = 0, [copyKinds]int64{}, 0, 0
	m.at, m.fail, m.byPass = map[string][]int64{}, "", []passMarks{{}}
	m.jobs = map[*meteredPattern]int{}
	if m.duplicates == nil {
		m.duplicates = map[uintptr]int64{}
	}
	m.base = liveHeap()
	m.live = m.base
}

// mark measures the live heap at the probe whose key is key.
func (m *heapMeter) mark(key string) {
	live := liveHeap()
	m.live = max(m.live, live)
	m.marks++
	m.at[key] = append(m.at[key], growth(live, m.base))
}

// copy records a copy of n bytes of kind k.
func (m *heapMeter) copy(k copyKind, n int64) {
	m.copies[k] = max(m.copies[k], n)
	m.transient = max(m.transient, n)
}

// endPass marks the end of the current pass and starts the next pass's
// schedule; v is the pass's value (passEnd).
func (m *heapMeter) endPass(v any) {
	m.passEnd(v)
	m.mark(endMark)
	m.ends++
	m.pass++
	m.entries, m.exits, m.matches, m.probes, m.nextProbe, m.nextMatch = 0, 0, 0, 0, 0, 0
	m.byPass = append(m.byPass, passMarks{})
}

// passEnd records, at the end of a pass over v, a number check's
// rationals when the document parses a number of the value and v holds a
// number: the library parses one at a time, in evaluations that may also
// fail on their type, const or enum before any probe of theirs runs.
func (m *heapMeter) passEnd(v any) {
	if m.parses && hasNumber(v) {
		m.copy(copyNumber, numberCheckBytes)
	}
}

// peak returns the largest live heap growth the measurement marked, with
// the largest copy it replayed or measured on top, or a metered match's
// live heap with its matcher's own peak, the larger.
func (m *heapMeter) peak() int64 {
	return max(addCap(growth(m.live, m.base), m.transient), m.matched)
}

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

// locateURL names the location pass of a probed compile (locateProbe).
const locateURL = "https://schema.ruralz.invalid/locate.json"

// The values of x-probe-end: the end of the decision pass and of the
// location pass.
const decisionEnd, locationEnd = "decision", "location"

// probeSite is a position of a probed compile: its probes and what the
// library's own keywords there copy (copies).
type probeSite struct {
	m   *heapMeter
	ptr string
	// trivial is set for true and {}, which the library evaluates as
	// true, running nothing.
	trivial bool
	// unique is set by uniqueItems true; closed by additionalProperties
	// false, which lists the names that neither declared (properties) nor
	// patterns (patternProperties) cover; dropping by contains false with
	// a minContains of 0, whose loop collects an error per item and drops
	// them.
	unique, closed, dropping bool
	declared                 map[string]bool
	patterns                 []*regexp.Regexp
}

// Validate is the entry probe: the asserted format of the site, which the
// library checks at the start of every evaluation of it that passes its
// type, const and enum, before its references and the keywords of the
// value's type, in either pass and whatever fails after (validator.go
// validate). It replays the copies of the evaluation and marks the meter
// when due.
func (s *probeSite) Validate(v any) error {
	m := s.m
	s.copies(v)
	m.entries++
	if !m.dueProbe(m.entries) || m.only != "" {
		return nil
	}
	m.byPass[m.pass].entries++
	m.mark(entryKey(s.ptr))
	return nil
}

// copies replays what the keywords of the site copy and drop at v before
// any probe runs again (validator.go strValidate, arrValidate,
// objValidate): the string's copy, which every evaluation of a schema
// makes but true's, the uniqueItems check and the names additionalProperties
// false lists.
func (s *probeSite) copies(v any) {
	if s.trivial {
		return
	}
	m := s.m
	switch v := v.(type) {
	case string:
		m.copy(copyString, stringCopy(len(v)))
	case []any:
		if s.unique {
			m.copy(copyDuplicates, m.duplicatesCopy(v))
		}
	case map[string]any:
		if s.closed {
			m.copy(copyListed, appendPair[string](s.undeclared(v)))
		}
	}
}

// undeclared returns how many members of obj neither declared nor
// patterns cover.
func (s *probeSite) undeclared(obj map[string]any) int {
	n := 0
	for name := range obj {
		if s.declared[name] || slices.ContainsFunc(s.patterns, func(re *regexp.Regexp) bool { return re.MatchString(name) }) {
			continue
		}
		n++
	}
	return n
}

// heapProbe is the exit probe of a site, the x-probe keyword, or with end
// set the x-probe-end keyword of a pass's end (decideProbe, locateProbe).
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
	site *probeSite
	m    *heapMeter
	end  string
}

// Validate marks the meter.
func (p *heapProbe) Validate(ctx *sjs.ValidatorContext, v any) {
	m := p.m
	switch p.end {
	case decisionEnd, locationEnd:
		m.endPass(v)
		return
	}
	s := p.site
	if arr, ok := v.([]any); ok && s.dropping {
		m.copy(copyDropped, droppedErrors(len(arr), len(ctx.ValueLocation())))
	}
	m.exits++
	root := s.ptr == ""
	if root {
		m.passEnd(v)
	}
	switch {
	case m.only != "":
		if s.ptr != m.only {
			return
		}
	case !m.dueProbe(m.exits) && !root:
		return
	}
	m.byPass[m.pass].exits++
	m.mark(s.ptr)
}

// decideProbe is the decision pass of a probed compile: as the decision
// pass, the negation of the document, whose anyOf keeps the document's
// errors while it evaluates its second branch, where x-probe-end marks the
// end of the pass with the whole error tree live before not true fails
// the branch, so that the wrapper decides as the decision pass does. The
// library evaluates the second branch only when the document failed, the
// wrapper having no unevaluated keyword (validator.go:580-598); a valid
// document ends at its root's exit probe.
func decideProbe() map[string]any {
	return map[string]any{"not": map[string]any{"anyOf": []any{
		map[string]any{"$ref": baseURL},
		map[string]any{"not": true, "x-probe-end": decisionEnd},
	}}}
}

// locateProbe is the location pass of a probed compile: a reference to
// the document, whose error wraps the document's errors and whose
// x-probe-end, which the location pass reaches at every evaluation, marks
// the end of the pass with the whole error tree live, also when the
// document fails before any probe of its own runs. The wrapping error has
// causes, so the location walk visits the document's leaves alone and
// reports what the document's root reports (leaves). The wrapper's own
// evaluation and error, a few hundred bytes, count in the pass's figures,
// which only adds to them.
func locateProbe() map[string]any {
	return map[string]any{"$ref": baseURL, "x-probe-end": locationEnd}
}

// probedCompiler returns a compiler that holds schema at baseURL, as
// compileLibrary adds it, with the regexp engine of a compile and the
// probe vocabulary, every probe marking m. With probes set, every schema
// position the rule walk lists gets an exit probe (x-probe) and an entry
// probe (an asserted format naming it, which replaces the position's
// format, an annotation in the package's compile), true becoming an
// object of the two probes, which the library evaluates as it evaluates
// true (compiler.go:246-259 gives both the same evaluated items and
// members), but where additionalProperties or additionalItems hold it,
// which the library keeps as a flag and evaluates no schema for; false
// stays, holding its one error and running nothing (an object failing in
// its place would change the error's keyword and so the location the
// pass reports). An object of probes copies a string and loops over an
// object's members where true does neither: garbage, which no mark
// counts, and its site replays no copy (trivial). With matchers set,
// every pattern matches through a meteredPattern. The compile's Schema is
// not used: its model is the one the measured figures are compared with.
func probedCompiler(t testing.TB, schema string, m *heapMeter, probes, matchers bool) *sjs.Compiler {
	t.Helper()
	p, issues := prepare([]byte(schema))
	if len(issues) > 0 {
		t.Fatalf("prepare(%.80q) = %v", schema, issues)
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
	sites := map[string]*probeSite{}
	if probes {
		c.AssertFormat()
		sites = probeSites(t, p, m)
		for _, s := range sites {
			c.RegisterFormat(&sjs.Format{Name: "x-probe " + s.ptr, Validate: s.Validate})
		}
	}
	c.RegisterVocabulary(&sjs.Vocabulary{
		URL: probeVocabulary,
		Compile: func(_ *sjs.CompilerContext, obj map[string]any) (sjs.SchemaExt, error) {
			if end, ok := obj["x-probe-end"].(string); ok {
				return &heapProbe{m: m, end: end}, nil
			}
			if ptr, ok := obj["x-probe"].(string); ok && sites[ptr] != nil {
				return &heapProbe{m: m, site: sites[ptr]}, nil
			}
			return nil, nil
		},
	})
	if err := c.AddResource(baseURL, p.doc); err != nil {
		t.Fatal(err)
	}
	return c
}

// probeSites walks the document of p and gives every schema position its
// probes (probedCompiler), returning the sites by position; m.parses is
// set when a position holds a keyword with which the library parses a
// number of the value.
func probeSites(t testing.TB, p *prepared, m *heapMeter) map[string]*probeSite {
	t.Helper()
	w := newWalker(p.doc)
	w.walk()
	if len(w.issues) > 0 {
		t.Fatalf("walk = %v", w.issues)
	}
	sites := map[string]*probeSite{}
	for _, ptr := range w.positions {
		var obj map[string]any
		s := &probeSite{m: m, ptr: ptr}
		switch v := w.values[ptr].(type) {
		case map[string]any:
			obj = v
			s.read(t, v)
		case bool:
			if !v || flagPosition(w, ptr) {
				continue
			}
			obj, s.trivial = map[string]any{}, true
			if !replaceTrue(p, ptr, obj) {
				continue
			}
		default:
			continue
		}
		// An alias of a position already probed (non-canonical indexes).
		if _, done := obj["x-probe"]; done {
			continue
		}
		obj["x-probe"], obj["format"] = ptr, "x-probe "+ptr
		sites[ptr] = s
		m.parses = m.parses || parsesNumbers(obj)
	}
	return sites
}

// read sets what the keywords of obj, a schema object, copy.
func (s *probeSite) read(t testing.TB, obj map[string]any) {
	t.Helper()
	s.trivial = len(obj) == 0
	s.unique = obj["uniqueItems"] == true
	if obj["additionalProperties"] == false {
		s.closed, s.declared = true, map[string]bool{}
		if props, ok := obj["properties"].(map[string]any); ok {
			for name := range props {
				s.declared[name] = true
			}
		}
		if pats, ok := obj["patternProperties"].(map[string]any); ok {
			for expr := range pats {
				re, err := regexp.Compile(expr)
				if err != nil {
					t.Fatalf("patternProperties %q: %v", expr, err)
				}
				s.patterns = append(s.patterns, re)
			}
		}
	}
	if least, ok := obj["minContains"].(json.Number); ok && obj["contains"] == false {
		r, ok := new(big.Rat).SetString(string(least))
		s.dropping = ok && r.Sign() == 0
	}
}

// flagPosition reports whether ptr is additionalProperties or
// additionalItems of a schema, which the library keeps as a flag when it
// is a boolean (objcompiler.go enqueueAdditional).
func flagPosition(w *walker, ptr string) bool {
	i := strings.LastIndexByte(ptr, '/')
	if i < 0 {
		return false
	}
	_, parent := w.values[ptr[:i]].(map[string]any)
	tok := ptr[i+1:]
	return parent && (tok == "additionalProperties" || tok == "additionalItems")
}

// replaceTrue replaces the true at ptr in the document of p with obj and
// reports whether it did: an alias of a position replaced before holds an
// object already.
func replaceTrue(p *prepared, ptr string, obj map[string]any) bool {
	if ptr == "" {
		if p.doc != true {
			return false
		}
		p.doc = obj
		return true
	}
	i := strings.LastIndexByte(ptr, '/')
	parent, ok := lookup(p.doc, ptr[:i])
	tok, okTok := unescapeToken(ptr[i+1:])
	if !ok || !okTok {
		return false
	}
	switch c := parent.(type) {
	case map[string]any:
		if c[tok] == true {
			c[tok] = obj
			return true
		}
	case []any:
		if j, err := strconv.Atoi(tok); err == nil && j >= 0 && j < len(c) && c[j] == true {
			c[j] = obj
			return true
		}
	}
	return false
}

// parsesNumbers reports whether the schema object obj holds a keyword
// with which the library parses a number of the value with math/big: a
// numeric bound or multipleOf (numValidate), an integer type (isInteger),
// a const or enum holding a number (equals) and uniqueItems (equals,
// writeHash).
func parsesNumbers(obj map[string]any) bool {
	for _, kw := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"} {
		if _, ok := obj[kw]; ok {
			return true
		}
	}
	switch t := obj["type"].(type) {
	case string:
		if t == "integer" {
			return true
		}
	case []any:
		if slices.Contains(t, any("integer")) {
			return true
		}
	}
	return obj["uniqueItems"] == true || hasNumber(obj["const"]) || hasNumber(obj["enum"])
}

// hasNumber reports whether v holds a number at any depth.
func hasNumber(v any) bool {
	switch t := v.(type) {
	case json.Number:
		return true
	case []any:
		return slices.ContainsFunc(t, hasNumber)
	case map[string]any:
		for _, e := range t {
			if hasNumber(e) {
				return true
			}
		}
	}
	return false
}

// probedSchemas returns the document's location pass (locateProbe) and
// decision pass (decideProbe), compiled by probedCompiler with probes at
// every position and metered patterns.
func probedSchemas(t testing.TB, schema string, m *heapMeter) (locate, decide *sjs.Schema) {
	t.Helper()
	c := probedCompiler(t, schema, m, true, true)
	for u, doc := range map[string]any{decideURL: decideProbe(), locateURL: locateProbe()} {
		if err := c.AddResource(u, doc); err != nil {
			t.Fatal(err)
		}
	}
	locate, err := c.Compile(locateURL)
	if err != nil {
		t.Fatal(err)
	}
	if decide, err = c.Compile(decideURL); err != nil {
		t.Fatal(err)
	}
	return locate, decide
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
// values, and in each pass as heapMeter schedules it, with the copies the
// passes made, and the location walk's pointers, on top.
func handlePeak(t testing.TB, f *Filter, schema, body string, m *heapMeter) (filter.Result, int64) {
	t.Helper()
	x, _ := exchange("application/json", []byte(body))
	pf := probedFilter(t, f, schema, m)
	mx := &meterExchange{Exchange: x, m: m}
	m.start()
	r := pf.Handle(t.Context(), phase.OnRequestBody, mx)
	m.check(t)
	if x.Budget.Reserved != 0 {
		t.Errorf("reservation left: %d", x.Budget.Reserved)
	}
	// The location pass ran: its walk's pointers, replayed on the tree of
	// f's own location pass.
	if m.ends == 2 {
		v, _, err := f.decode([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		var verr *sjs.ValidationError
		if errors.As(f.schema.root.Validate(v), &verr) {
			m.copy(copyWalk, walkCopy(verr))
		}
	}
	return r, mx.peak
}

// meteredPattern is a pattern of a probed compile whose matches mark the
// meter: the live heap at the match, whose collections empty the pools of
// Go's regexp, so that the match starts from nothing as after any
// collection, and the matcher's own peak on top of it (matcherPeak). A
// match is metered when due, when it runs on the NFA simulation, and when
// the backtracker's job stack, replayed (backtrackJobs), holds more jobs
// than at every metered match of its pattern in the measurement, so that
// the largest matcher is measured whichever match holds it (review
// finding: marks at the head of a pass and spread over it alone missed a
// longer input elsewhere): the backtracker's own peak grows with its job
// stack alone, its visited set being allocated at its largest whatever
// the input, and Go's regexp runs the NFA simulation only on inputs of at
// least maxBacktrackVector bits divided by the program's instructions,
// few in a body, whose machine and threads it allocates by the program.
type meteredPattern struct {
	*compiledPattern
	prog *syntax.Prog
	m    *heapMeter
}

// matchAttempts is how many times a metered match is measured at most.
const matchAttempts = 3

// MatchString matches s, measuring the match when it is metered. The
// collector is off while it matches, so that no collection during the
// match empties the pools again and makes the match allocate a pool's
// per-processor array anew: what the match allocates is then a function
// of the match alone, plus what the runtime allocates meanwhile (a thread
// it starts, heapNoise), never less. So a match whose allocations the
// replica of Go's backtracker does not account for is measured again, up
// to matchAttempts times, and its allocations are the fewest measured.
func (p *meteredPattern) MatchString(s string) bool {
	m := p.m
	metered := m.dueMatch()
	jobs := -1
	if n := len(p.prog.Inst); n <= maxBacktrackProg && len(s) < maxBacktrackVector/n {
		jobs = backtrackJobs(p.prog, s)
	}
	if most, seen := m.jobs[p]; !metered && jobs >= 0 && seen && jobs <= most {
		return p.compiledPattern.MatchString(s)
	}
	m.jobs[p] = max(m.jobs[p], jobs)
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
	m.copy(copyMatcher, peak)
	m.marks++
	m.byPass[m.pass].matches++
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
	return rest + pair, rest >= visitedBytes && rest <= backtrackState()
}

// backtrackState bounds what the backtracker allocates besides its job
// stack: its visited set, its state and its pool's per-processor arrays.
func backtrackState() int64 {
	return visitedBytes + bitStateBytes + poolLocalBytes*int64(runtime.GOMAXPROCS(0))
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

// appendPair replays a slice of E grown to n items by appending, as the
// library grows its lists, and returns the bytes of the largest pair of
// arrays live at once: an array and the one append copies it into, each
// with its header when E holds pointers (arrayBytes).
func appendPair[E any](n int) int64 {
	var s []E
	var zero E
	var pair int64
	for len(s) < n {
		old := cap(s)
		s = append(s, zero)
		if c := cap(s); c != old {
			pair = max(pair, arrayBytes[E](old)+arrayBytes[E](c))
		}
	}
	return pair
}

// arrayBytes returns what an array of n items of E takes: its allocation
// size (allocSize), with the header the allocator puts in front of it past
// 512 bytes when E holds pointers (pointerSlice).
func arrayBytes[E any](n int) int64 {
	typ := reflect.TypeFor[E]()
	if typ.Kind() == reflect.String || typ.Kind() == reflect.Pointer {
		return pointerSlice(n, int64(min(typ.Size(), math.MaxInt64)))
	}
	return allocSize(int64(n) * int64(min(typ.Size(), math.MaxInt64)))
}

// allocSize returns what the allocator takes for an object of n bytes: its
// size class, or whole pages past 32 KiB, as append rounds a byte slice up
// (runtime roundupsize).
func allocSize(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return int64(cap(append([]byte(nil), make([]byte, n)...)))
}

// stringCopy returns what the library's copy of a string of n bytes
// takes: none for an empty string, a share of a 16-byte tiny block, at
// most the block, below 16 bytes, and the allocation size of the bytes
// past them, as append rounds a byte slice up (a string's copy and a byte
// slice's growth both round with runtime roundupsize).
func stringCopy(n int) int64 {
	switch {
	case n == 0:
		return 0
	case n < tinyBlockBytes:
		return tinyBlockBytes
	}
	return allocSize(int64(n))
}

// numberCheckBytes bounds what one number check of the library holds at
// once for a number within the number bounds against a schema's number
// within them: the value's rational and the quotient multipleOf divides,
// or the two rationals of a comparison (TestNumberCheckMemory).
const numberCheckBytes = 16 << 10

// duplicatesCopy returns what the library's uniqueItems check of arr holds
// at its peak (util.go duplicates and writeHash): with numbers among the
// items, a number's rationals, one at a time, as it compares or hashes
// them; past pairwiseItems items, its map of the hashes of the items
// before the first duplicate to their indexes (mapCopy) and the sorted
// names of the objects it hashes, along the deepest path of nested ones
// (sortedNames). It is replayed once per array.
func (m *heapMeter) duplicatesCopy(arr []any) int64 {
	var n int64
	if len(arr) > 1 && hasNumber(arr) {
		n = numberCheckBytes
	}
	if len(arr) <= pairwiseItems {
		return n
	}
	key := reflect.ValueOf(arr).Pointer()
	if c, ok := m.duplicates[key]; ok {
		return c
	}
	names := int64(0)
	for _, item := range arr {
		names = max(names, sortedNames(item))
	}
	n += mapCopy(hashClasses(arr)) + names
	m.duplicates[key] = n
	return n
}

// hashClasses returns how many of the items of arr before the first that
// equals an earlier one hash alike, in classes: writeHash feeds equal
// values and the values whose bytes concatenate alike (a number and its
// negation, which it hashes without the sign) the same bytes, which the
// library's map keys by their hash, and equals finds the first duplicate.
func hashClasses(arr []any) []int {
	class, seen := map[string]int{}, map[string]bool{}
	var counts []int
	for _, item := range arr {
		key := canonical(item)
		if seen[key] {
			break
		}
		seen[key] = true
		var b strings.Builder
		hashBytes(&b, item)
		if i, ok := class[b.String()]; ok {
			counts[i]++
			continue
		}
		class[b.String()] = len(counts)
		counts = append(counts, 1)
	}
	return counts
}

// hashBytes writes the bytes writeHash feeds its hash for v (util.go).
func hashBytes(b *strings.Builder, v any) {
	switch t := v.(type) {
	case map[string]any:
		b.WriteByte(0)
		for _, k := range slices.Sorted(mapKeys(t)) {
			hashBytes(b, k)
			hashBytes(b, t[k])
		}
	case []any:
		b.WriteByte(1)
		for _, e := range t {
			hashBytes(b, e)
		}
	case nil:
		b.WriteByte(2)
	case bool:
		b.WriteByte(3)
		if t {
			b.WriteByte(1)
		} else {
			b.WriteByte(0)
		}
	case string:
		b.WriteByte(4)
		b.WriteString(t)
	case json.Number:
		b.WriteByte(5)
		if r, ok := new(big.Rat).SetString(string(t)); ok {
			b.Write(r.Num().Bytes())
			b.Write(r.Denom().Bytes())
		}
	}
}

// mapKeys returns the keys of m as an iterator's source.
func mapKeys(m map[string]any) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// canonical returns an encoding of v that two values share when equals
// finds them equal: numbers by their rational value.
func canonical(v any) string {
	switch t := v.(type) {
	case map[string]any:
		var b strings.Builder
		b.WriteByte('{')
		for _, k := range slices.Sorted(mapKeys(t)) {
			b.WriteString(strconv.Quote(k) + ":" + canonical(t[k]) + ",")
		}
		return b.String() + "}"
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for _, e := range t {
			b.WriteString(canonical(e) + ",")
		}
		return b.String() + "]"
	case json.Number:
		if r, ok := new(big.Rat).SetString(string(t)); ok {
			return "#" + r.RatString()
		}
	case string:
		return strconv.Quote(t)
	}
	return fmt.Sprint(v)
}

// mapCopy returns what the library's duplicates map holds at its peak
// with counts[i] indexes for the i-th hash, keyed by uint64 hashes to
// slices grown by appending, as duplicates builds it: the live heap a
// replica of that map adds, with the largest table it replaces while it
// grows on top (dupTable). The replica's tables, whose count follows the
// map's random hash seed, differ from the library's by about 1% past a
// thousand keys (TestDuplicatesReplica); seedApart allows for it.
func mapCopy(counts []int) int64 {
	base := liveHeap()
	replica := make(map[uint64][]int)
	for i, n := range counts {
		for j := range n {
			replica[uint64(i)] = append(replica[uint64(i)], j)
		}
	}
	live := growth(liveHeap(), base)
	runtime.KeepAlive(replica)
	return live + live/seedApart + dupTable(len(counts))
}

// seedApart sets how far a duplicates map may hold more than its replica,
// a 1/seedApart part of it.
const seedApart = 16

// dupTable returns a bound of the largest table a duplicates map of keys
// keys replaces while it grows: the map holding min(keys, 896) keys, which
// is its own table, or a full one of 1,024 slots, the largest Go's swiss
// tables grow to before they split (internal/runtime/maps).
func dupTable(keys int) int64 {
	keys = min(keys, 896)
	base := liveHeap()
	replica := make(map[uint64][]int)
	for i := range keys {
		replica[uint64(i)] = append(replica[uint64(i)], i)
	}
	live := growth(liveHeap(), base)
	runtime.KeepAlive(replica)
	return live
}

// sortedNames returns the bytes of the sorted name slices writeHash holds
// at once hashing v: an object's, then the deepest of its members'.
func sortedNames(v any) int64 {
	switch t := v.(type) {
	case map[string]any:
		var below int64
		for _, e := range t {
			below = max(below, sortedNames(e))
		}
		return pointerSlice(len(t), 16) + below
	case []any:
		var below int64
		for _, e := range t {
			below = max(below, sortedNames(e))
		}
		return below
	}
	return 0
}

// walkCopy returns what the location walk over the error tree verr holds
// at once besides the tree (locationOf): for a leaf it builds a pointer
// for, what keywordAt allocates, all of it counted as live at once, with
// the largest pointer it may hold as the best leaf's on top. The walk runs
// after the location pass, where no probe runs; the package's own
// functions build the pointers, measured once per location and keyword.
func walkCopy(verr *sjs.ValidationError) int64 {
	built := map[string]bool{}
	var most, best int64
	leaves(verr, func(l *sjs.ValidationError) bool {
		loc, kw, ok := keywordParts(l)
		key := loc + "\x00" + strings.Join(kw, "\x00")
		if !ok || built[key] {
			return true
		}
		built[key] = true
		var p string
		var a, b runtime.MemStats
		gc := debug.SetGCPercent(-1)
		runtime.ReadMemStats(&a)
		p, _ = keywordAt(loc, kw)
		runtime.ReadMemStats(&b)
		debug.SetGCPercent(gc)
		most = max(most, growth(b.TotalAlloc, a.TotalAlloc))
		best = max(best, stringCopy(len(p)))
		return true
	})
	return most + best
}

// droppedErrors returns what the errors of a contains loop over false hold
// at its end for n items at depth: the error of every item, its instance
// location in the location pass, and the list of their pointers.
func droppedErrors(n, depth int) int64 {
	return int64(n)*(validationErrorBytes()+pointerSlice(depth+1, 16)) + appendPair[*sjs.ValidationError](n)
}

// validationErrorBytes returns the size of the library's ValidationError:
// a schema URL, an instance location, an error kind and causes.
func validationErrorBytes() int64 {
	return int64(min(reflect.TypeFor[sjs.ValidationError]().Size(), math.MaxInt64))
}

// pointerSlice returns what a slice of n items of size bytes holding
// pointers takes, made at that length (make, slices.Clone): its bytes with
// the header the allocator puts in front of them past 512 bytes, at their
// allocation size (allocSize).
func pointerSlice(n int, size int64) int64 {
	bytes := int64(n) * size
	if bytes > 512 {
		bytes += mallocHeaderBytes
	}
	return allocSize(bytes)
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

// TestHeapMeter covers the meter (audit class GC-08, review findings: the
// meter marked the first probe calls and matches of a whole measurement
// and the document's root alone, so a failing body's decision pass, whose
// tree grows to its end where the root's probe does not run, was measured
// at 0.00 to 0.34 of its charge while it held up to 0.86, and Handle's
// location pass found the head used up by the decision pass; then the
// copies made between two probes, the evaluations of true and those that
// fail before their exit probe were never measured).
//
//   - due marks the first probeHead counts of a pass, then each count that
//     has grown by a quarter since the last mark, so the last falls in the
//     pass's last fifth.
//   - Through Handle, a failing body's passes are scheduled apart: each
//     pass of 20 strings failing their pattern marks the first of its
//     entry probes, exit probes and matches and those spread over it, and
//     each ends at its end probe.
//   - A pass whose live heap peaks before its end, contains' list of
//     matching items, which the decision pass drops when minContains fails
//     there, is measured with at least four fifths of the list live, also
//     where contains is true, which gets probes of its own.
//   - An evaluation that fails before its exit probe, in a failing branch
//     of a valid body, marks at its entry probe and replays its copies,
//     and a location pass whose document fails before any probe of its own
//     marks at its end probe.
//   - A match whose job stack is larger than at every metered match of its
//     pattern is metered wherever it falls in the pass, and so is every
//     match on the NFA simulation.
//   - A string's copy is replayed for every schema but true and {}, which
//     the library evaluates without copying; a number check's rationals
//     for a body holding a number under a keyword that parses it.
//   - locationTree counts no Schema out of a tree: a Schema of the largest
//     patterns that the caller drops stays live to the mark.
func TestHeapMeter(t *testing.T) {
	requireMeter(t)
	t.Run("schedule", func(t *testing.T) {
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
	})
	t.Run("passes apart", func(t *testing.T) {
		schema := `{"type":"array","items":{"type":"string","pattern":"^a"}}`
		m := &heapMeter{}
		if r, _ := handlePeak(t, buildFilter(t, schema, 0), schema, denseArray(20, `"x"`, 0), m); detailOf(t, r) != DetailMismatch+" at /items/pattern" {
			t.Fatalf("Handle = %+v, want the pattern's location", r)
		}
		// Each pass: 21 entry probes (the root and its 20 items); the
		// decision pass, failing at every item, no exit probe, so 13 of its
		// 21 probes due; the location pass 21 exit probes too, the first 8
		// of each kind and those due among its 42 probes marking, 11 and 11
		// with the root's exit; 20 matches, of which 13 due, the
		// backtracker's job stack being the same at each.
		if got := fmt.Sprint(m.byPass); m.ends != 2 || got != "[{13 0 13} {11 11 13} {0 0 0}]" || len(m.at[endMark]) != 2 {
			t.Errorf("the passes marked %s, ended %d times, %d end marks; want [{13 0 13} {11 11 13} {0 0 0}], twice and twice", got, m.ends, len(m.at[endMark]))
		}
	})
	t.Run("a list in the middle of a pass", func(t *testing.T) {
		const items = 20000
		for _, schema := range []string{`{"contains":{"type":"number"},"minContains":100000}`, `{"contains":true,"minContains":100000}`} {
			s := mustCompile(t, schema)
			v, _ := scanBody(t, s, denseArray(items, `1`, 0))
			m := &heapMeter{}
			_, decide := probedSchemas(t, schema, m)
			m.start()
			err := decide.Validate(v)
			runtime.KeepAlive(err)
			end, at := m.at[endMark], slices.Max(append(append(m.at["/contains"], m.at[entryKey("/contains")]...), 0))
			t.Logf("%s: %d marks, at contains up to %d, at the end %v", schema, m.marks, at, end)
			if len(end) != 1 || end[0] > treeSlack {
				t.Errorf("%s: the decision pass ended with %v live, want once with the list dropped", schema, end)
			}
			if list := int64(items * 8 * 4 / 5); at < list-heapNoise {
				t.Errorf("%s: the probes in the contains loop measured at most %d bytes, want at least four fifths of the list, %d", schema, at, list)
			}
		}
	})
	t.Run("a failing branch", func(t *testing.T) {
		schema := `{"anyOf":[{"uniqueItems":true,"maxItems":1},true]}`
		v, _ := scanBody(t, mustCompile(t, schema), distinct(5000, "%d"))
		m := &heapMeter{}
		_, decide := probedSchemas(t, schema, m)
		m.start()
		if err := decide.Validate(v); err == nil {
			t.Fatal("the body is not valid")
		}
		if len(m.at[entryKey("/anyOf/0")]) != 1 || len(m.at["/anyOf/0"]) != 0 || m.copies[copyDuplicates] < 5000*40 {
			t.Errorf("the failing branch marked %v at its start and %v at its end, copies %v; want once, never, and its duplicates map", m.at[entryKey("/anyOf/0")], m.at["/anyOf/0"], m.copies)
		}
		schema = `{"type":"array"}`
		m = &heapMeter{}
		locate, _ := probedSchemas(t, schema, m)
		m.start()
		if err := locate.Validate(map[string]any{}); err == nil {
			t.Fatal("the body is valid")
		}
		if m.marks != 1 || len(m.at[endMark]) != 1 {
			t.Errorf("a location pass failing at the root's type marked %d times, %v at its end; want once there", m.marks, m.at[endMark])
		}
	})
	t.Run("a larger job stack past the head", func(t *testing.T) {
		// The 30th match, past the head and not due, holds the most jobs.
		const long = 4000
		schema := `{"items":{"pattern":"a*b"}}`
		body := strings.TrimSuffix(denseArray(29, `"ab"`, 0), "]") + `,"` + strings.Repeat("a", long) + `"]`
		v, _ := scanBody(t, mustCompile(t, schema), body)
		m := &heapMeter{}
		_, decide := probedSchemas(t, schema, m)
		m.start()
		_ = decide.Validate(v)
		m.check(t)
		jobs := backtrackJobs(program(t, noOnePass+"a*b"), strings.Repeat("a", long))
		// The 14 due, the 25th the last of them, and the 30th.
		if m.matcher < int64(16*jobs) || m.byPass[0].matches != 15 {
			t.Errorf("the matcher's peak measured %d bytes in %d metered matches, want its %d jobs measured at the 30th, 15 metered", m.matcher, m.byPass[0].matches, jobs)
		}
	})
	t.Run("NFA matches past the head", func(t *testing.T) {
		// Inputs of 60,000 bytes, past the backtracker's 52,428 for the
		// program of ^a+$: every match runs on the NFA simulation.
		schema := `{"items":{"pattern":"^a+$"}}`
		v, _ := scanBody(t, mustCompile(t, schema), denseArray(30, `"`+strings.Repeat("a", 60000)+`"`, 0))
		m := &heapMeter{}
		_, decide := probedSchemas(t, schema, m)
		m.start()
		_ = decide.Validate(v)
		m.check(t)
		if m.byPass[0].matches != 30 {
			t.Errorf("%d of 30 matches on the NFA simulation metered, want all", m.byPass[0].matches)
		}
	})
	t.Run("copies", func(t *testing.T) {
		const n = 1 << 20
		str := strings.Repeat("s", n)
		copied := int64(cap(append([]byte(nil), str...)))
		for _, tc := range []struct {
			schema string
			v      any
			kind   copyKind
			want   int64
		}{
			{`{"type":"string"}`, str, copyString, copied},
			{`{"items":{"maxLength":2000000}}`, []any{str}, copyString, copied},
			{`{"items":true}`, []any{str}, copyString, 0},
			{`{"items":{}}`, []any{str}, copyString, 0},
			{`{"items":{"minimum":0}}`, []any{json.Number("1")}, copyNumber, numberCheckBytes},
			{`{"items":{"type":"string"}}`, []any{json.Number("1")}, copyNumber, 0},
		} {
			m := &heapMeter{}
			_, decide := probedSchemas(t, tc.schema, m)
			m.start()
			_ = decide.Validate(tc.v)
			if m.copies[tc.kind] != tc.want || m.peak() < tc.want {
				t.Errorf("%s: copies %v, peak %d, want %d of kind %d at the peak", tc.schema, m.copies, m.peak(), tc.want, tc.kind)
			}
		}
	})
	t.Run("locationTree", func(t *testing.T) {
		if tree := locationTree(mustCompile(t, patternMemoryCases()[0].schema), map[string]any{}); tree < -heapNoise {
			t.Errorf("locationTree of a valid body = %d, want no Schema counted out of it", tree)
		}
	})
}

// TestRequireMeter covers how the tests that measure memory run (audit
// class GC-08, review finding: the race run, and then the suites without
// -race, took the package near or past go test's default 10-minute
// timeout on a loaded machine): skipped under -race, and on one processor
// otherwise, with the processors restored after the test.
func TestRequireMeter(t *testing.T) {
	procs := runtime.GOMAXPROCS(0)
	var skipped bool
	t.Run("measuring", func(t *testing.T) {
		t.Cleanup(func() { skipped = t.Skipped() })
		requireMeter(t)
		if got := runtime.GOMAXPROCS(0); got != 1 {
			t.Errorf("a test that measures memory runs on %d processors, want 1", got)
		}
	})
	if skipped != raceEnabled {
		t.Errorf("a test that measures memory skipped: %t, want %t under -race alone", skipped, raceEnabled)
	}
	if got := runtime.GOMAXPROCS(0); got != procs {
		t.Errorf("after a test that measures memory, %d processors, want %d", got, procs)
	}
}

// TestDuplicatesReplica covers the meter's replica of the uniqueItems
// check's map (mapCopy, hashClasses; audit class GC-08, review finding:
// the map, which the library builds and drops before any probe runs, was
// never measured):
//
//   - the items are classed as the library hashes them, a number with its
//     negation and objects whatever their member order, up to the first
//     that equals an earlier one, 1.0 equaling 1;
//   - a replica of the map allocates what the library's check allocates
//     for an array of distinct strings, which it hashes without
//     allocating, but for what the maps' hash seeds move, within a
//     1/seedApart part, and the validation's own fixed allocations;
//   - replicas built with different seeds hold within a 1/seedApart part
//     of one another.
func TestDuplicatesReplica(t *testing.T) {
	requireMeter(t)
	decode := func(body string) []any {
		v, _ := scanBody(t, mustCompile(t, `true`), body)
		return v.([]any)
	}
	for _, tc := range []struct {
		body string
		want []int
	}{
		{`[1,-1,2,-2,3]`, []int{2, 2, 1}},
		{`[{"a":1,"b":2},{"b":2,"a":1},3]`, []int{1}},
		{`[1,2,1.0,3]`, []int{1, 1}},
		{`["a","b","a"]`, []int{1, 1}},
		{`[[1,2],[2,1],[1,2]]`, []int{1, 1}},
		{`[null,false,true,"null",0]`, []int{1, 1, 1, 1, 1}},
	} {
		if got := hashClasses(decode(tc.body)); !slices.Equal(got, tc.want) {
			t.Errorf("hashClasses(%s) = %v, want %v", tc.body, got, tc.want)
		}
	}
	allocated := func(f func()) uint64 {
		least := uint64(math.MaxUint64)
		for range matchAttempts {
			var a, b runtime.MemStats
			gc := debug.SetGCPercent(-1)
			runtime.ReadMemStats(&a)
			f()
			runtime.ReadMemStats(&b)
			debug.SetGCPercent(gc)
			least = min(least, b.TotalAlloc-a.TotalAlloc)
		}
		return least
	}
	s := mustCompile(t, `{"uniqueItems":true}`)
	pair := decode(`["a","b"]`)
	fixed := allocated(func() { _ = s.root.Validate(pair) })
	for _, n := range []int{21, 100, 896, 897, 5000, 50000} {
		arr := decode(distinct(n, `"s%05d"`))
		library := allocated(func() { _ = s.root.Validate(arr) }) - fixed
		replica := allocated(func() {
			r := make(map[uint64][]int)
			for i := range n {
				r[uint64(i)] = append(r[uint64(i)], i)
			}
		})
		t.Logf("%d items: the library's check allocated %d, the replica %d", n, library, replica)
		if apart := replica / seedApart; library > replica+apart || library+apart < replica {
			t.Errorf("%d items: the library's check allocated %d, its replica %d", n, library, replica)
		}
	}
	for _, n := range []int{1000, 10000, 100000, 427000} {
		counts := make([]int, n)
		for i := range counts {
			counts[i] = 1
		}
		var sizes []int64
		for range 5 {
			sizes = append(sizes, mapCopy(counts))
		}
		least, most := slices.Min(sizes), slices.Max(sizes)
		t.Logf("%d keys: replicas %v", n, sizes)
		if most-least > least/seedApart {
			t.Errorf("%d keys: replicas held %d to %d bytes", n, least, most)
		}
	}
}
