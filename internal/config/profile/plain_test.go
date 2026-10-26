// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strings"
	"testing"
)

// TestMultiLinePlainScalars covers plain scalars over several lines (YAML
// 1.2.2 section 7.3.3): a line that starts with '-' continues the scalar,
// line breaks and spaces fold, a comment ends it, the scalar starts at its
// first character, and ": " on a later line is a multi-line implicit key.
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
		{"k: x\n  -y\n", `{k@1:1:s:"x -y"@1:4}`},
		{"k: a\n  - b\n", `{k@1:1:s:"a - b"@1:4}`},
		{"k: a\n  -b\n", `{k@1:1:s:"a -b"@1:4}`},
		{"- single multiline\n - sequence entry\n", `[s:"single multiline - sequence entry"@1:3]`},
		// Line breaks and spaces fold.
		{"k: a\n  -b\n    c\n", `{k@1:1:s:"a -b c"@1:4}`},
		{"k: a  \n  -b  \n  c\n", `{k@1:1:s:"a -b c"@1:4}`},
		{"k: a\n\n  -b\n\n\n  c\n", `{k@1:1:s:"a\n-b\n\nc"@1:4}`},
		{"k: a\n  -b\n  #c\n", `{k@1:1:s:"a -b"@1:4}`},
		{"k: a\n  -b # c\n  # d\nz: 1\n", `{k@1:1:s:"a -b"@1:4,z@4:1:i:1@4:4}`},
		{"k: !!str a\n  -b # c\n", `{k@1:1:s:"a -b"@1:4}`},
		{"? a\n  -b # c\n: v\n", `{a -b@1:3:s:"v"@3:3}`},
		{"- a\n  -b\n- c\n", `[s:"a -b"@1:3,s:"c"@3:3]`},
		{"k: a\n    \n  \n  c\n", `{k@1:1:s:"a\n\nc"@1:4}`},
		{"k: a\n  \n  c\n", `{k@1:1:s:"a\nc"@1:4}`},
		{"k: [a\n  b, c\n  d]\n", `{k@1:1:[s:"a b"@1:5,s:"c d"@2:6]}`},
		{"k: [a\n\n  b]\n", `{k@1:1:[s:"a\nb"@1:5]}`},
		{"k: [a\n  -b ]\n", `{k@1:1:[s:"a -b"@1:5]}`},
		{"k: a\n  b # c\nz: 1\n", `{k@1:1:s:"a b"@1:4,z@3:1:i:1@3:4}`},
		// A continuation line indented as far as its mapping's keys is the
		// next key, at the root a second node.
		{"a: 1\n b: 2\n", "RZ-CFG-001@2:3"},
		{"a: x\nb\n", "RZ-CFG-001@2:1"},
		{"x\ny\n", `s:"x y"@1:1`},
		// YAML 1.2 refuses these.
		{"k: a\n  - b: c\n", "RZ-CFG-001@2:6"},
		{"k: a\n  -b: c\n", "RZ-CFG-001@2:5"},
		{"k: a\n  -b:\n", "RZ-CFG-001@2:5"},
		{"k: a\n  -b #c\n  d\n", "RZ-CFG-001@3:3"},
		{"k: a\n  -b # c\n  # d\n  e\n", "RZ-CFG-001@4:3"},
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
	for _, src := range []string{"k: a\n  - b: c\n", "k: a\n  -b:\n"} {
		if _, diags := parseYAML(t, src); len(diags) != 1 || diags[0].Message != msgMultiLineKey {
			t.Errorf("%q: %+v", src, diags)
		}
	}
	for _, src := range []string{"k: a\n  -b #c\n  d\n", "k: a\n  -b # c\n  # d\n  e\n"} {
		if _, diags := parseYAML(t, src); len(diags) != 1 || diags[0].Message != msgIndentMoreMap {
			t.Errorf("%q: %+v", src, diags)
		}
	}
}

// TestPlainScalarPlacement covers a plain scalar before the end of its
// document or the next entry, and one left of the key it would belong to:
// the value is at its first character, and a line indented less than its
// key's mapping but more than the mapping around it matches no collection.
func TestPlainScalarPlacement(t *testing.T) {
	args := `{spec@1:1:{args@2:3:[s:"run the job -v"@3:5]}}`
	tests := []struct{ src, want string }{
		{"spec:\n  args:\n  - run the job\n    -v\n", args},
		{"spec:\n  args:\n  - run the job\n    -v\n\n", args},
		{"spec:\n  args:\n  - run the job\n    -v\n...\n", args},
		{"spec:\n  args:\n  - run the job\n    -v # verbose\n\n", args},
		{"spec:\n  args:\n  - run the job\n    -v\nkind: x\n", `{spec@1:1:{args@2:3:[s:"run the job -v"@3:5]},kind@5:1:s:"x"@5:7}`},
		{"spec:\n  command: run the job\n    -v\n\n", `{spec@1:1:{command@2:3:s:"run the job -v"@2:12}}`},
		{"spec:\n  command: run the job\n    -v\n...\n", `{spec@1:1:{command@2:3:s:"run the job -v"@2:12}}`},
		{"- a: x\n   -y\n\n", `[{a@1:3:s:"x -y"@1:6}]`},
		{"a:\n b: x\n  - y\n\n", `{a@1:1:{b@2:2:s:"x - y"@2:5}}`},
		{"x:\n  k1: a\n   -b\n\n", `{x@1:1:{k1@2:3:s:"a -b"@2:7}}`},
		{"x:\n  - a\n    - b\n\n", `{x@1:1:[s:"a - b"@2:5]}`},
		{"spec:\n  name:\n value\n", "RZ-CFG-001@3:2"},
		{"spec:\n  name:\n value \n", "RZ-CFG-001@3:2"},
		{"x:\n  k:\n a   \ny: 1\n", "RZ-CFG-001@3:2"},
		{"spec:\n  items:\n  -\n x \n", "RZ-CFG-001@4:2"},
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

// TestNodesAfterBlockScalar covers the node on the line after an empty
// block scalar and an empty line: it is read as written, a quoted key, a
// tag, an anchor or a flow collection alike.
func TestNodesAfterBlockScalar(t *testing.T) {
	tests := []struct{ src, want, msg string }{
		{"x: |\n\n\"q\": 1\n", `{x:s:"",q:i:1}`, ""},
		{"x: |\n\n'q': 1\n", `{x:s:"",q:i:1}`, ""},
		{"x: |\n\n&a q: 1\n", "RZ-CFG-003@3:1", "anchors are not allowed (&a)"},
		{"x: |\n\n*a : 1\n", "RZ-CFG-003@3:1", "aliases are not allowed (*a)"},
		{"x: >-\n\n!e x: 1\n", "RZ-CFG-004@3:1", "tag !e is not allowed; only the YAML 1.2 core tags are"},
		{"x: |\n\n!!str b: 1\n", `{x:s:"",b:i:1}`, ""},
		{"x: |\n\n[a]: b\n", "RZ-CFG-001@3:1", msgComplexKey},
		{"x: |\n\n{a: b}\n", "RZ-CFG-001@3:1", ""},
		{"spec:\n  x: |\n\n  \"q\": 1\n", `{spec:{x:s:"",q:i:1}}`, ""},
		{"x: |\n  \n\"q\": 1\n", `{x:s:"",q:i:1}`, ""},
		{"x: |-\n\n\n\"a\": 1\n\"a\": 2\n", "RZ-CFG-002@5:1", `duplicate key "a"`},
		{"x: |\n\n&a q: 1\ny: !e 2\n", "RZ-CFG-003@3:1 RZ-CFG-004@4:4", ""},
		// An explicit key whose node starts on the next line.
		{"?\n  a\n: b\n", `{a:s:"b"}`, ""},
		{"x: |\n\"q\": 1\n", `{x:s:"",q:i:1}`, ""},
		{"x: |\n# c\n\n\"q\": 1\n", `{x:s:"",q:i:1}`, ""},
		{"x: |\n  t\n\n\"q\": 1\n", `{x:s:"t\n",q:i:1}`, ""},
		{"x: |\n\nq: 1\n", `{x:s:"",q:i:1}`, ""},
		{"? |\n\n: v\n", `{:s:"v"}`, ""},
		{"x: |\n\n# c\nq: 1\n", `{x:s:"",q:i:1}`, ""},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, false)
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
		if tt.msg == "" {
			continue
		}
		if _, diags := parseYAML(t, tt.src); len(diags) == 0 || diags[0].Message != tt.msg {
			t.Errorf("%q: messages %+v, want %q", tt.src, diags, tt.msg)
		}
	}
}

// TestTabsInPlainScalars covers a tab inside a plain scalar on one line:
// the value keeps it and is typed by the core schema from that text, so
// "x: 1\t2" is a string, keys with and without the tab differ, and a tag
// that does not match is refused. A tab after the scalar's last character
// is white space.
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
		{"x: b\t:\tc\n", "RZ-CFG-001@1:4"},
		{"x: a\tb\n  c\td\n", `{x:s:"a\tb c\td"}`},
		{"- 1\t2\n- 3\n", `[s:"1\t2",i:3]`},
		{"x:\t1\n", `{x:i:1}`},
		{"x: 1\t# c\n", `{x:i:1}`},
		// A ':' before a character other than white space stays in the
		// scalar, after a tab too.
		{"k: a\"\t:b\n", `{k:s:"a\"\t:b"}`},
		{"k: a\t:b\n", `{k:s:"a\t:b"}`},
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
	// The JSON twin of a tab in a string.
	jd, jdiags := parseJSON(t, "{\"x\":\"1\\t2\"}")
	yd, ydiags := parseYAML(t, "x: 1\t2\n")
	if len(jdiags) != 0 || len(ydiags) != 0 || show(jd[0].Root, false) != show(yd[0].Root, false) {
		t.Errorf("twins: %q %q", codes(jdiags), codes(ydiags))
	}
}

// TestPlainCharacters covers the characters a plain scalar may hold (YAML
// 1.2.2 rules 126-133): a ':' or '#' inside it, a non-breaking space, which
// is not white space in YAML, and in a flow collection a ':' before a
// character that is not white space or a flow indicator.
func TestPlainCharacters(t *testing.T) {
	tests := []struct{ src, want string }{
		{"url: http://example.com:8080/a#b\n", `{url:s:"http://example.com:8080/a#b"}`},
		{"x: a:b\n", `{x:s:"a:b"}`},
		{"x: a::\n", "RZ-CFG-001@1:4"},
		{"x: a #b\n", `{x:s:"a"}`},
		{"x: a\u00a0b\n", `{x:s:"a\u00a0b"}`},
		{"x: \u00a0b\n", `{x:s:"\u00a0b"}`},
		{"x: b\u00a0\n", `{x:s:"b\u00a0"}`},
		{"x\u00a0y: 1\n", "{x\u00a0y:i:1}"},
		{"x: {app:web}\n", `{x:{app:web:n}}`},
		{"x: [app:web]\n", `{x:[s:"app:web"]}`},
		{"x: [http://a:80/b]\n", `{x:[s:"http://a:80/b"]}`},
		{"x: {a:\"b\"}\n", `{x:{a:"b":n}}`},
		{"x: {a: b:c}\n", `{x:{a:s:"b:c"}}`},
		{"x: -a\n", `{x:s:"-a"}`},
		{"x: ?a\n", `{x:s:"?a"}`},
		{"x: :a\n", `{x:s:":a"}`},
		{"x: [-a, ?a, :a]\n", `{x:[s:"-a",s:"?a",s:":a"]}`},
		{"x: a]b\n", `{x:s:"a]b"}`},
		{"x: [a]b]\n", "RZ-CFG-001@1:7"},
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

// TestFlowIndicatorSpace covers '?' and ':' in a flow collection (YAML
// 1.2.2 rules 142, 147, 150): an explicit key's '?' is followed by white
// space, and after a key that is not quoted the value is separated from its
// ':' by white space, so "{a:[]}" is the key "a" with an empty value and
// then an unexpected '[', while a ':' before ',' or the closing bracket
// gives an empty value, and after a quoted key a value may follow the ':'
// directly.
func TestFlowIndicatorSpace(t *testing.T) {
	tests := []struct{ src, want, msg string }{
		{"x: {a:[]}\n", "RZ-CFG-001@1:7", msgColonSpace},
		{"x: {a:{}}\n", "RZ-CFG-001@1:7", msgColonSpace},
		{"x: [a:[]]\n", "RZ-CFG-001@1:7", msgColonSpace},
		{"x: {:[]}\n", "RZ-CFG-001@1:6", msgColonSpace},
		{"x: [:[]]\n", "RZ-CFG-001@1:6", msgColonSpace},
		{"x: {? a :[]}\n", "RZ-CFG-001@1:10", msgColonSpace},
		{"x: [? a :{}]\n", "RZ-CFG-001@1:10", msgColonSpace},
		{"x: {a: [], b:[c]}\n", "RZ-CFG-001@1:14", msgColonSpace},
		{"x: [?]\n", "RZ-CFG-001@1:5", msgQuestionSpace},
		{"x: {?}\n", "RZ-CFG-001@1:5", msgQuestionSpace},
		{"x: [a, ?]\n", "RZ-CFG-001@1:8", msgQuestionSpace},
		{"x: {?,}\n", "RZ-CFG-001@1:5", msgQuestionSpace},
		{"x: [?[a]]\n", "RZ-CFG-001@1:5", msgQuestionSpace},
		// Valid.
		{"x: {\"a\":[]}\n", `{x:{a:[]}}`, ""},
		{"x: {'a':{}}\n", `{x:{a:{}}}`, ""},
		{"x: [\"a\":[b]]\n", `{x:[{a:[s:"b"]}]}`, ""},
		{"x: {\"a\":b}\n", `{x:{a:s:"b"}}`, ""},
		{"x: {a: []}\n", `{x:{a:[]}}`, ""},
		{"x: {a:\n  []}\n", `{x:{a:[]}}`, ""},
		{"x: {a:}\n", `{x:{a:n}}`, ""},
		{"x: {a:, b}\n", `{x:{a:n,b:n}}`, ""},
		{"x: [a:]\n", `{x:[{a:n}]}`, ""},
		{"x: {:}\n", `{x:{:n}}`, ""},
		{"x: [:]\n", `{x:[{:n}]}`, ""},
		{"x: {? }\n", `{x:{:n}}`, ""},
		{"x: [? ]\n", `{x:[{:n}]}`, ""},
		{"x: {? a}\n", `{x:{a:n}}`, ""},
		{"x: {?\n a}\n", `{x:{a:n}}`, ""},
		{"x: {?: b}\n", `{x:{?:s:"b"}}`, ""},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, false)
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
		if _, diags := parseYAML(t, tt.src); tt.msg != "" && (len(diags) != 1 || diags[0].Message != tt.msg) {
			t.Errorf("%q: %+v, want the message %q", tt.src, diags, tt.msg)
		}
	}
}

// TestLoneQuestionMark covers a '?' that a line break follows: it is an
// explicit key indicator with an empty key, as YAML 1.2 reads it ("b:\n?"
// is {b: null, null: null}), the empty key being ""; on the line of a key's
// ':' it would start a block mapping there, which YAML 1.2 refuses; before
// ':' or a character that is not white space it starts a plain scalar.
func TestLoneQuestionMark(t *testing.T) {
	var wide strings.Builder
	for i := range 300 {
		wide.WriteString("w" + itoa(i) + ": v\n")
	}
	tests := []struct{ src, want string }{
		{"b:\n?\n", `{b:n,:n}`},
		{"a:\n  ?\n", `{a:{:n}}`},
		{"a:\n  ? # c\n", `{a:{:n}}`},
		{"a:\n  b:\n  ?\nc: 1\n", `{a:{b:n,:n},c:i:1}`},
		{"a:\n  b:\n    c:\n      ?\n", `{a:{b:{c:{:n}}}}`},
		{"a:\n  - ?\n", `{a:[{:n}]}`},
		{"a:\n- ?\n", `{a:[{:n}]}`},
		{"- ?\n- x\n", `[{:n},s:"x"]`},
		{"a:\n?\n# c\n", `{a:n,:n}`},
		{"a:\n?\n...\n", `{a:n,:n}`},
		{"x: {a: 1, ?\n }\n", `{x:{a:i:1,:n}}`},
		{"x: {a: 1, ?\n}\n", "RZ-CFG-001@2:1"},
		{"?\n", `{:n}`},
		// The empty key is in the column after its '?'.
		{"?\n?\n", "RZ-CFG-002@2:2"},
		{wide.String() + "?\n", ""},
		{"x: ?\n", "RZ-CFG-001@1:4"},
		{"?: x\n", `{?:s:"x"}`},
		{"x: ?y\n", `{x:s:"?y"}`},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" && len(docs) == 1 && tt.want != "" {
			got = show(docs[0].Root, false)
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src[:min(len(tt.src), 40)], got, tt.want)
		}
	}
	if _, diags := parseYAML(t, "x: ?\n"); len(diags) != 1 || diags[0].Message != msgSameLineValue {
		t.Errorf("x: ?: %+v", diags)
	}
}
