// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strings"
	"testing"
)

// flowRow is one row of the flow entry tables: the findings, or the tree
// without positions, and the message of the one finding.
type flowRow struct{ src, want, msg string }

// checkFlowRows parses each row at the split thresholds given, which never
// change a flow collection's result: goccy parses every flow collection
// whole.
func checkFlowRows(t *testing.T, rows []flowRow, thresholds ...int) {
	t.Helper()
	for _, tt := range rows {
		for _, splitAt := range thresholds {
			docs, diags, err := parse(t.Context(), []byte(tt.src), 0, "f.yaml", FormatYAML, Options{splitAt: splitAt}, false)
			if err != nil {
				t.Fatal(err)
			}
			got := codes(diags)
			if got == "" && len(docs) == 1 {
				got = show(docs[0].Root, false)
			}
			if got != tt.want {
				t.Errorf("%q, split at %d: %s, want %s", tt.src, splitAt, got, tt.want)
				continue
			}
			if tt.msg != "" && (len(diags) != 1 || diags[0].Message != tt.msg) {
				t.Errorf("%q, split at %d: messages %+v, want %q", tt.src, splitAt, diags, tt.msg)
			}
		}
	}
}

// TestFlowColon covers a ':' inside a flow collection that goccy reads
// otherwise than YAML 1.2 (01 req 8, 12; 01 test plan P2; blocker of the
// eighth WP-33 review). While a flow mapping is open, goccy makes every ':'
// a value indicator, so "{app:web}" read as {app: web} and a flow sequence
// inside a flow mapping split "10:30" into {10: 30}, where YAML 1.2 keeps a
// ':' followed by a character a plain scalar can hold in the scalar. Outside
// a flow mapping goccy keeps a ':' before a flow indicator in the scalar
// ("[a:]" as ["a:"]), where YAML 1.2 reads a value indicator. Each is
// RZ-CFG-001 at the ':'; a ':' after a JSON-like key, or followed by white
// space or a flow indicator inside a flow mapping, is read as YAML 1.2
// reads it.
func TestFlowColon(t *testing.T) {
	checkFlowRows(t, []flowRow{
		{"labels: {app:web, tier: db}\n", "RZ-CFG-001@1:13", msgFlowColon},
		{"k: {port:8080}\n", "RZ-CFG-001@1:9", msgFlowColon},
		{"k: {host:localhost, port:80}\n", "RZ-CFG-001@1:9", msgFlowColon},
		{"spec: {endpoints: [10.0.0.1:8080, 10.0.0.2:8080]}\n", "RZ-CFG-001@1:28", msgFlowColon},
		{"k: {x: [a:1]}\n", "RZ-CFG-001@1:10", msgFlowColon},
		{"k: {x: [10:30, 11:00]}\n", "RZ-CFG-001@1:11", msgFlowColon},
		{"{a:1}\n", "RZ-CFG-001@1:3", msgFlowColon},
		{"{a:b}", "RZ-CFG-001@1:3", msgFlowColon},
		{"k: {a:1, b: 2}\n", "RZ-CFG-001@1:6", msgFlowColon},
		{"k: {a: 1,\n  b:2}\n", "RZ-CFG-001@2:4", msgFlowColon},
		{"k: {a:true}\n", "RZ-CFG-001@1:6", msgFlowColon},
		{"k: {k: v, a:b}\n", "RZ-CFG-001@1:12", msgFlowColon},
		{"k: {a:\"x\"}\n", "RZ-CFG-001@1:6", msgFlowColon},
		{"k: {a::b}\n", "RZ-CFG-001@1:6", msgFlowColon},
		{"k: {a :b}\n", "RZ-CFG-001@1:7", msgFlowColon},
		{"k: {:b}\n", "RZ-CFG-001@1:5", msgFlowColon},
		{"k: {!!str :b}\n", "RZ-CFG-001@1:11", msgFlowColon},
		{"k: {a:#c}\n", "RZ-CFG-001@1:6", msgFlowColon},
		{"k: [a:]\n", "RZ-CFG-001@1:6", msgFlowColonEnd},
		{"k: [a:, b]\n", "RZ-CFG-001@1:6", msgFlowColonEnd},
		{"k: [b, a\t:]\n", "RZ-CFG-001@1:10", msgFlowColonEnd},
		{"k: [ab:{c: d}]\n", "RZ-CFG-001@1:7", msgFlowColonEnd},
		// Read as YAML 1.2 reads them.
		{"k: [a:1]\n", `{k:[s:"a:1"]}`, ""},
		{"spec:\n  endpoints: [10.0.0.1:8080, 10.0.0.2:8080]\n", `{spec:{endpoints:[s:"10.0.0.1:8080",s:"10.0.0.2:8080"]}}`, ""},
		{"k: {\"a\":1}\n", "{k:{a:i:1}}", ""},
		{"k: {'a':1}\n", "{k:{a:i:1}}", ""},
		{"k: {\"a\" :b}\n", `{k:{a:s:"b"}}`, ""},
		{"k: {a:}\n", "{k:{a:n}}", ""},
		{"k: {a:,b}\n", "{k:{a:n,b:n}}", ""},
		{"k: {url: http://x}\n", `{k:{url:s:"http://x"}}`, ""},
		{"k: {x: [\"a\":b]}\n", `{k:{x:[{a:s:"b"}]}}`, ""},
		{"k: {a: [b]}\n", `{k:{a:[s:"b"]}}`, ""},
	}, -1, 0, 3)
}

// TestFlowEntryShape covers flow entries that hold more than YAML 1.2's
// optional '?', node, ':' and node (01 req 8, 9, 10; 11 req 17, 26;
// blocker of the eighth WP-33 review). goccy needs no ',' between entries
// of a flow sequence: it reads "key:" entries by columns across lines, so
// "x: [\nk:\n k:\n  k: v\n]" nested a level per line, past what the token
// pass counts, and comma-less entries parsed in quadratic time. Each is
// RZ-CFG-001 at the first token past the entry, with goccy's own message,
// while entries separated by ',' parse. goccy refuses some valid flow
// pairs whose value is on a later line, and a ',' at the start of a line
// after a pair, which the profile keeps (goccy v1.19.2 limitations).
func TestFlowEntryShape(t *testing.T) {
	checkFlowRows(t, []flowRow{
		{"x: [\nk:\n k:\n  k: v\n]\n", "RZ-CFG-001@3:3", msgFlowSeqEntry},
		{"x: [\n a: 1\n b: 2\n]\n", "RZ-CFG-001@3:2", msgFlowSeqEntry},
		{"x: [\n? a\n: b\n? c\n: d\n]\n", "RZ-CFG-001@4:1", msgFlowSeqEntry},
		{"x: [\na:\n b:\n  c: 1\n d: 2\n]\n", "RZ-CFG-001@3:3", msgFlowSeqEntry},
		{"x: {k: [\na: 1\nb: 2]}\n", "RZ-CFG-001@3:1", msgFlowSeqEntry},
		{"x: [\na: [1]\nb: {c: 2}\n]\n", "RZ-CFG-001@3:1", msgFlowSeqEntry},
		{"x: [\"a\" \"b\"]\n", "RZ-CFG-001@1:9", msgFlowSeqEntry},
		{"x: [a #c\n b]\n", "RZ-CFG-001@2:2", msgFlowSeqEntry},
		{"x: {a: b\n c: d}\n", "RZ-CFG-001@2:2", msgFlowMapEntry},
		{"x: {a: b: c}\n", "RZ-CFG-001@1:8", msgPlainColon},
		{"x: {\"a\" ? b}\n", "RZ-CFG-001@1:9", msgFlowMapEntry},
		{"x: {a: b ? c: d, e}\n", "RZ-CFG-001@1:8", msgPlainColon},
		// Entries YAML 1.2 accepts.
		{"x: [\n k: v,\n l: w]\n", `{x:[{k:s:"v"},{l:s:"w"}]}`, ""},
		{"x: [\na: 1,\nb: 2\n]\n", "{x:[{a:i:1},{b:i:2}]}", ""},
		{"x: [\n a:\n   b, c]\n", `{x:[{a:s:"b"},s:"c"]}`, ""},
		{"x: [\n  ? a\n : b]\n", `{x:[{a:s:"b"}]}`, ""},
		{"x: [? a: b, c]\n", `{x:[{a:s:"b"},s:"c"]}`, ""},
		{"x: {a: 1, b, ? c, : d}\n", "RZ-CFG-001@1:19", ""},
		{"x: {a: 1, b, ? c}\n", "{x:{a:i:1,b:n,c:n}}", ""},
		{"x: {a:\nb}\n", `{x:{a:s:"b"}}`, ""},
		{"x: [!!str , x]\n", `{x:[s:"",s:"x"]}`, ""},
		// goccy v1.19.2 refuses these valid entries itself (doc.go).
		{"x: [a:\n b]\n", "RZ-CFG-001@2:2", msgFlowSeqEntry},
		{"x: [? a\n : b]\n", "RZ-CFG-001@2:4", msgFlowSeqEntry},
		{"x: [k:\nv]\n", "RZ-CFG-001@2:1", msgFlowSeqEntry},
		{"x: [\n a: 1\n , b: 2]\n", "RZ-CFG-001@3:2", "syntax error: non-map value is specified"},
		{"x: {a: b\n c}\n", "RZ-CFG-001@2:2", msgFlowMapEntry},
	}, -1, 0, 1)
}

// flowLadder is the first shape of the flow entry blocker of the eighth
// WP-33 review: "x: ", then levels levels, each "[\n" and rows lines of
// "key:", the line of row r indented by r spaces (the first key is "k",
// the others keyLen bytes), then " " before the next level; then "v" and
// the closing brackets. goccy nested every line one level deeper and built
// every path before the converter's depth bound ran: 1 level of 836 rows of
// 836-byte keys (1,048,771 bytes) took a 567 MiB peak heap, and 62 levels of
// 100 rows of 100-byte keys took 3,747 MiB and 16 s.
func flowLadder(levels, rows, keyLen int) string {
	var b strings.Builder
	b.WriteString("x: ")
	for range levels {
		b.WriteString("[\n")
		for r := range rows {
			key := "k"
			if r > 0 {
				key = strings.Repeat("k", keyLen)
			}
			b.WriteString(strings.Repeat(" ", r) + key + ":")
			if r < rows-1 {
				b.WriteByte('\n')
			}
		}
		b.WriteByte(' ')
	}
	b.WriteString("v" + strings.Repeat("]", levels) + "\n")
	return b.String()
}

// commaLess is the second shape of that blocker: "x: [\n", n lines that
// entry(i) gives, then last and "]\n". goccy parsed them as one block
// mapping inside the sequence, recursing once per entry: 80,000 entries
// took 17.9 s and 24.9 GiB of allocation.
func commaLess(n int, entry func(i int) string, last string) string {
	var b strings.Builder
	b.WriteString("x: [\n")
	for i := range n {
		b.WriteString(entry(i) + "\n")
	}
	b.WriteString(last + "]\n")
	return b.String()
}

// TestFlowLaddersStop checks that the shapes of the flow entry blocker stop
// at their first extra entry token, before goccy parses them (01 req 9; 11
// req 17, 26): small instances here, the 1 MiB ones in
// TestHostileInputsBounded.
func TestFlowLaddersStop(t *testing.T) {
	keyed := func(i int) string { return "k" + itoa(i) + ": v" }
	for _, tt := range []struct{ src, want string }{
		{flowLadder(1, 3, 2), "RZ-CFG-001@3:4"},
		{flowLadder(3, 4, 4), "RZ-CFG-001@3:6"},
		{commaLess(10, keyed, ""), "RZ-CFG-001@3:1"},
		{commaLess(10, func(i int) string { return "k" + itoa(i) + ":" }, "z: v"), "RZ-CFG-001@3:3"},
		{commaLess(10, func(int) string { return "k: v" }, ""), "RZ-CFG-001@3:1"},
	} {
		docs, diags := parseYAML(t, tt.src)
		if got := codes(diags); got != tt.want || len(docs) != 0 || diags[0].Message != msgFlowSeqEntry {
			t.Errorf("%q: %q %+v, want %s", tt.src, got, diags, tt.want)
		}
	}
}
