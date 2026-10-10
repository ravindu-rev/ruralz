// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/goccy/go-yaml/token"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestTabColumns covers positions after a tab (01 req 8, 13, 14, 15;
// major finding of the seventh WP-33 review). goccy's scanner skips most
// tabs outside a quoted scalar without counting them, so the token pass
// looked for a quoted or multi-line plain scalar one column short per tab
// and refused valid YAML ("{\"a\":\t\"b\"}"), and positions after a tab were
// short. Every token is now located in the source; a tag that goccy runs
// into the tab after it is scanned with that tab as a space. Results and
// positions are the same at the default threshold, with the threshold
// raised and with goccy's own parse.
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
		// After a plain scalar over two lines, and after an empty block
		// scalar, whose empty content goccy places on the next key's line.
		{"k: [a\n  b,\t[ c ]]\n", `{k@1:1:[s:"a b"@1:5,[s:"c"@2:8]]}`},
		{"a:\t>\n\nb:\t|+\n\n", `{a@1:1:s:""@1:4,b@3:1:s:"\n"@3:4}`},
		{"a:\t.inf\n", "RZ-CFG-005@1:4"},
		{"a:\t&x 1\n", "RZ-CFG-003@1:4"},
		{"a:\t!e 1\n", "RZ-CFG-004@1:4"},
		{"a: [!e\tr]\n", "RZ-CFG-004@1:5"},
		{"a:\t1\na:\t2\n", "RZ-CFG-002@2:1"},
		{"a:\t!!int x\n", "RZ-CFG-001@1:4"},
	}
	for _, tt := range tests {
		for _, splitAt := range []int{-1, 0, 3} {
			docs, got := parseWith(t, tt.src, splitAt)
			if got == "" && len(docs) == 1 {
				got = show(docs[0].Root, true)
			}
			if got != tt.want {
				t.Errorf("%q, split at %d: %s, want %s", tt.src, splitAt, got, tt.want)
			}
		}
	}
}

// TestTrailingSpaceColumns covers a plain scalar that spaces end its line
// (01 req 14; found by FuzzLoadYAML's position check in the seventh WP-33
// review): goccy placed it one column to the right per space ("a: b  "
// put b at 6, not 4).
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
		for _, splitAt := range []int{-1, 0, 3} {
			docs, got := parseWith(t, tt.src, splitAt)
			if got == "" && len(docs) == 1 {
				got = show(docs[0].Root, true)
			}
			if got != tt.want {
				t.Errorf("%q, split at %d: %s, want %s", tt.src, splitAt, got, tt.want)
			}
		}
	}
}

// TestTabIndentedJSONAsYAML parses JSON indented with tabs, which is YAML
// too (CM "Format decision": JSON is a strict subset of YAML; 01 req 13,
// 14, 15; seventh WP-33 review), as JSON and as YAML: the trees and every
// position agree.
func TestTabIndentedJSONAsYAML(t *testing.T) {
	for _, src := range []string{
		"{\n\t\"a\": 1,\n\t\"b\": [\n\t\t\"x\"\n\t]\n}\n",
		"{\n\t\"apiVersion\":\t\"ruralz/v1alpha1\",\n\t\"kind\": \"Route\",\n\t\"spec\": {\n\t\t\"hosts\": [\"a\",\t\"b\"],\n\t\t\"n\":\t2.5,\n\t\t\"t\": true,\n\t\t\"z\": null\n\t}\n}\n",
		"{\"a\":\t{\"b\":\t[1,\t{\"c\":\t\"d\\te\"}]}}\n",
		// A tab between a key and its ':', which goccy's scanner refused
		// (minor finding of the eighth WP-33 review; untabSeparators).
		"{\"a\"\t: 1}\n",
		"{\"a\": 1,\n\t\"b\"\t:\t2}\n",
		"{\"a\" \t : {\"b\"\t:[1]}}\n",
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
// quoted key and its ':' (01 req 8, 14, 15; findings of the eighth WP-33
// review). YAML 1.2 indents a block mapping with spaces only, so a key
// after a tab on its line is RZ-CFG-001, as goccy refuses a plain one and
// yaml.v3 every one; goccy read a quoted key there, or any key at the
// start of the input, as if the tab were a space. A tab between a quoted
// key and its ':' separates them as a space does.
func TestTabsAroundKeys(t *testing.T) {
	tests := []struct{ src, want string }{
		{"\t\"\": 1\n", "RZ-CFG-001@1:2"},
		{"\t\"k\": v\n", "RZ-CFG-001@1:2"},
		{"a:\n  \t\"b\": 1\n", "RZ-CFG-001@2:4"},
		{"- \t\"a\": 1\n", "RZ-CFG-001@1:4"},
		{"-\t\"a\": 1\n", "RZ-CFG-001@1:3"},
		{"-\t!!str a: 1\n", "RZ-CFG-001@1:3"},
		{"\tk: v\n", "RZ-CFG-001@1:2"},
		{"k:\n-\ta\n", `{k@1:1:[s:"a"@2:3]}`},
		{"a:\t\"b\"\n", `{a@1:1:s:"b"@1:4}`},
		{"\"a\"\t: b\n", `{a@1:1:s:"b"@1:7}`},
		{"'q'\t\t: v\nr:\t1\n", `{q@1:1:s:"v"@1:8,r@2:1:i:1@2:4}`},
		{"k: {\"a\"\t: 1}\n", `{k@1:1:{a@1:5:i:1@1:11}}`},
		{"k: \"a\t:\"\n", `{k@1:1:s:"a\t:"@1:4}`},
		{"# \"c\"\t: d\nk: |\n  \"x\"\t: y\n", `{k@2:1:s:"\"x\"\t: y\n"@2:4}`},
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
		if strings.HasPrefix(tt.want, "RZ-") && strings.Contains(tt.src, "\"") && diags[0].Message != msgTabKey {
			t.Errorf("%q: message %q", tt.src, diags[0].Message)
		}
	}
}

// TestColumnTable covers the table built from located tokens (01 req 14):
// corrections for tabs, for tags, and the comment goccy places past a
// block scalar header, each from its token's column on.
func TestColumnTable(t *testing.T) {
	src := "a:\t!!str\tx\nb: |  # c\n  t\nc:\t[1,\t2]\n"
	tks, err := scanString(strings.Replace(src, "!!str\t", "!!str ", 1))
	if err != nil {
		t.Fatal(err)
	}
	ct := columnsOf(tks, src)
	for _, tk := range tks {
		if tk.Type == token.StringType && strings.TrimSpace(tk.Value) == "t" {
			continue // a block scalar's content, which the table leaves alone
		}
		col := ct.fix(tk.Position.Line, tk.Position.Column)
		line := strings.Split(src, "\n")[tk.Position.Line-1]
		if first, _ := firstChar(tk); col < 1 || col > len(line) || line[col-1] != first {
			t.Errorf("%s %q at %d:%d is at column %d, which holds %q", tk.Type, tk.Value, tk.Position.Line, tk.Position.Column, col, line[min(max(col-1, 0), len(line)-1)])
		}
	}
	// Tags shift the columns after them, and a source that does not match
	// leaves goccy's columns.
	tks, _ = scanString("!!str a: !!int 1\n")
	if got := columnsOf(tks, "!!str a: !!int 1\n").fix(1, 14); got != 16 {
		t.Errorf("after two tags: %d", got)
	}
	tks, _ = scanString("a:\tb\n")
	if got := columnsOf(tks, "x:\tb\n").fix(1, 3); got != 3 {
		t.Errorf("unmatched source: %d", got)
	}
	if quoteEnd(`"a\"b`, 0) != -1 || quoteEnd(`'a''b'c`, 0) != 6 || plainEnd("a\tb", 0, "ac") != -1 || plainEnd("a\t", 0, "ab") != -1 {
		t.Error("token ends")
	}
	// A plain scalar over several lines ends where the last line of its
	// source text ends, if goccy's Origin holds that text.
	folded := func(origin, text string) int {
		return foldedEnd(&token.Token{Type: token.StringType, Value: "a b", Origin: origin}, text, 0)
	}
	if folded("a\n  b", "a\n  b, c") != 5 || folded("a\n  b", "a\n  x") != -1 || folded("a b", "a b") != -1 || folded("a\n  b", "a") != -1 || folded("a\n  b", "x\n  b") != -1 {
		t.Error("folded ends")
	}
}

// TestPositionsPointAtNodes checks that every key and value of every
// accepted YAML Test Suite stream, and of its variants with tabs after
// indicators and before comments, is reported at its first character (01
// req 14; seventh WP-33 review): a quote, a tag, a block scalar indicator,
// or the scalar's own first character, which a number written with a sign
// or a leading dot does not keep.
func TestPositionsPointAtNodes(t *testing.T) {
	tabs := regexp.MustCompile(`([:,\[{-]) ([^ \n#])`)
	checked := 0
	for _, in := range suiteInputs(t) {
		for _, src := range []string{in, tabs.ReplaceAllString(in, "$1\t$2"), strings.ReplaceAll(in, " #", "\t#")} {
			docs, findings := parseWith(t, src, 0)
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
