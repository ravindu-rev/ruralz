// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strings"
	"testing"
)

// TestBlockScalars covers block scalar values read from the source (01 req
// 12; blocker of the sixth WP-33 review). goccy dropped the trailing white
// space of the last line with strip chomping and at the end of the input,
// kept a line break for an empty scalar with keep chomping, read lines of
// spaces otherwise and made two keys of "? >\n    \n \n:"; the values are
// YAML 1.2's (PyYAML and the YAML Test Suite agree), whatever the split
// and with or without the keep-chomping rewrite. A text that does not end
// with a line break reads as if it did, as the YAML Test Suite reads it
// (L24T/01, JEF9/02).
func TestBlockScalars(t *testing.T) {
	run := strings.Repeat("\n", longBlankRun+1)
	tests := []struct{ src, want string }{
		{"a: |-\n  x  \nb: 1\n", `{a:s:"x  ",b:i:1}`},
		{"a: >-\n  x  \nb: 1\n", `{a:s:"x  ",b:i:1}`},
		{"a: |\n  x  ", `{a:s:"x  \n"}`},
		{"a: |-\n : \n", `{a:s:": "}`},
		{"foo: |\n  x\n   ", `{foo:s:"x\n \n"}`},
		{"foo: |\n  x\n   \n", `{foo:s:"x\n \n"}`},
		{"a: |+\n", `{a:s:""}`},
		{"a: |-\n  x\n   \n", `{a:s:"x\n "}`},
		{"a: |-1\n  \n", `{a:s:" "}`},
		{"a: >-\n  a\n   \n \n", `{a:s:"a\n "}`},
		{"? >\n    \n \n:\n", `{:n}`},
		{"- |+\n   ", `[s:"\n"]`},
		{"a: |+\n  x\n\n\nb: 1\n", `{a:s:"x\n\n\n",b:i:1}`},
		{"a: |\n  x\n\n\nb: 1\n", `{a:s:"x\n",b:i:1}`},
		{"a: >\n  x\n  y\n\n  z\n   w\n  v\n", `{a:s:"x y\nz\n w\nv\n"}`},
		{"a: >\n \t\n detected\n", `{a:s:"\t\ndetected\n"}`},
		{"a: |2\n   x\n  y\n", `{a:s:" x\ny\n"}`},
		{"- |1\n  x\n", `[s:" x\n"]`},
		{"--- |\nroot\n# no comment\n...\n", `s:"root\n# no comment\n"`},
		{"a: |\n  x\n # c\nb: 1\n", `{a:s:"x\n",b:i:1}`},
		{"a: !!str |\n  x  \n", `{a:s:"x  \n"}`},
		{"a: !!str  \t >-\n  x  \n", `{a:s:"x  "}`},
		{"a: |\n  x" + run + "b: 1\n", `{a:s:"x\n",b:i:1}`},
		{"a: |-\n  x  " + run + "b: 1\n", `{a:s:"x  ",b:i:1}`},
		// YAML 1.2 refuses a leading empty line with more spaces than the
		// first line of content.
		{"a: |\n     \n  x\n", "RZ-CFG-001@1:4"},
		// goccy refused a keep-chomping scalar of blank lines before a less
		// indented line or a comment, which YAML 1.2 accepts, while it
		// accepted the same scalar at the end of the input; such a scalar is
		// scanned with strip chomping now (stripEmptyKeep; eighth WP-33
		// review), and its value is read from the source.
		{"a: |+\nb: 1\n", `{a:s:"",b:i:1}`},
		{"keep: |+\n\n# c\n", `{keep:s:"\n"}`},
		{"keep: |+\n\n\n# c\nb: 1\n", `{keep:s:"\n\n",b:i:1}`},
		{"keep: !!str >2+\n\nb: 1\n", `{keep:s:"\n",b:i:1}`},
		// goccy refused a block scalar with an indentation indicator before
		// blank lines that end the input (trimBlankTail).
		{"a: >2\n  x\nb: >2\n\n  y\n\n", `{a:s:"x\n",b:s:"\ny\n"}`},
		{"k:\n- |-2\n  y\n\n", `{k:[s:"y"]}`},
	}
	for _, tt := range tests {
		for _, o := range []Options{{splitAt: -1}, {}, {splitAt: 1}, {noRewrite: true}} {
			docs, diags, err := parse(t.Context(), []byte(tt.src), 0, "f.yaml", FormatYAML, o, true)
			if err != nil {
				t.Fatal(err)
			}
			got := codes(diags)
			if got == "" && len(docs) == 1 {
				got = show(docs[0].Root, false)
			}
			if got != tt.want {
				t.Errorf("%q, %+v: %s, want %s", tt.src, o, got, tt.want)
			}
		}
	}
	_, diags := parseYAML(t, "a: |\n     \n  x\n")
	if len(diags) != 1 || diags[0].Message != msgBlockLeadingSpaces {
		t.Errorf("message %+v", diags)
	}
}

// TestReadBlockScalar covers the block scalar reader on its own (01 req
// 12): the indentation of the collection around it, explicit indentation,
// the lines it holds, and document markers, which end it.
func TestReadBlockScalar(t *testing.T) {
	tests := []struct {
		text  string
		n     int
		value string
		lines int
	}{
		{"|\n x\n", -1, "x\n", 1},
		{"|\nx\n...\n", -1, "x\n", 1},
		{"|1\n x\n", -1, " x\n", 1},
		{"|2+\n   x\n\n", 0, " x\n\n", 2},
		{"|-\n  a\n\n  b\nc\n", 0, "a\n\nb", 3},
		{">\n  a\n  b\n\n\n  c\n", 1, "a b\n\nc\n", 5},
		{">+\n", 0, "", 0},
		{"|", 0, "", 0},
		{"| # c\n  x\n", 0, "x\n", 1},
	}
	for _, tt := range tests {
		value, lines, msg := readBlockScalar(tt.text, 0, tt.n)
		if value != tt.value || lines != tt.lines || msg != "" {
			t.Errorf("readBlockScalar(%q, %d) = %q, %d, %q; want %q, %d", tt.text, tt.n, value, lines, msg, tt.value, tt.lines)
		}
	}
}

// TestBlockScalarChecks covers what the token pass refuses about a block
// scalar (01 req 8): one inside a flow collection, one whose content goccy
// reads to another end than YAML 1.2, and a header with two chomping
// indicators or text after its indicators, which goccy read as the header
// before it (minor finding of the eighth WP-33 review).
func TestBlockScalarChecks(t *testing.T) {
	tests := []struct{ src, want, msg string }{
		{"a: [|\n x]\n", "RZ-CFG-001@1:5", msgBlockInFlow},
		{"k: |--\n  a\n", "RZ-CFG-001@1:4", msgBlockHeaderRest},
		{"k: |-- # c\n  a\n", "RZ-CFG-001@1:4", msgBlockHeaderRest},
		{"k: |+-\n  a\n\n", "RZ-CFG-001@1:4", msgBlockHeaderRest},
		{"k: !!str >+-\n \n \n", "RZ-CFG-001@1:10", msgBlockHeaderRest},
		{"k: |22\n   a\n", "RZ-CFG-001@1:4", ""},
		{"k: |1#c\n  a\n", "RZ-CFG-001@1:4", ""},
		{"? |++", "RZ-CFG-001@1:3", msgBlockHeaderRest},
		{"k: |2- # c\n  a\nl: >+1\n  b\nm: |\t# c\n  c\n", `{k:s:"a",l:s:" b\n",m:s:"c\n"}`, ""},
		// After a finding, the check is skipped and the scan goes on.
		{"a: &x 1\nb: [|\n x]\nc: !e 1\n", "RZ-CFG-003@1:4", ""},
	}
	for _, tt := range tests {
		docs, diags := parseYAML(t, tt.src)
		got := codes(diags)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, false)
		}
		if !strings.HasPrefix(got, tt.want) {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
		if tt.msg != "" && (len(diags) != 1 || diags[0].Message != tt.msg) {
			t.Errorf("%q: messages %+v", tt.src, diags)
		}
	}
	// The token pass compares the end goccy's tokens give a block scalar
	// with YAML 1.2's.
	p := &fileParser{path: "f.yaml", opts: Options{}.withDefaults()}
	pass := &tokenPass{p: p, maxDepth: 64, maxTokens: 100}
	src := "a: |\n  x\nb: 1\n"
	tks, _ := scanString(src)
	tks[4].Position.Line = 4 // the next key, as if goccy read it a line later
	pass.run(tks, 0, columnsOf(tks, src), src)
	if codes(p.diags) != "RZ-CFG-001@1:4" || p.diags[0].Message != msgBlockScalar {
		t.Errorf("end: %q %+v", codes(p.diags), p.diags)
	}
}
