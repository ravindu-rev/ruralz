// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"os"
	"os/exec"
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

// oneOfConsts returns a oneOf of k const branches, the usual enum shape
// that fails at every branch.
func oneOfConsts(k int) string {
	branches := make([]string, k)
	for i := range branches {
		branches[i] = fmt.Sprintf(`{"const":"c%d"}`, i)
	}
	return `{"oneOf":[` + strings.Join(branches, ",") + `]}`
}

// denseArray returns an array of n copies of item nested depth arrays
// deep.
func denseArray(n int, item string, depth int) string {
	var b strings.Builder
	b.WriteString(strings.Repeat("[", depth+1))
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(item)
	}
	b.WriteString(strings.Repeat("]", depth+1))
	return b.String()
}

// itemsDeep returns a schema applying item to the values depth arrays
// below the root array.
func itemsDeep(depth int, item string) string {
	return strings.Repeat(`{"items":`, depth+1) + item + strings.Repeat(`}`, depth+1)
}

// wideObject returns an object of n members named k0 to k<n-1>, each 1.
func wideObject(n int) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%d":1`, i)
	}
	b.WriteByte('}')
	return b.String()
}

// allOfRefs returns an allOf of n references to the head of a chain of
// three in-place references ending in last: 5n+1 evaluations at a value.
func allOfRefs(n int, last string) string {
	const links = 3
	var b strings.Builder
	b.WriteString(`{"allOf":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"$ref":"#/$defs/c0"}`)
	}
	b.WriteString(`],"$defs":{`)
	for i := range links {
		fmt.Fprintf(&b, `"c%d":{"$ref":"#/$defs/c%d"},`, i, i+1)
	}
	fmt.Fprintf(&b, `"c%d":%s}}`, links, last)
	return b.String()
}

// unevaluatedChain returns a schema with unevaluatedProperties (or
// unevaluatedItems) whose root applies a chain of links in-place
// references, the shape that makes the library copy the member names (or
// item indexes) at every link.
func unevaluatedChain(links int, keyword string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"$ref":"#/$defs/d0","%s":true,"$defs":{`, keyword)
	for i := range links {
		fmt.Fprintf(&b, `"d%d":{"$ref":"#/$defs/d%d"},`, i, i+1)
	}
	fmt.Fprintf(&b, `"d%d":{"minProperties":0}}}`, links)
	return b.String()
}

// unionSchema returns an order with an id, a plain array of tags and a
// payment that is a oneOf of k object variants: a costly union next to a
// cheap sibling array.
func unionSchema(k int) string {
	variants := make([]string, k)
	for i := range variants {
		variants[i] = fmt.Sprintf(`{"type":"object","required":["type","amount","currency"],"additionalProperties":false,`+
			`"properties":{"type":{"const":"m%d"},"amount":{"type":"number","minimum":0,"maximum":1000000,"multipleOf":0.01},`+
			`"currency":{"type":"string","pattern":"^[A-Z]{3}$","minLength":3,"maxLength":3},`+
			`"reference":{"type":"string","maxLength":64},"note":{"type":"string","maxLength":256}}}`, i)
	}
	return `{"type":"object","required":["id"],"properties":{"id":{"type":"string"},` +
		`"tags":{"type":"array","items":{"type":"string","maxLength":32}},"payment":{"oneOf":[` + strings.Join(variants, ",") + `]}}}`
}

// unionBody returns a valid order of unionSchema with n tags of form tag.
func unionBody(n int, tag string) string {
	tags := make([]string, n)
	for i := range tags {
		tags[i] = fmt.Sprintf(tag, i)
	}
	return `{"id":"ord_1","payment":{"type":"m0","amount":10.5,"currency":"EUR"},"tags":[` + strings.Join(tags, ",") + `]}`
}

// distinct returns an array of n items made by formatting i, from 0 to
// n-1, with format.
func distinct(n int, format string) string {
	return "[" + joined(n, func(i int) string { return fmt.Sprintf(format, i) }) + "]"
}

// workingSetCases are dense bodies, each against a schema that fails at
// every value in a different way, or (the unevaluated shapes) holds the
// library's copies of member names or item indexes along a reference
// chain, or (the ordinary shapes) a valid or failing order body, or (the
// copies) makes a copy that it drops before any probe runs again.
func workingSetCases() []struct{ name, schema, body string } {
	var required, members []string
	for i := range 50 {
		required = append(required, fmt.Sprintf(`"r%02d"`, i))
		members = append(members, fmt.Sprintf(`"m%02d":%d`, i, i))
	}
	object := `{"type":"object","required":["a","b","c"],"minProperties":9,"maxProperties":1,` +
		`"dependentRequired":{"q":["r","s"],"y":["t"]},"additionalProperties":false,"properties":{"z":{"type":"integer"}}}`
	return []struct{ name, schema, body string }{
		{"type", `{"items":{"type":"integer"}}`, denseArray(20000, `"x"`, 0)},
		{"type list", `{"items":{"type":["integer","boolean","null","object","array","number"]}}`, denseArray(20000, `"x"`, 0)},
		{"oneOf of 8 consts", `{"items":` + oneOfConsts(8) + `}`, denseArray(20000, `"x"`, 0)},
		{"anyOf", `{"items":{"anyOf":[{"type":"integer"},{"type":"null"}]}}`, denseArray(20000, `"x"`, 0)},
		{"allOf of assertions", `{"items":{"allOf":[{"minLength":5},{"pattern":"^a"},{"enum":["a","b"]},{"maxLength":0}]}}`, denseArray(20000, `"x"`, 0)},
		{"string assertions", `{"items":{"minLength":3,"pattern":"^a","format":"email"}}`, denseArray(20000, `"x"`, 0)},
		{"reference chain", `{"$defs":{"a":{"$ref":"#/$defs/b"},"b":{"type":"integer"}},"items":{"$ref":"#/$defs/a"}}`, denseArray(20000, `"x"`, 0)},
		{"object keywords", `{"items":` + object + `}`, denseArray(10000, `{"q":1,"y":2,"w":3,"z":"s"}`, 0)},
		{"required list", `{"items":{"required":[` + strings.Join(required, ",") + `]}}`, denseArray(20000, `{}`, 0)},
		{"additional members", `{"items":{"additionalProperties":false}}`, denseArray(1000, `{`+strings.Join(members, ",")+`}`, 0)},
		{"unevaluated members", `{"items":{"properties":{"m00":true},"unevaluatedProperties":false}}`, denseArray(1000, `{`+strings.Join(members, ",")+`}`, 0)},
		{"property names", `{"items":{"propertyNames":{"maxLength":1}}}`, denseArray(1000, `{`+strings.Join(members, ",")+`}`, 0)},
		{"numbers", `{"items":{"minimum":1,"maximum":0,"exclusiveMinimum":2,"exclusiveMaximum":0,"multipleOf":7}}`, denseArray(5000, `1e-1000`, 0)},
		{"small numbers", `{"items":{"minimum":1,"multipleOf":7}}`, denseArray(20000, `0.5`, 0)},
		{"contains", `{"items":{"contains":{"const":1},"minItems":9}}`, denseArray(2000, `["x","x","x","x","x","x","x","x"]`, 0)},
		{"if then else", `{"items":{"if":{"type":"string"},"then":{"minLength":2,"pattern":"^a"},"else":{"type":"null"}}}`, denseArray(20000, `"x"`, 0)},
		{"deep type", itemsDeep(60, `{"type":"integer"}`), denseArray(5000, `"x"`, 60)},
		{"deep oneOf", itemsDeep(30, oneOfConsts(4)), denseArray(5000, `"x"`, 30)},
		{"nested minItems", `{"minItems":2,"items":{"minItems":2,"items":{"minItems":2,"items":{"type":"integer"}}}}`, denseArray(5000, `[["x"]]`, 0)},
		{"unevaluated names along a chain", unevaluatedChain(100, "unevaluatedProperties"), wideObject(2000)},
		{"unevaluated indexes along a chain", strings.Replace(unevaluatedChain(100, "unevaluatedItems"), "minProperties", "minItems", 1), denseArray(40000, `1`, 0)},
		// The copies of one chain at a time.
		{"unevaluated names under branches", unevaluatedBranches(50, "unevaluatedProperties"), wideObject(5000)},
		{"unevaluated indexes under branches", unevaluatedBranches(50, "unevaluatedItems"), denseArray(8000, `1`, 0)},
		{"records, valid", recordsSchema, records(20000)},
		{"records, members missing", recordsSchema, denseArray(20000, `{"qty":1}`, 0)},
		{"records, members failing", recordsSchema, denseArray(20000, `{"sku":"","qty":0.5,"price":-1}`, 0)},
		{"union next to tags, failing", unionSchema(12), unionBody(20000, `"%040d"`)},
		// The bounds that cannot fail together, a decimal under an integer
		// type, the lists of matching items contains keeps along a
		// reference chain, and the errors propertyNames wraps in every
		// evaluation that applies it.
		{"bounds that cannot both fail", `{"items":{"minimum":1,"maximum":9,"exclusiveMaximum":9}}`, denseArray(20000, `10`, 0)},
		{"integer type on decimals", `{"items":{"type":"integer"}}`, denseArray(20000, `1.5`, 0)},
		{"matching items along a chain", chain(100, `{"$ref":"#/$defs/d%d","contains":true,"maxContains":0,"x-n":%d}`), denseArray(16000, `1`, 0)},
		{"too few matching items", `{"allOf":[` + strings.TrimSuffix(strings.Repeat(`{"contains":{"type":"number"},"minContains":100000},`, 20), ",") + `]}`, denseArray(20000, `1`, 0)},
		{"property names in many evaluations", `{"anyOf":[` + strings.TrimSuffix(strings.Repeat(`{"propertyNames":{"type":"integer"}},`, 50), ",") + `]}`, wideObject(2000)},
		{"property names along a chain", chain(100, `{"$ref":"#/$defs/d%d","propertyNames":{"enum":["a","b"]},"x-n":%d}`), wideObject(2000)},
		// The member names of the depth bound, which clamps the states,
		// when the precise pass decides it.
		{"property names under mixins", strings.TrimSuffix(mixins, "}") + `,"propertyNames":{"allOf":[` + strings.TrimSuffix(strings.Repeat(`{"pattern":"^a","maxLength":2},`, 20), ",") + `]}}`, wideObject(2000)},
		// The copies the library makes and drops between two probes, which
		// the meter replays. The uniqueItems check of distinct integers,
		// strings and objects, of numbers that hash alike (a number and its
		// negation), of an array whose last item repeats the first, and of a
		// wide object among the items, whose names it sorts.
		{"unique integers", `{"uniqueItems":true}`, distinct(20000, "%d")},
		{"unique strings", `{"uniqueItems":true}`, distinct(20000, `"s%05d"`)},
		{"unique objects", `{"uniqueItems":true}`, distinct(10000, `{"id":%d,"name":"item"}`)},
		{"numbers that hash alike", `{"uniqueItems":true}`, "[" + joined(20000, func(i int) string { return fmt.Sprint((i/2 + 1) * (1 - 2*(i%2))) }) + "]"},
		{"a duplicate at the end", `{"uniqueItems":true}`, strings.TrimSuffix(distinct(20000, "%d"), "]") + ",0]"},
		{"names of hashed objects", `{"uniqueItems":true}`, "[" + wideObject(50000) + "," + strings.TrimPrefix(distinct(20, "%d"), "[")},
		// A string's copy at every evaluation; the names additionalProperties
		// false lists, the duplicates map, and the errors of contains over
		// false, each in an evaluation that drops them, the first two in a
		// failing branch of a valid body, where no exit probe runs.
		{"long strings", `{"items":{"type":"string"}}`, denseArray(8, `"`+strings.Repeat("s", 100000)+`"`, 0)},
		{"undeclared members in a failing branch", `{"anyOf":[{"additionalProperties":false},true]}`, wideObject(20000)},
		{"duplicates in a failing branch", `{"anyOf":[{"uniqueItems":true,"maxItems":1},true]}`, distinct(20000, "%d")},
		{"errors contains drops", `{"contains":false,"minContains":0}`, denseArray(20000, `1`, 0)},
	}
}

// scanBody returns the scan of body under s with no limits.
func scanBody(t testing.TB, s *Schema, body string) (any, *bodyScan) {
	t.Helper()
	v := decodeInstance(t, body)
	scan := &bodyScan{ws: &s.work, numbers: s.numbers, workLimit: maxWorkLimit}
	scan.scan(v)
	if scan.bad {
		t.Fatalf("body has a number out of range")
	}
	return v, scan
}

// TestWorkingSetCoversPeak covers the working-set bound (proposed) of
// both passes: for dense bodies of every error shape, the
// unevaluated-tracking copies the library makes along a reference
// chain, ordinary order bodies, valid or failing, and the copies the
// library drops before any probe runs again.
//
//   - The error tree the decision pass holds, measured exactly at its end,
//     stays within what the scan charges it (a valid body is charged the
//     decision pass's tree, never the location pass's).
//   - The error tree the location pass returns, measured exactly, stays
//     within what the scan charges it.
//   - The live heap at the peak of each pass stays within the pass's
//     whole working set, its tree, path and transient terms: the copies
//     of the unevaluated names or indexes along one path, the transient
//     copies and the tree the pass holds by then. It is measured
//     deterministically (heapMeter): at the start and the end of the
//     first evaluations of each pass, at those spread over the rest of
//     it, at its end and at its matches, with the largest copy the
//     pass made between two of them replayed on top. A sampled heap in
//     use, garbage included, reaches 1.64 of the charge under load on
//     the Go 1.26 floor (the unevaluated indexes under branches),
//     while the live heap stays within 0.38 of it.
//   - The live heap at the peak of each pass is at least the tree the
//     pass ends with, but for treeApart, so that the measurement reaches
//     it (a schema's extension keywords run after its applicators
//     failed, in either pass, so a failing body's decision pass
//     reaches a probe at every value with its tree growing, to 0.86 of
//     the charge); and each pass that ends at its end probe, the
//     decision pass of a failing body and every location pass, ends
//     with the tree measured apart live, so that those are the passes'
//     own.
//   - The copies are measured (the uniqueItems duplicates map, the
//     names additionalProperties false lists and the errors of a
//     contains loop over false, dropped before any probe runs again,
//     and every copy in a failing branch of a valid body, where no
//     exit probe runs): the rows that make one replay it, and their
//     peak holds it.
func TestWorkingSetCoversPeak(t *testing.T) {
	requireMeter(t)
	copies := map[string]struct {
		kind  copyKind
		least int64
	}{
		// A map entry with its index: at least the 8-byte key and the
		// 24-byte slice header, besides the 8-byte index.
		"unique integers":         {copyDuplicates, 20000 * 40},
		"unique strings":          {copyDuplicates, 20000 * 40},
		"unique objects":          {copyDuplicates, 10000 * 40},
		"numbers that hash alike": {copyDuplicates, 10000 * 40},
		"a duplicate at the end":  {copyDuplicates, 20000 * 40},
		// The sorted names of the wide object, 16 bytes each.
		"names of hashed objects":                {copyDuplicates, 50000 * 16},
		"long strings":                           {copyString, 100000},
		"undeclared members in a failing branch": {copyListed, 20000 * 16},
		"duplicates in a failing branch":         {copyDuplicates, 20000 * 40},
		"errors contains drops":                  {copyDropped, 20000 * 80},
		"numbers":                                {copyNumber, numberCheckBytes},
	}
	// The rows whose decision pass holds a list in the middle of the pass,
	// with the least live heap growth a mark measures there: the matching
	// items of the chain's contains, which is true, four fifths of one list
	// of 16,000 indexes.
	holds := map[string]int64{"matching items along a chain": 16000 * 8 * 4 / 5}
	for _, tc := range workingSetCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := mustCompile(t, tc.schema)
			v, scan := scanBody(t, s, tc.body)
			decide, locate := decisionTree(t, tc.schema, v), locationTree(s, v)
			t.Logf("decision tree %d of %d (%.2f), location tree %d of %d (%.2f), per body byte %.1f and %.1f",
				decide, scan.decide, float64(decide)/float64(max(scan.decide, 1)), locate, scan.locate,
				float64(locate)/float64(max(scan.locate, 1)), float64(scan.decide)/float64(len(tc.body)), float64(scan.locate)/float64(len(tc.body)))
			if decide > scan.decide+treeSlack {
				t.Errorf("the decision pass held an error tree of %d bytes, the scan charges %d", decide, scan.decide)
			}
			if locate > scan.locate+treeSlack {
				t.Errorf("the location pass held an error tree of %d bytes, the scan charges %d", locate, scan.locate)
			}
			// A failing body's decision pass ends at decideProbe's end probe,
			// a valid one's at the document's root, and every location pass
			// at locateProbe's end probe, each holding its whole tree.
			ends, walk := 0, int64(0)
			var verr *sjs.ValidationError
			if errors.As(s.root.Validate(v), &verr) {
				ends, walk = 1, walkCopy(verr)
			}
			m := &heapMeter{}
			locatePass, decidePass := probedSchemas(t, tc.schema, m)
			for _, pass := range []struct {
				name         string
				s            *sjs.Schema
				charge, tree int64
				ends         int
			}{
				{"decision", decidePass, scan.decideMemory(), decide, ends},
				{"location", locatePass, scan.locateMemory(), locate, 1},
			} {
				m.start()
				err := pass.s.Validate(v)
				if pass.name == "location" {
					m.copy(copyWalk, walk)
				}
				peak := m.peak()
				runtime.KeepAlive(err)
				m.check(t)
				t.Logf("%s pass: charged %d, live peak %d (%.2f), %d marks, copies %v", pass.name, pass.charge, peak, float64(peak)/float64(pass.charge), m.marks, m.copies)
				if m.marks == 0 || m.ends != pass.ends {
					t.Errorf("the %s pass marked %d times and ended %d times at an end probe, want marks and %d", pass.name, m.marks, m.ends, pass.ends)
				}
				if peak < pass.tree-pass.tree/treeApart-heapNoise {
					t.Errorf("the %s pass held %d bytes at its peak, less than its error tree of %d: the marks missed it", pass.name, peak, pass.tree)
				}
				// The trees measured apart are the pass's own: its end mark
				// holds the tree, but for treeApart.
				end, apart := m.at[endMark], pass.tree/treeApart+heapNoise
				if pass.ends > 0 && (len(end) == 0 || end[len(end)-1] > pass.tree+apart || end[len(end)-1] < pass.tree-apart) {
					t.Errorf("the %s pass ended with %v bytes live, its tree measured apart %d", pass.name, end, pass.tree)
				}
				// The copy is replayed, and the peak holds it.
				if c, ok := copies[tc.name]; ok && (m.copies[c.kind] < c.least || peak < c.least) {
					t.Errorf("the %s pass replayed copies %v and peaked at %d, want at least %d of kind %d in both", pass.name, m.copies, peak, c.least, c.kind)
				}
				if least, ok := holds[tc.name]; ok && pass.name == "decision" && growth(m.live, m.base) < least {
					t.Errorf("the decision pass measured at most %d bytes live, want at least %d of a list", growth(m.live, m.base), least)
				}
				if peak > pass.charge+treeSlack {
					t.Errorf("the %s pass held %d bytes at its peak, its working set is charged %d", pass.name, peak, pass.charge)
				}
			}
		})
	}
}

// treeApart sets how far apart two validations' error trees may be, a
// 1/treeApart part of the tree: the tiny allocator packs a pass's small
// allocations, kept or dropped, in the order the library iterates over an
// object's members, which Go varies, so the failing records' trees of five
// runs differed by up to 0.5%, and the other trees of workingSetCases by
// at most 8.5 KB, within heapNoise.
const treeApart = 64

// treeBesideCopies returns an anyOf under unevaluatedItems whose first
// branch fails at every item of an array under an 8-way oneOf and whose
// second follows a chain of links in-place references before failing at
// maxItems: at the chain's deepest link, the decision pass holds the first
// branch's error tree and the copies of the item indexes of the root, the
// second branch and every link.
func treeBesideCopies(links int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"unevaluatedItems":true,"anyOf":[{"items":%s},{"$ref":"#/$defs/d0","maxItems":0}],"$defs":{`, oneOfConsts(8))
	for i := range links {
		fmt.Fprintf(&b, `"d%d":{"$ref":"#/$defs/d%d"},`, i, i+1)
	}
	fmt.Fprintf(&b, `"d%d":{"minItems":0}}}`, links)
	return b.String()
}

// TestDecisionTreeBesideCopies covers the sum in the decision pass's
// working set: its error tree and one path's copies are live at once, so
// it is charged both. An array of 5,000 items under treeBesideCopies(6)
// holds, at the chain's deepest link, the first branch's tree with the
// nine copies of its 5,000 indexes on top, measured there at its one call:
// more than the tree's charge or the path's, and within their sum.
func TestDecisionTreeBesideCopies(t *testing.T) {
	requireMeter(t)
	const links, items = 6, 5000
	schema := treeBesideCopies(links)
	s := mustCompile(t, schema)
	v, scan := scanBody(t, s, denseArray(items, `"x"`, 0))
	deepest := fmt.Sprintf("/$defs/d%d", links)
	m := &heapMeter{only: deepest}
	_, decide := probedSchemas(t, schema, m)
	m.start()
	err := decide.Validate(v)
	peak := m.peak()
	runtime.KeepAlive(err)
	tree, at := decisionTree(t, schema, v), m.at[deepest]
	larger := addCap(max(scan.decide, scan.path), scan.transient)
	t.Logf("tree %d charged %d, path charged %d, transient %d; at the deepest link %v, live peak %d (%.2f of the working set %d)",
		tree, scan.decide, scan.path, scan.transient, at, peak, float64(peak)/float64(scan.decideMemory()), scan.decideMemory())
	// The copies of the root, the second branch and the seven links, at
	// least 8 bytes an index.
	if copies := int64((links + 3) * items * 8); len(at) != 1 || at[0] < tree+copies-heapNoise {
		t.Fatalf("the deepest link measured %v, want once with the tree of %d bytes and %d bytes of copies live", at, tree, copies)
	}
	if peak <= larger+treeSlack {
		t.Errorf("held %d bytes, within the larger of the tree's charge %d and the path's %d: the case no longer holds both", peak, scan.decide, scan.path)
	}
	if peak > scan.decideMemory()+treeSlack {
		t.Errorf("the decision pass held %d bytes at its peak, its working set is charged %d", peak, scan.decideMemory())
	}
}

// stackCaseEnv names the case TestStackWithinCharge runs in a child
// process.
const stackCaseEnv = "RURALZ_JSONSCHEMA_STACK_CASE"

// TestStackWithinCharge checks the stack term of the working set against
// the runtime: the library's call stack at a value holds one chain of
// in-place evaluations, whatever the value's evaluations (charging every
// evaluation a frame would reserve 29 MB of stack for a 14 KB rich-text
// body). For recursive bodies, unions and reference chains, validating in
// both passes on a goroutine whose stack may grow to two thirds of the
// path the scan charges never overflows: the stack, and the old one with
// the new while it doubles, stay within the charge. Each case runs in a
// child process, since a goroutine past its maximum stack aborts the
// program.
func TestStackWithinCharge(t *testing.T) {
	cases := []struct{ schema, body string }{
		{linearTree, nested(60)},
		{`{"items":{"$ref":"#"}}`, nested(60)},
		{`{"properties":{"a":{"$ref":"#"}}}`, strings.Repeat(`{"a":`, 60) + "1" + strings.Repeat("}", 60)},
		{chain(1000, `{"$ref":"#/$defs/d%d","x":%d}`), `{"a":1}`},
		{chain(200, `{"allOf":[{"$ref":"#/$defs/d%d"}],"x":%d}`), `{"a":1}`},
		{mixins, friends(30)},
		{componentTree, strings.Repeat(`{"kind":"row","children":[`, 9) + `{"text":"x"}` + strings.Repeat(`]}`, 9)},
		{allOfRefs(100, `{"minProperties":0}`), wideObject(50)},
	}
	if i, err := strconv.Atoi(os.Getenv(stackCaseEnv)); err == nil {
		s := mustCompile(t, cases[i].schema)
		v, scan := scanBody(t, s, cases[i].body)
		done := make(chan struct{})
		go func() {
			defer close(done)
			debug.SetMaxStack(int(scan.path * 2 / 3))
			_ = s.root.Validate(v)
			_ = s.decide.Validate(v)
		}()
		<-done
		return
	}
	if raceEnabled {
		t.Skip("the race detector enlarges the library's frames")
	}
	for i := range cases {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStackWithinCharge$", "-test.count=1") //nolint:gosec // G204: the test binary itself, with fixed arguments.
		cmd.Env = append(os.Environ(), stackCaseEnv+"="+strconv.Itoa(i))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("case %d: the stack outgrew two thirds of the charge: %v\n%.400s", i, err, out)
		}
	}
}

// TestHandleBoundsUnevaluatedMemory covers the unevaluated-tracking maps:
// under unevaluatedProperties the library copies every member name at every
// in-place evaluation of a reference chain, and all the copies are live at
// once. Through Factory.Build and Handle at the 10 MiB default, a 19 KB
// object against a 1,000-link chain (about 140 MB of copies) is oversized
// before the library runs; with a 64 MiB limit, the 100-link chain
// validates with the live heap at its peak, at the deepest link where all
// the copies are live (heapMeter), within the reservation.
func TestHandleBoundsUnevaluatedMemory(t *testing.T) {
	requireMeter(t)
	body := wideObject(2000)
	f := buildFilter(t, unevaluatedChain(1000, "unevaluatedProperties"), 0)
	x, _ := exchange("application/json", []byte(body))
	r := f.Handle(t.Context(), phase.OnRequestBody, x)
	if r.Outcome != filter.CannotDecide || !errors.Is(r.Err, errWorkingSetTooLarge) {
		t.Fatalf("Handle = %+v, want CannotDecide with the working-set ErrTooLarge", r)
	}
	if x.Budget.Reserved != 0 {
		t.Errorf("reservation left: %d", x.Budget.Reserved)
	}

	schema := unevaluatedChain(100, "unevaluatedProperties")
	f = buildFilter(t, schema, 64<<20)
	m := &heapMeter{}
	r, reserved := handlePeak(t, f, schema, body, m)
	if r.Outcome != filter.Continue || reserved == 0 {
		t.Fatalf("Handle at 64 MiB = %+v, reserved %d, want Continue", r, reserved)
	}
	if _, scan := scanBody(t, f.schema, body); scan.path < 100*2000*propMapEntry {
		t.Errorf("the path charges %d bytes, less than the 100 copies of 2,000 names", scan.path)
	}
	// The deepest link, the first exit probe, holds the 100 copies.
	deepest := m.at["/$defs/d100"]
	t.Logf("reserved %d (decoded values included), live peak %d, at the deepest link %v", reserved, m.peak(), deepest)
	if len(deepest) != 1 || deepest[0] < 100*2000*16 {
		t.Errorf("the deepest link measured %v, want once with the 100 copies of 2,000 names live", deepest)
	}
	if exceeds(m.peak(), reserved) {
		t.Errorf("Handle held %d bytes at its peak, reserved %d", m.peak(), reserved)
	}
}

// unevaluatedBranches returns an allOf of n references to the head of a
// chain of three in-place references (allOfRefs) under unevaluated
// keyword kw: 5n+1 evaluations at a value that may copy its member names
// or item indexes, of which one chain of five holds its copies at once.
func unevaluatedBranches(n int, kw string) string {
	last := `{"minProperties":0}`
	if kw == "unevaluatedItems" {
		last = `{"minItems":0}`
	}
	return strings.Replace(allOfRefs(n, last), `{"allOf":`, `{"`+kw+`":true,"allOf":`, 1)
}

// TestHandleUnevaluatedBranches pins that the unevaluated-tracking maps are
// charged the copies of one in-place chain, not one per evaluation that may
// copy them: the library drops a branch's copy when it merges it into its
// caller's, before the next branch of an allOf, anyOf or oneOf runs, so
// only the copies of one in-place chain are live at once, as for the call
// stack. Through Factory.Build and Handle at the 10 MiB default, a valid
// 49 KB object under an allOf of 50 three-link chains with
// unevaluatedProperties (129 MB, 84 times its peak, and 413 if charged per
// evaluation) and a valid array under the same with unevaluatedItems
// continue. With a 64 MiB limit the live heap at the deepest link of every
// branch, where the copies of its chain and of the root are live, is
// measured (heapMeter): it holds the copies of one chain, stays flat from
// the first branch to the last, and stays within the reservation.
func TestHandleUnevaluatedBranches(t *testing.T) {
	requireMeter(t)
	for _, tc := range []struct {
		kw, body string
		// n is the members or items, key the bytes of each in a copy's
		// map, at the least.
		n, key int64
	}{
		{"unevaluatedProperties", wideObject(5000), 5000, 16},
		{"unevaluatedItems", denseArray(8000, `1`, 0), 8000, 8},
	} {
		t.Run(tc.kw, func(t *testing.T) {
			schema := unevaluatedBranches(50, tc.kw)
			if r := handle(t, buildFilter(t, schema, 0), tc.body); r.Outcome != filter.Continue {
				t.Fatalf("Handle(%d bytes) at the default limit = %+v, want Continue", len(tc.body), r)
			}
			f := buildFilter(t, schema, 64<<20)
			m := &heapMeter{only: "/$defs/c3"}
			r, reserved := handlePeak(t, f, schema, tc.body, m)
			if r.Outcome != filter.Continue {
				t.Fatalf("Handle at 64 MiB = %+v, want Continue", r)
			}
			deepest := m.at["/$defs/c3"]
			if len(deepest) != 50 {
				t.Fatalf("the deepest link was measured %d times, want once per branch", len(deepest))
			}
			least, most := slices.Min(deepest), slices.Max(deepest)
			t.Logf("reserved %d (decoded values included), live at the deepest link %d to %d", reserved, least, most)
			// The root's copy and the five of one chain: the branch, its
			// reference and the three links.
			if least < 5*tc.n*tc.key {
				t.Errorf("the deepest link held %d bytes, less than five copies of %d keys", least, tc.n)
			}
			if most-least > treeSlack {
				t.Errorf("the deepest link held %d bytes in one branch and %d in another: the copies of earlier branches stayed live", least, most)
			}
			if exceeds(m.peak(), reserved) {
				t.Errorf("Handle held %d bytes at its peak, reserved %d", m.peak(), reserved)
			}
		})
	}
}

// TestHandleBoundsMatchedItems covers contains: every evaluation with
// contains lists the indexes of the items that match it, and the location
// pass keeps the list in a failing minContains or maxContains error: along
// a 100-link chain, 16,000 matching items hold a 12.7 MB error tree.
// Through Factory.Build and Handle at the 10 MiB default, the failing body
// is located with the live heap at its peak, at the end of the location
// pass (heapMeter), within the reservation.
func TestHandleBoundsMatchedItems(t *testing.T) {
	requireMeter(t)
	const n = 16000
	schema := chain(100, `{"$ref":"#/$defs/d%d","contains":true,"maxContains":0,"x-n":%d}`)
	body := denseArray(n, `1`, 0)
	f := buildFilter(t, schema, 0)
	m := &heapMeter{}
	r, reserved := handlePeak(t, f, schema, body, m)
	if got := detailOf(t, r); got != DetailMismatch+" at /$defs/d0/maxContains" {
		t.Fatalf("detail = %q, want the location of maxContains", got)
	}
	if _, scan := scanBody(t, f.schema, body); scan.locate < int64(100*n*8) {
		t.Errorf("the location pass is charged %d bytes, less than the 100 lists of %d indexes", scan.locate, n)
	}
	// The location pass ends at the root, its exit probe holding the lists
	// of the whole chain.
	root := m.at[""]
	t.Logf("reserved %d (decoded values included), live peak %d, at the root %v", reserved, m.peak(), root)
	if len(root) != 1 || root[0] < int64(100*n*8) {
		t.Errorf("the root measured %v, want once with the 100 lists of %d indexes live", root, n)
	}
	if exceeds(m.peak(), reserved) {
		t.Errorf("Handle held %d bytes at its peak, reserved %d", m.peak(), reserved)
	}
}

// peakExchange records the largest reservation.
type peakExchange struct {
	*filtertest.Exchange
	peak int64
}

func (x *peakExchange) Reserve(n int64) error {
	if err := x.Exchange.Reserve(n); err != nil {
		return err
	}
	x.peak = max(x.peak, x.Budget.Reserved)
	return nil
}

// TestHandleBoundsValidationMemory is the regression test of the
// working-set bound (proposed) through Handle: a dense failing array
// against an 8-way oneOf, validated in both passes, holds at its peak no
// more than Handle reserved, the live heap measured at the end of the
// location pass, which holds the tree (heapMeter), and a larger one is
// refused as oversized before the library runs, allocating no more than
// its decode.
func TestHandleBoundsValidationMemory(t *testing.T) {
	requireMeter(t)
	schema := `{"type":"array","items":` + oneOfConsts(8) + `}`
	// 1,000,000 items at the 10 MiB default: the decode fits (33 MiB of
	// the 40 MiB budget), validation could hold gigabytes.
	var before, after runtime.MemStats
	big := []byte(denseArray(1_000_000, `"x"`, 0))
	f := buildFilter(t, schema, 0)
	var r filter.Result
	allocs := func(run func()) float64 {
		runtime.GC()
		runtime.ReadMemStats(&before)
		run()
		runtime.ReadMemStats(&after)
		return float64(after.TotalAlloc - before.TotalAlloc)
	}
	decoded := allocs(func() { _, _, _ = f.decode(big) })
	x, _ := exchange("application/json", big)
	handled := allocs(func() { r = f.Handle(t.Context(), phase.OnRequestBody, x) })
	if r.Outcome != filter.CannotDecide || !errors.Is(r.Err, errWorkingSetTooLarge) || !errors.Is(r.Err, filter.ErrTooLarge) {
		t.Fatalf("Handle = %+v, want CannotDecide with the working-set ErrTooLarge", r)
	}
	if handled > decoded*1.125 {
		t.Errorf("a refused body allocated %.0f bytes, its decode %.0f: want no more than the decode", handled, decoded)
	}
	if x.Budget.Reserved != 0 {
		t.Errorf("reservation left: %d", x.Budget.Reserved)
	}

	f = buildFilter(t, schema, 4<<20)
	m := &heapMeter{}
	r, reserved := handlePeak(t, f, schema, denseArray(5000, `"x"`, 0), m)
	if got := detailOf(t, r); got != DetailMismatch+" at /items/oneOf/0/const" || reserved == 0 {
		t.Fatalf("detail = %q, reserved %d", got, reserved)
	}
	t.Logf("live peak %d, reserved %d (%.2f)", m.peak(), reserved, float64(m.peak())/float64(reserved))
	if len(m.at[""]) != 1 || len(m.at[endMark]) != 2 {
		t.Errorf("the location pass reached its root %d times, the passes ended %d times; want once and twice", len(m.at[""]), len(m.at[endMark]))
	}
	if exceeds(m.peak(), reserved) {
		t.Errorf("Handle held %d bytes at its peak, reserved %d", m.peak(), reserved)
	}
}

// TestWorkingSetCharges pins the working-set model (proposed) on small
// bodies. The decision pass is charged decideErrorBytes per error unit of
// the value's kind and per group error, an object one member's errors per
// evaluation with member keywords (a valid body is not charged a failing
// body's location-bearing tree); the location pass errorBytes plus
// locationBytes per level per unit, the name lists and a failing numeric
// keyword's rational; the path the stack at stackBytes per evaluation
// (the decision pass's wrapper adds decideFrames), the unevaluated copies
// and the names additionalProperties false lists; the transient the
// largest string an evaluation copies. Work terms never add to them.
func TestWorkingSetCharges(t *testing.T) {
	enum := make([]string, 40)
	for i := range enum {
		enum[i] = fmt.Sprintf(`"v%d"`, i)
	}
	const (
		at0, at1 = errorBytes, errorBytes + locationBytes
		e        = decideErrorBytes
		wrapper  = decideFrames*stackBytes + listGrowthBytes
		// short is the copy of a string shorter than 16 bytes, which may
		// start a block of the allocator's tiny allocator (copyBytes).
		short = tinyBlockBytes
	)
	for _, tc := range []struct {
		name, schema, body              string
		decide, locate, path, transient int64
	}{
		{"no subschema below the root", `{"type":"array"}`, `{"a":[1,2]}`, e, at0, stackBytes + wrapper, 0},
		{"a type that accepts the value's kind", `{"type":["array","object"]}`, `[1,2]`, 0, 0, stackBytes + wrapper, 0},
		{"true never fails", `true`, `{"a":[1]}`, 0, 0, stackBytes + wrapper, 0},
		// Two failing items and the group of the array's evaluation.
		{"items", `{"items":{"type":"string"}}`, `[1,2]`, 3 * e, 2 * at1, 2*stackBytes + wrapper, 0},
		{"type list", `{"type":["null","array","object","boolean","integer","number"]}`, `"x"`, e, 2 * at0, stackBytes + wrapper, short},
		{"required list", `{"required":["a","b","c","d","e","f","g"]}`, `{}`, e, 3 * at0, stackBytes + wrapper, 0},
		{"enum comparisons hold no memory", `{"enum":[` + strings.Join(enum, ",") + `]}`, `"x"`, e, at0, stackBytes + wrapper, short},
		{"lengths that hold", `{"minLength":1,"maxLength":3}`, `"xyz"`, 0, 0, stackBytes + wrapper, short},
		// An applicator next to lengths that hold can still fail, with its
		// branches.
		{"lengths that hold beside an applicator", `{"maxLength":9,"anyOf":[{"type":"integer"},{"type":"null"}]}`, `"xyz"`, 3 * e, 3 * at0, 2*stackBytes + wrapper, short},
		{"lengths in characters", `{"maxLength":1}`, `"é"`, 0, 0, stackBytes + wrapper, short},
		{"two failing lengths and a group", `{"minLength":2,"maxLength":0}`, `"x"`, 3 * e, 2 * at0, stackBytes + wrapper, short},
		{"a keyword of another kind", `{"minLength":1,"maximum":3}`, `""`, e, at0, stackBytes + wrapper, 0},
		{"reference", `{"$ref":"#/$defs/s","$defs":{"s":{"type":"string"}}}`, `1`, 2 * e, 2 * at0, 2*stackBytes + wrapper, 0},
		// A $recursiveRef wraps its target's errors as a $ref does.
		{"recursive reference", `{"$recursiveRef":"#/$defs/s","$defs":{"s":{"type":"string"}}}`, `1`, 2 * e, 2 * at0, 2*stackBytes + wrapper, 0},
		// if, then and else all count, then failing on its length and else
		// on its applicator; the applicators run only when nothing else
		// failed. The stack holds one chain of them at a time: the root,
		// else and one branch of its allOf.
		{"if, then and else", `{"if":{"type":"string"},"then":{"minLength":2},"else":{"allOf":[true,true]}}`, `"x"`, 2 * e, 2 * at0, 3*stackBytes + wrapper, short},
		// A failing name: its errors, located at the name, and the error
		// naming it, in either pass; the object's group.
		{"property names", `{"propertyNames":{"maxLength":3}}`, `{"abcd":1}`, e + 2*at0, at1 + at0, 2*stackBytes + wrapper, short},
		// One of the object's three evaluations applies propertyNames, so
		// the name failing both branches (three units with the group) is
		// wrapped once, in either pass; the root's allOf fails with its
		// group.
		{
			"property names in one of three evaluations", `{"allOf":[{"propertyNames":{"allOf":[{"maxLength":3},{"maxLength":2}]}},{"minProperties":1}]}`, `{"abcd":1}`,
			3*e + (3+1)*at0, 3*at1 + at0 + 2*at0, 4*stackBytes + wrapper, short,
		},
		{"additional members", `{"additionalProperties":false}`, `{"a":1,"b":2}`, e + e, at0 + 2*at1, 2*stackBytes + 2*listedNameBytes + wrapper, 0},
		// The group the evaluation keeping every member's errors may add.
		{"unevaluated names", `{"unevaluatedProperties":true}`, `{"a":1,"b":2}`, e, 0, 2*stackBytes + propMapBase + 2*propMapEntry + wrapper, 0},
		{"unevaluated indexes", `{"unevaluatedItems":true}`, `[1,2]`, e, 0, 2*stackBytes + itemMapBase + 2*itemMapEntry + wrapper, 0},
		{"unique items", `{"uniqueItems":true}`, `[{"a":1},{"b":2}]`, e, at0, 3*stackBytes + hashNameBytes + wrapper, 0},
		{"rational", `{"items":{"minimum":0}}`, `[1e-10]`, 2 * e, at1 + ratBytes + 8, 2*stackBytes + wrapper, 0},
		// One member's errors for the one evaluation looping over them,
		// or else the required failure: never both.
		{
			"one member of an object", `{"type":"object","required":["a"],"properties":{"a":{"type":"string","minLength":2,"pattern":"^x"},"b":{"type":"integer"}}}`,
			// The integer type admits an integer literal.
			`{"a":"x","b":1}`, 3 * e, 2*at0 + 2*at1, 2*stackBytes + wrapper, matcherBytes + matcherInstBytes*5 + jobArrayBytes + jobBytes,
		},
		// The integer type may fail on a decimal literal (1.0 passes, 1.5
		// not). A lower and an upper bound that leave numbers between them
		// cannot both fail, but the upper bounds can, which adds a group;
		// every failing bound keeps the number's rational.
		{"integer type on a decimal", `{"type":"integer"}`, `1.0`, e, at0, stackBytes + wrapper, 0},
		// A number type admits an integer literal.
		{"integer under a number type", `{"type":"number"}`, `1`, 0, 0, stackBytes + wrapper, 0},
		// An upper-case exponent makes a decimal literal too.
		{"integer type on an exponent", `{"type":"integer"}`, `1E-5`, e, at0, stackBytes + wrapper, 0},
		// The decision pass's evaluation with contains lists the matching
		// items while it loops, matchedItemBytes each, on the path.
		{"matching items listed", `{"contains":true}`, `[1,2,3,4]`, 2 * e, at0, 2*stackBytes + 4*matchedItemBytes + wrapper, 0},
		{"bounds that cannot both fail", `{"minimum":1,"maximum":9,"exclusiveMaximum":9}`, `0`, 3 * e, 2*at0 + 2*(ratBytes+1), stackBytes + wrapper, 0},
		{"bounds that can all fail", `{"minimum":2,"exclusiveMinimum":1,"exclusiveMaximum":1}`, `1`, 4 * e, 3*at0 + 3*(ratBytes+1), stackBytes + wrapper, 0},
		{"exclusive bounds that meet", `{"exclusiveMinimum":1,"exclusiveMaximum":1}`, `1`, 3 * e, 2*at0 + 2*(ratBytes+1), stackBytes + wrapper, 0},
		{"item counts", `{"minItems":1,"maxItems":3}`, `[]`, e, at0, stackBytes + wrapper, 0},
		{"member counts that can both fail", `{"minProperties":3,"maxProperties":1}`, `{}`, 3 * e, 2 * at0, stackBytes + wrapper, 0},
		// A string an evaluation reaches, even true's, is copied; one that
		// no evaluation reaches is no transient copy.
		{"a string true reaches", `{"items":false,"prefixItems":[true]}`, `["` + strings.Repeat("a", 64) + `"]`, e, 0, 2*stackBytes + wrapper, 64},
		{"a member no schema reaches", `{"properties":{"a":true}}`, `{"b":"` + strings.Repeat("a", 64) + `"}`, 0, 0, stackBytes + wrapper, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, scan := scanBody(t, mustCompile(t, tc.schema), tc.body)
			if scan.decide != tc.decide || scan.locate != tc.locate || scan.path != tc.path || scan.transient != tc.transient {
				t.Errorf("decide, locate, path, transient = %d, %d, %d, %d, want %d, %d, %d, %d",
					scan.decide, scan.locate, scan.path, scan.transient, tc.decide, tc.locate, tc.path, tc.transient)
			}
		})
	}
	// The largest copy is a string the library copies, or a matcher: of
	// the five instructions of the engine's program for ^a, with the
	// backtracker's starting job stack, which holds its one job (the
	// program has no alternation).
	_, scan := scanBody(t, mustCompile(t, `{"items":{"pattern":"^a"}}`), `["`+strings.Repeat("a", 64)+`"]`)
	if want := int64(matcherBytes + matcherInstBytes*5 + jobArrayBytes + jobBytes); scan.transient != want {
		t.Errorf("transient = %d, want the matcher's %d", scan.transient, want)
	}
	// With alternations, one job per alternation and byte, plus the first.
	_, scan = scanBody(t, mustCompile(t, `{"items":{"pattern":"a*b"}}`), `["`+strings.Repeat("a", 64)+`"]`)
	if want := int64(matcherBytes + matcherInstBytes*6 + jobArrayBytes + jobBytes*(1*65+1)); scan.transient != want {
		t.Errorf("transient = %d, want the matcher's %d", scan.transient, want)
	}
	_, scan = scanBody(t, mustCompile(t, `{"uniqueItems":true}`), denseArray(21, `1`, 0))
	if want := int64(21 * dupItemBytes); scan.transient != want {
		t.Errorf("transient = %d, want the duplicates map's %d", scan.transient, want)
	}
	// Nine evaluations looping over the members with nine different
	// outcomes after the loop (dependentRequired lists of 0 to 8 names),
	// more than a state details: each still keeps one member's errors, at
	// most the largest of them. Units: the root's allOf; the 8 groups of
	// the loops that may collect two errors; the 36 after the loops; and
	// 9 members' worth of 9 failing types.
	var branches, members []string
	for i := range 9 {
		var deps []string
		for j := range i {
			deps = append(deps, fmt.Sprintf(`"d%d":["x"]`, j))
		}
		branches = append(branches, `{"patternProperties":{"^m":{"type":"string"}},"dependentRequired":{`+strings.Join(deps, ",")+`}}`)
	}
	for i := range 20 {
		members = append(members, fmt.Sprintf(`"m%d":%d`, i, i))
	}
	_, scan = scanBody(t, mustCompile(t, `{"allOf":[`+strings.Join(branches, ",")+`]}`), `{`+strings.Join(members, ",")+`}`)
	if want := int64((1 + 8 + 36 + 9*9) * e); scan.decide != want {
		t.Errorf("decide = %d, want %d", scan.decide, want)
	}
}

// TestPathStates pins that each value is charged the schemas of its own
// path, not the costliest member class at its depth (a 12-way union of
// payment objects next to a plain array of tags would make every tag as
// costly as the union, and a valid 49 KB order 413 at the 10 MiB default):
// the union adds the same to the scan whatever the number of tags, a tag
// costs what it costs without the union, and a valid order with 300,000
// tags (3.4 MB) passes at the default with its reservation covering the
// live heap at its peak and within two and a half times it (heapMeter).
func TestPathStates(t *testing.T) {
	requireMeter(t)
	s := mustCompile(t, unionSchema(12))
	noUnion := mustCompile(t, strings.Replace(unionSchema(0), `,"payment":{"oneOf":[]}`, "", 1))
	extra := func(n int) (decide, locate, work int64) {
		body := unionBody(n, `"tag%d"`)
		_, with := scanBody(t, s, body)
		_, without := scanBody(t, noUnion, strings.Replace(body, `"payment":{"type":"m0","amount":10.5,"currency":"EUR"},`, "", 1))
		return with.decide - without.decide, with.locate - without.locate, with.work - without.work
	}
	d1, l1, w1 := extra(1000)
	d5, l5, w5 := extra(5000)
	if d1 != d5 || l1 != l5 || w1 != w5 {
		t.Errorf("the union adds %d, %d, %d with 1,000 tags and %d, %d, %d with 5,000: want the same", d1, l1, w1, d5, l5, w5)
	}
	body := unionBody(300000, `"tag%d"`)
	f := buildFilter(t, unionSchema(12), 0)
	m := &heapMeter{}
	r, reserved := handlePeak(t, f, unionSchema(12), body, m)
	if r.Outcome != filter.Continue {
		t.Fatalf("Handle(%d bytes) = %+v, want Continue", len(body), r)
	}
	t.Logf("body %d bytes: reserved %d (decoded values included), live peak %d (%.2f)", len(body), reserved, m.peak(), float64(reserved)/float64(m.peak()))
	if float64(reserved) > 2.5*float64(m.peak()) {
		t.Errorf("reserved %d, more than two and a half times the peak %d", reserved, m.peak())
	}
	// The tags of 9 bytes, each alone in its tiny block, would hold the
	// live heap 3.6% over a reservation of their bytes alone.
	if exceeds(m.peak(), reserved) {
		t.Errorf("Handle held %d bytes at its peak, reserved %d", m.peak(), reserved)
	}
}

// TestMatcherReach pins the patterns whose matchers a value is charged: a
// string only those its own path applies to it, a member name those of its
// object's patternProperties, a value of another kind none, and where the
// path is unknown, every pattern only where the depth's counts may scan a
// string, which uniqueItems hashing does not.
func TestMatcherReach(t *testing.T) {
	pc := newPatternCosts()
	reach := func(expr string) patternReach { return reachOf(pc.of(expr)) }
	email, bio := reach(emailExpr()), reach(`^[^<>]*$`)
	long := strings.Repeat("x", 1000)
	_, scan := scanBody(t, mustCompile(t, signUpSchema()), `{"email":"a@b.co","name":"Ann","bio":"`+long+`"}`)
	if want := max(email.bytes(len("a@b.co")), bio.bytes(len(long))); scan.transient != want {
		t.Errorf("sign-up: transient %d, want %d (the bio's own pattern; the e-mail pattern's charge on it would be %d)", scan.transient, want, email.bytes(len(long)))
	}
	// A member name is matched against its object's patternProperties,
	// the value against its own pattern.
	names := `{"patternProperties":{"^x+":true},"properties":{"a":{"pattern":"^[^<>]*$"}}}`
	_, scan = scanBody(t, mustCompile(t, names), `{"`+long+`":1,"a":"short"}`)
	if want := reach(`^x+`).bytes(len(long)); scan.transient != want || want <= bio.bytes(len("short")) {
		t.Errorf("member name: transient %d, want %d", scan.transient, want)
	}
	// The library matches a pattern against strings only.
	if _, scan := scanBody(t, mustCompile(t, `{"pattern":"^a"}`), `1`); scan.transient != 0 {
		t.Errorf("number under a pattern: transient %d, want 0", scan.transient)
	}
	// Where the scan does not know the path, a pattern may match a value at
	// the host's depth, but the uniqueItems hashing of the tags below it
	// scans none.
	tags := mustCompile(t, `{"type":"object","properties":{"host":{"type":"string","pattern":`+fmt.Sprintf("%q", hostnamePattern)+
		`},"tags":{"type":"array","uniqueItems":true,"items":{"type":"string"}}}}`)
	unknown := &Schema{work: tags.work, numbers: tags.numbers}
	unknown.work.states = nil
	_, scan = scanBody(t, unknown, `{"host":"example.com","tags":["`+strings.Repeat("t", 100)+`"]}`)
	if want := tags.work.flags.patterns.bytes(len("example.com")); scan.transient != want {
		t.Errorf("unknown paths: transient %d, want %d for the host alone (the tag's would be %d)", scan.transient, want, tags.work.flags.patterns.bytes(100))
	}
	if tags.work.at[2][dimScan] != 0 || tags.work.at[2][dimHash] == 0 {
		t.Errorf("the tags' counts: scan %d, hash %d, want none and some", tags.work.at[2][dimScan], tags.work.at[2][dimHash])
	}
	// A member name meets every pattern of its object's patternProperties,
	// here the costliest second by text, and where the path is unknown the
	// name is charged as a string is, whatever the value.
	const wide = `a(?:(?:a??){240}a)*b`
	pats := mustCompile(t, `{"patternProperties":{"^a":true,"`+wide+`":true}}`)
	name := strings.Repeat("a", 500)
	_, scan = scanBody(t, pats, `{"`+name+`":"x"}`)
	if want := reachOf(pc.of(`^a`), pc.of(wide)).bytes(len(name)); scan.transient != want || want <= reach(`^a`).bytes(len(name)) {
		t.Errorf("a name under two patterns: transient %d, want %d, more than the first pattern's %d", scan.transient, want, reach(`^a`).bytes(len(name)))
	}
	unknown = &Schema{work: pats.work, numbers: pats.numbers}
	unknown.work.states = nil
	_, scan = scanBody(t, unknown, `{"`+name+`":"x"}`)
	if want := pats.work.flags.patterns.bytes(len(name)); scan.transient != want || want <= pats.work.flags.patterns.bytes(1) {
		t.Errorf("a name on an unknown path: transient %d, want %d for the name", scan.transient, want)
	}
}

// TestHandleWalkMemory covers the location walk: after the location pass,
// which no probe follows, the walk builds a pointer for each leaf of the
// error tree and keeps the best (a document whose failing keyword lies
// under a $defs name of 100,000 bytes holds 106 KB in the walk of a body
// of one byte). The location pass is charged the walk's pointers
// (walkBytes), at least four of the document's longest location, and
// Handle reserves them; the walk's pointers, replayed on the tree
// (walkCopy), are at least the one it builds and the best, within the
// reservation, and bound what the walk allocates.
func TestHandleWalkMemory(t *testing.T) {
	requireMeter(t)
	const n = 100000
	name := strings.Repeat("n", n)
	schema := `{"$ref":"#/$defs/` + name + `","$defs":{"` + name + `":{"type":"string","minLength":2}}}`
	f := buildFilter(t, schema, 0)
	_, scan := scanBody(t, f.schema, `1`)
	if walk := walkBytes(f.schema.work.flags.maxPointer); walk < 4*n || scan.locateMemory() < walk {
		t.Errorf("the location pass is charged %d bytes, its walk %d, want at least four pointers of %d bytes", scan.locateMemory(), walk, n)
	}
	m := &heapMeter{}
	r, reserved := handlePeak(t, f, schema, `1`, m)
	if got := detailOf(t, r); got != DetailMismatch+" at /$defs" {
		t.Fatalf("detail = %q, want the location cut back to /$defs", got)
	}
	var verr *sjs.ValidationError
	if !errors.As(f.schema.root.Validate(decodeInstance(t, `1`)), &verr) {
		t.Fatal("the body is valid")
	}
	var a, b runtime.MemStats
	gc := debug.SetGCPercent(-1)
	runtime.ReadMemStats(&a)
	_, _, _ = locationOf(verr, nil)
	runtime.ReadMemStats(&b)
	debug.SetGCPercent(gc)
	t.Logf("reserved %d, live peak %d, the walk replayed %d, allocated %d", reserved, m.peak(), m.copies[copyWalk], b.TotalAlloc-a.TotalAlloc)
	if walk := m.copies[copyWalk]; walk < 2*n || walk < growth(b.TotalAlloc, a.TotalAlloc) {
		t.Errorf("the walk replayed %d bytes, want the two pointers of %d bytes and its %d allocated", walk, n, b.TotalAlloc-a.TotalAlloc)
	}
	if exceeds(m.peak(), reserved) {
		t.Errorf("Handle held %d bytes at its peak, reserved %d", m.peak(), reserved)
	}
}

// TestHandleLocationPass covers the two passes (proposed): a valid body
// runs the decision pass only, reserving its working set with the decoded
// values; a failing body gets the location of the failing keyword when the
// location pass fits the working-set limit, its work the work limit with
// the first pass's, and its reservation the budget, and otherwise
// DetailMismatch alone, a decision, never 413 or 503.
func TestHandleLocationPass(t *testing.T) {
	small := records(10)
	failing := strings.Replace(small, `"qty":1,`, `"qty":0,`, 1)
	f := buildFilter(t, recordsSchema, 0)
	var reserves []int64
	run := func(f *Filter, body string, refuse int) filter.Result {
		t.Helper()
		reserves = nil
		x := &recordingExchange{Exchange: &filtertest.Exchange{}, onReserve: func(n int64) { reserves = append(reserves, n) }}
		x.Msg = &filtertest.Message{Hdr: http.Header{"Content-Type": {"application/json"}}, Raw: []byte(body)}
		if refuse > 0 {
			x.reserveFrom, x.reserveErr = refuse, filter.ErrBudget
		}
		r := f.Handle(t.Context(), phase.OnRequestBody, x)
		if x.Budget.Reserved != 0 {
			t.Errorf("reservation left: %d", x.Budget.Reserved)
		}
		return r
	}
	_, scan := scanBody(t, f.schema, small)
	_, cost, err := f.decode([]byte(small))
	if err != nil {
		t.Fatal(err)
	}
	want := cost + scan.extra + scan.decideMemory()
	if r := run(f, small, 0); r.Outcome != filter.Continue || len(reserves) != 1 || reserves[0] != want {
		t.Errorf("valid body: %+v, reserved %v, want Continue reserving %d once", r, reserves, want)
	}
	if got := detailOf(t, run(f, failing, 0)); got != DetailMismatch+" at /$defs/item/properties/qty/minimum" || len(reserves) != 2 {
		t.Errorf("failing body: %q, reserved %v, want the location after two reservations", got, reserves)
	}
	// The location pass's reservation refused: the decision stands.
	if got := detailOf(t, run(f, failing, 2)); got != DetailMismatch {
		t.Errorf("location reservation refused: %q, want DetailMismatch alone", got)
	}
	// The location pass over the working-set limit, or the two passes
	// over the work limit: the decision alone.
	tight := buildFilter(t, recordsSchema, 0)
	_, fs := scanBody(t, tight.schema, failing)
	tight.limits.workingSet = fs.decideMemory()
	if got := detailOf(t, run(tight, failing, 0)); got != DetailMismatch || len(reserves) != 1 {
		t.Errorf("location over the working-set limit: %q, reserved %v, want DetailMismatch alone", got, reserves)
	}
	tight = buildFilter(t, recordsSchema, 0)
	tight.limits.work = 2*fs.work + fs.walk - 1
	if got := detailOf(t, run(tight, failing, 0)); got != DetailMismatch {
		t.Errorf("two passes and the walk over the work limit: %q, want DetailMismatch alone", got)
	}
	tight.limits.work = 2*fs.work + fs.walk
	if got := detailOf(t, run(tight, failing, 0)); got == DetailMismatch {
		t.Errorf("two passes and the walk at the work limit: %q, want the location", got)
	}
	// A request that ends between the passes gets the decision alone.
	x, _ := exchange("application/json", []byte(failing))
	if got := detailOf(t, f.Handle(&endingContext{Context: t.Context(), live: 1}, phase.OnRequestBody, x)); got != DetailMismatch {
		t.Errorf("context ended between the passes: %q, want DetailMismatch alone", got)
	}
	// Should the passes disagree, the decision pass's refusal stands.
	disagree := librarySchema(t, `true`)
	disagree.decide = libraryCompile(t, `true`)
	if got := detailOf(t, handle(t, newFilter(disagree, 400, limitsOf(testLimit)), `{}`)); got != DetailMismatch {
		t.Errorf("passes that disagree: %q, want DetailMismatch alone", got)
	}
}

// endingContext is a context whose Err is nil for its first live calls
// and context.Canceled after.
type endingContext struct {
	context.Context
	live int
}

func (c *endingContext) Err() error {
	if c.live > 0 {
		c.live--
		return nil
	}
	return context.Canceled
}

// TestScanAllocatesNothing checks that the walk before validation
// allocates nothing, member names under propertyNames included (the
// allocation target).
func TestScanAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not compared under -race")
	}
	s := mustCompile(t, `{"propertyNames":{"pattern":"^k"},"uniqueItems":true,"items":{"unevaluatedProperties":false,"minimum":0}}`)
	v := decodeInstance(t, `[{"k1":[1,2.5,"x"],"k2":{"k3":null}},{"k4":1e10}]`)
	allocs := testing.AllocsPerRun(100, func() {
		scan := bodyScan{ws: &s.work, numbers: s.numbers, workLimit: maxWorkLimit}
		scan.scan(v)
	})
	if allocs != 0 {
		t.Errorf("the scan allocates %.0f times", allocs)
	}
}

// TestWorkingSetFailsClosed covers the fallback of the working-set bound
// (proposed): without an analysis every body is oversized; the sums
// saturate.
func TestWorkingSetFailsClosed(t *testing.T) {
	s := mustCompile(t, `true`)
	s.work = newWorkingSet(nil, nil)
	r := handle(t, newFilter(s, 400, limitsOf(testLimit)), `{}`)
	if r.Outcome != filter.CannotDecide || !errors.Is(r.Err, errWorkingSetTooLarge) {
		t.Errorf("Handle without an analysis = %+v, want the working-set ErrTooLarge", r)
	}
	if got := mulCap(1<<40, 1<<40); got != math.MaxInt64 {
		t.Errorf("mulCap overflow = %d", got)
	}
	if got := addCap(math.MaxInt64-1, 2); got != math.MaxInt64 {
		t.Errorf("addCap overflow = %d", got)
	}
}

// growthItems is how many items TestGrowthCharges grows each list to.
const growthItems = 1 << 21

// checkGrowth replays a list of E grown by appending to growthItems items
// and checks, at every count n, the largest pair of arrays live at once so
// far against charge(n) and listGrowthBytes, and the last array against
// kept(n).
func checkGrowth[E any](t *testing.T, name string, charge, kept func(n int64) int64) {
	t.Helper()
	var s []E
	var zero E
	var pair, last int64
	for len(s) < growthItems {
		old := cap(s)
		s = append(s, zero)
		if c := cap(s); c != old {
			last = arrayBytes[E](c)
			pair = max(pair, arrayBytes[E](old)+last)
		}
		n := int64(len(s))
		if most := charge(n) + listGrowthBytes; pair > most {
			t.Fatalf("a list of %d %s held %d bytes while it grew, charged %d and listGrowthBytes", n, name, pair, charge(n))
		}
		if last > kept(n) {
			t.Fatalf("a list of %d %s holds %d bytes, charged %d", n, name, last, kept(n))
		}
	}
}

// TestGrowthCharges covers the charges per item of the lists and maps the
// library grows while it validates: a list or a map that grows holds its
// old array or table besides the new one while append or an insert copies
// it, which no mark of the meter sees. Each growth is replayed with the
// library's own types:
//
//   - the indexes of the items an evaluation with contains matches, an
//     []int it appends to while it loops (validator.go arrValidate): at
//     most matchedItemBytes per item of the array, of at least two items
//     (listedItems), the largest pair of arrays live at once (the pair of
//     a list just grown past a capacity holds up to 23.9 bytes per item,
//     one list of 175,105 matching items 3.15 MB), and the list the
//     location pass keeps in a minContains or maxContains error, its last
//     array, at most keptItemBytes per item;
//   - the names additionalProperties false lists, a []string (objValidate):
//     at most listedNameBytes per member;
//   - the errors an evaluation collects, a []*ValidationError (addErr): at
//     most the part of decideErrorBytes besides the error itself per error,
//     of at least two (a single error's list is one of the evaluation's
//     records, stackBytes);
//   - while a list grows, the allocator's size classes and the header it
//     puts in front of a large array of pointers round the new array up:
//     at most listGrowthBytes past the list's charge, charged once to a
//     pass, which grows one list at a time;
//   - the unevaluated names and indexes an evaluation copies, a
//     map[string]struct{} and a map[int]struct{} (unevalFrom): every
//     allocation of their growth, which bounds their peak, within
//     propMapBase plus propMapEntry per name and itemMapBase plus
//     itemMapEntry per index;
//   - the duplicates map of a uniqueItems check of more than pairwiseItems
//     items, a map[uint64][]int of one-index slices (duplicates): every
//     allocation of its growth within dupItemBytes per item.
//
// The maps' allocations follow their random hash seeds past a thousand
// keys by about 1% (TestDuplicatesReplica), which their charges leave room
// for.
func TestGrowthCharges(t *testing.T) {
	requireMeter(t)
	// The meter's replay of a list (appendPair) holds both arrays of a
	// growth: for 257 indexes, those of 256 and 512.
	if got := appendPair[int](257); got != (256+512)*8 {
		t.Errorf("appendPair(257) = %d, want the arrays of 256 and 512 indexes", got)
	}
	checkGrowth[int](t, "matching items",
		func(n int64) int64 { return listedItems(n) * matchedItemBytes },
		func(n int64) int64 { return listedItems(n) * keptItemBytes })
	checkGrowth[string](t, "undeclared names",
		func(n int64) int64 { return n * listedNameBytes },
		func(n int64) int64 { return n * listedNameBytes })
	// The first array of a single error's list is one of the evaluation's
	// own records (stackBytes).
	slot := decideErrorBytes - validationErrorBytes()
	checkGrowth[*sjs.ValidationError](t, "errors",
		func(n int64) int64 { return max(n, 2) * slot },
		func(n int64) int64 { return max(n, 2) * slot })
	// What a map's build allocates, the fewest of matchAttempts builds:
	// the runtime's own allocations meanwhile (heapNoise) only add to it.
	allocated := func(f func()) int64 {
		least := int64(math.MaxInt64)
		for range matchAttempts {
			var a, b runtime.MemStats
			gc := debug.SetGCPercent(-1)
			runtime.ReadMemStats(&a)
			f()
			runtime.ReadMemStats(&b)
			debug.SetGCPercent(gc)
			least = min(least, growth(b.TotalAlloc, a.TotalAlloc))
		}
		return least
	}
	var names []string
	for i := range 1 << 17 {
		names = append(names, "m"+strconv.Itoa(i))
	}
	for _, n := range []int{1, 2, 7, 8, 9, 13, 14, 15, 27, 28, 29, 55, 56, 57, 111, 112, 113, 223, 224, 225, 447, 448, 449, 895, 896, 897, 898, 1000, 1792, 1793, 2000, 3585, 5000, 7169, 10000, 14337, 28673, 50000, 57345, 100000, 1 << 17} {
		props := allocated(func() {
			m := map[string]struct{}{}
			for _, name := range names[:n] {
				m[name] = struct{}{}
			}
		})
		items := allocated(func() {
			m := map[int]struct{}{}
			for i := range n {
				m[i] = struct{}{}
			}
		})
		dups := allocated(func() {
			m := make(map[uint64][]int)
			for i := range n {
				m[uint64(i)] = append(m[uint64(i)], i)
			}
		})
		if most := int64(propMapBase + propMapEntry*n); props > most {
			t.Errorf("a copy of %d unevaluated names allocated %d bytes, charged %d", n, props, most)
		}
		if most := int64(itemMapBase + itemMapEntry*n); items > most {
			t.Errorf("a copy of %d unevaluated indexes allocated %d bytes, charged %d", n, items, most)
		}
		if most := int64(dupItemBytes * n); n > pairwiseItems && dups > most {
			t.Errorf("a duplicates map of %d items allocated %d bytes, charged %d", n, dups, most)
		}
	}
}

// numberLiterals returns number literals within the number bounds that
// the library parses at the most cost: the longest mantissas of nines and
// sevens with the largest exponents of either sign, an integer and a
// fraction of the longest, and short ones.
func numberLiterals() []string {
	lits := []string{"1", "-1", "0.5", "7e-1000", "7e1000", "1E+1000"}
	for _, exp := range []string{"", "e-1000", "e1000", "e-999", "e+999"} {
		digits := MaxNumberBytes - len(exp)
		lits = append(lits, strings.Repeat("9", digits)+exp, "-"+strings.Repeat("9", digits-1)+exp,
			"1."+strings.Repeat("7", digits-2)+exp, strings.Repeat("3", digits/2)+"."+strings.Repeat("3", digits-digits/2-1)+exp)
	}
	return lits
}

// TestNumberCheckMemory covers numberCheckBytes (the rationals of a number
// check, which the library holds between two probes, are not measured but
// bounded): for every pair of literals within the number bounds that the
// library parses at the most cost, what one check allocates, all of it
// counted as if live at once, stays within it: the value's rational with
// the quotient multipleOf divides (validator.go numValidate), the two
// rationals a comparison parses (util.go equals), and the bytes uniqueItems
// hashes (writeHash). The library's own multipleOf check on them allocates
// within it too, with the validation's fixed allocations.
func TestNumberCheckMemory(t *testing.T) {
	requireMeter(t)
	allocated := func(f func()) int64 {
		least := int64(math.MaxInt64)
		for range 3 {
			var a, b runtime.MemStats
			gc := debug.SetGCPercent(-1)
			runtime.ReadMemStats(&a)
			f()
			runtime.ReadMemStats(&b)
			debug.SetGCPercent(gc)
			least = min(least, growth(b.TotalAlloc, a.TotalAlloc))
		}
		return least
	}
	parse := func(lit string) *big.Rat {
		r, ok := new(big.Rat).SetString(fmt.Sprint(json.Number(lit)))
		if !ok {
			t.Fatalf("%.20s does not parse", lit)
		}
		return r
	}
	lits := numberLiterals()
	most, at := int64(0), ""
	for _, a := range lits {
		if !numberInRange(a) {
			t.Fatalf("%q is outside the number bounds", a)
		}
		ra := parse(a)
		value := allocated(func() { parse(a) })
		hash := allocated(func() {
			r := parse(a)
			_, _ = r.Num().Bytes(), r.Denom().Bytes()
		})
		if hash > most {
			most, at = hash, "hashing "+a
		}
		for _, b := range lits {
			rb := parse(b)
			if n := value + allocated(func() { parse(b) }); n > most {
				most, at = n, "comparing "+a+" with "+b
			}
			if rb.Sign() > 0 {
				if n := value + allocated(func() { new(big.Rat).Quo(ra, rb) }); n > most {
					most, at = n, "dividing "+a+" by "+b
				}
			}
		}
	}
	t.Logf("the costliest check allocated %d bytes, %.40s", most, at)
	if most > numberCheckBytes {
		t.Errorf("a number check allocated %d bytes (%.60s), numberCheckBytes is %d", most, at, numberCheckBytes)
	}
	fixed := allocated(func() { _ = mustCompile(t, `true`).root.Validate(json.Number("1")) })
	for _, d := range []string{lits[6], lits[len(lits)-1], "7e-1000"} {
		s := libraryCompile(t, `{"multipleOf":`+d+`}`)
		for _, v := range lits {
			if n := allocated(func() { _ = s.Validate(json.Number(v)) }); n > numberCheckBytes+fixed+heapNoise {
				t.Errorf("multipleOf %.20s on %.20s allocated %d bytes", d, v, n)
			}
		}
	}
}

// TestCopySizes covers the charges of the copies the library allocates
// whole while it validates (they are rounded up to their allocation size,
// up to a quarter more past 32 KiB: the copy of a 100,000-byte string
// takes 106,496 bytes, and the sorted names of an object of 50,000 members
// 802,824):
//
//   - a string's copy (copyBytes), for every length to 256 KiB, past which
//     whole pages round every length alike, and around every power of two
//     to 16 MiB, as the meter replays it (stringCopy);
//   - the sorted names of a hashed object (namesBytes), for every member
//     count to 16,384 and around powers of two past (pointerSlice);
//   - the replays round as the allocator does: 10,000 copies of 1,000
//     bytes and 10,000 slices of 40 names, held at once, take their
//     replayed sizes, the header of a slice of pointers past 512 bytes
//     included, but for heapNoise.
func TestCopySizes(t *testing.T) {
	requireMeter(t)
	// Every length of a size class, or of a run of whole pages, takes the
	// allocation of its largest: replayed once per class to 256 KiB, past
	// which pages round alike, and around powers of two to 16 MiB.
	checkCopy := func(n, end int) {
		got := stringCopy(n)
		for m := n; m <= end; m++ {
			if charged := copyBytes(int64(m)); got > charged {
				t.Fatalf("the copy of %d bytes takes %d, charged %d", m, got, charged)
			}
		}
	}
	checkNames := func(n, end int) {
		got := pointerSlice(n, 16)
		for m := n; m <= end; m++ {
			if charged := namesBytes(int64(m)); got > charged {
				t.Fatalf("the sorted names of %d members take %d, charged %d", m, got, charged)
			}
		}
	}
	for n := 1; n <= 256<<10; {
		end := tinyBlockBytes - 1
		if n >= tinyBlockBytes {
			end = int(stringCopy(n))
		}
		checkCopy(n, end)
		n = max(end, n) + 1
	}
	for n := 1; n <= 16<<10; {
		got := pointerSlice(n, 16)
		end := int(got / 16)
		if got > 512 {
			end = int((got - mallocHeaderBytes) / 16)
		}
		checkNames(n, end)
		n = max(end, n) + 1
	}
	for n := 1 << 19; n <= 1<<24; n <<= 1 {
		for _, m := range []int{n - 1, n, n + 1} {
			checkCopy(m, m)
			checkNames(m/16, m/16)
		}
	}
	const copies, length, names = 10000, 1000, 40
	src := strings.Repeat("s", length)
	held := make([][]byte, copies)
	base := liveHeap()
	for i := range held {
		held[i] = []byte(src)
	}
	if live, want := growth(liveHeap(), base), int64(copies)*stringCopy(length); exceeds(live, want) || exceeds(want, live) {
		t.Errorf("%d copies of %d bytes held %d bytes, replayed at %d", copies, length, live, want)
	}
	runtime.KeepAlive(held)
	sorted := make([][]string, copies)
	base = liveHeap()
	for i := range sorted {
		sorted[i] = make([]string, 0, names)
	}
	if live, want := growth(liveHeap(), base), int64(copies)*pointerSlice(names, 16); exceeds(live, want) || exceeds(want, live) {
		t.Errorf("%d slices of %d names held %d bytes, replayed at %d", copies, names, live, want)
	}
	runtime.KeepAlive(sorted)
}
