// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// emailPattern is the WHATWG e-mail address pattern, a large pattern in
// common use (261 instructions as the library's engine compiles it).
const emailPattern = `^[a-zA-Z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`

// longNumber is a literal at the number bounds, the costliest to parse.
const longNumber = `1.777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777e-999`

// records returns n order items, an ordinary body.
func records(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"sku":"SKU-%05d-BLUE","qty":%d,"price":%d.99}`, i%100000, i%1000+1, 10+i%90)
	}
	b.WriteByte(']')
	return b.String()
}

// recordsSchema validates records.
const recordsSchema = `{"type":"array","items":{"$ref":"#/$defs/item"},"$defs":{"item":{"type":"object","required":["sku","qty"],` +
	`"properties":{"sku":{"type":"string","minLength":1,"pattern":"^SKU-[0-9]{5}-[A-Z]+$"},"qty":{"type":"integer","minimum":1,"maximum":1000},"price":{"type":"number","exclusiveMinimum":0}}}}}`

// workCases are bodies that make the library work in every way the model
// charges: per evaluation, per member, per string byte (copies and
// patterns, adversarial inputs included), per number literal, const and
// enum comparisons, uniqueItems hashing and pairwise comparison, the
// unevaluated copies, scope walks, and failing bodies.
func workCases() []struct{ name, schema, body string } {
	var enum, nums, pats, distinct []string
	for i := range 600 {
		enum = append(enum, fmt.Sprintf(`"v%d"`, i))
	}
	for i := range 2000 {
		nums = append(nums, fmt.Sprint(i))
	}
	for i := range 10 {
		pats = append(pats, fmt.Sprintf(`"^z%d":true`, i))
	}
	for i := range 20 {
		distinct = append(distinct, fmt.Sprintf("1.%02d%se-999", i, strings.Repeat("7", 110)))
	}
	longs := make([]string, 2000)
	for i := range longs {
		longs[i] = fmt.Sprintf("1.%04d%se-999", i, strings.Repeat("7", 100))
	}
	var objects strings.Builder
	objects.WriteByte('[')
	for i := range 5000 {
		if i > 0 {
			objects.WriteByte(',')
		}
		fmt.Fprintf(&objects, `{"a":%d,"b":"x%d","c":[1,2],"d":{"e":null}}`, i, i)
	}
	objects.WriteByte(']')
	return []struct{ name, schema, body string }{
		{"members, allOf of references", allOfRefs(100, `{"minProperties":0}`), wideObject(5000)},
		{"string copies, allOf of references", allOfRefs(20, `{"minLength":0}`), `"` + strings.Repeat("a", 256<<10) + `"`},
		{"multi-byte lengths, allOf of references", allOfRefs(20, `{"maxLength":100000000}`), `"` + strings.Repeat("é€😀", 20<<10) + `"`},
		{"enum of 600 strings", `{"items":{"enum":[` + strings.Join(enum, ",") + `]}}`, denseArray(2000, `"v599"`, 0)},
		{"enum of 600 strings, failing", `{"items":{"enum":[` + strings.Join(enum, ",") + `]}}`, denseArray(2000, `"x"`, 0)},
		{"short numbers", `{"items":{"minimum":0,"maximum":1e9,"type":"integer"}}`, denseArray(20000, `12345`, 0)},
		{"const array", `{"allOf":[` + strings.TrimSuffix(strings.Repeat(`{"const":[`+strings.Join(nums, ",")+`]},`, 20), ",") + `]}`, `[` + strings.Join(nums, ",") + `]`},
		{"const array, long literals", `{"allOf":[` + strings.TrimSuffix(strings.Repeat(`{"const":[`+strings.Join(longs, ",")+`]},`, 20), ",") + `]}`, `[` + strings.Join(longs, ",") + `]`},
		{"enum of numbers, long literals", `{"items":{"enum":[` + strings.Join(nums[:300], ",") + `]}}`, denseArray(50, longNumber, 0)},
		{"uniqueItems pairwise", `{"items":{"uniqueItems":true}}`, denseArray(50, `[`+strings.Join(distinct, ",")+`]`, 0)},
		{"uniqueItems hashed objects", `{"uniqueItems":true}`, objects.String()},
		{"uniqueItems colliding arrays", `{"uniqueItems":true}`, "[" + strings.Join(splits(strings.Repeat("a\x04", 9)+"a"), ",") + "]"},
		{"unevaluated names along a chain", unevaluatedChain(100, "unevaluatedProperties"), wideObject(1000)},
		{"unevaluated indexes along a chain", strings.Replace(unevaluatedChain(100, "unevaluatedItems"), "minProperties", "minItems", 1), denseArray(2000, `1`, 0)},
		{"unanchored pattern", `{"items":{"pattern":"a$"}}`, denseArray(16, `"`+strings.Repeat("a", 16384)+`"`, 0)},
		{"pattern with a large queue", `{"items":{"pattern":"[a-z]{100}x"}}`, denseArray(4, `"`+strings.Repeat("a", 4096)+`"`, 0)},
		{"e-mail pattern", `{"items":{"pattern":"` + emailPattern + `"}}`, denseArray(64, `"a@`+strings.Repeat("a", 2048)+`"`, 0)},
		{"patternProperties", `{"patternProperties":{` + strings.Join(pats, ",") + `}}`, wideObject(5000)},
		{"propertyNames", `{"propertyNames":{"pattern":"^k[0-9]+$"}}`, wideObject(5000)},
		{"failing oneOf of consts", `{"items":` + oneOfConsts(8) + `}`, denseArray(5000, `"x"`, 0)},
		{"failing object keywords", `{"items":{"type":"object","required":["a","b","c"],"minProperties":9,"maxProperties":1,"additionalProperties":false,"properties":{"z":{"type":"integer"}}}}`, denseArray(2500, `{"q":1,"y":2,"w":3,"z":"s"}`, 0)},
		{"dynamic references, nested", `{"$dynamicAnchor":"n","items":{"$dynamicRef":"#n"},"$defs":{"x":{"$id":"urn:x","$dynamicAnchor":"n","items":{"$dynamicRef":"#n"}}}}`, denseArray(500, strings.Repeat("[", 30)+"1"+strings.Repeat("]", 30), 0)},
		{"reference chain at one value", chain(4000, `{"$ref":"#/$defs/d%d","x":%d}`), `{"a":1}`},
		{"records", recordsSchema, records(5000)},
		// Review finding: the library hashes member names whole, in maps of
		// more than eight names.
		{"long names, properties lookups", allOfRefs(100, tenProperties), longMember(9, 256<<10)},
		{"long names, unevaluated copies", strings.Replace(unevaluatedChain(100, "unevaluatedProperties"), `{"minProperties":0}`, tenProperties, 1), longMember(9, 32<<10)},
		{"long names, uniqueItems objects", `{"uniqueItems":true}`, prefixedObjects(30, 10, 4<<10)},
		{"long names, enum objects", `{"items":{"enum":[` + strings.TrimSuffix(strings.Repeat(nineMembers+",", 500), ",") + `]}}`, denseArray(4, longMember(8, 256<<10), 0)},
		// Review finding: a multipleOf check divides the value by the
		// divisor as math/big rationals, whose work grows with the digits
		// of both.
		{"multipleOf 0.01, short decimals", `{"items":{"multipleOf":0.01}}`, decimals(20000)},
		{"multipleOf 1e-1000, short decimals", `{"items":{"multipleOf":1e-1000}}`, decimals(5000)},
		{"multipleOf of 127 bytes, short decimals", `{"items":{"multipleOf":` + longDivisor + `}}`, decimals(5000)},
		{"multipleOf 3e1000, long literals", `{"items":{"multipleOf":3e1000}}`, denseArray(2000, longDecimal, 0)},
		{"multipleOf of 127 bytes, long literals", `{"items":{"multipleOf":` + longDivisor + `}}`, denseArray(2000, longDecimal, 0)},
		{"anyOf of 16 multipleOf, long literals", `{"items":{"anyOf":[` + bigDivisors(16) + `]}}`, denseArray(500, longDecimal, 0)},
	}
}

// longDivisor and longDecimal are literals of 127 bytes near the number
// bounds: 0.(120 fives)e-990 and 0.(119 sevens)3e-990.
const (
	longDivisor = "0.555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555e-990"
	longDecimal = "0.777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777777773e-990"
)

// decimals returns an array of n short decimals, the shape of prices or
// measurements.
func decimals(n int) string {
	return "[" + joined(n, func(i int) string { return fmt.Sprintf("%d.%02d", i%1000, i%97) }) + "]"
}

// bigDivisors returns n multipleOf schemas whose divisors have large
// exponents, the review finding's anyOf.
func bigDivisors(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"multipleOf":%de%d}`, 3+i%6, 1000-i/6)
	}
	return strings.Join(items, ",")
}

// propertiesChain returns an allOf of n references to the head of a chain
// of four schemas with tenProperties each: 5n+1 evaluations at a value,
// 4n of them looking every member name up.
func propertiesChain(n int) string {
	props := strings.TrimPrefix(strings.TrimSuffix(tenProperties, "}"), "{")
	return `{"allOf":[` + strings.TrimSuffix(strings.Repeat(`{"$ref":"#/$defs/c0"},`, n), ",") + `],"$defs":{` +
		`"c0":{"$ref":"#/$defs/c1",` + props + `},"c1":{"$ref":"#/$defs/c2",` + props + `},"c2":{"$ref":"#/$defs/c3",` + props + `},"c3":` + tenProperties + `}}`
}

// tenProperties is a schema with ten properties, so the library looks a
// member name up by its hash.
const tenProperties = `{"properties":{"a":true,"b":true,"c":true,"d":true,"e":true,"f":true,"g":true,"h":true,"i":true,"j":true}}`

// nineMembers is an object of nine members, eight of which longMember(8,
// n) shares.
const nineMembers = `{"a":"x","b":"x","c":"x","d":"x","e":"x","f":"x","g":"x","h":"x","z":"x"}`

// prefixedObjects returns an array of n distinct objects of k members
// whose names share a prefix of l bytes, which sorting them compares.
func prefixedObjects(n, k, l int) string {
	prefix := strings.Repeat("p", l)
	items := make([]string, n)
	for i := range items {
		members := make([]string, k)
		for j := range members {
			members[j] = fmt.Sprintf(`"%s%02d":%d`, prefix, j, i)
		}
		items[i] = "{" + strings.Join(members, ",") + "}"
	}
	return "[" + strings.Join(items, ",") + "]"
}

// longMember returns an object of k short members a, b, ... and one more
// whose name is n bytes long.
func longMember(k, n int) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := range k {
		fmt.Fprintf(&b, `"%c":"x",`, 'a'+i)
	}
	fmt.Fprintf(&b, `"%s":"x"}`, strings.Repeat("n", n))
	return b.String()
}

// BenchmarkWorkModel measures the work model (proposed, package
// documentation "Work bound", 07 section 9 item 21) against the library:
// for every way the library's work grows, it reports the nanoseconds
// validating the body takes per work unit the scan charges (ns/unit),
// which must stay below 1 on the reference machine. A benchmark, not a
// test: the ratio depends on the machine and its load (review finding,
// no wall-clock time in unit tests); TestWorkCharges pins the model
// itself.
func BenchmarkWorkModel(b *testing.B) {
	for _, tc := range workCases() {
		b.Run(tc.name, func(b *testing.B) {
			s := mustCompile(b, tc.schema)
			v, scan := scanBody(b, s, tc.body)
			for b.Loop() {
				_ = s.root.Validate(v)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(scan.work), "ns/unit")
		})
	}
}

// TestWorkCharges pins the work model (proposed) on small bodies: each
// evaluation's weight, the cycle check, the location copies, and the terms
// that grow with the value (string bytes, number literals, members, the
// unevaluated copies, the pairwise uniqueItems comparisons).
//
// Requirements: 07 section 9 item 21.
func TestWorkCharges(t *testing.T) {
	const kw = evalUnits + keywordUnits
	for _, tc := range []struct {
		name, schema, body string
		want               int64
	}{
		{"string", `{"type":"string"}`, `"abc"`, kw + scopeUnits + 3*copyUnits},
		// Minor review finding: the runes of a string are counted per byte.
		{"lengths", `{"minLength":1}`, `"é"`, kw + scopeUnits + 2*(copyUnits+lengthByteUnits)},
		{"members", `{"minProperties":0}`, `{"a":1,"b":2}`, kw + scopeUnits + 2*memberUnits},
		{"number", `{"minimum":0}`, `1.5`, kw + scopeUnits + numberUnits + 3*numberByteUnits},
		// The engine's program: the no-op of noOnePass, ^, a, match and
		// fail; a queue of the no-op, ^ and a; a one-rune literal makes no
		// class search.
		{"pattern", `{"pattern":"^a"}`, `"aaaa"`, kw + matchUnits + 5*matchInstUnits + scopeUnits + 4*(copyUnits+byteUnits+3*queueUnits)},
		// The search in a class of eight ranges: four steps per queue
		// entry and byte.
		{"pattern class", `{"pattern":"^[a-bd-eg-hj-kmoqs]"}`, `"aaaa"`, kw + matchUnits + 5*matchInstUnits + scopeUnits + 4*(copyUnits+byteUnits+3*(queueUnits+4*searchUnits))},
		{"enum of strings", `{"enum":["ab","cd"]}`, `"x"`, kw + 2*compareUnits + scopeUnits + copyUnits},
		{"enum of numbers", `{"enum":[1,2]}`, `3`, kw + 2*(compareUnits+numberUnits+numberByteUnits) + scopeUnits + 2*(numberUnits+numberByteUnits)},
		// A number nested in a const value compares with a nested body
		// number, charged at the costliest literal (minor review finding).
		{"nested const number", `{"const":[1.5]}`, `[1.5]`, kw + 2*compareUnits + numberUnits + 3*numberByteUnits + maxNumberWork + scopeUnits},
		{
			// Minor review finding: a dynamic reference walks the whole scope,
			// the chains of every value on the path (1 at the root, 2 at the
			// item); the item's error unit copies its location.
			"dynamic reference", `{"$dynamicAnchor":"n","items":{"$dynamicRef":"#n"}}`, `[1]`,
			evalUnits + scopeUnits + scopeUnits +
				kw + evalUnits + 3*locationUnits + 2*2*scopeUnits + 2*3*scopeUnits,
		},
		{
			// Audit class GC-01 (rows VAL-47, REFS-32, REFS-34): a
			// $recursiveRef is one evaluation and an in-place edge, as a $ref
			// is, its cycle check walking its chain of two at the value, and
			// never the whole dynamic scope, which only a $recursiveAnchor of
			// true, refused at compile, would make it walk.
			"recursive reference", `{"$recursiveRef":"#/$defs/s","$defs":{"s":{"type":"string"}}}`, `"abc"`,
			2*kw + 2*2*scopeUnits + 2*3*copyUnits,
		},
		{
			"unevaluated names", `{"unevaluatedProperties":true}`, `{"a":1}`,
			// The member's true schema cannot fail: no error unit.
			evalUnits + scopeUnits + memberUnits + unevalPropUnits + evalUnits + scopeUnits + locationUnits,
		},
		{
			// Each of the 2 items is compared with the other one.
			"pairwise uniqueItems", `{"uniqueItems":true}`, `["a","b"]`,
			kw + scopeUnits + 2*(hashUnits+locationUnits+scopeUnits+(copyUnits+hashByteUnits)+compareUnits),
		},
		// Review finding: a member name is hashed per byte, by the properties
		// lookup of an evaluation with member keywords, by every unevaluated
		// copy, and by a const or enum object that lacks it.
		{"long name, properties", `{"properties":{"x":true}}`, `{"` + strings.Repeat("n", 800) + `":1}`, evalUnits + scopeUnits + memberUnits + 800*nameHashes/nameHashBytes},
		{
			"long name, unevaluated copy", `{"unevaluatedProperties":true}`, `{"` + strings.Repeat("n", 800) + `":1}`,
			evalUnits + scopeUnits + memberUnits + unevalPropUnits + evalUnits + scopeUnits + locationUnits + 800*unevalNameHashes/nameHashBytes,
		},
		{"long name, const object", `{"const":{"a":"x"}}`, `{"` + strings.Repeat("n", 800) + `":"x"}`, kw + 3*compareUnits + scopeUnits + memberUnits + 800/nameHashBytes},
		// The names of the schema's lists, looked up in the object.
		{"long required name", `{"required":["` + strings.Repeat("r", 800) + `"]}`, `{}`, kw + listUnits + 800/nameHashBytes + scopeUnits},
		// Review finding: a multipleOf check, 0.01 being 1/100 (a numerator
		// of one digit, a denominator of three) and 1.5 3/2 (two and two):
		// the divisor's part, the parse, the GCD's steps (the smaller
		// product, 1*2 against 3*2, three digits), the literal's digits and
		// the digit products, each rounded up to a unit.
		{"multipleOf", `{"multipleOf":0.01}`, `1.5`, kw + quoUnits + 4*quoDigitUnits + 1 + scopeUnits + numberUnits + 3*numberByteUnits + 3*quoMinUnits + 4*quoDigitUnits + 1 + 1},
		// Two checks against 1e-1000, 1/10^1000 (one digit and 1,001): the
		// smaller products sum to 2*(1+2), and the literal's four digits
		// meet the divisors' 2,004; three evaluations on a chain of two.
		{"two multipleOf", `{"allOf":[{"multipleOf":1e-1000},{"multipleOf":1e-1000}]}`, `1.5`, evalUnits + keywordUnits + 2*(kw+quoUnits+1002*quoDigitUnits+(1001+quoProductDigits-1)/quoProductDigits) +
			3*2*scopeUnits + 2*(numberUnits+3*numberByteUnits) + 6*quoMinUnits + 2*(4*quoDigitUnits+1) + (4*2004+quoProductDigits-1)/quoProductDigits},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, scan := scanBody(t, mustCompile(t, tc.schema), tc.body); scan.work != tc.want {
				t.Errorf("work = %d, want %d", scan.work, tc.want)
			}
		})
	}
	// Minor review finding (mutants that survived): the terms that grow
	// with a name, each the work of a name of 808 bytes less that of one of
	// 8 (%s in the schema and the body), 100 units per hash.
	const long = (808 - 8) / nameHashBytes
	for _, tc := range []struct {
		name, schema, body string
		want               int64
	}{
		// Each of two objects below a uniqueItems array: the hashing pass
		// hashes its name once plus twice the bits of its member count
		// (sorting), the comparison once and the hashing evaluation's
		// properties lookup once; its value is hashed after the name.
		{"uniqueItems objects", `{"uniqueItems":true}`, `[{"%s":1},{"%s":2}]`, 2 * (long*(1+(1+2*1)+1) + (808-8)*hashByteUnits)},
		// An object nested in a value under a const of two objects is
		// compared with both, per byte of its longest name.
		{"object inside a const value", `{"const":{"a":[{"b":1}]}}`, `{"a":[{"%s":1}]}`, 2 * long},
		{"object against enum objects", `{"enum":[{"a":1},{"b":2}]}`, `{"%s":1}`, 2 * long},
		// The keys a dependentSchemas or dependencies member names, looked
		// up in the object.
		{"dependentSchemas key", `{"dependentSchemas":{"%s":true}}`, `{}`, long},
		{"dependencies key of a list", `{"dependencies":{"%s":["b"]}}`, `{}`, long},
		{"dependencies key of a schema", `{"dependencies":{"%s":true}}`, `{}`, long},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var work [2]int64
			for i, n := range []int{8, 808} {
				name := strings.Repeat("n", n)
				_, scan := scanBody(t, mustCompile(t, strings.ReplaceAll(tc.schema, "%s", name)), strings.ReplaceAll(tc.body, "%s", name))
				work[i] = scan.work
			}
			if got := work[1] - work[0]; got != tc.want {
				t.Errorf("work of 800 more bytes of name = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestMultipleOfCounts is the regression test of the minor review finding
// that mutants of the multipleOf work term survived: a state counts each
// of its divisors once per evaluation, also for a schema evaluated twice
// at the value, and a value whose path is unknown is charged as many
// checks as its parses against the document's largest divisor, its
// numerator's and its denominator's digits each.
//
// Requirements: 07 section 9 item 21 and architecture R-77.
func TestMultipleOfCounts(t *testing.T) {
	// 1e-1000 is 1/10^1000: one digit and 1,001.
	s := mustCompile(t, `{"allOf":[{"$ref":"#/$defs/m"},{"$ref":"#/$defs/m"}],"$defs":{"m":{"multipleOf":1e-1000}}}`)
	if f := s.work.flags; f.divNum != 1 || f.divDen != 1001 {
		t.Errorf("the document's divisor: %d and %d digits, want 1 and 1,001", f.divNum, f.divDen)
	}
	root := &s.work.states[0]
	if root.multiples != 2 || root.divNum != 2 || root.divDen != 2002 {
		t.Errorf("the root state: %d checks of %d and %d digits, want 2 of 2 and 2,002", root.multiples, root.divNum, root.divDen)
	}
	scan := &bodyScan{ws: &s.work}
	if got, want := scan.quoWork(valueCharge{st: root, c: root.c}, "1.5"), quoWork("1.5", 2, 2, 2002); got != want {
		t.Errorf("quoWork in the root state = %d, want %d", got, want)
	}
	unknown := s.work
	unknown.states = nil
	n := int64(unknown.at[0][dimParses])
	scan = &bodyScan{ws: &unknown}
	if got, want := scan.quoWork(valueCharge{c: unknown.at[0]}, "1.5"), quoWork("1.5", n, n, 1001*n); got != want || n < 2 {
		t.Errorf("quoWork on an unknown path = %d, want %d for %d parses", got, want, n)
	}
}

// TestLiteralDigits pins the digit bounds of the multipleOf work model:
// a literal's numerator and denominator, and a divisor's, never have more
// decimal digits than literalDigits and ratDigits allow, on literals at
// and around the number bounds (review finding).
//
// Requirements: 07 section 9 item 21 and architecture R-77.
func TestLiteralDigits(t *testing.T) {
	for _, tc := range []struct {
		lit      string
		num, den int64
	}{
		{"123.45", 5, 3},
		{"7", 1, 1},
		{"1e3", 4, 1},
		{"1.5e-3", 2, 5},
		{"-0.25E+2", 3, 1},
		{"12E-1000", 2, 1001},
		{longDecimal, 121, 1111},
	} {
		if num, den := literalDigits(tc.lit); num != tc.num || den != tc.den {
			t.Errorf("literalDigits(%.20s) = %d, %d, want %d, %d", tc.lit, num, den, tc.num, tc.den)
		}
	}
	digits := func(n *big.Int) int64 { return int64(len(new(big.Int).Abs(n).String())) }
	lits := []string{"0", "-0", "0.001", "100", "1e-1000", "9e1000", "-123.456e-7", longDivisor, longDecimal, longNumber, "1" + strings.Repeat("0", 127)}
	for _, mant := range []string{"9", "12.5", "0.0625", strings.Repeat("9", 60) + "." + strings.Repeat("9", 60)} {
		for _, exp := range []string{"", "e0", "e-1", "e+17", "e-999", "E1000", "e-1000"} {
			lits = append(lits, mant+exp, "-"+mant+exp)
		}
	}
	for _, lit := range lits {
		r, ok := new(big.Rat).SetString(lit)
		if !ok {
			t.Fatalf("literal %q", lit)
		}
		num, den := literalDigits(lit)
		if digits(r.Num()) > num || digits(r.Denom()) > den {
			t.Errorf("literalDigits(%.30s) = %d, %d; the rational has %d and %d digits", lit, num, den, digits(r.Num()), digits(r.Denom()))
		}
		if c, d := ratDigits(r); int64(c) < digits(r.Num()) || int64(d) < digits(r.Denom()) {
			t.Errorf("ratDigits(%.30s) = %d, %d; the rational has %d and %d digits", lit, c, d, digits(r.Num()), digits(r.Denom()))
		}
	}
}

// splits returns the arrays of strings that split s at each subset of its
// \u0004 characters, which the library's uniqueItems hash cannot tell
// apart: it writes a string as the byte 4 and its bytes, with no length.
func splits(s string) []string {
	parts := strings.Split(s, "\x04")
	var out []string
	for mask := range 1 << (len(parts) - 1) {
		var items []string
		cur := parts[0]
		for i, p := range parts[1:] {
			if mask&(1<<i) != 0 {
				items = append(items, cur)
				cur = p
			} else {
				cur += "\x04" + p
			}
		}
		items = append(items, cur)
		quoted := make([]string, len(items))
		for i, it := range items {
			quoted[i] = `"` + strings.ReplaceAll(it, "\x04", `\u0004`) + `"`
		}
		out = append(out, "["+strings.Join(quoted, ",")+"]")
	}
	return out
}

// TestUniqueItemsComparisons is the regression test of the minor review
// finding on the uniqueItems hash (validator util.go duplicates and
// writeHash): past pairwiseItems items the library compares an item with
// the earlier items of its hash bucket, and its hash of a value keeps
// neither a number's sign nor the bounds of an array's or object's parts,
// so distinct items can share a bucket. Arrays and objects may share one
// with every other item, a decimal with the few numbers whose numerator
// and denominator bytes concatenate alike; strings, booleans, nulls and
// integers (with their negation) never. 2,048 distinct arrays that all
// share one bucket are charged every pair, over the work limit when the
// body is at the raw limit.
//
// Requirements: 07 section 9 item 21.
func TestUniqueItemsComparisons(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int64
	}{
		{denseArray(5, `"x"`, 0), 4},
		{`[` + strings.TrimSuffix(strings.Repeat(`"a",`, 30), ",") + `]`, 1},
		{`[` + strings.TrimSuffix(strings.Repeat(`-12,`, 30), ",") + `]`, 1},
		// 5/4: no other split of its bytes is a JSON number, so only -1.25
		// shares its hash (minor review finding: every split was counted).
		{`[` + strings.TrimSuffix(strings.Repeat(`1.25,`, 29), ",") + `,true,null]`, 1},
		// 18.4375 is 295/16, bytes 01 27 10, which split as 1/10000 too:
		// 0.0001 and both negations.
		{`[` + strings.TrimSuffix(strings.Repeat(`1.25,`, 29), ",") + `,18.4375]`, 3},
		// 0.0015625 is 1/640, bytes 01 02 80: 258/128 is no number's
		// reduced rational, so only its negation shares its hash (minor
		// review finding: a mutant without the common-divisor test
		// survived).
		{`[` + strings.TrimSuffix(strings.Repeat(`1.25,`, 29), ",") + `,0.0015625]`, 1},
		// Past 64 bits, every split of about half the literal's bytes.
		{`[` + strings.Repeat(`1.25,`, 1999) + longNumber + `]`, 2 * (ratCost(longNumber) - ratBytes + 3)},
		{`[` + strings.TrimSuffix(strings.Repeat(`"a",`, 29), ",") + `,[]]`, 29},
	} {
		v, _ := decodeInstance(t, tc.body).([]any)
		if got := comparisons(v); got != tc.want {
			t.Errorf("comparisons(%.40s) = %d, want %d", tc.body, got, tc.want)
		}
	}
	// Decimals that share a hash, the library's bytes of the numerator
	// then the denominator, are counted for each of them: for every
	// p/(2^i 5^j), the numbers whose bytes concatenate alike, with their
	// negations.
	groups := map[string][]string{}
	for p := int64(1); p < 3000; p++ {
		for i := range 9 {
			for j := range 9 {
				r := big.NewRat(p, int64(math.Pow(2, float64(i))*math.Pow(5, float64(j))))
				k := string(r.Num().Bytes()) + string(r.Denom().Bytes())
				// One literal per value, its shortest exact decimal.
				lit := r.FloatString(max(i, j))
				if strings.Contains(lit, ".") {
					lit = strings.TrimSuffix(strings.TrimRight(lit, "0"), ".")
				}
				if !slices.Contains(groups[k], lit) {
					groups[k] = append(groups[k], lit)
				}
			}
		}
	}
	shared := 0
	for _, lits := range groups {
		if len(lits) > 1 {
			shared++
		}
		for _, lit := range lits {
			if k, ok := hashColliders(lit); !ok || k < int64(2*len(lits)-1) {
				t.Errorf("hashColliders(%s) = %d, %v; %d numbers share its hash", lit, k, ok, 2*len(lits))
			}
		}
	}
	if shared == 0 {
		t.Error("no decimals share a hash")
	}
	colliding := splits(strings.Repeat("a\x04", 11) + "a")
	body := "[" + strings.Join(colliding, ",") + "]"
	s := mustCompile(t, `{"uniqueItems":true}`)
	_, scan := scanBody(t, s, body)
	n := int64(len(colliding))
	if scan.work < n*(n-1)*compareUnits {
		t.Errorf("%d colliding arrays: work %d, want every pair charged (%d)", n, scan.work, n*(n-1)*compareUnits)
	}
	// At a raw limit of the body itself, the square of the array is over
	// the work limit.
	f := buildFilter(t, `{"uniqueItems":true}`, int64(len(body)))
	x, _ := exchange("application/json", []byte(body))
	if r := f.Handle(t.Context(), phase.OnRequestBody, x); !errors.Is(r.Err, errWorkTooLarge) {
		t.Errorf("Handle(%d colliding arrays) = %+v, want the work ErrTooLarge", n, r)
	}
	f = buildFilter(t, `{"uniqueItems":true}`, 1<<20)
	// 10,000 distinct strings share no bucket: cheap, and valid.
	strs := make([]string, 10000)
	for i := range strs {
		strs[i] = fmt.Sprintf(`"s%d"`, i)
	}
	if r := handle(t, f, "["+strings.Join(strs, ",")+"]"); r.Outcome != filter.Continue {
		t.Errorf("10,000 distinct strings: %+v, want Continue", r)
	}
}

// TestHandleWorkBound is the regression test of the review finding that
// validation work grows with the value as well as with the evaluations:
// through Factory.Build and Handle at the 10 MiB default, bodies that
// would take seconds of uncancellable work (many evaluations at a wide
// object or a long string, comparisons with a large const) are
// filter.ErrTooLarge before the library runs, without reserving anything.
//
// Requirements: 07 section 9 item 21.
func TestHandleWorkBound(t *testing.T) {
	var nums []string
	for i := range 20000 {
		nums = append(nums, fmt.Sprint(i))
	}
	bigConst := `{"const":[` + strings.Join(nums, ",") + `]}`
	for _, tc := range []struct{ name, schema, body string }{
		{"members", allOfRefs(800, `{"minProperties":0}`), wideObject(30000)},
		{"string copies", allOfRefs(800, `{"minLength":0}`), `"` + strings.Repeat("a", 2<<20) + `"`},
		{"const comparisons", `{"allOf":[` + strings.TrimSuffix(strings.Repeat(`{"$ref":"#/$defs/c"},`, 200), ",") + `],"$defs":{"c":` + bigConst + `}}`, `[` + strings.Join(nums, ",") + `]`},
		// Review finding: a 10 MiB member name, which the properties lookup of
		// 3,200 evaluations hashes whole, about 1.6 s of the library charged
		// nothing per byte.
		{"long member name", propertiesChain(800), `{"` + strings.Repeat("n", 10<<20-64) + `":1}`},
		// Review finding: 806,202 short decimals checked against a divisor
		// of 127 bytes, 52% of the limit, took 6.0 to 6.5 s, charged 0.49 of
		// the work limit as parses of the body literals.
		{"multipleOf of 127 bytes", `{"type":"array","items":{"multipleOf":` + longDivisor + `}}`, "[" + joined(806202, func(i int) string { return fmt.Sprintf("%d.%d", i%1000, i%97) }) + "]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFilter(t, tc.schema, 0)
			x, _ := exchange("application/json", []byte(tc.body))
			px := &peakExchange{Exchange: x}
			r := f.Handle(t.Context(), phase.OnRequestBody, px)
			if r.Outcome != filter.CannotDecide || !errors.Is(r.Err, errWorkTooLarge) || !errors.Is(r.Err, filter.ErrTooLarge) {
				t.Fatalf("Handle = %+v, want CannotDecide with the work ErrTooLarge", r)
			}
			if px.peak != 0 || x.Budget.Reserved != 0 {
				t.Errorf("reserved %d, left %d; want nothing", px.peak, x.Budget.Reserved)
			}
		})
	}
}

// lineSchema validates arrays of line items, the review finding's plain
// object-array schema.
const lineSchema = `{"type":"array","items":{"type":"object","required":["sku","qty"],"properties":{"sku":{"type":"string","minLength":1},` +
	`"qty":{"type":"integer","minimum":1,"maximum":1000},"price":{"type":"number","exclusiveMinimum":0}}}}`

// TestHandleAcceptsLargeBodies is the regression test of the review findings
// that valid ordinary bodies were refused far inside the raw limit
// (architecture R-63 and R-77: typical records fit up to about 70 to 84% of
// the limit; 07 reqs 13, 15 and 80): through Factory.Build and Handle at the
// 10 MiB default, an order-item array at 80% of the limit and a line-item
// array at 69% pass, the decoded values (about 4.9 and 5.7 times the body)
// being what limits them, within the reservation limit; a long string under
// a pattern, a base64-class string and an array of enum values pass too. A
// valid body reserves the decoded values, with the bytes their maps, arrays
// and strings hold beyond the decoder's charge
// (TestHandleReservesDecodedMaps), and the decision pass's working set,
// never the location pass's, and at most two and a half times the live heap
// at Handle's peak, measured at the reservation and at the probes and
// matches heapMeter spreads over the decision pass to its end (audit class
// GC-08; the sampled heap in use it replaces counted the garbage of the
// decode and of validation), four and a half times for the enum values, each
// of which the decision pass is charged an error it may hold, 104 bytes
// against its 7; work is never reserved.
func TestHandleAcceptsLargeBodies(t *testing.T) {
	enum := make([]string, 600)
	for i := range enum {
		enum[i] = fmt.Sprintf(`"v%d"`, i)
	}
	// The fractions do not depend on the limit: under -race, which slows
	// the library tenfold, a tenth of the default limit and of the bodies.
	limit, scale, raw := int64(10<<20), 1, int64(0)
	if raceEnabled {
		limit, scale, raw = 1<<20, 10, 1<<20
	}
	for _, tc := range []struct {
		name, schema, body string
		fraction           float64
		// most is the largest reservation allowed, in times the live heap
		// at Handle's peak.
		most float64
	}{
		{"order items", recordsSchema, records(171000 / scale), 0.79, 2.5},
		{"line items", lineSchema, denseArray(176000/scale, `{"sku":"ABC-12345","qty":2,"price":9.99}`, 0), 0.68, 2.5},
		{"long string under a pattern", `{"type":"string","pattern":"^a"}`, `"` + strings.Repeat("a", 7<<20/scale) + `"`, 0.7, 2.5},
		{"long string under a character class", `{"type":"string","pattern":"^[A-Za-z0-9+/]*={0,2}$"}`, `"` + strings.Repeat("QUJD", 2<<20/scale) + `"`, 0.8, 2.5},
		{"array of enum values", `{"type":"array","items":{"enum":[` + strings.Join(enum, ",") + `]}}`, denseArray(100000/scale, `"v599"`, 0), 0.06, 4.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := float64(len(tc.body)) / float64(limit); got < tc.fraction {
				t.Fatalf("body is %.2f of the limit, want at least %.2f", got, tc.fraction)
			}
			f := buildFilter(t, tc.schema, raw)
			m := &heapMeter{}
			r, reserved := handlePeak(t, f, tc.schema, tc.body, m)
			if r.Outcome != filter.Continue {
				t.Fatalf("Handle = %+v, want Continue", r)
			}
			_, scan := scanBody(t, f.schema, tc.body)
			peak := m.peak()
			t.Logf("body %d bytes: reserved %d, live peak %d (%.2f), work %d of %d", len(tc.body), reserved, peak, float64(reserved)/float64(peak), scan.work, f.limits.work)
			if float64(reserved) > tc.most*float64(peak) {
				t.Errorf("reserved %d, more than %.1f times the live peak %d", reserved, tc.most, peak)
			}
			if _, cost, _ := f.decode([]byte(tc.body)); reserved != cost+scan.extra+scan.decideMemory() {
				t.Errorf("reserved %d, want the decoded values %d, their maps and arrays %d and the decision pass's working set %d", reserved, cost, scan.extra, scan.decideMemory())
			}
		})
	}
}

// TestHandleReservesDecodedMaps is the regression test of the review finding
// that the decoded values were reserved at the decoder's cost, which leaves
// out the hash table of every object (internal/jsonval charges a one-member
// object 80 bytes; it holds 336) and the spare capacity of every array:
// through Factory.Build and Handle at the 10 MiB default, valid arrays of
// one-, two- and nine-member objects and of five-item arrays, and one wide
// object, at about a quarter of the limit, held up to 3.8 times what Handle
// reserved (07 req 82, architecture R-63, filter.Exchange.Reserve). The
// reservation now covers the decoded tree, measured after a collection, and
// the live heap at Handle's peak, measured at the reservation and at the
// probes of the decision pass, both but for heapNoise (heapMeter, audit
// class GC-08: the sampled heap in use this replaces counted the tables a
// wide object's map outgrew and the arrays the decoder outgrew, garbage
// before anything is reserved, and reached 1.41 times the reservation under
// load); strings are charged their allocation size class, or below 16 bytes
// their share of the allocator's blocks (TestTinyBlocks).
func TestHandleReservesDecodedMaps(t *testing.T) {
	limit, scale, raw := int64(10<<20), 1, int64(0)
	if raceEnabled {
		limit, scale, raw = 1<<19, 20, 1<<19
	}
	nine := `{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7,"h":8,"i":9}`
	for _, tc := range []struct {
		name, schema, body string
		fraction           float64
	}{
		// Arrays of one-member objects hold 47 times their bytes decoded:
		// the reservation limit binds at about a quarter of the raw limit
		// (TestHandleReservationLimit).
		{"one member", `{"type":"array","items":{"type":"object"}}`, denseArray(320000/scale, `{"a":1}`, 0), 0.24},
		{"one typed member", `{"type":"array","items":{"type":"object","properties":{"a":{"type":"integer"}}}}`, denseArray(320000/scale, `{"a":1}`, 0), 0.24},
		{"two members", `{"type":"array","items":{"type":"object","required":["id","ok"],"properties":{"id":{"type":"integer"},"ok":{"type":"boolean"}}}}`, denseArray(150000/scale, `{"id":7,"ok":true}`, 0), 0.25},
		{"nine members", `{"type":"array","items":{"type":"object"}}`, denseArray(50000/scale, nine, 0), 0.25},
		{"five items", `{"type":"array","items":{"type":"array"}}`, denseArray(150000/scale, `[1,2,3,4,5]`, 0), 0.15},
		// Strings of 33 bytes take the 48-byte size class.
		{"strings", `{"type":"array","items":{"type":"string"}}`, denseArray(80000/scale, `"`+strings.Repeat("s", 33)+`"`, 0), 0.25},
		// Minor review finding: member names of 1,025 bytes take the
		// 1,152-byte size class.
		{"long member names", `{"type":"array","items":{"type":"object"}}`, denseArray(2000/scale, `{"`+strings.Repeat("n", 1025)+`":1}`, 0), 0.19},
		{"one wide object", `{"type":"object"}`, wideObject(250000 / scale), 0.2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := float64(len(tc.body)) / float64(limit); got < tc.fraction {
				t.Fatalf("body is %.2f of the limit, want at least %.2f", got, tc.fraction)
			}
			f := buildFilter(t, tc.schema, raw)
			m := &heapMeter{}
			r, reserved := handlePeak(t, f, tc.schema, tc.body, m)
			if r.Outcome != filter.Continue {
				t.Fatalf("Handle = %+v, want Continue", r)
			}
			base := liveHeap()
			v, cost, err := f.decode([]byte(tc.body))
			live := growth(liveHeap(), base)
			runtime.KeepAlive(v)
			if err != nil {
				t.Fatal(err)
			}
			_, scan := scanBody(t, f.schema, tc.body)
			peak := m.peak()
			if raceEnabled {
				// The race detector turns off the allocator's packing of
				// small strings: each takes a size class of its own.
				live -= raceTinyBytes(v)
				peak -= raceTinyBytes(v)
			}
			t.Logf("decoded tree %d, charged %d and %d for its maps, arrays and strings; reserved %d, live peak %d (%.2f)", live, cost, scan.extra, reserved, peak, float64(peak)/float64(reserved))
			if exceeds(live, cost+scan.extra) {
				t.Errorf("the decoded tree holds %d bytes, Handle reserves %d for it", live, cost+scan.extra)
			}
			if exceeds(peak, reserved) {
				t.Errorf("Handle held %d bytes at its peak, reserved %d", peak, reserved)
			}
		})
	}
	// The model: a map of up to 8 members is one group of 8 slots, larger
	// ones a power of two slots filled to at most 7/8, and past one table
	// of 1,024 slots, 96 bytes per member.
	for _, tc := range []struct{ n, want int64 }{
		{0, 0}, {1, 256}, {8, 32}, {9, 16*mapSlotBytes*5/4 + mapTableBytes - 9*32}, {897, 897 * (96 - 32)},
	} {
		if got := mapExtra(tc.n); got != tc.want {
			t.Errorf("mapExtra(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
	// A string: of one byte nothing more, then up to 15 bytes twice its
	// bytes up to a whole tiny block (TestTinyBlocks), from 16 bytes its
	// size class, whole pages past 32 KiB.
	for _, tc := range []struct{ n, want int64 }{
		{0, 0}, {1, 0}, {2, 2}, {5, 5}, {8, 8}, {9, 7}, {15, 1}, {16, 0}, {17, 7}, {33, 15}, {32 << 10, 0}, {32<<10 + 1, 8<<10 - 1},
	} {
		if got := stringExtra(tc.n); got != tc.want {
			t.Errorf("stringExtra(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
	// An array: the capacity append grows it to up to 1,023 items, then at
	// most two thirds spare up to 4,095, 0.31 past.
	for _, tc := range []struct{ n, want int64 }{{0, 0}, {1, 16}, {5, 3 * 16}, {33, 38 * 16}, {1023, 0}, {1536, 1152 * 16}, {8192, 3072 * 16}} {
		if got := arrayExtra(tc.n); got != tc.want {
			t.Errorf("arrayExtra(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

// raceTinyBytes returns what a -race build's allocator adds to the decoded
// strings, member names and number literals of v beyond their charge: it
// gives every allocation shorter than 16 bytes a block of its own, the
// runtime's static one-byte strings aside.
func raceTinyBytes(v any) int64 {
	var n int64
	eachString(v, func(size int) {
		if size > 1 && size < tinyBlockBytes {
			n += tinyBlockBytes - int64(size) - stringExtra(int64(size))
		}
	})
	return n
}

// eachString calls f with the length of every string, member name and
// number literal of v.
func eachString(v any, f func(size int)) {
	switch t := v.(type) {
	case string:
		f(len(t))
	case json.Number:
		f(len(t))
	case []any:
		for _, e := range t {
			eachString(e, f)
		}
	case map[string]any:
		for k, e := range t {
			f(len(k))
			eachString(e, f)
		}
	}
}

// tinyBlocks replays the allocator's tiny allocator (runtime mallocgcTiny
// in Go 1.26 and 1.27, without -race) on allocations of sizes: each is
// aligned to 8, 4 or 2 bytes when its size is a multiple of them and
// placed in the current 16-byte block when it fits there, else in a new
// block, which becomes the current one when it has more room left. It
// returns the bytes each block holds and the current block's index.
func tinyBlocks(sizes []int) (blocks []int, current int) {
	current = -1
	offset := 0
	for _, size := range sizes {
		align := 1
		switch {
		case size%8 == 0:
			align = 8
		case size%4 == 0:
			align = 4
		case size%2 == 0:
			align = 2
		}
		at := (offset + align - 1) / align * align
		if current >= 0 && at+size <= tinyBlockBytes {
			blocks[current] += size
			offset = at + size
			continue
		}
		blocks = append(blocks, size)
		if current < 0 || size < offset {
			current, offset = len(blocks)-1, size
		}
	}
	return blocks, current
}

// TestTinyBlocks covers the charge of a decoded string, member name or
// number literal shorter than 16 bytes (review finding, audit class GC-08:
// charged its bytes alone, an array of 9-byte strings, each alone in its
// tiny block, held more than its reservation). A one-byte string takes
// nothing, being static. Every block the tiny allocator leaves holds at
// least tinyBlockFill bytes of allocations of 2 bytes or more, for every
// sequence of up to five sizes and for random longer ones, the fewest in a
// block that holds a 2-, 4- and 2-byte allocation before a 7-byte one
// needs a block of its own; so a string is charged at most its own block
// or twice its bytes, and every block but the current one is charged
// whole.
//
// The allocator itself, for strings of every such length, alone and in
// the sequences that leave the most room unused, built as the decoder
// builds them: the blocks their bytes lie in, counted by address, are the
// replica's but for tinyApart, and are charged; the live heap they add
// holds nothing besides those blocks, but for heapNoise (audit class
// GC-08: measured as a live heap growth beside the charge, with no
// allowance, a thread the scheduler started under load failed the test).
func TestTinyBlocks(t *testing.T) {
	least, at := tinyBlockBytes, []int(nil)
	check := func(sizes []int) {
		blocks, current := tinyBlocks(sizes)
		for i, b := range blocks {
			if i != current && b < least {
				least, at = b, slices.Clone(sizes)
			}
		}
	}
	var sizes []int
	var all func(depth int)
	all = func(depth int) {
		if depth == 0 {
			return
		}
		for size := 2; size < tinyBlockBytes; size++ {
			sizes = append(sizes, size)
			check(sizes)
			all(depth - 1)
			sizes = sizes[:len(sizes)-1]
		}
	}
	all(5)
	rng := rand.New(rand.NewPCG(5, 6)) //nolint:gosec // G404: fixed-seed sequences make the test deterministic; nothing here is secret.
	for range 20000 {
		sizes = sizes[:0]
		for range 1 + rng.IntN(60) {
			sizes = append(sizes, 2+rng.IntN(tinyBlockBytes-2))
		}
		check(sizes)
	}
	if least != tinyBlockFill || !slices.Equal(at, []int{2, 4, 2, 7}) {
		t.Errorf("the fewest bytes a block left holds: %d after %v, want %d after [2 4 2 7]", least, at, tinyBlockFill)
	}
	// With one-byte allocations, which strings never make, a block could
	// be left holding 6 bytes.
	if blocks, current := tinyBlocks([]int{1, 4, 1, 8}); current != 1 || blocks[0] != 6 {
		t.Errorf("tinyBlocks(1, 4, 1, 8) = %v, current %d, want the first block left with 6 bytes", blocks, current)
	}
	if raceEnabled {
		t.Skip("the race detector gives every small allocation a block of its own")
	}
	for _, seq := range [][]int{
		{1},
		{2},
		{3},
		{4},
		{5},
		{6},
		{7},
		{8},
		{9},
		{10},
		{11},
		{12},
		{13},
		{14},
		{15},
		{2, 4, 2, 7},
		{1, 4, 1, 8},
		{9, 8},
		{9, 2, 9},
		{15, 2},
		{10, 7},
		{7, 10},
		{3, 15},
	} {
		got := tinyCensus(seq)
		t.Logf("%v: %d blocks, replica %d, live %d, charged %d (%.3f)", seq, got.blocks, got.replica, got.live, got.charged, float64(got.live)/float64(got.charged))
		if whole := tinyBlockBytes * int64(max(got.replica-1, 0)); got.charged < whole {
			t.Errorf("%v: %d strings charged %d, under the %d of the replica's blocks but the current one", seq, tinyStrings, got.charged, whole)
		}
		if got.blocks < got.replica-tinyApart || got.blocks > got.replica+tinyApart {
			t.Errorf("%v: %d strings lie in %d blocks, the replica of the tiny allocator places them in %d", seq, tinyStrings, got.blocks, got.replica)
		}
		if got.statics > 1 {
			t.Errorf("%v: the one-byte strings lie at %d addresses, want the one static byte", seq, got.statics)
		}
		if exceeds(got.live, tinyBlockBytes*int64(got.blocks)) {
			t.Errorf("%v: %d strings added %d bytes to the live heap, their %d blocks hold %d", seq, tinyStrings, got.live, got.blocks, tinyBlockBytes*got.blocks)
		}
	}
}

// tinyStrings is how many strings tinyCensus builds, and tinyApart how
// many blocks the allocator's may differ from the replica's by: the
// allocator starts in its current block, which the replica does not know,
// and when the scheduler moves the goroutine to another processor, or
// runs another one that allocates small objects on its processor, the
// strings go on in another block.
const tinyStrings, tinyApart = 60000, 8

// census is what tinyCensus measures: the 16-byte blocks the strings'
// bytes lie in and the blocks the replica places them in, the addresses
// of the one-byte strings, the live heap growth and the charge.
type census struct {
	blocks, replica, statics int
	live, charged            int64
}

// tinyCensus builds tinyStrings strings of the lengths of seq in turn, as
// the decoder builds them, with the collector off so that no collection
// starts a new block, and measures them.
func tinyCensus(seq []int) census {
	var c census
	src := []byte(strings.Repeat("s", tinyBlockBytes))
	sizes, allocated := make([]int, tinyStrings), make([]int, 0, tinyStrings)
	for i := range sizes {
		sizes[i] = seq[i%len(seq)]
		c.charged += int64(sizes[i]) + stringExtra(int64(sizes[i]))
		if sizes[i] > 1 {
			allocated = append(allocated, sizes[i])
		}
	}
	replica, _ := tinyBlocks(allocated)
	c.replica = len(replica)
	keep := make([]string, tinyStrings)
	gc := debug.SetGCPercent(-1)
	base := liveHeap()
	for i := range keep {
		keep[i] = string(src[:sizes[i]])
	}
	c.live = growth(liveHeap(), base)
	debug.SetGCPercent(gc)
	runtime.KeepAlive(sizes)
	runtime.KeepAlive(src)
	blocks, statics := map[uintptr]bool{}, map[uintptr]bool{}
	for _, s := range keep {
		if p := reflect.ValueOf(s).Pointer(); len(s) == 1 {
			statics[p] = true
		} else {
			blocks[p/tinyBlockBytes] = true
		}
	}
	c.blocks, c.statics = len(blocks), len(statics)
	return c
}

// TestWorkLimit covers the work limit (proposed): WorkPerByte work units
// per byte of maxRequestBodyBytes, the 10 MiB default when unset, and
// below the saturation of the counts however large the limit; a body at
// the limit validates, one unit above it is filter.ErrTooLarge, and past
// the limit a number out of range is still the deterministic 400 (minor
// review findings).
//
// Requirements: 07 section 9 item 21.
func TestWorkLimit(t *testing.T) {
	body := records(50)
	f := buildFilter(t, recordsSchema, testLimit)
	_, scan := scanBody(t, f.schema, body)
	f.limits.work = scan.work
	if r := handle(t, f, body); r.Outcome != filter.Continue {
		t.Errorf("Handle at the work limit = %+v, want Continue", r)
	}
	f.limits.work = scan.work - 1
	if r := handle(t, f, body); !errors.Is(r.Err, errWorkTooLarge) {
		t.Errorf("Handle one unit past the work limit = %+v, want the work ErrTooLarge", r)
	}
	// The scan goes on past the work limit for the numbers: the last
	// record's quantity is out of range.
	f.limits.work = scan.work / 4
	if got := detailOf(t, handle(t, f, strings.Replace(body, `"qty":50,`, `"qty":1e1001,`, 1))); got != DetailNumber {
		t.Errorf("a number out of range past the work limit: %q, want %q", got, DetailNumber)
	}
	if got := buildFilter(t, `true`, testLimit).limits.work; got != WorkPerByte*testLimit {
		t.Errorf("maxWork = %d, want %d", got, WorkPerByte*testLimit)
	}
	if got := workLimit(0); got != WorkPerByte*10<<20 {
		t.Errorf("workLimit(0) = %d", got)
	}
	if got := workLimit(1 << 62); got > maxWorkLimit || got < maxWorkLimit-WorkPerByte {
		t.Errorf("workLimit(1<<62) = %d, want just under %d", got, int64(countCap))
	}
	// A count that saturates is over any limit (without states, the
	// depth bound applies).
	s := mustCompile(t, `true`)
	s.work.states = nil
	s.work.at[0][dimWork] = countCap
	r := handle(t, newFilter(s, 400, bodyLimits{work: workLimit(1 << 62)}), `{}`)
	if !errors.Is(r.Err, errWorkTooLarge) {
		t.Errorf("Handle with a saturated count = %+v, want the work ErrTooLarge", r)
	}
}

// TestHandleReservationLimit is the regression test of the review finding
// that nothing bounded the reservation as a whole: the decoded values were
// limited at the decoder's charge and the decision pass's working set on
// its own, but not what the maps, arrays and strings hold beyond the
// decoder's charge, so a valid 2.15 MB array of one-member objects reserved
// 17.6 times the 10 MiB default (185 MB), and under a 24 MiB limit a valid
// body needed more than a gate's share of the Node's budget, 503 on an idle
// Node. What Handle reserves is now at most ReservationFactor times the raw
// limit, or the body is filter.ErrTooLarge (413) before anything is
// reserved:
//
//   - a body whose reservation is at the limit validates, and one byte
//     more is 413;
//   - a failing body whose location pass would take the reservation past
//     the limit is refused with DetailMismatch alone;
//   - through Factory.Build, the largest valid array of one-member objects
//     under a properties schema reserves at most ReservationFactor times
//     the raw limit, and one more item is 413.
//
// Requirements: architecture R-77 and R-63, 07 reqs 15 and 82.
func TestHandleReservationLimit(t *testing.T) {
	body := records(50)
	f := buildFilter(t, recordsSchema, testLimit)
	if f.limits.reserve != ReservationFactor*testLimit {
		t.Fatalf("reservation limit %d, want %d", f.limits.reserve, ReservationFactor*testLimit)
	}
	reserve := func(f *Filter, body string) (filter.Result, int64) {
		t.Helper()
		x, _ := exchange("application/json", []byte(body))
		px := &peakExchange{Exchange: x}
		r := f.Handle(t.Context(), phase.OnRequestBody, px)
		if x.Budget.Reserved != 0 {
			t.Errorf("reservation left: %d", x.Budget.Reserved)
		}
		return r, px.peak
	}
	_, scan := scanBody(t, f.schema, body)
	_, cost, _ := f.decode([]byte(body))
	n := cost + scan.extra + scan.decideMemory()
	f.limits.reserve = n
	if r, peak := reserve(f, body); r.Outcome != filter.Continue || peak != n {
		t.Errorf("at the reservation limit: %+v, reserved %d, want Continue reserving %d", r, peak, n)
	}
	f.limits.reserve = n - 1
	if r, peak := reserve(f, body); !errors.Is(r.Err, errReservationTooLarge) || !errors.Is(r.Err, filter.ErrTooLarge) || peak != 0 {
		t.Errorf("one byte over the reservation limit: %+v, reserved %d, want the reservation ErrTooLarge reserving nothing", r, peak)
	}
	failing := strings.Replace(body, `"qty":1,`, `"qty":0,`, 1)
	_, fs := scanBody(t, f.schema, failing)
	_, fcost, _ := f.decode([]byte(failing))
	decoded := fcost + fs.extra
	if fs.locateMemory() <= fs.decideMemory() {
		t.Fatalf("the location pass holds %d, the decision pass %d: want more", fs.locateMemory(), fs.decideMemory())
	}
	f.limits.reserve = decoded + fs.locateMemory() - 1
	if r, _ := reserve(f, failing); detailOf(t, r) != DetailMismatch {
		t.Errorf("location pass over the reservation limit: %q, want DetailMismatch alone", detailOf(t, r))
	}
	f.limits.reserve = decoded + fs.locateMemory()
	if r, _ := reserve(f, failing); detailOf(t, r) == DetailMismatch {
		t.Errorf("location pass at the reservation limit: %q, want the location", detailOf(t, r))
	}

	// The review finding's shape, at a raw limit of 1 MiB: the largest
	// valid array that passes every limit before Handle reserves.
	const raw = 1 << 20
	f = buildFilter(t, `{"type":"array","items":{"type":"object","properties":{"a":{"type":"integer","minimum":0,"maximum":9}}}}`, raw)
	fits := func(items int) bool {
		v, cost, err := f.decode([]byte(denseArray(items, `{"a":1}`, 0)))
		if err != nil {
			return false
		}
		scan := bodyScan{ws: &f.schema.work, numbers: f.schema.numbers, workLimit: f.limits.work}
		scan.scan(v)
		return !scan.bad && !scan.deep && scan.decideMemory() <= f.limits.workingSet && !scan.workOver() &&
			addCap(addCap(cost, scan.extra), scan.decideMemory()) <= f.limits.reserve
	}
	lo, hi := 1, raw/8
	for hi-lo > 1 {
		if mid := (lo + hi) / 2; fits(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	largest := denseArray(lo, `{"a":1}`, 0)
	r, peak := reserve(f, largest)
	if r.Outcome != filter.Continue || peak > ReservationFactor*raw {
		t.Errorf("the largest valid array, %d items: %+v, reserved %d, want Continue within %d", lo, r, peak, ReservationFactor*raw)
	}
	t.Logf("largest valid array of one-member objects: %d items, %.2f of the raw limit, reserving %.2f times it", lo, float64(len(largest))/raw, float64(peak)/raw)
	if r, peak := reserve(f, denseArray(lo+1, `{"a":1}`, 0)); !errors.Is(r.Err, errReservationTooLarge) || peak != 0 {
		t.Errorf("one item more: %+v, reserved %d, want the reservation ErrTooLarge", r, peak)
	}
}
