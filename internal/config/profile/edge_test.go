// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestEdgeCases runs streams the YAML Test Suite does not cover: each row
// gives the findings, or the accepted tree, as YAML 1.2 reads the stream.
func TestEdgeCases(t *testing.T) {
	tests := []struct{ src, want string }{
		{"a: - x\nb: 1\n", "RZ-CFG-001@1:4"},
		{"a: b: c\nd: 1\n", "RZ-CFG-001@1:4"},
		{"a: [1\nb: 2\n", "RZ-CFG-001@2:1"},
		{"a: 1\nb: {x: [1}\n", "RZ-CFG-001@2:10"},
		{"? [x]\n: y\nz: 1\n", "RZ-CFG-001@1:3"},
		// An entry with an empty key, and the next entry at its column.
		{": v\nz: 1\n", `{:s:"v",z:i:1}`},
		{"? \n: v\nz: 1\n", `{:s:"v",z:i:1}`},
		{"!!seq\na: 1\nb: 2\n", "RZ-CFG-001@1:1"},
		{"!!map\na: 1\nb: 2\n", "{a:i:1,b:i:2}"},
		{"--- a: 1\nb: 2\n", "RZ-CFG-001@1:5"},
		{"? a\n? b\nc: 1\n", "{a:n,b:n,c:i:1}"},
		{"a: 1\nb: 2\na: 3\n", "RZ-CFG-002@3:1"},
		{"a: 1\na: 2\nb: .inf\na: 3\nc: !!int x\n", "RZ-CFG-002@2:1 RZ-CFG-005@3:4 RZ-CFG-002@4:1 RZ-CFG-001@5:4"},
		{"!!int x: 1\ny: 2\n", "RZ-CFG-001@1:1"},
		{"? |\n  k\n: v\nw: 1\n", "{k\n:s:\"v\",w:i:1}"},
		{"a:\n  - x\n  - y: 1\n    z:\n      - !!str 1\nb: |\n  t\n", `{a:[s:"x",{y:i:1,z:[s:"1"]}],b:s:"t\n"}`},
		{"a:\n- 1\n- - 2\n  -\n- k: !!seq\n  - x\n", `{a:[i:1,[i:2,n],{k:[s:"x"]}]}`},
		{"a: !!map\n  b: 1\n  c: 2\nd: !!seq\n- 1\n", "{a:{b:i:1,c:i:2},d:[i:1]}"},
		// An empty !!str node is "".
		{"a: !!str\nb: 1\n", `{a:s:"",b:i:1}`},
		{"- !!str\n- x\n", `[s:"",s:"x"]`},
		{"a:\n  b: !!str\nc: 1\n", `{a:{b:s:""},c:i:1}`},
		{"a: !!str # c\nb: 1\n", `{a:s:"",b:i:1}`},
		{"a:\n  - !!str\n  - x\n", `{a:[s:"",s:"x"]}`},
		{"a: !!null\n# c\n", "{a:n}"},
		{"- !!str\n", `[s:""]`},
		{"a: 1 # c\n# c\nb: # c\n  # c\n  c: 2\n", "{a:i:1,b:{c:i:2}}"},
		{"%\n---\n0:\n", "RZ-CFG-001@1:1"},
		{"%FOO bar\n---\na: 1\nb: 2\n", "{a:i:1,b:i:2}"},
		{"? |+0\n", "RZ-CFG-001@1:3"},
		{"0:\n-\n1:\n", "{0:[n],1:n}"},
		// A second node on the '?' line, and "? k:", the complex key
		// {k: null}.
		{"? \"q\"k\n", "RZ-CFG-001@1:6"},
		{"a:\n  ? \"q\"''\n", "RZ-CFG-001@2:8"},
		{"? k:\n", "RZ-CFG-001@1:3"},
		{"? \"q\"k\nz: 1\n", "RZ-CFG-001@1:6"},
		// An explicit key, then a line indented past it.
		{"? \"a\"\n  - \"b\"\n:\nz: 1\n", "RZ-CFG-001@2:3"},
		{"? \"a\"\n  \"b\"\n:\nz: 1\n", "RZ-CFG-001@2:3"},
		{"? !!str \"a\"\n: v\n? a\n  b\n: w\nz: 1\n", `{a:s:"v",a b:s:"w",z:i:1}`},
		{"? a\nb\nz: 1\n", "RZ-CFG-001@2:1"},
		{"? \nz: 1\n", `{:n,z:i:1}`},
		{"? !!str {k: v}\nz: 1\n", "RZ-CFG-001@1:3"},
		// A block collection on its tag's line; in a compact mapping the
		// tag is the first key's, which "a" does not match.
		{"k: !!seq - a\nz: 1\n", "RZ-CFG-001@1:10"},
		{"- !!map a: b\n- 1\n", "RZ-CFG-001@1:3"},
		{"k: !!map ? a\nz: 1\n", "RZ-CFG-001@1:10"},
		{"k: !!seq\n- a\nz: 1\n", `{k:[s:"a"],z:i:1}`},
		// A flow collection at its key's column on a later line is no
		// value: YAML 1.2 indents a value on a later line past its key.
		{"k:\n{a: b}\nz: 1\n", "RZ-CFG-001@2:1"},
		{"k:\n[a]\nz: 1\n", "RZ-CFG-001@2:1"},
		// A directive after "---", and keys, which are never typed.
		{"--- %0\n-\n", "RZ-CFG-001@1:5"},
		{"10000000000000000000: 1\n.inf: 2\n0x: 3\n", "{10000000000000000000:i:1,.inf:i:2,0x:i:3}"},
		// A value on a later line at or left of its entry's column, which
		// YAML 1.2 refuses (s-l+block-node indents it by at least n+1);
		// only a block sequence may be a mapping value at its key's column.
		// A mapping entry at a block sequence's column belongs to the
		// mapping holding the sequence, or is refused.
		{"k:\nv\n", "RZ-CFG-001@2:1"},
		{"a:\n  k:\n  v\n", "RZ-CFG-001@3:3"},
		{"spec:\n  name:\n  [a]\n", "RZ-CFG-001@3:3"},
		{"x:\n  a:\n  'v'\nb: 1\n", "RZ-CFG-001@3:3"},
		{"-\nv\n", "RZ-CFG-001@2:1"},
		{"k:\n-\nv\n", "RZ-CFG-001@3:1"},
		{"a:\n  -\n  v\n", "RZ-CFG-001@3:3"},
		{"spec:\n  items:\n  -\n  x\n  - y\n", "RZ-CFG-001@4:3"},
		{"? a\n:\nv\n", "RZ-CFG-001@3:1"},
		{"x:\n  k2:\n  -\n  -? a\n   -b\n  - x\n", "RZ-CFG-001@4:3"},
		{"-\nk: v\n", "RZ-CFG-001@2:1"},
		{"x:\n  -\n  k: v\n  m: 1\ny: 2\n", "RZ-CFG-001@3:3"},
		{"- -\n  k: v\n", "RZ-CFG-001@2:3"},
		{"-\n? k\n: v\n", "RZ-CFG-001@2:1"},
		{"rules:\n    -\n    match: /api\n", "RZ-CFG-001@3:5"},
		{"- a\n- b\nk: v\n", "RZ-CFG-001@3:1"},
		{"? a\n:\n- x\nk: v\n", `{a:[s:"x"],k:s:"v"}`},
		{"k:\n- a\nz:\n  - b\n", `{k:[s:"a"],z:[s:"b"]}`},
		{"!!str k: c\n  x\n", `{k:s:"c x"}`},
		{"k: a\nb\nz: 1\n", "RZ-CFG-001@2:1"},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, false)
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
	}
}

// TestNullPositions covers the implicit null of an empty value: it sits in
// the column after its ':' or '-', or after the position of an explicit key
// without a ':', whatever follows the entry.
func TestNullPositions(t *testing.T) {
	tests := []struct{ src, want string }{
		{"k1:\n", "{k1@1:1:n@1:4}"},
		{"k1: # c\n", "{k1@1:1:n@1:4}"},
		{"? a\n  -b\n", "{a -b@1:3:n@1:4}"},
		{"? a\n:\n", "{a@1:3:n@2:2}"},
		{"x:\n- \n", "{x@1:1:[n@2:2]}"},
		{"x:\n  ? !!str c\n", "{x@1:1:{c@2:5:n@2:6}}"},
		{"x: {a, b: }\n", "{x@1:1:{a@1:5:n@1:6,b@1:8:n@1:10}}"},
		{"x:\n  a:\n  # c\n", "{x@1:1:{a@2:3:n@2:5}}"},
		{"x:\n  a: 1\n  b:\n", "{x@1:1:{a@2:3:i:1@2:6,b@3:3:n@3:5}}"},
		{"x:\n  !!str a:\n", "{x@1:1:{a@2:3:n@2:11}}"},
		{"x:\n- ? c\n", "{x@1:1:[{c@2:5:n@2:6}]}"},
	}
	for _, tt := range tests {
		for _, tail := range []string{"", "\n", "# c\n", "...\n", "zz: 1\n"} {
			docs, got := parseWith(t, tt.src+tail)
			if got != "" || len(docs) != 1 {
				t.Errorf("%q: %q, %d documents", tt.src+tail, got, len(docs))
				continue
			}
			root := docs[0].Root
			if tail == "zz: 1\n" {
				root = &tree.Node{Kind: tree.KindMap, Members: root.Members[:len(root.Members)-1]}
			}
			if got := show(root, true); got != tt.want {
				t.Errorf("%q: %s, want %s", tt.src+tail, got, tt.want)
			}
		}
	}
}

// TestEmptyEntryBeforeKey covers an empty value before a key: an empty last
// sequence entry before a key at the sequence's column, and a tagged key at
// or left of a mapping whose last entry has no value (an implicit entry
// with nothing after its ':', a '?' entry with no ':', a bare ':'). Each
// gives YAML 1.2's tree alone and after 300 sibling entries, where
// duplicate keys are looked up in a map.
func TestEmptyEntryBeforeKey(t *testing.T) {
	var wide strings.Builder
	for i := range 300 {
		wide.WriteString("w" + itoa(i) + ": v\n")
	}
	wideRoot := mustRoot(t, wide.String())
	tests := []struct{ src, want string }{
		{"a:\n-\nb: 1\n", "{a:[n],b:i:1}"},
		{"a:\n- # c\n# d\nb: 1\n", "{a:[n],b:i:1}"},
		{"a:\n- x\n-\nb: 1\n", `{a:[s:"x",n],b:i:1}`},
		{"a:\n-\n? b\n: 1\n", "{a:[n],b:i:1}"},
		{"a:\n-\n!!str b: 1\n", `{a:[n],b:i:1}`},
		{"x:\n  a:\n  -\n  b: 1\nc: 2\n", "{x:{a:[n],b:i:1},c:i:2}"},
		{"- a:\n  -\n  b: 1\n- 2\n", "[{a:[n],b:i:1},i:2]"},
		{"a:\n- x\nb: 1\n", `{a:[s:"x"],b:i:1}`},
		{"a:\n -\n!!str b: 1\n", "{a:[n],b:i:1}"},
		{"x:\n  a:\n   -\n!!str b: 1\n", "{x:{a:[n]},b:i:1}"},
		{"a:\n - # c\n# d\n!!str b: 1\n", "{a:[n],b:i:1}"},
		{"a:\n  b:\n!!str c: 1\n", "{a:{b:n},c:i:1}"},
		{"e:\n  ? k\n!!str x: v\n", `{e:{k:n},x:s:"v"}`},
		{"a:\n  ? b\n!!str c: 1\n", "{a:{b:n},c:i:1}"},
		{"x:\n  a: 1\n  ? b\n  :\n!!str d: 1\n", "{x:{a:i:1,b:n},d:i:1}"},
		{"a:\n  b:\n    c:\n  !!str d: 1\n", "{a:{b:{c:n},d:i:1}}"},
		{"- a:\n    b:\n  !!str c: 1\n", "[{a:{b:n},c:i:1}]"},
		{"a:\n  b:\n  # c\n!!str c: 1\n", "{a:{b:n},c:i:1}"},
		{"a:\n  b:\n\n!!str c: 1\n", "{a:{b:n},c:i:1}"},
		{"a:\n  b:\n!!int 1: 1\n", "{a:{b:n},1:i:1}"},
		{"a:\n  b:\n!!str 'c': 1\n", "{a:{b:n},c:i:1}"},
		{"e:\n ? k\n!!str x: v", `{e:{k:n},x:s:"v"}`},
		{"  e:e:\n   ? k  \n  !!str x: v\n  #c", `{e:e:{k:n},x:s:"v"}`},
		{"!!str c: 1\na:\n  b:\n", "{c:i:1,a:{b:n}}"},
		{"   :[ !!str k:\n    ,y z :\n   !!str x {x: y}\n", "RZ-CFG-001@2:5"},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, false)
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
		if strings.HasPrefix(tt.src, "- ") || strings.HasPrefix(tt.src, " ") {
			continue
		}
		docs, wd := parseWith(t, wide.String()+tt.src)
		if strings.HasPrefix(tt.want, "RZ-") {
			if want := shiftCodes(t, tt.want, 300); wd != want {
				t.Errorf("%q wide: %q, want %q", tt.src, wd, want)
			}
			continue
		}
		if wd != "" || len(docs) != 1 {
			t.Errorf("%q wide: %q", tt.src, wd)
			continue
		}
		want := strings.TrimSuffix(show(wideRoot, false), "}") + "," + strings.TrimPrefix(tt.want, "{")
		if got := show(docs[0].Root, false); got != want {
			t.Errorf("%q wide: %s, want %s", tt.src, got, want)
		}
	}
}

// shiftCodes moves the lines of codes output down by shift.
func shiftCodes(t *testing.T, codes string, shift int) string {
	t.Helper()
	var out []string
	for _, c := range strings.Fields(codes) {
		code, pos, _ := strings.Cut(c, "@")
		line, col, _ := strings.Cut(pos, ":")
		n, err := strconv.Atoi(line)
		if err != nil {
			t.Fatalf("codes %q", codes)
		}
		out = append(out, code+"@"+strconv.Itoa(n+shift)+":"+col)
	}
	return strings.Join(out, " ")
}

// TestExplicitEntries covers explicit entries ('?') (YAML 1.2.2 rules
// 189-191, 142): the key is one node, which may start on the next line or
// span lines, the empty key is "", and a key that is a collection is
// RZ-CFG-001 at it; a node at or left of the '?' column that is no new
// entry is refused. In a flow collection the key's tag may end its line and
// its content follow on the next.
func TestExplicitEntries(t *testing.T) {
	tests := []struct{ src, want, msg string }{
		// A node at or left of the '?' column that is no new entry.
		{"? a\nb\n", "RZ-CFG-001@2:1", msgNoColon},
		{"? a\n[x]\n", "RZ-CFG-001@2:1", ""},
		{"- ? a\n  b\n", "RZ-CFG-001@2:3", msgNoColon},
		{"k:\n  ? a\nb\n", "RZ-CFG-001@3:1", msgNoColon},
		{"? a\n- b\n", "RZ-CFG-001@2:1", ""},
		// An empty key.
		{"? \n", "{:n}", ""},
		{"? \n: v\n", `{:s:"v"}`, ""},
		{"? \nz: 1\n", "{:n,z:i:1}", ""},
		{"? # c\nz: 1\n", "{:n,z:i:1}", ""},
		{"? \n!!str k: |\n", `{:n,k:s:""}`, ""},
		// A key that is a collection, or a line indented past the key.
		{"? \n  k: \n", "RZ-CFG-001@2:3", msgComplexKey},
		{"? \"b\"\n {k: v}\n", "RZ-CFG-001@2:2", msgIndentMoreMap},
		{"? ? a\n: b\n", "RZ-CFG-001@1:3", msgComplexKey},
		{"? : v\n", "RZ-CFG-001@1:3", msgComplexKey},
		{"? !!str {k: v}\n", "RZ-CFG-001@1:3", msgComplexKey},
		{"? !!seq [a]\n: b\n", "RZ-CFG-001@1:3", msgComplexKey},
		{"? !!map a\n", "RZ-CFG-001@1:3", ""},
		// One-node keys, and entries the next entry ends.
		{"? a\n? b\n", "{a:n,b:n}", ""},
		{"? a\nz: 1\n", "{a:n,z:i:1}", ""},
		{"- ? a\n- b\n", `[{a:n},s:"b"]`, ""},
		{"? !!int 1\n: v\n", `{1:s:"v"}`, ""},
		{"? !!str |\n  x\n: v\n", "{x\n:s:\"v\"}", ""},
		{"? a\n: b\n? c\n: d\n", `{a:s:"b",c:s:"d"}`, ""},
		{"k:\n  ? a\n  : b\nm: 1\n", `{k:{a:s:"b"},m:i:1}`, ""},
		{"? 'a'\n: - b\n", `{a:[s:"b"]}`, ""},
		{"? a\n--- \nb: 1\n", "{a:n} {b:i:1}", ""},
		{"?\n  a\n: b\n", `{a:s:"b"}`, ""},
		{"? &x a\n  b\nc: !e 1\n", "RZ-CFG-003@1:3 RZ-CFG-004@3:4", ""},
		// Explicit keys in flow collections.
		{"k: {? !!str\n  x}\n", "{k:{x:n}}", ""},
		{"k: {p: q, ? !!str\n  x}\n", `{k:{p:s:"q",x:n}}`, ""},
		{"k: {p: q,\n ? !!str\n  x,\n  r: s}\n", `{k:{p:s:"q",x:n,r:s:"s"}}`, ""},
		{"k: {p: q,\n ? !!str\n  x\n  r: s}\n", `{k:{p:s:"q",x r:s:"s"}}`, ""},
		{"k: [p,\n ? !!str\n  x\n  r: s]\n", `{k:[s:"p",{x r:s:"s"}]}`, ""},
		{"{p: q,\n? !!str\n  x\n  r: s}\n", `{p:s:"q",x r:s:"s"}`, ""},
		{"k: {p: q,\n ? !!int\n  1}\n", `{k:{p:s:"q",1:n}}`, ""},
		{"k: {? !!str # c\n  x: v}\n", `{k:{x:s:"v"}}`, ""},
		{"k: {? !!str {a: b}}\n", "RZ-CFG-001@1:7", msgComplexKey},
		{"k: {? \"a\" \"b\": c}\n", "RZ-CFG-001@1:11", msgFlowMapNext},
		{"k: {? [a] b: c}\n", "RZ-CFG-001@1:7", msgComplexKey},
		{"k: {? ? a: b}\n", "RZ-CFG-001@1:7", msgExplicitPlace},
		{"k: {a: ? b}\n", "RZ-CFG-001@1:8", msgExplicitPlace},
		{"k: {? a: b}\nm: {? !e\n  x}\n", "RZ-CFG-004@2:7", ""},
		{"k: {? !!str x}\n", "{k:{x:n}}", ""},
		{"k: {p: q,\n ? x\n  r: s}\n", `{k:{p:s:"q",x r:s:"s"}}`, ""},
		{"k: {? !!str , a: 1}\n", "{k:{:n,a:i:1}}", ""},
		{"k: {? !!str\n  : v}\n", `{k:{:s:"v"}}`, ""},
		{"k: [? a: b, c]\n", `{k:[{a:s:"b"},s:"c"]}`, ""},
		{"k: {? !!int 1: a, ? 'b': c}\n", `{k:{1:s:"a",b:s:"c"}}`, ""},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" {
			var trees []string
			for _, d := range docs {
				trees = append(trees, show(d.Root, false))
			}
			got = strings.Join(trees, " ")
		}
		if got != tt.want {
			t.Errorf("%q: %q, want %q", tt.src, got, tt.want)
		}
		if _, diags := parseYAML(t, tt.src); tt.msg != "" && (len(diags) != 1 || diags[0].Message != tt.msg) {
			t.Errorf("%q: messages %+v, want %q", tt.src, diags, tt.msg)
		}
	}
}
