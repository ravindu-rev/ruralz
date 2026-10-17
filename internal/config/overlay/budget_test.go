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

// runBudget runs the producer's stage D input; yield, when set, is
// Options.Yield.
func runBudget(t testing.TB, p budgetProducer, base, patches []*tree.Resource, yield ...func()) diag.List {
	t.Helper()
	opts := Options{Schemas: schemaSet(t, p.versions...)}
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
// Apply or CheckBase call records the first DefaultMaxDiagnostics+1
// diagnostics in walk order, so the caller's cap appends its RZ-CFG-001,
// and stops walking, so a document with many more occurrences allocates
// no more. Every producer of a per-occurrence diagnostic is one row.
func TestDiagnosticBudget(t *testing.T) {
	const k = DefaultMaxDiagnostics + 50
	for _, p := range budgetProducers() {
		t.Run(p.name, func(t *testing.T) {
			b, o := p.build(k)
			ds := runBudget(t, p, b, o)
			if len(ds) != DefaultMaxDiagnostics+1 {
				t.Fatalf("%d diagnostics, want %d", len(ds), DefaultMaxDiagnostics+1)
			}
			for i, d := range ds {
				if d.Code != p.code {
					t.Fatalf("diagnostic %d is %s, want %s", i, d.AppendText(nil), p.code)
				}
			}
			c := diag.NewCollector(DefaultMaxDiagnostics)
			c.AddAll(ds)
			if l := c.List(); len(l) != DefaultMaxDiagnostics+1 || !c.Full() || l.FirstErrorCode() != "RZ-CFG-001" {
				t.Errorf("a collector with the same cap kept %d diagnostics (full %v, first error %s), want %d and RZ-CFG-001",
					len(l), c.Full(), l.FirstErrorCode(), DefaultMaxDiagnostics+1)
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
			if a1, a2 := allocs(k), allocs(2*k); a2 > a1+k/8 {
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

// TestSettleAfterBudget: once the diagnostic budget is spent, settle
// copies and normalizes no overlay value (01 req 50 and risk 1), even
// where one replacing a base scalar would decide the dispatch: the
// allocations do not grow with the number of such values.
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
	if a1, a2 := allocs(k), allocs(2*k); a2 > a1+k/8 {
		t.Errorf("%.0f allocations for %d values, %.0f for %d: settle runs past the budget", a1, k, a2, 2*k)
	}
}
