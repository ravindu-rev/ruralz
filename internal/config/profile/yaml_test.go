// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml/scanner"
	"github.com/goccy/go-yaml/token"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestParseTree checks typing, styles and positions (01 req 12, 14): keys
// and values carry 1-based lines and code-point columns.
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

// TestTokenPassCollects covers 01 req 8: anchors, aliases and merge keys
// are RZ-CFG-003, other tags and %TAG RZ-CFG-004, all collected across
// the file, and a file with any finding is not parsed.
func TestTokenPassCollects(t *testing.T) {
	src := "a: &x 1\n" + //       1:4 anchor
		"b: *x\n" + //            2:4 alias
		"c:\n" +
		"  <<: {d: 1}\n" + //     4:3 merge key
		"e: !include f.yaml\n" + // 5:4 tag
		"g: !!binary aGk=\n" + //  6:4 tag
		"dup: 1\n" +
		"dup: 2\n" + //           a duplicate key is never reported here
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
		// After an empty block scalar and a blank line goccy scans the
		// next line's first node as plain text (sixth WP-33 review).
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
		t.Errorf("%d documents, want none from a file with token-pass findings", len(docs))
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

// TestFinalLineBreak covers a text that does not end with a line break
// (01 req 8, 12, 14; blocker of the sixth WP-33 review): Parse reads it as
// if it did, as the YAML Test Suite reads such a stream, so goccy's
// scanner no longer drops a tag at the end of the input (no RZ-CFG-004)
// nor the trailing spaces of a block scalar's last line, and no position
// moves.
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
		// input ends (minor finding of the eighth WP-33 review).
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

// TestCoreTagsAccepted covers the seven core tags passing the token pass
// and forcing types (01 req 8, 12).
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

// TestTagMismatch covers a tagged value that does not match its tag (01
// req 12): RZ-CFG-001 at the tag.
func TestTagMismatch(t *testing.T) {
	tests := []struct {
		src       string
		code      string
		line, col int
	}{
		{"a: !!int x\n", codeParse, 1, 4},
		{"a: !!bool yes\n", codeParse, 1, 4},
		{"a: !!null 0\n", codeParse, 1, 4},
		// goccy checks these itself and reports at the value, whose
		// column follows the tag's (01 req 14).
		{"a: !!map [1]\n", codeParse, 1, 10},
		{"a: !!seq {b: 1}\n", codeParse, 1, 10},
		{"a: !!str {b: 1}\n", codeParse, 1, 10},
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

// TestTagChains covers a node with more than one tag (11 req 17, 26;
// finding 3 of the WP-33 review): RZ-CFG-001 at the second tag, which
// stops the file before goccy's parser recurses once per tag. Anchors in
// between are RZ-CFG-003, so such a file is never parsed either. A tag
// that follows a tag on an earlier line and starts an implicit key belongs
// to another node, the key of a tagged mapping, and is valid (01 req 8,
// 12; finding 2 of the second WP-33 review); a chain stays bounded at two.
func TestTagChains(t *testing.T) {
	tests := []struct{ src, want string }{
		{"a: !!str !!str x\n", "RZ-CFG-001@1:10"},
		{"a: !!str # c\n  !!str x\n", "RZ-CFG-001@2:3"},
		{"a: !!str\n  !!int 1\n", "RZ-CFG-001@2:3"},
		{"a: [!!str !!str x]\n", "RZ-CFG-001@1:11"},
		{"a: !e !!str x\n", "RZ-CFG-004@1:4 RZ-CFG-001@1:7"},
		{"a: &x !!str y\n", "RZ-CFG-003@1:4"},
		{"a: !!str &x !!str y\n", "RZ-CFG-003@1:10"},
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
		docs, diags := parseYAML(t, tt.src)
		got := codes(diags)
		if len(docs) == 1 && len(diags) == 0 {
			got = show(docs[0].Root, false)
		}
		if got != tt.want {
			t.Errorf("%q: %q, want %q", tt.src, got, tt.want)
		}
		for _, d := range diags {
			if d.Code == codeParse && d.Message != "a node has at most one tag" {
				t.Errorf("%q: message %q", tt.src, d.Message)
			}
		}
	}
	// A chain of 512 KiB, which the token limit lets through to the token
	// pass, stops at its second tag, without the stack growth of goccy's
	// recursion; one of 1 MiB is refused as too large to tokenize.
	src := "a: " + strings.Repeat("!!str ", (1<<19)/6) + "x\n"
	var diags diag.List
	peak := peakHeap(func() { _, diags = parseYAML(t, src) })
	wantOne(t, diags, codeParse, 1, 10)
	if peak > 128<<20 {
		t.Errorf("512 KiB tag chain: peak heap and stack growth %d MiB", peak>>20)
	}
	_, diags = parseYAML(t, "a: "+strings.Repeat("!!str ", (1<<20)/6)+"x\n")
	if len(diags) != 1 || !strings.HasPrefix(diags[0].Message, "document is too large to tokenize") {
		t.Errorf("1 MiB tag chain: %q", codes(diags))
	}
}

// TestTaggedNodeWithoutValue covers a tag that ends its line, in a block
// collection, before a token at or left of that collection's column (01
// req 8, 9; 11 req 17, 26; blocker of the third WP-33 review). goccy nests
// that token under the tagged node, so "a: !!map\nb: 1" read as
// {a: {b: 1}} and a ladder of such lines nested past MaxDepth unseen; the
// token pass stops the file with RZ-CFG-001 at the tag, split or not. A
// compact sequence at a mapping's column and the ':' of an
// explicit key give the tag its value, and a document's tag, a tag before
// a document marker and a tag at the end of the input are outside the
// check.
func TestTaggedNodeWithoutValue(t *testing.T) {
	tests := []struct{ src, want string }{
		{"a: !!map\nb: 1\n", "RZ-CFG-001@1:4"},
		{"a: !!map # c\nb: 1\n", "RZ-CFG-001@1:4"},
		{"a: !!map\n# c\n\nb: 1\n", "RZ-CFG-001@1:4"},
		{"? a\n: !!map\nb: 1\n", "RZ-CFG-001@2:3"},
		{"- !!map\na: 1\n", "RZ-CFG-001@1:3"},
		{"a:\n  - !!map\n  b: 1\n", "RZ-CFG-001@2:5"},
		// The second tag of a line, at its real column (01 req 14).
		{"!!str a: !!map\n!!str b: 1\n", "RZ-CFG-001@1:10"},
		{"a: !!map\n!!str b: 1\n", "RZ-CFG-001@1:4"},
		{"a:\n  !!map\nb: 1\n", "RZ-CFG-001@2:3"},
		{"a: !!map\n  b: !!map\n  c: 1\n", "RZ-CFG-001@2:6"},
		{"a: !!map\n? b\n: 1\n", "RZ-CFG-001@1:4"},
		{"a: !!map\n: 1\n", "RZ-CFG-001@1:4"},
		{"? a\n: !!str\n? b\n", "RZ-CFG-001@2:3"},
		{"? !!str\n? b\n", "RZ-CFG-001@1:3"},
		{"a: !!seq\n- !!seq\n- x\n", "RZ-CFG-001@2:3"},
		{"- !!seq\n- x\n", "RZ-CFG-001@1:3"},
		// A "-" left of the innermost mapping, or at a sequence's column.
		{"a:\n  - !!seq\n- x\n", "RZ-CFG-001@2:5"},
		{"x:\n  a: !!seq\n- y\n", "RZ-CFG-001@2:6"},
		// After a finding the file is never parsed, so the check is
		// skipped and the scan goes on collecting RZ-CFG-003 and
		// RZ-CFG-004 (01 req 8; minor finding of the fourth WP-33
		// review).
		{"a: !e\nb: 1\n", "RZ-CFG-004@1:4"},
		{"a: !e\nb: !f\nc: &x 1\n", "RZ-CFG-004@1:4 RZ-CFG-004@2:4 RZ-CFG-003@3:4"},
		{"a: &x !!map\nb: 1\nc: !e 1\n", "RZ-CFG-003@1:4 RZ-CFG-004@3:4"},
		// The tag has a value, or no sibling follows it.
		{"a: !!seq\n- x\n", `{a:[s:"x"]}`},
		// Its value on its line, a plain scalar that a line starting with
		// '-' continues, which goccy reports at the scalar's last line
		// (finding of the eighth WP-33 review, found by FuzzLoadYAML).
		{"0: !!str 0\n -\n...\n", `{0:s:"0 -"}`},
		// Its content on the next line at the mapping's column, where
		// YAML 1.2 reads no content of the tag: refused, whatever follows.
		{"0: !!str\n0\n -\n", "RZ-CFG-001@1:4"},
		{"0: !!str\n0\n -\n# c\n", "RZ-CFG-001@1:4"},
		{"0: !!str\n # c\n 0\n  -\n", `{0:s:"0 -"}`},
		{"k: !!str a\n  -b\n\nz: 1\n", `{k:s:"a -b",z:i:1}`},
		{"? a\n: !!seq\n- x\n", `{a:[s:"x"]}`},
		{"- a: !!seq\n  - b\n  c: 1\n", `[{a:[s:"b"],c:i:1}]`},
		{"x:\n  a: !!seq\n  - y\n", `{x:{a:[s:"y"]}}`},
		{"--- !!map\na: 1\n", `{a:i:1}`},
		{"!!map\na: 1\n", `{a:i:1}`},
		{"a: !!map\n  b: 1\n", `{a:{b:i:1}}`},
		{"a:\n  !!map\n  b: 1\n", `{a:{b:i:1}}`},
		{"- !!map\n  a: 1\n", `[{a:i:1}]`},
		{"? !!str\n: v\n", `{:s:"v"}`},
		{"a: !!null # c\n# d\n", `{a:n}`},
		{"a: !!null\n...\n", `{a:n}`},
		{"a: !!null\n---\nb: 1\n", `{a:n} {b:i:1}`},
	}
	for _, tt := range tests {
		for _, splitAt := range []int{-1, 0, 3} {
			docs, got := parseWith(t, tt.src, splitAt)
			if got == "" {
				var trees []string
				for _, d := range docs {
					trees = append(trees, show(d.Root, false))
				}
				got = strings.Join(trees, " ")
			}
			if got != tt.want {
				t.Errorf("%q, split at %d: %q, want %q", tt.src, splitAt, got, tt.want)
			}
		}
		_, diags := parseYAML(t, tt.src)
		for _, d := range diags {
			if d.Code == codeParse && d.Message != "a tagged node has no value" {
				t.Errorf("%q: message %q", tt.src, d.Message)
			}
		}
	}
	// The ladder stops at its first tag in a mapping of 255 entries as in
	// one of 256, the split threshold of earlier reviews.
	for _, rows := range []int{255, 256} {
		for _, cols := range []int{1, 63} {
			for _, splitAt := range []int{-1, 0, 3} {
				if _, got := parseWith(t, taggedLadder(cols, rows, 0), splitAt); got != "RZ-CFG-001@1:5" {
					t.Errorf("ladder of %d columns of %d lines, split at %d: %q", cols, rows, splitAt, got)
				}
			}
		}
	}
}

// TestMultiLineImplicitKey covers a ':' with no key on its line after a
// node that started on an earlier line or is a block scalar (01 req 8, 9;
// 11 req 17, 26; blocker of the fourth WP-33 review). YAML 1.2 keeps an
// implicit key on one line. goccy read such a node as a key and attached
// the entry to the innermost open sequence, so a ladder of these lines
// nested past MaxDepth unseen, and the split parse refused the same lines.
// The token pass now stops the file with RZ-CFG-001 at the ':', split or
// not. The
// ':' of an explicit key ('?') is valid on a later line when the key is
// one node, which is all goccy groups with the '?'; otherwise goccy reads
// the last node before the ':' as a multi-line implicit key, and the ':' is
// RZ-CFG-001 too (blocker of the fifth WP-33 review), as are a second node
// and a ':' on the '?' line (its minor finding). An empty key after a value
// ("a: 1\n: b") is refused at its ':' too: goccy refuses it anyway, and
// the ladder has its shape at a mapping's column; its message says the key
// is missing (minor finding of the seventh WP-33 review).
func TestMultiLineImplicitKey(t *testing.T) {
	var wide strings.Builder
	for i := range 300 {
		wide.WriteString("w" + itoa(i) + ": v\n")
	}
	tests := []struct{ src, want string }{
		{"\"x\"\n: y\n", "RZ-CFG-001@2:1"},
		{"a:\n  - \"x\"\n:\n", "RZ-CFG-001@3:1"},
		{"a:\n  - 'x'\n:\n", "RZ-CFG-001@3:1"},
		{"a:\n  - >\n:\n", "RZ-CFG-001@3:1"},
		{"a:\n  - - \"\"\n  :\n", "RZ-CFG-001@3:3"},
		{"a:\n  b:\n    - \"x\"\n  :\n", "RZ-CFG-001@4:3"},
		{"  !!str\n     :\n    k0:\n", "RZ-CFG-001@2:6"},
		// goccy puts the empty content of a block scalar on the ':' line;
		// the node still starts at the header.
		{"- >\n:\n", "RZ-CFG-001@2:1"},
		{"- >\n  - x\n:\n", "RZ-CFG-001@3:1"},
		{"!!str |\n  k\n: v\n", "RZ-CFG-001@3:1"},
		// A multi-line plain scalar, and a flow collection over two lines.
		{"a: 1\n b: 2\n", "RZ-CFG-001@2:3"},
		{"[a,\n b]: c\n", "RZ-CFG-001@2:4"},
		{"a:\n  - [\n  ]\n  :\n", "RZ-CFG-001@4:3"},
		// Not at the column of its explicit key.
		{"? \"x\"\n  : y\n", "RZ-CFG-001@2:3"},
		// An empty key after a value (YAML Test Suite 2JQS).
		{": a\n: b\n", "RZ-CFG-001@2:1"},
		// After a finding the file is never parsed, so the check is
		// skipped and the scan goes on collecting RZ-CFG-003 and
		// RZ-CFG-004 (01 req 8).
		{"a: &x \"y\"\n: z\nb: !e 1\n", "RZ-CFG-003@1:4 RZ-CFG-004@3:4"},
		// An explicit key of more than one node: goccy groups the ':'
		// with the last node, a block sequence, a scalar or a tag on a
		// later line, not with the '?' (fifth WP-33 review).
		{"? \"a\"\n  - \"b\"\n:\n", "RZ-CFG-001@3:1"},
		{"? \"a\"\n  \"b\"\n:\n", "RZ-CFG-001@3:1"},
		{"? \"a\"\n  !!str\n:\n", "RZ-CFG-001@3:1"},
		{"x:\n  ? \"a\"\n    - \"b\"\n  :\n", "RZ-CFG-001@4:3"},
		{"- ? \"a\"\n    - \"b\"\n  :\n", "RZ-CFG-001@3:3"},
		// A key whose tag ends the '?' line is one node, which goccy reads
		// as the tag alone with the content as the entry's value; its
		// message says so (minor finding of the eighth WP-33 review).
		{"? !!str\n  \"a\"\n: v\n", "RZ-CFG-001@3:1"},
		{"? - a\n  - b\n: c\n", "RZ-CFG-001@3:1"},
		{explicitKeyLadder(31, 32), "RZ-CFG-001@3:1"},
		{explicitKeyLadder(-1, 62), "RZ-CFG-001@3:1"},
		{wide.String() + explicitKeyLadder(31, 32), "RZ-CFG-001@303:1"},
		// A second node or a ':' on the '?' line (minor finding of the
		// fifth WP-33 review); "? k:" is the complex key {k: null}.
		{"? \"q\"k\n", "RZ-CFG-001@1:6"},
		{"a:\n  ? \"q\"''\n", "RZ-CFG-001@2:8"},
		{"? k:\n", "RZ-CFG-001@1:4"},
		{"? : v\n", "RZ-CFG-001@1:3"},
		{"? [a] b\n: c\n", "RZ-CFG-001@2:1"},
		// Valid: an explicit key's ':' after one node, and keys on one
		// line.
		{"? \"x\"\n: y\n", `{x:s:"y"}`},
		{"? |\n  k\n: v\n", "{k\n:s:\"v\"}"},
		{"? >\n: v\n", `{:s:"v"}`},
		{"? !!str\n: v\n", `{:s:"v"}`},
		{"? !!str \"a\"\n: v\n", `{a:s:"v"}`},
		{"? a\n  b\n: v\n", `{a b:s:"v"}`},
		{"? \"a\" # c\n# d\n: v\n", `{a:s:"v"}`},
		{"- ? a\n  : b\n", `[{a:s:"b"}]`},
		{"\"x\": y\n", `{x:s:"y"}`},
		{"a:\n  - \"x\"\nb: 1\n", `{a:[s:"x"],b:i:1}`},
		{"a: |\n  t\nb: 1\n", `{a:s:"t\n",b:i:1}`},
		{"a: !!str |\n  t\nb: 1\n", `{a:s:"t\n",b:i:1}`},
		{"a: [x,\n  y]\nb: 1\n", `{a:[s:"x",s:"y"],b:i:1}`},
		{"a: \"x\n  y\"\nb: 1\n", `{a:s:"x y",b:i:1}`},
	}
	for _, tt := range tests {
		for _, splitAt := range []int{-1, 0, 3} {
			docs, got := parseWith(t, tt.src, splitAt)
			if got == "" && len(docs) == 1 {
				got = show(docs[0].Root, false)
			}
			if got != tt.want {
				t.Errorf("%q, split at %d: %q, want %q", tt.src, splitAt, got, tt.want)
			}
		}
		_, diags := parseYAML(t, tt.src)
		for _, d := range diags {
			if d.Code == codeParse && d.Line > 1 && d.Message != msgMultiLineKey && d.Message != msgEmptyKey && d.Message != msgExplicitKey && d.Message != msgExplicitKeyTag {
				t.Errorf("%q: message %q", tt.src, d.Message)
			}
		}
	}
	for src, msg := range map[string]string{
		"a: 1\n: y\n":                  msgEmptyKey,
		": a\n: b\n":                   msgEmptyKey,
		"a:\n  - \"x\"\n:\n":           msgEmptyKey,
		"a:\n  b:\n    - \"x\"\n  :\n": msgEmptyKey,
		"a:\n- b:\n    - \"x\"\n:\n":   msgEmptyKey,
		"a:\n  - - \"\"\n  :\n":        msgMultiLineKey,
		"\"x\"\n: y\n":                 msgMultiLineKey,
		"a: 1\n b: 2\n":                msgMultiLineKey,
		"- >\n:\n":                     msgMultiLineKey,
	} {
		if _, diags := parseYAML(t, src); len(diags) != 1 || diags[0].Message != msg {
			t.Errorf("%q: %q, want the message %q", src, codes(diags), msg)
		}
	}
	// The ladder stops at its first ':' with 254 repetitions as with 256,
	// the split threshold of earlier reviews: 1 MiB of 16 dashes, and 4 MiB
	// of 63 dashes, which parsed to depth 16,257 in a 440 MiB peak heap.
	for _, dashes := range []int{16, 63} {
		for _, reps := range []int{254, 256} {
			src := keyLadder(dashes, reps)
			var got string
			peak := peakHeap(func() { _, got = parseWith(t, src, 0) })
			if got != "RZ-CFG-001@3:1" {
				t.Errorf("ladder of %d dashes, %d repetitions: %q", dashes, reps, got)
			}
			if peak > 64<<20 {
				t.Errorf("ladder of %d dashes, %d repetitions (%d bytes): peak heap growth %d MiB", dashes, reps, len(src), peak>>20)
			}
		}
	}
}

// TestColumnsAfterTags covers columns after a tag (01 req 14; minor finding
// of the fourth WP-33 review). goccy's scanner places every later token on
// a tag's line one column to the left for each tag before it. The positions
// in the tree and in the findings of the token pass, goccy's parse and the
// split parse are the real columns.
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
		// Scalars read from the source (sixth WP-33 review) after a tag
		// on their first line: a plain scalar continued by a line starting
		// with '-', which goccy reported at a later line, and a quoted
		// one. After a tagged key goccy scans the '-' line as a node of
		// its own and refuses it.
		{"k: !!str a\n  -b # c\n", `{k@1:1:s:"a -b"@1:4}`},
		{"!!str k: \"a\\ \n  b\"\n", `{k@1:1:s:"a  b"@1:10}`},
		{"!!str k: 'a\t\n  b'\n", `{k@1:1:s:"a b"@1:10}`},
		{"!!str k: a\n  -b # c\n", "RZ-CFG-001@2:3"},
	}
	for _, tt := range tests {
		for _, splitAt := range []int{-1, 0, 3} {
			docs, got := parseWith(t, tt.src, splitAt)
			if got == "" && len(docs) == 1 {
				got = show(docs[0].Root, true)
			}
			if got != tt.want {
				t.Errorf("%q, split at %d: %q, want %q", tt.src, splitAt, got, tt.want)
			}
		}
	}
	// The table itself, built from tokens out of order.
	tag := func(line, col int) *token.Token {
		return &token.Token{Type: token.TagType, Position: &token.Position{Line: line, Column: col}}
	}
	tc := columnsOf(token.Tokens{tag(3, 4), tag(1, 1), tag(1, 9), {Type: token.StringType, Position: &token.Position{Line: 1, Column: 6}}}, "")
	for _, c := range []struct{ line, col, want int }{
		{1, 1, 1}, {1, 6, 7}, {1, 9, 10}, {1, 14, 16}, {2, 5, 5}, {3, 4, 4}, {3, 9, 10}, {4, 1, 1},
	} {
		if got := tc.fix(c.line, c.col); got != c.want {
			t.Errorf("fix(%d, %d) = %d, want %d", c.line, c.col, got, c.want)
		}
	}
	if got := columnTable(nil).fix(1, 7); got != 7 {
		t.Errorf("empty table: fix(1, 7) = %d", got)
	}
}

// TestFlowDashes covers "-" in flow context (01 req 9, 11 req 26; finding 1
// of the WP-33 review). A block sequence entry inside a flow collection
// is RZ-CFG-001, which stops the file before goccy nests one sequence per
// dash past the depth limit; so is a plain scalar "-" before a flow
// indicator. "-" before ':', and scalars that start with '-', are valid.
func TestFlowDashes(t *testing.T) {
	tests := []struct{ src, want string }{
		{"a: [- x]\n", "RZ-CFG-001@1:5"},
		{"a: {k: - x}\n", "RZ-CFG-001@1:8"},
		{"a: [x, -\n]\n", "RZ-CFG-001@1:8"},
		{"a: [\n  - x\n]\n", "RZ-CFG-001@2:3"},
		{"a: [-]\n", "RZ-CFG-001@1:5"},
		{"a: [-, -]\n", "RZ-CFG-001@1:5"},
		{"a: [x, -]\n", "RZ-CFG-001@1:8"},
		{"a: {k: -}\n", "RZ-CFG-001@1:8"},
		{"a: [-: x]\n", `{a:[{-:s:"x"}]}`},
		{"a: {-: x}\n", `{a:{-:s:"x"}}`},
		{"a: [-1, -x, \"-\", '-', -.5, a -, -#]\n", `{a:[i:-1,s:"-x",s:"-",s:"-",f:-0.5,s:"a -",s:"-#"]}`},
		{"a:\n- [x]\n- - y\n", `{a:[[s:"x"],[s:"y"]]}`},
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
	}
	// goccy's parse of nested dashes costs quadratic memory; the token
	// pass stops at the first one.
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
		wantOne(t, diags, codeParse, tt.line, tt.col)
		if got := allocBytes() - before; got > 16<<20 {
			t.Errorf("%d bytes of dashes allocate %d MiB", len(tt.src), got>>20)
		}
	}
}

// TestDirectives covers %YAML and reserved directives (01 req 8).
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
}

// TestInvalidTokenStopsFile covers a scanner InvalidType token: RZ-CFG-001
// at its position, ending the file (01 req 8).
func TestInvalidTokenStopsFile(t *testing.T) {
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

// nest returns YAML nested depth levels deep in the given style.
func nest(style string, depth int) string {
	var b strings.Builder
	switch style {
	case "block map":
		for i := range depth {
			b.WriteString(strings.Repeat("  ", i) + "k" + strconv.Itoa(i) + ":\n")
		}
		b.WriteString(strings.Repeat("  ", depth) + "leaf\n")
		// Replace the deepest key's empty value with a scalar.
		s := b.String()
		return strings.TrimSuffix(s[:strings.LastIndex(s, "\n"+strings.Repeat("  ", depth)+"leaf")], ":") + ": leaf\n"
	case "block seq":
		for i := range depth {
			b.WriteString(strings.Repeat("  ", i) + "-\n")
		}
		b.WriteString(strings.Repeat("  ", depth) + "leaf\n")
		return b.String()
	case "compact seq":
		b.WriteString(strings.Repeat("- ", depth) + "leaf\n")
		return b.String()
	case "flow":
		return "a: " + strings.Repeat("[", depth-1) + strings.Repeat("]", depth-1) + "\n"
	case "mixed":
		// map > seq > map > seq ... in block style, then flow.
		ind := 0
		for i := 0; i < depth; i++ {
			if i%2 == 0 {
				b.WriteString(strings.Repeat(" ", ind) + "k:\n")
			} else {
				b.WriteString(strings.Repeat(" ", ind) + "-\n")
			}
			ind += 2
		}
		b.WriteString(strings.Repeat(" ", ind) + "leaf\n")
		return b.String()
	}
	panic(style)
}

// TestDepth covers the depth algorithm of 01 req 9: the root mapping is
// depth 1, 64 levels pass and 65 is RZ-CFG-001.
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

// TestDepthOptions honors a configured limit (01 req 6, 9).
func TestDepthOptions(t *testing.T) {
	src := []byte("a:\n  b:\n    c: 1\n")
	for limit, want := range map[int]int{2: 1, 3: 0} {
		_, diags, err := Parse(context.Background(), src, 0, "f", FormatYAML, Options{MaxDepth: limit})
		if err != nil || len(diags) != want {
			t.Errorf("MaxDepth %d: %q, %v", limit, codes(diags), err)
		}
	}
}

// TestDepthNotInflated checks constructs the literal stack rule would
// count again at every entry: sequences at their key's indentation,
// tagged keys and explicit keys (01 req 9).
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

// TestDepthResetsPerDocument checks that "---" resets the stack (01 req
// 9).
func TestDepthResetsPerDocument(t *testing.T) {
	doc := nest("block map", 64)
	docs, diags := parseYAML(t, doc+"---\n"+doc+"---\n"+doc)
	if len(diags) != 0 || len(docs) != 3 {
		t.Fatalf("diagnostics %q, %d documents", codes(diags), len(docs))
	}
}

// TestTokenLimit covers MaxDocumentTokens (01 req 6, 8, risk 1): the
// limit counts the tokens of each document, the tokens of a whole file are
// bounded to five quarters of it (maxFileTokens; minor finding of the
// eighth WP-33 review), the bound from the bytes refuses a document before
// it is tokenized, and the exact count after tokenizing is the backstop.
func TestTokenLimit(t *testing.T) {
	tests := []struct {
		src   string
		limit int
		want  string
		msg   string
	}{
		// One document of 11 tokens.
		{"b: [1, 2, 3, 4]\n", 11, "", ""},
		{"b: [1, 2, 3, 4]\n", 10, "RZ-CFG-001@1:15", "more than 10 tokens"},
		// Documents of 3 and 12 tokens ("---" included): the count starts
		// again at each document, and the file holds 15.
		{"a: 1\n---\nb: [1, 2, 3, 4]\n", 12, "", ""},
		{"a: [1, 2, 3]\n---\nb: [1, 2, 3, 4]\n", 8, "RZ-CFG-001@1:12", "more than 8 tokens"},
		// Documents of 7 and 12 tokens pass the file's 15 at the second
		// document's ninth token.
		{"a: [1, 2]\n---\nb: [1, 2, 3, 4]\n", 12, "RZ-CFG-001@3:11", "file has more than 15 tokens in its documents"},
	}
	for _, tt := range tests {
		docs, diags, err := Parse(context.Background(), []byte(tt.src), 0, "f", FormatYAML, Options{MaxDocumentTokens: tt.limit})
		if err != nil {
			t.Fatal(err)
		}
		if got := codes(diags); got != tt.want {
			t.Errorf("%q, limit %d: %q, want %q", tt.src, tt.limit, got, tt.want)
		}
		if tt.want != "" && docs != nil {
			// The documents before the finding are not kept, not even in
			// the storage of an empty slice.
			t.Errorf("%q, limit %d: documents %d of %d kept", tt.src, tt.limit, len(docs), cap(docs))
		}
		if tt.want != "" && !strings.Contains(diags[0].Message, tt.msg) {
			t.Errorf("%q, limit %d: message %q, want %q", tt.src, tt.limit, diags[0].Message, tt.msg)
		}
	}
	// A flood of one-character tokens is refused from its bytes, at the
	// character where the bound passes the limit.
	flood := "a: " + strings.Repeat("[", 50) + "\n"
	_, diags, err := Parse(context.Background(), []byte(flood), 0, "f", FormatYAML, Options{MaxDocumentTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	if d := wantOne(t, diags, codeParse, 1, 21); d.Message != "document is too large to tokenize: estimated more than 20 tokens" {
		t.Errorf("message %q", d.Message)
	}
	// The bound over-counts indicators inside a quoted scalar, three real
	// tokens here, so its finding says the count is an estimate (finding 5
	// of the WP-33 review).
	_, diags, err = Parse(context.Background(), []byte(`a: "`+strings.Repeat("x,", 20)+"\"\n"), 0, "f", FormatYAML, Options{MaxDocumentTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) != 1 || !strings.Contains(diags[0].Message, "estimated more than 20 tokens") {
		t.Errorf("quoted scalar: %q %v", codes(diags), diags)
	}
	// The exact count after tokenizing.
	p := &fileParser{path: "f"}
	pass := &tokenPass{p: p, maxDepth: 64, maxTokens: 3}
	var s scanner.Scanner
	s.Init("a: [b]\n")
	tks, _ := s.Scan()
	pass.run(tks, 4, columnsOf(tks, "a: [b]\n"), "a: [b]\n")
	if !pass.fatal || codes(p.diags) != "RZ-CFG-001@5:5" || p.diags[0].Message != "document has more than 3 tokens" {
		t.Errorf("exact count: %q %v", codes(p.diags), p.diags)
	}
}

// TestEstimateBoundsTokens checks that the byte bound is never below
// goccy's token count, over the YAML Test Suite inputs and this package's
// test inputs (01 req 8; 01 risk 1).
func TestEstimateBoundsTokens(t *testing.T) {
	inputs := suiteInputs(t)
	inputs = append(inputs, nest("mixed", 40), "a: [1,2,{b: c}]\n# c\n? x\n: y\n", "'a''b': \"c\\\"d\"\n- &a b\n- *a\n<<: {}\n")
	checked := 0
	for _, in := range inputs {
		text := strings.ReplaceAll(strings.ReplaceAll(in, "\r\n", "\n"), "\r", "\n")
		for _, seg := range splitDocuments(text) {
			var s scanner.Scanner
			s.Init(seg.text)
			n := 0
			for {
				tks, err := s.Scan()
				n += len(tks)
				if err != nil || len(tks) == 0 {
					break
				}
			}
			est, _ := estimateTokens(seg.text, 1<<30)
			if est < n {
				t.Errorf("estimate %d < %d tokens for %q", est, n, seg.text)
			}
			checked++
		}
	}
	if checked < 100 {
		t.Fatalf("checked %d documents", checked)
	}
}

// TestDuplicateKeys covers 01 req 11: RZ-CFG-002 at the second key with
// the first as related location, every duplicate reported, keys compared
// as written (1 equals "1").
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

// TestDocuments covers 01 req 13: empty, comment-only and null documents
// are skipped, a non-mapping root is RZ-CFG-005, documents keep stream
// order and start at their marker.
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

// TestNonScalarKey covers 01 req 10: a non-scalar key is RZ-CFG-001.
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

// TestSyntaxError covers 01 req 10: a goccy syntax error is RZ-CFG-001 at
// the token position, and other documents still parse.
func TestSyntaxError(t *testing.T) {
	docs, diags := parseYAML(t, "a: 1\n---\nb: [1, 2\n---\nc: 3\n")
	if len(diags) != 1 || diags[0].Code != codeParse || diags[0].Line < 3 || !strings.HasPrefix(diags[0].Message, "syntax error: ") {
		t.Fatalf("diagnostics %q", codes(diags))
	}
	if len(docs) != 2 {
		t.Fatalf("%d documents, want the two valid ones", len(docs))
	}
	_, diags = parseYAML(t, "a:\n  b: 1\n c: 2\n")
	wantOne(t, diags, codeParse, 3, 2)
}

// TestNumberRange covers integers outside int64 and infinities: RZ-CFG-005
// (01 req 12).
func TestNumberRange(t *testing.T) {
	for _, src := range []string{"a: 9223372036854775808\n", "a: -.inf\n", "a: .NaN\n", "a: [0x10000000000000000]\n"} {
		docs, diags := parseYAML(t, src)
		if len(diags) != 1 || diags[0].Code != codeSchema || len(docs) != 0 {
			t.Errorf("%q: %q", src, codes(diags))
		}
	}
}

// TestColumnsCountCodePoints covers 01 req 14.
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
// partial result (01 req 53).
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

// TestYield checks the Yield hook runs per document and every 256 nodes
// (01 req 53).
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

// TestFormat covers FormatOf, which matches the extensions .yaml, .yml
// and .json case-sensitively (01 req 3), Format.String and an unknown
// format (ErrFormat).
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

// TestSplitDocuments covers the document cut: markers at column 1 only,
// directives with the document they precede, comment-only segments
// dropped (01 req 9, 13).
func TestSplitDocuments(t *testing.T) {
	text := "# c\n%YAML 1.2\n---\na: 1\n  ---\n---x\n...\n# between\n%TAG ! x\n--- |\n  b\n... # end\n# tail\n"
	var got []string
	for _, s := range splitDocuments(text) {
		got = append(got, strconv.Itoa(s.startLine)+":"+strconv.Quote(s.text))
	}
	want := []string{
		`1:"# c\n%YAML 1.2\n---\na: 1\n  ---\n---x\n...\n"`,
		`8:"# between\n%TAG ! x\n--- |\n  b\n... # end\n"`,
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("segments\n got %v\nwant %v", got, want)
	}
}

// TestMaxDiagnostics bounds the findings kept for one file at
// MaxDiagnostics+1, so the pipeline's cap (diag.Collector) still sees the
// overflow and a file of many findings stays cheap (01 req 8, 50).
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
