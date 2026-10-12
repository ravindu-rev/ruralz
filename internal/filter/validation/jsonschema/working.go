// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"math/big"
	"math/bits"
	"slices"
	"strings"
	"unicode/utf8"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"
)

// The working set and the work of validation (architecture R-77, package
// documentation "Working set" and "Work bound"). Validation runs in up to
// two passes: a decision pass, in which the library keeps no location of
// its errors and stops early where it can, and, only for a body that
// fails, a location pass that names the failing keyword. Before either,
// the Filter walks the decoded body once and, from the counts of each
// value's own path (states.go) within the counts the compile analysis
// allows at every depth (workBound), sums upper bounds of:
//
//   - the memory each pass may hold (bytes, reserved from
//     limits.maxBufferedBytes with the decoded values): its error tree,
//     the call stack and the unevaluated-tracking maps live along any one
//     path of the body, and the largest transient copy;
//   - the work validation may take (work units, never reserved): every
//     evaluation, and the parts of the library's work that grow with the
//     value it evaluates (members, string bytes, number literals, the
//     unevaluated copies, the uniqueItems hashing and comparisons).
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
	// stackBytes is the goroutine stack one evaluation of the chain on the
	// call stack may take (evalGraph.chain): measured at 1.4 to 1.6 KiB per
	// in-place evaluation and 2.2 to 2.5 KiB per evaluation of a nesting
	// level's chain, whose first evaluation also descends into the value
	// (debug.SetMaxStack), up to three times that while the stack doubles
	// and copies.
	stackBytes = 8192
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
	// matcherBytes and matcherInstBytes bound a regexp matcher but for
	// the backtracker's job stack, which grows with the input
	// (backtrackBytes): the NFA simulation's queues and threads per
	// program instruction and the backtracker's visited set.
	matcherBytes, matcherInstBytes = 32 << 10, 64
	// decideErrorBytes is what one error may hold in the decision pass,
	// where the library's errors carry no location, kind or causes of
	// their own (validator.go error): the 80-byte error and its slot in
	// the one list that holds it, up to 24 bytes while the list doubles. An
	// error unit is one error, and an evaluation that collects two or more
	// errors adds a group error.
	decideErrorBytes = 104
	// decideFrames are the evaluations the decision pass adds at the body
	// root, on the call stack below the document's: the wrapper, its not
	// that asks for a boolean result, and the not's reference to the
	// document.
	decideFrames = 3
	// listedNameBytes is what an evaluation with additionalProperties
	// false holds per member while it loops: the names it does not
	// declare, in a slice grown by appending.
	listedNameBytes = 48
	// matchedItemBytes is what an evaluation with contains holds per item
	// while it loops, and the location pass keeps in a minContains or
	// maxContains error: the index of every item that matches, in a slice
	// grown by appending (review finding: 100 such evaluations over 16,000
	// items held 12.7 MB charged 58 KB).
	matchedItemBytes = 16
)

// The hash table of a decoded object (a map[string]any, Go's swiss
// tables), which internal/jsonval charges as its 48-byte header and, per
// member, the 32 bytes of the name's and the value's headers in its slot
// (mapObject). Measured on Go 1.26 and 1.27 (linux/amd64): within 1% of
// mapBytes for every member count to 3,000 and at powers of two to
// 200,000.
const (
	// mapSmallBytes is the one group of 8 slots of a map of up to
	// mapGroupSlots members (8 control bytes and 8 slots of 32 bytes, in
	// the 288-byte size class).
	mapSmallBytes, mapGroupSlots = 288, 8
	// mapSlotBytes is one slot of a larger table with its control byte,
	// in a table of a power of two slots, at least 16 and at most
	// mapTableSlots, filled to at most 7/8; mapTableBytes its header and
	// directory.
	mapSlotBytes, mapTableBytes, mapTableSlots = 33, 160, 1024
	// mapMemberBytes is what a map of several tables holds per member: a
	// table that splits leaves two of half the members (measured at most
	// 91.5).
	mapMemberBytes = 96
	// mapChargedBytes is what jsonval charges per member in the slot.
	mapChargedBytes = 32
)

// mapExtra returns what the map of a decoded object of n members holds
// beyond what jsonval charges for it (review finding: a one-member object
// holds 336 bytes, of which jsonval charges 80, so arrays of small objects
// held up to 3.8 times what Handle reserved).
func mapExtra(n int64) int64 {
	var table int64
	switch {
	case n == 0:
		return 0
	case n <= mapGroupSlots:
		table = mapSmallBytes
	case n <= mapTableSlots*7/8:
		slots := int64(16)
		for slots*7/8 < n {
			slots *= 2
		}
		// Rounded up to its allocation size class, at most a quarter more.
		table = slots*mapSlotBytes*5/4 + mapTableBytes
	default:
		table = mulCap(n, mapMemberBytes)
	}
	return max(0, table-mapChargedBytes*n)
}

// sizeClasses returns the allocation size classes of Go 1.26 and 1.27 from
// 16 bytes to 32 KiB (internal/runtime/gc/sizeclasses.go); past them an
// allocation is rounded up to whole 8 KiB pages.
func sizeClasses() [66]uint16 {
	return [66]uint16{
		16, 24, 32, 48, 64, 80, 96, 112, 128, 144, 160, 176, 192, 208, 224, 240, 256, 288, 320, 352, 384, 416, 448,
		480, 512, 576, 640, 704, 768, 896, 1024, 1152, 1280, 1408, 1536, 1792, 2048, 2304, 2688, 3072, 3200, 3456,
		4096, 4864, 5376, 6144, 6528, 6784, 6912, 8192, 9472, 9728, 10240, 10880, 12288, 13568, 14336, 16384, 18432,
		19072, 20480, 21760, 24576, 27264, 28672, 32768,
	}
}

// pageBytes is the page size an allocation past the size classes is
// rounded up to.
const pageBytes = 8 << 10

// stringExtra returns what the bytes of a decoded string, member name or
// number literal of n bytes hold beyond the n bytes jsonval charges: the
// rounding up to their allocation size class. Shorter than 16 bytes, the
// allocator packs them into shared 16-byte blocks.
func stringExtra(n int64) int64 {
	if n < 16 {
		return 0
	}
	if n > 32<<10 {
		return (n+pageBytes-1)/pageBytes*pageBytes - n
	}
	classes := sizeClasses()
	i, _ := slices.BinarySearch(classes[:], uint16(n))
	return int64(classes[i]) - n
}

// arraySlotBytes is one item's slot in a decoded array's backing array,
// which internal/jsonval charges as the item's CostValue.
const arraySlotBytes = 16

// arrayCaps returns the capacities append grows a decoded array through
// up to 1,023 items, measured on Go 1.26 and 1.27: it doubles, and past
// 512 bytes the allocation header moves the size class.
func arrayCaps() [10]int64 { return [10]int64{2, 4, 8, 16, 32, 71, 143, 303, 591, 1023} }

// arrayExtra returns what the backing array of a decoded array of n items
// holds beyond what jsonval charges for it, the spare capacity append
// leaves: exactly up to 1,023 items (arrayCaps), and past it, measured to
// 4,000,000 items, at most two thirds of the items up to 4,095 and 0.31
// past (growth by a quarter, rounded to pages).
func arrayExtra(n int64) int64 {
	var spare int64
	switch {
	case n == 0:
		return 0
	case n < 1024:
		for _, c := range arrayCaps() {
			if c >= n {
				spare = c - n
				break
			}
		}
	case n < 4096:
		spare = n * 3 / 4
	default:
		spare = n * 3 / 8
	}
	return mulCap(spare, arraySlotBytes)
}

// Work, in work units: one unit is about a nanosecond of the library on
// the reference machine (a shared 4-vCPU 2.8 GHz Xeon, Go 1.27). Each
// constant is the measured cost rounded up by at least a quarter;
// BenchmarkWorkModel compares the model with the library.
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
	// short strings alone, 6.5 to 9 with the loop over an enum's values,
	// BenchmarkWorkModel; minor review finding: at 16 an array of
	// two-letter codes under a 250-value enum was refused above 27% of the
	// limit); compareBytes are the string bytes compared per unit.
	compareUnits, compareBytes = 12, 8
	// memberUnits is one member of an object per evaluation at the
	// object: the library loops over every member (measured 19 to 35).
	memberUnits = 48
	// nameHashBytes is the bytes of a member name per work unit of
	// hashing or comparing it: the library looks the name up in, inserts
	// it into and deletes it from maps keyed by names, which hashes the
	// whole name (measured 0.02 to 0.05 ns per byte), and sorts the names
	// of an object it hashes for uniqueItems, comparing them byte by byte
	// (review finding: a 10 MiB name, charged nothing per byte, was hashed
	// for 1.35 to 3.2 s).
	nameHashBytes = 8
	// nameHashes is how often an evaluation with member keywords may hash
	// each member name: the properties lookup. unevalNameHashes is how
	// often an evaluation that copies the unevaluated names may: the
	// copy's insert, the delete once a keyword evaluates the member, the
	// merge's lookup and delete, and the unevaluatedProperties lookup.
	nameHashes, unevalNameHashes = 1, 5
	// copyUnits is one byte of a string per evaluation at the string: the
	// library copies it (measured 0.65 to 0.74).
	copyUnits = 1
	// lengthByteUnits is one byte of a string per evaluation with
	// minLength or maxLength, which copies the string again and counts its
	// runes (measured up to 0.9 more than copyUnits for multi-byte runes,
	// 0.1 for ASCII).
	lengthByteUnits = 2
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
	// walkUnits is what the location walk takes per leaf of the error tree
	// besides reading its location (walkWork): the kind, the keyword path,
	// the comparison; walkByteUnits per byte of the location and keyword,
	// which it unescapes, extends and compares (measured at most 120 and
	// 1.4, BenchmarkLocationWalk).
	walkUnits, walkByteUnits = 192, 2
	// numberUnits, numberByteUnits and numberExpUnits bound one parse and
	// comparison of a number literal with math/big: measured 420 to 610
	// units for short literals and up to 9,200 at the number bounds,
	// growing with the literal's length and exponent.
	numberUnits, numberByteUnits, numberExpUnits = 768, 28, 12
)

// The work of a multipleOf check (review finding: a body number's check
// against a divisor was charged as one parse of the body literal, so a
// short decimal against 0.(120 fives)e-990 took 4.7 ns per charged unit).
// The library divides the value by the divisor as math/big rationals,
// a/b by c/d: it multiplies a by d and c by b, divides both products by
// their greatest common divisor (Lehmer's algorithm), and checks the
// quotient's denominator. With a, b, c and d bounded in decimal digits
// (literalDigits, ratDigits), one check takes at most quoUnits, quoMinUnits
// per digit of the smaller product (each step of the GCD), quoDigitUnits
// per digit of all four, and a unit per quoProductDigits of the digit
// products a*b, c*d and (a+b)*(c+d) (the multiplications, the GCD's
// updates and the final divisions): measured on 1,500 random pairs within
// the number bounds and at their edges, from 0.25 to 0.5 µs for short
// literals to 26 µs, every one at least 40% under the model, which is up
// to about 30 times above it for literals sharing large powers of ten
// (BenchmarkWorkModel's multipleOf cases). The divisor's part is the
// schema's (workWeight); the rest is the scan's (bodyScan.quoWork).
const (
	quoUnits, quoMinUnits, quoDigitUnits, quoProductDigits = 512, 64, 16, 128
)

// literalDigits returns upper bounds of the decimal digits of the
// numerator and the denominator of the rational the number literal lit,
// in range, denotes: a mantissa of m digits, f of them after the point,
// and an exponent e denote an integer of m digits times 10^(e-f).
func literalDigits(lit string) (num, den int64) {
	mant, exp := lit, 0
	if i := exponentIndex(lit); i >= 0 {
		mant, exp = lit[:i], exponentOf(lit)
		if strings.HasPrefix(lit[i+1:], "-") {
			exp = -exp
		}
	}
	mant = strings.TrimPrefix(mant, "-")
	m, f := len(mant), 0
	if i := strings.IndexByte(mant, '.'); i >= 0 {
		m, f = len(mant)-1, len(mant)-1-i
	}
	if shift := exp - f; shift >= 0 {
		return int64(m + shift), 1
	}
	return int64(m), int64(f-exp) + 1
}

// ratDigits returns upper bounds of the decimal digits of the numerator
// and the denominator of r (log10(2) is below 0.30103).
func ratDigits(r *big.Rat) (num, den int) {
	return r.Num().BitLen()*30103/100000 + 1, r.Denom().BitLen()*30103/100000 + 1
}

// divisorWork returns the divisor's part of the work of one multipleOf
// check against r: the fixed work, its digits and their product.
func divisorWork(r *big.Rat) int {
	c, d := ratDigits(r)
	return quoUnits + quoDigitUnits*(c+d) + (c*d+quoProductDigits-1)/quoProductDigits
}

// quoWork returns the value's part of the work of n multipleOf checks of
// the number literal lit against divisors whose numerators sum to divNum
// digits and denominators to divDen: the GCD's steps, at most the smaller
// of the two products' digits summed over the checks, the literal's
// digits, and the digit products with the literal's.
func quoWork(lit string, n, divNum, divDen int64) int64 {
	if n == 0 {
		return 0
	}
	a, b := literalDigits(lit)
	steps := min(addCap(mulCap(n, a), divDen), addCap(divNum, mulCap(n, b)))
	work := addCap(mulCap(steps, quoMinUnits), mulCap(n, quoDigitUnits*(a+b)+(a*b+quoProductDigits-1)/quoProductDigits))
	return addCap(work, (mulCap(a+b, addCap(divNum, divDen))+quoProductDigits-1)/quoProductDigits)
}

// WorkPerByte bounds the work of validating one request body (architecture
// R-77, package documentation "Work bound"): at most WorkPerByte work units
// per byte of limits.maxRequestBodyBytes.
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

// walkWork returns the work units the location walk may take per leaf of
// the error tree under a document whose error locations have at most
// maxPointer bytes (review finding: the walk reads every leaf's location,
// so its work grows with the leaves times that length, which the library's
// does not).
func walkWork(maxPointer int) int64 {
	return walkUnits + walkByteUnits*int64(maxPointer)
}

// maxNumberWork is numberWork of the costliest literal within the number
// bounds.
const maxNumberWork = numberUnits + numberByteUnits*MaxNumberBytes + numberExpUnits*MaxNumberExponent

// exponentOf returns the magnitude of the decimal exponent of a number
// literal, at most MaxNumberExponent.
func exponentOf(lit string) int {
	i := exponentIndex(lit)
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

// units are the error units one evaluation of a schema may hold, by the
// JSON kind of the value (errorUnits).
type units struct {
	// full are the units of the location pass and names the units its
	// name lists add.
	full, names [kinds]int
	// decide are the units of the decision pass. At an object, an
	// evaluation with member keywords of its own also keeps the errors of
	// one member, unless it failed before its member loop: such an
	// evaluation holds either first (failures before the loop, or its
	// applicators) or after (failures after the loop) and the member's,
	// and decide[kindObject] is the larger of the two.
	decide       [kinds]int
	first, after int
	// groups[k] is 1 when an evaluation at a value of kind k may collect
	// two or more errors in the decision pass, which adds a group error:
	// its own units, the errors of the children it validates (every item,
	// one member, every member under unevaluatedProperties or
	// propertyNames) and of then, else, dependentSchemas and dependencies.
	groups [kinds]int
	// length holds the string units apart when minLength or maxLength is
	// set, for the scan to decide their failure from the string's length;
	// the string units above count both as failing.
	length *lengthUnits
}

// lengthUnits are the parts of the string units of an evaluation with
// minLength or maxLength (min and max, -1 when absent): a type, const,
// enum or format failure (early), $ref, the other string keywords, the
// applicators, then, else and dependent schemas (direct), and the units
// the type list adds to the location pass.
type lengthUnits struct {
	min, max                                int
	early, ref, others, cond, direct, names int
}

// at returns the string units of a string of runes characters: the
// location pass's, the decision pass's and its group.
func (l lengthUnits) at(runes int) (full, decide, groups int) {
	fails := 0
	if l.min >= 0 && runes < l.min {
		fails++
	}
	if l.max >= 0 && runes > l.max {
		fails++
	}
	return l.units(fails)
}

// worst returns the string units with both keywords failing.
func (l lengthUnits) worst() (full, decide, groups int) {
	fails := 0
	for _, set := range []bool{l.min >= 0, l.max >= 0} {
		if set {
			fails++
		}
	}
	return l.units(fails)
}

func (l lengthUnits) units(fails int) (full, decide, groups int) {
	full = max(l.early, l.ref+l.cond+l.others+fails)
	decide = max(l.early, l.ref+l.others+fails, l.cond)
	if decide+l.direct >= 2 {
		groups = 1
	}
	return full, decide, groups
}

// errorUnits returns the error units one evaluation of s may hold at a
// value of each JSON kind (a keyword of another kind cannot fail on it).
// Every keyword that can report an error of its own counts one unit:
// assertions, and the references and applicators that wrap their
// branches' errors. A type, const, enum or format that fails ends the
// evaluation with one error.
//
// The location pass evaluates every other keyword. The decision pass
// (validator.go with boolResult) evaluates $ref and the keywords of the
// value's kind, then the other references and the applicators only when
// none failed; at an object it stops after minProperties, maxProperties
// and required when one failed, and its member loop stops at the first
// member that fails, so a failure before the loop, the failures after it,
// and the applicators exclude each other. The name lists of required,
// dependentRequired and dependencies and the type list past its first
// type add units to the location pass's errors only.
func errorUnits(s *sjs.Schema) units {
	var u units
	if s.Bool != nil {
		if !*s.Bool {
			for k := range u.full {
				u.full[k], u.decide[k] = 1, 1
			}
			u.first = 1
		}
		return u
	}
	count := func(set ...bool) int {
		n := 0
		for _, b := range set {
			if b {
				n++
			}
		}
		return n
	}
	var types []string
	if s.Types != nil {
		types = s.Types.ToStrings()
	}
	ref := count(s.Ref != nil)
	cond := count(s.RecursiveRef != nil, s.DynamicRef != nil, s.Not != nil, len(s.AllOf) > 0, len(s.AnyOf) > 0, len(s.OneOf) > 0)
	before := pairFailing(s.MinProperties, s.MaxProperties) + count(len(s.Required) > 0)
	after := count(s.AdditionalProperties == false) + len(s.DependentRequired)
	listed := len(s.Required)
	for _, list := range s.DependentRequired {
		listed += len(list)
	}
	for _, d := range s.Dependencies {
		if list, ok := d.([]string); ok {
			after++
			listed += len(list)
		}
	}
	direct := count(s.Then != nil || s.Else != nil) + len(s.DependentSchemas)
	for _, d := range s.Dependencies {
		if _, ok := d.(*sjs.Schema); ok {
			direct++
		}
	}
	others := count(s.Pattern != nil, s.ContentEncoding != nil, s.ContentMediaType != nil, s.ContentSchema != nil)
	var own [kinds]int
	own[kindObject] = before + after
	own[kindArray] = pairFailing(s.MinItems, s.MaxItems) + count(s.UniqueItems, s.Contains != nil, s.MaxContains != nil, s.AdditionalItems == false)
	own[kindString] = count(s.MinLength != nil, s.MaxLength != nil) + others
	own[kindNumber] = boundsFailing(s) + count(s.MultipleOf != nil)
	own[kindInteger] = own[kindNumber]
	loops := len(s.Properties) > 0 || len(s.PatternProperties) > 0 || s.AdditionalProperties != nil
	all := 2 * count(s.UnevaluatedProperties != nil || s.PropertyNames != nil)
	itemKeywords := s.Items != nil || s.Items2020 != nil || len(s.PrefixItems) > 0 || s.Contains != nil || s.UnevaluatedItems != nil || s.AdditionalItems != nil
	for k, name := range typeNames() {
		// The type keyword fails only on a value of a kind it does not
		// list: an integer type may fail on any number but one whose literal
		// is an integer, which a number type admits too.
		typeFails := types != nil && !slices.Contains(types, name)
		if k == kindInteger && slices.Contains(types, "number") {
			typeFails = false
		}
		early := count(typeFails || s.Const != nil || s.Enum != nil || s.Format != nil)
		u.full[k] = max(early, ref+cond+own[k])
		u.decide[k] = max(early, ref+own[k], cond)
		listedHere := 0
		if typeFails {
			// The measured error already holds a one-type list.
			listedHere = len(types) - 1
		}
		if k == kindObject {
			listedHere += listed
			u.first, u.after = max(early, ref+before, cond), after
			u.decide[k] = max(u.first, u.after)
		}
		u.names[k] = (listedHere*listNameBytes + errorBytes - 1) / errorBytes
		entries := u.decide[k] + direct
		switch k {
		case kindObject:
			entries = max(u.first, u.after+count(loops)) + direct + all
		case kindArray:
			entries += 2 * count(itemKeywords)
		case kindString:
			if s.MinLength != nil || s.MaxLength != nil {
				u.length = &lengthUnits{min: -1, max: -1, early: early, ref: ref, others: others, cond: cond, direct: direct, names: u.names[k]}
				if s.MinLength != nil {
					u.length.min = *s.MinLength
				}
				if s.MaxLength != nil {
					u.length.max = *s.MaxLength
				}
			}
		}
		u.groups[k] = count(entries >= 2)
	}
	return u
}

// pairFailing returns how many of a lower and an upper bound on a count
// (minItems and maxItems, minProperties and maxProperties) can fail on one
// value: one of them only, when the lower is at most the upper.
func pairFailing(lower, upper *int) int {
	switch {
	case lower != nil && upper != nil && *lower <= *upper:
		return 1
	case lower != nil && upper != nil:
		return 2
	case lower != nil || upper != nil:
		return 1
	}
	return 0
}

// boundsFailing returns how many of the numeric bounds of s can fail on
// one number: every lower bound (minimum, exclusiveMinimum) may fail
// together, and so may every upper bound (maximum, exclusiveMaximum), but a
// lower and an upper bound only when the lower lies above the upper, or at
// it when both exclude it.
func boundsFailing(s *sjs.Schema) int {
	type bound struct {
		v         *big.Rat
		exclusive bool
	}
	var lower, upper []bound
	for _, b := range []bound{{s.Minimum, false}, {s.ExclusiveMinimum, true}} {
		if b.v != nil {
			lower = append(lower, b)
		}
	}
	for _, b := range []bound{{s.Maximum, false}, {s.ExclusiveMaximum, true}} {
		if b.v != nil {
			upper = append(upper, b)
		}
	}
	for _, l := range lower {
		for _, u := range upper {
			if c := l.v.Cmp(u.v); c > 0 || (c == 0 && l.exclusive && u.exclusive) {
				return len(lower) + len(upper)
			}
		}
	}
	return max(len(lower), len(upper))
}

// composite reports whether s compares whole values with a const or enum
// value that holds an array or an object, whose nested numbers the library
// parses when it compares them with the value's.
func composite(s *sjs.Schema) bool {
	isComposite := func(v any) bool {
		switch v.(type) {
		case []any, map[string]any:
			return true
		default:
			return false
		}
	}
	if s.Const != nil && isComposite(*s.Const) {
		return true
	}
	if s.Enum != nil {
		return slices.ContainsFunc(s.Enum.Values, isComposite)
	}
	return false
}

// workWeight returns the work units one evaluation of s takes apart from
// the parts that grow with the value: the evaluation, every keyword that
// can fail (type, const and enum count once, since the first of them that
// fails ends the evaluation), the names of its lists, its dependency
// members, the bytes of the names it looks up, the schema side of its
// const and enum comparisons, the setup of its pattern, and the divisor's
// part of its multipleOf check.
func workWeight(s *sjs.Schema, pc *patternCosts) int {
	if s.Bool != nil {
		return evalUnits
	}
	n, names := keywords(s)
	if s.Types != nil || s.Const != nil || s.Enum != nil {
		// One of them may fail, which ends the evaluation.
		n++
	}
	w := evalUnits + keywordUnits*n + listUnits*names + depUnits*(len(s.DependentSchemas)+len(s.Dependencies)+len(s.DependentRequired)) +
		lookedUpBytes(s)/nameHashBytes
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
	if s.MultipleOf != nil {
		w += divisorWork(s.MultipleOf)
	}
	return min(w, countCap)
}

// lookedUpBytes returns the bytes of the names one evaluation of s looks
// up in the object it evaluates, which hashes them whole: its required
// names, and the names of its dependentRequired, dependencies and
// dependentSchemas members and lists.
func lookedUpBytes(s *sjs.Schema) int {
	n := 0
	for _, name := range s.Required {
		n += len(name)
	}
	for name, list := range s.DependentRequired {
		n += len(name)
		for _, l := range list {
			n += len(l)
		}
	}
	for name, d := range s.Dependencies {
		n += len(name)
		if list, ok := d.([]string); ok {
			for _, l := range list {
				n += len(l)
			}
		}
	}
	for name := range s.DependentSchemas {
		n += len(name)
	}
	return n
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

// comparedObjects returns the objects of the const and enum values of s,
// at any depth, that one evaluation of s compares with the value.
func comparedObjects(s *sjs.Schema) int {
	n := 0
	if s.Const != nil {
		n = objectsIn(*s.Const)
	}
	if s.Enum != nil {
		for _, v := range s.Enum.Values {
			n = min(n+objectsIn(v), countCap)
		}
	}
	return n
}

// objectsIn returns the objects of v, v itself included, at any depth.
func objectsIn(v any) int {
	n := 0
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			n = min(n+objectsIn(e), countCap)
		}
	case map[string]any:
		n = 1
		for _, e := range t {
			n = min(n+objectsIn(e), countCap)
		}
	}
	return n
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
// of s matches against its pattern, and copies and counts the runes of
// under minLength or maxLength (minor review finding: counting multi-byte
// runes took 2.5 to 3 times the work per byte of a copy).
func scanWeight(s *sjs.Schema, pc *patternCosts) int {
	w := 0
	if s.MinLength != nil || s.MaxLength != nil {
		w += lengthByteUnits
	}
	if s.Pattern != nil {
		w += pc.of(s.Pattern.String()).perByte
	}
	return w
}

// workingSet is the runtime model of a compiled schema: the analysis
// bound at every depth, the nesting limit, the states of the body paths
// and the facts the model needs.
type workingSet struct {
	at    [maxNesting + 1]vec
	flags workFlags
	// limit is the nesting limit: a body value this deep or deeper is
	// refused unless its state bounds it (bodyScan.bounded).
	limit int
	// states are the per-path states, states[0] the root's; empty when
	// the bounds left even the root's unknown.
	states []evalState
}

// newWorkingSet returns the model of the analysis bound wb with the states
// of the body paths; a nil wb (no analysis) charges past any limit, so
// every body is refused.
func newWorkingSet(wb *workBound, states []evalState) workingSet {
	ws := workingSet{}
	if wb == nil {
		for d := range ws.at {
			for k := range ws.at[d] {
				ws.at[d][k] = capOf(k)
			}
		}
		ws.limit = maxNesting + 1
		return ws
	}
	ws.at, ws.flags, ws.limit, ws.states = wb.at, wb.flags, wb.limit, states
	return ws
}

// bodyScan sums the working set of each pass and the work of validating a
// decoded body, checks it against the nesting limit and, where the
// library may parse them, checks its numbers against the number bounds.
// It allocates nothing; recursion is bounded by the decoder's nesting
// limit.
type bodyScan struct {
	ws *workingSet
	// numbers checks the number bounds.
	numbers bool
	// workLimit is the largest work the body may reach; past it the scan
	// stops unless it still looks for numbers.
	workLimit int64
	// decide is the error tree the decision pass may hold and locate the
	// location pass's; path is the largest stack and unevaluated copies
	// along one path; transient the largest temporary copy. work sums the
	// work of every value, the same bound for either pass, and walk the
	// work of the location pass's walk over its error tree. extra sums the
	// bytes the decoded objects' hash tables, the arrays' spare capacity and
	// the strings' size classes hold beyond what the decoder charged them
	// (mapExtra, arrayExtra, stringExtra), reserved with the decoded values.
	decide, locate, path, transient, work, walk, extra int64
	// compare is the most other items that an item of an enclosing
	// uniqueItems array may be compared with; objects the const and enum
	// objects that the enclosing values' evaluations compare with them
	// (dimObjects), each of which may compare with an object of the value.
	compare, objects int64
	// comparing counts the enclosing values that some evaluation compares
	// whole with a const or enum value holding arrays or objects.
	comparing int
	// bad is set at the first number out of range the library may parse;
	// deep at the first value at the nesting limit that its state does not
	// bound.
	bad, deep bool
}

// decideMemory and locateMemory return the bytes each pass may hold
// besides the decoded values.
func (b *bodyScan) decideMemory() int64 {
	return addCap(addCap(b.decide, b.path), b.transient)
}

func (b *bodyScan) locateMemory() int64 {
	return addCap(addCap(b.locate, b.path), b.transient)
}

// workOver reports whether the body is past the work limit.
func (b *bodyScan) workOver() bool { return b.work > b.workLimit }

// locateOver reports whether the decision pass, then the location pass
// and its walk, are past the work limit together.
func (b *bodyScan) locateOver() bool {
	return addCap(addCap(b.work, b.work), b.walk) > b.workLimit
}

// stop reports whether the rest of the body cannot change the outcome.
func (b *bodyScan) stop() bool { return b.bad || (!b.numbers && (b.deep || b.workOver())) }

// scan scans the body v.
func (b *bodyScan) scan(v any) {
	st := unknownState
	if len(b.ws.states) > 0 {
		st = 0
	}
	path, decide := b.value(v, 0, "", 0, st, nil)
	b.path, b.decide = addCap(path, decideFrames*stackBytes), decide
}

// valueCharge is what bodyScan charges every value before its kind's
// terms: the value's state (nil when unknown) and counts, the error units
// of its kind and its own share of the decision pass's error tree, the
// work and stack of its evaluations, and the comparisons it takes part in.
type valueCharge struct {
	st            *evalState
	c             vec
	full, decide  int64
	evals, frames int64
	work, hash    int64
	own           int64
}

// enter charges the value v at depth d in state st (unknownState when
// unknown), a member named name ("" for an item, the root or a member
// name) of an object in state parent (nil when it is not a member or its
// parent's state is unknown), below frames evaluations on the path: it
// adds the location pass's errors and the largest transient copy and
// returns the rest.
func (b *bodyScan) enter(d int, name string, frames int64, st int, parent *evalState, v any) valueCharge {
	dd := min(d, maxNesting)
	c := b.ws.at[dd]
	if d > b.ws.limit {
		// Past the nesting limit the analysis has no bound: only a state
		// within MaxEvaluations does (bounded); without one, the value is
		// too deep.
		for k := range c {
			c[k] = capOf(k)
		}
		b.deep = b.deep || !b.bounded(st)
	}
	// Without a state: every unit of the depth, and at most one group per
	// evaluation.
	full, decide, groups := int64(c[dimErrs]), int64(c[dimErrs]), int64(c[dimEvals])
	var s *evalState
	if st >= 0 {
		s = &b.ws.states[st]
		own := s.c
		if parent != nil {
			own[dimWork] = min(own[dimWork]+parent.match[dimWork], countCap)
			own[dimScan] = min(own[dimScan]+parent.match[dimScan], countCap)
		}
		for i := range c {
			c[i] = min(c[i], own[i])
		}
		k := kindOf(v)
		sf, sd, sg := int64(s.full[k])+int64(s.names[k]), int64(s.decide[k]), int64(s.groups[k])
		if str, ok := v.(string); ok && len(s.lengths) > 0 {
			runes := utf8.RuneCountInString(str)
			for _, l := range s.lengths {
				f, d, g := l.u.at(runes)
				n := int64(l.n)
				sf = addCap(sf, mulCap(n, int64(f+l.u.names)))
				sd = addCap(sd, mulCap(n, int64(d)))
				sg = addCap(sg, mulCap(n, int64(g)))
			}
		}
		full, decide, groups = min(full, sf), min(decide, sd), min(groups, sg)
	}
	evals := int64(c[dimEvals])
	// The evaluations at the value run one chain of in-place evaluations at
	// a time: the call stack and the dynamic scope hold one chain per value
	// on the path, never more than the value's evaluations.
	chain := evals
	switch {
	case s != nil:
		chain = min(chain, int64(s.chain))
	case b.ws.flags.chain > 0:
		chain = min(chain, int64(b.ws.flags.chain))
	}
	frames = addCap(frames, chain)
	b.locate = addCap(b.locate, mulCap(full, errorBytes+locationBytes*int64(dd)))
	b.walk = addCap(b.walk, mulCap(full, walkWork(b.ws.flags.maxPointer)))
	work := addCap(int64(c[dimWork]), mulCap(addCap(evals, full), int64(dd)*locationUnits))
	// The cycle check of every evaluation walks the scope of its chain at
	// the value; a dynamic reference the whole path's.
	work = addCap(work, mulCap(evals, chain*scopeUnits))
	if b.ws.flags.dynamic {
		work = addCap(work, mulCap(evals, mulCap(frames, scopeUnits)))
	}
	work = addCap(work, mulCap(scanRate(c), int64(len(name))))
	b.matchers(s, parent, c, name, v)
	return valueCharge{
		st: s, c: c, full: full,
		decide: mulCap(addCap(decide, groups), decideErrorBytes),
		evals:  evals, frames: frames, work: work,
		hash: mulCap(int64(c[dimHash]), max(1, b.compare)),
		own:  mulCap(chain, stackBytes),
	}
}

// scanRate returns the work units per byte of a string or member name
// under counts c: the patterns and rune counts that scan it (dimScan) and
// the uniqueItems passes that hash it (dimHash).
func scanRate(c vec) int64 {
	return addCap(int64(c[dimScan]), mulCap(int64(c[dimHash]), hashByteUnits))
}

// matchers charges the largest transient copy a pattern may take matching
// the value v, a string, or its member name: a regexp matcher of the
// largest program that may match it (review finding: charged to every
// body, it took 64 MB of the budget for {}) and the backtracker's job
// stack (patternReach). Where the value's state is known (s), only the
// patterns its evaluations apply to the string and those its parent's
// apply to the name count (review finding: every pattern of the document
// was charged to every string a pattern or a uniqueItems pass reached, so
// small valid bodies were refused at a 64 KiB limit); elsewhere, where
// the counts c may scan it, every pattern of the document, for the longer
// of the two.
func (b *bodyScan) matchers(s, parent *evalState, c vec, name string, v any) {
	str, isString := v.(string)
	if s == nil {
		if c[dimScan] > 0 {
			b.transient = max(b.transient, b.ws.flags.patterns.bytes(max(len(name), len(str))))
		}
		return
	}
	if isString {
		b.transient = max(b.transient, s.valuePats.bytes(len(str)))
	}
	if parent != nil {
		b.transient = max(b.transient, parent.namePats.bytes(len(name)))
	}
}

// str charges a string of n bytes entered as a: the copy every evaluation
// makes, the patterns that scan it and the passes that hash it, and the
// uniqueItems comparisons; it returns the stack.
func (b *bodyScan) str(a valueCharge, n int64) int64 {
	work := addCap(a.work, mulCap(n, addCap(mulCap(a.evals, copyUnits), scanRate(a.c))))
	b.addWork(addCap(work, mulCap(a.hash, compareUnits+n/compareBytes)))
	if a.evals > 0 {
		b.transient = max(b.transient, n)
	}
	return a.own
}

// bounded reports whether st is a state whose evaluations stay within
// MaxEvaluations, so that a value in it may lie at or past the schema's
// nesting limit (minor review finding: the limit refused every value that
// deep, also the members of a free-form object that no subschema
// evaluates).
func (b *bodyScan) bounded(st int) bool {
	return st >= 0 && b.ws.states[st].c[dimEvals] <= MaxEvaluations
}

// child returns the state of a child of the value in state s: f picks it,
// or it is unknown below an unknown state.
func child(s *evalState, f func(*evalState) int) int {
	if s == nil {
		return unknownState
	}
	return f(s)
}

// value scans v at depth d in state st, a member named name ("" for an
// item or the root) of an object in state parent (nil when unknown or not
// a member), below frames evaluations on the path, and returns the stack
// and unevaluated copies on the costliest path from v down and the error
// tree the decision pass may hold for v.
func (b *bodyScan) value(v any, d int, name string, frames int64, st int, parent *evalState) (path, decide int64) {
	if d >= b.ws.limit && !b.bounded(st) {
		b.deep = true
		if b.numbers {
			b.bound(v, d)
		}
		return 0, 0
	}
	a := b.enter(d, name, frames, st, parent, v)
	c, work, own := a.c, a.work, a.own
	switch t := v.(type) {
	case string:
		b.extra = addCap(b.extra, stringExtra(int64(len(t))))
		return b.str(a, int64(len(t))), a.decide
	case json.Number:
		lit := string(t)
		b.extra = addCap(b.extra, stringExtra(int64(len(lit))))
		if b.numbers && (a.st == nil || c[dimParses] > 0 || c[dimHash] > 0 || b.comparing > 0) && !numberInRange(lit) {
			b.bad = true
			return 0, 0
		}
		if b.ws.flags.rats {
			b.locate = addCap(b.locate, mulCap(min(int64(c[dimParses]), a.full), ratCost(lit)))
		}
		work = addCap(work, mulCap(int64(c[dimParses]), numberWork(lit)))
		work = addCap(work, b.quoWork(a, lit))
		work = addCap(work, mulCap(a.hash, compareUnits+numberWork(lit)))
	case []any:
		return b.array(t, d, a)
	case map[string]any:
		return b.object(t, d, a)
	}
	b.addWork(work)
	return own, a.decide
}

// quoWork returns the value's part of the work of the multipleOf checks
// of the number lit entered as a (quoWork): those of its state, or where
// the state is unknown, as many as its parses, each against the largest
// divisor of the document.
func (b *bodyScan) quoWork(a valueCharge, lit string) int64 {
	n := int64(a.c[dimParses])
	if n == 0 {
		return 0
	}
	if s := a.st; s != nil {
		return quoWork(lit, min(n, int64(s.multiples)), int64(s.divNum), int64(s.divDen))
	}
	f := &b.ws.flags
	if f.divNum == 0 {
		return 0
	}
	return quoWork(lit, n, mulCap(n, int64(f.divNum)), mulCap(n, int64(f.divDen)))
}

// array scans the items of t, an array at depth d entered as a.
func (b *bodyScan) array(t []any, d int, a valueCharge) (path, decide int64) {
	c, work, own := a.c, a.work, a.own
	n := int64(len(t))
	b.extra = addCap(b.extra, arrayExtra(n))
	if b.ws.flags.uneItems {
		u := int64(c[dimUneval])
		work = addCap(work, mulCap(mulCap(u, n), unevalItemUnits))
		own = addCap(own, mulCap(u, itemMapBase+itemMapEntry*n))
	}
	work = addCap(work, mulCap(a.hash, compareUnits))
	if b.ws.flags.contains {
		// Every evaluation with contains lists the matching items while it
		// loops; the location pass keeps the lists of minContains and
		// maxContains in their errors.
		lists, kept := a.evals, a.evals
		if a.st != nil {
			lists, kept = min(lists, int64(a.st.matches)), min(kept, int64(a.st.kept))
		}
		own = addCap(own, mulCap(mulCap(lists, n), matchedItemBytes))
		b.locate = addCap(b.locate, mulCap(mulCap(kept, n), matchedItemBytes))
	}
	unique := c[dimUnique] > 0
	if unique && n > pairwiseItems {
		b.transient = max(b.transient, mulCap(n, dupItemBytes))
	}
	b.addWork(work)
	compare, comparing, objects := b.compare, b.comparing, b.objects
	if unique && n > 1 {
		b.compare = max(b.compare, comparisons(t))
	}
	if a.st != nil && a.st.compares {
		b.comparing++
	}
	b.objects = addCap(b.objects, int64(c[dimObjects]))
	var below, sum int64
	for i, e := range t {
		if b.stop() {
			break
		}
		p, dt := b.value(e, d+1, "", a.frames, child(a.st, func(s *evalState) int { return s.item(i) }), nil)
		below, sum = max(below, p), addCap(sum, dt)
	}
	b.compare, b.comparing, b.objects = compare, comparing, objects
	return addCap(own, below), addCap(a.decide, sum)
}

// object scans the members of t, an object at depth d entered as a. In
// the decision pass an evaluation looping over the members stops at the
// first that fails, so it keeps the errors of one member, unless
// unevaluatedProperties, which evaluates the members it reaches after the
// loop, keeps them all. Every evaluation with propertyNames validates every
// member name on its own, as the location pass does, and wraps the errors
// of a name that fails in an error of its own: at most one per such
// evaluation, and only for a name whose errors the scan cannot rule out
// (review finding: one wrapper per name, whether or not it could fail and
// however many evaluations applied propertyNames).
func (b *bodyScan) object(t map[string]any, d int, a valueCharge) (path, decide int64) {
	c, work, own := a.c, a.work, a.own
	n := int64(len(t))
	b.extra = addCap(b.extra, mapExtra(n))
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
	lists := a.evals
	if a.st != nil {
		lists = min(lists, int64(a.st.lists))
	}
	if b.ws.flags.lists {
		own = addCap(own, mulCap(mulCap(lists, n), listedNameBytes))
	}
	work = addCap(work, mulCap(a.hash, compareUnits*(1+n)))
	// The member names, hashed or compared per byte: by the properties
	// lookup of every evaluation with member keywords, the unevaluated
	// copies, the uniqueItems passes (looking each name up again and
	// sorting the names) and comparisons (looking each name up in the
	// other object), and by every const or enum object compared with this
	// one, which stops at the first name it lacks.
	var names, longest int64
	for k := range t {
		names, longest = names+int64(len(k)), max(longest, int64(len(k)))
	}
	lookups := a.evals
	if a.st != nil {
		lookups = min(lookups, int64(a.st.members))
	}
	hashes := mulCap(lookups, nameHashes)
	if b.ws.flags.uneProps {
		hashes = addCap(hashes, mulCap(int64(c[dimUneval]), unevalNameHashes))
	}
	hashes = addCap(hashes, mulCap(int64(c[dimHash]), 1+2*int64(bits.Len64(uint64(n)))))
	hashes = addCap(hashes, a.hash)
	objects := addCap(b.objects, int64(c[dimObjects]))
	work = addCap(work, addCap(mulCap(names, hashes), mulCap(longest, objects))/nameHashBytes)
	b.addWork(work)
	comparing, enclosing := b.comparing, b.objects
	if a.st != nil && a.st.compares {
		b.comparing++
	}
	b.objects = objects
	naming := a.evals
	if a.st != nil {
		naming = min(naming, int64(a.st.propNames))
	}
	var below, keys, sum, top int64
	for k, e := range t {
		if b.stop() {
			break
		}
		b.extra = addCap(b.extra, stringExtra(int64(len(k))))
		if b.ws.flags.names {
			// The name is a string value of its own (propertyNames),
			// validated on top of the current path; its errors, located
			// at the name, are kept in either pass, wrapped in one more
			// error by each evaluation applying propertyNames that it
			// fails, which takes at least one of its error units.
			ka := b.enter(d+1, "", a.frames, child(a.st, func(s *evalState) int { return s.key }), nil, k)
			below = max(below, b.str(ka, int64(len(k))))
			wrappers := min(naming, ka.full)
			b.locate = addCap(b.locate, mulCap(wrappers, errorBytes))
			keys = addCap(keys, mulCap(addCap(ka.full, wrappers), errorBytes))
		}
		p, dt := b.value(e, d+1, k, a.frames, child(a.st, func(s *evalState) int { return s.member(k) }), a.st)
		below, sum, top = max(below, p), addCap(sum, dt), max(top, dt)
	}
	b.comparing, b.objects = comparing, enclosing
	// The depth bound: every evaluation's units and a group each, and
	// every member's errors.
	decide = addCap(mulCap(addCap(int64(c[dimErrs]), a.evals), decideErrorBytes), sum)
	if s := a.st; s != nil {
		// The state's evaluations without member keywords of their own
		// are in a.decide; those with them keep one member's errors each
		// (all of them under unevaluatedProperties).
		loops := addCap(mulCap(int64(s.loose), decideErrorBytes), sum)
		if !s.uneval {
			loops = addCap(mulCap(int64(s.loose), decideErrorBytes), min(sum, mulCap(int64(s.members), top)))
			if len(s.loops) > 0 && s.loops[0].n >= 0 {
				exact := int64(0)
				for _, l := range s.loops {
					first := mulCap(int64(l.first), decideErrorBytes)
					after := addCap(mulCap(int64(l.after), decideErrorBytes), top)
					exact = addCap(exact, mulCap(int64(l.n), max(first, after)))
				}
				loops = min(loops, exact)
			}
		}
		decide = min(decide, addCap(a.decide, loops))
	}
	return addCap(own, below), addCap(decide, keys)
}

// comparisons returns the most other items that an item of t, an array
// with uniqueItems, may be compared with. Up to pairwiseItems items the
// library compares every pair. Past it, it compares an item only with the
// earlier items whose hash it shares, and its hash of a value keeps
// neither the sign of a number nor the bounds of the parts of an array or
// object: distinct strings, booleans and nulls never share one, integers
// share one with their negation, a number with the few numbers whose
// numerator and denominator bytes concatenate alike (hashColliders), and
// an array or object with any other.
func comparisons(t []any) int64 {
	n := int64(len(t))
	if n <= pairwiseItems {
		return n - 1
	}
	integers, most := true, int64(1)
	for _, e := range t {
		switch x := e.(type) {
		case []any, map[string]any:
			return n - 1
		case json.Number:
			lit := string(x)
			integers = integers && !strings.ContainsAny(lit, ".eE")
			k, ok := hashColliders(lit)
			if !ok {
				// A literal of l bytes has a numerator and denominator of
				// at most about l/2 + 2 bytes together, each split of which
				// gives two numbers.
				k = 2 * (ratCost(lit) - ratBytes + 3)
			}
			most = max(most, k)
		}
	}
	if integers {
		return 1
	}
	return min(n-1, most)
}

// hashColliders returns how many other numbers may share the uniqueItems
// hash of the number literal lit, whose rational p/q fits 64 bits: the
// library hashes the bytes of p then those of q, with neither sign nor
// bounds, so a number r/s shares it when the bytes of r and s concatenate
// alike, at most one per split of those bytes, with its negation. A JSON
// number's denominator divides a power of ten, so a split counts only
// where its second part is a product of twos and fives without a leading
// zero byte and coprime with the first (minor review finding: counting
// every split charged a short decimal 12 comparisons, so arrays of distinct
// prices under uniqueItems were refused above 12% of the limit; 18.4375
// and 0.0001, 295/16 and 1/10000, share their bytes). It reports false
// when p or q does not fit.
func hashColliders(lit string) (int64, bool) {
	p, q, ok := smallRat(lit)
	if !ok {
		return 0, false
	}
	var buf [16]byte
	n := putBytes(buf[:], p)
	n += putBytes(buf[n:], q)
	splits := int64(0)
	for k := range n {
		if buf[k] == 0 {
			// The second part would start with a zero byte.
			continue
		}
		if k > 8 || n-k > 8 {
			// A part past 64 bits: counted.
			splits++
			continue
		}
		r, s := bytesValue(buf[:k]), bytesValue(buf[k:n])
		if smooth(s) && gcd(r, s) == 1 {
			splits++
		}
	}
	return 2*splits - 1, true
}

// smallRat returns the reduced rational p/q the number literal lit denotes,
// without its sign, or false when p or q does not fit 64 bits.
func smallRat(lit string) (p, q uint64, ok bool) {
	lit = strings.TrimPrefix(lit, "-")
	mant, exp := lit, 0
	if i := exponentIndex(lit); i >= 0 {
		mant, exp = lit[:i], exponentOf(lit)
		if strings.HasPrefix(lit[i+1:], "-") {
			exp = -exp
		}
	}
	var m uint64
	f := 0
	if i := strings.IndexByte(mant, '.'); i >= 0 {
		f = len(mant) - 1 - i
	}
	for _, c := range []byte(mant) {
		if c == '.' {
			continue
		}
		if m > (math.MaxUint64-9)/10 {
			return 0, 0, false
		}
		m = m*10 + uint64(c-'0')
	}
	if m == 0 {
		return 0, 1, true
	}
	shift := exp - f
	q = 1
	for ; shift > 0; shift-- {
		if m > math.MaxUint64/10 {
			return 0, 0, false
		}
		m *= 10
	}
	for ; shift < 0; shift++ {
		if q > math.MaxUint64/10 {
			return 0, 0, false
		}
		q *= 10
	}
	g := gcd(m, q)
	return m / g, q / g, true
}

// putBytes writes the big-endian bytes of v without leading zeros, as
// big.Int.Bytes returns them, to buf and returns their count.
func putBytes(buf []byte, v uint64) int {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return copy(buf, b[8-(bits.Len64(v)+7)/8:])
}

// bytesValue returns the big-endian value of at most eight bytes.
func bytesValue(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// smooth reports whether v is a product of twos and fives.
func smooth(v uint64) bool {
	if v == 0 {
		return false
	}
	for v%2 == 0 {
		v /= 2
	}
	for v%5 == 0 {
		v /= 5
	}
	return v == 1
}

// gcd returns the greatest common divisor of a and b.
func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// bound checks every number of v, at depth d, against the number bounds,
// for the values past the nesting limit, which the scan does not follow;
// it stops below the decoder's nesting limit, which a decoded body never
// passes.
func (b *bodyScan) bound(v any, d int) {
	if d > maxNesting {
		return
	}
	switch t := v.(type) {
	case json.Number:
		b.bad = b.bad || !numberInRange(string(t))
	case []any:
		for _, e := range t {
			if b.bad {
				return
			}
			b.bound(e, d+1)
		}
	case map[string]any:
		for _, e := range t {
			if b.bad {
				return
			}
			b.bound(e, d+1)
		}
	}
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
	hi, lo := bits.Mul64(uint64(a), uint64(b)) //nolint:gosec // G115: the operands are non-negative counts and limits.
	if hi != 0 || lo > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(lo)
}
