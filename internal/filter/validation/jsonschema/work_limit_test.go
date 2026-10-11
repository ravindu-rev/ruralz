// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// emailPattern is the WHATWG e-mail address pattern, a large pattern in
// common use (259 program instructions).
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
		{"enum of 600 strings", `{"items":{"enum":[` + strings.Join(enum, ",") + `]}}`, denseArray(2000, `"v599"`, 0)},
		{"enum of 600 strings, failing", `{"items":{"enum":[` + strings.Join(enum, ",") + `]}}`, denseArray(2000, `"x"`, 0)},
		{"short numbers", `{"items":{"minimum":0,"maximum":1e9,"type":"integer"}}`, denseArray(20000, `12345`, 0)},
		{"const array", `{"allOf":[` + strings.TrimSuffix(strings.Repeat(`{"const":[`+strings.Join(nums, ",")+`]},`, 20), ",") + `]}`, `[` + strings.Join(nums, ",") + `]`},
		{"enum of numbers, long literals", `{"items":{"enum":[` + strings.Join(nums[:300], ",") + `]}}`, denseArray(50, longNumber, 0)},
		{"uniqueItems pairwise", `{"items":{"uniqueItems":true}}`, denseArray(50, `[`+strings.Join(distinct, ",")+`]`, 0)},
		{"uniqueItems hashed objects", `{"uniqueItems":true}`, objects.String()},
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
	}
}

// TestWorkCoversTime checks the work model (proposed, package
// documentation "Work bound", 07 section 9 item 21) against the library:
// for every way the library's work grows, validating the body takes no
// more nanoseconds than the work units the scan charges. A timing test:
// skipped under -race, and the best of several runs, retried, so a busy
// machine does not fail it.
func TestWorkCoversTime(t *testing.T) {
	if raceEnabled {
		t.Skip("-race slows the library, the model is calibrated without it")
	}
	for _, tc := range workCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := mustCompile(t, tc.schema)
			v, scan := scanBody(t, s, tc.body)
			best := time.Duration(1<<63 - 1)
			for range 3 {
				for range 3 {
					start := time.Now()
					_ = s.root.Validate(v)
					best = min(best, time.Since(start))
				}
				if best.Nanoseconds() <= scan.work {
					break
				}
			}
			t.Logf("estimate %d units, measured %v (%.2f)", scan.work, best, float64(best.Nanoseconds())/float64(scan.work))
			if best.Nanoseconds() > scan.work {
				t.Errorf("validation took %v, the work model charges %d units", best, scan.work)
			}
		})
	}
}

// TestWorkCharges pins the work model (proposed) on small bodies: each
// evaluation's weight, the cycle check, the location copies, and the terms
// that grow with the value (string bytes, number literals, members, the
// unevaluated copies, the pairwise uniqueItems comparisons).
func TestWorkCharges(t *testing.T) {
	const kw = evalUnits + keywordUnits
	for _, tc := range []struct {
		name, schema, body string
		want               int64
	}{
		{"string", `{"type":"string"}`, `"abc"`, kw + scopeUnits + 3*copyUnits},
		{"members", `{"minProperties":0}`, `{"a":1,"b":2}`, kw + scopeUnits + 2*memberUnits},
		{"number", `{"minimum":0}`, `1.5`, kw + scopeUnits + numberUnits + 3*numberByteUnits},
		{"pattern", `{"pattern":"^a"}`, `"aaaa"`, kw + matchUnits + 4*matchInstUnits + scopeUnits + 4*(copyUnits+byteUnits+2*queueUnits)},
		{"enum of strings", `{"enum":["ab","cd"]}`, `"x"`, kw + 2*compareUnits + scopeUnits + copyUnits},
		{"enum of numbers", `{"enum":[1,2]}`, `3`, kw + 2*(compareUnits+numberUnits+numberByteUnits) + scopeUnits + 2*(numberUnits+numberByteUnits)},
		{
			"unevaluated names", `{"unevaluatedProperties":true}`, `{"a":1}`,
			evalUnits + scopeUnits + memberUnits + unevalPropUnits + evalUnits + scopeUnits + 2*locationUnits,
		},
		{
			"pairwise uniqueItems", `{"uniqueItems":true}`, `["a","b"]`,
			kw + scopeUnits + 2*(hashUnits+locationUnits+scopeUnits+(copyUnits+hashByteUnits)+(pairwiseItems-1)*compareUnits),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, scan := scanBody(t, mustCompile(t, tc.schema), tc.body); scan.work != tc.want {
				t.Errorf("work = %d, want %d", scan.work, tc.want)
			}
		})
	}
}

// TestHandleWorkBound is the regression test of the review finding that
// validation work grows with the value as well as with the evaluations:
// through Factory.Build and Handle at the 10 MiB default, bodies that
// would take seconds of uncancellable work (many evaluations at a wide
// object or a long string, comparisons with a large const) are
// filter.ErrTooLarge before the library runs, without reserving anything.
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

// TestHandleAcceptsLargeBodies covers the other review finding: work is
// never reserved from limits.maxBufferedBytes, so at the 10 MiB default a
// valid 7 MiB string under a pattern and a valid 0.7 MiB array of enum
// values (100,000 items, near what the error tree of a failing one allows)
// pass, reserving only the decoded values and the memory validation may
// hold.
func TestHandleAcceptsLargeBodies(t *testing.T) {
	enum := make([]string, 600)
	for i := range enum {
		enum[i] = fmt.Sprintf(`"v%d"`, i)
	}
	for _, tc := range []struct{ name, schema, body string }{
		{"long string under a pattern", `{"type":"string","pattern":"^a"}`, `"` + strings.Repeat("a", 7<<20) + `"`},
		{"long string under a character class", `{"type":"string","pattern":"^[A-Za-z0-9+/]*={0,2}$"}`, `"` + strings.Repeat("QUJD", 2<<20) + `"`},
		{"array of enum values", `{"type":"array","items":{"enum":[` + strings.Join(enum, ",") + `]}}`, denseArray(100000, `"v599"`, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFilter(t, tc.schema, 0)
			x, _ := exchange("application/json", []byte(tc.body))
			px := &peakExchange{Exchange: x}
			if r := f.Handle(t.Context(), phase.OnRequestBody, px); r.Outcome != filter.Continue {
				t.Fatalf("Handle = %+v, want Continue", r)
			}
			_, scan := scanBody(t, f.schema, tc.body)
			t.Logf("body %d bytes: reserved %d, work %d of %d", len(tc.body), px.peak, scan.work, f.maxWork)
			if x.Budget.Reserved != 0 {
				t.Errorf("reservation left: %d", x.Budget.Reserved)
			}
		})
	}
}

// TestWorkLimit covers the work limit (proposed): WorkPerByte work units
// per byte of maxRequestBodyBytes, the 10 MiB default when unset, and
// below the saturation of the counts however large the limit.
func TestWorkLimit(t *testing.T) {
	if got := buildFilter(t, `true`, testLimit).maxWork; got != WorkPerByte*testLimit {
		t.Errorf("maxWork = %d, want %d", got, WorkPerByte*testLimit)
	}
	if got := workLimit(0); got != WorkPerByte*10<<20 {
		t.Errorf("workLimit(0) = %d", got)
	}
	if got := workLimit(1 << 62); got > maxWorkLimit || got < maxWorkLimit-WorkPerByte {
		t.Errorf("workLimit(1<<62) = %d, want just under %d", got, int64(countCap))
	}
	// A count that saturates is over any limit.
	s := mustCompile(t, `true`)
	s.work.at[0][dimWork] = countCap
	r := handle(t, newFilter(s, 400, 0, workLimit(1<<62)), `{}`)
	if !errors.Is(r.Err, errWorkTooLarge) {
		t.Errorf("Handle with a saturated count = %+v, want the work ErrTooLarge", r)
	}
}
