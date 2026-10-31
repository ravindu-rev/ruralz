// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"testing"
)

// TestQuotedScalars covers quoted scalars: the spaces an escape gives
// before a line break stay, the white space before an unescaped line break
// goes, and an escape YAML 1.2 does not define is RZ-CFG-001 at the scalar,
// as YAML 1.2 (and PyYAML, libyaml) read them.
func TestQuotedScalars(t *testing.T) {
	tests := []struct{ src, want string }{
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
		{`k: "\uD800\u0041"` + "\n", "RZ-CFG-001@1:4"},
		{`k: "\U00110000"` + "\n", "RZ-CFG-001@1:4"},
		{`k: "\UFFFFFFFF"` + "\n", "RZ-CFG-001@1:4"},
		{`k: "\U0000D800"` + "\n", "RZ-CFG-001@1:4"},
		{`k: "\x4"` + "\n", "RZ-CFG-001@1:4"},
		// A tag before the scalar.
		{"k: !!str \"a\\ \n  b\"\n", `{k:s:"a  b"}`},
		{"k: [!!str 'a', !!str \"b\\x20\"]\n", `{k:[s:"a",s:"b "]}`},
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
	_, diags := parseYAML(t, `k: "a\xZZb"`+"\n")
	if len(diags) != 1 || diags[0].Message != `syntax error: invalid \x escape in a double-quoted scalar` {
		t.Errorf("message %+v", diags)
	}
}

// TestQuotedLines covers the lines of a quoted scalar after its first (YAML
// 1.2.2 section 7.3, s-flow-line-prefix): in block context they are
// indented more than the block collection holding the scalar, with spaces,
// and no document marker starts one, so YAML Test Suite QB6E, DK95/01 and
// 5TRB are refused; a quoted scalar ends its node, so a comment after it
// needs white space before its '#'.
func TestQuotedLines(t *testing.T) {
	tests := []struct{ src, want, msg string }{
		// QB6E: the lines at the mapping's column.
		{"quoted: \"a\nb\nc\"\n", "RZ-CFG-001@2:1", msgQuotedIndent},
		{"quoted: 'a\nb'\n", "RZ-CFG-001@2:1", msgQuotedIndent},
		{"a:\n  b: \"x\n  y\"\n", "RZ-CFG-001@3:3", msgQuotedIndent},
		{"- \"x\ny\"\n", "RZ-CFG-001@2:1", msgQuotedIndent},
		// An empty line needs no indentation; the closing quote does.
		{"k: \"a\n\n  b\"\n", `{k:s:"a\nb"}`, ""},
		{"k: \"a\n\"\n", "RZ-CFG-001@2:1", msgQuotedIndent},
		{"k: \"a\n \"\n", `{k:s:"a "}`, ""},
		// At the document level any indentation does.
		{"\"a\nb\"\n", `s:"a b"`, ""},
		{"--- \"a\nb\"\n", `s:"a b"`, ""},
		// In a flow collection the block collection around it counts.
		{"k: [\"a\nb\"]\n", "RZ-CFG-001@2:1", msgQuotedIndent},
		{"k: [\"a\n b\"]\n", `{k:[s:"a b"]}`, ""},
		// DK95/01: a tab does not indent a line; the finding is at the
		// line's first character after its white space.
		{"foo: \"bar\n\tbaz\"\n", "RZ-CFG-001@2:2", msgQuotedIndent},
		{"k: \"\\ \n\"\n", "RZ-CFG-001@2:1", msgQuotedIndent},
		{"foo: \"bar\n \tbaz\"\n", `{foo:s:"bar baz"}`, ""},
		// 5TRB: a document marker inside the scalar.
		{"--- \"\n---\n\"\n", "RZ-CFG-001@2:1", msgQuotedMarker},
		{"k: \"a\n...\n\"\n", "RZ-CFG-001@2:1", msgQuotedMarker},
		{"\"a\n--- b\"\n", "RZ-CFG-001@2:1", msgQuotedMarker},
		// "---x" is not a marker.
		{"\"a\n---x\"\n", `s:"a ---x"`, ""},
		// A comment needs white space before it.
		{"a: \"x\"#c\n", "RZ-CFG-001@1:7", msgCommentSpace},
		{"a: 'x'#c\n", "RZ-CFG-001@1:7", msgCommentSpace},
		{"a: [\"x\"#c\n]\n", "RZ-CFG-001@1:8", msgCommentSpace},
		{"\"\t0\"#:\n", "RZ-CFG-001@1:5", msgCommentSpace},
		{"a: \"x\" #c\n", `{a:s:"x"}`, ""},
		{"a: \"x\"\t#c\n", `{a:s:"x"}`, ""},
		// Text after the closing quote.
		{"a: \"x\"y\n", "RZ-CFG-001@1:7", msgAfterNode},
		{"a: \"x\" y\n", "RZ-CFG-001@1:8", msgUnexpected},
		{"a: [\"x\"y]\n", "RZ-CFG-001@1:8", msgAfterFlowNode},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, false)
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
		if _, diags := parseYAML(t, tt.src); tt.msg != "" && (len(diags) == 0 || diags[0].Message != tt.msg) {
			t.Errorf("%q: first finding %q, want the message %q", tt.src, codes(diags), tt.msg)
		}
	}
}

// TestReadQuoted covers the quoted scalar reader on its own: unterminated
// scalars, an unknown escape, and the offset past the closing quote.
func TestReadQuoted(t *testing.T) {
	tests := []struct {
		text, value, msg string
		end              int
	}{
		{`"a" b`, "a", "", 3},
		{`'a''b' c`, "a'b", "", 6},
		{`''`, "", "", 2},
		{`''''`, "'", "", 4},
		{`"a`, "", msgQuoteOpen, 0},
		{`"a\`, "", msgQuoteOpen, 0},
		{`'a`, "", msgQuoteOpen, 0},
		{`'a''`, "", msgQuoteOpen, 0},
		{`"\q"`, "", `syntax error: unknown escape \q in a double-quoted scalar`, 0},
		{`"\é"`, "", `syntax error: unknown escape \é in a double-quoted scalar`, 0},
		{`"\uD800\u"`, "", `syntax error: invalid \u escape in a double-quoted scalar`, 0},
		{`"\uD800\uDBFF"`, "", `syntax error: invalid \u escape in a double-quoted scalar`, 0},
	}
	for _, tt := range tests {
		value, end, msg := readQuoted(nil, tt.text, 0)
		if string(value) != tt.value || end != tt.end || msg != tt.msg {
			t.Errorf("readQuoted(%q) = %q, %d, %q; want %q, %d, %q", tt.text, value, end, msg, tt.value, tt.end, tt.msg)
		}
	}
}

// TestTabsInDoubleQuotes covers tabs inside a quoted scalar: the value
// keeps them, the positions after the scalar stay right, and a "..." line
// after it is a marker.
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
		{"k: 'a\tb'\nl: 1\n", `{k@1:1:s:"a\tb"@1:4,l@2:1:i:1@2:4}`},
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
