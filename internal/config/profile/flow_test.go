// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"testing"
)

// flowRow is one row of the flow collection tables: the findings, or the
// tree without positions, and the message of the one finding.
type flowRow struct{ src, want, msg string }

// checkFlowRows parses each row and compares its findings or tree.
func checkFlowRows(t *testing.T, rows []flowRow) {
	t.Helper()
	for _, tt := range rows {
		docs, diags, err := parse(t.Context(), []byte(tt.src), 0, "f.yaml", FormatYAML, Options{}, true)
		if err != nil {
			t.Fatal(err)
		}
		got := codes(diags)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, false)
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
			continue
		}
		if tt.msg != "" && (len(diags) != 1 || diags[0].Message != tt.msg) {
			t.Errorf("%q: messages %+v, want %q", tt.src, diags, tt.msg)
		}
	}
}

// TestFlowColon covers a ':' inside a flow collection (YAML 1.2.2 rules
// 130, 147): a ':' followed by a character a plain scalar can hold stays in
// the scalar, so "{app:web}" is the key "app:web" with a null value and
// "[10:30]" the string "10:30", as YAML 1.2 reads them; a ':' followed by
// white space or a flow indicator is a value indicator.
func TestFlowColon(t *testing.T) {
	checkFlowRows(t, []flowRow{
		{"labels: {app:web, tier: db}\n", `{labels:{app:web:n,tier:s:"db"}}`, ""},
		{"k: {port:8080}\n", `{k:{port:8080:n}}`, ""},
		{"k: {host:localhost, port:80}\n", `{k:{host:localhost:n,port:80:n}}`, ""},
		{"spec: {endpoints: [10.0.0.1:8080, 10.0.0.2:8080]}\n", `{spec:{endpoints:[s:"10.0.0.1:8080",s:"10.0.0.2:8080"]}}`, ""},
		{"spec:\n  endpoints: [10.0.0.1:8080, 10.0.0.2:8080]\n", `{spec:{endpoints:[s:"10.0.0.1:8080",s:"10.0.0.2:8080"]}}`, ""},
		{"k: {x: [a:1]}\n", `{k:{x:[s:"a:1"]}}`, ""},
		{"k: {x: [10:30, 11:00]}\n", `{k:{x:[s:"10:30",s:"11:00"]}}`, ""},
		{"{a:1}\n", `{a:1:n}`, ""},
		{"{a:b}", `{a:b:n}`, ""},
		{"k: {a:1, b: 2}\n", `{k:{a:1:n,b:i:2}}`, ""},
		{"k: {a: 1,\n  b:2}\n", `{k:{a:i:1,b:2:n}}`, ""},
		{"k: {a:true}\n", `{k:{a:true:n}}`, ""},
		{"k: {k: v, a:b}\n", `{k:{k:s:"v",a:b:n}}`, ""},
		{"k: {a::b}\n", `{k:{a::b:n}}`, ""},
		{"k: {a :b}\n", `{k:{a :b:n}}`, ""},
		{"k: {:b}\n", `{k:{:b:n}}`, ""},
		{"k: {!!str :b}\n", `{k:{:b:n}}`, ""},
		{"k: {a:#c}\n", `{k:{a:#c:n}}`, ""},
		{"k: [a:1]\n", `{k:[s:"a:1"]}`, ""},
		{"k: {url: http://x}\n", `{k:{url:s:"http://x"}}`, ""},
		// A value indicator before a flow indicator or white space.
		{"k: [a:]\n", `{k:[{a:n}]}`, ""},
		{"k: [a:, b]\n", `{k:[{a:n},s:"b"]}`, ""},
		{"k: [b, a\t:]\n", `{k:[s:"b",{a:n}]}`, ""},
		{"k: {a:}\n", "{k:{a:n}}", ""},
		{"k: {a:,b}\n", "{k:{a:n,b:n}}", ""},
		{"k: {a: [b]}\n", `{k:{a:[s:"b"]}}`, ""},
		{"k: [ab:{c: d}]\n", "RZ-CFG-001@1:8", msgColonSpace},
		// After a JSON-like key the value may follow the ':' directly.
		{"k: {\"a\":1}\n", "{k:{a:i:1}}", ""},
		{"k: {'a':1}\n", "{k:{a:i:1}}", ""},
		{"k: {\"a\" :b}\n", `{k:{a:s:"b"}}`, ""},
		{"k: {x: [\"a\":b]}\n", `{k:{x:[{a:s:"b"}]}}`, ""},
	})
}

// TestFlowEntryShape covers what a flow entry holds (YAML 1.2.2 section
// 7.4): an optional '?', a node, a ':' and a node, entries separated by
// ',', lines indented more than the block collection holding the flow
// collection, and a plain scalar that spans lines; anything more is
// RZ-CFG-001 at the first character past the entry.
func TestFlowEntryShape(t *testing.T) {
	checkFlowRows(t, []flowRow{
		{"x: [\nk:\n k:\n  k: v\n]\n", "RZ-CFG-001@2:1", msgFlowIndent},
		// A plain value continues on the next line, "?" included, so the
		// ':' after it is the extra text.
		{"x: [\n a: 1\n b: 2\n]\n", "RZ-CFG-001@3:3", msgFlowSeqNext},
		{"x: [\n ? a\n : b\n ? c\n : d\n ]\n", "RZ-CFG-001@5:2", msgFlowSeqNext},
		{"x: [\n? a\n: b\n]\n", "RZ-CFG-001@2:1", msgFlowIndent},
		{"x: {k: [\na: 1\nb: 2]}\n", "RZ-CFG-001@2:1", msgFlowIndent},
		{"x: [\"a\" \"b\"]\n", "RZ-CFG-001@1:9", msgFlowSeqNext},
		{"x: [a #c\n b]\n", "RZ-CFG-001@2:2", msgFlowSeqNext},
		{"x: {a: b\n c: d}\n", "RZ-CFG-001@2:3", msgFlowMapNext},
		{"x: {a: b: c}\n", "RZ-CFG-001@1:9", msgFlowMapNext},
		{"x: {\"a\" ? b}\n", "RZ-CFG-001@1:9", msgFlowMapNext},
		{"x: {a: b ? c: d, e}\n", "RZ-CFG-001@1:13", msgFlowMapNext},
		// A flow pair's key is on one line; a flow mapping's key may span
		// lines (YAML 1.2.2 rules 144 and 153).
		{"x: [a\n b: c]\n", "RZ-CFG-001@2:3", msgMultiLineKey},
		{"x: [a:\nb]\n", "RZ-CFG-001@2:1", msgFlowIndent},
		{"x: {a:\nb}\n", "RZ-CFG-001@2:1", msgFlowIndent},
		{"x: [\na: 1,\nb: 2\n]\n", "RZ-CFG-001@2:1", msgFlowIndent},
		// Entries YAML 1.2 accepts.
		{"x: {a\n b: c}\n", `{x:{a b:s:"c"}}`, ""},
		{"x: [a, b c: d]\n", `{x:[s:"a",{b c:s:"d"}]}`, ""},
		{"x: [\n k: v,\n l: w]\n", `{x:[{k:s:"v"},{l:s:"w"}]}`, ""},
		{"x: [\n a: 1,\n b: 2\n ]\n", "{x:[{a:i:1},{b:i:2}]}", ""},
		{"x: [\n a:\n   b, c]\n", `{x:[{a:s:"b"},s:"c"]}`, ""},
		{"x: [\n  ? a\n : b]\n", `{x:[{a:s:"b"}]}`, ""},
		{"x: [? a: b, c]\n", `{x:[{a:s:"b"},s:"c"]}`, ""},
		{"x: {a: 1, b, ? c, : d}\n", `{x:{a:i:1,b:n,c:n,:s:"d"}}`, ""},
		{"x: {a: 1, b, ? c}\n", "{x:{a:i:1,b:n,c:n}}", ""},
		{"x: [!!str , x]\n", `{x:[s:"",s:"x"]}`, ""},
		{"x: [a:\n b]\n", `{x:[{a:s:"b"}]}`, ""},
		{"x: [? a\n : b]\n", `{x:[{a:s:"b"}]}`, ""},
		{"x: [\n a: 1\n , b: 2]\n", "{x:[{a:i:1},{b:i:2}]}", ""},
		{"x: {a: b\n c}\n", `{x:{a:s:"b c"}}`, ""},
		{"x: [a, b,]\n", `{x:[s:"a",s:"b"]}`, ""},
		{"x: {a: 1,}\n", `{x:{a:i:1}}`, ""},
		{"x: [\n  a,\n  b\n  ]\n", `{x:[s:"a",s:"b"]}`, ""},
		// An empty entry.
		{"x: [a,,b]\n", "RZ-CFG-001@1:7", msgFlowEmpty},
		{"x: [,]\n", "RZ-CFG-001@1:5", msgFlowEmpty},
		{"x: {,}\n", "RZ-CFG-001@1:5", msgFlowEmpty},
	})
}

// TestFlowClosingBracket covers the closing bracket of a flow collection in
// a block collection (YAML 1.2.2 rule 69, s-flow-line-prefix): like every
// line of the collection it is indented more than the block collection, so
// a bracket at its key's column, which libyaml accepts, is refused with a
// message naming the column.
func TestFlowClosingBracket(t *testing.T) {
	checkFlowRows(t, []flowRow{
		{"hosts: [\n  a,\n  b\n]\n", "RZ-CFG-001@4:1", msgFlowClose},
		{"hosts: {\n  a: 1\n}\n", "RZ-CFG-001@3:1", msgFlowClose},
		{"spec:\n  hosts: [\n    a\n  ]\n", "RZ-CFG-001@4:3", msgFlowClose},
		{"- [\n  a\n]\n", "RZ-CFG-001@3:1", msgFlowClose},
		{"hosts: [\n  a,\n  b\n ]\n", `{hosts:[s:"a",s:"b"]}`, ""},
		{"spec:\n  hosts: [\n    a\n   ]\n", `{spec:{hosts:[s:"a"]}}`, ""},
		// At the document level any column does.
		{"[\na\n]\n", `[s:"a"]`, ""},
		{"--- {\na: 1\n}\n", `{a:i:1}`, ""},
	})
}

// TestFlowLaddersStop checks that comma-less and ladder-shaped flow entries
// stop at their first extra entry or under-indented line: small instances
// here, the 1 MiB ones in TestHostileInputsBounded.
func TestFlowLaddersStop(t *testing.T) {
	keyed := func(i int) string { return " k" + itoa(i) + ": v" }
	for _, tt := range []struct{ src, want, msg string }{
		{flowLadder(1, 3, 2), "RZ-CFG-001@2:1", msgFlowIndent},
		{flowLadder(3, 4, 4), "RZ-CFG-001@2:1", msgFlowIndent},
		// The value continues on the next line, up to its ':'.
		{commaLess(10, keyed, ""), "RZ-CFG-001@3:4", msgFlowSeqNext},
		{commaLess(10, func(i int) string { return " k" + itoa(i) + ":" }, " z: v"), "RZ-CFG-001@3:4", msgFlowSeqNext},
		{commaLess(10, func(int) string { return " k: v" }, ""), "RZ-CFG-001@3:3", msgFlowSeqNext},
	} {
		docs, diags := parseYAML(t, tt.src)
		if got := codes(diags); got != tt.want || len(docs) != 0 || diags[0].Message != tt.msg {
			t.Errorf("%q: %q %+v, want %s", tt.src, got, diags, tt.want)
		}
	}
}
