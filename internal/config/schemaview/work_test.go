// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"cmp"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// counter returns a work value whose yield hook counts its calls.
func counter() (*work, *int) {
	calls := new(int)
	return &work{yield: func() { *calls++ }}, calls
}

// TestWorkCharge checks the yield cadence: one Yield call per yieldEvery
// units ticked, one after a span charged at once however long, none for
// no work, and a nil hook or work value is a no-op (01 req 53: checked
// every 256 nodes inside a unit).
func TestWorkCharge(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(w *work)
		// yields and total are the Yield calls and units counted.
		yields, total int
	}{
		{"255 ticks", func(w *work) { tickN(w, yieldEvery-1) }, 0, yieldEvery - 1},
		{"256 ticks", func(w *work) { tickN(w, yieldEvery) }, 1, yieldEvery},
		{"511 ticks", func(w *work) { tickN(w, 2*yieldEvery-1) }, 1, 2*yieldEvery - 1},
		{"512 ticks", func(w *work) { tickN(w, 2*yieldEvery) }, 2, 2 * yieldEvery},
		{"1000 ticks", func(w *work) { tickN(w, 1000) }, 3, 1000},
		{"one long span", func(w *work) { w.charge(1000) }, 1, 1000},
		{"a span after 255 ticks", func(w *work) { tickN(w, yieldEvery-1); w.charge(1) }, 1, yieldEvery},
		{"no work", func(w *work) { w.charge(0); w.charge(-5) }, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, calls := counter()
			tc.do(w)
			if *calls != tc.yields || w.total != tc.total {
				t.Errorf("Yield calls = %d, units = %d, want %d and %d", *calls, w.total, tc.yields, tc.total)
			}
		})
	}
	var none *work
	none.tick()
	none.charge(1000)
	silent := &work{}
	tickN(silent, 1000)
	if silent.total != 0 {
		t.Errorf("work without a hook counted %d units", silent.total)
	}
}

func tickN(w *work, n int) {
	for range n {
		w.tick()
	}
}

// firstValue is tree.Node.JSONValue with the first of two members with
// one key winning, as tree.Node.Get resolves them.
func firstValue(n *tree.Node) any {
	if n == nil || n.Kind != tree.KindMap {
		if n != nil && n.Kind == tree.KindList {
			l := make([]any, len(n.Items))
			for i, it := range n.Items {
				l[i] = firstValue(it)
			}
			return l
		}
		return n.JSONValue()
	}
	m := map[string]any{}
	for _, mem := range n.Members {
		if _, dup := m[mem.Key]; !dup {
			m[mem.Key] = firstValue(mem.Value)
		}
	}
	return m
}

// TestInstance checks that the instance built for the validator equals
// tree.Node.JSONValue for every kind and unknown kinds, keeps the first of
// two members with one key as tree.Node.Get does, and that each node
// converted is one work unit (01 req 53; review findings: tree.JSONValue
// built the instance in one span, and the last duplicate member won).
func TestInstance(t *testing.T) {
	str := func(s string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: s} }
	long := strings.Repeat("n", 4096)
	for _, tc := range []struct {
		name  string
		n     *tree.Node
		nodes int
		// hashed is the bytes of the member names hashed (every member of
		// a converted object); keys and strs the bytes of the member names
		// and strings converted.
		hashed, keys, strs int
	}{
		{"nil", nil, 1, 0, 0, 0},
		{"null", &tree.Node{Kind: tree.KindNull}, 1, 0, 0, 0},
		{"bool", &tree.Node{Kind: tree.KindBool, Bool: true}, 1, 0, 0, 0},
		{"integer", &tree.Node{Kind: tree.KindInt, Text: "-7"}, 1, 0, 0, 0},
		{"float", &tree.Node{Kind: tree.KindFloat, Text: "1.50e3"}, 1, 0, 0, 0},
		{"string", str("a"), 1, 0, 0, 1},
		{"long string", str(long), 1, 0, 0, len(long)},
		{"unknown kind", &tree.Node{Kind: tree.Kind(99), Text: "x"}, 1, 0, 0, 0},
		{"empty map", &tree.Node{Kind: tree.KindMap}, 1, 0, 0, 0},
		{"empty list", &tree.Node{Kind: tree.KindList}, 1, 0, 0, 0},
		{"nested", &tree.Node{Kind: tree.KindMap, Members: []tree.Member{
			{Key: "a", Value: &tree.Node{Kind: tree.KindList, Items: []*tree.Node{str("x"), {Kind: tree.KindNull}, {Kind: tree.KindMap}}}},
			{Key: "b", Value: &tree.Node{Kind: tree.KindInt, Text: "1"}},
		}}, 6, 2, 2, 1},
		{"long member name", &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: long, Value: str("v")}}}, 2, len(long), len(long), 1},
		{"duplicate keys, first wins", &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "k", Value: str("first")}, {Key: "k", Value: str("last")}}}, 2, 2, 1, 5},
		{"duplicate keys with a subtree, first wins", &tree.Node{Kind: tree.KindMap, Members: []tree.Member{
			{Key: "k", Value: &tree.Node{Kind: tree.KindList, Items: []*tree.Node{str("a")}}},
			{Key: "j", Value: str("b")},
			{Key: "k", Value: &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "x", Value: str("c")}}}},
		}}, 4, 3, 2, 2},
		{"nil member value", &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "k"}}}, 2, 1, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := counter()
			b := w.instance(tc.n, testView(t).nums)
			got, nodes := b.value, b.nodes
			if want := firstValue(tc.n); !reflect.DeepEqual(got, want) {
				t.Errorf("instance() = %#v, want %#v", got, want)
			}
			if nodes != tc.nodes || w.total != tc.nodes+tc.hashed {
				t.Errorf("instance() converted %d nodes and charged %d units, want %d and %d", nodes, w.total, tc.nodes, tc.nodes+tc.hashed)
			}
			if b.keyBytes != tc.keys || b.stringBytes != tc.strs {
				t.Errorf("instance() counted %d member-name and %d string bytes, want %d and %d", b.keyBytes, b.stringBytes, tc.keys, tc.strs)
			}
		})
	}
}

// textBytes returns the bytes of every member name and of every string at
// and below n.
func textBytes(n *tree.Node) (keys, strs int) {
	if n == nil {
		return 0, 0
	}
	if n.Kind == tree.KindString {
		strs = len(n.Text)
	}
	for _, m := range n.Members {
		k, s := textBytes(m.Value)
		keys, strs = keys+len(m.Key)+k, strs+s
	}
	for _, it := range n.Items {
		k, s := textBytes(it)
		keys, strs = keys+k, strs+s
	}
	return keys, strs
}

// TestDuplicateMemberFirstWins checks that the validator checks the first
// of two members with one key, the member locate and tree.Node.Get
// resolve, so a finding describes the value at its position: a valid
// first port and an invalid second one give no finding, and an invalid
// first port is reported as itself at its own position (review finding:
// the instance kept the last member, so "port" 8080 was reported as "must
// be an integer, not an integer").
func TestDuplicateMemberFirstWins(t *testing.T) {
	str := func(s string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: s} }
	for _, tc := range []struct {
		name   string
		first  string
		second *tree.Node
		want   string
	}{
		{"valid first", `8080`, str("x"), ""},
		{"invalid first", `"x"`, &tree.Node{Kind: tree.KindInt, Text: "8080"}, "r.json:1:146 error RZ-CFG-005 Gateway/x spec.listeners[name=a].port: must be an integer, not a string\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := &tree.FileTable{}
			res := mustResource(t, files, "r.json", envelope("Gateway", `{"listeners": [{"name": "a", "protocol": "http", "port": `+tc.first+`}]}`))
			spec, _ := res.Root.Get("spec")
			ls, _ := spec.Get("listeners")
			l := ls.Items[0]
			l.Members = append(l.Members, tree.Member{Key: "port", Value: tc.second})
			if got := texts(testView(t).Validate(res, files)); got != tc.want {
				t.Errorf("Validate() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestShadowedMembersSkipped checks that every check after the validator
// skips a member that an earlier member with its key hides, as the
// validator does (the class of the first-wins finding): the uniqueness
// walk does not report duplicates inside a hidden list, the
// reserved-prefix check reports a repeated reserved key once, at the
// first member, and naming substituted variables skips a hidden value,
// while every visible duplicate is still reported. The JSON front ends
// refuse duplicate names, so the later member, parsed from d.json, is
// appended to the object at path.
func TestShadowedMembersSkipped(t *testing.T) {
	route := `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x", "labels": {"ruralz.io/a": "1"}}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`
	dups := `[{"name": "b", "port": 1, "protocol": "http"}, {"name": "b", "port": 2, "protocol": "http", "hostnames": ["h", "h"]}]`
	for _, tc := range []struct {
		name, src string
		path      []string
		key, dup  string
		want      string
	}{
		{"hidden list", envelope("Gateway", `{"listeners": [{"name": "a", "port": 1, "protocol": "http"}]}`), []string{"spec"}, "listeners", dups, ""},
		{
			"visible list", envelope("Gateway", `{"listeners": `+dups+`}`),
			[]string{"spec"},
			"listeners", `[]`,
			"r.json:1:159 error RZ-CFG-005 Gateway/x spec.listeners[name=b]: duplicate entry: name \"b\" is already used by an earlier entry (first entry at r.json:1:113)\n" +
				"r.json:1:214 error RZ-CFG-005 Gateway/x spec.listeners[name=b].hostnames[item=h]: duplicate element \"h\" in a set (first element at r.json:1:209)\n",
		},
		{
			"repeated reserved label", route,
			[]string{"metadata", "labels"},
			"ruralz.io/a", `"2"`,
			"r.json:1:89 error RZ-CFG-005 Route/x metadata.labels[\"ruralz.io/a\"]: label key \"ruralz.io/a\" uses the reserved prefix ruralz.io/\n",
		},
		{
			"distinct reserved labels", route,
			[]string{"metadata", "labels"},
			"ruralz.io/b", `"2"`,
			"d.json:1:1 error RZ-CFG-005 Route/x metadata.labels[\"ruralz.io/b\"]: label key \"ruralz.io/b\" uses the reserved prefix ruralz.io/\n" +
				"r.json:1:89 error RZ-CFG-005 Route/x metadata.labels[\"ruralz.io/a\"]: label key \"ruralz.io/a\" uses the reserved prefix ruralz.io/\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := &tree.FileTable{}
			res := mustResource(t, files, "r.json", tc.src)
			dup := mustResource(t, files, "d.json", tc.dup).Root
			obj := res.Root
			for _, k := range tc.path {
				obj, _ = obj.Get(k)
			}
			obj.Members = append(obj.Members, tree.Member{Key: tc.key, KeyPos: dup.Pos, Value: dup})
			if got := texts(testView(t).Validate(res, files)); got != tc.want {
				t.Errorf("Validate() =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
	v := testView(t)
	str := func(s string, vars ...string) *tree.Node {
		return &tree.Node{Kind: tree.KindString, Text: s, Vars: vars}
	}
	hidden := str("y", "B")
	obj := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "a", Value: str("x", "A")}, {Key: "b", Value: str("z", "C")}, {Key: "a", Value: hidden}}}
	b := (&work{}).instance(obj, v.nums)
	if len(b.shadowed) != 1 || !b.shadowed[hidden] {
		t.Fatalf("instance() shadowed %v, want only the second a", b.shadowed)
	}
	m := newMapper(v, mustResource(t, &tree.FileTable{}, "r.json", `{}`), nil, MaxDiagnostics, Options{})
	m.shadowed = b.shadowed
	if got := m.varsBelow(obj); !slices.Equal(got, []string{"A", "C"}) {
		t.Errorf("varsBelow() = %q, want [A C]", got)
	}
}

// TestHelpersMatchValidate checks that the test helpers that map
// validation errors outside View.check, mappingAlloc and rawValidate,
// validate a tree with a repeated key as Validate does: the first member
// wins and the hidden one is skipped by the checks after the validator
// (built.shadowed; review finding: the helpers mapped without the hidden
// members, and rawValidate let the last member win).
func TestHelpersMatchValidate(t *testing.T) {
	v := testView(t)
	files := &tree.FileTable{}
	res := mustResource(t, files, "r.json", envelope("Gateway", `{"listeners": [{"name": "a", "port": 1, "protocol": "http"}]}`))
	dup := mustResource(t, files, "d.json", `[{"name": "b", "port": "x", "protocol": "http"}, {"name": "b", "port": 2, "protocol": "http", "hostnames": ["h", "h"]}]`).Root
	spec, _ := res.Root.Get("spec")
	spec.Members = append(spec.Members, tree.Member{Key: "listeners", KeyPos: dup.Pos, Value: dup})
	want := texts(v.Validate(res, files))
	if want != "" {
		t.Fatalf("Validate() = %s, want no finding: the hidden list is skipped", want)
	}
	if _, got := mappingAlloc(v, res, files); texts(got) != want {
		t.Errorf("mappingAlloc() =\n%s\nwant\n%s", texts(got), want)
	}
	if got := rawValidate(v, res, files); texts(got) != want {
		t.Errorf("rawValidate() =\n%s\nwant\n%s", texts(got), want)
	}
	// A nil node is a null one for both helpers, as for Validate
	// (nullFilled).
	for _, tc := range nilCases() {
		files := &tree.FileTable{}
		res := mustResource(t, files, "r.json", tc.src)
		for _, p := range tc.paths {
			setAt(t, res.Root, p, nil)
		}
		if tc.unknownKind {
			res.ID.Kind = ""
		}
		want := texts(v.Validate(res, files))
		if _, got := mappingAlloc(v, res, files); texts(got) != want {
			t.Errorf("%s: mappingAlloc() =\n%s\nwant\n%s", tc.name, texts(got), want)
		}
		if got := rawValidate(v, res, files); texts(got) != want {
			t.Errorf("%s: rawValidate() =\n%s\nwant\n%s", tc.name, texts(got), want)
		}
	}
}

// yieldDocs are resources whose stage F work spans every yielding loop:
// many nodes, keyed lists and sets, unknown members, typed-map type
// errors and secret findings.
func yieldDocs() map[string]string {
	var labels, hosts, keys strings.Builder
	members(&labels, 600, "k", false, strconv.Itoa)
	ints(&hosts, 600)
	for i := range 300 {
		if i > 0 {
			keys.WriteString(", ")
		}
		keys.WriteString(`{"name": "k` + strconv.Itoa(i) + `", "secretRef": "literal"}`)
	}
	var unknown strings.Builder
	members(&unknown, 600, "u", true, one)
	return map[string]string{
		"labels":    `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x", "labels": {` + labels.String() + `}}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`,
		"hostnames": envelope("Gateway", `{"listeners": [{"name": "a", "port": 1, "protocol": "http", "hostnames": `+hosts.String()+`}, {"name": "a", "port": 2, "protocol": "http"}], "trustedProxies": ["10.0.0.0/8", "10.0.0.0/8", "192.168.0.0/16"]}`),
		"secrets":   envelope("Consumer", `{"credentials": {"apiKeys": [`+keys.String()+`]}}`),
		"unknown":   largeSetElement(600),
		"members":   `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x"}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]` + unknown.String() + `}}`,
	}
}

// TestYieldSchemaErrors checks that building the instance charges one
// unit per node and per byte of a member name, and that the validator's
// span is charged after it with the instance's nodes and the bytes of its
// member names and strings (01 req 53; review finding: a 16 MiB name or
// string was one unit).
func TestYieldSchemaErrors(t *testing.T) {
	v := testView(t)
	for name, src := range yieldDocs() {
		t.Run(name, func(t *testing.T) {
			res := mustResource(t, &tree.FileTable{}, "r.json", src)
			w, calls := counter()
			_, _, _ = v.runSchema(res, w)
			nodes := countNodes(res.Root)
			keys, strs := textBytes(res.Root)
			if want := 2*nodes + 2*keys + strs; w.total != want {
				t.Errorf("runSchema() charged %d units for %d nodes, %d member-name and %d string bytes, want %d", w.total, nodes, keys, strs, want)
			}
			if *calls < nodes/yieldEvery+1 {
				t.Errorf("runSchema() yielded %d times for %d nodes, want at least %d", *calls, nodes, nodes/yieldEvery+1)
			}
		})
	}
}

// walkCost returns the units checkLists charges for res: per node Walk
// reports, one, two per member of an object (Walk dispatches it twice),
// the bytes of its member name, the members of a keyed entry (ItemElem
// scans them for the key) and the bytes of a set element's canonical JSON
// or number text; per keyed-list entry, one, a unit per member scanned
// for its key and the bytes of a scalar key's canonical JSON and number
// literal; per set element, the bytes of its canonical JSON and number
// literals and, for each duplicate, the
// bytes ItemElem reads to name it again and the nodes of the two elements
// whose variables are named. The path segments of the duplicates' paths
// are charged by memo.intern and counted by internedBytes.
func walkCost(t *testing.T, v *View, res *tree.Resource) int {
	t.Helper()
	units := 0
	err := v.idx.Walk(string(res.ID.Kind), res.Root, func(c *schemaidx.Cursor) error {
		units += 1 + 2*len(c.Node.Members)
		if last, ok := c.Last(); ok {
			switch last.Kind {
			case diag.ElemField:
				units += len(last.Name)
			case diag.ElemKeyed:
				units += len(c.Node.Members)
			case diag.ElemItem:
				units += itemBytes(last.Item, c.Node)
			case diag.ElemIndex:
			}
		}
		if c.Node.Kind != tree.KindList || c.Info.Node == nil || c.Info.InSecret {
			return nil
		}
		lt, key := c.Info.Node.List()
		switch {
		case lt.Keyed():
			for _, it := range c.Node.Items {
				units += 1 + scanned(it, key)
				if k, ok := it.Get(key); ok && (k.Kind == tree.KindString || k.Kind == tree.KindInt || k.Kind == tree.KindFloat) {
					units += len(schemaidx.AppendItemJSON(nil, k)) + numberBytes(k)
				}
			}
		case lt == schemaidx.ListSet:
			first := map[string]*tree.Node{}
			for i, it := range c.Node.Items {
				canon := string(schemaidx.AppendItemJSON(nil, it))
				units += len(canon) + numberBytes(it)
				if prev, dup := first[canon]; dup {
					units += itemBytes(schemaidx.ItemElem(c.Info.Node, it, i).Item, it)
					units += countNodes(it) + countNodes(prev)
					continue
				}
				first[canon] = it
			}
		default:
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return units
}

// itemBytes returns the bytes ItemElem reads or writes for a set element
// named by item value v: the canonical JSON of an object or array with
// the number literals it parses, the text of a number; none for a string
// or boolean.
func itemBytes(v any, n *tree.Node) int {
	switch v := v.(type) {
	case json.RawMessage:
		return len(v) + numberBytes(n)
	case json.Number:
		return len(n.Text)
	default:
		return 0
	}
}

// internedBytes returns the segment bytes of the distinct path nodes at
// and above the paths of m's diagnostics: what memo.intern charged to
// build them, each built once.
func internedBytes(m *mapper) int {
	seen := map[*pathNode]bool{}
	n := 0
	for i := range m.out {
		for p := m.out[i].at; p != nil && !seen[p]; p = p.parent {
			seen[p] = true
			n += len(p.text.text)
		}
	}
	return n
}

// scanned returns the members tree.Node.Get scans to find key in n.
func scanned(n *tree.Node, key string) int {
	if n == nil || n.Kind != tree.KindMap {
		return 0
	}
	for i := range n.Members {
		if n.Members[i].Key == key {
			return i + 1
		}
	}
	return len(n.Members)
}

// TestYieldCheckLists checks that the uniqueness walk charges Walk's own
// work per node (walkCost), one unit per keyed-list entry and member
// scanned for an entry's key, the canonical JSON bytes of keys and set
// elements and the segments of the duplicates' paths, and that the
// reserved-prefix check charges one per member it scans for metadata,
// labels and annotations, one per label and annotation and the bytes of
// the path segments it renders (01 req 53; review finding: Walk's
// ItemElem, a key's canonical JSON and path rendering were uncharged).
func TestYieldCheckLists(t *testing.T) {
	v := testView(t)
	for name, src := range yieldDocs() {
		t.Run(name, func(t *testing.T) {
			res := mustResource(t, &tree.FileTable{}, "r.json", src)
			m := newMapper(v, res, nil, MaxDiagnostics, Options{Yield: func() {}})
			m.checkLists()
			if want := walkCost(t, v, res) + internedBytes(m); m.work.total != want {
				t.Errorf("checkLists() charged %d units, want %d", m.work.total, want)
			}
		})
	}
	src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x", "labels": {"a": "1", "b": "2", "ruralz.io/c": "3"}, "annotations": {"d": "4", "ruralz.io/conversion-data": "{}"}}, "spec": {}}`
	res := mustResource(t, &tree.FileTable{}, "r.json", src)
	m := newMapper(v, res, nil, MaxDiagnostics, Options{Yield: func() {}})
	m.checkReserved()
	// metadata is the third root member, labels the second and
	// annotations the third of metadata, then five labels and annotations,
	// the three labels scanned to find the first ruralz.io/c with its 11
	// bytes compared, and the segments metadata, .labels, ["ruralz.io/c"]
	// and .annotations rendered.
	if want := 3 + 2 + 3 + 5 + 3 + len("ruralz.io/c") + len("metadata") + len(".labels") + len(`["ruralz.io/c"]`) + len(".annotations"); m.work.total != want {
		t.Errorf("checkReserved() charged %d units, want %d", m.work.total, want)
	}
	for _, tc := range []struct {
		name  string
		n     *tree.Node
		key   string
		units int
		found bool
	}{
		{"nil", nil, "a", 0, false},
		{"not an object", &tree.Node{Kind: tree.KindList}, "a", 0, false},
		{"first member", res.Root, "apiVersion", 1, true},
		{"last member", res.Root, "spec", 4, true},
		{"absent", res.Root, "status", 4, false},
	} {
		m.work.total = 0
		got, ok := m.member(tc.n, tc.key)
		want, wantOK := tc.n.Get(tc.key)
		if got != want || ok != wantOK || ok != tc.found || m.work.total != tc.units {
			t.Errorf("member(%s, %q) = %v, %v charging %d units, want %v, %v charging %d", tc.name, tc.key, got, ok, m.work.total, want, wantOK, tc.units)
		}
	}
	m.work.total = 0
	if got := m.present(res.Root, []string{"kind", "status", "spec"}); !slices.Equal(got, []string{"kind", "spec"}) || m.work.total != 2+4+4 {
		t.Errorf("present() = %q charging %d units, want [kind spec] charging 10", got, m.work.total)
	}
}

// countErrors returns the number of validation errors in e's tree that
// are still reachable: mapping releases each cause it has mapped
// (mapper.visitCauses).
func countErrors(e *jsonschema.ValidationError) int {
	if e == nil {
		return 0
	}
	n := 1
	for _, c := range e.Causes {
		n += countErrors(c)
	}
	return n
}

// eachLocation calls fn with the instance location of every error in e's
// tree.
func eachLocation(e *jsonschema.ValidationError, fn func([]string)) {
	fn(e.InstanceLocation)
	for _, c := range e.Causes {
		eachLocation(c, fn)
	}
}

// countSteps returns the number of steps in the trie below s.
func countSteps(s *step) int {
	n := 0
	for _, c := range s.few {
		n += 1 + countSteps(c)
	}
	for _, c := range s.many {
		n += 1 + countSteps(c)
	}
	return n
}

// validationError returns the validation error of res.
func validationError(t *testing.T, v *View, res *tree.Resource) *jsonschema.ValidationError {
	t.Helper()
	var verr *jsonschema.ValidationError
	if _, _, err := v.runSchema(res, nil); !errors.As(err, &verr) {
		t.Fatalf("runSchema() = %v, want a validation error", err)
	}
	return verr
}

// indexBytes returns the bytes memo.index charges to look up key in n
// beyond the members it reads: a name compared in full (as long as key)
// while scanning a small object, or for a large one the names hashed into
// its index the first time (built records it) and key hashed per lookup.
func indexBytes(n *tree.Node, key string, built map[*tree.Node]bool) int {
	if n == nil || n.Kind != tree.KindMap {
		return 0
	}
	b := 0
	if len(n.Members) < memberIndexMin {
		for _, m := range n.Members {
			if len(m.Key) == len(key) {
				b += len(key)
				if m.Key == key {
					break
				}
			}
		}
		return b
	}
	if !built[n] {
		built[n] = true
		for _, m := range n.Members {
			b += len(m.Key)
		}
	}
	return b + len(key)
}

// TestYieldLocate checks that resolving instance locations charges one
// unit per path step created, every member and element the steps read
// (memo.read: step lookups, scans, index builds, dispatches, list
// elements), the bytes every lookup compares or hashes (lookupBytes),
// and for each step created the bytes of its token (looked up in the
// schema's members or parsed as an index), of its segment (rendered and
// interned), of its set element (elemBytes) and of the member names its
// member lookup compares or hashes (01 req 53; review findings: segment
// rendering and long tokens were uncharged, and a lookup charged the
// token once however many equal-length siblings it compared).
func TestYieldLocate(t *testing.T) {
	v := testView(t)
	for name, src := range yieldDocs() {
		t.Run(name, func(t *testing.T) {
			res := mustResource(t, &tree.FileTable{}, "r.json", src)
			verr := validationError(t, v, res)
			m := newMapper(v, res, nil, MaxDiagnostics, Options{Yield: func() {}})
			lookups := 0
			eachLocation(verr, func(loc []string) {
				lookups += lookupBytes(m.memo.top, loc)
				m.locate(loc)
			})
			steps := countSteps(m.memo.top)
			if steps == 0 {
				t.Fatal("no step resolved")
			}
			built := map[*tree.Node]bool{}
			var created func(s *step) int
			created = func(s *step) int {
				b := 0
				for _, c := range append(slices.Clone(s.few), slices.Collect(maps.Values(s.many))...) {
					b += len(c.tok) + len(c.pn.text.text) + elemBytes(c.pn.elem, c.node)
					if s.node == nil || s.node.Kind != tree.KindList {
						b += indexBytes(s.node, c.tok, built)
					}
					b += created(c)
				}
				return b
			}
			if want := steps + m.memo.read + lookups + created(m.memo.top); m.work.total != want {
				t.Errorf("locate() charged %d units for %d steps reading %d members and elements, want %d", m.work.total, steps, m.memo.read, want)
			}
		})
	}
}

// lookupBytes returns the bytes the lookups of loc compare or hash in the
// trie below top as it stands before locate resolves loc (step.find): per
// step, the token's bytes for each child of the token's length that a
// scan compares, or once for an indexed step's lookup; a step created
// along the way has no child to compare.
func lookupBytes(top *step, loc []string) int {
	b := 0
	s := top
	for _, tok := range loc {
		if s == nil {
			return b
		}
		if s.many != nil {
			b += len(tok)
			s = s.many[tok]
			continue
		}
		var next *step
		for _, c := range s.few {
			if len(c.tok) == len(tok) {
				b += len(tok)
				if c.tok == tok {
					next = c
					break
				}
			}
		}
		s = next
	}
	return b
}

// TestYieldVisit checks that mapping charges one unit per validation
// error visited, what locating it charges (every location resolved
// before), and per unknown member reported one unit, its share of the
// comparisons that sort the unknown members (one unit and the bytes of
// the common prefix each reads), its member lookup and the bytes of its
// path segment, and that flushing the secret findings charges one per
// finding (01 req 53; review finding: a long name was one unit).
func TestYieldVisit(t *testing.T) {
	v := testView(t)
	for name, src := range yieldDocs() {
		t.Run(name, func(t *testing.T) {
			res := mustResource(t, &tree.FileTable{}, "r.json", src)
			verr := validationError(t, v, res)
			m := newMapper(v, res, nil, MaxDiagnostics, Options{Yield: func() {}})
			want := countErrors(verr)
			eachLocation(verr, func(loc []string) {
				p := m.locate(loc)
				if p.node != nil && p.node.Kind == tree.KindMap {
					m.memo.index(p.node, "")
				}
			})
			m.work.total = 0
			eachLocation(verr, func(loc []string) { m.locate(loc) })
			want += m.work.total
			built := map[*tree.Node]bool{}
			var unknown func(e *jsonschema.ValidationError)
			unknown = func(e *jsonschema.ValidationError) {
				if k, ok := e.ErrorKind.(*kind.AdditionalProperties); ok {
					if p := m.locate(e.InstanceLocation); p.secret == nil {
						built[p.node] = true
						want += len(k.Properties) + sortCost(k.Properties)
						for _, name := range k.Properties {
							want += membersScanned(p.node, name) + indexBytes(p.node, name, built)
							want += len(segmentText(diag.Field(name), p.path.depth()))
						}
					}
				}
				for _, c := range e.Causes {
					unknown(c)
				}
			}
			unknown(verr)
			m.work.total = 0
			m.visit(verr)
			if m.work.total != want {
				t.Errorf("visit() charged %d units, want %d", m.work.total, want)
			}
			m.work.total = 0
			pending := len(m.order)
			m.flushSecrets()
			if m.work.total != pending {
				t.Errorf("flushSecrets() charged %d units for %d secret findings", m.work.total, pending)
			}
			if name == "secrets" && pending != 300 {
				t.Errorf("secrets: %d secret findings, want 300", pending)
			}
			if len(m.order) != 0 || len(m.secrets) != 0 {
				t.Errorf("flushSecrets() kept %d pending findings", len(m.order))
			}
		})
	}
}

// membersScanned returns the members memo.index reads to find key in n
// once its index is built: those a small object scans, none for a large
// one.
func membersScanned(n *tree.Node, key string) int {
	if n == nil || n.Kind != tree.KindMap || len(n.Members) >= memberIndexMin {
		return 0
	}
	return scanned(n, key)
}

// sortCost returns the units sorting names charges, one per comparison
// and the bytes of the common prefix each reads, with the comparisons
// slices.SortFunc makes, which are deterministic for one input.
func sortCost(names []string) int {
	n := 0
	slices.SortFunc(slices.Clone(names), func(a, b string) int {
		n += 1 + commonPrefix(a, b)
		return strings.Compare(a, b)
	})
	return n
}

// TestYieldSortAndResult checks that each comparison that sorts the
// diagnostics is a work unit, and that materializing the result charges
// one unit per returned diagnostic after sorting (01 req 53).
func TestYieldSortAndResult(t *testing.T) {
	v := testView(t)
	src := yieldDocs()["labels"]
	res := mustResource(t, &tree.FileTable{}, "r.json", src)
	stripPositions(res)
	m := v.check(res, nil, MaxDiagnostics, Options{Yield: func() {}})
	if len(m.out) < 600 {
		t.Fatalf("check() kept %d diagnostics, want at least 600", len(m.out))
	}
	m.sortEntries()
	m.work.total = 0
	m.sortEntries()
	sorted := m.work.total
	if sorted < len(m.out)-1 {
		t.Fatalf("sortEntries() charged %d units for %d entries, want at least one per comparison", sorted, len(m.out))
	}
	m.work.total = 0
	got := m.result()
	if want := sorted + len(got); m.work.total != want {
		t.Errorf("result() charged %d units, want %d comparisons and %d diagnostics", m.work.total, sorted, len(got))
	}
	// Sorting diagnostics at long sibling segments charges each
	// comparison and the bytes it scans (memo.scanned), counted here by
	// the same sort on the rendered paths, which compares the same pairs
	// (review finding: a first scan of two long siblings was one unit).
	long := mustResource(t, &tree.FileTable{}, "r.json", siblingLabels(512, 256, false))
	stripPositions(long)
	m = v.check(long, nil, MaxDiagnostics, Options{Yield: func() {}})
	if len(m.out) != 512 {
		t.Fatalf("check() kept %d diagnostics, want 512", len(m.out))
	}
	paths := map[*pathNode]string{}
	for _, e := range m.out {
		paths[e.at] = e.at.path().String()
	}
	comparisons := 0
	slices.SortFunc(slices.Clone(m.out), func(a, b entry) int {
		comparisons++
		return cmp.Or(cmp.Compare(paths[a.at], paths[b.at]), cmp.Compare(a.d.Message, b.d.Message))
	})
	m.work.total = 0
	before := m.memo.scanned
	m.sortEntries()
	if scanned := m.memo.scanned - before; scanned < 256*comparisons/2 || m.work.total != comparisons+scanned {
		t.Errorf("sortEntries() charged %d units, want %d comparisons and the %d bytes they scanned", m.work.total, comparisons, scanned)
	}
}

// TestYieldPrune checks that pruning charges one unit per comparison that
// sorts the kept diagnostics and one per diagnostic it keeps (01 req 53).
func TestYieldPrune(t *testing.T) {
	v := testView(t)
	res := mustResource(t, &tree.FileTable{}, "r.json", yieldDocs()["labels"])
	stripPositions(res)
	m := v.check(res, nil, 1<<30, Options{Yield: func() {}})
	if len(m.out) < 600 {
		t.Fatalf("check() kept %d diagnostics, want at least 600", len(m.out))
	}
	comparisons := 0
	slices.SortFunc(slices.Clone(m.out), func(a, b entry) int {
		comparisons++
		return m.compareEntries(&a, &b)
	})
	m.keep = len(m.out) / 2
	m.work.total = 0
	m.prune()
	if want := comparisons + m.keep; m.work.total != want {
		t.Errorf("prune() charged %d units, want %d comparisons and %d kept", m.work.total, comparisons, m.keep)
	}
}

// TestYieldVarsBelow checks that naming the variables of a duplicate set
// element charges one unit per node it reads (01 req 53).
func TestYieldVarsBelow(t *testing.T) {
	res := mustResource(t, &tree.FileTable{}, "r.json", largeSetElement(300))
	m := newMapper(testView(t), res, nil, MaxDiagnostics, Options{Yield: func() {}})
	if got := m.varsBelow(res.Root); got != nil {
		t.Errorf("varsBelow() = %q, want none", got)
	}
	if want := countNodes(res.Root); m.work.total != want {
		t.Errorf("varsBelow() charged %d units for %d nodes", m.work.total, want)
	}
}

// TestValidateYields checks Options.Yield end to end: ValidateWith on a
// resource of N nodes calls Yield at least once per 256 nodes for each
// pass it makes over the nodes (building the instance, the uniqueness
// walk) and once after the validator's span, through a View and a Set,
// and returns what Validate returns (01 req 53; review finding: stage F
// had no yield hook).
func TestValidateYields(t *testing.T) {
	s, err := embedded()
	if err != nil {
		t.Fatal(err)
	}
	v := testView(t)
	for _, n := range []int{0, 10, 20_000} {
		var hosts strings.Builder
		ints(&hosts, n)
		src := envelope("Gateway", `{"listeners": [{"name": "a", "port": 1, "protocol": "http", "hostnames": `+hosts.String()+`}]}`)
		t.Run(strconv.Itoa(n)+" hostnames", func(t *testing.T) {
			files := &tree.FileTable{}
			res := mustResource(t, files, "r.json", src)
			nodes := countNodes(res.Root)
			want := v.Validate(res, files)
			calls := 0
			got := v.ValidateWith(res, files, Options{Yield: func() { calls++ }})
			if !slices.EqualFunc(got, want, sameDiagnostic) {
				t.Error("ValidateWith() differs from Validate()")
			}
			if least := 2*(nodes/yieldEvery) + min(nodes/yieldEvery, 1); calls < least {
				t.Errorf("ValidateWith() yielded %d times for %d nodes, want at least %d", calls, nodes, least)
			}
			t.Logf("%d nodes: %d Yield calls", nodes, calls)
			setCalls := 0
			setGot, ok := s.ValidateWith(res, files, Options{Yield: func() { setCalls++ }})
			if !ok || !slices.EqualFunc(setGot, want, sameDiagnostic) || setCalls != calls {
				t.Errorf("Set.ValidateWith() = %d diagnostics, %v, %d yields; want %d, true, %d", len(setGot), ok, setCalls, len(want), calls)
			}
		})
	}
	if got := v.ValidateWith(nil, nil, Options{Yield: func() { t.Error("yielded for a nil resource") }}); got != nil {
		t.Errorf("ValidateWith(nil) = %v", got)
	}
	if _, ok := s.ValidateWith(nil, nil, Options{}); ok {
		t.Error("Set.ValidateWith(nil) ok")
	}
	if _, ok := s.ValidateWith(&tree.Resource{APIVersion: "ruralz/v9"}, nil, Options{}); ok {
		t.Error("Set.ValidateWith(unserved) ok")
	}
}
