// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/json"
	"math"
	"math/bits"
	"slices"
	"strings"
	"unicode/utf8"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"
)

// The working set and the work of validation (proposed, package
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
	// decideErrorBytes is what one error may hold in the decision pass,
	// where the library's errors carry no location, kind or causes of
	// their own (validator.go error): the 80-byte error and its slot in
	// the one list that holds it, up to 24 bytes while the list doubles. An
	// error unit is one error, and an evaluation that collects two or more
	// errors adds a group error.
	decideErrorBytes = 104
	// decideFrames are the evaluations the decision pass adds at the body
	// root: the wrapper that asks for a boolean result and its reference
	// to the schema.
	decideFrames = 3
	// listedNameBytes is what an evaluation with additionalProperties
	// false holds per member while it loops: the names it does not
	// declare, in a slice grown by appending.
	listedNameBytes = 48
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
	before := count(s.MaxProperties != nil, s.MinProperties != nil, len(s.Required) > 0)
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
	own[kindArray] = count(s.MinItems != nil, s.MaxItems != nil, s.UniqueItems, s.Contains != nil, s.MaxContains != nil, s.AdditionalItems == false)
	own[kindString] = count(s.MinLength != nil, s.MaxLength != nil) + others
	own[kindNumber] = count(s.Minimum != nil, s.Maximum != nil, s.ExclusiveMinimum != nil, s.ExclusiveMaximum != nil, s.MultipleOf != nil)
	loops := len(s.Properties) > 0 || len(s.PatternProperties) > 0 || s.AdditionalProperties != nil
	all := 2 * count(s.UnevaluatedProperties != nil || s.PropertyNames != nil)
	itemKeywords := s.Items != nil || s.Items2020 != nil || len(s.PrefixItems) > 0 || s.Contains != nil || s.UnevaluatedItems != nil || s.AdditionalItems != nil
	for k, name := range typeNames() {
		// The type keyword fails only on a value of a kind it does not
		// list (an integer type may fail on any number).
		typeFails := types != nil && !slices.Contains(types, name)
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
	// uniqueItems array may be compared with.
	compare int64
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
	if b.ws.flags.maxInsts > 0 {
		b.transient = max(b.transient, matcherBytes+matcherInstBytes*int64(b.ws.flags.maxInsts))
	}
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
// name) whose parent's patterns cost match to match it (nil when it is not
// a member or its parent's state is unknown), below frames evaluations on
// the path: it adds the location pass's errors and returns the rest.
func (b *bodyScan) enter(d int, name string, frames int64, st int, match *vec, v any) valueCharge {
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
		if match != nil {
			own[dimWork] = min(own[dimWork]+match[dimWork], countCap)
			own[dimScan] = min(own[dimScan]+match[dimScan], countCap)
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
	frames = addCap(frames, evals)
	b.locate = addCap(b.locate, mulCap(full, errorBytes+locationBytes*int64(dd)))
	b.walk = addCap(b.walk, mulCap(full, walkWork(b.ws.flags.maxPointer)))
	work := addCap(int64(c[dimWork]), mulCap(addCap(evals, full), int64(dd)*locationUnits))
	// The cycle check walks the evaluations at the value; a dynamic
	// reference the whole path.
	work = addCap(work, mulCap(evals, evals*scopeUnits))
	if b.ws.flags.dynamic {
		work = addCap(work, mulCap(evals, mulCap(frames, scopeUnits)))
	}
	work = addCap(work, mulCap(int64(c[dimScan]), int64(len(name))))
	return valueCharge{
		st: s, c: c, full: full,
		decide: mulCap(addCap(decide, groups), decideErrorBytes),
		evals:  evals, frames: frames, work: work,
		hash: mulCap(int64(c[dimHash]), max(1, b.compare)),
		own:  mulCap(evals, stackBytes),
	}
}

// str charges a string of n bytes entered as a: the copy every evaluation
// makes, the patterns that scan it, and the uniqueItems comparisons; it
// returns the stack.
func (b *bodyScan) str(a valueCharge, n int64) int64 {
	work := addCap(a.work, mulCap(n, addCap(mulCap(a.evals, copyUnits), int64(a.c[dimScan]))))
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
// item or the root) matched at match, below frames evaluations on the
// path, and returns the stack and unevaluated copies on the costliest path
// from v down and the error tree the decision pass may hold for v.
func (b *bodyScan) value(v any, d int, name string, frames int64, st int, match *vec) (path, decide int64) {
	if d >= b.ws.limit && !b.bounded(st) {
		b.deep = true
		if b.numbers {
			b.bound(v, d)
		}
		return 0, 0
	}
	a := b.enter(d, name, frames, st, match, v)
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
		work = addCap(work, mulCap(a.hash, compareUnits+numberWork(lit)))
	case []any:
		return b.array(t, d, a)
	case map[string]any:
		return b.object(t, d, a)
	}
	b.addWork(work)
	return own, a.decide
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
	unique := c[dimUnique] > 0
	if unique && n > pairwiseItems {
		b.transient = max(b.transient, mulCap(n, dupItemBytes))
	}
	b.addWork(work)
	compare, comparing := b.compare, b.comparing
	if unique && n > 1 {
		b.compare = max(b.compare, comparisons(t))
	}
	if a.st != nil && a.st.compares {
		b.comparing++
	}
	var below, sum int64
	for i, e := range t {
		if b.stop() {
			break
		}
		p, dt := b.value(e, d+1, "", a.frames, child(a.st, func(s *evalState) int { return s.item(i) }), nil)
		below, sum = max(below, p), addCap(sum, dt)
	}
	b.compare, b.comparing = compare, comparing
	return addCap(own, below), addCap(a.decide, sum)
}

// object scans the members of t, an object at depth d entered as a. In
// the decision pass an evaluation looping over the members stops at the
// first that fails, so it keeps the errors of one member, unless
// unevaluatedProperties, which evaluates the members it reaches after the
// loop, keeps them all; member names under propertyNames are validated on
// their own, as the location pass does.
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
	b.addWork(work)
	comparing := b.comparing
	if a.st != nil && a.st.compares {
		b.comparing++
	}
	var match *vec
	if a.st != nil {
		match = &a.st.match
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
			// at the name, are kept in either pass with the one naming it.
			ka := b.enter(d+1, "", a.frames, child(a.st, func(s *evalState) int { return s.key }), nil, k)
			below = max(below, b.str(ka, int64(len(k))))
			if ka.evals > 0 {
				b.locate = addCap(b.locate, errorBytes)
				keys = addCap(keys, mulCap(addCap(ka.full, 1), errorBytes))
			}
		}
		p, dt := b.value(e, d+1, k, a.frames, child(a.st, func(s *evalState) int { return s.member(k) }), match)
		below, sum, top = max(below, p), addCap(sum, dt), max(top, dt)
	}
	b.comparing = comparing
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
// share one with their negation, a number with the numbers whose
// numerator and denominator bytes concatenate alike (at most two per
// split of those bytes), and an array or object with any other.
func comparisons(t []any) int64 {
	n := int64(len(t))
	if n <= pairwiseItems {
		return n - 1
	}
	integers, longest := true, int64(0)
	for _, e := range t {
		switch x := e.(type) {
		case []any, map[string]any:
			return n - 1
		case json.Number:
			lit := string(x)
			integers = integers && !strings.ContainsAny(lit, ".eE")
			longest = max(longest, ratCost(lit)-ratBytes)
		}
	}
	if integers {
		return 1
	}
	// A literal of l bytes has a numerator and denominator of at most
	// about l/2 + 2 bytes together.
	return min(n-1, 2*(longest+3))
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
