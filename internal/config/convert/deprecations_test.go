// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package convert

import (
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

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

// TestDeprecationsSchemaPaths covers the incremental schema path of
// Deprecations: with every field deprecated, it warns at exactly the
// positions schemaidx.Index.SchemaPath resolves, with the same schema
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

// longNameDoc returns an auth.api-key Policy whose open config holds one
// member with a name of nameBytes bytes and n members below it.
func longNameDoc(t *testing.T, name string, n int) (*tree.Resource, int) {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "auth.api-key", "config": {"header": "x-a", "` + name + `": {"k": 1`)
	for i := range n {
		b.WriteString(`, "m` + strconv.Itoa(i) + `": 1`)
	}
	b.WriteString(`}}}}`)
	src := b.String()
	return &tree.Resource{ID: tree.ID{Kind: "Policy", Name: "p"}, APIVersion: HubAPIVersion, Root: mustParse(t, src)}, len(src)
}

// TestDeprecationsLongName is the scale case of a long member name with
// many members below it, where copying the parent's whole schema path at
// each position would take 2 GB for a 400 KB source: whether a
// deprecated path leads into the subtree (one warning, at its last
// member, whose message names the table's path) or not (none, and
// nothing below the name is walked), Deprecations allocates less than the
// source, since no position builds its schema path text.
func TestDeprecationsLongName(t *testing.T) {
	idx := hubIndex(t)
	name := strings.Repeat("n", 100_000)
	for _, tc := range []struct {
		what, field string
		warnings    int
	}{
		{"deprecated path elsewhere", "spec.timeout", 0},
		{"deprecated path below the name", "spec.config." + name + ".m19999", 1},
	} {
		t.Run(tc.what, func(t *testing.T) {
			r, err := NewRegistry(Lifecycle{Fields: map[string]map[string]Notice{HubAPIVersion: {tc.field: {Removal: "0.8.0"}}}}, V1alpha1())
			if err != nil {
				t.Fatal(err)
			}
			res, src := longNameDoc(t, name, 20_000)
			deprecationAlloc(t, r, idx, res) // warm the schema caches
			got, alloc := deprecationAlloc(t, r, idx, res)
			t.Logf("%d warnings, %d bytes allocated for a %d-byte source", got, alloc, src)
			if got != tc.warnings || alloc > uint64(src) { //nolint:gosec // G115: the size is positive.
				t.Errorf("%d warnings and %d bytes allocated for a %d-byte source; want %d and less than the source", got, alloc, src, tc.warnings)
			}
		})
	}
}

// TestDeprecationsAuthoredOnly covers the tree.StyleDefaulted rule of
// Deprecations: a deprecated field that a default wrote, and everything
// below a defaulted object, gives no warning; the same field authored
// gives one.
func TestDeprecationsAuthoredOnly(t *testing.T) {
	idx := hubIndex(t)
	r, err := NewRegistry(Lifecycle{Fields: map[string]map[string]Notice{HubAPIVersion: {
		"spec.upstreams[].weight": {Removal: "0.8.0"}, "spec.retries.attempts": {Removal: "0.8.0"},
	}}}, V1alpha1())
	if err != nil {
		t.Fatal(err)
	}
	route := mustParse(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r"}, "spec": {"match": {}, "upstreams": [{"name": "a", "weight": 2}, {"name": "b", "weight": 1}]}}`)
	spec, _ := route.Get("spec")
	l, _ := spec.Get("upstreams")
	w, _ := l.Items[1].Get("weight")
	w.Style = tree.StyleDefaulted
	res := &tree.Resource{ID: tree.ID{Kind: "Route", Name: "r"}, APIVersion: HubAPIVersion, Root: route}
	ds := r.Deprecations(idx, res, nil)
	if len(ds) != 1 || ds[0].Path.String() != "spec.upstreams[name=a].weight" {
		t.Errorf("Deprecations = %v, want one warning at the authored weight", ds)
	}
	up := mustParse(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "u"}, "spec": {"protocol": "http", "endpoints": [{"address": "a:80"}], "retries": {"attempts": 3}}}`)
	upSpec, _ := up.Get("spec")
	retries, _ := upSpec.Get("retries")
	retries.Style = tree.StyleDefaulted
	if ds := r.Deprecations(idx, &tree.Resource{ID: tree.ID{Kind: "Upstream", Name: "u"}, APIVersion: HubAPIVersion, Root: up}, nil); len(ds) != 0 {
		t.Errorf("Deprecations below a defaulted object = %v, want none", ds)
	}
}

// TestDeprecationsCounted covers the work count of DeprecationsCounted:
// it reports one unit per position visited plus the members and elements
// there, the same warnings as Deprecations, and nothing for an empty
// table.
func TestDeprecationsCounted(t *testing.T) {
	idx := hubIndex(t)
	r, err := NewRegistry(Lifecycle{Fields: map[string]map[string]Notice{HubAPIVersion: {"spec.endpoints[].weight": {Removal: "0.8.0"}}}}, V1alpha1())
	if err != nil {
		t.Fatal(err)
	}
	res := manyEndpoints(100)
	units, calls := 0, 0
	ds := r.DeprecationsCounted(idx, res, nil, func(k int) { units += k; calls++ })
	if len(ds) != 100 || len(r.Deprecations(idx, res, nil)) != 100 {
		t.Fatalf("%d warnings, want 100", len(ds))
	}
	// One call for the scan for nil nodes (nullsFilled), counting the
	// tree's nodes, then one per position: the root, its 4 members, the
	// spec's protocol and endpoints, 100 entries with their address and
	// weight.
	positions := 1 + 4 + 2 + 100*3
	if wantCalls := 1 + positions; calls != wantCalls || units < 2*positions+100*2 {
		t.Errorf("count called %d times for %d units, want %d calls and at least %d units", calls, units, wantCalls, 2*positions+100*2)
	}
	units = 0
	if ds := Default().DeprecationsCounted(idx, res, nil, func(k int) { units += k }); len(ds) != 0 || units != 0 {
		t.Errorf("empty table: %d warnings and %d units, want none", len(ds), units)
	}
}

// TestClipBounded covers clip, which never converts the whole string to
// runes (64 MB for a 16 MiB value) and never returns a short value that is
// not UTF-8 as it is, raw bytes in a message: it gives what the
// whole-string conversion gives for short, exact, long, multi-byte and
// invalid UTF-8 values, and allocates only its result for a 16 MiB value.
func TestClipBounded(t *testing.T) {
	ref := func(s string) string {
		if len([]rune(s)) <= maxShown {
			return string([]rune(s))
		}
		return string([]rune(s)[:maxShown]) + "..."
	}
	for _, s := range []string{
		"", "ruralz/v1", strings.Repeat("a", maxShown), strings.Repeat("a", maxShown+1), strings.Repeat("é", maxShown+1),
		strings.Repeat("\xff", maxShown+3), strings.Repeat("a", maxShown-1) + "\xffbc", strings.Repeat("\xff", maxShown),
		// Short values that are not UTF-8, which clip must not return as
		// they are, raw bytes in the message.
		"caf\xe9", "\xff", "a\xc3", "\xed\xa0\x80",
	} {
		if got, want := clip(s), ref(s); got != want || !utf8.ValidString(got) {
			t.Errorf("clip(%q) = %q, want %q, valid UTF-8", s, got, want)
		}
	}
	long := strings.Repeat("1", 16<<20)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := clip(long)
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 4096 || got != strings.Repeat("1", maxShown)+"..." {
		t.Errorf("clip of 16 MiB = %d bytes, allocating %d; want %d bytes and at most 4096", len(got), alloc, maxShown+3)
	}
}

// TestDeprecationsCountedBytes covers the byte charges of
// DeprecationsCounted's walk: a set element of objects is named by its
// RFC 8785 text, whose bytes count; a member name the table lookups
// compare counts its bytes once per comparison; and a tree with a nil node
// is scanned and copied (nullsFilled), each node counted. Each pair of
// resources differs in one place only.
func TestDeprecationsCountedBytes(t *testing.T) {
	idx := hubIndex(t)
	r, err := NewRegistry(Lifecycle{Fields: map[string]map[string]Notice{HubAPIVersion: {
		"spec.config.subjects[].commonName": {Removal: "0.8.0"},
		"spec.config.headers[].value":       {Removal: "0.8.0"},
	}}}, V1alpha1())
	if err != nil {
		t.Fatal(err)
	}
	units := func(src string, edit func(root *tree.Node)) int {
		root := mustParse(t, src)
		if edit != nil {
			edit(root)
		}
		res := &tree.Resource{ID: tree.ID{Kind: "Policy", Name: "p"}, APIVersion: HubAPIVersion, Root: root}
		total := 0
		r.DeprecationsCounted(idx, res, nil, func(k int) { total += k })
		return total
	}
	mtls := func(cn string) string {
		return `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "auth.mtls", "config": {"subjects": [{"commonName": "` + cn + `"}]}}}`
	}
	const extra = 1000
	if got := units(mtls(strings.Repeat("a", extra)), nil) - units(mtls(""), nil); got < extra {
		t.Errorf("a set element %d bytes longer counts %d more units, want at least %d", extra, got, extra)
	}
	open := func(name string) string {
		return `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "plugin", "plugin": "pl", "config": {"` + name + `": 1}}}`
	}
	// The config's range holds two paths: two binary searches of one
	// comparison or more each read the name.
	if got := units(open("x"+strings.Repeat("y", extra)), nil) - units(open("x"), nil); got < 2*extra {
		t.Errorf("a member name %d bytes longer counts %d more units, want at least %d", extra, got, 2*extra)
	}
	nilled := func(root *tree.Node) {
		meta, _ := root.Get("metadata")
		meta.Members[0].Value = nil
	}
	plain := units(mtls("a"), nil)
	if got := units(mtls("a"), nilled) - plain; got < 7 {
		t.Errorf("a tree with a nil node counts %d more units, want the copy of its nodes", got)
	}
}
