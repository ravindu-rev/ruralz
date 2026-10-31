// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestTabColumns covers positions after a tab: a tab is one column, as
// every character is, so a node after it starts in the next column, and a
// tab separates nodes and indicators as a space does where YAML 1.2 allows
// white space.
func TestTabColumns(t *testing.T) {
	tests := []struct{ src, want string }{
		{"{\"a\":\t\"b\"}\n", `{a@1:2:s:"b"@1:7}`},
		{"a:\t'x'\n", `{a@1:1:s:"x"@1:4}`},
		{"a:\t\"b\"\n", `{a@1:1:s:"b"@1:4}`},
		{"- \t\"x\"\n", `[s:"x"@1:4]`},
		{"x: [b,\t'e']\n", `{x@1:1:[s:"b"@1:5,s:"e"@1:8]}`},
		{"x: {a: b\tc, d: \"e\"}\n", `{x@1:1:{a@1:5:s:"b\tc"@1:8,d@1:13:s:"e"@1:16}}`},
		{"apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name:\t\"r1\"\n", `{apiVersion@1:1:s:"ruralz/v1alpha1"@1:13,kind@2:1:s:"Route"@2:7,metadata@3:1:{name@4:3:s:"r1"@4:9}}`},
		{"a:\t!!str x\n  y\n", `{a@1:1:s:"x y"@1:4}`},
		{"a:\tb\n  c\n", `{a@1:1:s:"b c"@1:4}`},
		{"a: !!str\tx\n", `{a@1:1:s:"x"@1:4}`},
		{"a: [!!str\t\"q\", !!int\t1]\n", `{a@1:1:[s:"q"@1:5,i:1@1:16]}`},
		{"kind:\tRoute\n", `{kind@1:1:s:"Route"@1:7}`},
		{"a:\t\t!!str x\n", `{a@1:1:s:"x"@1:5}`},
		{"a: [x,\ty]\n", `{a@1:1:[s:"x"@1:5,s:"y"@1:8]}`},
		{"a:\t|\n  x\n", `{a@1:1:s:"x\n"@1:4}`},
		{"a:\t>-\n  x\n  y\nb:\tc\n", `{a@1:1:s:"x y"@1:4,b@4:1:s:"c"@4:4}`},
		{"x: 1\t2 # c\ny:\t3\n", `{x@1:1:s:"1\t2"@1:4,y@2:1:i:3@2:4}`},
		{"a:\t\"x\ty\"\t# c\nb:\t[1,\t2]\n", `{a@1:1:s:"x\ty"@1:4,b@2:1:[i:1@2:5,i:2@2:8]}`},
		{"k: [a\n  b,\t[ c ]]\n", `{k@1:1:[s:"a b"@1:5,[s:"c"@2:8]]}`},
		{"a:\t>\n\nb:\t|+\n\n", `{a@1:1:s:""@1:4,b@3:1:s:"\n"@3:4}`},
		// A flow node on the line after its key may follow a tab (YAML
		// Test Suite DK95/00).
		{"foo:\n\tbar\n", `{foo@1:1:s:"bar"@2:2}`},
		{"foo:\n \t[bar]\n", `{foo@1:1:[s:"bar"@2:4]}`},
		{"a:\t.inf\n", "RZ-CFG-005@1:4"},
		{"a:\t&x 1\n", "RZ-CFG-003@1:4"},
		{"a:\t!e 1\n", "RZ-CFG-004@1:4"},
		{"a: [!e\tr]\n", "RZ-CFG-004@1:5"},
		{"a:\t1\na:\t2\n", "RZ-CFG-002@2:1"},
		{"a:\t!!int x\n", "RZ-CFG-001@1:4"},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, true)
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
	}
}

// TestTrailingSpaceColumns covers a plain scalar that spaces end its line:
// it is at its first character.
func TestTrailingSpaceColumns(t *testing.T) {
	tests := []struct{ src, want string }{
		{"a: b \n", `{a@1:1:s:"b"@1:4}`},
		{"a: b  \nc: 1\n", `{a@1:1:s:"b"@1:4,c@2:1:i:1@2:4}`},
		{"0: 0 ", `{0@1:1:i:0@1:4}`},
		{"- a  \n- b\n", `[s:"a"@1:3,s:"b"@2:3]`},
		{"a:  !!str b \n", `{a@1:1:s:"b"@1:5}`},
		{"a: b  # c\n", `{a@1:1:s:"b"@1:4}`},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, true)
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
	}
}

// TestTabIndentedJSONAsYAML parses JSON indented with tabs, which is YAML
// too (configuration model, "Format decision": JSON is a strict subset of
// YAML), as JSON and as YAML: the trees and every position agree, a tab
// between a key and its ':' included.
func TestTabIndentedJSONAsYAML(t *testing.T) {
	for _, src := range []string{
		"{\n\t\"a\": 1,\n\t\"b\": [\n\t\t\"x\"\n\t]\n}\n",
		"{\n\t\"apiVersion\":\t\"ruralz/v1alpha1\",\n\t\"kind\": \"Route\",\n\t\"spec\": {\n\t\t\"hosts\": [\"a\",\t\"b\"],\n\t\t\"n\":\t2.5,\n\t\t\"t\": true,\n\t\t\"z\": null\n\t}\n}\n",
		"{\"a\":\t{\"b\":\t[1,\t{\"c\":\t\"d\\te\"}]}}\n",
		"{\"a\"\t: 1}\n",
		"{\"a\": 1,\n\t\"b\"\t:\t2}\n",
		"{\"a\" \t : {\"b\"\t:[1]}}\n",
		"{\"a\":[1,2],\"b\":{\"c\":\"d\"},\"e\":\"\"}\n",
	} {
		jd, jdiags := parseJSON(t, src)
		yd, ydiags := parseYAML(t, src)
		if len(jdiags) != 0 || len(ydiags) != 0 || len(jd) != 1 || len(yd) != 1 {
			t.Errorf("%q: JSON %q, YAML %q", src, codes(jdiags), codes(ydiags))
			continue
		}
		if a, b := show(jd[0].Root, true), show(yd[0].Root, true); a != b {
			t.Errorf("%q:\njson %s\nyaml %s", src, a, b)
		}
	}
}

// TestTabsAroundKeys covers tabs before a block mapping key and between a
// quoted key and its ':' (YAML Test Suite DK95/06, Y79Y/004-010). YAML 1.2
// indents a block mapping with spaces only, so a key after a tab on its
// line is RZ-CFG-001, and a tab cannot separate a '-' from a compact
// mapping after it; a tab between a key and its ':' separates them as a
// space does.
func TestTabsAroundKeys(t *testing.T) {
	tests := []struct{ src, want, msg string }{
		{"\t\"\": 1\n", "RZ-CFG-001@1:2", msgTabKey},
		{"\t\"k\": v\n", "RZ-CFG-001@1:2", msgTabKey},
		{"\tk: v\n", "RZ-CFG-001@1:2", msgTabKey},
		{"a:\n  \t\"b\": 1\n", "RZ-CFG-001@2:4", msgTabKey},
		{"foo:\n  a: 1\n  \tb: 2\n", "RZ-CFG-001@3:4", msgTabKey},
		{"- \t\"a\": 1\n", "RZ-CFG-001@1:4", msgTabCompact},
		{"-\t\"a\": 1\n", "RZ-CFG-001@1:3", msgTabCompact},
		{"-\t!!str a: 1\n", "RZ-CFG-001@1:3", msgTabCompact},
		{"-\t-\n", "RZ-CFG-001@1:3", msgTabCompact},
		{"- \t-\n", "RZ-CFG-001@1:4", msgTabCompact},
		{"? a\n:\t-\n", "RZ-CFG-001@2:3", msgTabCompact},
		{"? a\n:\tkey:\n", "RZ-CFG-001@2:3", msgTabCompact},
		// A collection after '?' is a key that is not a scalar, which the
		// profile refuses first.
		{"?\t-\n", "RZ-CFG-001@1:3", msgComplexKey},
		{"?\tkey:\n", "RZ-CFG-001@1:3", msgComplexKey},
		{"? -\n:\t-\n", "RZ-CFG-001@1:3", msgComplexKey},
		{"k:\n-\ta\n", `{k@1:1:[s:"a"@2:3]}`, ""},
		{"k:\n-\t-1\n", `{k@1:1:[i:-1@2:3]}`, ""},
		{"a:\t\"b\"\n", `{a@1:1:s:"b"@1:4}`, ""},
		{"\"a\"\t: b\n", `{a@1:1:s:"b"@1:7}`, ""},
		{"'q'\t\t: v\nr:\t1\n", `{q@1:1:s:"v"@1:8,r@2:1:i:1@2:4}`, ""},
		{"k: {\"a\"\t: 1}\n", `{k@1:1:{a@1:5:i:1@1:11}}`, ""},
		{"k: \"a\t:\"\n", `{k@1:1:s:"a\t:"@1:4}`, ""},
		{"# \"c\"\t: d\nk: |\n  \"x\"\t: y\n", `{k@2:1:s:"\"x\"\t: y\n"@2:4}`, ""},
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
		if tt.msg != "" && len(diags) > 0 && diags[0].Message != tt.msg {
			t.Errorf("%q: message %q, want %q", tt.src, diags[0].Message, tt.msg)
		}
	}
}

// TestPositionsPointAtNodes checks that every key and value of every
// accepted YAML Test Suite stream, and of its variants with tabs after
// indicators and before comments, is reported at its first character: a
// quote, a tag, a block scalar indicator, or the scalar's own first
// character, which a number written with a sign or a leading dot does not
// keep.
func TestPositionsPointAtNodes(t *testing.T) {
	tabs := regexp.MustCompile(`([:,\[{-]) ([^ \n#])`)
	checked := 0
	for _, in := range suiteInputs(t) {
		for _, src := range []string{in, tabs.ReplaceAllString(in, "$1\t$2"), strings.ReplaceAll(in, " #", "\t#")} {
			docs, findings := parseWith(t, src)
			if findings != "" {
				continue
			}
			lines := strings.Split(src, "\n")
			for _, d := range docs {
				checkPositions(t, src, d.Root, lines)
			}
			checked++
		}
	}
	if checked < 600 {
		t.Fatalf("checked %d streams", checked)
	}
}

// checkPositions reports the keys and values of n whose position does not
// hold their first character.
func checkPositions(t *testing.T, src string, n *tree.Node, lines []string) {
	t.Helper()
	at := func(p tree.Pos) rune {
		if l := int(p.Line) - 1; l >= 0 && l < len(lines) {
			for i, r := range []rune(lines[l]) {
				if i == int(p.Column)-1 {
					return r
				}
			}
		}
		return 0
	}
	first := func(s string) rune {
		r, _ := utf8.DecodeRuneInString(s)
		return r
	}
	switch n.Kind {
	case tree.KindMap:
		for _, m := range n.Members {
			if r := at(m.KeyPos); m.Key != "" && r != first(m.Key) && !strings.ContainsRune(`"'!|>`, r) {
				t.Errorf("key %q at %d:%d holds %q in %q", m.Key, m.KeyPos.Line, m.KeyPos.Column, r, src)
			}
			checkPositions(t, src, m.Value, lines)
		}
	case tree.KindList:
		for _, it := range n.Items {
			checkPositions(t, src, it, lines)
		}
	case tree.KindNull:
	default:
		if r := at(n.Pos); n.Text != "" && r != first(n.Text) && !strings.ContainsRune(`"'!|>+-.0`, r) {
			t.Errorf("value %q at %d:%d holds %q in %q", n.Text, n.Pos.Line, n.Pos.Column, r, src)
		}
	}
}
