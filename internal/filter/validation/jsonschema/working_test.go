// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// workingSetCases are dense bodies, each against a schema that fails at
// every value in a different way, or (the unevaluated shapes) holds the
// library's copies of member names or item indexes along a reference
// chain.
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
		{"unevaluated names along a chain", unevaluatedChain(100, "unevaluatedProperties"), wideObject(2000)},
		{"unevaluated indexes along a chain", strings.Replace(unevaluatedChain(100, "unevaluatedItems"), "minProperties", "minItems", 1), denseArray(40000, `1`, 0)},
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
	scan := &bodyScan{ws: &s.work, numbers: s.numbers, memLimit: math.MaxInt64 - 1, workLimit: maxWorkLimit}
	scan.scan(v)
	if scan.bad {
		t.Fatalf("body has a number out of range")
	}
	return v, scan
}

// gcSlack is the garbage the collector may leave in use between two
// collections while peakHeap runs it often (GOGC 10, with a heap minimum
// of 0.4 MiB), which a sample of the heap in use cannot tell from live
// memory.
const gcSlack = 1 << 20

// TestWorkingSetCoversPeak covers the working-set bound (proposed, 07
// reqs 80 and 82, section 9 item 21, architecture R-63): for dense bodies
// of every error shape, and for the unevaluated-tracking copies the
// library makes along a reference chain (review finding: copies of every
// member name at every link, live together), the heap in use at its peak
// while the library validates stays within what Handle reserves besides
// the decoded values, give or take the collector's slack.
func TestWorkingSetCoversPeak(t *testing.T) {
	for _, tc := range workingSetCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := mustCompile(t, tc.schema)
			v, scan := scanBody(t, s, tc.body)
			reserved := scan.memory()
			peak := peakHeap(func() any { return s.root.Validate(v) })
			t.Logf("reserved %d, peak %.0f (%.2f)", reserved, peak, peak/float64(reserved))
			if peak > float64(reserved+gcSlack) {
				t.Errorf("validation held %.0f bytes at its peak, the reservation is %d", peak, reserved)
			}
		})
	}
}

// TestHandleBoundsUnevaluatedMemory is the regression test of the review
// finding on the unevaluated-tracking maps: under unevaluatedProperties
// the library copies every member name at every in-place evaluation of a
// reference chain, and all the copies are live at once. Through
// Factory.Build and Handle at the 10 MiB default, a 26 KB object against a
// 1,000-link chain (about 140 MB of copies) is oversized before the
// library runs; with a 64 MiB limit, the 100-link chain validates with the
// heap at its peak within the reservation.
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
	v := decodeInstance(t, body)
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
	runtime.KeepAlive(v)
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
// working-set bound: a dense failing array against an 8-way oneOf
// allocates within a small multiple of what Handle reserved, and a larger
// one is refused as oversized before the library runs, allocating no more
// than its decode.
func TestHandleBoundsValidationMemory(t *testing.T) {
	schema := `{"type":"array","items":` + oneOfConsts(8) + `}`
	f := buildFilter(t, schema, 4<<20)
	body := []byte(denseArray(5000, `"x"`, 0))
	x, _ := exchange("application/json", body)
	px := &peakExchange{Exchange: x}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := f.Handle(t.Context(), phase.OnRequestBody, px)
	runtime.ReadMemStats(&after)
	if got := detailOf(t, r); got != DetailMismatch+" at /items/oneOf/0/const" {
		t.Fatalf("detail = %q", got)
	}
	allocated := float64(after.TotalAlloc - before.TotalAlloc)
	t.Logf("allocated %.0f, reserved %d (%.2f)", allocated, px.peak, allocated/float64(px.peak))
	if px.peak == 0 || allocated > 3*float64(px.peak) {
		t.Errorf("Handle allocated %.0f bytes, reserved %d: want at most 3 times the reservation", allocated, px.peak)
	}
	if x.Budget.Reserved != 0 {
		t.Errorf("reservation left: %d", x.Budget.Reserved)
	}

	// 1,000,000 items at the 10 MiB default: the decode fits (33 MiB of
	// the 40 MiB budget), validation could hold gigabytes.
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
// bodies: every value is charged its depth's error units at errorBytes
// plus locationBytes per level, the path the stack at stackBytes per
// evaluation and the unevaluated copies, and the largest string copy;
// work terms never add to it.
func TestWorkingSetCharges(t *testing.T) {
	enum := make([]string, 40)
	for i := range enum {
		enum[i] = fmt.Sprintf(`"v%d"`, i)
	}
	const at0, at1, at2 = errorBytes, errorBytes + locationBytes, errorBytes + 2*locationBytes
	for _, tc := range []struct {
		name, schema, body   string
		mem, path, transient int64
	}{
		{"no subschema below the root", `{"type":"array"}`, `[1,2]`, at0, stackBytes, 0},
		{"true", `true`, `{"a":[1]}`, at0, stackBytes, 0},
		{"items", `{"items":{"type":"string"}}`, `["a","bc"]`, at0 + 2*at1, 2 * stackBytes, 2},
		{"type list", `{"type":["string","null","array","object","boolean","integer","number"]}`, `"x"`, 2 * at0, stackBytes, 1},
		{"required list", `{"required":["a","b","c","d","e","f","g"]}`, `{}`, 3 * at0, stackBytes, 0},
		{"enum comparisons hold no memory", `{"enum":[` + strings.Join(enum, ",") + `]}`, `"x"`, at0, stackBytes, 1},
		{"assertions", `{"minLength":1,"maxLength":3}`, `"x"`, 2 * at0, stackBytes, 1},
		{"reference", `{"$ref":"#/$defs/s","$defs":{"s":{"type":"string"}}}`, `"x"`, 2 * at0, 2 * stackBytes, 1},
		{"if and the costlier of then and else", `{"if":{"type":"string"},"then":{"minLength":2},"else":{"allOf":[true,true]}}`, `"x"`, (1 + 1 + 3) * at0, 5 * stackBytes, 1},
		{"property names", `{"propertyNames":{"maxLength":3}}`, `{"ab":1}`, at0 + 2*at1, 2 * stackBytes, 2},
		{"additional members", `{"additionalProperties":false}`, `{"a":1,"b":2}`, at0 + 2*at1, 2 * stackBytes, 0},
		{"unevaluated names", `{"unevaluatedProperties":true}`, `{"a":1,"b":2}`, at0 + 2*at1, 2*stackBytes + propMapBase + 2*propMapEntry, 0},
		{"unevaluated indexes", `{"unevaluatedItems":true}`, `[1,2]`, at0 + 2*at1, 2*stackBytes + itemMapBase + 2*itemMapEntry, 0},
		{"unique items", `{"uniqueItems":true}`, `[{"a":1},{"b":2}]`, at0, 3*stackBytes + hashNameBytes, 0},
		{"rational", `{"items":{"minimum":0}}`, `[1e-10]`, at0 + at1 + ratBytes + 8, 2 * stackBytes, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, scan := scanBody(t, mustCompile(t, tc.schema), tc.body)
			if scan.mem != tc.mem || scan.path != tc.path || scan.transient != tc.transient {
				t.Errorf("errors, path, transient = %d, %d, %d, want %d, %d, %d", scan.mem, scan.path, scan.transient, tc.mem, tc.path, tc.transient)
			}
		})
	}
	// The largest copy is a string the library copies, or a matcher.
	_, scan := scanBody(t, mustCompile(t, `{"items":{"pattern":"^a"}}`), `["`+strings.Repeat("a", 64)+`"]`)
	if want := int64(matcherBytes + matcherInstBytes*4); scan.transient != want {
		t.Errorf("transient = %d, want the matcher's %d", scan.transient, want)
	}
	_, scan = scanBody(t, mustCompile(t, `{"uniqueItems":true}`), denseArray(21, `1`, 0))
	if want := int64(21 * dupItemBytes); scan.transient != want {
		t.Errorf("transient = %d, want the duplicates map's %d", scan.transient, want)
	}
}

// TestScanAllocatesNothing checks that the walk before validation
// allocates nothing, member names under propertyNames included.
func TestScanAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not compared under -race")
	}
	s := mustCompile(t, `{"propertyNames":{"pattern":"^k"},"uniqueItems":true,"items":{"unevaluatedProperties":false,"minimum":0}}`)
	v := decodeInstance(t, `[{"k1":[1,2.5,"x"],"k2":{"k3":null}},{"k4":1e10}]`)
	allocs := testing.AllocsPerRun(100, func() {
		scan := bodyScan{ws: &s.work, numbers: s.numbers, memLimit: math.MaxInt64 - 1, workLimit: maxWorkLimit}
		scan.scan(v)
	})
	if allocs != 0 {
		t.Errorf("the scan allocates %.0f times", allocs)
	}
}

// TestWorkingSetFailsClosed covers the fallback of the working-set bound:
// without an analysis every body is oversized; the sums saturate.
func TestWorkingSetFailsClosed(t *testing.T) {
	s := mustCompile(t, `true`)
	s.work = newWorkingSet(nil)
	r := handle(t, newFilter(s, 400, testLimit, 0), `{}`)
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
