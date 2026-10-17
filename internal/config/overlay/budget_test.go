// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"slices"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// budgetProducer builds a stage D input with k occurrences of one
// diagnostic; code is the diagnostic's code.
type budgetProducer struct {
	name, code string
	// base runs CheckBase on the base resources only.
	base bool
	// list marks a producer whose occurrences are list elements past
	// which, once the budget is spent, the walk does no work.
	list     bool
	versions []string
	build    func(k int) (base, patches []*tree.Resource)
}

// repeated returns an object with k members m0..mk-1 of value v(i).
func repeated(k int, v func(i int) *tree.Node) *tree.Node {
	n := obj()
	for i := range k {
		n.Members = append(n.Members, tree.Member{Key: "m" + strconv.Itoa(i), Value: v(i)})
	}
	return n
}

// items returns a list of k elements e(i).
func items(k int, e func(i int) *tree.Node) *tree.Node {
	l := arr()
	for i := range k {
		l.Items = append(l.Items, e(i))
	}
	return l
}

func plugin(config *tree.Node) *tree.Node {
	return obj("type", sv("plugin"), "plugin", sv("geo"), "config", config)
}

func budgetProducers() []budgetProducer {
	patch1 := func(int) *tree.Node { return obj(PatchKey, iv(1)) }
	gw := func(spec *tree.Node) []*tree.Resource { return []*tree.Resource{doc("Gateway", "edge", spec)} }
	routes := func(k int, apiVersion string, annotated bool) []*tree.Resource {
		out := make([]*tree.Resource, k)
		for i := range out {
			var md []any
			if annotated {
				md = []any{"annotations", obj(PatchAnnotation, sv("merge"))}
			}
			out[i] = doc("Route", "r"+strconv.Itoa(i), obj("timeout", sv("1s")), md...)
			out[i].APIVersion = apiVersion
			out[i].Root.Members[0].Value = sv(apiVersion)
		}
		return out
	}
	return []budgetProducer{
		{name: "CheckBase $patch in a base document", code: CodeBasePatch, base: true, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			return []*tree.Resource{doc("Policy", "p", plugin(repeated(k, patch1)))}, nil
		}},
		{name: "CheckBase $patch in base list elements", code: CodeBasePatch, base: true, list: true, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			return []*tree.Resource{doc("Policy", "p", plugin(obj("l", items(k, patch1))))}, nil
		}},
		{name: "misplaced $patch in overlay objects", code: CodeSchema, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			return []*tree.Resource{doc("Policy", "p", plugin(obj()))}, []*tree.Resource{doc("Policy", "p", plugin(repeated(k, patch1)))}
		}},
		{name: "wholeListEntry $patch in a set list", code: CodeSchema, list: true, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			e := func(int) *tree.Node { return obj(PatchKey, iv(1), "x", iv(1)) }
			return []*tree.Resource{doc("Route", "r", obj())},
				[]*tree.Resource{doc("Route", "r", obj("match", obj("methods", items(k, e))))}
		}},
		{name: "keyed entry that is not an object", code: CodeSchema, list: true, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			return gw(obj()), gw(obj("listeners", items(k, iv)))
		}},
		{name: "keyed entry with an unsupported $patch", code: CodeSchema, list: true, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			e := func(i int) *tree.Node { return obj("name", sv("l"+strconv.Itoa(i)), PatchKey, sv("merge")) }
			return gw(obj()), gw(obj("listeners", items(k, e)))
		}},
		{name: "keyed entry without its key", code: CodeSchema, list: true, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			return gw(obj()), gw(obj("listeners", items(k, func(i int) *tree.Node { return obj("port", iv(i)) })))
		}},
		{name: "two keyed entries with one key", code: CodeSchema, list: true, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			return gw(obj()), gw(obj("listeners", items(k+1, func(int) *tree.Node { return obj("name", sv("a")) })))
		}},
		{name: "delete entry with other members", code: CodeSchema, list: true, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			e := func(i int) *tree.Node {
				return obj("name", sv("l"+strconv.Itoa(i)), PatchKey, sv(PatchDelete), "port", iv(1))
			}
			return gw(obj()), gw(obj("listeners", items(k, e)))
		}},
		{name: "{$patch: replace} not first", code: CodeSchema, list: true, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			l := items(k, func(int) *tree.Node { return obj(PatchKey, sv(PatchReplace)) })
			l.Items = append([]*tree.Node{obj("name", sv("a"))}, l.Items...)
			return gw(obj()), gw(obj("policies", l))
		}},
		{name: "{$patch: replace} in a map list", code: CodeSchema, list: true, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			return gw(obj()), gw(obj("listeners", items(k, func(int) *tree.Node { return obj(PatchKey, sv(PatchReplace)) })))
		}},
		{name: "misplaced $patch in values replacing base scalars", code: CodeSchema, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			base := doc("Policy", "p", plugin(repeated(k, func(int) *tree.Node { return sv("s") })))
			return []*tree.Resource{base}, []*tree.Resource{doc("Policy", "p", obj("config", repeated(k, patch1)))}
		}},
		// Review finding: with the dispatch member replaced too, settle
		// normalized a copy of every such value before the merge reported
		// anything, past the budget.
		{name: "misplaced $patch in values replacing base scalars beside a replaced dispatch member", code: CodeSchema, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			base := repeated(k, func(int) *tree.Node { return sv("s") })
			base.Members = append([]tree.Member{{Key: "type", Value: sv("cors")}}, base.Members...)
			over := repeated(k, func(int) *tree.Node { return obj("x", obj(PatchKey, iv(1), "a", iv(1))) })
			over.Members = append([]tree.Member{{Key: "type", Value: obj("a", iv(1))}}, over.Members...)
			return []*tree.Resource{doc("Policy", "p", base)}, []*tree.Resource{doc("Policy", "p", over)}
		}},
		{name: "misplaced $patch in the value replacing a dispatch member", code: CodeSchema, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			return []*tree.Resource{doc("Policy", "p", obj("type", sv("cors")))}, []*tree.Resource{doc("Policy", "p", obj("type", repeated(k, patch1)))}
		}},
		{name: "base $patch moved out of a JSON Schema document", code: CodeBasePatch, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			base := doc("Policy", "p", obj("type", sv("validation.json-schema"), "config", obj("schema", repeated(k, patch1))))
			return []*tree.Resource{base}, []*tree.Resource{doc("Policy", "p", obj("type", sv("cors")))}
		}},
		{name: "unsupported ruralz.io/patch per document", code: CodeSchema, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			return nil, routes(k, v1, true)
		}},
		{name: "one identity in many documents", code: CodeDuplicate, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			out := make([]*tree.Resource, k+1)
			for i := range out {
				out[i] = doc("Route", "r", obj("timeout", sv("1s")))
			}
			return nil, out
		}},
		{name: "unserved apiVersion per document", code: CodeAPIVersion, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			return nil, routes(k, "ruralz/v9", false)
		}},
		{name: "unserved apiVersion per base resource", code: CodeAPIVersion, build: func(k int) ([]*tree.Resource, []*tree.Resource) {
			return routes(k, "ruralz/v9", false), nil
		}},
		{
			name: "apiVersion differing from the base per document", code: CodeVersionMismatch, versions: []string{"ruralz/v1alpha2"},
			build: func(k int) ([]*tree.Resource, []*tree.Resource) {
				return routes(k, v1, false), routes(k, "ruralz/v1alpha2", false)
			},
		},
	}
}

// raceScaled returns full, or race in a test binary built with -race
// (raceEnabled): a size the asymptotic checks still measure.
func raceScaled(full, race int) int {
	if raceEnabled {
		return race
	}
	return full
}

// testBudget is the diagnostic budget TestDiagnosticBudget sets:
// DefaultMaxDiagnostics, or a tenth of it under -race, where the budget
// check, which needs some multiple of it in occurrences, runs at that
// size (TestMaxDiagnosticsOption covers a budget other than the default).
func testBudget() int { return raceScaled(DefaultMaxDiagnostics, DefaultMaxDiagnostics/10) }

// runBudget runs the producer's stage D input with the budget testBudget
// gives; yield, when set, is Options.Yield.
func runBudget(t testing.TB, p budgetProducer, base, patches []*tree.Resource, yield ...func()) diag.List {
	t.Helper()
	opts := Options{Schemas: schemaSet(t, p.versions...), MaxDiagnostics: testBudget()}
	if len(yield) > 0 {
		opts.Yield = yield[0]
	}
	if p.base {
		var ds diag.List
		for _, r := range base {
			ds = append(ds, CheckBase(r, opts)...)
		}
		return ds
	}
	_, ds, err := Apply(t.Context(), base, patches, opts)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

// TestDiagnosticBudget covers 01 req 50 and risk 1 (review finding): one
// Apply or CheckBase call records the first MaxDiagnostics+1 diagnostics
// in walk order, so the caller's cap appends its RZ-CFG-001, and stops
// walking, so a document with many more occurrences allocates no more.
// Every producer of a per-occurrence diagnostic is one row. The budget is
// the default one, but under -race (testBudget).
func TestDiagnosticBudget(t *testing.T) {
	budget := testBudget()
	k := budget + 50
	for _, p := range budgetProducers() {
		t.Run(p.name, func(t *testing.T) {
			b, o := p.build(k)
			ds := runBudget(t, p, b, o)
			if len(ds) != budget+1 {
				t.Fatalf("%d diagnostics, want %d", len(ds), budget+1)
			}
			for i, d := range ds {
				if d.Code != p.code {
					t.Fatalf("diagnostic %d is %s, want %s", i, d.AppendText(nil), p.code)
				}
			}
			c := diag.NewCollector(budget)
			c.AddAll(ds)
			if l := c.List(); len(l) != budget+1 || !c.Full() || l.FirstErrorCode() != "RZ-CFG-001" {
				t.Errorf("a collector with the same cap kept %d diagnostics (full %v, first error %s), want %d and RZ-CFG-001",
					len(l), c.Full(), l.FirstErrorCode(), budget+1)
			}
			allocs := func(k int) float64 {
				var inputs [][2][]*tree.Resource
				for range 2 {
					b, o := p.build(k)
					inputs = append(inputs, [2][]*tree.Resource{b, o})
				}
				i := 0
				return testing.AllocsPerRun(1, func() {
					in := inputs[i]
					i++
					_ = runBudget(t, p, in[0], in[1])
				})
			}
			if a1, a2 := allocs(k), allocs(2*k); a2 > a1+float64(k/8) {
				t.Errorf("%.0f allocations for %d occurrences, %.0f for %d: the walk goes on past the budget", a1, k, a2, 2*k)
			}
			if !p.list {
				return
			}
			units := func(k int) int {
				calls := 0
				b, o := p.build(k)
				_ = runBudget(t, p, b, o, func() { calls++ })
				return calls
			}
			if w1, w2 := units(k), units(2*k); w2 > w1+2 {
				t.Errorf("%d Yield calls for %d occurrences, %d for %d: the walk goes on past the budget", w1, k, w2, 2*k)
			}
		})
	}
}

// TestReportAfterBudget: once the diagnostic beyond the budget is
// recorded, report records nothing more, whatever still calls it.
func TestReportAfterBudget(t *testing.T) {
	a := newApplier(Options{MaxDiagnostics: 1})
	for range 3 {
		a.report(diag.Diagnostic{Code: CodeSchema})
	}
	if len(a.diags) != 2 || !a.full {
		t.Errorf("%d diagnostics recorded (full %v), want 2 and full", len(a.diags), a.full)
	}
}

// TestMaxDiagnosticsOption: Options.MaxDiagnostics sets the budget and
// the first diagnostics in walk order are kept, one more than the budget
// when the input has more.
func TestMaxDiagnosticsOption(t *testing.T) {
	base := []*tree.Resource{doc("Policy", "p", plugin(repeated(5, func(int) *tree.Node { return obj(PatchKey, iv(1)) })))}
	for _, tc := range []struct {
		max  int
		want []string
	}{
		{3, []string{"config.m0", "config.m1", "config.m2", "config.m3"}},
		{5, []string{"config.m0", "config.m1", "config.m2", "config.m3", "config.m4"}},
		{0, []string{"config.m0", "config.m1", "config.m2", "config.m3", "config.m4"}},
	} {
		ds := CheckBase(base[0], Options{Schemas: schemas(t), MaxDiagnostics: tc.max})
		var got []string
		for _, d := range ds {
			got = append(got, d.Path[1:3].String())
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("MaxDiagnostics %d: paths %q, want %q", tc.max, got, tc.want)
		}
	}
}

// TestSettleMergesOnce covers the review finding on settle's trial (01
// req 54; 01 req 50 and risk 1): settle normalized a copy of the overlay
// value of every member whose base value is a scalar, members no
// dispatch rule reads included, before the merge normalized each again,
// so an overlay replacing spec.type cost about three times the
// allocations and twice the work units of the same overlay setting a new
// type. settle now merges the members the dispatch reads once, before the
// dispatch is read, and leaves the others to the merge, so both overlays
// cost the same within a few units and allocations, whatever the size of
// the member no rule reads.
func TestSettleMergesOnce(t *testing.T) {
	const k = 20_000
	measure := func(typ func() *tree.Node) (calls int, allocs float64) {
		build := func() ([]*tree.Resource, []*tree.Resource) {
			base := doc("Policy", "p", obj("type", sv("validation.json-schema"), "config", sv("x")))
			over := doc("Policy", "p", obj("type", typ(), "config", repeated(k, func(i int) *tree.Node { return obj("a", iv(i)) })))
			return []*tree.Resource{base}, []*tree.Resource{over}
		}
		b, o := build()
		if _, ds, err := Apply(t.Context(), b, o, Options{Schemas: schemas(t), Yield: func() { calls++ }}); err != nil || len(ds) != 0 {
			t.Fatalf("Apply: %v, %q", err, texts(ds))
		}
		var inputs [][2][]*tree.Resource
		for range 2 {
			b, o := build()
			inputs = append(inputs, [2][]*tree.Resource{b, o})
		}
		i := 0
		allocs = testing.AllocsPerRun(1, func() {
			in := inputs[i]
			i++
			_, _, _ = Apply(t.Context(), in[0], in[1], Options{Schemas: schemas(t)})
		})
		return calls, allocs
	}
	sc, sa := measure(func() *tree.Node { return obj("a", iv(1)) })
	pc, pa := measure(func() *tree.Node { return sv("cors") })
	t.Logf("a replaced type: %d Yield calls and %.0f allocations; a new type: %d and %.0f", sc, sa, pc, pa)
	if sc > pc+2 {
		t.Errorf("a replaced type costs %d Yield calls, a new type %d: settle repeats the merge", sc, pc)
	}
	if sa > pa+64 {
		t.Errorf("a replaced type costs %.0f allocations, a new type %.0f: settle repeats the merge", sa, pa)
	}
}

// TestSettleReadsDispatchTwice covers the cost of settle when the
// dispatch reads none of the scalars the overlay replaces with composites
// (01 req 54): settle then reads the dispatch twice, with the base
// scalars and with every overlay value standing in, and probes no member
// on its own, so an overlay replacing every declared PolicySpec member
// but the type costs what it costs without settle plus two dispatch reads
// of the object, whatever number of declared members it replaces.
func TestSettleReadsDispatchTwice(t *testing.T) {
	const k = 20_000
	declared := []string{"config", "failureMode", "filterClass", "overridable", "plugin", "slot", "stateStoreTimeout", "when"}
	work := func(replace bool) int {
		base, over := obj("type", sv("cors")), obj()
		for _, d := range declared {
			base.Members = append(base.Members, tree.Member{Key: d, Value: sv("s")})
			if replace {
				over.Members = append(over.Members, tree.Member{Key: d, Value: obj("a", iv(1))})
			}
		}
		for i := range k {
			base.Members = append(base.Members, tree.Member{Key: "m" + strconv.Itoa(i), Value: sv("s")})
			over.Members = append(over.Members, tree.Member{Key: "m" + strconv.Itoa(i), Value: sv("t")})
		}
		calls := 0
		_, ds, err := Apply(t.Context(), []*tree.Resource{doc("Policy", "p", base)}, []*tree.Resource{doc("Policy", "p", over)},
			Options{Schemas: schemas(t), Yield: func() { calls++ }})
		if err != nil || len(ds) != 0 {
			t.Fatalf("Apply: %v, %q", err, texts(ds))
		}
		return calls
	}
	with, without := work(true), work(false)
	t.Logf("%d Yield calls with the declared members replaced, %d without", with, without)
	// One dispatch read of the object passes over its k members at most
	// three times (selectOf), so two reads cost at most 6k units.
	if limit := without + 6*k/yieldEvery + 4; with > limit {
		t.Errorf("%d Yield calls with the declared members replaced, %d without, want at most %d: settle probes members the dispatch does not read",
			with, without, limit)
	}
}

// TestSettleAfterBudget: once the diagnostic budget is spent, settle
// merges no overlay value (01 req 50 and risk 1), even where one
// replacing a base scalar would decide the dispatch: the allocations do
// not grow with the number of such values.
func TestSettleAfterBudget(t *testing.T) {
	build := func(k int) ([]*tree.Resource, []*tree.Resource) {
		spec := repeated(k, func(int) *tree.Node { return sv("s") })
		spec.Members = append(spec.Members, tree.Member{Key: "type", Value: sv("validation.json-schema")})
		over := repeated(k, func(int) *tree.Node { return obj("a", iv(1)) })
		over.Members = append(over.Members, tree.Member{Key: "type", Value: obj("a", iv(1))})
		// A misplaced directive in the overlay root and one in spec, both
		// reported before spec's dispatch is read, spend a budget of one.
		over.Members = append([]tree.Member{{Key: PatchKey, Value: iv(1)}}, over.Members...)
		o := doc("Policy", "p", over)
		o.Root.Members = append(o.Root.Members, tree.Member{Key: PatchKey, Value: iv(1)})
		return []*tree.Resource{doc("Policy", "p", spec)}, []*tree.Resource{o}
	}
	opts := Options{Schemas: schemas(t), MaxDiagnostics: 1}
	allocs := func(k int) float64 {
		var inputs [][2][]*tree.Resource
		for range 2 {
			b, o := build(k)
			inputs = append(inputs, [2][]*tree.Resource{b, o})
		}
		i := 0
		return testing.AllocsPerRun(1, func() {
			in := inputs[i]
			i++
			if _, ds, err := Apply(t.Context(), in[0], in[1], opts); err != nil || len(ds) != 2 {
				t.Fatalf("Apply: %v, %d diagnostics, want the 2 that spend the budget", err, len(ds))
			}
		})
	}
	const k = 2000
	if a1, a2 := allocs(k), allocs(2*k); a2 > a1+float64(k/8) {
		t.Errorf("%.0f allocations for %d values, %.0f for %d: settle runs past the budget", a1, k, a2, 2*k)
	}
}
