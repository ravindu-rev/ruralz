// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"cmp"
	"math"
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
// these documents. TestLargePathElements measured 176 to 275 bytes per
// byte on Go 1.27.1 with and without -race, and 195 to 298 on the Go
// 1.26.8 floor with GOEXPERIMENT=jsonv2, with and without -race.
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

// mappingAlloc maps the schema errors of res as mapper.run does and
// returns the bytes the mapping allocated, with its result. The schema
// validation itself (jsonschema/v6) runs before the measurement.
func mappingAlloc(v *View, res *tree.Resource, files *tree.FileTable) (uint64, diag.List) {
	shadowed, err := v.runSchema(res, nil)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	m := newMapper(v, res, files, MaxDiagnostics, Options{})
	m.shadowed = shadowed
	m.mapErrors(err)
	got := m.result()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, got
}

// TestLargePathElements bounds the bytes the mapping allocates when many
// errors lie below one large key-aware path element: a set element with
// that many unknown members and a keyed entry with a largeKey key and
// that many type errors, with source positions and without (when the
// path orders every diagnostic). The larger size passes 2*MaxDiagnostics,
// so the mapping prunes and then compares every later error with the
// bound below the large element (mapper.record), a comparison that must
// not render the path either. The mapping must allocate at most
// allocBudget bytes per document byte, and doubling the errors must not
// quadruple it (01 reqs 6, 50 and 54; review findings: rendering the path
// per diagnostic cost errors times element size, 12 GB for a 249 KB
// document, and no case reached the bound).
func TestLargePathElements(t *testing.T) {
	if testing.Short() {
		t.Skip("validates documents with 30,000 errors")
	}
	sizes := [2]int{3 * MaxDiagnostics / 2, 3 * MaxDiagnostics}
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
				for i, n := range sizes {
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
					t.Logf("%d errors: %.1f bytes per document byte", n, float64(alloc)/float64(len(src)))
					if len(got) != MaxDiagnostics+1 {
						t.Fatalf("%d errors: Validate() returned %d diagnostics, want MaxDiagnostics+1 (%d)", n, len(got), MaxDiagnostics+1)
					}
					if got[0].Code != tc.code || got[0].Message != tc.msg {
						t.Errorf("%d errors: first diagnostic = %s %s, want %s %s", n, got[0].Code, got[0].Message, tc.code, tc.msg)
					}
				}
				if allocs[1] > 3*allocs[0] {
					t.Errorf("mapping allocated %d bytes for %d errors and %d for %d, want growth linear in the errors", allocs[0], sizes[0], allocs[1], sizes[1])
				}
			})
		}
	}
}

// prefixBudget bounds the bytes that comparing long sibling path
// segments reads per byte of the document (memo.scanned). A comparison
// sort of the at most 2*(MaxDiagnostics+1) entries compares a long
// sibling with others in about log2 of their number, at most 15, of its
// levels, and the memo reads each pair's common prefix once, however many
// diagnostics lie below the pair. For k long siblings with one error each
// that is about log2(k) bytes per byte: about 12 for the 4,096 typed-map
// keys below (11 to 13 measured; the count varies from run to run,
// because the validator reports a typed map's errors in Go map order, so
// the sort compares different pairs), and the budget allows about twice
// the levels. A mapping that compares the segments once per diagnostic
// pair reads thousands of bytes per document byte when many errors lie
// below each of two siblings.
const prefixBudget = 32

// sharedPrefix returns n bytes of "a", or of "a " when quoted, so a key
// or field name built from it is JSON-quoted in paths (01 req 48).
func sharedPrefix(n int, quoted bool) string {
	if quoted {
		return strings.Repeat("a ", n/2)
	}
	return strings.Repeat("a", n)
}

// suffix names the distinct end of sibling i of k.
type suffix func(i, k int) string

// permuted is a fixed permutation of 1000 to 1000+k-1 (k a power of
// two), descending for two siblings: the validator reports a list's
// entries in list order, so it reports the errors below a sibling that
// sorts later first, and the sort compares across siblings. Errors that
// one error reports together, such as an object's unknown members
// (mapper.unknown sorts them), arrive in sorted order whatever the
// suffixes.
func permuted(i, k int) string { return strconv.Itoa(1000 + (k-1-i)*7919%k) }

// ascending is 1000+i: the siblings' errors arrive in sorted order, so
// once a sibling's errors pass 2*MaxDiagnostics every later error is
// compared with the bound below an earlier sibling (mapper.record).
func ascending(i, _ int) string { return strconv.Itoa(1000 + i) }

// siblingListeners returns a Gateway whose k listeners have names that
// share a prefix of keyLen bytes and end in end(i, k), each with n
// non-string hostnames: n type errors below each of k long keyed-entry
// segments.
func siblingListeners(k, keyLen, n int, quoted bool, end suffix) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "g"}, "spec": {"listeners": [`)
	for i := range k {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`{"name": "` + sharedPrefix(keyLen, quoted) + end(i, k) + `", "port": 8080, "protocol": "http", "hostnames": `)
		ints(&b, n)
		b.WriteString(`}`)
	}
	b.WriteString(`]}}`)
	return b.String()
}

// siblingSubjects returns an auth.mtls Policy whose k config.subjects
// elements (set elements, named in paths by their canonical JSON) have
// subjects that share a prefix of keyLen bytes and end in end(i, k),
// each with n unknown members: n RZ-CFG-006 errors below each of
// k long set-element segments.
func siblingSubjects(k, keyLen, n int, end suffix) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "auth.mtls", "config": {"caCertificate": {"secretRef": {"provider": "env", "name": "CA"}}, "subjects": [`)
	for i := range k {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`{"subject": "CN=` + sharedPrefix(keyLen, false) + end(i, k) + `"`)
		members(&b, n, "u", true, one)
		b.WriteString(`}`)
	}
	b.WriteString(`]}}}`)
	return b.String()
}

// siblingFields returns a Route whose spec has k unknown members with
// names that share a prefix of keyLen bytes and end in permuted(i, k):
// one RZ-CFG-006 at each of k long field segments. One
// additionalProperties error reports them all and mapper.unknown sorts
// them, so they arrive in path order and the sort compares each sibling
// with its neighbors only: the pre-sorted path. siblingLabels covers
// field segments whose errors arrive out of order.
func siblingFields(k, keyLen int, quoted bool) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x"}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]`)
	prefix := sharedPrefix(keyLen, quoted)
	for i := range k {
		b.WriteString(`, "` + prefix + permuted(i, k) + `": 1`)
	}
	b.WriteString(`}}`)
	return b.String()
}

// siblingLabels returns a Route whose metadata.labels has k keys that
// share a prefix of keyLen bytes and end in permuted(i, k), each with an
// integer value: one type error at each of k long field segments, which
// the validator reports one by one in map order, so the sort compares
// siblings across the whole map.
func siblingLabels(k, keyLen int, quoted bool) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x", "labels": {`)
	prefix := sharedPrefix(keyLen, quoted)
	for i := range k {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`"` + prefix + permuted(i, k) + `": ` + strconv.Itoa(i))
	}
	b.WriteString(`}}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`)
	return b.String()
}

// descending is 10,000,000+k-i: every sibling sorts before the earlier
// ones, so each prune keeps only the newest errors and the sorts compare
// mostly pairs never compared before.
func descending(i, k int) string { return strconv.Itoa(10_000_000 + k - i) }

// siblingHostnames returns a Gateway whose one listener has 2k hostnames:
// k strings that share a prefix of keyLen bytes and end in permuted(i, k),
// then the same k again, so each second occurrence is an RZ-CFG-005
// duplicate at a long string set-element segment ([item=...], bare or
// JSON-quoted). duplicateElements reports them, through memo.child and
// comparePaths like the validator's errors.
func siblingHostnames(k, keyLen int, quoted bool) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "g"}, "spec": {"listeners": [{"name": "l", "port": 8080, "protocol": "http", "hostnames": [`)
	prefix := sharedPrefix(keyLen, quoted)
	for i := range 2 * k {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`"` + prefix + permuted(i%k, k) + `"`)
	}
	b.WriteString(`]}]}}`)
	return b.String()
}

// siblingRootFields returns a Route with k unknown members at its root,
// with names that share a prefix of keyLen bytes and end in permuted(i,
// k): one RZ-CFG-006 at each of k long segments of depth 0, rendered
// without the "." separator.
func siblingRootFields(k, keyLen int, quoted bool) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x"}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}`)
	prefix := sharedPrefix(keyLen, quoted)
	for i := range k {
		b.WriteString(`, "` + prefix + permuted(i, k) + `": 1`)
	}
	b.WriteString(`}`)
	return b.String()
}

// sortedOrder reports whether l is in diag.List.Sort order, rendering each
// path once (diag.List.Sort renders both paths per comparison), and the
// first pair out of order.
func sortedOrder(l diag.List) (int, bool) {
	paths := make([]string, len(l))
	for i := range l {
		paths[i] = l[i].Path.String()
	}
	for i := 1; i < len(l); i++ {
		a, b := l[i-1], l[i]
		c := cmp.Or(
			cmp.Compare(a.Environment, b.Environment),
			cmp.Compare(a.File, b.File),
			cmp.Compare(a.Line, b.Line),
			cmp.Compare(a.Column, b.Column),
			cmp.Compare(a.Code, b.Code),
			cmp.Compare(paths[i-1], paths[i]),
			cmp.Compare(a.Message, b.Message),
		)
		if c > 0 {
			return i - 1, false
		}
	}
	return 0, true
}

// lcpBudget is the document bytes per memo.lcp entry the mapping may
// hold: an entry is a pair whose common prefix reached lcpMemo bytes, all
// scanned, and the scans read at most prefixBudget bytes per document
// byte.
const lcpBudget = lcpMemo / prefixBudget

// TestLongSiblingPrefixes bounds the bytes that comparing long sibling
// path segments reads (memo.scanned) and the pairs memo.lcp holds when
// many diagnostics lie below each of two long siblings, below many long
// siblings, and at many long siblings, for every segment kind whose text
// can be long: bare and JSON-quoted keyed entries, set elements named by
// canonical JSON, bare and quoted string set elements (duplicates),
// and bare and quoted field names, as typed-map keys whose errors arrive
// in map order, as unknown members, which arrive sorted, and as unknown
// members at depth 0. Prefixes are long (64 KiB, 16 KiB, 1 KiB, past
// lcpMemo) and medium (900 bytes, remembered only per segment). Siblings
// are met in every order: permuted, so the sort compares across them;
// ascending with 2*MaxDiagnostics errors below each, so every error of
// the second sibling is compared with the bound below the first
// (mapper.record); and descending, so the sorts compare mostly new pairs.
// Positions are stripped, so the paths order every diagnostic, and a
// result whose paths are short enough to render is checked to be in
// diag.List.Sort order. The mapping must read at most prefixBudget bytes
// per document byte, and memo.lcp must hold only pairs whose prefix
// reached lcpMemo, at most one per lcpBudget document bytes (01 reqs 6,
// 50 and 54; review findings:
// nothing guarded the common-prefix memo, whose loss costs errors times
// segment length in CPU but no allocation; no case met the bound below a
// long sibling; the memo held 16 entries per error for siblings in
// descending order, and two segment kinds had no case).
func TestLongSiblingPrefixes(t *testing.T) {
	if testing.Short() {
		t.Skip("validates documents with 40,000 errors")
	}
	v := testView(t)
	for _, tc := range []struct {
		name string
		src  string
		// want is the number of diagnostics Validate returns.
		want int
	}{
		{"two keyed entries", siblingListeners(2, largeKey, scaleBelow/2, false, permuted), MaxDiagnostics + 1},
		{"two quoted keyed entries", siblingListeners(2, largeKey, scaleBelow/2, true, permuted), MaxDiagnostics + 1},
		{"two set elements", siblingSubjects(2, largeKey, scaleBelow/2, permuted), MaxDiagnostics + 1},
		{"two keyed entries in ascending order", siblingListeners(2, largeKey, 2*MaxDiagnostics, false, ascending), MaxDiagnostics + 1},
		{"two quoted keyed entries in ascending order", siblingListeners(2, largeKey, 2*MaxDiagnostics, true, ascending), MaxDiagnostics + 1},
		{"two set elements in ascending order", siblingSubjects(2, largeKey, 2*MaxDiagnostics, ascending), MaxDiagnostics + 1},
		{"two keyed entries with medium names", siblingListeners(2, 900, scaleBelow/2, false, permuted), MaxDiagnostics + 1},
		{"two keyed entries with medium names in ascending order", siblingListeners(2, 900, 2*MaxDiagnostics, false, ascending), MaxDiagnostics + 1},
		{"many keyed entries", siblingListeners(64, 16<<10, 300, false, permuted), MaxDiagnostics + 1},
		{"many keyed entries with medium names", siblingListeners(1024, 900, 20, false, permuted), MaxDiagnostics + 1},
		{"many keyed entries with medium names in descending order", siblingListeners(1024, 900, 20, false, descending), MaxDiagnostics + 1},
		{"many keyed entries with 1 KiB names in descending order", siblingListeners(4000, lcpMemo, 10, false, descending), MaxDiagnostics + 1},
		{"many keyed entries with one error each", siblingListeners(4096, 256, 1, false, permuted), 4096},
		{"many keyed entries with one error each in descending order", siblingListeners(20_000, 64, 1, false, descending), MaxDiagnostics + 1},
		{"many keyed entries with ten errors each in descending order", siblingListeners(8000, 64, 10, false, descending), MaxDiagnostics + 1},
		{"many string set elements", siblingHostnames(4096, 256, false), 4096},
		{"many quoted string set elements", siblingHostnames(4096, 256, true), 4096},
		{"many label keys", siblingLabels(4096, 256, false), 4096},
		{"many quoted label keys", siblingLabels(4096, 256, true), 4096},
		{"many field names", siblingFields(4096, 256, false), 4096},
		{"many quoted field names", siblingFields(4096, 256, true), 4096},
		{"many field names at depth 0", siblingRootFields(4096, 256, false), 4096},
		{"many quoted field names at depth 0", siblingRootFields(4096, 256, true), 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := &tree.FileTable{}
			res := mustResource(t, files, "r.json", tc.src)
			stripPositions(res)
			m := v.check(res, files, MaxDiagnostics, Options{})
			// result sorts what the mapping kept; prune sorted before.
			got := m.result()
			if len(got) != tc.want {
				t.Errorf("Validate() returned %d diagnostics, want %d", len(got), tc.want)
			}
			// Paths of a few KiB are rendered to check the order.
			if len(got) > 0 && len(got[0].Path.String()) <= 4<<10 {
				if i, ok := sortedOrder(got); !ok {
					t.Errorf("diagnostics %d and %d are not in diag.List.Sort order", i, i+1)
				}
			}
			if m.memo.scanned == 0 {
				t.Fatal("no two long sibling segments were compared")
			}
			if limit := prefixBudget * len(tc.src); m.memo.scanned > limit {
				t.Errorf("comparing long sibling segments read %d bytes for a %d-byte document, want at most %d (%d per byte)", m.memo.scanned, len(tc.src), limit, prefixBudget)
			}
			if pairs := len(m.memo.lcp); pairs*lcpMemo > m.memo.scanned || pairs > len(tc.src)/lcpBudget {
				t.Errorf("memo.lcp holds %d pairs after scanning %d bytes of a %d-byte document, want at most %d (one per %d scanned bytes and per %d document bytes)",
					pairs, m.memo.scanned, len(tc.src), min(m.memo.scanned/lcpMemo, len(tc.src)/lcpBudget), lcpMemo, lcpBudget)
			}
			t.Logf("scanned %d bytes for a %d-byte document (%.1f per byte), %d memoized pairs", m.memo.scanned, len(tc.src), float64(m.memo.scanned)/float64(len(tc.src)), len(m.memo.lcp))
		})
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
			if got := v.check(res, files, limit, Options{}).result(); !slices.EqualFunc(got, all[:limit+1], sameDiagnostic) {
				t.Errorf("%.80s: check(limit %d) is not the first %d of the full result", src, limit, limit+1)
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
			m := v.check(res, files, MaxDiagnostics, Options{})
			nodes := countNodes(res.Root)
			if read := m.memo.read; read > readBudget*nodes {
				t.Errorf("mapping read %d members and elements, want at most %d (%d per node)", read, readBudget*nodes, readBudget)
			}
			got := m.result()
			if len(got) != MaxDiagnostics+1 {
				t.Fatalf("Validate() returned %d diagnostics, want MaxDiagnostics+1 (%d)", len(got), MaxDiagnostics+1)
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
	// The cap keeps cap+1 diagnostics (Options.MaxDiagnostics); a cap
	// below 1 keeps 2. Caps from math.MaxInt/2 up keep everything without
	// overflowing (review finding: 2*keep wrapped and prune sliced out of
	// range, a panic at math.MaxInt/2 and above).
	for _, limit := range []int{0, 1, 7, 50, 598, 599, 600, 601, math.MaxInt/2 - 1, math.MaxInt / 2, math.MaxInt/2 + 1, math.MaxInt - 1, math.MaxInt} {
		want := all[:min(max(limit, 1), len(all)-1)+1]
		for range 5 {
			got := v.check(res, files, limit, Options{}).result()
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

// TestStepIndex checks the step trie's child lookup for 0, fewSteps-1,
// fewSteps, fewSteps+1 and 100 children: find agrees with add for present
// and absent tokens, the index replaces the scanned slice exactly past
// fewSteps children, and find reports the children it compared (each one
// scanned, or one index lookup), which memo.step adds to memo.read and
// charges with the token's bytes (review finding: a lookup that scanned
// every child was never counted, so losing the index went unnoticed).
func TestStepIndex(t *testing.T) {
	for _, n := range []int{0, fewSteps - 1, fewSteps, fewSteps + 1, 100} {
		t.Run(strconv.Itoa(n)+" children", func(t *testing.T) {
			s := &step{}
			kids := make([]*step, n)
			for i := range n {
				kids[i] = &step{tok: "t" + strconv.Itoa(i)}
				s.add(kids[i])
			}
			if indexed := s.many != nil; indexed != (n > fewSteps) || (indexed && (s.few != nil || len(s.many) != n)) || (!indexed && len(s.few) != n) {
				t.Fatalf("after %d adds: %d scanned, %d indexed; want the index exactly past %d", n, len(s.few), len(s.many), fewSteps)
			}
			for i := range n {
				c, compared, ok := s.find(kids[i].tok)
				want := i + 1
				if n > fewSteps {
					want = 1
				}
				if !ok || c != kids[i] || compared != want {
					t.Errorf("find(%s) = %p, %d, %v; want %p, %d, true", kids[i].tok, c, compared, ok, kids[i], want)
				}
			}
			c, compared, ok := s.find("absent")
			want := n
			if n > fewSteps {
				want = 1
			}
			if ok || c != nil || compared != want {
				t.Errorf("find(absent) = %p, %d, %v; want nil, %d, false", c, compared, ok, want)
			}
		})
	}
	// memo.step adds the children compared to memo.read and charges them
	// with the token's bytes, on a hit as on a miss.
	x := &memo{work: &work{yield: func() {}}}
	parent := &step{}
	for i := range fewSteps {
		parent.add(&step{tok: "t" + strconv.Itoa(i)})
	}
	x.read, x.work.total = 0, 0
	if c := x.step(parent, "t3"); c.tok != "t3" || x.read != 4 || x.work.total != 4+len("t3") {
		t.Errorf("step(t3) read %d and charged %d, want 4 and %d", x.read, x.work.total, 4+len("t3"))
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
				if got := v.Validate(res, files); len(got) != MaxDiagnostics+1 {
					b.Fatalf("Validate() returned %d diagnostics", len(got))
				}
			}
		})
	}
}
