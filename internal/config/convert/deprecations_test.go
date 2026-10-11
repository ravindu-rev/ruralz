// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package convert

import (
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// schemaPathOracle lists "<key-aware path> <schema path>" for every
// position of res below the root that schemaidx.Index.SchemaPath resolves,
// resolving each path from the root: the definition the incremental
// derivation of Deprecations must agree with.
func schemaPathOracle(t *testing.T, idx *schemaidx.Index, res *tree.Resource) []string {
	t.Helper()
	var out []string
	kind := string(res.ID.Kind)
	err := idx.Walk(kind, res.Root, func(c *schemaidx.Cursor) error {
		if c.Depth() == 0 {
			return nil
		}
		if sp, ok := idx.SchemaPath(kind, res.Root, c.Path()); ok {
			out = append(out, c.Path().String()+" "+sp)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// deprecatedEverywhere runs Deprecations with every schema path the oracle
// found deprecated and returns "<path> <schema path>" per warning.
func deprecatedEverywhere(t *testing.T, idx *schemaidx.Index, res *tree.Resource, oracle []string) []string {
	t.Helper()
	fields := map[string]Notice{}
	for _, o := range oracle {
		_, sp, _ := strings.Cut(o, " ")
		fields[sp] = Notice{Removal: "9.9.9"}
	}
	r, err := NewRegistry(Lifecycle{Fields: map[string]map[string]Notice{res.APIVersion: fields}}, V1alpha1())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range r.Deprecations(idx, res, nil) {
		sp := strings.TrimSuffix(strings.TrimPrefix(d.Message, "field "), " is deprecated and is removed in 9.9.9")
		got = append(got, d.Path.String()+" "+sp)
	}
	return got
}

// TestDeprecationsSchemaPaths covers 02 req 76 and the incremental schema
// path of Deprecations: with every field deprecated, it warns at exactly
// the positions schemaidx.Index.SchemaPath resolves, with the same schema
// path, for a generated document of every field of the rendered schema
// and for documents with free content, typed maps, keyed and set lists and
// members the schema does not allow.
func TestDeprecationsSchemaPaths(t *testing.T) {
	idx := hubIndex(t)
	var docs []*tree.Resource
	for f := range idx.FieldsSeq() {
		doc, ok := fieldDoc(f)
		if !ok {
			continue
		}
		docs = append(docs, &tree.Resource{ID: tree.ID{Kind: kindOf(doc)}, APIVersion: HubAPIVersion, Root: doc})
	}
	for _, src := range []string{
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "a", "labels": {"x": "y"}, "unknown": {"deep": 1}},
		  "spec": {"match": {"hosts": ["a", "b"], "path": {"prefix": "/"}}, "upstreams": [{"name": "u", "weight": 2}, {"name": "u", "weight": 3}, {"weight": 4}], "bogus": [1, {"a": 2}]}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "q"}, "spec": {"type": "quota", "config": {"consumerQuota": "c", "free": {"a": [1, {"b": 2}]}}}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "v"}, "spec": {"type": "validation.json-schema", "config": {"schema": {"properties": {"a": {"enum": [1]}}}}}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Plugin", "metadata": {"name": "w"}, "spec": {"configSchema": {"type": "object", "x": [[1]]}}}`,
	} {
		root := mustParse(t, src)
		docs = append(docs, &tree.Resource{ID: tree.ID{Kind: kindOf(root)}, APIVersion: HubAPIVersion, Root: root})
	}
	if len(docs) < 500 {
		t.Fatalf("only %d documents", len(docs))
	}
	for _, res := range docs {
		want := schemaPathOracle(t, idx, res)
		if got := deprecatedEverywhere(t, idx, res, want); !slices.Equal(got, want) {
			t.Fatalf("%s %s: Deprecations\n%s\nwant\n%s", res.ID.Kind, dump(res.Root), strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

// kindOf returns the kind member of an envelope.
func kindOf(root *tree.Node) v1alpha1.Kind {
	k, _ := root.Get("kind")
	if k == nil {
		return ""
	}
	return v1alpha1.Kind(k.Text)
}

// manyEndpoints returns an Upstream with n endpoints, the keyed list whose
// per-entry resolution from the root made Deprecations quadratic.
func manyEndpoints(n int) *tree.Resource {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "u"}, "spec": {"protocol": "http", "endpoints": [`)
	for i := range n {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`{"address": "h` + strconv.Itoa(i) + `:80", "weight": 2}`)
	}
	b.WriteString(`]}}`)
	root, err := parse(b.String(), 0)
	if err != nil {
		panic(err)
	}
	return &tree.Resource{ID: tree.ID{Kind: "Upstream", Name: "u"}, APIVersion: HubAPIVersion, Root: root}
}

// deprecationAlloc returns the warnings of a deprecated endpoint weight on
// res and the bytes Deprecations allocated, on this goroutine.
func deprecationAlloc(t *testing.T, r *Registry, idx *schemaidx.Index, res *tree.Resource) (int, uint64) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	ds := r.Deprecations(idx, res, nil)
	runtime.ReadMemStats(&after)
	return len(ds), after.TotalAlloc - before.TotalAlloc
}

// TestDeprecationsScale is the scale case of a deprecated field in a list
// of thousands of keyed entries: one warning per entry at its key-aware
// path, and four times the entries allocate about four times the bytes
// (counted, not timed, so the result does not depend on the machine;
// BenchmarkDeprecations times it).
func TestDeprecationsScale(t *testing.T) {
	idx := hubIndex(t)
	r, err := NewRegistry(Lifecycle{Fields: map[string]map[string]Notice{HubAPIVersion: {"spec.endpoints[].weight": {Removal: "0.8.0"}}}}, V1alpha1())
	if err != nil {
		t.Fatal(err)
	}
	const n = 4000
	ds := r.Deprecations(idx, manyEndpoints(n), nil)
	if len(ds) != n {
		t.Fatalf("%d warnings, want %d", len(ds), n)
	}
	for i, d := range ds {
		if want := "spec.endpoints[address=h" + strconv.Itoa(i) + ":80].weight"; d.Path.String() != want {
			t.Fatalf("warning %d at %s, want %s", i, d.Path, want)
		}
	}
	deprecationAlloc(t, r, idx, manyEndpoints(10)) // warm the schema caches
	small, a := deprecationAlloc(t, r, idx, manyEndpoints(n))
	large, b := deprecationAlloc(t, r, idx, manyEndpoints(4*n))
	ratio := float64(b) / float64(max(a, 1))
	t.Logf("%d entries allocated %d bytes, %d entries %d: ratio %.2f", small, a, large, b, ratio)
	if large != 4*n || ratio < 2 || ratio > 8 {
		t.Errorf("%d entries allocated %d bytes, %d entries %d: ratio %.2f, want about 4", small, a, large, b, ratio)
	}
}

// BenchmarkDeprecations times Deprecations with a deprecated field in a
// keyed list of 1,000 and 8,000 entries; the two ns/op should differ by
// about eight times.
func BenchmarkDeprecations(b *testing.B) {
	idx := hubIndex(b)
	r, err := NewRegistry(Lifecycle{Fields: map[string]map[string]Notice{HubAPIVersion: {"spec.endpoints[].weight": {Removal: "0.8.0"}}}}, V1alpha1())
	if err != nil {
		b.Fatal(err)
	}
	for _, n := range []int{1000, 8000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			res := manyEndpoints(n)
			b.ReportAllocs()
			for b.Loop() {
				if ds := r.Deprecations(idx, res, nil); len(ds) != n {
					b.Fatalf("%d warnings", len(ds))
				}
			}
		})
	}
}
