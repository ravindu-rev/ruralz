// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

func sv(text string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: text} }

func iv(i int) *tree.Node { return &tree.Node{Kind: tree.KindInt, Text: strconv.Itoa(i)} }

func nullv() *tree.Node { return &tree.Node{Kind: tree.KindNull} }

// obj returns an object of alternating keys and values.
func obj(kv ...any) *tree.Node {
	n := &tree.Node{Kind: tree.KindMap}
	for i := 0; i+1 < len(kv); i += 2 {
		n.Members = append(n.Members, tree.Member{Key: kv[i].(string), Value: kv[i+1].(*tree.Node)})
	}
	return n
}

func arr(items ...*tree.Node) *tree.Node { return &tree.Node{Kind: tree.KindList, Items: items} }

// doc returns a resource of kind and name holding spec (and metadata
// members md, when given).
func doc(kind, name string, spec *tree.Node, md ...any) *tree.Resource {
	meta := obj(append([]any{"name", sv(name)}, md...)...)
	root := obj("apiVersion", sv(v1), "kind", sv(kind), "metadata", meta)
	if spec != nil {
		root.Members = append(root.Members, tree.Member{Key: "spec", Value: spec})
	}
	return &tree.Resource{ID: tree.ID{Kind: v1alpha1.Kind(kind), Name: name}, APIVersion: v1, Root: root}
}

// scaleShape builds a stage D input of size n: one or more base
// resources and overlay documents.
type scaleShape struct {
	name  string
	build func(n int) (base, patches []*tree.Resource)
}

// listenerName is a listener name of width bytes or more.
func listenerName(i, width int) string {
	s := "l" + strconv.Itoa(i)
	if len(s) < width {
		s += strings.Repeat("x", width-len(s))
	}
	return s
}

func listeners(n, width int, entry func(name string) *tree.Node) *tree.Node {
	l := arr()
	for i := range n {
		l.Items = append(l.Items, entry(listenerName(i, width)))
	}
	return l
}

func baseListener(name string) *tree.Node {
	return obj("name", sv(name), "protocol", sv("http"), "port", iv(80))
}

// scaleShapes are the hostile merge shapes: each costs work quadratic in n
// when a lookup scans a whole object or list per overlay entry or member.
func scaleShapes() []scaleShape {
	gw := func(spec *tree.Node) *tree.Resource { return doc("Gateway", "edge", spec) }
	return []scaleShape{
		{"map list patched by key", func(n int) ([]*tree.Resource, []*tree.Resource) {
			return []*tree.Resource{gw(obj("listeners", listeners(n, 0, baseListener)))},
				[]*tree.Resource{gw(obj("listeners", listeners(n, 0, func(s string) *tree.Node { return obj("name", sv(s), "port", iv(2)) })))}
		}},
		{"map list entries deleted", func(n int) ([]*tree.Resource, []*tree.Resource) {
			return []*tree.Resource{gw(obj("listeners", listeners(n, 0, baseListener)))},
				[]*tree.Resource{gw(obj("listeners", listeners(n, 0, func(s string) *tree.Node { return obj("name", sv(s), PatchKey, sv(PatchDelete)) })))}
		}},
		{"map list with long keys", func(n int) ([]*tree.Resource, []*tree.Resource) {
			return []*tree.Resource{gw(obj("listeners", listeners(n, 40, baseListener)))},
				[]*tree.Resource{gw(obj("listeners", listeners(n, 40, func(s string) *tree.Node { return obj("name", sv(s), "port", iv(2)) })))}
		}},
		{"absent entries deleted from a short list", func(n int) ([]*tree.Resource, []*tree.Resource) {
			over := listeners(n, 0, func(s string) *tree.Node { return obj("name", sv("d"+s), PatchKey, sv(PatchDelete)) })
			return []*tree.Resource{gw(obj("listeners", listeners(2, 0, baseListener)))}, []*tree.Resource{gw(obj("listeners", over))}
		}},
		{"absent entries deleted from a short list of wide entries", func(n int) ([]*tree.Resource, []*tree.Resource) {
			wide := listeners(4, 0, func(s string) *tree.Node {
				e := obj()
				for i := range n / 4 {
					e.Members = append(e.Members, tree.Member{Key: "x" + strconv.Itoa(i), Value: iv(i)})
				}
				e.Members = append(e.Members, tree.Member{Key: "name", Value: sv(s)})
				return e
			})
			over := listeners(n, 0, func(s string) *tree.Node { return obj("name", sv("d"+s), PatchKey, sv(PatchDelete)) })
			return []*tree.Resource{gw(obj("listeners", wide))}, []*tree.Resource{gw(obj("listeners", over))}
		}},
		{"orderedMap list merged and appended", func(n int) ([]*tree.Resource, []*tree.Resource) {
			named := func(s string) *tree.Node { return obj("name", sv(s)) }
			over := listeners(n, 0, named)
			for i := range n {
				over.Items = append(over.Items, named("new"+strconv.Itoa(i)))
			}
			return []*tree.Resource{gw(obj("policies", listeners(n, 0, named)))}, []*tree.Resource{gw(obj("policies", over))}
		}},
		{"labels merged, added and removed", func(n int) ([]*tree.Resource, []*tree.Resource) {
			base, over := obj(), obj()
			for i := range n {
				k := "k" + strconv.Itoa(i)
				base.Members = append(base.Members, tree.Member{Key: k, Value: sv("a")})
				switch i % 3 {
				case 0:
					over.Members = append(over.Members, tree.Member{Key: k, Value: nullv()})
				case 1:
					over.Members = append(over.Members, tree.Member{Key: k, Value: sv("b")})
				default:
					over.Members = append(over.Members, tree.Member{Key: "n" + k, Value: sv("c")})
				}
			}
			return []*tree.Resource{doc("Route", "r", obj("timeout", sv("1s")), "labels", base)},
				[]*tree.Resource{doc("Route", "r", obj("timeout", sv("2s")), "labels", over)}
		}},
		{"added resource with a long keyed list", func(n int) ([]*tree.Resource, []*tree.Resource) {
			return nil, []*tree.Resource{gw(obj("listeners", listeners(n, 0, baseListener)))}
		}},
		{"added resource with a wide object", func(n int) ([]*tree.Resource, []*tree.Resource) {
			spec := obj()
			for i := range n {
				spec.Members = append(spec.Members, tree.Member{Key: "m" + strconv.Itoa(i), Value: obj()})
			}
			return nil, []*tree.Resource{doc("Route", "r", spec)}
		}},
		{"redispatched Policy with a wide config", func(n int) ([]*tree.Resource, []*tree.Resource) {
			config := obj("schema", obj(PatchKey, iv(1)))
			for i := range n {
				config.Members = append(config.Members, tree.Member{Key: "x" + strconv.Itoa(i), Value: obj("a", iv(i))})
			}
			over := obj()
			for i := range n {
				over.Members = append(over.Members, tree.Member{Key: "x" + strconv.Itoa(i), Value: obj("b", iv(i))})
			}
			return []*tree.Resource{doc("Policy", "p", obj("type", sv("validation.json-schema"), "config", config))},
				[]*tree.Resource{doc("Policy", "p", obj("type", sv("cors"), "config", over))}
		}},
	}
}

// work returns the Yield calls of one stage D run over shape at size n
// (CheckBase on every base resource, then Apply): Yield runs every 256
// work units, so the count measures the work without a clock.
func work(t testing.TB, sh scaleShape, n int) int {
	t.Helper()
	calls := 0
	base, patches := sh.build(n)
	opts := Options{Schemas: schemas(t), Yield: func() { calls++ }}
	for _, r := range base {
		_ = CheckBase(r, opts)
	}
	if _, _, err := Apply(t.Context(), base, patches, opts); err != nil {
		t.Fatal(err)
	}
	return calls
}

// TestScaleLinear checks that no merge is quadratic: for every shape,
// four times the input costs at most about four times the work units. A
// lookup scanning a whole object or keyed list per overlay member or
// entry costs sixteen times. The units count every member, entry and key
// a lookup reads, so the bound holds whatever the machine.
func TestScaleLinear(t *testing.T) {
	const n = 2000
	for _, sh := range scaleShapes() {
		t.Run(sh.name, func(t *testing.T) {
			w1, w4 := work(t, sh, n), work(t, sh, 4*n)
			if w1 < n/yieldEvery {
				t.Fatalf("%d work units at n=%d: the shape does too little to measure", w1*yieldEvery, n)
			}
			t.Logf("%d Yield calls at n=%d, %d at n=%d (ratio %.2f)", w1, n, w4, 4*n, float64(w4)/float64(w1))
			if w4 > 4*w1+w1/2+4 {
				t.Errorf("work grows faster than the input: %d Yield calls at n=%d, %d at n=%d (ratio %.1f, want about 4)",
					w1, n, w4, 4*n, float64(w4)/float64(w1))
			}
		})
	}
}

// TestScaleAllocations: the keyed-list lookups allocate nothing per
// comparison, so allocations grow linearly with the list; matching by
// "s"+key concatenation would allocate once per comparison for keys of 32
// bytes or more, about n²/2 allocations.
func TestScaleAllocations(t *testing.T) {
	sh := scaleShapes()[2]
	allocs := func(n int) float64 {
		var inputs [][2][]*tree.Resource
		for range 2 {
			b, p := sh.build(n)
			inputs = append(inputs, [2][]*tree.Resource{b, p})
		}
		opts := Options{Schemas: schemas(t)}
		i := 0
		return testing.AllocsPerRun(1, func() {
			in := inputs[i]
			i++
			if _, _, err := Apply(t.Context(), in[0], in[1], opts); err != nil {
				t.Fatal(err)
			}
		})
	}
	a1, a4 := allocs(500), allocs(2000)
	t.Logf("%.0f allocations at n=500, %.0f at n=2000 (ratio %.2f)", a1, a4, a4/a1)
	if a4 > 4.5*a1+100 {
		t.Errorf("allocations grow faster than the list: %.0f at n=500, %.0f at n=2000 (ratio %.1f)", a1, a4, a4/a1)
	}
}

// TestYield: Apply and CheckBase call Options.Yield every 256 work units
// inside one resource, and Apply also between resources, so the loader's
// worker can yield within a large resource; a nil Yield is never called.
func TestYield(t *testing.T) {
	const n = 1024
	base, patches := scaleShapes()[0].build(n)
	calls := 0
	opts := Options{Schemas: schemas(t), Yield: func() { calls++ }}
	_ = CheckBase(base[0], opts)
	// CheckBase visits every listener and its three members.
	if calls < 4*n/yieldEvery {
		t.Errorf("CheckBase on %d listeners called Yield %d times, want at least %d", n, calls, 4*n/yieldEvery)
	}
	calls = 0
	if _, _, err := Apply(t.Context(), base, patches, opts); err != nil {
		t.Fatal(err)
	}
	if calls < 2*n/yieldEvery {
		t.Errorf("Apply merging %d listeners called Yield %d times, want at least %d", n, calls, 2*n/yieldEvery)
	}
	calls = 0
	many := make([]*tree.Resource, 300)
	for i := range many {
		many[i] = doc("Route", "r"+strconv.Itoa(i), obj())
	}
	if _, _, err := Apply(t.Context(), many, nil, opts); err != nil {
		t.Fatal(err)
	}
	if calls < len(many) {
		t.Errorf("Apply over %d small resources called Yield %d times, want one between each", len(many), calls)
	}
	base, patches = scaleShapes()[0].build(n)
	if _, _, err := Apply(t.Context(), base, patches, Options{Schemas: schemas(t)}); err != nil {
		t.Fatal(err)
	}
}

// BenchmarkMergeShapes merges each scaleShapes input at 10,000 entries
// (stage D share of the validation budget on hostile shapes).
func BenchmarkMergeShapes(b *testing.B) {
	for _, sh := range scaleShapes() {
		b.Run(sh.name, func(b *testing.B) {
			opts := Options{Schemas: schemas(b)}
			b.ReportAllocs()
			for range b.N {
				b.StopTimer()
				base, patches := sh.build(10_000)
				b.StartTimer()
				for _, r := range base {
					_ = CheckBase(r, opts)
				}
				if _, _, err := Apply(b.Context(), base, patches, opts); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
