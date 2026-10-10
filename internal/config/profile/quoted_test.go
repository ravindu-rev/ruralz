// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strings"
	"testing"
)

// TestQuotedScalars covers quoted scalars read from the source (01 req 10,
// 12; blocker of the sixth WP-33 review). goccy drops the spaces an escape
// gives before a line break, keeps the tabs before a line break of a
// single-quoted scalar, and accepts escapes YAML 1.2 does not define;
// these read as YAML 1.2 (and PyYAML, libyaml) reads them, whatever the
// split, and an undefined escape is RZ-CFG-001 at the scalar.
func TestQuotedScalars(t *testing.T) {
	tests := []struct{ src, want string }{
		{"k: \"\\ \n\"\n", `{k:s:"  "}`},
		{"k: \"\\ \n  \"\n", `{k:s:"  "}`},
		{"k: \"a\\ \n  b\"\n", `{k:s:"a  b"}`},
		{"k: \"a\\ \n\n  b\"\n", `{k:s:"a \nb"}`},
		{"k: \"a\\x20\n  b\"\n", `{k:s:"a  b"}`},
		{"k: \"a\\u0020\n  b\"\n", `{k:s:"a  b"}`},
		{"k: \"a\\U00000020\n  b\"\n", `{k:s:"a  b"}`},
		{"k: [\"a\\ \n  b\"]\n", `{k:[s:"a  b"]}`},
		{"k: \"a\\x20 \t\n  b\"\n", `{k:s:"a  b"}`},
		{"\"a\\ \n b\": 1\n", "RZ-CFG-001@2:4"},
		// Folding: white space before a line break goes, escaped or not
		// is the difference; an escaped line break keeps the white space
		// before it and joins the lines.
		{"k: \"a\t\n  b\"\n", `{k:s:"a b"}`},
		{"k: \"a\\t\n  b\"\n", `{k:s:"a\t b"}`},
		{"k: \"a\\\t\n  b\"\n", `{k:s:"a\t b"}`},
		{"k: \"a \\\n  b\"\n", `{k:s:"a b"}`},
		{"k: \"a\\\n  b\"\n", `{k:s:"ab"}`},
		{"k: \"a\\\n\n  b\"\n", `{k:s:"a\nb"}`},
		{"k: \"a\n\n\n  b\"\n", `{k:s:"a\n\nb"}`},
		{"k: \"  a  \"\n", `{k:s:"  a  "}`},
		{"k: \"a\n   \"\n", `{k:s:"a "}`},
		// Single quotes: no escapes but '', and the same folding.
		{"k: 'a\t\n  b'\n", `{k:s:"a b"}`},
		{"k: 'a \t \n  b'\n", `{k:s:"a b"}`},
		{"k: 'a ''b'' \n\n  c'\n", `{k:s:"a 'b'\nc"}`},
		{"k: 'a\\ \n  b'\n", `{k:s:"a\\ b"}`},
		// Escapes.
		{`k: "\0\a\b\t\n\v\f\r\e\ \"\/\\"` + "\n", `{k:s:"\x00\a\b\t\n\v\f\r\x1b \"/\\"}`},
		{`k: "\N\_\L\P\x41\u00e9\U0001F600\uD83D\uDE00"` + "\n", `{k:s:"\u0085\u00a0\u2028\u2029Aé😀😀"}`},
		{`k: "a\xZZb"` + "\n", "RZ-CFG-001@1:4"},
		{`k: "\uDC00"` + "\n", "RZ-CFG-001@1:4"},
		{`k: "\uD800\u0041"` + "\n", "RZ-CFG-001@1:5"}, // goccy refuses it at the escape
		{`k: "\U00110000"` + "\n", "RZ-CFG-001@1:4"},
		{`k: "\UFFFFFFFF"` + "\n", "RZ-CFG-001@1:4"},
		{`k: "\U0000D800"` + "\n", "RZ-CFG-001@1:4"},
		{`k: "\x4"` + "\n", "RZ-CFG-001@1:4"},
		// A tag before the scalar moves goccy's columns (columns.go).
		{"k: !!str \"a\\ \n  b\"\n", `{k:s:"a  b"}`},
		{"k: [!!str 'a', !!str \"b\\x20\"]\n", `{k:[s:"a",s:"b "]}`},
	}
	for _, tt := range tests {
		for _, splitAt := range []int{-1, 0, 3} {
			docs, got := parseWith(t, tt.src, splitAt)
			if got == "" && len(docs) == 1 {
				got = show(docs[0].Root, false)
			}
			if got != tt.want {
				t.Errorf("%q, split at %d: %s, want %s", tt.src, splitAt, got, tt.want)
			}
		}
	}
	_, diags := parseYAML(t, `k: "a\xZZb"`+"\n")
	if len(diags) != 1 || diags[0].Message != `syntax error: invalid \x escape in a double-quoted scalar` {
		t.Errorf("message %+v", diags)
	}
}

// TestReadQuoted covers the quoted scalar reader on its own (01 req 12):
// unterminated scalars, an unknown escape, and the offset past the closing
// quote.
func TestReadQuoted(t *testing.T) {
	tests := []struct {
		text, value, msg string
		end              int
	}{
		{`"a" b`, "a", "", 3},
		{`'a''b' c`, "a'b", "", 6},
		{`"a`, "", msgQuoteOpen, 0},
		{`"a\`, "", msgQuoteOpen, 0},
		{`'a`, "", msgQuoteOpen, 0},
		{`"\q"`, "", `syntax error: unknown escape \q in a double-quoted scalar`, 0},
		{`"\é"`, "", `syntax error: unknown escape \é in a double-quoted scalar`, 0},
	}
	for _, tt := range tests {
		value, end, msg := readQuoted(nil, tt.text, 0)
		if string(value) != tt.value || end != tt.end || msg != tt.msg {
			t.Errorf("readQuoted(%q) = %q, %d, %q; want %q, %d, %q", tt.text, value, end, msg, tt.value, tt.end, tt.msg)
		}
	}
	if !strings.Contains(msgQuoteEnd, "closing quote") || !strings.Contains(msgQuoteStart, "opening quote") {
		t.Error("messages")
	}
}

// TestTabsInDoubleQuotes covers a tab inside a double-quoted scalar (01
// req 8, 12, 14; found by FuzzLoadYAML's appended-content check in the
// eighth WP-33 review). goccy's scanner stepped one character past the
// scalar's end for each such tab, losing the line break after it: it
// refused "a: \"x\ty\"\nb: 1", read a "..." line after the scalar as a
// plain scalar, and reported every later token a line too early. The
// scalar is scanned with its tabs as spaces (untabQuotes) and read from the
// source, so its value keeps them. A tab that indents a continuation line
// stays refused, as YAML 1.2 refuses it (YAML Test Suite DK95/01).
func TestTabsInDoubleQuotes(t *testing.T) {
	tests := []struct{ src, want string }{
		{"a: \"x\ty\"\nb: 1\n", `{a@1:1:s:"x\ty"@1:4,b@2:1:i:1@2:4}`},
		{"a: \"x\ty\"\n\nb: 1\n", `{a@1:1:s:"x\ty"@1:4,b@3:1:i:1@3:4}`},
		{"a: \"x\t\t\ty\"\n\n\nb:\n  c: 1\n", `{a@1:1:s:"x\t\t\ty"@1:4,b@4:1:{c@5:3:i:1@5:6}}`},
		{"k:\n- \"x\ty\"\n- z\n", `{k@1:1:[s:"x\ty"@2:3,s:"z"@3:3]}`},
		{"k: \"a\tb\"\n...\n", `{k@1:1:s:"a\tb"@1:4}`},
		{"foo: \"bar\n \t \t baz \t \t \"\n# c\nz: 1\n", `{foo@1:1:s:"bar baz \t \t "@1:6,z@4:1:i:1@4:4}`},
		{"k: [\"a\tb\", c]\nl: 1\n", `{k@1:1:[s:"a\tb"@1:5,s:"c"@1:12],l@2:1:i:1@2:4}`},
		{"k: \"\\\ta\"\nl: 1\n", `{k@1:1:s:"\ta"@1:4,l@2:1:i:1@2:4}`},
		{"foo: \"bar\n\tbaz\"\n", "RZ-CFG-001@2:1"},
	}
	for _, tt := range tests {
		for _, o := range []Options{{}, {splitAt: -1}, {noRewrite: true}} {
			docs, diags, err := parse(t.Context(), []byte(tt.src), 0, "f.yaml", FormatYAML, o, false)
			if err != nil {
				t.Fatal(err)
			}
			got := codes(diags)
			if got == "" && len(docs) == 1 {
				got = show(docs[0].Root, true)
			}
			if got != tt.want {
				t.Errorf("%q, %+v: %s, want %s", tt.src, o, got, tt.want)
			}
		}
	}
	if got, n := untabQuotes("a: \"x\ty\" # \"\t\"\nb: |\n  \"\t\"\nc: \"d\n\te\"\n"); n != 1 || got != "a: \"x y\" # \"\t\"\nb: |\n  \"\t\"\nc: \"d\n\te\"\n" {
		t.Errorf("untabQuotes: %q, %d", got, n)
	}
}
