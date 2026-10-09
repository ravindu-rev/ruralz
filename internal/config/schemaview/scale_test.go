// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// scaleMembers is the member count of the large-object regression cases:
// a 64 MiB JSON document can hold millions of members (01 req 6), and the
// mapping that scanned the object once per error took about 20 s at this
// size.
const scaleMembers = 100_000

// scaleBelow is the size of the cases with many errors below one large
// object or keyed list entry, which every error's location passes.
const scaleBelow = 20_000

// readBudget bounds the members and elements the mapping reads per node
// of the document: the fixed work budget of the regression cases. Each
// position is resolved once (memo steps), so a large object's index and
// dispatch and a list element's path element are computed once, and a
// mapping that rescans a large object per error reads thousands per node.
// The budget is counted, not timed, so the tests do not depend on the
// machine; BenchmarkValidateLarge reports the time. It does not count
// path rendering or allocation: allocBudget bounds those.
const readBudget = 16

// allocBudget bounds the bytes the mapping allocates per byte of the
// document in the hostile cases, whose errors are a few bytes each (01
// reqs 6 and 54, the hostile-input heap bounds of the 01 test plan): each
// error costs a few hundred bytes, whatever the size of the path elements
// above it. A mapping that renders a large key-aware element once per
// diagnostic allocates its size per error: thousands of bytes per byte of
// these documents.
const allocBudget = 1024

// largeKey is the size of the keyed-entry key in the hostile cases.
const largeKey = 64 << 10

// members writes n members "<prefix>0": <value(i)> ... with a leading
// comma when comma is set.
func members(b *strings.Builder, n int, prefix string, comma bool, value func(int) string) {
	for i := range n {
		if comma || i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`"` + prefix + strconv.Itoa(i) + `": ` + value(i))
	}
}

// ints writes the array [0, 1, ..., n-1].
func ints(b *strings.Builder, n int) {
	b.WriteString("[")
	for i := range n {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Itoa(i))
	}
	b.WriteString("]")
}

func one(int) string { return "1" }

// largeRoute returns a Route whose spec has n unknown members.
func largeRoute(n int) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x"}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]`)
	members(&b, n, "u", true, one)
	b.WriteString(`}}`)
	return b.String()
}

// largeLabels returns a Route whose metadata.labels has n non-string
// values, each a typed-map type error.
func largeLabels(n int) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x", "labels": {`)
	members(&b, n, "k", false, strconv.Itoa)
	b.WriteString(`}}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`)
	return b.String()
}

// largeListener returns a Gateway whose one listener has n unknown
// members before its key name and n non-string hostnames: every type
// error's location passes the keyed entry.
func largeListener(n int) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "g"}, "spec": {"listeners": [{`)
	members(&b, n, "u", false, one)
	b.WriteString(`, "port": 8080, "protocol": "http", "hostnames": `)
	ints(&b, n)
	b.WriteString(`, "name": "a"}]}}`)
	return b.String()
}

// largePolicy returns a Policy whose spec has n unknown members before
// its type and n non-string config.allow entries: every type error's
// location passes the dispatch of spec by type.
func largePolicy(n int) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {`)
	members(&b, n, "u", false, one)
	b.WriteString(`, "type": "authz.ip", "config": {"allow": `)
	ints(&b, n)
	b.WriteString(`}}}`)
	return b.String()
}

// largeSetElement returns an auth.mtls Policy whose one config.subjects
// element (a set element, named in paths by its canonical JSON) has n
// unknown members: every RZ-CFG-006 path repeats the element.
func largeSetElement(n int) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "auth.mtls", "config": {"caCertificate": {"secretRef": {"provider": "env", "name": "CA"}}, "subjects": [{"subject": "CN=a"`)
	members(&b, n, "u", true, one)
	b.WriteString(`}]}}}`)
	return b.String()
}

// largeKeyedEntry returns a Gateway whose one listener has a name of
// largeKey bytes and n non-string hostnames: every type error's path
// repeats the key.
func largeKeyedEntry(n int) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "g"}, "spec": {"listeners": [{"name": "` + strings.Repeat("a", largeKey) + `", "port": 8080, "protocol": "http", "hostnames": `)
	ints(&b, n)
	b.WriteString(`}]}}`)
	return b.String()
}

// stripPositions clears every source position of res, as a resource
// rebuilt without a source map has none: every diagnostic then has the
// same location, and the path alone orders them (01 req 50).
func stripPositions(res *tree.Resource) {
	res.Start = tree.Pos{}
	var strip func(n *tree.Node)
	strip = func(n *tree.Node) {
		if n == nil {
			return
		}
		n.Pos = tree.Pos{}
		for i := range n.Members {
			n.Members[i].KeyPos = tree.Pos{}
			strip(n.Members[i].Value)
		}
		for _, it := range n.Items {
			strip(it)
		}
	}
	strip(res.Root)
}

// mappingAlloc maps the schema errors of res and returns the bytes the
// mapping allocated, with its result. The schema validation itself
// (jsonschema/v6) runs before the measurement.
func mappingAlloc(v *View, res *tree.Resource, files *tree.FileTable) (uint64, diag.List) {
	err := v.schemaErrors(res)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	m := newMapper(v, res, files, MaxDiagnostics)
	m.mapErrors(err)
	got := m.result()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, got
}

// TestLargePathElements bounds the bytes the mapping allocates when many
// errors lie below one large key-aware path element: a set element with
// scaleBelow unknown members and a keyed entry with a largeKey key and
// scaleBelow type errors, with source positions and without (when the
// path orders every diagnostic). The mapping must allocate at most
// allocBudget bytes per document byte, and doubling the errors must not
// quadruple it (01 reqs 6, 50 and 54; review finding: rendering the path
// per diagnostic cost errors times element size, 12 GB for a 249 KB
// document).
func TestLargePathElements(t *testing.T) {
	if testing.Short() {
		t.Skip("validates documents with 20,000 errors")
	}
	v := testView(t)
	for _, tc := range []struct {
		name      string
		src       func(int) string
		code, msg string
	}{
		{"set element", largeSetElement, CodeUnknownField, `unknown field "u0"`},
		{"keyed entry", largeKeyedEntry, CodeSchema, "must be a string, not an integer"},
	} {
		for _, tied := range []bool{false, true} {
			name := tc.name
			if tied {
				name += " without positions"
			}
			t.Run(name, func(t *testing.T) {
				var allocs [2]uint64
				for i, n := range []int{scaleBelow / 2, scaleBelow} {
					src := tc.src(n)
					files := &tree.FileTable{}
					res := mustResource(t, files, "r.json", src)
					if tied {
						stripPositions(res)
					}
					alloc, got := mappingAlloc(v, res, files)
					allocs[i] = alloc
					if limit := uint64(allocBudget) * uint64(len(src)); alloc > limit {
						t.Errorf("%d errors: mapping allocated %d bytes for a %d-byte document, want at most %d (%d per byte)", n, alloc, len(src), limit, allocBudget)
					}
					if len(got) != MaxDiagnostics {
						t.Fatalf("%d errors: Validate() returned %d diagnostics, want MaxDiagnostics (%d)", n, len(got), MaxDiagnostics)
					}
					if got[0].Code != tc.code || got[0].Message != tc.msg {
						t.Errorf("%d errors: first diagnostic = %s %s, want %s %s", n, got[0].Code, got[0].Message, tc.code, tc.msg)
					}
				}
				if allocs[1] > 3*allocs[0] {
					t.Errorf("mapping allocated %d bytes for %d errors and %d for %d, want growth linear in the errors", allocs[0], scaleBelow/2, allocs[1], scaleBelow)
				}
			})
		}
	}
}

// TestTiedLocationsSorted checks that diagnostics with equal locations
// are ordered by their rendered paths exactly as diag.List.Sort orders
// them, below a large set element and a large keyed entry, for every
// limit (01 req 50).
func TestTiedLocationsSorted(t *testing.T) {
	v := testView(t)
	for _, src := range []string{largeSetElement(300), largeKeyedEntry(300), largeRoute(300), largeListener(300)} {
		files := &tree.FileTable{}
		res := mustResource(t, files, "r.json", src)
		stripPositions(res)
		all := v.Validate(res, files)
		if len(all) < 300 {
			t.Fatalf("Validate() returned %d diagnostics, want at least 300", len(all))
		}
		sorted := slices.Clone(all)
		sorted.Sort()
		if !slices.EqualFunc(all, sorted, sameDiagnostic) {
			t.Errorf("%.80s: Validate() result is not in diag.List.Sort order", src)
		}
		for _, limit := range []int{1, 7, 50, len(all) - 1} {
			if got := v.check(res, files, limit).result(); !slices.EqualFunc(got, all[:limit], sameDiagnostic) {
				t.Errorf("%.80s: check(limit %d) is not the first %d of the full result", src, limit, limit)
			}
		}
	}
}

// scaleCases are the large-object regression cases with the first
// diagnostic each must return.
func scaleCases() []struct {
	name, src, code, first, msg string
} {
	return []struct {
		name, src, code, first, msg string
	}{
		{"unknown members", largeRoute(scaleMembers), CodeUnknownField, "spec.u0", `unknown field "u0"`},
		{"typed-map type errors", largeLabels(scaleMembers), CodeSchema, "metadata.labels.k0", "must be a string, not an integer"},
		{"errors below a keyed entry", largeListener(scaleBelow), CodeUnknownField, "spec.listeners[name=a].u0", `unknown field "u0"`},
		{"errors below a dispatch", largePolicy(scaleBelow), CodeUnknownField, "spec.u0", `unknown field "u0"`},
	}
}

// countNodes returns the number of nodes of a tree.
func countNodes(n *tree.Node) int {
	if n == nil {
		return 0
	}
	c := 1
	for _, m := range n.Members {
		c += countNodes(m.Value)
	}
	for _, it := range n.Items {
		c += countNodes(it)
	}
	return c
}

// TestLargeObjects validates one object with scaleMembers unknown members,
// one typed map with scaleMembers type errors, and many errors below one
// large keyed entry and below one large dispatched object, each within the
// fixed work budget, and checks that the result is the first
// MaxDiagnostics findings in diag.List.Sort order (01 reqs 6, 50 and 54:
// bounded work and output per resource).
func TestLargeObjects(t *testing.T) {
	if testing.Short() {
		t.Skip("validates documents of up to 100,000 members")
	}
	v := testView(t)
	for _, tc := range scaleCases() {
		t.Run(tc.name, func(t *testing.T) {
			files := &tree.FileTable{}
			res := mustResource(t, files, "r.json", tc.src)
			m := v.check(res, files, MaxDiagnostics)
			nodes := countNodes(res.Root)
			if read := m.memo.read; read > readBudget*nodes {
				t.Errorf("mapping read %d members and elements, want at most %d (%d per node)", read, readBudget*nodes, readBudget)
			}
			got := m.result()
			if len(got) != MaxDiagnostics {
				t.Fatalf("Validate() returned %d diagnostics, want MaxDiagnostics (%d)", len(got), MaxDiagnostics)
			}
			if got[0].Code != tc.code || got[0].Path.String() != tc.first || got[0].Message != tc.msg {
				t.Errorf("first diagnostic = %s, want %s at %s: %s", texts(got[:1]), tc.code, tc.first, tc.msg)
			}
			sorted := slices.Clone(got)
			sorted.Sort()
			if !slices.EqualFunc(got, sorted, sameDiagnostic) {
				t.Error("Validate() result is not in diag.List.Sort order")
			}
		})
	}
}

// sameDiagnostic compares the fields that identify a diagnostic.
func sameDiagnostic(a, b diag.Diagnostic) bool {
	return a.Code == b.Code && a.Location == b.Location && a.Path.String() == b.Path.String() &&
		a.Message == b.Message && a.Hint == b.Hint
}

// TestTruncationDeterministic checks that a resource with more findings
// than the limit returns the first ones in sorted order on every run,
// although the validator reports a large object's errors in map order
// (01 req 50: identical diagnostics whatever the run).
func TestTruncationDeterministic(t *testing.T) {
	v := testView(t)
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x", "labels": {`)
	for i := range 300 {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`"k` + strconv.Itoa(i) + `": ` + strconv.Itoa(i))
	}
	b.WriteString(`}}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]`)
	for i := range 300 {
		b.WriteString(`, "u` + strconv.Itoa(i) + `": 1`)
	}
	b.WriteString(`}}`)
	files := &tree.FileTable{}
	res := mustResource(t, files, "r.json", b.String())
	all := v.Validate(res, files)
	if len(all) != 600 {
		t.Fatalf("Validate() returned %d diagnostics, want 600", len(all))
	}
	for _, limit := range []int{0, 1, 7, 50, 599, 600, 601} {
		want := all[:min(max(limit, 1), len(all))]
		for range 5 {
			got := v.check(res, files, limit).result()
			if !slices.EqualFunc(got, want, sameDiagnostic) {
				t.Fatalf("check(limit %d) = %d diagnostics starting %s, want the first %d of the full result", limit, len(got), texts(got[:1]), len(want))
			}
		}
	}
}

// TestMemberIndex checks that indexed lookups agree with tree.Node.Get,
// first member winning, for small and large objects.
func TestMemberIndex(t *testing.T) {
	for _, n := range []int{0, 3, memberIndexMin - 1, memberIndexMin, 100} {
		obj := &tree.Node{Kind: tree.KindMap}
		for i := range n {
			obj.Members = append(obj.Members, tree.Member{Key: "k" + strconv.Itoa(i%max(n-2, 1)), Value: &tree.Node{Kind: tree.KindInt, Text: strconv.Itoa(i)}})
		}
		x := &memo{}
		for i := range n + 2 {
			key := "k" + strconv.Itoa(i)
			want, wantOK := obj.Get(key)
			got, ok := x.get(obj, key)
			if got != want || ok != wantOK {
				t.Errorf("%d members: get(%s) = %v, %v, want %v, %v", n, key, got, ok, want, wantOK)
			}
		}
	}
	x := &memo{}
	if _, ok := x.get(&tree.Node{Kind: tree.KindList}, "a"); ok {
		t.Error("get(list) = ok")
	}
	if _, ok := x.get(nil, "a"); ok {
		t.Error("get(nil) = ok")
	}
}

// BenchmarkValidateLarge measures stage F on the large-object regression
// cases (01 req 54).
func BenchmarkValidateLarge(b *testing.B) {
	v := testView(b)
	for _, bc := range scaleCases() {
		b.Run(bc.name, func(b *testing.B) {
			files := &tree.FileTable{}
			res := mustResource(b, files, "r.json", bc.src)
			b.ReportAllocs()
			for b.Loop() {
				if got := v.Validate(res, files); len(got) != MaxDiagnostics {
					b.Fatalf("Validate() returned %d diagnostics", len(got))
				}
			}
		})
	}
}
