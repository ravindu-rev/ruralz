// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/json"
	"math"
	"math/bits"
	"slices"
	"strings"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"
)

// The working set and the work of validation (proposed, package
// documentation "Working set" and "Work bound"). Before validating, the
// Filter walks the decoded body once and, from the counts the compile
// analysis allows at every depth (workBound), sums two upper bounds:
//
//   - the memory validation may hold (bytes, reserved from
//     limits.maxBufferedBytes with the decoded values): the error tree of a
//     failing body, the call stack and the unevaluated-tracking maps live
//     along any one path of the body, and the largest transient copy;
//   - the work validation may take (work units, never reserved): every
//     evaluation, and the parts of the library's work that grow with the
//     value it evaluates (members, string bytes, number literals, the
//     unevaluated copies, the uniqueItems hashing).
//
// Both are checked before the library runs, which takes no context.

// Memory, in bytes. Each constant is measured on Go 1.27 (linux/amd64) and
// rounded up.
const (
	// errorBytes is what one error unit may hold at the body root: the
	// error, its kind, its slot in the error list and a share of the
	// group error that collects several. Measured at 123 to 141 bytes per
	// failing evaluation for the common kinds, plus half for the group
	// errors.
	errorBytes = 192
	// locationBytes is what each level of depth adds to an error unit:
	// the error copies its instance location, one 16-byte string header
	// per level (measured 16.7 bytes per level with allocation size
	// classes), plus half for the group errors.
	locationBytes = 32
	// listNameBytes is what one name of a required, dependentRequired or
	// dependencies list, or one type of a type list, may add to a failing
	// evaluation: a string header in a slice grown by appending.
	listNameBytes = 32
	// ratBytes is the fixed part of the math/big rational a failing
	// numeric keyword keeps for the instance number.
	ratBytes = 96
	// stackBytes is the goroutine stack one evaluation on the current path
	// may take: measured at 1.2 to 1.3 KiB used per in-place evaluation,
	// up to three times that while the stack doubles and copies.
	stackBytes = 4096
	// propMapBase and propMapEntry bound the map of unevaluated member
	// names one evaluation copies, with every allocation of its growth:
	// measured at most 256 + 122 bytes per name.
	propMapBase, propMapEntry = 256, 128
	// itemMapBase and itemMapEntry bound the map of unevaluated item
	// indexes: measured at most 192 + 83 bytes per index.
	itemMapBase, itemMapEntry = 192, 96
	// hashNameBytes is what hashing an object for uniqueItems holds per
	// member: the sorted slice of its names.
	hashNameBytes = 16
	// dupItemBytes is what the uniqueItems check of an array of more than
	// pairwiseItems items holds per item: its hash map (measured 102 to 168
	// bytes per item).
	dupItemBytes = 192
	// matcherBytes and matcherInstBytes bound a regexp matcher: its
	// queues per program instruction and the backtracker's visited set.
	matcherBytes, matcherInstBytes = 32 << 10, 64
)

// Work, in work units: one unit is about a nanosecond of the library on
// the reference machine (a shared 4-vCPU 2.8 GHz Xeon, Go 1.27). Each
// constant is the measured cost rounded up by at least a quarter;
// TestWorkCoversTime checks the model against the library.
const (
	// evalUnits is one evaluation: its frame, scope and unevaluated
	// record (measured 212 to 222 units passing).
	evalUnits = 384
	// keywordUnits is a keyword that fails: the error and its location
	// (measured 180 to 211 units).
	keywordUnits = 320
	// listUnits is one name of a required, dependentRequired or
	// dependencies list or a type list: a lookup or an append.
	listUnits = 32
	// depUnits is one member of dependentSchemas, dependencies or
	// dependentRequired: a lookup.
	depUnits = 32
	// compareUnits is one comparison of two values (measured 4 units for
	// short strings); compareBytes are the string bytes compared per unit.
	compareUnits, compareBytes = 16, 8
	// memberUnits is one member of an object per evaluation at the
	// object: the library loops over every member (measured 19 to 35).
	memberUnits = 48
	// copyUnits is one byte of a string per evaluation at the string: the
	// library copies it (measured 0.65 to 0.74).
	copyUnits = 1
	// unevalPropUnits and unevalItemUnits are one member or item per
	// evaluation that copies the unevaluated names or indexes, with the
	// merge (measured 110 to 319 and 112 to 167).
	unevalPropUnits, unevalItemUnits = 384, 256
	// scopeUnits is one step through the dynamic scope: the cycle check
	// walks the evaluations at the value, a $dynamicRef or $recursiveRef
	// the whole path.
	scopeUnits = 4
	// locationUnits is one level of depth per evaluation and error unit:
	// the instance location the library extends and copies.
	locationUnits = 4
	// hashUnits is one value one uniqueItems pass hashes, with its entry
	// in the duplicates map (measured 95 to 273).
	hashUnits = 384
	// hashByteUnits is one byte of a string or name hashed.
	hashByteUnits = 1
	// sortUnits is one member and bit of the member count of an object
	// hashed: sorting the names.
	sortUnits = 8
	// pairwiseItems is the array length up to which the library checks
	// uniqueItems pairwise: every value is then compared with up to
	// pairwiseItems-1 others.
	pairwiseItems = 20
	// numberUnits, numberByteUnits and numberExpUnits bound one parse and
	// comparison of a number literal with math/big: measured 420 to 610
	// units for short literals and up to 9,200 at the number bounds,
	// growing with the literal's length and exponent.
	numberUnits, numberByteUnits, numberExpUnits = 768, 28, 12
)

// WorkPerByte bounds the work of validating one request body (proposed,
// package documentation "Work bound"): at most WorkPerByte work units per
// byte of limits.maxRequestBodyBytes.
const WorkPerByte = 256

// maxWorkLimit is the largest work limit; it stays below countCap, so a
// saturated count is always over the limit.
const maxWorkLimit = countCap - 1

// workLimit returns the work limit for a raw body limit (the configuration
// default when unset, as decodedLimit).
func workLimit(rawLimit int64) int64 {
	if rawLimit <= 0 {
		rawLimit = defaultMaxRequestBodyBytes
	}
	return min(rawLimit, maxWorkLimit/WorkPerByte) * WorkPerByte
}

// numberWork returns the work units of one parse and comparison of the
// number literal lit, in range.
func numberWork(lit string) int64 {
	return numberUnits + numberByteUnits*int64(len(lit)) + numberExpUnits*int64(exponentOf(lit))
}

// maxNumberWork is numberWork of the costliest literal within the number
// bounds.
const maxNumberWork = numberUnits + numberByteUnits*MaxNumberBytes + numberExpUnits*MaxNumberExponent

// exponentOf returns the magnitude of the decimal exponent of a number
// literal, at most MaxNumberExponent.
func exponentOf(lit string) int {
	i := strings.IndexAny(lit, "eE")
	if i < 0 {
		return 0
	}
	exp := 0
	for _, c := range []byte(lit[i+1:]) {
		if c >= '0' && c <= '9' {
			exp = min(exp*10+int(c-'0'), MaxNumberExponent)
		}
	}
	return exp
}

// keywords returns the keywords of s that can report an error of their
// own (assertions, and the applicators and references that wrap their
// branches' errors) and the names its required, dependentRequired and
// dependencies lists and its type list past the first type may list.
func keywords(s *sjs.Schema) (n, names int) {
	for _, set := range []bool{
		s.Ref != nil, s.RecursiveRef != nil, s.DynamicRef != nil, s.Not != nil,
		len(s.AllOf) > 0, len(s.AnyOf) > 0, len(s.OneOf) > 0, s.Format != nil,
		s.MaxProperties != nil, s.MinProperties != nil, len(s.Required) > 0, s.AdditionalProperties == false,
		s.MinItems != nil, s.MaxItems != nil, s.UniqueItems, s.Contains != nil, s.MaxContains != nil,
		s.AdditionalItems == false,
		s.MinLength != nil, s.MaxLength != nil, s.Pattern != nil,
		s.ContentEncoding != nil, s.ContentMediaType != nil, s.ContentSchema != nil,
		s.Minimum != nil, s.Maximum != nil, s.ExclusiveMinimum != nil, s.ExclusiveMaximum != nil, s.MultipleOf != nil,
	} {
		if set {
			n++
		}
	}
	names = len(s.Required)
	n += len(s.DependentRequired)
	for _, list := range s.DependentRequired {
		names += len(list)
	}
	for _, d := range s.Dependencies {
		if list, ok := d.([]string); ok {
			n++
			names += len(list)
		}
	}
	if s.Types != nil {
		// The measured error already holds a one-type list.
		names += len(s.Types.ToStrings()) - 1
	}
	return n, names
}

// errorWeight returns the error units one evaluation of s may hold when
// it fails: one per keyword of s that can report an error of its own, at
// least one, and the names its lists may copy into errors at
// listNameBytes each. The type, const and enum keywords end the
// evaluation when they fail, so they add no error past the first unit.
// Work is not memory: the time enum comparisons or pattern matching take
// is counted in dimWork and dimScan, never here.
func errorWeight(s *sjs.Schema) int {
	if s.Bool != nil {
		return 1
	}
	n, names := keywords(s)
	return max(n, 1) + (names*listNameBytes+errorBytes-1)/errorBytes
}

// workWeight returns the work units one evaluation of s takes apart from
// the parts that grow with the value: the evaluation, every keyword that
// can fail (type, const and enum count once, since the first of them that
// fails ends the evaluation), the names of its lists, its dependency
// members, the schema side of its const and enum comparisons, and the
// setup of its pattern.
func workWeight(s *sjs.Schema, pc *patternCosts) int {
	if s.Bool != nil {
		return evalUnits
	}
	n, names := keywords(s)
	if s.Types != nil || s.Const != nil || s.Enum != nil {
		// One of them may fail, which ends the evaluation.
		n++
	}
	w := evalUnits + keywordUnits*n + listUnits*names + depUnits*(len(s.DependentSchemas)+len(s.Dependencies)+len(s.DependentRequired))
	if s.Const != nil {
		w += compareWork(*s.Const, false)
	}
	if s.Enum != nil {
		for _, v := range s.Enum.Values {
			w += compareWork(v, false)
		}
	}
	if s.Pattern != nil {
		w += pc.of(s.Pattern.String()).fixed
	}
	return min(w, countCap)
}

// compareWork returns the work of comparing a value with v, a const or
// enum value, on v's side: the value side of a top-level number is in
// dimParses; a number nested in v compares with a nested body number,
// charged at the costliest literal.
func compareWork(v any, nested bool) int {
	w := compareUnits
	switch t := v.(type) {
	case string:
		w += len(t) / compareBytes
	case json.Number:
		w += int(numberWork(string(t)))
		if nested {
			w += maxNumberWork
		}
	case []any:
		for _, e := range t {
			w = min(w+compareWork(e, true), countCap)
		}
	case map[string]any:
		for k, e := range t {
			w = min(w+compareUnits+len(k)/compareBytes+compareWork(e, true), countCap)
		}
	}
	return w
}

// parsesOf returns how often one evaluation of s may parse and compare
// the number it evaluates: once per numeric keyword, once for the integer
// type, and once per number among its const and enum values.
func parsesOf(s *sjs.Schema) int {
	n := 0
	for _, set := range []bool{s.Minimum != nil, s.Maximum != nil, s.ExclusiveMinimum != nil, s.ExclusiveMaximum != nil, s.MultipleOf != nil} {
		if set {
			n++
		}
	}
	if s.Types != nil && slices.Contains(s.Types.ToStrings(), "integer") {
		n++
	}
	if s.Const != nil {
		if _, ok := (*s.Const).(json.Number); ok {
			n++
		}
	}
	if s.Enum != nil {
		for _, v := range s.Enum.Values {
			if _, ok := v.(json.Number); ok {
				n++
			}
		}
	}
	return n
}

// scanWeight returns the work units per byte of the string one evaluation
// of s matches against its pattern.
func scanWeight(s *sjs.Schema, pc *patternCosts) int {
	if s.Pattern == nil {
		return 0
	}
	return pc.of(s.Pattern.String()).perByte
}

// workingSet is the runtime model of a compiled schema: the analysis
// bound at every depth and the facts the model needs.
type workingSet struct {
	at    [maxNesting + 1]vec
	flags workFlags
	// errBytes[d] is the error tree one value at depth d may hold.
	errBytes [maxNesting + 1]int64
}

// newWorkingSet returns the model of the analysis bound wb; a nil wb (no
// analysis) charges past any limit, so every body is refused.
func newWorkingSet(wb *workBound) workingSet {
	ws := workingSet{}
	if wb == nil {
		for d := range ws.at {
			for k := range ws.at[d] {
				ws.at[d][k] = capOf(k)
			}
			ws.errBytes[d] = math.MaxInt64
		}
		return ws
	}
	ws.at, ws.flags = wb.at, wb.flags
	for d := range ws.at {
		ws.errBytes[d] = mulCap(int64(ws.at[d][dimErrs]), errorBytes+locationBytes*int64(d))
	}
	return ws
}

// bodyScan sums the working set and the work of validating a decoded
// body and, when the schema compares numbers, checks every number against
// the number bounds. It allocates nothing; recursion is bounded by the
// decoder's nesting limit.
type bodyScan struct {
	ws *workingSet
	// numbers checks the number bounds.
	numbers bool
	// memLimit and workLimit are the largest totals the body may reach;
	// past one the scan stops unless it still looks for numbers.
	memLimit, workLimit int64
	// mem sums the error trees of every value; path is the largest stack
	// and unevaluated copies along one path; transient the largest
	// temporary copy. work sums the work of every value.
	mem, path, transient, work int64
	// pairwise counts the arrays above the current value that the library
	// may check for uniqueItems pairwise.
	pairwise int
	// bad is set at the first number out of range.
	bad bool
}

// memory returns the bytes the scan reserves besides the decoded values.
func (b *bodyScan) memory() int64 {
	return addCap(addCap(b.mem, b.path), b.transient)
}

// over reports whether the body is past either limit.
func (b *bodyScan) over() bool {
	return b.memoryOver() || b.workOver()
}

// memoryOver and workOver report which limit the body is past.
func (b *bodyScan) memoryOver() bool { return b.memory() > b.memLimit }
func (b *bodyScan) workOver() bool   { return b.work > b.workLimit }

// stop reports whether the rest of the body cannot change the outcome.
func (b *bodyScan) stop() bool { return b.bad || (!b.numbers && b.over()) }

// scan scans the body v.
func (b *bodyScan) scan(v any) {
	b.path = b.value(v, 0, "", 0)
	if b.ws.flags.maxInsts > 0 {
		b.transient = max(b.transient, matcherBytes+matcherInstBytes*int64(b.ws.flags.maxInsts))
	}
}

// valueCharge is what bodyScan charges every value before its kind's
// terms: the value's counts, the work and stack of its evaluations, and
// the hashing passes over it.
type valueCharge struct {
	c               *vec
	evals, frames   int64
	work, hash, own int64
}

// enter charges the error tree of a value at depth d, a member named name
// ("" for an item, the root or a member name) below frames evaluations on
// the path, and returns the work and stack its evaluations take whatever
// its kind.
func (b *bodyScan) enter(d int, name string, frames int64) valueCharge {
	dd := min(d, maxNesting)
	c := &b.ws.at[dd]
	evals := int64(c[dimEvals])
	frames = addCap(frames, evals)
	b.mem = addCap(b.mem, b.ws.errBytes[dd])
	work := addCap(int64(c[dimWork]), mulCap(int64(c[dimEvals]+c[dimErrs]), int64(dd)*locationUnits))
	// The cycle check walks the evaluations at the value; a dynamic
	// reference the whole path.
	work = addCap(work, mulCap(evals, evals*scopeUnits))
	if b.ws.flags.dynamic {
		work = addCap(work, mulCap(evals, mulCap(frames, scopeUnits)))
	}
	work = addCap(work, mulCap(int64(c[dimScan]), int64(len(name))))
	hash := int64(c[dimHash])
	if b.pairwise > 0 {
		hash = mulCap(hash, pairwiseItems-1)
	}
	return valueCharge{c: c, evals: evals, frames: frames, work: work, hash: hash, own: mulCap(evals, stackBytes)}
}

// str charges a string of n bytes entered as a: the copy every evaluation
// makes,
// the patterns that scan it, and the uniqueItems comparisons; it returns
// the stack.
func (b *bodyScan) str(a valueCharge, n int64) int64 {
	work := addCap(a.work, mulCap(n, addCap(mulCap(a.evals, copyUnits), int64(a.c[dimScan]))))
	b.addWork(addCap(work, mulCap(a.hash, compareUnits+n/compareBytes)))
	if a.evals > 0 {
		b.transient = max(b.transient, n)
	}
	return a.own
}

// value scans v at depth d, a member named name ("" for an item or the
// root) below frames evaluations on the path, and returns the stack and
// unevaluated copies on the costliest path from v down.
func (b *bodyScan) value(v any, d int, name string, frames int64) int64 {
	a := b.enter(d, name, frames)
	c, work, own := a.c, a.work, a.own
	switch t := v.(type) {
	case string:
		return b.str(a, int64(len(t)))
	case json.Number:
		lit := string(t)
		if b.numbers && !numberInRange(lit) {
			b.bad = true
			return 0
		}
		if b.ws.flags.rats {
			b.mem = addCap(b.mem, mulCap(int64(c[dimErrs]), ratCost(lit)))
		}
		work = addCap(work, mulCap(int64(c[dimParses]), numberWork(lit)))
		work = addCap(work, mulCap(a.hash, compareUnits+numberWork(lit)))
	case []any:
		n := int64(len(t))
		if b.ws.flags.uneItems {
			u := int64(c[dimUneval])
			work = addCap(work, mulCap(mulCap(u, n), unevalItemUnits))
			own = addCap(own, mulCap(u, itemMapBase+itemMapEntry*n))
		}
		work = addCap(work, mulCap(a.hash, compareUnits))
		unique := c[dimUnique] > 0
		if unique && n > pairwiseItems {
			b.transient = max(b.transient, mulCap(n, dupItemBytes))
		}
		b.addWork(work)
		short := unique && n > 1 && n <= pairwiseItems
		if short {
			b.pairwise++
		}
		var below int64
		for _, e := range t {
			if b.stop() {
				break
			}
			below = max(below, b.value(e, d+1, "", a.frames))
		}
		if short {
			b.pairwise--
		}
		return addCap(own, below)
	case map[string]any:
		n := int64(len(t))
		work = addCap(work, mulCap(mulCap(a.evals, n), memberUnits))
		if b.ws.flags.uneProps {
			u := int64(c[dimUneval])
			work = addCap(work, mulCap(mulCap(u, n), unevalPropUnits))
			own = addCap(own, mulCap(u, propMapBase+propMapEntry*n))
		}
		if c[dimHash] > 0 {
			work = addCap(work, mulCap(mulCap(int64(c[dimHash]), n), sortUnits*int64(bits.Len64(uint64(n)))))
			own = addCap(own, mulCap(n, hashNameBytes))
		}
		work = addCap(work, mulCap(a.hash, compareUnits*(1+n)))
		b.addWork(work)
		var below int64
		for k, e := range t {
			if b.stop() {
				break
			}
			if b.ws.flags.names {
				// The name is a string value of its own (propertyNames),
				// validated on top of the current path.
				below = max(below, b.str(b.enter(d+1, "", a.frames), int64(len(k))))
			}
			below = max(below, b.value(e, d+1, k, a.frames))
		}
		return addCap(own, below)
	}
	b.addWork(work)
	return own
}

// addWork adds w to the work, which saturates at workLimit+1.
func (b *bodyScan) addWork(w int64) {
	if w > b.workLimit-b.work {
		b.work = b.workLimit + 1
		return
	}
	b.work += w
}

// ratCost returns the bytes of the rational the library parses from the
// number literal lit, in range: ratBytes plus half a byte per decimal
// digit of the literal and of its exponent (a decimal digit takes log2(10)
// bits, about 0.42 bytes, in the numerator or the power of ten of the
// denominator).
func ratCost(lit string) int64 {
	return ratBytes + int64(len(lit)+exponentOf(lit)+1)/2
}

// addCap returns a+b for non-negative a and b, or math.MaxInt64 when the
// sum overflows.
func addCap(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

// mulCap returns a*b for non-negative a and b, or math.MaxInt64 when the
// product overflows.
func mulCap(a, b int64) int64 {
	if a != 0 && b > math.MaxInt64/a {
		return math.MaxInt64
	}
	return a * b
}
