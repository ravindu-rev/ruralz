// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"encoding/json"
	"math/rand/v2"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// spreadShape is a stage D input of size n for TestYieldSpread: one member
// of the class of bursty Yield calls (work charged in one bulk charge
// before or after a loop over the whole input, or not charged at all).
type spreadShape struct {
	name string
	// manyDiags lifts the diagnostic cap, for shapes whose work is one
	// diagnostic per element.
	manyDiags bool
	build     func(n int) (base, patches []*tree.Resource)
}

// wide returns an object with members m0..mn-1 of value v(i), then the
// members of tail.
func wide(n int, v func(i int) *tree.Node, tail ...any) *tree.Node {
	o := repeated(n, v)
	o.Members = append(o.Members, obj(tail...).Members...)
	return o
}

// spreadShapes are the members of the bursty Yield class that scaleShapes
// does not cover, one per code path, each at n elements.
func spreadShapes() []spreadShape {
	one := func(int) *tree.Node { return iv(1) }
	policy := func(spec *tree.Node) *tree.Resource { return doc("Policy", "p", spec) }
	return []spreadShape{
		{name: "memberIndex: a wide config, one member patched", build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			return []*tree.Resource{policy(plugin(repeated(n, one)))}, []*tree.Resource{policy(obj("config", obj("m0", iv(2))))}
		}},
		{name: "selectOf and settle: a wide PolicySpec, type last, overlay type {a: null}", build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			spec := wide(n, one, "config", obj("schema", obj(PatchKey, iv(1))), "type", sv("validation.json-schema"))
			return []*tree.Resource{policy(spec)}, []*tree.Resource{policy(obj("type", obj("a", nullv())))}
		}},
		{name: "entries: a long keyed list, one entry patched", build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			return []*tree.Resource{doc("Gateway", "edge", obj("listeners", listeners(n, 0, baseListener)))},
				[]*tree.Resource{doc("Gateway", "edge", obj("listeners", arr(obj("name", sv("l0"), "port", iv(2)))))}
		}},
		{name: "normalize: set elements holding only $patch dropped", manyDiags: true, build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			return []*tree.Resource{doc("Route", "r", obj())},
				[]*tree.Resource{doc("Route", "r", obj("match", obj("methods", items(n, func(int) *tree.Node { return obj(PatchKey, iv(1)) }))))}
		}},
		{name: "wholeListEntry: a wide set element, $patch first", build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			e := obj(PatchKey, iv(1))
			e.Members = append(e.Members, repeated(n, one).Members...)
			return []*tree.Resource{doc("Route", "r", obj())}, []*tree.Resource{doc("Route", "r", obj("match", obj("methods", arr(e))))}
		}},
		{name: "redispatched: a wide held object", build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			schemaDoc := wide(n, one, PatchKey, iv(1))
			return []*tree.Resource{policy(obj("type", sv("validation.json-schema"), "config", obj("schema", schemaDoc)))},
				[]*tree.Resource{policy(obj("type", sv("cors")))}
		}},
		{name: "settle: a wide composite over the type scalar", build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			return []*tree.Resource{policy(obj("type", sv("cors"), "config", obj()))}, []*tree.Resource{policy(obj("type", repeated(n, one)))}
		}},
		{name: "exemptHolders: wide base and overlay under a changed dispatch", build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			base := wide(n, func(i int) *tree.Node { return obj("a", iv(i)) }, "schema", obj(PatchKey, iv(1)))
			over := repeated(n, func(i int) *tree.Node { return arr(iv(i)) })
			return []*tree.Resource{policy(obj("type", sv("validation.json-schema"), "config", base))},
				[]*tree.Resource{policy(obj("type", sv("cors"), "config", over))}
		}},
		{name: "takeAction: wide annotations, the patch annotation last", build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			ann := wide(n, func(int) *tree.Node { return sv("a") }, PatchAnnotation, sv(PatchReplace))
			return []*tree.Resource{doc("Route", "r", obj("timeout", sv("1s")))}, []*tree.Resource{doc("Route", "r", obj("timeout", sv("2s")), "annotations", ann)}
		}},
		{name: "Apply: a wide root, its spec replaced", build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			b := doc("Route", "r", nil)
			b.Root.Members = append(b.Root.Members, repeated(n, one).Members...)
			b.Root.Members = append(b.Root.Members, tree.Member{Key: "spec", Value: obj("timeout", sv("1s"))})
			return []*tree.Resource{b}, []*tree.Resource{doc("Route", "r", obj("timeout", sv("2s")), "annotations", obj(PatchAnnotation, sv(PatchReplace)))}
		}},
		{name: "unique and the added order: many overlay documents", build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			var patches []*tree.Resource
			for i := range n {
				patches = append(patches, doc("Route", "r"+strconv.Itoa(n-i), obj()))
			}
			return nil, patches
		}},
		{name: "Apply: many base documents deleted", build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			var base, patches []*tree.Resource
			for i := range n {
				base = append(base, doc("Route", "r"+strconv.Itoa(i), obj()))
				patches = append(patches, doc("Route", "r"+strconv.Itoa(i), nil, "annotations", obj(PatchAnnotation, sv(PatchDelete))))
			}
			return base, patches
		}},
		{name: "unique: one identity declared many times, the first with a wide root", manyDiags: true, build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			first := doc("Route", "r", nil)
			first.Root.Members = append(repeated(n, one).Members, first.Root.Members...)
			first.Start = tree.Pos{Line: 1, Column: 1}
			patches := []*tree.Resource{first}
			for i := range n {
				p := doc("Route", "r", nil)
				p.Start = tree.Pos{Line: int32(i + 2), Column: 1}
				patches = append(patches, p)
			}
			return nil, patches
		}},
		{name: "itemElem: a wide composite set element in a base document", build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			return []*tree.Resource{doc("Route", "r", obj("match", obj("methods", arr(repeated(n, one)))))}, nil
		}},
		{name: "overlay keys: a wide overlay object repeating its keys", build: func(n int) ([]*tree.Resource, []*tree.Resource) {
			over := obj()
			for i := range n {
				over.Members = append(over.Members, tree.Member{Key: "k" + strconv.Itoa(i%(n/2+1)), Value: sv("b")})
			}
			return []*tree.Resource{doc("Route", "r", obj(), "labels", obj("k0", sv("a")))}, []*tree.Resource{doc("Route", "r", nil, "labels", over)}
		}},
	}
}

// spreadRun runs stage D over base and patches with a probe and returns
// it with the Yield calls.
func spreadRun(t testing.TB, base, patches []*tree.Resource, manyDiags bool) (*probe, int) {
	t.Helper()
	calls := 0
	opts := Options{Schemas: schemas(t), Yield: func() { calls++ }}
	if manyDiags {
		opts.MaxDiagnostics = 1 << 30
	}
	p := &probe{}
	for _, r := range base {
		_ = checkBase(r, opts, p)
	}
	if _, _, err := apply(t.Context(), base, patches, opts, p); err != nil {
		t.Fatal(err)
	}
	return p, calls
}

// TestYieldSpread checks that Yield runs every 256 units spread over the
// work, not in bursts: for every scale shape and every spreadShapes class
// member at 1<<16 elements, no more than yieldEvery work units are
// charged between two Yield calls (each unit is charged as its work is
// done) and schemaidx Select never reads an object of more than a few
// members. A loop over the whole input charged in one charge before or
// after it charges thousands of units between two calls, and an uncounted
// Select reads 1<<16 members.
func TestYieldSpread(t *testing.T) {
	const n = 1 << 16
	type row struct {
		name      string
		manyDiags bool
		build     func(int) ([]*tree.Resource, []*tree.Resource)
	}
	var rows []row
	for _, sh := range scaleShapes() {
		rows = append(rows, row{"scale: " + sh.name, false, sh.build})
	}
	for _, sh := range spreadShapes() {
		rows = append(rows, row(sh))
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			base, patches := r.build(n)
			p, calls := spreadRun(t, base, patches, r.manyDiags)
			if p.last() > yieldEvery {
				t.Errorf("%d work units between two Yield calls, want at most %d", p.last(), yieldEvery)
			}
			if p.selectWidth > 2*linearScan {
				t.Errorf("schemaidx Select read an object of %d members, want at most %d", p.selectWidth, 2*linearScan)
			}
			if calls < n/yieldEvery {
				t.Errorf("%d Yield calls for %d elements, want at least %d", calls, n, n/yieldEvery)
			}
		})
	}
}

// TestSpreadShapesLinear: the spreadShapes class members cost work linear
// in their size too, among them one identity declared n times, whose
// first declaration's name position each later one relates (it was read
// once per later declaration, quadratic with a wide root).
func TestSpreadShapesLinear(t *testing.T) {
	const n = 2000
	for _, sh := range spreadShapes() {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			units := func(n int) int {
				b, o := sh.build(n)
				_, calls := spreadRun(t, b, o, sh.manyDiags)
				return calls
			}
			w1, w4 := units(n), units(4*n)
			if w1 < n/yieldEvery {
				t.Fatalf("%d Yield calls at n=%d: the shape does too little to measure", w1, n)
			}
			if w4 > 4*w1+w1/2+4 {
				t.Errorf("work grows faster than the input: %d Yield calls at n=%d, %d at n=%d", w1, n, w4, 4*n)
			}
		})
	}
}

// meterProbe returns an applier with a probe.
func meterProbe() (*applier, *probe) {
	a := newApplier(Options{})
	a.probe = &probe{}
	return a, a.probe
}

// checkSpread fails when the work was charged in a stretch of more than
// yieldEvery units, or when less than want units were charged.
func checkSpread(t *testing.T, name string, p *probe, units, want int) {
	t.Helper()
	if p.last() > yieldEvery {
		t.Errorf("%s: %d units between two Yield calls, want at most %d", name, p.last(), yieldEvery)
	}
	if units < want {
		t.Errorf("%s: %d units charged, want at least %d", name, units, want)
	}
}

// totalUnits returns the units a charged since it was made.
func totalUnits(a *applier, calls int) int { return calls*yieldEvery + a.units }

// TestMeteredHelpers checks each counted replacement of a library call
// against the call it replaces, on random inputs of many sizes, and that
// it charges one unit per element as it goes.
func TestMeteredHelpers(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: deterministic test data
	for _, n := range []int{0, 1, 2, 7, 63, 64, 65, 129, 1000} {
		ints := make([]int, n)
		for i := range ints {
			ints[i] = r.IntN(n/3 + 1)
		}
		t.Run("deleteFunc/"+strconv.Itoa(n), func(t *testing.T) {
			a, p := meterProbe()
			calls := 0
			a.opts.Yield = func() { calls++ }
			got := deleteFunc(a, slices.Clone(ints), func(v int) bool { return v%2 == 0 })
			want := slices.DeleteFunc(slices.Clone(ints), func(v int) bool { return v%2 == 0 })
			if !slices.Equal(got, want) {
				t.Errorf("deleteFunc = %v, want %v", got, want)
			}
			checkSpread(t, "deleteFunc", p, totalUnits(a, calls), n)
		})
		t.Run("deleteFunc clears the tail/"+strconv.Itoa(n), func(t *testing.T) {
			a, _ := meterProbe()
			s := make([]*int, n)
			for i := range s {
				v := i
				s[i] = &v
			}
			kept := deleteFunc(a, s, func(v *int) bool { return *v%3 != 0 })
			for i, v := range s[len(kept):] {
				if v != nil {
					t.Fatalf("element %d past the kept ones is not cleared", len(kept)+i)
				}
			}
		})
		for _, i := range []int{0, n / 2, n - 1} {
			if i < 0 || i >= n {
				continue
			}
			t.Run("deleteAt/"+strconv.Itoa(n)+"/"+strconv.Itoa(i), func(t *testing.T) {
				a, p := meterProbe()
				calls := 0
				a.opts.Yield = func() { calls++ }
				s := slices.Clone(ints)
				got := deleteAt(a, s, i)
				want := slices.Delete(slices.Clone(ints), i, i+1)
				if !slices.Equal(got, want) {
					t.Errorf("deleteAt(%d) = %v, want %v", i, got, want)
				}
				if s[n-1] != 0 {
					t.Errorf("deleteAt left %d in the freed last element", s[n-1])
				}
				checkSpread(t, "deleteAt", p, totalUnits(a, calls), (n-1-i)/moveStep)
			})
		}
		t.Run("sortStable/"+strconv.Itoa(n), func(t *testing.T) {
			type kv struct{ k, i int }
			s := make([]kv, n)
			for i := range s {
				s[i] = kv{ints[i], i}
			}
			a, p := meterProbe()
			calls := 0
			a.opts.Yield = func() { calls++ }
			got := slices.Clone(s)
			sortStable(a, got, func(x, y kv) int { return x.k - y.k })
			want := slices.Clone(s)
			slices.SortStableFunc(want, func(x, y kv) int { return x.k - y.k })
			if !slices.Equal(got, want) {
				t.Errorf("sortStable = %v, want %v", got, want)
			}
			checkSpread(t, "sortStable", p, totalUnits(a, calls), min(n, max(0, n-1)*2))
		})
		t.Run("clearSteps/"+strconv.Itoa(n), func(t *testing.T) {
			a, p := meterProbe()
			calls := 0
			a.opts.Yield = func() { calls++ }
			s := slices.Clone(ints)
			for i := range s {
				s[i]++
			}
			clearSteps(a, s)
			if slices.ContainsFunc(s, func(v int) bool { return v != 0 }) {
				t.Errorf("clearSteps left %v", s)
			}
			checkSpread(t, "clearSteps", p, totalUnits(a, calls), n/moveStep)
		})
		t.Run("get, remove and memberIndex/"+strconv.Itoa(n), func(t *testing.T) {
			o := obj()
			for i := range n {
				o.Members = append(o.Members, tree.Member{Key: "k" + strconv.Itoa(ints[i]), Value: iv(i)})
			}
			for _, key := range []string{"k0", "k1", "k" + strconv.Itoa(n/3), "absent"} {
				a, p := meterProbe()
				calls := 0
				a.opts.Yield = func() { calls++ }
				v, ok := a.get(o, key)
				w, wok := o.Get(key)
				if ok != wok || v != w {
					t.Errorf("get(%q) = %v, %v; want %v, %v", key, v, ok, w, wok)
				}
				for _, lookups := range []int{1, linearScan + 1} {
					x := a.memberIndex(o.Members, lookups)
					j, seen := x.claim([]tree.Member{{Key: key}}, 0)
					if seen || (j >= 0) != wok || (j >= 0 && o.Members[j].Value != w) {
						t.Errorf("memberIndex(%d lookups).claim(%q) = %d, %v, want the first member %v", lookups, key, j, seen, w)
					}
				}
				checkSpread(t, "get", p, totalUnits(a, calls), 0)
				c := obj()
				c.Members = slices.Clone(o.Members)
				d := obj()
				d.Members = slices.Clone(o.Members)
				if a.remove(c, key) != d.Delete(key) || !slices.Equal(c.Members, d.Members) {
					t.Errorf("remove(%q) differs from tree.Node.Delete", key)
				}
			}
		})
	}
	t.Run("claim", func(t *testing.T) {
		base := []tree.Member{{Key: "a"}, {Key: "b"}, {Key: "a"}}
		for _, n := range []int{3, linearScan, linearScan + 1, 100} {
			ov := make([]tree.Member, n)
			for i := range ov {
				ov[i].Key = []string{"a", "c", "b", "c", "a", "d"}[i%6] + strings.Repeat("x", i/6)
			}
			a, _ := meterProbe()
			x := a.memberIndex(base, n)
			for j := range ov {
				wantSeen := slices.ContainsFunc(ov[:j], func(m tree.Member) bool { return m.Key == ov[j].Key })
				wantAt := slices.IndexFunc(base, func(m tree.Member) bool { return m.Key == ov[j].Key })
				if wantSeen {
					wantAt = -1
				}
				if at, seen := x.claim(ov, j); at != wantAt || seen != wantSeen {
					t.Errorf("n=%d: claim(%d %q) = %d, %v; want %d, %v", n, j, ov[j].Key, at, seen, wantAt, wantSeen)
				}
			}
		}
	})
}

// TestClip: input text quoted in a message is cut to maxShown bytes
// before a rune, so a diagnostic's message stays short whatever the input
// and quoting a 32 MiB value is no uncounted pass.
func TestClip(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{strings.Repeat("a", maxShown), strings.Repeat("a", maxShown)},
		{strings.Repeat("a", maxShown+1), strings.Repeat("a", maxShown) + "..."},
		{strings.Repeat("a", maxShown-1) + "é", strings.Repeat("a", maxShown-1) + "..."},
		{strings.Repeat("a", maxShown-2) + "€x", strings.Repeat("a", maxShown-2) + "..."},
	} {
		if got := clip(tc.in); got != tc.want {
			t.Errorf("clip(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	long := strings.Repeat("v", 1<<20)
	for name, d := range map[string]diag.List{
		"unsupported annotation value": runMessage(t, doc("Route", "r", obj("timeout", sv(long)), "annotations", obj(PatchAnnotation, sv(long)))),
		"unsupported $patch value":     runMessage(t, doc("Gateway", "edge", obj("listeners", arr(obj("name", sv("a"), PatchKey, sv(long)))))),
		"duplicate key in a list":      runMessage(t, doc("Gateway", "edge", obj("listeners", arr(obj("name", sv(long)), obj("name", sv(long)))))),
	} {
		if len(d) != 1 {
			t.Fatalf("%s: %q, want one diagnostic", name, texts(d))
		}
		if len(d[0].Message) > 4*maxShown {
			t.Errorf("%s: a message of %d bytes, want the value clipped", name, len(d[0].Message))
		}
	}
	x := newApplier(Options{})
	for _, ds := range [][2]string{{long, "v"}, {"v", long}} {
		p := doc("Route", "r", obj())
		p.APIVersion = ds[0]
		b := doc("Route", "r", obj())
		b.APIVersion = ds[1]
		x.diags = nil
		x.mismatch(p, b)
		if _, ok := x.index(p); ok {
			t.Fatalf("apiVersion %d bytes long is served", len(ds[0]))
		}
		for _, d := range x.diags {
			if len(d.Message) > 4*maxShown {
				t.Errorf("a message of %d bytes for a long apiVersion, want it clipped", len(d.Message))
			}
		}
	}
}

// runMessage merges patch into an empty base of its identity and returns
// the diagnostics.
func runMessage(t *testing.T, patch *tree.Resource) diag.List {
	t.Helper()
	b := doc(string(patch.ID.Kind), patch.ID.Name, obj())
	_, ds := pipeline(t, []*tree.Resource{b}, []*tree.Resource{patch}, Options{Schemas: schemas(t)})
	return ds
}

// TestItemElemBounded: itemElem names a keyed entry by its key, found by
// a counted scan, and a set element by its canonical JSON when that is
// at most maxShown bytes, else by its index, as config/canonical names
// it, deciding by a walk that stops past maxShown bytes, since
// schemaidx.ItemElem encodes and sorts a whole set element, an uncounted
// pass over it.
func TestItemElemBounded(t *testing.T) {
	x, _ := schemas(t).Index(v1)
	route, _ := x.Resource("Route")
	methods := route
	for _, name := range []string{"spec", "match", "methods"} {
		methods = memberSchema(methods, name)
	}
	gw, _ := x.Resource("Gateway")
	listeners := memberSchema(memberSchema(gw, "spec"), "listeners")
	small := obj("b", sv("x"), "a", iv(1))
	long := obj("a", sv(strings.Repeat("x", maxShown)))
	for _, tc := range []struct {
		name string
		list *schemaidx.Node
		item *tree.Node
		want diag.PathElem
	}{
		{"small set element", methods, small, schemaidx.ItemElem(methods, small, 3)},
		{"set element with long text", methods, long, diag.Index(3)},
		{"wide set element", methods, repeated(1<<16, func(int) *tree.Node { return iv(1) }), diag.Index(3)},
		{"set element with many members", methods, repeated(20, func(int) *tree.Node { return iv(1) }), diag.Index(3)},
		{"set element whose JSON is longer than its text", methods, letters(12), diag.Index(3)},
		{"set element with a long list", methods, obj("a", items(40, func(int) *tree.Node { return iv(1) })), diag.Index(3)},
		{"small set element with a list", methods, obj("a", arr(iv(1), iv(2))), schemaidx.ItemElem(methods, obj("a", arr(iv(1), iv(2))), 3)},
		{"keyed entry", listeners, obj("port", iv(1), "name", sv("https")), diag.Keyed("name", "https")},
		{"keyed entry without its key", listeners, obj("port", iv(1)), diag.Index(3)},
		{"keyed entry with a composite key", listeners, obj("name", obj()), diag.Index(3)},
		{"keyed entry that is not an object", listeners, sv("x"), diag.Index(3)},
		{"element of a list without x-ruralz-list", nil, small, diag.Index(3)},
	} {
		a, p := meterProbe()
		got := a.itemElem(tc.list, tc.item, 3)
		if !samePathElem(got, tc.want) {
			t.Errorf("%s: itemElem = %v, want %v", tc.name, got, tc.want)
		}
		if a.units > 4*maxShown {
			t.Errorf("%s: %d units, want the walk to stop past maxShown bytes", tc.name, a.units)
		}
		if p.last() > yieldEvery {
			t.Errorf("%s: %d units between two Yield calls", tc.name, p.last())
		}
	}
	if e := schemaidx.ItemElem(methods, small, 3); e.Kind != diag.ElemItem {
		t.Fatalf("the small element is named %v, want by its JSON", e)
	}
	// A wide element is never encoded: schemaidx.ItemElem would copy
	// and sort its members and write all of its JSON.
	wideElem := repeated(1<<16, func(int) *tree.Node { return iv(1) })
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	a, _ := meterProbe()
	_ = a.itemElem(methods, wideElem, 3)
	runtime.ReadMemStats(&after)
	if b := after.TotalAlloc - before.TotalAlloc; b > 64<<10 {
		t.Errorf("naming a wide set element allocated %d bytes, want it not encoded", b)
	}
}

// letters returns an object of n members a, b, c, ... of value 1: short
// text, but JSON of five bytes per member.
func letters(n int) *tree.Node {
	o := obj()
	for i := range n {
		o.Members = append(o.Members, tree.Member{Key: string(rune('a' + i)), Value: iv(1)})
	}
	return o
}

// samePathElem compares two path elements by their JSON form.
func samePathElem(a, b diag.PathElem) bool {
	x, errA := json.Marshal(diag.Path{a})
	y, errB := json.Marshal(diag.Path{b})
	return errA == nil && errB == nil && string(x) == string(y)
}

// TestDuplicateOverlayKeys covers an overlay object holding one key twice:
// the first member of the key applies, as the profile keeps it (stage B),
// and no other member changes, in both orders of a removal and a
// composite, for a short object (scanned) and a long one (indexed),
// without a panic (index out of range) or another member's value
// replaced.
func TestDuplicateOverlayKeys(t *testing.T) {
	pad := func(n int) string {
		var b strings.Builder
		for i := range n {
			b.WriteString(`,"p` + strconv.Itoa(i) + `":` + strconv.Itoa(i))
		}
		return b.String()
	}
	for _, size := range []struct {
		name string
		pad  int
	}{{"short", 0}, {"long", linearScan + 2}} {
		p := pad(size.pad)
		for _, tc := range []struct {
			name, base, over, want string
		}{
			{"removal first, removed member last in the base", `{"b":2,"a":1` + p + `}`, `{"a":null,"a":{"x":1}}`, `{"b":2` + p + `}`},
			{"removal first, removed member first in the base", `{"a":1,"b":2` + p + `}`, `{"a":null,"a":{"x":1}}`, `{"b":2` + p + `}`},
			{"composite first", `{"b":2,"a":1` + p + `}`, `{"a":{"x":1},"a":null}`, `{"b":2,"a":{"x":1}` + p + `}`},
			{"composite first over a composite", `{"b":2,"a":{"y":1}` + p + `}`, `{"a":{"x":1},"a":null}`, `{"b":2,"a":{"y":1,"x":1}` + p + `}`},
			{"a new key twice", `{"b":2` + p + `}`, `{"n":{"x":1},"n":{"y":2}}`, `{"b":2` + p + `,"n":{"x":1}}`},
			{"a new key, then its removal", `{"b":2` + p + `}`, `{"n":3,"n":null}`, `{"b":2` + p + `,"n":3}`},
			{"a scalar twice", `{"b":2` + p + `}`, `{"b":3,"b":4}`, `{"b":3` + p + `}`},
		} {
			t.Run(size.name+"/"+tc.name, func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panic: %v", r)
					}
				}()
				pol := func(config string) string {
					return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"v"},"spec":{"type":"plugin","plugin":"g","config":` + config + `}}`
				}
				out, ds, _ := run(t, []file{{"b.yaml", pol(tc.base)}}, []file{{"o.yaml", pol(tc.over)}})
				if len(ds) != 0 {
					t.Fatalf("diagnostics %q", texts(ds))
				}
				config, _ := at(t, out[0].Root, "spec").Get("config")
				if got := dump(config); got != compact(t, tc.want) {
					t.Errorf("config = %s, want %s", got, compact(t, tc.want))
				}
			})
		}
	}
}
