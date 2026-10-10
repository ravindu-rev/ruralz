// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestParseJSONTree checks the JSON front end's tree, styles and positions
// (01 req 15, 14).
func TestParseJSONTree(t *testing.T) {
	src := "{\n" +
		"  \"apiVersion\": \"ruralz/v1alpha1\",\n" +
		"  \"spec\": {\"port\": 8080, \"ratio\": 0.5, \"neg\": -0, \"big\": 1E+3,\n" +
		"    \"ok\": true, \"no\": false, \"none\": null, \"list\": [1, \"é\", {}], \"empty\": []}\n" +
		"}\n"
	docs, diags := parseJSON(t, src)
	if len(diags) != 0 || len(docs) != 1 {
		t.Fatalf("diagnostics %q, %d documents", codes(diags), len(docs))
	}
	want := `{apiVersion@2:3:s:"ruralz/v1alpha1"@2:17,spec@3:3:{port@3:12:i:8080@3:20,ratio@3:26:f:0.5@3:35,` +
		`neg@3:40:i:0@3:47,big@3:51:f:1E+3@3:58,ok@4:5:b:true@4:11,no@4:17:b:false@4:23,none@4:30:n@4:38,` +
		`list@4:44:[i:1@4:53,s:"é"@4:56,{}],empty@4:66:[]}}`
	if got := show(docs[0].Root, true); got != want {
		t.Errorf("tree:\n got %s\nwant %s", got, want)
	}
	if docs[0].Start != (tree.Pos{Line: 1, Column: 1}) || docs[0].Root.Style != tree.StyleJSON {
		t.Errorf("start %+v, style %d", docs[0].Start, docs[0].Root.Style)
	}
}

// TestJSONErrors covers the strict front end (01 req 15): every error is
// RZ-CFG-001 at the scanner's offset, a non-object top level is
// RZ-CFG-005, and duplicates are RZ-CFG-002 with both positions.
func TestJSONErrors(t *testing.T) {
	tests := []struct {
		name      string
		src       string
		code      string
		line, col int
		msg       string
	}{
		{"comment", "{\"a\": 1 // c\n}", codeParse, 1, 9, "invalid JSON"},
		{"block comment", "{/* c */}", codeParse, 1, 2, "invalid JSON"},
		{"trailing comma", "{\"a\": 1,\n}", codeParse, 2, 1, "invalid JSON"},
		{"trailing comma in array", "{\"a\": [1,]}", codeParse, 1, 10, "invalid JSON"},
		{"leading zero", "{\"a\": 01}", codeParse, 1, 8, "invalid JSON"},
		{"two values", "{}\n{}", codeParse, 2, 1, "invalid JSON"},
		{"trailing garbage", "{} x", codeParse, 1, 4, "invalid JSON"},
		{"empty", "", codeParse, 1, 1, "invalid JSON"},
		{"only space", " \n ", codeParse, 2, 2, "invalid JSON"},
		{"invalid UTF-8", "{\"a\": \"é\xff\"}", codeParse, 1, 9, "UTF-8"},
		{"unpaired surrogate", "{\"a\": \"\\ud800\"}", codeParse, 1, 8, "surrogate"},
		{"single quotes", "{'a': 1}", codeParse, 1, 2, "invalid JSON"},
		{"BOM", "\uFEFF{}", codeParse, 1, 1, "byte order mark"},
		{"array root", "[1]", codeSchema, 1, 1, "a resource must be a mapping"},
		{"string root", "\"x\"", codeSchema, 1, 1, "a resource must be a mapping"},
		{"null root", " null", codeSchema, 1, 2, "a resource must be a mapping"},
		{"integer range", "{\"a\": 9223372036854775808}", codeSchema, 1, 7, "64-bit range"},
		{"depth", "{\"a\":" + strings.Repeat("[", 64) + strings.Repeat("]", 64) + "}", codeParse, 1, 69, "nesting depth exceeds 64"},
	}
	for _, tt := range tests {
		docs, diags := parseJSON(t, tt.src)
		if len(diags) != 1 || diags[0].Code != tt.code || diags[0].Line != tt.line || diags[0].Column != tt.col {
			t.Errorf("%s: %q, want %s@%d:%d", tt.name, codes(diags), tt.code, tt.line, tt.col)
			continue
		}
		if !strings.Contains(diags[0].Message, tt.msg) {
			t.Errorf("%s: message %q, want %q", tt.name, diags[0].Message, tt.msg)
		}
		if len(docs) != 0 {
			t.Errorf("%s: %d documents", tt.name, len(docs))
		}
	}
	// 64 levels pass: the object is level 1.
	docs, diags := parseJSON(t, "{\"a\":"+strings.Repeat("[", 63)+strings.Repeat("]", 63)+"}")
	if len(diags) != 0 || len(docs) != 1 {
		t.Errorf("depth 64: %q", codes(diags))
	}
}

// TestJSONDuplicates reports every duplicate name with both positions (01
// req 11, 15).
func TestJSONDuplicates(t *testing.T) {
	docs, diags := parseJSON(t, "{\"a\": 1,\n \"b\": {\"x\": 1, \"x\": 2},\n \"a\": 3, \"\\u0061\": 4}")
	if got, want := codes(diags), "RZ-CFG-002@2:16 RZ-CFG-002@3:2 RZ-CFG-002@3:10"; got != want {
		t.Fatalf("diagnostics %s, want %s", got, want)
	}
	for i, rel := range []string{"2:8", "1:2", "1:2"} {
		r := diags[i].Related
		if len(r) != 1 || strconv.Itoa(r[0].Line)+":"+strconv.Itoa(r[0].Column) != rel || r[0].File != "f.json" {
			t.Errorf("diagnostic %d related %+v, want %s", i, r, rel)
		}
	}
	if len(docs) != 0 {
		t.Errorf("%d documents", len(docs))
	}
}

// TestJSONYAMLTwins checks that JSON and YAML twins produce the same tree
// (01 req 15: tabs, \/ and surrogate-pair escapes; property P2).
func TestJSONYAMLTwins(t *testing.T) {
	twins := []struct{ json, yaml string }{
		{"{\t\"a\":\t1,\t\"b\": \"x\\ty\"}", "a: 1\nb: \"x\\ty\"\n"},
		{`{"path": "a\/b"}`, "path: a/b\n"},
		{`{"emoji": "\ud83d\ude00", "raw": "😀"}`, "emoji: \"\\U0001F600\"\nraw: 😀\n"},
		{`{"n": -0, "f": 1.5, "e": 1e3, "z": 0.0}`, "n: -0\nf: +1.5\ne: 1e3\nz: 0.0\n"},
		{`{"s": "0777", "t": "yes", "u": "true", "v": ""}`, "s: \"0777\"\nt: yes\nu: 'true'\nv: ''\n"},
		{`{"m": {"k": [1, [2], {"x": null}]}, "e": {}, "l": []}`, "m:\n  k:\n  - 1\n  - - 2\n  - x: ~\ne: {}\nl: []\n"},
		{`{"esc": "\"\\\/\b\f\n\r\t\u0001"}`, "esc: \"\\\"\\\\/\\b\\f\\n\\r\\t\\x01\"\n"},
	}
	for _, tw := range twins {
		jd, jdiags := parseJSON(t, tw.json)
		yd, ydiags := parseYAML(t, tw.yaml)
		if len(jdiags) != 0 || len(ydiags) != 0 || len(jd) != 1 || len(yd) != 1 {
			t.Errorf("%s: diagnostics %q / %q", tw.json, codes(jdiags), codes(ydiags))
			continue
		}
		if a, b := show(jd[0].Root, false), show(yd[0].Root, false); a != b {
			t.Errorf("twins differ:\n json %s\n yaml %s", a, b)
		}
	}
}
