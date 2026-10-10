// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strings"
	"testing"
)

// TestMultiLinePlainScalars covers plain scalars over several lines (01
// req 8, 10, 12, 14; blocker of the sixth WP-33 review). goccy scans the
// rest of one with a line that starts with '-' as folded block text: it
// kept a comment and ": " in the value, kept line breaks and spaces, and
// reported the scalar at its last line; and it folded empty lines of
// spaces into one line feed. Such a scalar is now read from the source as
// YAML 1.2 (and PyYAML) reads it, at its first character, whatever the
// split; a comment ends it, and ": " or a line of it after a comment is
// RZ-CFG-001 at the offending text.
func TestMultiLinePlainScalars(t *testing.T) {
	tests := []struct{ src, want string }{
		{"description: run the job\n  -v # verbose\nimage: x\n", `{description@1:1:s:"run the job -v"@1:14,image@3:1:s:"x"@3:8}`},
		{"k: x\n  -1 # c\n", `{k@1:1:s:"x -1"@1:4}`},
		{"k:\n  x\n  -y # c\n", `{k@1:1:s:"x -y"@2:3}`},
		{"k: a\n  -b #c\n", `{k@1:1:s:"a -b"@1:4}`},
		{"k: y\n  - z #c\n", `{k@1:1:s:"y - z"@1:4}`},
		{"k: y\n  - #c\n", `{k@1:1:s:"y -"@1:4}`},
		{"x:\n- a\n  - b #c\n", `{x@1:1:[s:"a - b"@2:3]}`},
		{"command: echo\n  --flag # note\nb: 1\n", `{command@1:1:s:"echo --flag"@1:10,b@3:1:i:1@3:4}`},
		// The text was right, the position was not.
		{"k: x\n  -y\n", `{k@1:1:s:"x -y"@1:4}`},
		{"k: a\n  - b\n", `{k@1:1:s:"a - b"@1:4}`},
		{"k: a\n  -b\n", `{k@1:1:s:"a -b"@1:4}`},
		{"- single multiline\n - sequence entry\n", `[s:"single multiline - sequence entry"@1:3]`},
		// Line breaks and spaces fold as a plain scalar's.
		{"k: a\n  -b\n    c\n", `{k@1:1:s:"a -b c"@1:4}`},
		{"k: a  \n  -b  \n  c\n", `{k@1:1:s:"a -b c"@1:4}`},
		{"k: a\n\n  -b\n\n\n  c\n", `{k@1:1:s:"a\n-b\n\nc"@1:4}`},
		{"k: a\n  -b\n  #c\n", `{k@1:1:s:"a -b"@1:4}`},
		{"k: a\n  -b # c\n  # d\nz: 1\n", `{k@1:1:s:"a -b"@1:4,z@4:1:i:1@4:4}`},
		{"k: !!str a\n  -b # c\n", `{k@1:1:s:"a -b"@1:4}`},
		{"? a\n  -b # c\n: v\n", `{a -b@1:3:s:"v"@3:3}`},
		{"- a\n  -b\n- c\n", `[s:"a -b"@1:3,s:"c"@3:3]`},
		// Empty lines of spaces, which goccy folded into one line feed,
		// and scalars that the next token ends on their last line.
		{"k: a\n    \n  \n  c\n", `{k@1:1:s:"a\n\nc"@1:4}`},
		{"k: a\n  \n  c\n", `{k@1:1:s:"a\nc"@1:4}`},
		{"k: [a\n  b, c\n  d]\n", `{k@1:1:[s:"a b"@1:5,s:"c d"@2:6]}`},
		{"k: [a\n\n  b]\n", `{k@1:1:[s:"a\nb"@1:5]}`},
		{"k: a\n  b # c\nz: 1\n", `{k@1:1:s:"a b"@1:4,z@3:1:i:1@3:4}`},
		{"a: 1\n b: 2\n", "RZ-CFG-001@2:3"},
		// YAML 1.2 refuses these.
		{"k: a\n  - b: c\n", "RZ-CFG-001@2:6"},
		{"k: a\n  -b: c\n", "RZ-CFG-001@2:5"},
		{"k: a\n  -b:\n", "RZ-CFG-001@2:5"},
		{"k: a\n  -b #c\n  d\n", "RZ-CFG-001@3:3"},
		{"k: a\n  -b # c\n  # d\n  e\n", "RZ-CFG-001@4:3"},
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
	_, diags := parseYAML(t, "k: a\n  - b: c\n")
	if len(diags) != 1 || diags[0].Message != msgPlainColon {
		t.Errorf("message %+v", diags)
	}
	_, diags = parseYAML(t, "k: a\n  -b #c\n  d\n")
	if len(diags) != 1 || diags[0].Message != msgPlainComment {
		t.Errorf("message %+v", diags)
	}
}

// TestPlainScalarsBeforeTrailers covers plain scalars whose value goccy
// reports at another position than their first character, followed by
// content that changed the split parse's result (01 req 8, 10, 12, 14;
// minor finding of the eighth WP-33 review). goccy reports a plain scalar
// continued by a line starting with '-' at its last line, column 1 when
// blank lines end the document, and one with trailing spaces too far
// right; the split parse checked a value's placement with goccy's
// positions, so a final blank line, "..." or a document marker refused
// valid YAML, and trailing spaces accepted a value left of its key. The
// split parse uses source positions now, at every threshold that splits
// the document; goccy's own parse (split at -1, and at 3, which leaves
// these narrow mappings whole) gives goccy's reading, recorded in goccy.
func TestPlainScalarsBeforeTrailers(t *testing.T) {
	args := `{spec@1:1:{args@2:3:[s:"run the job -v"@3:5]}}`
	tests := []struct{ src, want, goccy string }{
		{"spec:\n  args:\n  - run the job\n    -v\n", args, ""},
		{"spec:\n  args:\n  - run the job\n    -v\n\n", args, ""},
		{"spec:\n  args:\n  - run the job\n    -v\n...\n", args, "RZ-CFG-001@3:5"},
		{"spec:\n  args:\n  - run the job\n    -v # verbose\n\n", args, ""},
		{"spec:\n  args:\n  - run the job\n    -v\nkind: x\n", `{spec@1:1:{args@2:3:[s:"run the job -v"@3:5]},kind@5:1:s:"x"@5:7}`, ""},
		{"spec:\n  command: run the job\n    -v\n\n", `{spec@1:1:{command@2:3:s:"run the job -v"@2:12}}`, ""},
		{"spec:\n  command: run the job\n    -v\n...\n", `{spec@1:1:{command@2:3:s:"run the job -v"@2:12}}`, "RZ-CFG-001@2:12"},
		{"- a: x\n   -y\n\n", `[{a@1:3:s:"x -y"@1:6}]`, ""},
		{"a:\n b: x\n  - y\n\n", `{a@1:1:{b@2:2:s:"x - y"@2:5}}`, ""},
		{"x:\n  k1: a\n   -b\n\n", `{x@1:1:{k1@2:3:s:"a -b"@2:7}}`, ""},
		{"x:\n  - a\n    - b\n\n", `{x@1:1:[s:"a - b"@2:5]}`, ""},
		// A value left of its key, which yaml.v3 refuses too.
		{"spec:\n  name:\n value\n", "RZ-CFG-001@3:2", ""},
		{"spec:\n  name:\n value \n", "RZ-CFG-001@3:2", `{spec@1:1:{name@2:3:s:"value"@3:2}}`},
		{"x:\n  k:\n a   \ny: 1\n", "RZ-CFG-001@3:2", `{x@1:1:{k@2:3:s:"a"@3:2},y@4:1:i:1@4:4}`},
		{"spec:\n  items:\n  -\n x \n", "RZ-CFG-001@4:2", `{spec@1:1:{items@2:3:[s:"x"@4:2]}}`},
	}
	for _, tt := range tests {
		for _, splitAt := range []int{-1, 0, 1, 3} {
			docs, got := parseWith(t, tt.src, splitAt)
			if got == "" && len(docs) == 1 {
				got = show(docs[0].Root, true)
			}
			want := tt.want
			if (splitAt < 0 || splitAt == 3) && tt.goccy != "" {
				want = tt.goccy
			}
			if strings.HasPrefix(got, "RZ-") && strings.HasPrefix(want, "RZ-") {
				got, want = strings.Fields(got)[0], strings.Fields(want)[0]
			}
			if got != want {
				t.Errorf("%q, split at %d: %s, want %s", tt.src, splitAt, got, want)
			}
		}
	}
}

// TestMisreadPlainTokens covers plain tokens goccy scans out of other
// nodes (01 req 8, 10, 12; blocker of the sixth WP-33 review). After an
// empty block scalar and a blank line, goccy scans the next line's first
// node as plain text: a quoted key kept its quotes, an anchor, an alias or
// a tag became part of a key with no RZ-CFG-003 or RZ-CFG-004, and a flow
// collection a text key. Such a token is RZ-CFG-001, except that one
// starting with an anchor or alias is RZ-CFG-003 and one starting with a
// tag other than a core tag RZ-CFG-004. '?' at the end of its line, which
// goccy scans with the next line as the text "? a", gets an explicit-key
// message (minor finding of that review).
func TestMisreadPlainTokens(t *testing.T) {
	tests := []struct{ src, want, msg string }{
		{"x: |\n\n\"q\": 1\n", "RZ-CFG-001@3:1", `syntax error: unexpected '"'`},
		{"x: |\n\n'q': 1\n", "RZ-CFG-001@3:1", `syntax error: unexpected '\''`},
		{"x: |\n\n&a q: 1\n", "RZ-CFG-003@3:1", "anchors are not allowed (&a)"},
		{"x: |\n\n*a : 1\n", "RZ-CFG-003@3:1", "aliases are not allowed (*a)"},
		{"x: >-\n\n!e x: 1\n", "RZ-CFG-004@3:1", "tag !e is not allowed; only the YAML 1.2 core tags are"},
		{"x: |\n\n!!str b: 1\n", "RZ-CFG-001@3:1", "syntax error: unexpected '!'"},
		{"x: |\n\n[a]: b\n", "RZ-CFG-001@3:1", "syntax error: unexpected '['"},
		{"x: |\n\n{a: b}\n", "RZ-CFG-001@3:1", "syntax error: unexpected '{'"},
		{"spec:\n  x: |\n\n  \"q\": 1\n", "RZ-CFG-001@4:3", `syntax error: unexpected '"'`},
		{"x: |\n  \n\"q\": 1\n", "RZ-CFG-001@3:1", `syntax error: unexpected '"'`},
		{"x: |-\n\n\n\"a\": 1\n\"a\": 2\n", "RZ-CFG-001@4:1", `syntax error: unexpected '"'`},
		// The anchor is reported, and the scan goes on collecting.
		{"x: |\n\n&a q: 1\ny: !e 2\n", "RZ-CFG-003@3:1 RZ-CFG-004@4:4", ""},
		// An explicit key with its key on the next line.
		{"?\n  a\n: b\n", "RZ-CFG-001@1:1", msgExplicitKeyLine},
		// Valid: the line after a comment, after a non-empty block scalar,
		// or with no blank line, scans as written.
		{"x: |\n\"q\": 1\n", `{x:s:"",q:i:1}`, ""},
		{"x: |\n# c\n\n\"q\": 1\n", `{x:s:"",q:i:1}`, ""},
		{"x: |\n  t\n\n\"q\": 1\n", `{x:s:"t\n",q:i:1}`, ""},
		{"x: |\n\nq: 1\n", `{x:s:"",q:i:1}`, ""},
		{"? |\n\n: v\n", `{:s:"v"}`, ""},
		{"x: |\n\n# c\nq: 1\n", `{x:s:"",q:i:1}`, ""},
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
		if tt.msg == "" {
			continue
		}
		if _, diags := parseYAML(t, tt.src); len(diags) == 0 || diags[0].Message != tt.msg {
			t.Errorf("%q: messages %+v, want %q", tt.src, diags, tt.msg)
		}
	}
}

// TestPlainTokenChecks covers the remaining checks of a plain token that
// no valid YAML 1.2 plain scalar fails (01 req 8): goccy produces such
// tokens only where it misreads the source.
func TestPlainTokenChecks(t *testing.T) {
	tk := func(v string) string {
		p := &fileParser{path: "f.yaml", opts: Options{}.withDefaults()}
		pass := &tokenPass{p: p, maxDepth: 64, maxTokens: 100}
		tks, _ := scanString("a: x\n")
		pass.tks, pass.src = tks, newCursor("a: x\n")
		tks[2].Value = v // goccy's text, which is one line in the source
		pass.plain(2)
		if len(p.diags) == 0 {
			return ""
		}
		return p.diags[0].Message
	}
	tests := []struct{ value, want string }{
		{"", msgPlainSpace},
		{" a", msgPlainSpace},
		{"a\n b", msgPlainSpace},
		{"a #b", msgPlainComment},
		{"a: b", msgPlainColon},
		{"- a", "syntax error: unexpected '-'"},
		{"%a", "syntax error: unexpected '%'"},
		{"a::", ""},
		{"a#b", ""},
		{"-a", ""},
	}
	for _, tt := range tests {
		if got := tk(tt.value); got != tt.want {
			t.Errorf("plain %q: %q, want %q", tt.value, got, tt.want)
		}
	}
	// In a flow collection a flow indicator cannot be inside the scalar.
	_, diags := parseYAML(t, "k: [a\n  -b ]\n")
	if len(diags) != 1 || diags[0].Code != codeParse {
		t.Errorf("flow: %q", codes(diags))
	}
	if strings.Contains(codes(diags), "RZ-CFG-002") {
		t.Error(codes(diags))
	}
}

// TestTabsInPlainScalars covers a tab inside a plain scalar on one line
// (01 req 11, 12, 15; blocker of the seventh WP-33 review). goccy's
// scanner left it out of the value, so "x: 1\t2" read as the integer 12,
// "tr\tue" as true, the keys "bc" and "b\tc" as duplicates, and a
// mismatched tag passed. The value is read from the source now, typed by
// the core schema from that text, at the default threshold, with the
// threshold raised and with goccy's own parse.
func TestTabsInPlainScalars(t *testing.T) {
	tests := []struct{ src, want string }{
		{"x: 1\t2\n", `{x:s:"1\t2"}`},
		{"x: tr\tue\n", `{x:s:"tr\tue"}`},
		{"x: .in\tf\n", `{x:s:".in\tf"}`},
		{"metadata:\n  name: orders\t-v2\n", `{metadata:{name:s:"orders\t-v2"}}`},
		{"bc: 1\nb\tc: 2\n", "{bc:i:1,b\tc:i:2}"},
		{"b\tc: 1\nb\tc: 2\n", "RZ-CFG-002@2:1"},
		{"when: request.method\t== 'GET'\n", `{when:s:"request.method\t== 'GET'"}`},
		{"x: !!int 1\t2\n", "RZ-CFG-001@1:4"},
		{"x: !!str 1\t2\n", `{x:s:"1\t2"}`},
		{"x: [a\tb, c]\n", `{x:[s:"a\tb",s:"c"]}`},
		{"x: {a\tb: c\td}\n", "{x:{a\tb:s:\"c\\td\"}}"},
		{"x: b \tc\n", `{x:s:"b \tc"}`},
		{"x: b\t \tc # d\n", `{x:s:"b\t \tc"}`},
		{"x: b\t\n", `{x:s:"b"}`},
		{"x: a\tb\n  c\td\n", `{x:s:"a\tb c\td"}`},
		{"- 1\t2\n- 3\n", `[s:"1\t2",i:3]`},
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
	// The JSON twin of a tab in a string.
	jd, jdiags := parseJSON(t, "{\"x\":\"1\\t2\"}")
	yd, ydiags := parseYAML(t, "x: 1\t2\n")
	if len(jdiags) != 0 || len(ydiags) != 0 || show(jd[0].Root, false) != show(yd[0].Root, false) {
		t.Errorf("twins: %q %q", codes(jdiags), codes(ydiags))
	}
	// A token whose source holds other text than goccy's value with its
	// tabs is refused, not misread.
	p := &fileParser{path: "f.yaml", opts: Options{}.withDefaults()}
	pass := &tokenPass{p: p, maxDepth: 64, maxTokens: 100}
	tks, _ := scanString("a: x\ty\n")
	pass.tks, pass.src, pass.cols = tks, newCursor("a: x\ty\n"), columnsOf(tks, "a: x\ty\n")
	tks[2].Value = "xz"
	if pass.plain(2); codes(p.diags) != "RZ-CFG-001@1:4" || p.diags[0].Message != msgPlainStart {
		t.Errorf("other text: %+v", p.diags)
	}
}

// TestLoneQuestionMark covers a '?' that a line break or a flow
// indicator follows (01 req 8, 10, 12, 13; 11 req 18; blocker of the
// seventh WP-33 review). goccy scans it as the plain scalar "?", where
// YAML 1.2 reads an explicit key indicator with an empty key ("b:\n?" is
// {b: null, null: null}), as "? # c" is, and refuses "x: ?". It is
// RZ-CFG-001 with the empty-key message, nested or not, at every
// threshold, alone and after 300 sibling keys; before ':' it is a valid
// plain scalar.
func TestLoneQuestionMark(t *testing.T) {
	tests := []struct{ src, want string }{
		{"b:\n?\n", "RZ-CFG-001@2:1"},
		{"a:\n  ?\n", "RZ-CFG-001@2:3"},
		{"x:\n  ?\n", "RZ-CFG-001@2:3"},
		{"a:\n  ? # c\n", "RZ-CFG-001@2:3"},
		{"a:\n  b:\n  ?\nc: 1\n", "RZ-CFG-001@3:3"},
		{"a:\n  b:\n    c:\n      ?\n", "RZ-CFG-001@4:7"},
		{"a:\n  - ?\n", "RZ-CFG-001@2:5"},
		{"a:\n- ?\n", "RZ-CFG-001@2:3"},
		{"- ?\n- x\n", "RZ-CFG-001@1:3"},
		{"a:\n?\n# c\n", "RZ-CFG-001@2:1"},
		{"a:\n?\n...\n", "RZ-CFG-001@2:1"},
		{"x: {a: 1, ?\n}\n", "RZ-CFG-001@1:11"},
		{"x: ?\n", "RZ-CFG-001@1:4"},
		{"x: [?]\n", "RZ-CFG-001@1:5"},
		{"x: {?}\n", "RZ-CFG-001@1:5"},
		{"x: [a, ?]\n", "RZ-CFG-001@1:8"},
		{"?\n", "RZ-CFG-001@1:1"},
		{"?: x\n", `{?:s:"x"}`},
		{"x: {?: y}\n", `{x:{?:s:"y"}}`},
		{"x: ?y\n", `{x:s:"?y"}`},
	}
	for _, tt := range tests {
		for _, splitAt := range []int{-1, 0, 2, 3, 256} {
			docs, got := parseWith(t, tt.src, splitAt)
			if got == "" && len(docs) == 1 {
				got = show(docs[0].Root, false)
			}
			if got != tt.want {
				t.Errorf("%q, split at %d: %s, want %s", tt.src, splitAt, got, tt.want)
			}
		}
		if _, diags := parseYAML(t, tt.src); strings.HasPrefix(tt.want, "RZ-") && (len(diags) != 1 || diags[0].Message != msgEmptyExplicitKey) {
			t.Errorf("%q: %+v, want the message %q", tt.src, diags, msgEmptyExplicitKey)
		}
		widthIndependent(t, []byte(tt.src), 300)
	}
}
