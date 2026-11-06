// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestParseTree checks typing, styles and positions: keys and values carry
// 1-based lines and code-point columns.
func TestParseTree(t *testing.T) {
	src := "# head\n" +
		"apiVersion: ruralz/v1alpha1\n" +
		"kind: Route\n" +
		"metadata:\n" +
		"  name: \"shop\"\n" +
		"spec:\n" +
		"  port: 0777\n" +
		"  ratio: .5\n" +
		"  on: yes\n" +
		"  é: ü\n" +
		"  tagged: !!str 0777\n" +
		"  empty:\n" +
		"  flow: [a, {b: 'c'}]\n" +
		"  list:\n" +
		"  - x\n" +
		"  - y: 1\n" +
		"    z: true\n" +
		"  lit: |\n" +
		"    text\n" +
		"  fold: >-\n" +
		"    a\n" +
		"    b\n"
	docs, diags := parseYAML(t, src)
	if len(diags) != 0 || len(docs) != 1 {
		t.Fatalf("diagnostics %q, %d documents", codes(diags), len(docs))
	}
	want := `{apiVersion@2:1:s:"ruralz/v1alpha1"@2:13,kind@3:1:s:"Route"@3:7,` +
		`metadata@4:1:{name@5:3:s:"shop"@5:9},` +
		`spec@6:1:{port@7:3:i:777@7:9,ratio@8:3:f:0.5@8:10,on@9:3:s:"yes"@9:7,é@10:3:s:"ü"@10:6,` +
		`tagged@11:3:s:"0777"@11:11,empty@12:3:n@12:9,` +
		`flow@13:3:[s:"a"@13:10,{b@13:14:s:"c"@13:17}],` +
		`list@14:3:[s:"x"@15:5,{y@16:5:i:1@16:8,z@17:5:b:true@17:8}],` +
		`lit@18:3:s:"text\n"@18:8,fold@20:3:s:"a b"@20:9}}`
	if got := show(docs[0].Root, true); got != want {
		t.Errorf("tree:\n got %s\nwant %s", got, want)
	}
	if docs[0].Start != (tree.Pos{Line: 2, Column: 1}) {
		t.Errorf("start = %+v", docs[0].Start)
	}
	spec, _ := docs[0].Root.Get("spec")
	styles := map[string]tree.Style{"port": tree.StylePlain, "lit": tree.StyleLiteral, "fold": tree.StyleFolded}
	for k, st := range styles {
		if v, _ := spec.Get(k); v.Style != st {
			t.Errorf("%s style = %d, want %d", k, v.Style, st)
		}
	}
	md, _ := docs[0].Root.Get("metadata")
	if v, _ := md.Get("name"); v.Style != tree.StyleDoubleQuoted {
		t.Errorf("name style = %d", v.Style)
	}
	flow, _ := spec.Get("flow")
	if v, _ := flow.Items[1].Get("b"); v.Style != tree.StyleSingleQuoted {
		t.Errorf("b style = %d", v.Style)
	}
	if flow.Pos != (tree.Pos{Line: 13, Column: 9}) || flow.Items[1].Pos != (tree.Pos{Line: 13, Column: 13}) {
		t.Errorf("flow positions %+v %+v", flow.Pos, flow.Items[1].Pos)
	}
}

// TestProfileFindingsCollected covers the profile's refusals: anchors,
// aliases and merge keys are RZ-CFG-003, other tags and %TAG RZ-CFG-004,
// all collected across the file, and a file with any of them yields no
// documents and no finding about its values.
func TestProfileFindingsCollected(t *testing.T) {
	src := "a: &x 1\n" + //       1:4 anchor
		"b: *x\n" + //            2:4 alias
		"c:\n" +
		"  <<: {d: 1}\n" + //     4:3 merge key
		"e: !include f.yaml\n" + // 5:4 tag
		"g: !!binary aGk=\n" + //  6:4 tag
		"dup: 1\n" +
		"dup: 2\n" + //           a duplicate key is not reported here
		"---\n" +
		"h: !env X\n" + //        10:4 tag
		"i: !<tag:yaml.org,2002:str> x\n" + // 11:4
		"j: !!timestamp 2001-12-14\n" + // 12:4
		"k: !!set {a}\n" + //     13:4
		"l: !!omap []\n" + //     14:4
		"...\n" +
		"%TAG !e! tag:example.com,2000:\n" + // 16:1
		"---\n" +
		"m: 1\n" +
		"---\n" +
		// Keys after an empty block scalar and a blank line.
		"x: |\n\n&y q: 1\n" + // 22:1
		"z: >-\n\n!e w: 1\n" + // 25:1
		"t: !f" // 26:4, a tag at the end of the input
	docs, diags := parseYAML(t, src)
	want := "RZ-CFG-003@1:4 RZ-CFG-003@2:4 RZ-CFG-003@4:3 RZ-CFG-004@5:4 RZ-CFG-004@6:4 " +
		"RZ-CFG-004@10:4 RZ-CFG-004@11:4 RZ-CFG-004@12:4 RZ-CFG-004@13:4 RZ-CFG-004@14:4 RZ-CFG-004@16:1 " +
		"RZ-CFG-003@22:1 RZ-CFG-004@25:1 RZ-CFG-004@26:4"
	if got := codes(diags); got != want {
		t.Errorf("diagnostics\n got %s\nwant %s", got, want)
	}
	if len(docs) != 0 {
		t.Errorf("%d documents, want none from a file with profile findings", len(docs))
	}
	for _, d := range diags[:3] {
		if !strings.Contains(d.Message, "not allowed") {
			t.Errorf("message %q", d.Message)
		}
	}
	if !strings.Contains(diags[0].Message, "&x") || !strings.Contains(diags[1].Message, "*x") || !strings.Contains(diags[3].Message, "!include") {
		t.Errorf("messages %q, %q, %q should name the anchor, alias and tag", diags[0].Message, diags[1].Message, diags[3].Message)
	}
}

// TestMergeKeys covers the merge key: a plain, untagged "<<" key, implicit
// or explicit, in block or flow context, is RZ-CFG-003 at it; a quoted or
// tagged "<<", a longer key and a "<<" value are strings.
func TestMergeKeys(t *testing.T) {
	tests := []struct{ src, want string }{
		{"<<: {a: 1}\n", "RZ-CFG-003@1:1"},
		{"<<:\tx\n", "RZ-CFG-003@1:1"},
		{"<< : x\n", "RZ-CFG-003@1:1"},
		{"? <<\n: x\n", "RZ-CFG-003@1:3"},
		{"- <<: x\n", "RZ-CFG-003@1:3"},
		{"a: {<<: b}\n", "RZ-CFG-003@1:5"},
		{"a: [<<: b]\n", "RZ-CFG-003@1:5"},
		{"a: {? <<}\n", "RZ-CFG-003@1:7"},
		{"a:\n  b: 1\n  <<: c\n", "RZ-CFG-003@3:3"},
		{"<<<: x\n", `{<<<:s:"x"}`},
		{"\"<<\": x\n", `{<<:s:"x"}`},
		{"'<<': x\n", `{<<:s:"x"}`},
		{"!!str <<: x\n", `{<<:s:"x"}`},
		{"a: <<\n", `{a:s:"<<"}`},
		{"a: [<<]\n", `{a:[s:"<<"]}`},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, false)
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
		if _, diags := parseYAML(t, tt.src); len(diags) == 1 && diags[0].Code == codeAnchor && diags[0].Message != "merge keys (<<) are not allowed" {
			t.Errorf("%q: message %q", tt.src, diags[0].Message)
		}
	}
}

// TestLongImplicitKey covers the length of an implicit key (YAML 1.2.2
// section 7.4: at most 1,024 Unicode characters, from its first character,
// a property or quote included, to its ':'): one more is RZ-CFG-001 at the
// key, in block context and in a flow sequence's pair, while an explicit
// key and a flow mapping's key have no such limit.
func TestLongImplicitKey(t *testing.T) {
	k := func(n int) string { return strings.Repeat("k", n) }
	tests := []struct{ name, src, want string }{
		{"plain", k(1024) + ": v\n", ""},
		{"plain past the limit", k(1025) + ": v\n", "RZ-CFG-001@1:1"},
		{"two-byte characters", strings.Repeat("é", 1024) + ": v\n", ""},
		{"two-byte characters past the limit", strings.Repeat("é", 1025) + ": v\n", "RZ-CFG-001@1:1"},
		{"quoted", "\"" + k(1022) + "\": v\n", ""},
		{"quoted past the limit", "\"" + k(1023) + "\": v\n", "RZ-CFG-001@1:1"},
		{"tagged", "!!str " + k(1018) + ": v\n", ""},
		{"tagged past the limit", "!!str " + k(1019) + ": v\n", "RZ-CFG-001@1:1"},
		{"space before the colon", k(1023) + " : v\n", ""},
		{"space before the colon past the limit", k(1024) + " : v\n", "RZ-CFG-001@1:1"},
		{"nested", "a:\n  " + k(1024) + ": v\n", ""},
		{"nested past the limit", "a:\n  " + k(1025) + ": v\n", "RZ-CFG-001@2:3"},
		{"flow pair", "a: [" + k(1024) + ": v]\n", ""},
		{"flow pair past the limit", "a: [" + k(1025) + ": v]\n", "RZ-CFG-001@1:5"},
		{"explicit key", "? " + k(5000) + "\n: v\n", ""},
		{"flow mapping key", "a: {" + k(5000) + ": v}\n", ""},
		{"flow explicit key", "a: [? " + k(5000) + ": v]\n", ""},
	}
	for _, tt := range tests {
		_, diags := parseYAML(t, tt.src)
		if got := codes(diags); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
			continue
		}
		if tt.want != "" && diags[0].Message != msgLongKey {
			t.Errorf("%s: message %q", tt.name, diags[0].Message)
		}
	}
}

// TestFinalLineBreak covers a text that does not end with a line break:
// Parse reads it as if it did, as the YAML Test Suite reads such a stream,
// so a tag at the end of the input is still reported and the trailing
// spaces of a block scalar's last line are kept, and no position moves.
func TestFinalLineBreak(t *testing.T) {
	tests := []struct{ src, want string }{
		{"k: !foo", "RZ-CFG-004@1:4"},
		{"k: [!foo]", "RZ-CFG-004@1:5"},
		{"k: !foo\n", "RZ-CFG-004@1:4"},
		{"k: !!str", `{k@1:1:s:""@1:4}`},
		{"a: |\n  x  ", `{a@1:1:s:"x  \n"@1:4}`},
		{"a: |-\n  x  ", `{a@1:1:s:"x  "@1:4}`},
		{"k: v", `{k@1:1:s:"v"@1:4}`},
		{"k: 'v'", `{k@1:1:s:"v"@1:4}`},
		// The null after the ':', in the column after it, wherever the
		// input ends.
		{"k:", `{k@1:1:n@1:3}`},
		{"k: v\r", `{k@1:1:s:"v"@1:4}`},
	}
	for _, tt := range tests {
		docs, diags := parseYAML(t, tt.src)
		got := codes(diags)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, true)
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
	}
}

// TestCoreTagsAccepted covers the seven core tags, which force types.
func TestCoreTagsAccepted(t *testing.T) {
	src := "a: !!str 0777\n" +
		"b: !!int \"12\"\n" +
		"c: !!float 1\n" +
		"d: !!bool true\n" +
		"f: !!map {x: 1}\n" +
		"g: !!seq [1]\n" +
		"!!str k: v\n" +
		"i: !!str |\n" +
		"  0777\n" +
		"e: !!null\n" +
		"---\n" +
		"h: !!str\n"
	docs, diags := parseYAML(t, src)
	if len(diags) != 0 || len(docs) != 2 {
		t.Fatalf("diagnostics %q, %d documents", codes(diags), len(docs))
	}
	want := `{a:s:"0777",b:i:12,c:f:1,d:b:true,f:{x:i:1},g:[i:1],k:s:"v",i:s:"0777\n",e:n}`
	if got := show(docs[0].Root, false); got != want {
		t.Errorf("tree:\n got %s\nwant %s", got, want)
	}
	if got := show(docs[1].Root, false); got != `{h:s:""}` {
		t.Errorf("empty !!str: %s", got)
	}
	if a, _ := docs[0].Root.Get("a"); a.Pos != (tree.Pos{Line: 1, Column: 4}) {
		t.Errorf("a tagged scalar starts at its tag, got %+v", a.Pos)
	}
}

// TestTagMismatch covers a tagged value that does not match its tag:
// RZ-CFG-001 at the tag, for scalars and collections alike.
func TestTagMismatch(t *testing.T) {
	tests := []struct {
		src       string
		code      string
		line, col int
	}{
		{"a: !!int x\n", codeParse, 1, 4},
		{"a: !!bool yes\n", codeParse, 1, 4},
		{"a: !!null 0\n", codeParse, 1, 4},
		{"a: !!map [1]\n", codeParse, 1, 4},
		{"a: !!seq {b: 1}\n", codeParse, 1, 4},
		{"a: !!str {b: 1}\n", codeParse, 1, 4},
		{"a: !!int\n", codeParse, 1, 4},
		{"a: !!map\n", codeParse, 1, 4},
		{"a: !!int |\n  x\n", codeParse, 1, 4},
		{"a: !!float .inf\n", codeSchema, 1, 4},
	}
	for _, tt := range tests {
		docs, diags := parseYAML(t, tt.src)
		wantOne(t, diags, tt.code, tt.line, tt.col)
		if len(docs) != 0 {
			t.Errorf("%q: %d documents", tt.src, len(docs))
		}
	}
}

// TestTagChains covers a node with more than one tag: RZ-CFG-001 "a node
// has at most one tag" at the second tag, on one line or over two, in block
// and flow context, while a tag on a later line that starts an implicit key
// belongs to that key, the key of a tagged mapping, and is valid. A chain
// of any length stops at its second tag, so its cost is that of its first
// two.
func TestTagChains(t *testing.T) {
	tests := []struct{ src, want string }{
		{"a: !!str !!str x\n", "RZ-CFG-001@1:10"},
		{"a: !!str # c\n  !!str x\n", "RZ-CFG-001@2:3"},
		{"a: !!str\n  !!int 1\n", "RZ-CFG-001@2:3"},
		{"a: [!!str !!str x]\n", "RZ-CFG-001@1:11"},
		{"a: !e !!str x\n", "RZ-CFG-004@1:4 RZ-CFG-001@1:7"},
		{"a: &x !!str y\n", "RZ-CFG-003@1:4"},
		{"a: !!str &x !!str y\n", "RZ-CFG-003@1:10 RZ-CFG-001@1:13"},
		{"a: !!str x\nb: !!str y\n", `{a:s:"x",b:s:"y"}`},
		{"--- !!map\na: 1\n--- !!map\nb: 2\n", ""},
		// The key of a tagged mapping.
		{"spec: !!map\n  !!str name: x\n", `{spec:{name:s:"x"}}`},
		{"--- !!map\n!!str a: 1\n", `{a:i:1}`},
		{"!!map\n!!str a: 1\n", `{a:i:1}`},
		{"a: !!map # c\n  !!str b: 1\n  !!str c: 2\n", `{a:{b:i:1,c:i:2}}`},
		{"a: !!map\n  !!str \"k\": v\n", `{a:{k:s:"v"}}`},
		{"a: !!map\n  !!str : 1\n", `{a:{:i:1}}`},
		{"a: !!map\n  !!str b: !!map\n    !!str c: 2\n", `{a:{b:{c:i:2}}}`},
		{"a: !!map\n  !!str &x k: v\n", "RZ-CFG-003@2:9"},
		// Not a key: the second tag is the same node's.
		{"!!map\n!!map\n!!str a: 1\n", "RZ-CFG-001@2:1"},
		{"a: !!map\n  !!str\n  k: v\n", "RZ-CFG-001@2:3"},
		{"a: !!map\n  !!str k\n  : v\n", "RZ-CFG-001@2:3"},
		{"a: !!map\n  !!str k:v\n", "RZ-CFG-001@2:3"},
		{"a: !!map !!str k: v\n", "RZ-CFG-001@1:10"},
		{"a: [!!str\n  !!str x: y]\n", "RZ-CFG-001@2:3"},
		{"a: {!!str\n  !!str x: y}\n", "RZ-CFG-001@2:3"},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if len(docs) == 1 && got == "" {
			got = show(docs[0].Root, false)
		}
		if got != tt.want {
			t.Errorf("%q: %q, want %q", tt.src, got, tt.want)
		}
		_, diags := parseYAML(t, tt.src)
		for _, d := range diags {
			if d.Code == codeParse && d.Message != msgTagChain {
				t.Errorf("%q: message %q", tt.src, d.Message)
			}
		}
	}
	// Chains of 512 KiB and 1 MiB stop at their second tag.
	for _, size := range []int{1 << 19, 1 << 20} {
		src := "a: " + strings.Repeat("!!str ", size/6) + "x\n"
		var diags diag.List
		peak := peakHeap(func() { _, diags = parseYAML(t, src) })
		if d := wantOne(t, diags, codeParse, 1, 10); d.Message != msgTagChain {
			t.Errorf("%d-byte tag chain: message %q", size, d.Message)
		}
		if peak > 16<<20 {
			t.Errorf("%d-byte tag chain: peak heap and stack growth %d MiB", size, peak>>20)
		}
	}
}

// TestPropertiesBeforeNode covers properties on the lines before their
// node: a flow collection or an alias below its tag takes it as one on its
// own line does, so a core tag it does not match is RZ-CFG-001 at the tag,
// the node starts at the tag, and an alias with a tag is refused.
func TestPropertiesBeforeNode(t *testing.T) {
	tests := []struct {
		src, want string
		at        tree.Pos
	}{
		{"!!map\n{}\n", "{}", tree.Pos{Line: 1, Column: 1}},
		{"--- !!map\n{b: 1}\n", "{b:i:1}", tree.Pos{Line: 1, Column: 5}},
		{"--- # c\n!!map\n{b: 1}\n", "{b:i:1}", tree.Pos{Line: 2, Column: 1}},
		{"!!seq\n{}\n", "RZ-CFG-001@1:1", tree.Pos{}},
		{"!!str\n[a]\n", "RZ-CFG-001@1:1", tree.Pos{}},
		{"a: !!map\n  {b: 1}\n", "{a:{b:i:1}}", tree.Pos{Line: 1, Column: 4}},
		{"a: !!seq\n  {b: 1}\n", "RZ-CFG-001@1:4", tree.Pos{}},
		{"a: !!seq # c\n  # d\n  [x]\n", "{a:[s:\"x\"]}", tree.Pos{Line: 1, Column: 4}},
		{"- !!seq\n  [x]\n", "[[s:\"x\"]]", tree.Pos{Line: 1, Column: 3}},
		{"- !!map\n  [x]\n", "RZ-CFG-001@1:3", tree.Pos{}},
		{"? !!str\n  [x]\n", "RZ-CFG-001@1:3", tree.Pos{}},
		{"a: !!map\n  !!map {}\n", "RZ-CFG-001@2:3", tree.Pos{}},
		{"a: !!str\n  *x\n", "RZ-CFG-001@2:3", tree.Pos{}},
		{"a: &y\n  *x\n", "RZ-CFG-003@1:4 RZ-CFG-001@2:3", tree.Pos{}},
		{"a: !!int\n  \"x\"\n", "RZ-CFG-001@1:4", tree.Pos{}},
		{"a: !!str\n  \"x\"\n", "{a:s:\"x\"}", tree.Pos{Line: 1, Column: 4}},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got != "" || len(docs) != 1 {
			if got != tt.want {
				t.Errorf("%q: %q, want %q", tt.src, got, tt.want)
			}
			continue
		}
		root := docs[0].Root
		if got := show(root, false); got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
		node := root
		switch {
		case root.Kind == tree.KindMap && len(root.Members) == 1 && root.Members[0].Key == "a":
			node = root.Members[0].Value
		case root.Kind == tree.KindList && len(root.Items) == 1:
			node = root.Items[0]
		}
		if node.Pos != tt.at {
			t.Errorf("%q: the node is at %+v, want %+v", tt.src, node.Pos, tt.at)
		}
	}
}

// TestTaggedEmptyNode covers a tag whose node is empty: it types the empty
// scalar, so !!str gives "" and !!null null, while !!map and !!seq need
// content, "a tagged node has no value" at the tag, and another scalar tag
// does not match it. A node on a later line belongs to the tag only when it
// is indented past the entry, or is a block sequence at a mapping key's
// column.
func TestTaggedEmptyNode(t *testing.T) {
	tests := []struct{ src, want, msg string }{
		{"a: !!map\nb: 1\n", "RZ-CFG-001@1:4", msgNoValue},
		{"a: !!map # c\nb: 1\n", "RZ-CFG-001@1:4", msgNoValue},
		{"a: !!map\n# c\n\nb: 1\n", "RZ-CFG-001@1:4", msgNoValue},
		{"? a\n: !!map\nb: 1\n", "RZ-CFG-001@2:3", msgNoValue},
		{"!!str a: !!map\n!!str b: 1\n", "RZ-CFG-001@1:10", msgNoValue},
		{"a: !!map\n!!str b: 1\n", "RZ-CFG-001@1:4", msgNoValue},
		{"a:\n  !!map\nb: 1\n", "RZ-CFG-001@2:3", msgNoValue},
		{"a: !!map\n  b: !!map\n  c: 1\n", "RZ-CFG-001@2:6", msgNoValue},
		{"a: !!map\n? b\n: 1\n", "RZ-CFG-001@1:4", msgNoValue},
		{"a: !!map\n: 1\n", "RZ-CFG-001@1:4", msgNoValue},
		{"a: !!seq\n- !!seq\n- x\n", "RZ-CFG-001@2:3", msgNoValue},
		{"- !!seq\n- x\n", "RZ-CFG-001@1:3", msgNoValue},
		// The empty node's finding comes before a syntax error later in
		// its document.
		{"- !!map\na: 1\n", "RZ-CFG-001@1:3 RZ-CFG-001@2:1", msgNoValue},
		{"a:\n  - !!map\n  b: 1\n", "RZ-CFG-001@2:5 RZ-CFG-001@3:3", msgNoValue},
		{"a:\n  - !!seq\n- x\n", "RZ-CFG-001@2:5 RZ-CFG-001@3:1", msgNoValue},
		{"x:\n  a: !!seq\n- y\n", "RZ-CFG-001@2:6 RZ-CFG-001@3:1", msgNoValue},
		// An empty !!str is "": the next line at the mapping's column is
		// a key without ':'.
		{"0: !!str\n0\n -\n", "RZ-CFG-001@2:1", msgNoColon},
		{"0: !!str\n0\n -\n# c\n", "RZ-CFG-001@2:1", msgNoColon},
		// Profile findings, with which the values are not checked.
		{"a: !e\nb: 1\n", "RZ-CFG-004@1:4", ""},
		{"a: !e\nb: !f\nc: &x 1\n", "RZ-CFG-004@1:4 RZ-CFG-004@2:4 RZ-CFG-003@3:4", ""},
		{"a: &x !!map\nb: 1\nc: !e 1\n", "RZ-CFG-003@1:4 RZ-CFG-004@3:4", ""},
		// The node has content, or the tag needs none.
		{"? a\n: !!str\n? b\n", `{a:s:"",b:n}`, ""},
		{"? !!str\n? b\n", `{:n,b:n}`, ""},
		{"a: !!seq\n- x\n", `{a:[s:"x"]}`, ""},
		{"0: !!str 0\n -\n...\n", `{0:s:"0 -"}`, ""},
		{"0: !!str\n # c\n 0\n  -\n", `{0:s:"0 -"}`, ""},
		{"k: !!str a\n  -b\n\nz: 1\n", `{k:s:"a -b",z:i:1}`, ""},
		{"? a\n: !!seq\n- x\n", `{a:[s:"x"]}`, ""},
		{"- a: !!seq\n  - b\n  c: 1\n", `[{a:[s:"b"],c:i:1}]`, ""},
		{"x:\n  a: !!seq\n  - y\n", `{x:{a:[s:"y"]}}`, ""},
		{"--- !!map\na: 1\n", `{a:i:1}`, ""},
		{"!!map\na: 1\n", `{a:i:1}`, ""},
		{"a: !!map\n  b: 1\n", `{a:{b:i:1}}`, ""},
		{"a:\n  !!map\n  b: 1\n", `{a:{b:i:1}}`, ""},
		{"- !!map\n  a: 1\n", `[{a:i:1}]`, ""},
		{"? !!str\n: v\n", `{:s:"v"}`, ""},
		{"a: !!null # c\n# d\n", `{a:n}`, ""},
		{"a: !!null\n...\n", `{a:n}`, ""},
		{"a: !!null\n---\nb: 1\n", `{a:n} {b:i:1}`, ""},
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
		if _, diags := parseYAML(t, tt.src); tt.msg != "" && (len(diags) == 0 || diags[0].Message != tt.msg) {
			t.Errorf("%q: first finding %q, want the message %q", tt.src, codes(diags), tt.msg)
		}
	}
	// A ladder of such entries (taggedLadder) gives one finding per empty
	// mapping, up to the diagnostics cap, in linear time and memory.
	for _, rows := range []int{255, 256} {
		for _, cols := range []int{1, 63} {
			src := taggedLadder(cols, rows, 0)
			var diags diag.List
			peak := peakHeap(func() { _, diags = parseYAML(t, src) })
			if len(diags) == 0 || codes(diags[:1]) != "RZ-CFG-001@1:5" {
				t.Errorf("ladder of %d columns of %d lines: %q", cols, rows, codes(diags))
				continue
			}
			for _, d := range diags {
				if d.Code != codeParse || d.Message != msgNoValue {
					t.Errorf("ladder of %d columns of %d lines: finding %s %q", cols, rows, d.Code, d.Message)
					break
				}
			}
			if peak > 64<<20 {
				t.Errorf("ladder of %d columns of %d lines: peak heap growth %d MiB", cols, rows, peak>>20)
			}
		}
	}
}

// TestMultiLineImplicitKey covers implicit keys, which YAML 1.2 keeps on
// one line (YAML 1.2.2 section 7.4): a ':' on a later line after a scalar
// is "an implicit key must be on one line", and a flow collection before a
// ':' is a key that is not a scalar. An entry with an empty key (": v") is
// valid YAML 1.2, and two in one mapping are duplicate keys. An explicit
// key ('?') may span lines when it is one node; a line indented past it is
// refused.
func TestMultiLineImplicitKey(t *testing.T) {
	var wide strings.Builder
	for i := range 300 {
		wide.WriteString("w" + itoa(i) + ": v\n")
	}
	tests := []struct{ src, want, msg string }{
		{"\"x\"\n: y\n", "RZ-CFG-001@2:1", msgMultiLineKey},
		{"- >\n:\n", "RZ-CFG-001@2:1", msgMultiLineKey},
		{"- >\n  - x\n:\n", "RZ-CFG-001@3:1", msgMultiLineKey},
		{"!!str |\n  k\n: v\n", "RZ-CFG-001@3:1", msgMultiLineKey},
		{"a: 1\n b: 2\n", "RZ-CFG-001@2:3", msgMultiLineKey},
		// A quoted key over lines, in a block mapping and in a flow pair,
		// with its tag on its first line or none (YAML Test Suite D49Q,
		// JKF3).
		{"'c\n d': 1\n", "RZ-CFG-001@2:4", msgMultiLineKey},
		{"- [\"a\n  b\": c]\n", "RZ-CFG-001@2:5", msgMultiLineKey},
		{"- [!!str 'a\n  b': c]\n", "RZ-CFG-001@2:5", msgMultiLineKey},
		{"[a,\n b]: c\n", "RZ-CFG-001@1:1", msgComplexKey},
		{"a:\n  - [\n  ]\n  :\n", "RZ-CFG-001@3:3", msgFlowClose},
		{"a:\n  - - \"\"\n  :\n", "RZ-CFG-001@3:3", msgIndentMoreMap},
		{"  !!str\n     :\n    k0:\n", "RZ-CFG-001@1:3 RZ-CFG-001@3:5", "value does not match tag !!str"},
		// Entries with an empty key.
		{"a:\n  - \"x\"\n:\n", `{a:[s:"x"],:n}`, ""},
		{"a:\n  - 'x'\n:\n", `{a:[s:"x"],:n}`, ""},
		{"a:\n  - >\n:\n", `{a:[s:""],:n}`, ""},
		{"a:\n  b:\n    - \"x\"\n  :\n", `{a:{b:[s:"x"],:n}}`, ""},
		{"a: 1\n: y\n", `{a:i:1,:s:"y"}`, ""},
		{": a\n: b\n", "RZ-CFG-002@2:1", `duplicate key ""`},
		{"a: &x \"y\"\n: z\nb: !e 1\n", "RZ-CFG-003@1:4 RZ-CFG-004@3:4", ""},
		// An explicit key of one node, then a line indented past it, or
		// a ':' not at its column.
		{"? \"x\"\n  : y\n", "RZ-CFG-001@2:3", msgIndentMoreMap},
		{"? \"a\"\n  - \"b\"\n:\n", "RZ-CFG-001@2:3", msgIndentMoreMap},
		{"? \"a\"\n  \"b\"\n:\n", "RZ-CFG-001@2:3", msgIndentMoreMap},
		{"? \"a\"\n  !!str\n:\n", "RZ-CFG-001@2:3", msgIndentMoreMap},
		{"x:\n  ? \"a\"\n    - \"b\"\n  :\n", "RZ-CFG-001@3:5", msgIndentMoreMap},
		{"- ? \"a\"\n    - \"b\"\n  :\n", "RZ-CFG-001@2:5", msgIndentMoreMap},
		{explicitKeyLadder(31, 32), "RZ-CFG-001@2:3", msgIndentMoreMap},
		{explicitKeyLadder(-1, 62), "RZ-CFG-001@2:3", msgIndentMoreMap},
		{wide.String() + explicitKeyLadder(31, 32), "RZ-CFG-001@302:3", msgIndentMoreMap},
		// A second node or a ':' on the '?' line; "? k:" is the complex
		// key {k: null}.
		{"? \"q\"k\n", "RZ-CFG-001@1:6", msgAfterNode},
		{"a:\n  ? \"q\"''\n", "RZ-CFG-001@2:8", msgAfterNode},
		{"? k:\n", "RZ-CFG-001@1:3", msgComplexKey},
		{"? : v\n", "RZ-CFG-001@1:3", msgComplexKey},
		{"? [a] b\n: c\n", "RZ-CFG-001@1:3", msgComplexKey},
		{"? - a\n  - b\n: c\n", "RZ-CFG-001@1:3", msgComplexKey},
		// Valid: an explicit key's ':' after one node, and keys on one
		// line.
		{"? \"x\"\n: y\n", `{x:s:"y"}`, ""},
		{"? |\n  k\n: v\n", "{k\n:s:\"v\"}", ""},
		{"? >\n: v\n", `{:s:"v"}`, ""},
		{"? !!str\n: v\n", `{:s:"v"}`, ""},
		{"? !!str \"a\"\n: v\n", `{a:s:"v"}`, ""},
		{"? !!str\n  \"a\"\n: v\n", `{a:s:"v"}`, ""},
		{"? a\n  b\n: v\n", `{a b:s:"v"}`, ""},
		{"? \"a\" # c\n# d\n: v\n", `{a:s:"v"}`, ""},
		{"- ? a\n  : b\n", `[{a:s:"b"}]`, ""},
		{"\"x\": y\n", `{x:s:"y"}`, ""},
		{"a:\n  - \"x\"\nb: 1\n", `{a:[s:"x"],b:i:1}`, ""},
		{"a: |\n  t\nb: 1\n", `{a:s:"t\n",b:i:1}`, ""},
		{"a: !!str |\n  t\nb: 1\n", `{a:s:"t\n",b:i:1}`, ""},
		{"a: [x,\n  y]\nb: 1\n", `{a:[s:"x",s:"y"],b:i:1}`, ""},
		{"a: \"x\n  y\"\nb: 1\n", `{a:s:"x y",b:i:1}`, ""},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, false)
		}
		if got != tt.want {
			t.Errorf("%q: %q, want %q", tt.src, got, tt.want)
		}
		if _, diags := parseYAML(t, tt.src); tt.msg != "" && (len(diags) == 0 || diags[0].Message != tt.msg) {
			t.Errorf("%q: first finding %q, want the message %q", tt.src, codes(diags), tt.msg)
		}
	}
	// The key ladder (keyLadder) is a run of entries with an empty key,
	// each value the sequence below it: duplicate keys, in linear memory.
	for _, dashes := range []int{16, 63} {
		for _, reps := range []int{254, 256} {
			src := keyLadder(dashes, reps)
			var diags diag.List
			peak := peakHeap(func() { _, diags = parseYAML(t, src) })
			if len(diags) != reps-1 || codes(diags[:1]) != "RZ-CFG-002@5:1" {
				t.Errorf("ladder of %d dashes, %d repetitions: %d findings, first %q", dashes, reps, len(diags), codes(diags[:min(1, len(diags))]))
			}
			if peak > 64<<20 {
				t.Errorf("ladder of %d dashes, %d repetitions (%d bytes): peak heap growth %d MiB", dashes, reps, len(src), peak>>20)
			}
		}
	}
}

// TestColumnsAfterTags covers positions on a line after tags: keys and
// values carry their real columns, and a tagged node starts at its tag.
func TestColumnsAfterTags(t *testing.T) {
	tests := []struct{ src, want string }{
		{"!!str a: !!int 1\n", `{a@1:1:i:1@1:10}`},
		{"x: [!!str a, !!str b, c]\n", `{x@1:1:[s:"a"@1:5,s:"b"@1:14,s:"c"@1:23]}`},
		{"x: {!!str a: !!str b, c: d}\n", `{x@1:1:{a@1:5:s:"b"@1:14,c@1:23:s:"d"@1:26}}`},
		{"- !!str a\n- b: !!str c\n  d: e\n", `[s:"a"@1:3,{b@2:3:s:"c"@2:6,d@3:3:s:"e"@3:6}]`},
		{"{!!str a: 1, a: 2}\n", "RZ-CFG-002@1:14"},
		{"a: [!!str x, &y z]\n", "RZ-CFG-003@1:14"},
		{"a: [!!str x, !!int y]\n", "RZ-CFG-001@1:14"},
		{"a: [!e x, !f y]\n", "RZ-CFG-004@1:5 RZ-CFG-004@1:11"},
		// Scalars over several lines after a tag on their first line: a
		// plain scalar continued by a line starting with '-', and quoted
		// ones.
		{"k: !!str a\n  -b # c\n", `{k@1:1:s:"a -b"@1:4}`},
		{"!!str k: \"a\\ \n  b\"\n", `{k@1:1:s:"a  b"@1:10}`},
		{"!!str k: 'a\t\n  b'\n", `{k@1:1:s:"a b"@1:10}`},
		{"!!str k: a\n  -b # c\n", `{k@1:1:s:"a -b"@1:10}`},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, true)
		}
		if got != tt.want {
			t.Errorf("%q: %q, want %q", tt.src, got, tt.want)
		}
	}
}

// TestFlowDashes covers "-" in flow context: a block sequence entry inside
// a flow collection is RZ-CFG-001, as is a plain scalar "-" before a flow
// indicator; "-" before ':', and scalars that start with '-', are valid.
func TestFlowDashes(t *testing.T) {
	tests := []struct{ src, want, msg string }{
		{"a: [- x]\n", "RZ-CFG-001@1:5", msgFlowDash},
		{"a: {k: - x}\n", "RZ-CFG-001@1:8", msgFlowDash},
		{"a: [x, -\n]\n", "RZ-CFG-001@1:8", msgFlowDash},
		{"a: [\n  - x\n]\n", "RZ-CFG-001@2:3", msgFlowDash},
		{"a: {\"\":-}\n", "RZ-CFG-001@1:8", msgDashAlone},
		{"a: [-[x]]\n", "RZ-CFG-001@1:5", msgDashAlone},
		{"a: [-]\n", "RZ-CFG-001@1:5", msgDashAlone},
		{"a: [-, -]\n", "RZ-CFG-001@1:5", msgDashAlone},
		{"a: [x, -]\n", "RZ-CFG-001@1:8", msgDashAlone},
		{"a: {k: -}\n", "RZ-CFG-001@1:8", msgDashAlone},
		{"a: [-: x]\n", `{a:[{-:s:"x"}]}`, ""},
		{"a: {-: x}\n", `{a:{-:s:"x"}}`, ""},
		{"a: [-1, -x, \"-\", '-', -.5, a -, -#]\n", `{a:[i:-1,s:"-x",s:"-",s:"-",f:-0.5,s:"a -",s:"-#"]}`, ""},
		{"a:\n- [x]\n- - y\n", `{a:[[s:"x"],[s:"y"]]}`, ""},
	}
	for _, tt := range tests {
		docs, diags := parseYAML(t, tt.src)
		got := codes(diags)
		if len(docs) == 1 {
			got = show(docs[0].Root, false)
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
		if tt.msg != "" && len(diags) > 0 && diags[0].Message != tt.msg {
			t.Errorf("%q: message %q, want %q", tt.src, diags[0].Message, tt.msg)
		}
	}
	// A run of dashes stops at the first one.
	for _, tt := range []struct {
		src  string
		line int
		col  int
	}{
		{"a: [" + strings.Repeat("- ", 16000) + "x]\n", 1, 5},
		{"a: {k: " + strings.Repeat("- ", 4000) + "x}\n", 1, 8},
	} {
		before := allocBytes()
		_, diags := parseYAML(t, tt.src)
		if d := wantOne(t, diags, codeParse, tt.line, tt.col); d.Message != msgFlowDash {
			t.Errorf("message %q", d.Message)
		}
		if got := allocBytes() - before; got > 16<<20 {
			t.Errorf("%d bytes of dashes allocate %d MiB", len(tt.src), got>>20)
		}
	}
}

// TestDirectives covers %YAML and reserved directives: %YAML 1.2 and
// reserved directives pass, another %YAML version is RZ-CFG-001 and stops
// the file, and directives need a "---" after them.
func TestDirectives(t *testing.T) {
	docs, diags := parseYAML(t, "%YAML 1.2\n---\na: 1\n")
	if len(diags) != 0 || len(docs) != 1 || docs[0].Start != (tree.Pos{Line: 2, Column: 1}) {
		t.Fatalf("%%YAML 1.2: diagnostics %q, documents %+v", codes(diags), docs)
	}
	docs, diags = parseYAML(t, "%FOO bar baz\n---\na: 1\n")
	if len(diags) != 0 || len(docs) != 1 {
		t.Fatalf("reserved directive: diagnostics %q, %d documents", codes(diags), len(docs))
	}
	// %YAML 1.1 is RZ-CFG-001 and stops the file: the later anchor is not
	// reported.
	_, diags = parseYAML(t, "a: 1\n...\n%YAML 1.1\n---\nb: &x 1\n")
	d := wantOne(t, diags, codeParse, 3, 1)
	if !strings.Contains(d.Message, "%YAML 1.1") {
		t.Errorf("message %q", d.Message)
	}
	_, diags = parseYAML(t, "%YAML\n---\na: 1\n")
	wantOne(t, diags, codeParse, 1, 1)
	tests := []struct{ src, want, msg string }{
		{"%YAML 1.2\n%YAML 1.2\n---\na: 1\n", "RZ-CFG-001@2:1", msgYAMLTwice},
		{"%YAML 1.2 1.1\n---\na: 1\n", "RZ-CFG-001@1:1", msgYAMLParams},
		{"%YAML 1.2\na: 1\n", "RZ-CFG-001@1:1", msgDirectivesEnd},
		{"%FOO\n...\n", "RZ-CFG-001@1:1", msgDirectivesEnd},
		{"%\n---\na: 1\n", "RZ-CFG-001@1:1", msgDirectiveName},
		// After a document ended by "---", a '%' line is content.
		{"a: 1\n---\n%YAML 1.2\n---\nb: 1\n", "RZ-CFG-001@3:1", msgPercent},
		{"%YAML 1.2 # c\n---\na: 1\n", "", ""},
	}
	for _, tt := range tests {
		_, diags := parseYAML(t, tt.src)
		if got := codes(diags); got != tt.want || (tt.msg != "" && diags[0].Message != tt.msg) {
			t.Errorf("%q: %q %v, want %q %q", tt.src, got, diags, tt.want, tt.msg)
		}
	}
}

// TestInvalidCharacterEndsDocument covers a character that starts no node:
// RZ-CFG-001 at it, which leaves its document out.
func TestInvalidCharacterEndsDocument(t *testing.T) {
	_, diags := parseYAML(t, "a: 1\nb: @x\nc: &y 1\n")
	d := wantOne(t, diags, codeParse, 2, 4)
	if !strings.Contains(d.Message, "reserved") {
		t.Errorf("message %q", d.Message)
	}
	_, diags = parseYAML(t, "a: \"unterminated\n")
	if len(diags) != 1 || diags[0].Code != codeParse || diags[0].Line != 1 {
		t.Errorf("unterminated quote: %q", codes(diags))
	}
}

// TestDepth covers the depth count: the root mapping is depth 1, 64
// levels pass and 65 is RZ-CFG-001.
func TestDepth(t *testing.T) {
	for _, style := range []string{"block map", "block seq", "compact seq", "flow", "mixed"} {
		for _, depth := range []int{64, 65} {
			src := nest(style, depth)
			_, diags, err := parse(context.Background(), []byte(src), 0, "f.yaml", FormatYAML, Options{}, true)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case depth == 64 && len(diags) != 0:
				t.Errorf("%s depth 64: %q", style, codes(diags))
			case depth == 65 && (len(diags) != 1 || diags[0].Code != codeParse || !strings.Contains(diags[0].Message, "nesting depth exceeds 64")):
				t.Errorf("%s depth 65: %q", style, codes(diags))
			}
		}
	}
	// The failure points at the token opening level 65.
	_, diags := parseYAML(t, nest("flow", 65))
	wantOne(t, diags, codeParse, 1, 4+63)
	_, diags = parseYAML(t, nest("compact seq", 65))
	wantOne(t, diags, codeParse, 1, 1+2*64)
}

// TestDepthOptions honors a configured limit.
func TestDepthOptions(t *testing.T) {
	src := []byte("a:\n  b:\n    c: 1\n")
	for limit, want := range map[int]int{2: 1, 3: 0} {
		_, diags, err := Parse(context.Background(), src, 0, "f", FormatYAML, Options{MaxDepth: limit})
		if err != nil || len(diags) != want {
			t.Errorf("MaxDepth %d: %q, %v", limit, codes(diags), err)
		}
	}
}

// TestDepthNotInflated checks constructs the literal stack rule would count
// again at every entry: sequences at their key's indentation, tagged keys
// and explicit keys.
func TestDepthNotInflated(t *testing.T) {
	var b strings.Builder
	for i := range 100 {
		b.WriteString("k" + strconv.Itoa(i) + ":\n- a\n- b: 1\n  c:\n  - d\n")
	}
	for i := range 100 {
		b.WriteString("!!str t" + strconv.Itoa(i) + ":\n  x: 1\n")
	}
	for i := range 100 {
		b.WriteString("? e" + strconv.Itoa(i) + "\n: v\n")
	}
	docs, diags := parseYAML(t, b.String())
	if len(diags) != 0 || len(docs) != 1 || len(docs[0].Root.Members) != 300 {
		t.Fatalf("diagnostics %q, %d documents", codes(diags), len(docs))
	}
}

// TestDepthResetsPerDocument checks that "---" resets the stack.
func TestDepthResetsPerDocument(t *testing.T) {
	doc := nest("block map", 64)
	docs, diags := parseYAML(t, doc+"---\n"+doc+"---\n"+doc)
	if len(diags) != 0 || len(docs) != 3 {
		t.Fatalf("diagnostics %q, %d documents", codes(diags), len(docs))
	}
}

// TestDuplicateKeys covers duplicate keys: RZ-CFG-002 at the second key
// with the first as related location, every duplicate reported, keys
// compared as written (1 equals "1").
func TestDuplicateKeys(t *testing.T) {
	src := "a: 1\n" +
		"b:\n" +
		"  x: 1\n" +
		"  'x': 2\n" +
		"a: 3\n" +
		"\"a\": 4\n" +
		"1: one\n" +
		"\"1\": uno\n" +
		"f: {p: 1, p: 2}\n"
	docs, diags := parseYAML(t, src)
	want := "RZ-CFG-002@4:3 RZ-CFG-002@5:1 RZ-CFG-002@6:1 RZ-CFG-002@8:1 RZ-CFG-002@9:11"
	if got := codes(diags); got != want {
		t.Fatalf("diagnostics\n got %s\nwant %s", got, want)
	}
	related := []string{"f.yaml:3:3", "f.yaml:1:1", "f.yaml:1:1", "f.yaml:7:1", "f.yaml:9:5"}
	for i, d := range diags {
		if len(d.Related) != 1 || d.Related[0].Message != "first defined at" {
			t.Fatalf("diagnostic %d related %+v", i, d.Related)
		}
		r := d.Related[0]
		if got := r.File + ":" + strconv.Itoa(r.Line) + ":" + strconv.Itoa(r.Column); got != related[i] {
			t.Errorf("diagnostic %d related %s, want %s", i, got, related[i])
		}
	}
	if len(docs) != 0 {
		t.Errorf("a document with duplicates is left out")
	}
	var b strings.Builder
	_ = diag.WriteText(&b, diags[:1])
	if !strings.Contains(b.String(), `duplicate key "x" (first defined at f.yaml:3:3)`) {
		t.Errorf("text form %q", b.String())
	}
}

// TestDocuments covers the documents of a stream: empty, comment-only and
// null documents are skipped, a non-mapping root is RZ-CFG-005, documents
// keep stream order and start at their marker.
func TestDocuments(t *testing.T) {
	src := "# only a comment\n" +
		"---\n" +
		"---\n" +
		"# comment\n" +
		"---\n" +
		"~\n" +
		"--- null\n" +
		"---\n" +
		"a: 1\n" +
		"--- # comment\n" +
		"b: 2\n" +
		"...\n" +
		"c: 3\n" +
		"--- scalar\n" +
		"--- [1]\n" +
		"---\n" +
		"- x\n"
	docs, diags := parseYAML(t, src)
	if got, want := codes(diags), "RZ-CFG-005@14:5 RZ-CFG-005@15:5 RZ-CFG-005@17:1"; got != want {
		t.Errorf("diagnostics %s, want %s", got, want)
	}
	if len(docs) != 3 {
		t.Fatalf("%d documents, want 3", len(docs))
	}
	starts := []tree.Pos{{Line: 8, Column: 1}, {Line: 10, Column: 1}, {Line: 13, Column: 1}}
	for i, d := range docs {
		if d.Start != starts[i] {
			t.Errorf("document %d starts at %+v, want %+v", i, d.Start, starts[i])
		}
	}
	if !strings.Contains(diags[0].Message, "a resource must be a mapping") {
		t.Errorf("message %q", diags[0].Message)
	}
	for _, empty := range []string{"", "\n\n", "# c\n# d\n", "---\n...\n", "null\n"} {
		docs, diags := parseYAML(t, empty)
		if len(docs) != 0 || len(diags) != 0 {
			t.Errorf("%q: %d documents, %q", empty, len(docs), codes(diags))
		}
	}
}

// TestNonScalarKey checks that a non-scalar key is RZ-CFG-001.
func TestNonScalarKey(t *testing.T) {
	for _, src := range []string{"? [a, b]\n: c\n", "[a]: b\n", "{a: 1}: b\n", "? - a\n: b\n", "? a: b\n: c\n"} {
		docs, diags := parseYAML(t, src)
		if len(diags) == 0 || diags[0].Code != codeParse || len(docs) != 0 {
			t.Errorf("%q: %q, %d documents", src, codes(diags), len(docs))
		}
	}
	// An explicit key holding a scalar is a scalar key.
	docs, diags := parseYAML(t, "? a\n: b\n? |\n  c\n: d\n")
	if len(diags) != 0 || len(docs) != 1 || show(docs[0].Root, false) != "{a:s:\"b\",c\n:s:\"d\"}" {
		t.Errorf("explicit keys: %q, %+v", codes(diags), docs)
	}
}

// TestSyntaxError covers syntax errors: RZ-CFG-001 at the character that
// breaks the grammar, which leaves out only its document, and the parser
// resumes at the next document marker.
func TestSyntaxError(t *testing.T) {
	docs, diags := parseYAML(t, "a: 1\n---\nb: [1, 2\n---\nc: 3\n")
	if len(diags) != 1 || diags[0].Code != codeParse || diags[0].Line != 3 || !strings.HasPrefix(diags[0].Message, "syntax error: ") {
		t.Fatalf("diagnostics %q", codes(diags))
	}
	if len(docs) != 2 {
		t.Fatalf("%d documents, want the two valid ones", len(docs))
	}
	_, diags = parseYAML(t, "a:\n  b: 1\n c: 2\n")
	wantOne(t, diags, codeParse, 3, 2)
	// Two broken documents give one finding each, an unclosed flow
	// collection at its bracket; a "..." marker also ends a broken
	// document.
	docs, diags = parseYAML(t, "a: [\n---\nb: 1\n...\nc: }\n...\nd: 2\n")
	if got := codes(diags); got != "RZ-CFG-001@1:4 RZ-CFG-001@5:4" || len(docs) != 2 {
		t.Errorf("two broken documents: %s, %d documents", got, len(docs))
	}
}

// TestNodeLimit covers MaxDocumentTokens, the node limit: a document of
// more nodes than the limit is RZ-CFG-001 at the node past it and stops
// the file, and so do the documents of one file when together they hold
// more than five quarters of it, so a file of many small documents is
// bounded too.
func TestNodeLimit(t *testing.T) {
	opts := Options{MaxDocumentTokens: 10}
	tests := []struct{ src, want, msg string }{
		// The root mapping, four keys and four values: 9 nodes.
		{"a: 1\nb: 2\nc: 3\nd: 4\n", "", ""},
		// Five entries: the eleventh node is the fifth value. Keys and
		// collections are nodes.
		{"a: 1\nb: 2\nc: 3\nd: 4\ne: 5\n", "RZ-CFG-001@5:4", "document has more than 10 nodes"},
		{"- [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]\n", "RZ-CFG-001@1:28", "document has more than 10 nodes"},
		// The limit stops the file: a later finding is not reported.
		{"[1, 2, 3, 4, 5, 6, 7, 8, 9, 10]\n---\na: &x 1\n", "RZ-CFG-001@1:29", "document has more than 10 nodes"},
		// Three documents of 5 nodes each: the file limit is 12, and the
		// thirteenth node is the third document's first value.
		{"a: 1\nb: 2\n---\na: 1\nb: 2\n---\na: 1\nb: 2\n", "RZ-CFG-001@7:4", "file has more than 12 nodes in its documents"},
		{"a: 1\nb: 2\n---\na: 1\nb: 2\n", "", ""},
	}
	for _, tt := range tests {
		_, diags, err := parse(context.Background(), []byte(tt.src), 0, "f.yaml", FormatYAML, opts, true)
		if err != nil {
			t.Fatal(err)
		}
		if got := codes(diags); got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
			continue
		}
		if tt.msg != "" && diags[0].Message != tt.msg {
			t.Errorf("%q: message %q, want %q", tt.src, diags[0].Message, tt.msg)
		}
	}
	// The default limits admit a file of 20,000 Routes, one document each.
	var b strings.Builder
	for i := range 20000 {
		b.WriteString("---\napiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: r" + strconv.Itoa(i) + "\nspec:\n  hosts: [a.example]\n")
	}
	docs, diags := parseYAML(t, b.String())
	if len(diags) != 0 || len(docs) != 20000 {
		t.Errorf("20,000 Routes: %d documents, %q", len(docs), codes(diags[:min(3, len(diags))]))
	}
}

// TestNumberRange covers integers outside int64 and infinities: RZ-CFG-005.
func TestNumberRange(t *testing.T) {
	for _, src := range []string{"a: 9223372036854775808\n", "a: -.inf\n", "a: .NaN\n", "a: [0x10000000000000000]\n"} {
		docs, diags := parseYAML(t, src)
		if len(diags) != 1 || diags[0].Code != codeSchema || len(docs) != 0 {
			t.Errorf("%q: %q", src, codes(diags))
		}
	}
}

// TestColumnsCountCodePoints checks that columns count code points, not
// bytes.
func TestColumnsCountCodePoints(t *testing.T) {
	docs, diags := parseYAML(t, "ééé: 😀😀\n  # x\nk: [\"日本\", x]\n")
	if len(diags) != 0 {
		t.Fatal(codes(diags))
	}
	want := `{ééé@1:1:s:"😀😀"@1:6,k@3:1:[s:"日本"@3:5,s:"x"@3:11]}`
	if got := show(docs[0].Root, true); got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	_, diags = parseYAML(t, "ééé: 😀\nb: &a x\n")
	wantOne(t, diags, codeAnchor, 2, 4)
}

// TestParseCancel checks that a canceled context returns its error and no
// partial result.
func TestParseCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, f := range []Format{FormatYAML, FormatJSON} {
		docs, diags, err := Parse(ctx, []byte("a: 1\n"), 0, "f", f, Options{})
		if !errors.Is(err, context.Canceled) || docs != nil || diags != nil {
			t.Errorf("%v: %v, %v, %v", f, docs, diags, err)
		}
	}
	// Cancellation between documents.
	n := 0
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	docs, diags, err := Parse(ctx, []byte(strings.Repeat("a: [1, 2]\n---\n", 2000)), 0, "f", FormatYAML, Options{Yield: func() {
		if n++; n == 3 {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) || docs != nil || diags != nil {
		t.Errorf("mid-file: %d documents, %v", len(docs), err)
	}
}

// TestYield checks the Yield hook runs per document and every 256 nodes.
func TestYield(t *testing.T) {
	var b strings.Builder
	b.WriteString("a:\n")
	for i := range 1000 {
		b.WriteString("- " + strconv.Itoa(i) + "\n")
	}
	n := 0
	docs, diags, err := Parse(context.Background(), []byte(b.String()), 0, "f", FormatYAML, Options{Yield: func() { n++ }})
	if err != nil || len(diags) != 0 || len(docs) != 1 {
		t.Fatal(err, codes(diags))
	}
	if n < 1000/256 {
		t.Errorf("Yield called %d times", n)
	}
	n = 0
	if _, _, err := Parse(context.Background(), []byte(`{"a":[`+strings.Repeat("1,", 999)+`1]}`), 0, "f", FormatJSON, Options{Yield: func() { n++ }}); err != nil || n < 3 {
		t.Errorf("JSON: Yield called %d times, %v", n, err)
	}
}

// TestFormat covers FormatOf, which matches the extensions .yaml, .yml and
// .json case-sensitively, Format.String and an unknown format (ErrFormat).
func TestFormat(t *testing.T) {
	tests := []struct {
		name string
		f    Format
		ok   bool
	}{
		{"ruralz.yaml", FormatYAML, true},
		{"a/b.yml", FormatYAML, true},
		{"x.json", FormatJSON, true},
		{"x.YAML", 0, false},
		{"x.Json", 0, false},
		{"yaml", 0, false},
	}
	for _, tt := range tests {
		f, ok := FormatOf(tt.name)
		if f != tt.f || ok != tt.ok {
			t.Errorf("FormatOf(%q) = %v, %v", tt.name, f, ok)
		}
	}
	if FormatYAML.String() != "yaml" || FormatJSON.String() != "json" || Format(9).String() != "unknown" {
		t.Error("Format.String")
	}
	if _, _, err := Parse(context.Background(), nil, 0, "f", Format(9), Options{}); !errors.Is(err, ErrFormat) {
		t.Errorf("unknown format: %v", err)
	}
}

// TestMaxDiagnostics bounds the findings kept for one file at
// MaxDiagnostics+1, so the pipeline's cap (diag.Collector) still sees the
// overflow and a file of many findings stays cheap.
func TestMaxDiagnostics(t *testing.T) {
	src := strings.Repeat("- &a x\n", 10) + "---\n" + strings.Repeat("- !x y\n", 10)
	docs, diags, err := Parse(context.Background(), []byte(src), 0, "f", FormatYAML, Options{MaxDiagnostics: 3})
	if err != nil || len(docs) != 0 {
		t.Fatal(err, len(docs))
	}
	if got := codes(diags); got != "RZ-CFG-003@1:3 RZ-CFG-003@2:3 RZ-CFG-003@3:3 RZ-CFG-003@4:3" {
		t.Errorf("diagnostics %s", got)
	}
	var dup strings.Builder
	for range 10 {
		dup.WriteString("k: 1\n")
	}
	_, diags, _ = Parse(context.Background(), []byte(dup.String()+"---\nk: 1\nk: 2\n"), 0, "f", FormatYAML, Options{MaxDiagnostics: 3})
	if len(diags) != 4 {
		t.Errorf("duplicates: %q", codes(diags))
	}
	_, diags, _ = Parse(context.Background(), []byte("{"+strings.Repeat(`"k":1,`, 10)+`"k":1}`), 0, "f", FormatJSON, Options{MaxDiagnostics: 3})
	if len(diags) != 4 {
		t.Errorf("JSON duplicates: %q", codes(diags))
	}
}
