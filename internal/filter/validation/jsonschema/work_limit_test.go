// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
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
	}
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, scan := scanBody(t, mustCompile(t, tc.schema), tc.body); scan.work != tc.want {
				t.Errorf("work = %d, want %d", scan.work, tc.want)
			}
		})
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
		{`[` + strings.TrimSuffix(strings.Repeat(`1.25,`, 29), ",") + `,true,null]`, 2 * ((4+1)/2 + 3)},
		{`[` + strings.TrimSuffix(strings.Repeat(`"a",`, 29), ",") + `,[]]`, 29},
	} {
		v, _ := decodeInstance(t, tc.body).([]any)
		if got := comparisons(v); got != tc.want {
			t.Errorf("comparisons(%.40s) = %d, want %d", tc.body, got, tc.want)
		}
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

// TestHandleAcceptsLargeBodies is the regression test of the review
// findings that valid ordinary bodies were refused far inside the raw
// limit (architecture R-63 and R-77: typical records fit up to about 70 to
// 84% of the limit; 07 reqs 13, 15 and 80): through Factory.Build and
// Handle at the 10 MiB default, an order-item array at 80% of the limit
// and a line-item array at 69% pass, the decoded values (about 4.9 and 5.7
// times the body) being what limits them, within the reservation limit; a
// long string under a pattern, a base64-class string and an array of enum
// values pass too. A valid body reserves the decoded values, with the
// bytes their maps, arrays and strings hold beyond the decoder's charge
// (TestHandleReservesDecodedMaps), and the decision pass's working set,
// never the location pass's, and at most two and a half times the heap
// Handle holds at its peak; work is never reserved.
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
	}{
		{"order items", recordsSchema, records(171000 / scale), 0.79},
		{"line items", lineSchema, denseArray(176000/scale, `{"sku":"ABC-12345","qty":2,"price":9.99}`, 0), 0.68},
		{"long string under a pattern", `{"type":"string","pattern":"^a"}`, `"` + strings.Repeat("a", 7<<20/scale) + `"`, 0.7},
		{"long string under a character class", `{"type":"string","pattern":"^[A-Za-z0-9+/]*={0,2}$"}`, `"` + strings.Repeat("QUJD", 2<<20/scale) + `"`, 0.8},
		{"array of enum values", `{"type":"array","items":{"enum":[` + strings.Join(enum, ",") + `]}}`, denseArray(100000/scale, `"v599"`, 0), 0.06},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := float64(len(tc.body)) / float64(limit); got < tc.fraction {
				t.Fatalf("body is %.2f of the limit, want at least %.2f", got, tc.fraction)
			}
			f := buildFilter(t, tc.schema, raw)
			x, _ := exchange("application/json", []byte(tc.body))
			px := &peakExchange{Exchange: x}
			var r filter.Result
			peak := peakHeap(func() any { r = f.Handle(t.Context(), phase.OnRequestBody, px); return r })
			if r.Outcome != filter.Continue {
				t.Fatalf("Handle = %+v, want Continue", r)
			}
			_, scan := scanBody(t, f.schema, tc.body)
			t.Logf("body %d bytes: reserved %d, peak %.0f, work %d of %d", len(tc.body), px.peak, peak, scan.work, f.limits.work)
			// The sampled peak follows the collector's pacing: under load it
			// may miss part of the live heap (minor review finding), hence
			// two and a half times.
			if float64(px.peak) > 2.5*peak+gcSlack {
				t.Errorf("reserved %d, more than two and a half times the peak %.0f", px.peak, peak)
			}
			if _, cost, _ := f.decode([]byte(tc.body)); px.peak != cost+scan.extra+scan.decideMemory() {
				t.Errorf("reserved %d, want the decoded values %d, their maps and arrays %d and the decision pass's working set %d", px.peak, cost, scan.extra, scan.decideMemory())
			}
			if x.Budget.Reserved != 0 {
				t.Errorf("reservation left: %d", x.Budget.Reserved)
			}
		})
	}
}

// TestHandleReservesDecodedMaps is the regression test of the review
// finding that the decoded values were reserved at the decoder's cost,
// which leaves out the hash table of every object (internal/jsonval
// charges a one-member object 80 bytes; it holds 336) and the spare
// capacity of every array: through Factory.Build and Handle at the 10 MiB
// default, valid arrays of one-, two- and nine-member objects and of
// five-item arrays, and one wide object, at about a quarter of the limit,
// held up to 3.8 times what Handle reserved (07 req 82, architecture R-63,
// filter.Exchange.Reserve). The reservation now covers the decoded tree,
// measured exactly after a collection; for the arrays the heap at
// Handle's peak stays within it but for the collector's pacing and its
// spans' free slots (peakHeap collects at 10% growth; measured up to 1.22
// times, and the sampler under load up to 1.41, so the check allows 1.6).
// A wide object's map leaves the tables it outgrew to the collector while
// the decoder builds it, before anything is reserved, so only its decoded
// tree is compared, as for an array of 33-byte strings, whose backing
// array, large next to its strings, the decoder outgrows several times;
// strings are charged their allocation size class.
func TestHandleReservesDecodedMaps(t *testing.T) {
	limit, scale, raw := int64(10<<20), 1, int64(0)
	if raceEnabled {
		limit, scale, raw = 1<<19, 20, 1<<19
	}
	nine := `{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7,"h":8,"i":9}`
	for _, tc := range []struct {
		name, schema, body string
		fraction           float64
		peak               bool
	}{
		// Arrays of one-member objects hold 47 times their bytes decoded:
		// the reservation limit binds at about a quarter of the raw limit
		// (TestHandleReservationLimit).
		{"one member", `{"type":"array","items":{"type":"object"}}`, denseArray(320000/scale, `{"a":1}`, 0), 0.24, true},
		{"one typed member", `{"type":"array","items":{"type":"object","properties":{"a":{"type":"integer"}}}}`, denseArray(320000/scale, `{"a":1}`, 0), 0.24, true},
		{"two members", `{"type":"array","items":{"type":"object","required":["id","ok"],"properties":{"id":{"type":"integer"},"ok":{"type":"boolean"}}}}`, denseArray(150000/scale, `{"id":7,"ok":true}`, 0), 0.25, true},
		{"nine members", `{"type":"array","items":{"type":"object"}}`, denseArray(50000/scale, nine, 0), 0.25, true},
		{"five items", `{"type":"array","items":{"type":"array"}}`, denseArray(150000/scale, `[1,2,3,4,5]`, 0), 0.15, true},
		// Strings of 33 bytes take the 48-byte size class.
		{"strings", `{"type":"array","items":{"type":"string"}}`, denseArray(80000/scale, `"`+strings.Repeat("s", 33)+`"`, 0), 0.25, false},
		// Minor review finding: member names of 1,025 bytes take the
		// 1,152-byte size class.
		{"long member names", `{"type":"array","items":{"type":"object"}}`, denseArray(2000/scale, `{"`+strings.Repeat("n", 1025)+`":1}`, 0), 0.19, false},
		{"one wide object", `{"type":"object"}`, wideObject(250000 / scale), 0.2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := float64(len(tc.body)) / float64(limit); got < tc.fraction {
				t.Fatalf("body is %.2f of the limit, want at least %.2f", got, tc.fraction)
			}
			f := buildFilter(t, tc.schema, raw)
			body := []byte(tc.body)
			x, _ := exchange("application/json", body)
			px := &peakExchange{Exchange: x}
			var r filter.Result
			peak := peakHeap(func() any { r = f.Handle(t.Context(), phase.OnRequestBody, px); return r })
			if r.Outcome != filter.Continue {
				t.Fatalf("Handle = %+v, want Continue", r)
			}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			v, cost, err := f.decode(body)
			runtime.GC()
			runtime.ReadMemStats(&after)
			runtime.KeepAlive(v)
			if err != nil {
				t.Fatal(err)
			}
			live := growth(after.HeapAlloc, before.HeapAlloc)
			_, scan := scanBody(t, f.schema, tc.body)
			if raceEnabled {
				// The race detector turns off the allocator's packing of
				// small strings: each takes a size class of its own.
				live -= 16 * shortStrings(v)
			}
			t.Logf("decoded tree %d, charged %d and %d for its maps, arrays and strings; reserved %d, peak %.0f (%.2f)", live, cost, scan.extra, px.peak, peak, peak/float64(px.peak))
			if live > cost+scan.extra {
				t.Errorf("the decoded tree holds %d bytes, Handle reserves %d for it", live, cost+scan.extra)
			}
			if tc.peak && peak > 1.6*float64(px.peak)+gcSlack {
				t.Errorf("Handle held %.0f bytes at its peak, reserved %d", peak, px.peak)
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
	// A string: its size class from 16 bytes, whole pages past 32 KiB.
	for _, tc := range []struct{ n, want int64 }{{0, 0}, {15, 0}, {16, 0}, {17, 7}, {33, 15}, {32 << 10, 0}, {32<<10 + 1, 8<<10 - 1}} {
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

// shortStrings counts the strings, member names and number literals of v
// shorter than 16 bytes.
func shortStrings(v any) int64 {
	switch t := v.(type) {
	case string:
		return int64(min(len(t)/16, 1) ^ 1)
	case json.Number:
		return int64(min(len(t)/16, 1) ^ 1)
	case []any:
		n := int64(0)
		for _, e := range t {
			n += shortStrings(e)
		}
		return n
	case map[string]any:
		n := int64(0)
		for k, e := range t {
			n += int64(min(len(k)/16, 1)^1) + shortStrings(e)
		}
		return n
	default:
		return 0
	}
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
