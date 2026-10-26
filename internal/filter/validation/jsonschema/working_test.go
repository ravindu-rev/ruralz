// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// workingSetCases are dense bodies, each against a schema that fails at
// every value in a different way, or (the unevaluated shapes) holds the
// library's copies of member names or item indexes along a reference
// chain, or (the ordinary shapes) a valid or failing order body.
func workingSetCases() []struct{ name, schema, body string } {
	// denseArray, with a quarter of the items under -race, which slows the
	// library tenfold: the trees are measured exactly at any size.
	denseArray := func(n int, item string, depth int) string {
		if raceEnabled {
			n = max(n/4, 1)
		}
		return denseArray(n, item, depth)
	}
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
		// type, the lists of matching items contains keeps along a reference
		// chain, and the errors propertyNames wraps in every evaluation that
		// applies it.
		{"bounds that cannot both fail", `{"items":{"minimum":1,"maximum":9,"exclusiveMaximum":9}}`, denseArray(20000, `10`, 0)},
		{"integer type on decimals", `{"items":{"type":"integer"}}`, denseArray(20000, `1.5`, 0)},
		{"matching items along a chain", chain(100, `{"$ref":"#/$defs/d%d","contains":true,"maxContains":0,"x-n":%d}`), denseArray(16000, `1`, 0)},
		{"too few matching items", `{"allOf":[` + strings.TrimSuffix(strings.Repeat(`{"contains":{"type":"number"},"minContains":100000},`, 20), ",") + `]}`, denseArray(20000, `1`, 0)},
		{"property names in many evaluations", `{"anyOf":[` + strings.TrimSuffix(strings.Repeat(`{"propertyNames":{"type":"integer"}},`, 50), ",") + `]}`, wideObject(2000)},
		{"property names along a chain", chain(100, `{"$ref":"#/$defs/d%d","propertyNames":{"enum":["a","b"]},"x-n":%d}`), wideObject(2000)},
		// The member names of the depth bound, which clamps the states, when
		// the precise pass decides it.
		{"property names under mixins", strings.TrimSuffix(mixins, "}") + `,"propertyNames":{"allOf":[` + strings.TrimSuffix(strings.Repeat(`{"pattern":"^a","maxLength":2},`, 20), ",") + `]}}`, wideObject(2000)},
	}
}

// peakHeap returns the largest growth of the heap in use while f runs and
// until it returns, sampled, with the collector running often so that the
// samples follow the live heap rather than garbage.
func peakHeap(f func() any) float64 {
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	var base, m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	var done atomic.Bool
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var m runtime.MemStats
		for !done.Load() {
			runtime.ReadMemStats(&m)
			peak.Store(max(peak.Load(), m.HeapInuse))
			time.Sleep(100 * time.Microsecond)
		}
	}()
	kept := f()
	done.Store(true)
	<-sampled
	runtime.ReadMemStats(&m)
	runtime.KeepAlive(kept)
	return float64(max(peak.Load(), m.HeapInuse)) - float64(base.HeapInuse)
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

// gcSlack is the garbage the collector may leave in use between two
// collections while peakHeap runs it often (GOGC 10, with a heap minimum
// of 0.4 MiB), and the garbage allocated while it marks, which a sample of
// the heap in use cannot tell from live memory: the library allocates an
// error kind, a scope and an index string at every evaluation and drops
// them.
const gcSlack = 1 << 20

// liveProbe is the x-probe keyword of the test vocabulary: each time an
// evaluation reaches it, it collects the heap fully and records the
// largest live heap it measured.
type liveProbe struct{ live uint64 }

// Validate measures the live heap.
func (p *liveProbe) Validate(*sjs.ValidatorContext, any) {
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	p.live = max(p.live, m.HeapAlloc)
}

// probeURL names the measuring wrapper of decisionTree.
const probeURL = "https://schema.ruralz.invalid/probe.json"

// decisionTree returns the bytes of the error tree the decision pass holds
// for v under schema, measured from inside the pass when the tree is
// complete: the wrapper validates the document for a boolean result, as
// the decision pass does, under an anyOf that keeps the document's errors
// while it evaluates its second branch, whose x-probe keyword collects the
// heap and measures it. It is 0 for a valid v.
func decisionTree(t testing.TB, schema string, v any) int64 {
	t.Helper()
	probe := &liveProbe{}
	doc, err := sjs.UnmarshalJSON(strings.NewReader(schema))
	if err != nil {
		t.Fatal(err)
	}
	c := sjs.NewCompiler()
	c.DefaultDraft(sjs.Draft2020)
	c.UseLoader(refusingLoader{})
	c.AssertVocabs()
	c.RegisterVocabulary(&sjs.Vocabulary{
		URL: "https://schema.ruralz.invalid/vocab/probe",
		Compile: func(_ *sjs.CompilerContext, obj map[string]any) (sjs.SchemaExt, error) {
			if _, ok := obj["x-probe"]; ok {
				return probe, nil
			}
			return nil, nil
		},
	})
	wrapper := map[string]any{"not": map[string]any{"anyOf": []any{map[string]any{"$ref": baseURL}, map[string]any{"x-probe": true}}}}
	if err := c.AddResource(baseURL, doc); err != nil {
		t.Fatal(err)
	}
	if err := c.AddResource(probeURL, wrapper); err != nil {
		t.Fatal(err)
	}
	measure, err := c.Compile(probeURL)
	if err != nil {
		t.Fatal(err)
	}
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	_ = measure.Validate(v)
	if probe.live == 0 {
		return 0
	}
	return growth(probe.live, m.HeapAlloc)
}

// growth returns after - before, two heap sizes, as an int64.
func growth(after, before uint64) int64 {
	if after < before {
		return -growth(before, after)
	}
	return int64(min(after-before, math.MaxInt64))
}

// locationTree returns the bytes of the error tree the location pass
// returns for v, which holds every error it collected.
func locationTree(s *Schema, v any) int64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	err := s.root.Validate(v)
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(err)
	return growth(after.HeapAlloc, before.HeapAlloc)
}

// treeSlack covers what a measured tree holds besides the errors: the
// frames on the path to the measuring keyword and the collector's own
// bookkeeping.
const treeSlack = 64 << 10

// TestWorkingSetCoversPeak covers the working-set bound (proposed) of both
// passes: for dense bodies of every error shape, the unevaluated-tracking
// copies the library makes along a reference chain, and ordinary order
// bodies, valid or failing.
//
//   - The error tree the decision pass holds, measured exactly from inside
//     it, stays within what the scan charges it (a valid body is charged
//     the decision pass's tree, never the location pass's).
//   - The error tree the location pass returns, measured exactly, stays
//     within what the scan charges it.
//   - The heap in use at its peak while the location pass validates stays
//     within its whole working set, give or take the collector's slack:
//     that covers the stack, the unevaluated copies and the transient
//     copies, which the decision pass holds alike. A sample of the
//     decision pass's peak would mostly measure the garbage the library
//     allocates and drops in it (error kinds, scopes, parsed numbers).
func TestWorkingSetCoversPeak(t *testing.T) {
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
			peak := peakHeap(func() any { return s.root.Validate(v) })
			t.Logf("location pass: reserved %d, peak %.0f (%.2f)", scan.locateMemory(), peak, peak/float64(scan.locateMemory()))
			// The trees are measured exactly above; the sampled peak, which
			// adds the copies and the collector's pacing, gets a quarter more
			// (1.01 on a quiet machine).
			if peak > 1.25*float64(scan.locateMemory())+gcSlack {
				t.Errorf("the location pass held %.0f bytes at its peak, the reservation is %d", peak, scan.locateMemory())
			}
		})
	}
}

// stackCaseEnv names the case TestStackWithinCharge runs in a child
// process.
const stackCaseEnv = "RURALZ_JSONSCHEMA_STACK_CASE"

// TestStackWithinCharge checks the stack term of the working set against
// the runtime: the library's call stack at a value holds one chain of
// in-place evaluations, whatever the value's evaluations (charging every
// evaluation a frame made a 14 KB rich-text body reserve 29 MB of stack).
// For recursive bodies, unions and reference chains, validating in both
// passes on a goroutine whose stack may grow to two thirds of the path the
// scan charges never overflows: the stack, and the old one with the new
// while it doubles, stay within the charge. Each case runs in a child
// process, since a goroutine past its maximum stack aborts the program.
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
// validates with the heap at its peak within the reservation.
func TestHandleBoundsUnevaluatedMemory(t *testing.T) {
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

	f = buildFilter(t, unevaluatedChain(100, "unevaluatedProperties"), 64<<20)
	x, _ = exchange("application/json", []byte(body))
	px := &peakExchange{Exchange: x}
	peak := peakHeap(func() any { return f.Handle(t.Context(), phase.OnRequestBody, px) })
	if px.peak == 0 {
		t.Fatal("nothing reserved")
	}
	t.Logf("reserved %d (decoded values included), peak %.0f", px.peak, peak)
	if peak > float64(px.peak+gcSlack) {
		t.Errorf("Handle held %.0f bytes at its peak, reserved %d", peak, px.peak)
	}
	if _, scan := scanBody(t, f.schema, body); scan.path < 100*2000*propMapEntry {
		t.Errorf("the path charges %d bytes, less than the 100 copies of 2,000 names", scan.path)
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

// TestHandleUnevaluatedBranches covers the unevaluated-tracking maps under
// branches, which are not charged once per evaluation that may copy them:
// the library drops a branch's copy when it merges it into its caller's,
// before the next branch of an allOf, anyOf or oneOf runs, so only the
// copies of one in-place chain are live at once, as for the call stack.
// Through Factory.Build and Handle at the 10 MiB default, a valid 49 KB
// object under an allOf of 50 three-link chains with unevaluatedProperties
// (charged 129 MB, 84 times its peak, and 413) and a valid array under the
// same with unevaluatedItems continue; with a 64 MiB limit the heap at
// Handle's peak stays within the reservation.
func TestHandleUnevaluatedBranches(t *testing.T) {
	for _, tc := range []struct{ kw, body string }{
		{"unevaluatedProperties", wideObject(5000)},
		{"unevaluatedItems", denseArray(8000, `1`, 0)},
	} {
		t.Run(tc.kw, func(t *testing.T) {
			schema := unevaluatedBranches(50, tc.kw)
			if r := handle(t, buildFilter(t, schema, 0), tc.body); r.Outcome != filter.Continue {
				t.Fatalf("Handle(%d bytes) at the default limit = %+v, want Continue", len(tc.body), r)
			}
			f := buildFilter(t, schema, 64<<20)
			x, _ := exchange("application/json", []byte(tc.body))
			px := &peakExchange{Exchange: x}
			var r filter.Result
			peak := peakHeap(func() any { r = f.Handle(t.Context(), phase.OnRequestBody, px); return r })
			if r.Outcome != filter.Continue {
				t.Fatalf("Handle at 64 MiB = %+v, want Continue", r)
			}
			t.Logf("reserved %d (decoded values included), peak %.0f", px.peak, peak)
			if peak > float64(px.peak+gcSlack) {
				t.Errorf("Handle held %.0f bytes at its peak, reserved %d", peak, px.peak)
			}
		})
	}
}

// TestHandleBoundsMatchedItems covers contains: every evaluation with
// contains lists the indexes of the items that match it, and the location
// pass keeps the list in a failing minContains or maxContains error, which
// the scan charges: uncharged, along a 100-link chain, 16,000 matching
// items held a 12.7 MB error tree charged 58 KB, the heap at its peak 10
// times the reservation. Through Factory.Build and Handle at the 10 MiB
// default, the failing body is located with the heap at its peak within
// the reservation.
func TestHandleBoundsMatchedItems(t *testing.T) {
	n := 16000
	if raceEnabled {
		n /= 4
	}
	schema := chain(100, `{"$ref":"#/$defs/d%d","contains":true,"maxContains":0,"x-n":%d}`)
	body := denseArray(n, `1`, 0)
	f := buildFilter(t, schema, 0)
	x, _ := exchange("application/json", []byte(body))
	px := &peakExchange{Exchange: x}
	var r filter.Result
	peak := peakHeap(func() any { r = f.Handle(t.Context(), phase.OnRequestBody, px); return r })
	if got := detailOf(t, r); got != DetailMismatch+" at /$defs/d0/maxContains" {
		t.Fatalf("detail = %q, want the location of maxContains", got)
	}
	t.Logf("reserved %d (decoded values included), peak %.0f", px.peak, peak)
	if peak > float64(px.peak+gcSlack) {
		t.Errorf("Handle held %.0f bytes at its peak, reserved %d", peak, px.peak)
	}
	if _, scan := scanBody(t, f.schema, body); scan.locate < int64(100*n*8) {
		t.Errorf("the location pass is charged %d bytes, less than the 100 lists of %d indexes", scan.locate, n)
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
// more than Handle reserved, and a larger one is refused as oversized
// before the library runs, allocating no more than its decode.
func TestHandleBoundsValidationMemory(t *testing.T) {
	schema := `{"type":"array","items":` + oneOfConsts(8) + `}`
	f := buildFilter(t, schema, 4<<20)
	body := []byte(denseArray(5000, `"x"`, 0))
	x, _ := exchange("application/json", body)
	px := &peakExchange{Exchange: x}
	var r filter.Result
	peak := peakHeap(func() any { r = f.Handle(t.Context(), phase.OnRequestBody, px); return r })
	if got := detailOf(t, r); got != DetailMismatch+" at /items/oneOf/0/const" {
		t.Fatalf("detail = %q", got)
	}
	t.Logf("peak %.0f, reserved %d (%.2f)", peak, px.peak, peak/float64(px.peak))
	if px.peak == 0 || peak > float64(px.peak+gcSlack) {
		t.Errorf("Handle held %.0f bytes at its peak, reserved %d", peak, px.peak)
	}
	if x.Budget.Reserved != 0 {
		t.Errorf("reservation left: %d", x.Budget.Reserved)
	}

	// 1,000,000 items at the 10 MiB default: the decode fits (33 MiB of
	// the 40 MiB budget), validation could hold gigabytes.
	var before, after runtime.MemStats
	big := []byte(denseArray(1_000_000, `"x"`, 0))
	f = buildFilter(t, schema, 0)
	allocs := func(run func()) float64 {
		runtime.GC()
		runtime.ReadMemStats(&before)
		run()
		runtime.ReadMemStats(&after)
		return float64(after.TotalAlloc - before.TotalAlloc)
	}
	decoded := allocs(func() { _, _, _ = f.decode(big) })
	x, _ = exchange("application/json", big)
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
}

// TestWorkingSetCharges pins the working-set model (proposed) on small
// bodies. The decision pass is charged decideErrorBytes per error unit of
// the value's kind and per group error, an object one member's errors per
// evaluation with member keywords (a valid body is not charged a
// failing body's location-bearing tree); the location pass errorBytes plus
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
		wrapper  = decideFrames * stackBytes
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
		{"type list", `{"type":["null","array","object","boolean","integer","number"]}`, `"x"`, e, 2 * at0, stackBytes + wrapper, 1},
		{"required list", `{"required":["a","b","c","d","e","f","g"]}`, `{}`, e, 3 * at0, stackBytes + wrapper, 0},
		{"enum comparisons hold no memory", `{"enum":[` + strings.Join(enum, ",") + `]}`, `"x"`, e, at0, stackBytes + wrapper, 1},
		{"lengths that hold", `{"minLength":1,"maxLength":3}`, `"xyz"`, 0, 0, stackBytes + wrapper, 3},
		// An applicator next to lengths that hold can still fail, with its
		// branches.
		{"lengths that hold beside an applicator", `{"maxLength":9,"anyOf":[{"type":"integer"},{"type":"null"}]}`, `"xyz"`, 3 * e, 3 * at0, 2*stackBytes + wrapper, 3},
		{"lengths in characters", `{"maxLength":1}`, `"é"`, 0, 0, stackBytes + wrapper, 2},
		{"two failing lengths and a group", `{"minLength":2,"maxLength":0}`, `"x"`, 3 * e, 2 * at0, stackBytes + wrapper, 1},
		{"a keyword of another kind", `{"minLength":1,"maximum":3}`, `""`, e, at0, stackBytes + wrapper, 0},
		{"reference", `{"$ref":"#/$defs/s","$defs":{"s":{"type":"string"}}}`, `1`, 2 * e, 2 * at0, 2*stackBytes + wrapper, 0},
		// if, then and else all count, then failing on its length and else
		// on its applicator; the applicators run only when nothing else
		// failed. The stack holds one chain of them at a time: the root,
		// else and one branch of its allOf.
		{"if, then and else", `{"if":{"type":"string"},"then":{"minLength":2},"else":{"allOf":[true,true]}}`, `"x"`, 2 * e, 2 * at0, 3*stackBytes + wrapper, 1},
		// A failing name: its errors, located at the name, and the error
		// naming it, in either pass; the object's group.
		{"property names", `{"propertyNames":{"maxLength":3}}`, `{"abcd":1}`, e + 2*at0, at1 + at0, 2*stackBytes + wrapper, 4},
		// One of the object's three evaluations applies propertyNames, so the
		// name failing both branches (three units with the group) is wrapped
		// once, in either pass; the root's allOf fails with its group.
		{
			"property names in one of three evaluations", `{"allOf":[{"propertyNames":{"allOf":[{"maxLength":3},{"maxLength":2}]}},{"minProperties":1}]}`, `{"abcd":1}`,
			3*e + (3+1)*at0, 3*at1 + at0 + 2*at0, 4*stackBytes + wrapper, 4,
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
		// items while it loops, 16 bytes each, on the path.
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

// TestPathStates covers the charge of each value's own path: with every
// value at a depth charged the costliest member class at that depth, a
// 12-way union of payment objects next to a plain array of tags made every
// tag as costly as the union, so a valid 49 KB order was 413 at the 10 MiB
// default. Each value is charged the schemas of its own path: the
// union adds the same to the scan whatever the number of tags, a tag costs
// what it costs without the union, and a valid order with 300,000 tags
// (3.4 MB) passes at the default with its reservation within two and a
// half times its measured peak.
func TestPathStates(t *testing.T) {
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
	x, _ := exchange("application/json", []byte(body))
	px := &peakExchange{Exchange: x}
	var r filter.Result
	peak := peakHeap(func() any { r = f.Handle(t.Context(), phase.OnRequestBody, px); return r })
	if r.Outcome != filter.Continue {
		t.Fatalf("Handle(%d bytes) = %+v, want Continue", len(body), r)
	}
	t.Logf("body %d bytes: reserved %d (decoded values included), peak %.0f", len(body), px.peak, peak)
	if float64(px.peak) > 2.5*peak+gcSlack {
		t.Errorf("reserved %d, more than two and a half times the peak %.0f", px.peak, peak)
	}
}

// TestMatcherReach covers the pattern charges: rather than the job stack of
// the document's costliest pattern program for every string a pattern or a
// uniqueItems pass reaches, a string is charged only the patterns its own
// path applies to it, a member name those of its object's
// patternProperties, a value of another kind none, and where the
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
